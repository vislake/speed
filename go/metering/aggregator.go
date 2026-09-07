package metering

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/vislake/speed/go/dbkit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// counterEntry is one real-time quota counter: the running Quantity within
// one (tenant, feature, period) bucket, guarded by its own mutex so
// concurrent Ingest calls for the same key never lose an increment.
// notifiedOverage latches once EventOverageThresholdCrossed has fired for
// this bucket, so a threshold that stays crossed for the rest of the
// period publishes exactly one event rather than one per subsequent
// event -- resetting only implicitly, when a new period's counterEntry is
// created fresh (see realtimeKey embedding the period start in its key).
//
// seeded records that this entry has been reconstructed from the durable
// UsageSummary row for its bucket (ensureSeeded): the first touch of a
// key in a process -- a restart, or merely a key no event has hit since
// the process started -- starts from the summary's quantity rather than
// from zero, and latches notifiedOverage when that quantity already meets
// the bucket's threshold. See Aggregator's "Reconstruction after a
// restart" doc comment for the full argument.
type counterEntry struct {
	mu              sync.Mutex
	quantity        float64
	notifiedOverage bool
	seeded          bool
}

// Aggregator is the in-process aggregation backend
// (docs/internal/06-billing-and-metering.md's backend-comparison table,
// in-process implementation column): the one place both reliability tiers
// (AnalyticsRecorder's flush loop and Dispatcher's delivery loop) funnel
// measured usage into. It maintains:
//
//   - A real-time quota counter per (tenant, feature, period), held in an
//     in-process sync.Map -- RealtimeCount reads it back.
//   - A database-backed UsageSummary row per (tenant, feature, period),
//     upserted through SummaryRepository.
//   - The overage-threshold check that publishes
//     EventOverageThresholdCrossed on the wired pkgcore.EventBus.
//
// # Summary writes are serialized by a single process-wide mutex
//
// dbkit.Repository[T]'s Create/FindByID/Update each open their OWN
// transaction (dbkit.WithTenantSession), so a naive
// "FindByID, then Create-or-Update" sequence run without external
// coordination is a lost-update race under concurrent Ingest calls for
// the same (tenant, feature, period) key. Aggregator closes that race
// with mu: every summary read-modify-write runs under it, so at most one
// goroutine performs the sequence at a time, in this process. That is a
// deliberate, round-1 simplification, not an oversight -- this round
// ships the in-process aggregation backend only
// (docs/internal/06-billing-and-metering.md's own "MVP now, split into its
// own container once volume grows" framing), so "this process" is the
// whole deployment. A Redis- or
// PostgreSQL-backed aggregation backend (a later round, per AGENTS.md's
// Known limitations) would replace this mutex with a real atomic upsert
// (Redis INCRBYFLOAT, or a PostgreSQL INSERT ... ON CONFLICT) that holds
// across processes; it does not need to reuse this one.
//
// # Real-time counters are exact; summary rows are eventually applied
//
// The in-process sync.Map counter is updated synchronously and atomically
// under counterEntry's own mutex before Ingest returns, so RealtimeCount
// always reflects every event Ingest has accepted. The database summary
// row is updated in the same call, under mu -- both effects land together,
// there is no separate async flush for the summary half within
// Aggregator itself (any staleness in a caller's view of usage comes from
// where in AnalyticsRecorder's or Dispatcher's own queue an event is
// sitting, not from Aggregator internals).
//
// # Reconstruction after a restart
//
// The counters are in-process state: they die with the process, while the
// UsageSummary rows they mirror are durable. If a fresh Aggregator simply
// started at zero, a mid-period restart would under-report usage until
// enough new events arrived -- and its overage latch would be unset, so a
// tenant whose durable usage already crossed a threshold would cross
// "again" on the first post-restart event and fire a second
// EventOverageThresholdCrossed for one period (reviewer finding
// P2-metering-12). Aggregator therefore reconstructs each counter from
// its bucket's summary row on the key's first touch in the process
// (ensureSeeded, run by Ingest and IngestBillingGrade before the event's
// own write and by RealtimeCount on a map miss): quantity starts at the
// summary's quantity, and the overage latch is set when that quantity
// already meets the bucket's threshold -- the crossing is a durable fact
// once the summary holds it, so it must not fire again. This is the
// per-key, lazy equivalent of a startup-time backfill, which this module
// cannot do: UsageSummary is tenant-scoped, so reading every tenant's
// rows at once would require the cross-tenant system-context path, which
// metering is not on this codebase's sanctioned list for. Every key a
// host actually touches post-restart reconstructs correctly; a key nobody
// touches has nothing to under-report to. The billing-grade redelivery
// path needs one extra rule on top: an alreadyIngested redelivery (the
// receipt already exists) must NOT add its delta, because the seed read
// happened after the earlier delivery's fold committed and therefore
// already includes it -- adding again would double-count. The crash
// window between a fold's commit and its counter increment is closed by
// the seed, not by the redelivery: a redelivered event after a restart
// reconstructs a counter that already holds its fold.
//
// # Expired periods are evicted, never resident forever
//
// A counterEntry lives per (tenant, feature, period), and periods end:
// without eviction the map would grow without bound for the process
// lifetime, one entry per bucket a tenant ever used (reviewer finding
// P2-metering-13). Ingest and IngestBillingGrade therefore sweep, under
// mu and at most once per period boundary crossed, every entry whose
// period predates the event's own -- see sweepExpiredCountersLocked. The
// sweep is safe against a concurrent add for a swept key because the
// summary row already holds that add (the add only ever follows its own
// committed upsert): a later event or read for the expired period
// re-seeds from the summary and loses nothing. Entries are recreated by
// a later touch of their period -- a query for an old period, a
// backdated event -- and retired again at the next crossed boundary;
// residency is bounded by traffic, not by the calendar.
type Aggregator struct {
	summaries *SummaryRepository

	bucket     string
	thresholds OverageThresholds
	bus        pkgcore.EventBus

	counters sync.Map // string -> *counterEntry
	mu       sync.Mutex
	// sweptThrough is the newest period start a sweep has run for, guarded
	// by mu: a sweep runs only when an event's period start is newer, so
	// the full-map walk happens at most once per period boundary crossed
	// rather than on every Ingest.
	sweptThrough time.Time
}

// NewAggregator returns an Aggregator over summaries, with the default
// period bucket (PeriodBucketMonthly) and no overage thresholds
// configured. Module wires the period bucket and thresholds a host
// selected via its own Option functions, which mutate the fields below
// directly (same package, see module.go).
func NewAggregator(summaries *SummaryRepository) *Aggregator {
	return &Aggregator{
		summaries: summaries,
		bucket:    defaultPeriodBucket,
	}
}

// realtimeKey identifies one counterEntry. It embeds periodStart (not just
// tenant and feature) so a new calendar period gets a fresh zero-valued
// counter automatically, with no explicit reset logic needed anywhere --
// LoadOrStore below simply finds nothing for the new key and creates one.
func realtimeKey(tenantID, feature string, periodStart time.Time) string {
	return tenantID + "|" + feature + "|" + periodStart.UTC().Format(time.RFC3339)
}

// Ingest is the one place both reliability tiers deliver a validated
// UsageEvent into: it upserts the database summary row, then increments
// the real-time counter and -- on the event that first crosses a
// configured overage threshold within the current period -- publishes
// EventOverageThresholdCrossed. ctx need not carry a tenant (Ingest builds
// its own tenant context from event.TenantID before touching
// SummaryRepository, since both callers are asynchronous paths that
// cannot assume ctx carries the original request's tenant -- see this
// codebase's "workers do not inherit tenant context" trap).
//
// A zero event.OccurredAt is treated as time.Now().
//
// Persistence precedes the counter delta and the overage latch, mirroring
// IngestBillingGrade's own persist-then-count order: an event whose
// summary-row write fails is refused before the real-time counter or the
// notifiedOverage latch is touched, so a later event that first reaches
// the threshold within the period is still the crossing event and still
// publishes. The count-then-persist order this replaces lost the crossing
// forever -- the latch was already set when the summary write failed, so
// no subsequent event ever crossed again, while the counter held a delta
// the database never received.
//
// Publishing the overage event, if one fires, is best-effort: a publish
// failure is logged and does NOT fail Ingest. The usage measurement
// itself (the summary row, then the counter increment) has already
// committed by that point; failing the whole call over a secondary
// notification signal would make a real, already-durable measurement look
// like it was lost, which is worse than a missed notification.
func (a *Aggregator) Ingest(ctx context.Context, event UsageEvent) error {
	if err := event.validate(); err != nil {
		return err
	}
	occurredAt := event.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	start, end, err := periodBounds(occurredAt, a.bucket)
	if err != nil {
		return err
	}

	tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID(event.TenantID))

	// The seed read and the upsert run under the same mu acquisition, in
	// that order: the counter entry must be reconstructed from the summary
	// state BEFORE this event's own delta lands in it, or the delta would
	// be counted twice (once by the seed, once by applyDelta below).
	a.mu.Lock()
	entry, err := a.ensureSeeded(tenantCtx, event.TenantID, event.Feature, start)
	if err == nil {
		err = upsertSummaryInto(tenantCtx, a.summaries, event.Feature, start, end, event.Quantity)
	}
	a.sweepExpiredCountersLocked(start)
	a.mu.Unlock()
	if err != nil {
		return err
	}

	quantity, crossed := a.applyDelta(entry, event.Feature, event.Quantity)
	if crossed {
		a.publishOverageCrossed(ctx, event, start, end, quantity, occurredAt)
	}
	return nil
}

// ensureSeeded returns the counter entry for (tenantID, feature, start),
// creating it if needed and reconstructing it from the bucket's durable
// UsageSummary row on the key's first touch in this process -- the
// restart-reconstruction mechanism the Aggregator type's own doc comment
// describes. The reconstruction runs under the entry's own mutex, so two
// concurrent first touches (an Ingest and a RealtimeCount for the same
// key) read the same summary value and only one of them performs the
// read; callers that need the reconstruction ordered before their own
// summary write hold a.mu across this call and that write (see Ingest).
// tenantCtx must carry the tenant the summary row lives under.
//
// A bucket with no summary row yet seeds to zero (record-not-found is not
// an error); an Aggregator with no summaries repository (a pure-counter
// construction) seeds to zero without reading. On a genuine read error
// the entry is left unseeded and the error returned, so a later call
// retries the reconstruction rather than silently running from zero past
// a durable history.
func (a *Aggregator) ensureSeeded(tenantCtx context.Context, tenantID, feature string, start time.Time) (*counterEntry, error) {
	key := realtimeKey(tenantID, feature, start)
	entryAny, _ := a.counters.LoadOrStore(key, &counterEntry{})
	entry, ok := entryAny.(*counterEntry)
	if !ok {
		// Unreachable: this Aggregator is the only writer of a.counters,
		// and every value it ever stores is a *counterEntry.
		panic("metering: a.counters holds a value that is not a *counterEntry")
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.seeded {
		return entry, nil
	}
	if a.summaries == nil {
		entry.seeded = true
		return entry, nil
	}
	existing, err := a.summaries.FindByID(tenantCtx, summaryID(feature, start))
	if err != nil && !hasCode(err, dbkit.ErrRecordNotFound.Code) {
		return nil, err
	}
	if err == nil {
		entry.quantity = existing.Quantity
		// The overage latch is reconstructed too: once the durable summary
		// holds a quantity at or above this bucket's threshold, the
		// crossing has happened -- whether the pre-restart process managed
		// to publish its event or died between the fold and the publish,
		// the latch must be set or the first post-restart event would fire
		// the crossing a second time for one period.
		if threshold, hasThreshold := a.thresholds.resolve(feature); hasThreshold && entry.quantity >= threshold {
			entry.notifiedOverage = true
		}
	}
	entry.seeded = true
	return entry, nil
}

// applyDelta folds delta into an already-seeded counter entry, returning
// the counter's new value and whether this call is the one that first
// crossed a configured overage threshold within this period. The caller
// hands in the entry ensureSeeded returned and runs this only after the
// event's usage has durably persisted -- the summary upsert, or the
// receipt-plus-summary transaction -- so the increment and the
// notifiedOverage latch below are never committed for an event whose
// persistence failed: a refused event leaves the latch open, and a later
// event that crosses can still be the one to publish
// EventOverageThresholdCrossed. IngestBillingGrade additionally calls it
// only when its fold actually applied the event (not for an
// alreadyIngested redelivery), since a redelivered event's delta is
// already inside the entry via its seed -- see that method's doc comment.
func (a *Aggregator) applyDelta(entry *counterEntry, feature string, delta float64) (quantity float64, crossed bool) {
	entry.mu.Lock()
	defer entry.mu.Unlock()

	entry.quantity += delta
	quantity = entry.quantity

	threshold, hasThreshold := a.thresholds.resolve(feature)
	if hasThreshold && !entry.notifiedOverage && quantity >= threshold {
		entry.notifiedOverage = true
		crossed = true
	}
	return quantity, crossed
}

// RealtimeCount returns the real-time counter's current value for
// (tenantID, feature) within the calendar period at contains, or (0, nil)
// when no event has been ingested for that bucket yet. A key this process
// has never touched is reconstructed from its durable UsageSummary row
// first (ensureSeeded), so a read immediately after a restart reflects
// the period's history rather than reporting zero until the first new
// event arrives.
func (a *Aggregator) RealtimeCount(tenantID, feature string, at time.Time) (float64, error) {
	start, _, err := periodBounds(at, a.bucket)
	if err != nil {
		return 0, err
	}
	key := realtimeKey(tenantID, feature, start)
	v, ok := a.counters.Load(key)
	if !ok {
		if a.summaries == nil {
			return 0, nil
		}
		tenantCtx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID(tenantID))
		entry, err := a.ensureSeeded(tenantCtx, tenantID, feature, start)
		if err != nil {
			return 0, err
		}
		v = entry
	}
	entry, ok := v.(*counterEntry)
	if !ok {
		// Unreachable: see ensureSeeded's identical guard.
		panic("metering: a.counters holds a value that is not a *counterEntry")
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.quantity, nil
}

// sweepExpiredCountersLocked evicts every resident counter entry whose
// period predates start -- the newest period boundary an Ingest has seen
// -- retiring the map's expired-period entries at most once per boundary
// crossed rather than on every call (sweptThrough remembers the last
// sweep's boundary; only a strictly newer start triggers another walk).
// Callers must hold a.mu, which both ingest paths do when they call it,
// so no concurrent add for a swept key can be mid-flight against the
// Delete; an add that committed before the sweep already landed in the
// key's summary row, and a later touch of the expired period re-seeds
// from that row, so eviction loses nothing (see the Aggregator type's own
// "Expired periods are evicted" doc comment).
func (a *Aggregator) sweepExpiredCountersLocked(start time.Time) {
	if !start.After(a.sweptThrough) {
		return
	}
	a.sweptThrough = start
	a.counters.Range(func(key, _ any) bool {
		if periodFromRealtimeKey(key).Before(start) {
			a.counters.Delete(key)
		}
		return true
	})
}

// periodFromRealtimeKey extracts the period start realtimeKey embedded in
// key (the RFC 3339 rendering after the last "|"). The last segment is
// always the period start, whatever the tenant or feature segments hold;
// an unparseable key (impossible for keys this Aggregator wrote) simply
// never matches the eviction predicate and stays resident.
func periodFromRealtimeKey(key any) time.Time {
	s, ok := key.(string)
	if !ok {
		return time.Time{}
	}
	idx := strings.LastIndex(s, "|")
	if idx < 0 {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s[idx+1:])
	if err != nil {
		return time.Time{}
	}
	return t
}

// upsertSummaryInto is the summary-upsert core Ingest and
// IngestBillingGrade's transaction path both use: the exact
// same find-or-create/update sequence, run against whichever
// *SummaryRepository the caller hands it -- a.summaries for the plain
// Ingest path, or one freshly built over an open transaction for
// IngestBillingGrade's atomic path (see that method's doc comment). The
// caller owns whatever serialization the chosen repo's connection needs
// (a.mu for a.summaries; a single already-locked call for a
// transaction-scoped repo, since GORM transactions are not safe for
// concurrent use from multiple goroutines).
func upsertSummaryInto(tenantCtx context.Context, summaries *SummaryRepository, feature string, start, end time.Time, delta float64) error {
	id := summaryID(feature, start)
	existing, err := summaries.FindByID(tenantCtx, id)
	if err != nil {
		if !hasCode(err, dbkit.ErrRecordNotFound.Code) {
			return err
		}
		return summaries.Create(tenantCtx, &UsageSummary{
			ID:          id,
			Feature:     feature,
			PeriodStart: start,
			PeriodEnd:   end,
			Quantity:    delta,
		})
	}
	existing.Quantity += delta
	existing.PeriodEnd = end
	return summaries.Update(tenantCtx, existing)
}

// upsertSummaryTx is upsertSummaryInto's transaction-scoped twin: the
// identical find-or-create/update sequence, composed directly against an
// already-open transaction handle (tx) rather than through
// dbkit.Repository[T] -- because foldIntoSummaryOnce's own
// dbkit.WithTenantSession call already opened that transaction. Routing
// through dbkit.Repository[T] here (as an earlier version of this function
// did, via NewSummaryRepository(tx)) would nest a second WithTenantSession
// transaction inside the first one; dbkit refuses that outright
// (dbkit.ErrNestedTenantSession) precisely because a nested call's own nil
// return does not mean the real, outermost transaction has genuinely
// committed -- see that error's own doc comment in tenant_session.go.
// Composing raw GORM calls against the tx a WithTenantSession callback
// already received is the documented shape for "several statements, one
// transaction" (go/dbkit/AGENTS.md's "Repository[T] growing a
// transactional batch-write seam" entry; go/org's tree.go and
// membership.go use the identical idiom against their own repositories).
//
// It writes no explicit "tenant_id = ?" clause -- unlike
// dbkit.Repository[T]'s own methods, which are dbkit's own defense-in-depth
// and exempt from this codebase's hand-written-tenant-filter discipline as
// the infrastructure that discipline is built on. tx still carries the
// tenant-scoping plugin every dbkit.Open connection installs (Aggregator
// never builds its own *gorm.DB), and UsageSummary implements
// dbkit.TenantScoped, so the plugin injects the filter on First/Save and
// forces the column on Create exactly as it would through Repository[T] --
// the same reliance go/org's tree.go and membership.go place on it inside
// their own dbkit.WithTenantSession callbacks.
func upsertSummaryTx(tx *gorm.DB, feature string, start, end time.Time, delta float64) error {
	id := summaryID(feature, start)
	var existing UsageSummary
	err := tx.Where("id = ?", id).First(&existing).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return tx.Create(&UsageSummary{
			ID:          id,
			Feature:     feature,
			PeriodStart: start,
			PeriodEnd:   end,
			Quantity:    delta,
		}).Error
	case err != nil:
		return err
	}
	existing.Quantity += delta
	existing.PeriodEnd = end
	return tx.Where("id = ?", id).Select("*").Save(&existing).Error
}

// IngestBillingGrade is Ingest's billing-grade-only sibling: Dispatcher
// calls this instead of Ingest so that redelivering the same event --
// exactly what happens when a crash or a transient failure leaves an
// outbox row "pending" after its delivery already committed -- is a safe,
// idempotent no-op rather than a second, silently double-counted
// application. See IngestReceipt's doc comment for the full crash-window
// argument and why the fix lives here rather than as a change to the
// shared Ingest -- AnalyticsRecorder keeps calling plain Ingest
// unaffected.
//
// Publishing the overage event, when IngestBillingGrade is the call that
// first crosses a configured threshold, follows the identical best-effort
// contract Ingest's own doc comment describes -- logged on failure, never
// causing this method itself to return an error.
func (a *Aggregator) IngestBillingGrade(ctx context.Context, event UsageEvent) error {
	if err := event.validate(); err != nil {
		return err
	}
	occurredAt := event.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	start, end, err := periodBounds(occurredAt, a.bucket)
	if err != nil {
		return err
	}

	tenantCtx := pkgcore.WithTenant(ctx, pkgcore.TenantID(event.TenantID))

	// The seed read runs before the fold, under the same mu acquisition:
	// the counter must be reconstructed from the summary state as it is
	// BEFORE this call's fold -- if the fold then commits, applyDelta adds
	// the delta on top of that base exactly once. When the fold reports
	// alreadyIngested instead, no delta is added at all: the seed read
	// happened after the earlier delivery's own fold committed, so the
	// entry already holds this event -- in the same process the earlier
	// delivery's applyDelta added it, and across a restart the seed read
	// reconstructed it from the summary row that fold wrote. Adding again
	// would double-count either way. This is what makes the crash window
	// between a fold's commit and its counter increment close across a
	// restart: the redelivered event's own delta is inside the summary,
	// and the seed hands it back to the counter (see the Aggregator
	// type's "Reconstruction after a restart" doc comment).
	a.mu.Lock()
	entry, err := a.ensureSeeded(tenantCtx, event.TenantID, event.Feature, start)
	var alreadyIngested bool
	if err == nil {
		alreadyIngested, err = a.foldIntoSummaryOnce(tenantCtx, event, start, end)
	}
	a.sweepExpiredCountersLocked(start)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	if alreadyIngested {
		// An earlier attempt's transaction already committed both the
		// receipt and the summary delta; this call applies nothing beyond
		// the reconstruction the seed already did. See IngestReceipt's doc
		// comment -- this is the recovered case, not an error.
		return nil
	}

	quantity, crossed := a.applyDelta(entry, event.Feature, event.Quantity)
	if crossed {
		a.publishOverageCrossed(ctx, event, start, end, quantity, occurredAt)
	}
	return nil
}

// foldIntoSummaryOnce atomically records event's idempotency receipt and
// folds its Quantity into the UsageSummary row, in one real database
// transaction opened on a.summaries' own connection (never Dispatcher's --
// see IngestReceipt's doc comment for why that independence matters). The
// transaction is dbkit.WithTenantSession itself, not a bare
// db.Transaction: both writes below run as raw GORM calls directly against
// the tx WithTenantSession hands its callback (see upsertSummaryTx's own
// doc comment for why -- routing either write back through
// dbkit.Repository[T] here would nest a second WithTenantSession
// transaction inside this one, which dbkit refuses outright).
//
// The receipt insert runs as ON CONFLICT DO NOTHING, never as a plain
// insert whose unique-violation error would have to be caught: a
// statement error inside a PostgreSQL transaction leaves the whole
// transaction aborted, so a catch-then-commit recovery would commit a
// poisoned transaction (the pgx driver reports the server's
// COMMIT-became-ROLLBACK as an error), and an SQLite-only suite -- where
// a failed statement is harmless -- could never see that. DO NOTHING
// never aborts anything on either dialect: a redelivered event's insert
// simply reports RowsAffected == 0 below, the healthy transaction commits
// having changed nothing, and the receipt's own existence -- which by
// construction implies an earlier, successful call already folded the
// summary in the same transaction -- is the whole answer.
// Reports (true, nil) when event.IdempotencyKey already has a receipt from
// an earlier, successful call, in which case the transaction commits
// having changed nothing.
//
// Callers must hold a.mu: a single WithTenantSession transaction is not a
// substitute for it here any more than dbkit.Repository[T]'s own
// transactions are for upsertSummary's plain path -- see the Aggregator
// type's own doc comment for why summary writes need a single serializing
// point at all.
func (a *Aggregator) foldIntoSummaryOnce(tenantCtx context.Context, event UsageEvent, start, end time.Time) (alreadyIngested bool, err error) {
	txErr := dbkit.WithTenantSession(tenantCtx, a.summaries.db, func(tx *gorm.DB) error {
		receipt := &IngestReceipt{ID: event.IdempotencyKey, TenantID: event.TenantID}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(receipt)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// The receipt already exists -- this event was folded in by an
			// earlier, successful call. Nothing to do; the transaction
			// commits having changed nothing.
			alreadyIngested = true
			return nil
		}
		return upsertSummaryTx(tx, event.Feature, start, end, event.Quantity)
	})
	if txErr != nil {
		return false, txErr
	}
	return alreadyIngested, nil
}

// publishOverageCrossed publishes an EventOverageThresholdCrossed for
// event, best-effort -- see Ingest's own doc comment for why a publish
// failure here is logged, not returned.
func (a *Aggregator) publishOverageCrossed(ctx context.Context, event UsageEvent, start, end time.Time, quantity float64, occurredAt time.Time) {
	if a.bus == nil {
		return
	}
	threshold, _ := a.thresholds.resolve(event.Feature)
	err := a.bus.Publish(ctx, pkgcore.Event{
		Type:     EventOverageThresholdCrossed,
		TenantID: pkgcore.TenantID(event.TenantID),
		Payload: OverageThresholdCrossedEvent{
			TenantID:    event.TenantID,
			Feature:     event.Feature,
			Threshold:   threshold,
			Quantity:    quantity,
			PeriodStart: start,
			PeriodEnd:   end,
			OccurredAt:  occurredAt,
		},
	})
	if err != nil {
		obs.FromContext(ctx).Warn("metering.overage_event_publish_failed",
			"error", err,
			"tenant_id", event.TenantID,
			"feature", event.Feature,
		)
	}
}
