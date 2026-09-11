package metering

import (
	"context"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"go.opentelemetry.io/otel/metric"

	"github.com/vislake/speed/go/dbkit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// counterEntry is one real-time quota counter: the running Quantity within
// one (tenant, feature, period) bucket, guarded by its own mutex so
// concurrent Ingest calls for the same key never lose an increment.
//
// notifiedOverage latches once EventOverageThresholdCrossed has actually
// been DELIVERED for this bucket -- the latch is set only after the bus
// publish succeeded (see applyDelta and deliverOverageCrossing), never
// before, so a publish failure leaves the latch open for the next crossing
// fold to retry: a transient bus failure delays the overage signal, it
// never consumes it. With the latch set, a threshold that stays crossed
// for the rest of the period publishes exactly one event rather than one
// per subsequent event -- resetting only implicitly, when a new period's
// counterEntry is created fresh (see realtimeKey embedding the period
// start in its key).
//
// publishPending marks the crossing whose publish is currently in flight:
// the fold that set it owns the delivery, and no other fold can claim the
// crossing while it stands, so the edge fires exactly once even when
// folds race the in-flight publish. deliverOverageCrossing clears it and
// sets notifiedOverage (or leaves it open) when the publish resolves.
//
// seeded records that this entry has been reconstructed from the durable
// UsageSummary row for its bucket (ensureSeeded): the first touch of a
// key in a process -- a restart, or merely a key no event has hit since
// the process started -- starts from the summary's quantity rather than
// from zero, and latches notifiedOverage when that quantity already meets
// the bucket's threshold AND the row attests that the current threshold
// was the one in force when the row was last folded (the row's
// OverageThreshold equals the current effective threshold -- see
// Aggregator's "Reconstruction after a restart" doc comment for why the
// attestation, not the quantity comparison alone, decides).
type counterEntry struct {
	mu              sync.Mutex
	quantity        float64
	notifiedOverage bool
	publishPending  bool
	seeded          bool
}

// Aggregator is the in-process aggregation backend, the one place both
// reliability tiers (AnalyticsRecorder's flush loop and Dispatcher's
// delivery loop) funnel measured usage into. It maintains:
//
//   - A real-time quota counter per (tenant, feature, period), held in an
//     in-process sync.Map -- RealtimeCount reads it back.
//   - A database-backed UsageSummary row per (tenant, feature, period),
//     upserted through SummaryRepository.
//   - The overage-threshold check that publishes
//     EventOverageThresholdCrossed on the wired pkgcore.EventBus.
//
// # Summary folds are database-arbitrated; mu orders, it does not arbitrate
//
// dbkit.Repository[T]'s Create/FindByID/Update each open their OWN
// transaction (dbkit.WithTenantSession), so a naive
// "FindByID, then Create-or-Update" sequence run without external
// coordination is a lost-update race under concurrent Ingest calls for
// the same (tenant, feature, period) key. A mutex around a Go-level
// read-modify-write is correct only while "this process" is the whole
// deployment (the shipped aggregation backend is the in-process one);
// it is not correct once a second replica exists: no other process
// shares that mutex, and a lost fold there is a SILENT one -- the
// outbox row is already marked delivered and its receipt committed, so
// nothing ever retries or compensates the delta the clobbered fold
// dropped.
//
// Both fold paths therefore write the summary through ONE
// database-arbitrated statement -- an INSERT ... ON CONFLICT DO UPDATE
// whose conflict branch does the arithmetic server-side (upsertSummaryTx,
// the shape go/billing's credit ledger established in applyBalanceDelta,
// with the create half folded into the same statement since a summary
// row's birth is its first delta). Two folds of the same row are
// serialized by the database's own row locking on the (id, tenant_id)
// primary key -- plain, portable SQL that runs identically on SQLite and
// PostgreSQL -- so the second fold to run sees the first's already-applied
// quantity and adds onto it, never a stale read a Go-level
// read-modify-write could race on, within one process or across many.
//
// mu stays, but its job narrows to in-process ordering: each event's seed
// read (ensureSeeded) must run under the same mu acquisition as its own
// fold, so an entry is never reconstructed from a summary that already
// holds that event's delta and then folds it a second time; and the
// expired-period sweep must not evict a counter a concurrent fold is
// mid-flight on (see sweepExpiredCountersLocked). Real-time counters stay
// per-process state by design -- a second replica's folds land in the
// shared summary through the same atomic statement, and its counters
// reconstruct from that summary on first touch. A distributed aggregation
// backend that would replace the in-process counters too is not
// implemented.
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
// EventOverageThresholdCrossed for one period. Aggregator therefore
// reconstructs each counter from
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
// The latch half of that reconstruction is qualified by threshold
// identity: the equivalence "quantity >= threshold means the crossing
// happened" holds only while the threshold is unchanged, and thresholds
// ARE changeable -- they are construction-time values
// (WithOverageThresholds mutates the Aggregator before the assembly
// returns), so changing one, an operator lowering a limit being the
// routine shape, happens exactly across a restart. A post-restart seed
// that latched on the quantity comparison alone would then treat "the
// threshold moved below an already-existing quantity" as "the crossing
// was published", and the crossing under the new threshold would never
// fire for the period -- a silently absent overage signal, the failure
// direction this design treats as worse than a bounded duplicate. Every
// fold therefore records on the summary
// row the effective threshold that was in force for its feature at that
// fold (UsageSummary.OverageThreshold, written in the same statement as
// the quantity -- see upsertSummaryTx), and the seed latches only when
// the row attests that the CURRENT threshold was in force at its last
// fold: *recorded == current. An unattested row -- one last folded under
// a different threshold, under none at all, or before migration 0006 --
// leaves the latch open, so the first post-restart fold that reaches the
// threshold is the crossing event under the current configuration, and a
// RealtimeCount on such a row reconstructs the quantity without latching
// or publishing anything: a read cannot swallow a signal only an event
// fold may deliver. Under a same-configuration restart the recorded
// value equals the current threshold, so the latch is set and the
// crossing does not refire. See the 0006 migration's header comment for
// the bounded duplicate-fire residual the unattested state leaves on the
// one period straddling an upgrade.
//
// # Expired periods are evicted, never resident forever
//
// A counterEntry lives per (tenant, feature, period), and periods end:
// without eviction the map would grow without bound for the process
// lifetime, one entry per bucket a tenant ever used. Ingest and
// IngestBillingGrade therefore sweep, under mu and at most once per
// period boundary crossed, every entry whose period predates the event's
// own -- see sweepExpiredCountersLocked. The
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

	counters sync.Map // counterKey -> *counterEntry
	mu       sync.Mutex

	// aggDuration carries metering.aggregation.duration (metrics.go),
	// registered by NewAggregator; nil for a bare struct literal.
	aggDuration metric.Float64Histogram
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
//
// summaries is the repository both ingest paths persist through and
// RealtimeCount reconstructs an uncounted bucket from. The constructor's
// contract is an Aggregator over summaries (module.go's NewModule always
// passes NewSummaryRepository(db)'s answer); this documentation never
// claimed nil support -- it simply never refused it. A nil repository is
// an unconfigured construction, and every operation that needs the
// repository refuses it with the coded ErrUsageSummariesUnconfigured:
// RealtimeCount on a counter miss (answering (0, nil) would be a silent
// zero, an exactly-legal quota value -- zero usage means certainly within
// quota -- for a reader that cannot answer at all), and both ingest paths
// through ensureSeeded, whose refusal keeps their folds from ever
// reaching the nil repository's connection (see that error's doc
// comment).
func NewAggregator(summaries *SummaryRepository) *Aggregator {
	return &Aggregator{
		summaries:   summaries,
		bucket:      defaultPeriodBucket,
		aggDuration: registerAggregationDurationMetric(),
	}
}

// counterKey is one counterEntry's map key: the (tenantID, feature,
// periodStart) identity triple itself, used directly as the key of
// a.counters rather than through a serialized encoding. It embeds
// periodStart (not just tenant and feature) so a new calendar period gets
// a fresh zero-valued counter automatically, with no explicit reset logic
// needed anywhere -- LoadOrStore below simply finds nothing for the new
// key and creates one.
//
// The key must not be a concatenation of its segments around a
// separator. The durable summary id can rely on such an encoding,
// because there the tenant rides in its own primary-key column and only
// the feature and period segments travel in-band (see summaryID's doc
// comment); the counter map is flat, so its
// key carries the tenant in-band too, and tenantID and feature are both
// variable-length values neither this module nor the layers beneath it
// restrict against "|" (UsageEvent.validate bounds length only;
// pkgcore.TenantID is an unrestricted string). Concatenating two
// variable-length segments around an unescaped "|" is ambiguous whenever
// a segment contains the separator: ("a", "b|c") and ("a|b", "c") both
// encode to "a|b|c|" + the period, which would merge two distinct
// (tenant, feature) buckets into one counter entry and one overage latch
// -- a silent misattribution across (tenant, feature) boundaries in the
// billing-grade counter. A comparable struct key has no encoding to be
// ambiguous: map equality is field-wise, so two distinct triples can
// never collide, whatever the segments contain. The key lives only in
// this process's map (counters die with the process; restart
// reconstruction reads the durable UsageSummary rows), so its shape is
// package-internal and carries no format-compatibility obligation.
type counterKey struct {
	tenantID    string
	feature     string
	periodStart time.Time
}

// realtimeKey returns the counterKey identifying one counterEntry for
// (tenantID, feature) within the period starting at periodStart.
func realtimeKey(tenantID, feature string, periodStart time.Time) counterKey {
	// periodStart is canonicalized to UTC: period bounds already are UTC
	// (periodBounds), and the normalization keeps key identity indifferent
	// to the location a caller's clock happens to carry.
	return counterKey{
		tenantID:    tenantID,
		feature:     feature,
		periodStart: periodStart.UTC(),
	}
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
// publishes. The order is load-bearing: had the counter delta and the
// latch run first, a summary-write failure would leave the latch set for
// a delta the database never received -- no subsequent event would ever
// cross again, and the crossing would be lost forever.
//
// Publishing the overage event, if one fires, is best-effort: a publish
// failure is logged and does NOT fail Ingest -- the usage measurement
// itself (the summary row, then the counter increment) has already
// committed by that point, and failing the whole call over a secondary
// notification signal would make a real, already-durable measurement look
// like it was lost, which is worse than a missed notification. Best-effort
// must not mean lossy, though: the notifiedOverage latch is set only once
// the publish has succeeded (applyDelta claims the crossing,
// deliverOverageCrossing latches it on success and leaves it open on
// failure), so a failed publish leaves the next fold that finds the bucket
// still above the threshold as the crossing event and it retries the
// delivery -- a transient bus failure delays the overage signal, never
// consumes it.
func (a *Aggregator) Ingest(ctx context.Context, event UsageEvent) error {
	start := time.Now()
	defer func() {
		recordAggregationDuration(ctx, a.aggDuration, aggregationChannelAnalytics, start)
	}()
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
		err = upsertSummaryInto(tenantCtx, a.summaries, event.Feature, start, end, event.Quantity, a.foldThreshold(event.Feature))
	}
	a.sweepExpiredCountersLocked(start)
	a.mu.Unlock()
	if err != nil {
		return err
	}

	quantity, crossed := a.applyDelta(entry, event.Feature, event.Quantity)
	if crossed {
		a.deliverOverageCrossing(ctx, entry, event, start, end, quantity, occurredAt)
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
// an error). An Aggregator built with no summaries repository
// (NewAggregator(nil)) is NOT a pure-counter construction: it has no
// durable state to reconstruct from and no fold could persist anywhere,
// so the seed refuses it with the coded ErrUsageSummariesUnconfigured
// before any counter entry is created (module.go's NewModule always
// passes NewSummaryRepository(db)'s answer -- see that error's doc
// comment for why the nil form is unconfigured, not sanctioned). On a
// genuine read error the entry is left unseeded and the error returned,
// so a later call retries the reconstruction rather than silently
// running from zero past a durable history.
func (a *Aggregator) ensureSeeded(tenantCtx context.Context, tenantID, feature string, start time.Time) (*counterEntry, error) {
	// An Aggregator built with no summaries repository (NewAggregator(nil))
	// cannot seed anything. The refusal must stand before the LoadOrStore
	// below: seeding to zero -- or even merely inserting an unseeded entry
	// -- would let the ingest paths' folds reach the nil repository's
	// connection and crash on it, and would leave a lingering entry a
	// later read could misreport as a real zero. Refusing up front keeps
	// every operation on the unconfigured object on the same coded error
	// path (see ErrUsageSummariesUnconfigured's doc comment).
	if a.summaries == nil {
		return nil, ErrUsageSummariesUnconfigured
	}
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
	existing, err := a.summaries.FindByID(tenantCtx, summaryID(feature, start))
	if err != nil && !dbkit.IsRecordNotFound(err) {
		return nil, err
	}
	if err == nil {
		entry.quantity = existing.Quantity
		// The overage latch is reconstructed too, qualified by threshold
		// identity: the equivalence "a durable quantity at or above the
		// threshold means the crossing has happened" holds only while the
		// threshold the row was folded under IS the threshold now in force
		// -- an attested crossing, whether the pre-restart process
		// published its event or died between the fold and the publish,
		// must not fire a second time for one period. The row records the
		// threshold that was in force
		// at its last fold (UsageSummary.OverageThreshold), so the latch
		// is set only when that record equals the current effective
		// threshold; a row last folded under a different threshold (an
		// operator lowering one is exactly this restart), under none at
		// all, or before the recording column existed, attests nothing
		// about the current configuration, and the first post-restart
		// fold that finds the bucket at or above the current threshold is
		// the crossing event under it. Because a read-only touch may not
		// publish, it must not latch either: this rule leaves an
		// unattested row open for RealtimeCount's map-miss seed too, so a
		// pure read can never swallow the lowered threshold's signal.
		threshold, hasThreshold := a.thresholds.resolve(feature)
		if hasThreshold && entry.quantity >= threshold && existing.OverageThreshold != nil && *existing.OverageThreshold == threshold {
			entry.notifiedOverage = true
		}
	}
	entry.seeded = true
	return entry, nil
}

// applyDelta folds delta into an already-seeded counter entry, returning
// the counter's new value and whether this call CLAIMED the overage
// crossing: the first fold within this period whose quantity reaches or
// passes a configured threshold while no crossing is latched
// (notifiedOverage) and none is mid-delivery (publishPending). The caller
// hands in the entry ensureSeeded returned and runs this only after the
// event's usage has durably persisted -- the summary upsert, or the
// receipt-plus-summary transaction -- so the fold and any crossing claim
// below are never made for an event whose persistence failed: a refused
// event leaves the latch open, and a later event that crosses can still
// be the one to publish EventOverageThresholdCrossed. IngestBillingGrade
// additionally calls it only when its fold actually applied the event
// (not for an alreadyIngested redelivery), since a redelivered event's
// delta is already inside the entry via its seed -- see that method's doc
// comment.
//
// A claimed crossing is not latched here: applyDelta only marks the
// delivery as pending (publishPending), so no other fold can claim the
// crossing while the publish runs -- the edge fires exactly once -- and
// the caller then completes the claim through deliverOverageCrossing,
// which sets notifiedOverage only once the publish has succeeded. The
// latch must follow the effect it guards, never precede it: a latch set
// before an unconfirmed publish is a latch a failed publish can consume,
// after which no later fold in the period can ever fire the crossing
// again -- the overage signal lost permanently.
func (a *Aggregator) applyDelta(entry *counterEntry, feature string, delta float64) (quantity float64, crossed bool) {
	entry.mu.Lock()
	defer entry.mu.Unlock()

	entry.quantity += delta
	quantity = entry.quantity

	threshold, hasThreshold := a.thresholds.resolve(feature)
	if hasThreshold && !entry.notifiedOverage && !entry.publishPending && quantity >= threshold {
		entry.publishPending = true
		crossed = true
	}
	return quantity, crossed
}

// deliverOverageCrossing completes a crossing applyDelta claimed: it
// publishes EventOverageThresholdCrossed and then settles the claim --
// the notifiedOverage latch is set ONLY once the publish has succeeded,
// and a failed publish leaves the latch open (only the in-flight marker
// is released), so the next fold that finds the bucket still above the
// threshold is the crossing event again and retries the delivery. A
// publish failure is logged, never surfaced: the usage measurement has
// already committed by this point (see Ingest's own doc comment for why
// the whole call must not fail over a secondary notification signal).
//
// The settle happens under the entry's mutex so the claim lifecycle is
// race-free: while publishPending stands no other fold may claim (see
// applyDelta), and the release and the latch are one atomic step, so a
// failed publish can never be followed by a second publisher racing the
// first one's settle. The publish itself runs OUTSIDE the entry's lock --
// the bus may block (an in-process bus dispatches to subscribers
// synchronously), and no subscriber should ever run under a counter lock.
func (a *Aggregator) deliverOverageCrossing(ctx context.Context, entry *counterEntry, event UsageEvent, start, end time.Time, quantity float64, occurredAt time.Time) {
	err := a.publishOverageCrossed(ctx, event, start, end, quantity, occurredAt)

	entry.mu.Lock()
	entry.publishPending = false
	if err == nil {
		entry.notifiedOverage = true
	}
	entry.mu.Unlock()

	if err != nil {
		obs.FromContext(ctx).Warn("metering.overage_event_publish_failed",
			"error", err,
			"tenant_id", event.TenantID,
			"feature", event.Feature,
		)
	}
}

// RealtimeCount returns the real-time counter's current value for
// (tenantID, feature) within the calendar period at contains, or (0, nil)
// when no event has been ingested for that bucket yet. A key this process
// has never touched is reconstructed from its durable UsageSummary row
// first (ensureSeeded), so a read immediately after a restart reflects
// the period's history rather than reporting zero until the first new
// event arrives.
//
// A counter miss on an Aggregator built with no summaries repository --
// NewAggregator(nil), an unconfigured construction the module path never
// makes -- returns ErrUsageSummariesUnconfigured rather than (0, nil): the
// miss can only be answered by reconstructing from durable state, and
// answering zero for a reader that cannot know is a silent zero -- an
// exactly-legal quota answer (zero usage means within quota) -- for what
// is really a configuration error. Both ingest paths refuse the same
// construction with the same coded error through their ensureSeeded seed,
// so no operation on the nil-summaries object half-works: no fold ever
// reaches the nil repository's connection and no read ever answers a
// value it cannot know. See that error's doc comment for the alignment
// with go/billing's own refusal vocabulary.
func (a *Aggregator) RealtimeCount(tenantID, feature string, at time.Time) (float64, error) {
	start, _, err := periodBounds(at, a.bucket)
	if err != nil {
		return 0, err
	}
	key := realtimeKey(tenantID, feature, start)
	v, ok := a.counters.Load(key)
	if !ok {
		if a.summaries == nil {
			return 0, ErrUsageSummariesUnconfigured
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
// Callers must hold a.mu to call it (sweptThrough is a.mu-guarded), as
// both ingest paths do -- but that exclusion does NOT cover an event's
// counter add: the delta each event applies to its entry lands later,
// in applyDelta OUTSIDE a.mu (see Ingest), so an applyDelta for a swept
// key CAN be mid-flight against the Delete below. That is harmless, for
// two independent reasons. First, entry.mu -- not a.mu -- is the lock
// that serializes an entry's mutations: applyDelta and
// deliverOverageCrossing always take it, so the Delete removing the
// entry from the map races nothing they do to the object (any code that
// reads or writes an entry while holding only a.mu would race them;
// entry.mu is the rule for touching an entry). Second, eviction can
// never strand a delta: applyDelta runs only after its event's own fold
// committed, and folds and this sweep are mutually exclusive under a.mu,
// so every delta whose applyDelta is still in flight when the sweep
// deletes its key has already landed in the key's summary row -- a later
// touch of the expired period re-seeds from that row, and eviction loses
// nothing (see the Aggregator type's own "Expired periods are evicted"
// doc comment).
func (a *Aggregator) sweepExpiredCountersLocked(start time.Time) {
	if !start.After(a.sweptThrough) {
		return
	}
	a.sweptThrough = start
	a.counters.Range(func(key, _ any) bool {
		k, ok := key.(counterKey)
		if ok && k.periodStart.Before(start) {
			a.counters.Delete(key)
		}
		return true
	})
}

// foldThreshold returns the overage threshold in force for feature as the
// value every summary fold records on its row (see
// UsageSummary.OverageThreshold), or nil when no threshold applies to the
// feature. The resolved EFFECTIVE value is the record: an equality between
// two configurations that resolve to the same value (a threshold moved
// from PerFeature to Default at the same number, say) is the same
// configuration for the restart-reconstruction attestation, and one that
// resolves to a different value is not, whatever the map it came from.
func (a *Aggregator) foldThreshold(feature string) *float64 {
	if t, ok := a.thresholds.resolve(feature); ok {
		return &t
	}
	return nil
}

// upsertSummaryInto folds delta into the summary row for (feature, start)
// through a.summaries' own connection, in ONE dbkit.WithTenantSession
// transaction that runs the same atomic upsert statement the billing-grade
// path runs inside its own transaction (upsertSummaryTx) -- see that
// function's doc comment for why the fold is a single
// INSERT ... ON CONFLICT DO UPDATE rather than a read-modify-write.
// Routing the fold through dbkit.Repository[T]'s FindByID/Create/Update
// would make it a Go-level read-modify-write that only Aggregator's
// in-process mu serializes -- a lost-update race the moment two replicas
// fold the same bucket, since no other process shares that mutex.
// Repository[T] cannot express the server-side arithmetic the atomic fold
// needs (the identical reason go/billing's applyBalanceDelta steps
// outside its own repository), so this function composes a raw GORM call
// against the tx a WithTenantSession callback receives, the documented
// "several statements, one transaction" shape go/org's tree.go and
// membership.go use against their own repositories -- with the difference
// that this upsert is expressed through the ORM's Create and the
// tenant-scoping plugin, never hand-written SQL (see upsertSummaryTx's doc
// comment).
//
// The caller (Ingest) holds a.mu across this call and the ensureSeeded
// read that precedes it, so each event's seed read stays ordered before
// its own fold; the summary row itself needs no in-process serialization
// at all. threshold is the fold-time in-force threshold (foldThreshold's
// answer) that travels into the row with the delta -- see
// UsageSummary.OverageThreshold for why quantity and the threshold it was
// accumulated under must land in the same statement.
func upsertSummaryInto(tenantCtx context.Context, summaries *SummaryRepository, feature string, start, end time.Time, delta float64, threshold *float64) error {
	return dbkit.WithTenantSession(tenantCtx, summaries.db, func(tx *gorm.DB) error {
		return upsertSummaryTx(tx, feature, start, end, delta, threshold)
	})
}

// upsertSummaryTx is the one summary fold, in one database-arbitrated
// statement, shared by both ingest paths: an INSERT ... ON CONFLICT DO
// UPDATE whose insert branch creates the bucket's row at quantity delta
// (the row's birth is its first fold -- a summary row exists only to hold
// quantity, there is no zero-materialize-then-accumulate phase to keep
// separate) and whose conflict branch adds delta to the existing row's
// quantity server-side, in the same statement. This is the credit-ledger
// shape go/billing's applyBalanceDelta established: two concurrent
// statements against the same row are serialized by the database's own
// row locking -- ordinary, portable SQL that runs identically on SQLite
// and PostgreSQL, no dialect-specific atomic-increment feature required
// -- so the second to run sees the first's already-applied change, never
// a stale read a Go-level read-modify-write could race on. Billing's own
// ensureBalance create half (an INSERT ... ON CONFLICT DO NOTHING, the
// same dialect-neutral upsert clause go/config's store.put uses) is
// folded into the same statement, since billing materializes a balance at
// zero before its first delta and metering has no equivalent phase to
// sequence.
//
// The DO UPDATE row lock on the (id, tenant_id) primary key is what makes
// concurrent folds safe across processes, not just across goroutines: two
// replicas folding the same bucket serialize on the row, the second fold
// adds onto the first's committed quantity, and no fold ever commits a
// stale read of the row the way a Go-level find-mutate-save sequence
// would (a lost fold there is silent -- the outbox row is already marked
// delivered and its receipt committed, with no compensation path; see the
// Aggregator type's "Summary folds are database-arbitrated" doc comment).
//
// The statement also carries the fold's context into the row: threshold
// -- the effective overage threshold in force for feature at this fold
// (foldThreshold's answer, nil when none applies) -- is written into
// UsageSummary.OverageThreshold by both branches of the statement, the
// same way period_end rides along on every fold. The restart
// reconstruction reads that record to decide whether the crossing under
// the currently configured threshold has happened (see the Aggregator
// type's "Reconstruction after a restart" doc comment and the 0006
// migration's header), so the record must land in the SAME
// database-arbitrated statement as the quantity arithmetic: a fold that
// commits a delta under a new configuration but fails to commit the
// threshold it was folded under would leave the row attesting the old
// configuration, and a later restart would treat the quantity as
// unattested history and re-fire a crossing the fold already delivered.
//
// tx must be a transaction whose context carries the tenant (a
// dbkit.WithTenantSession callback's tx): the tenant-scoping plugin every
// dbkit.Open connection installs forces the tenant_id column on the
// insert from the context, and the conflict target names both primary-key
// columns, so the statement writes no explicit "tenant_id = ?" clause and
// needs none -- unlike billing's raw-Exec shape, this upsert is expressed
// entirely through the ORM (a Create of a concrete UsageSummary), so every
// isolation layer (the plugin's injected filter and forced column, and
// PostgreSQL's row-level-security session GUC on the WithTenantSession
// path) applies exactly as it would to a Repository[T] write. The conflict
// branch is likewise an ordinary DO UPDATE, never an error a caller would
// have to catch: on PostgreSQL a caught unique-violation error would leave
// the transaction aborted, and this statement never aborts anything on
// either dialect -- the same non-poisoning property foldIntoSummaryOnce's
// receipt insert relies on (see that method's doc comment).
func upsertSummaryTx(tx *gorm.DB, feature string, start, end time.Time, delta float64, threshold *float64) error {
	id := summaryID(feature, start)
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}, {Name: "tenant_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			// Server-side arithmetic on the row's current value -- the
			// create branch and the update branch of one statement,
			// serialized by the database itself. The existing-row reference
			// is qualified with the table's own name because PostgreSQL
			// demands it spelled out -- an unqualified RHS is ambiguous
			// there (SQLSTATE 42702, the name being in scope against both
			// the target row and the excluded row) -- and SQLite accepts
			// the identical qualified form, so one statement serves both
			// dialects. overage_threshold likewise takes the insert
			// branch's value (excluded), so every fold -- the row's birth
			// included -- records the threshold it was folded under.
			"quantity":          gorm.Expr(tableUsageSummaries + ".quantity + excluded.quantity"),
			"period_end":        gorm.Expr("excluded.period_end"),
			"overage_threshold": gorm.Expr("excluded.overage_threshold"),
			"updated_at":        gorm.Expr("excluded.updated_at"),
		}),
	}).Create(&UsageSummary{
		ID:               id,
		Feature:          feature,
		PeriodStart:      start,
		PeriodEnd:        end,
		Quantity:         delta,
		OverageThreshold: threshold,
	}).Error
}

// IngestBillingGrade is Ingest's billing-grade-only sibling: Dispatcher
// calls this instead of Ingest so that redelivering the same event --
// exactly what happens when a crash or a transient failure leaves an
// outbox row "pending" after its delivery already committed -- is a safe,
// idempotent no-op rather than a second, silently double-counted
// application. See IngestReceipt's doc comment for the full crash-window
// argument and why the idempotency gate sits on this method rather than
// on the shared Ingest -- AnalyticsRecorder keeps calling plain Ingest
// unaffected.
//
// Publishing the overage event, when IngestBillingGrade is the call that
// first crosses a configured threshold, follows the identical contract
// Ingest's own doc comment describes -- best-effort, logged on failure,
// never causing this method itself to return an error, and latched only
// after the publish has succeeded (applyDelta claims the crossing,
// deliverOverageCrossing settles it), so a failed publish leaves the
// latch open for the next crossing fold to retry.
func (a *Aggregator) IngestBillingGrade(ctx context.Context, event UsageEvent) error {
	start := time.Now()
	defer func() {
		recordAggregationDuration(ctx, a.aggDuration, aggregationChannelOutbox, start)
	}()
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
		alreadyIngested, err = a.foldIntoSummaryOnce(tenantCtx, event, start, end, a.foldThreshold(event.Feature))
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
		a.deliverOverageCrossing(ctx, entry, event, start, end, quantity, occurredAt)
	}
	return nil
}

// foldIntoSummaryOnce atomically records event's idempotency receipt and
// folds its Quantity into the UsageSummary row, in one real database
// transaction opened on a.summaries' own connection (never Dispatcher's --
// see IngestReceipt's doc comment for why that independence matters). The
// transaction is dbkit.WithTenantSession itself, not a bare
// db.Transaction: both writes below run as raw GORM calls directly against
// the tx WithTenantSession hands its callback (the summary fold because
// upsertSummaryTx is the module's atomic upsert core -- see its doc
// comment -- and routing either write back through dbkit.Repository[T]
// would nest a second WithTenantSession transaction inside this one,
// which dbkit refuses outright).
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
// having changed nothing. threshold is the fold-time in-force threshold
// that travels into the summary row with the delta (see
// upsertSummaryTx's doc comment), foldThreshold's answer.
//
// Callers must hold a.mu, but not to serialize the summary fold -- the
// fold's atomic upsert statement is serialized by the database itself
// (see the Aggregator type's "Summary folds are database-arbitrated" doc
// comment). a.mu orders the seed read ahead of this fold in-process, so a
// counter entry is never reconstructed from a summary that already holds
// this event's delta and then folds it a second time, and it keeps the
// expired-period sweep from evicting a counter mid-flight.
func (a *Aggregator) foldIntoSummaryOnce(tenantCtx context.Context, event UsageEvent, start, end time.Time, threshold *float64) (alreadyIngested bool, err error) {
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
		return upsertSummaryTx(tx, event.Feature, start, end, event.Quantity, threshold)
	})
	if txErr != nil {
		return false, txErr
	}
	return alreadyIngested, nil
}

// publishOverageCrossed publishes an EventOverageThresholdCrossed for
// event and returns the bus error -- or nil when no bus is wired, which is
// not a failure (a bus-less Aggregator is a valid, purely measuring
// construction). The caller (deliverOverageCrossing) owns the failure
// handling: the latch decision depends on this error, and the warning is
// logged there with the latch context around it, never here.
func (a *Aggregator) publishOverageCrossed(ctx context.Context, event UsageEvent, start, end time.Time, quantity float64, occurredAt time.Time) error {
	if a.bus == nil {
		return nil
	}
	threshold, _ := a.thresholds.resolve(event.Feature)
	return a.bus.Publish(ctx, pkgcore.Event{
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
}
