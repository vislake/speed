package dbkit

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/vislake/speed/go/pkgcore"
)

// auditCapturePluginName identifies auditCapturePlugin to GORM (gorm.Plugin.Name
// and db.Plugins) and prefixes the callback names it registers, mirroring
// tenantScopePluginName's own convention so the two plugins' callbacks can
// never collide with GORM's own "gorm:*" callbacks, each other, or another
// plugin's.
const auditCapturePluginName = "dbkit:audit_capture"

// EventWriteCaptured is the pkgcore.Event.Type auditCapturePlugin publishes
// after every successful write against an Auditable model, following the
// "<module>.<entity>.<action>" convention (backend coding standard §8).
// Its Payload is a WriteCapturedEvent.
//
// dbkit itself is not a pkgcore.Module and so has no Register method of its
// own to declare this event on a Registry's EventRegistrar — the audit
// persister module (go/dbkit/audit's Module) declares it on dbkit's behalf,
// since that module is this event's one real subscriber.
const EventWriteCaptured = "dbkit.write.captured"

// WriteCapturedEvent is the Payload carried by an EventWriteCaptured event.
// Actor, OnBehalfOf and TenantID are read from the write's own context and
// embedded here as plain fields — never left for a subscriber to re-derive
// from ctx — because the distributed deployment mode's EventBus delivers
// asynchronously across a real network hop (Redis Streams): a subscriber's
// ctx is not the publisher's ctx, so anything the audit trail needs must
// travel on the event itself. This mirrors
// tenancy.SystemContextEnteredEvent's identical choice for the same reason.
type WriteCapturedEvent struct {
	// Actor is the acting identity captured from the write's context
	// (pkgcore.ActorFromContext), the zero pkgcore.Actor when none was set.
	Actor pkgcore.Actor
	// OnBehalfOf is the real administrator behind an impersonated Actor,
	// captured from pkgcore.OnBehalfOfFromContext. Nil when the write's
	// context carried none — the ordinary, non-impersonated case.
	OnBehalfOf *pkgcore.Actor
	// TenantID is the tenant of the row the write actually affected. For a
	// model implementing TenantScoped it is the write's ctx tenant, which the
	// co-installed tenantScopePlugin has already enforced -- forced tenant_id
	// column on Create, WHERE clause on Update/Delete -- as the row's real
	// tenant by the time capture runs. For a model that does not implement
	// TenantScoped at all (a platform- or identity-domain Auditable model) it
	// is always empty, whatever the ctx carries: such a row belongs to no
	// tenant, so a ctx tenant that happens to be present for reasons entirely
	// unrelated to the row must never be stamped onto it. See stampTenantID
	// for the full reasoning, including the no-ctx-tenant fallback for a
	// TenantScoped model.
	TenantID string
	// Table is the SQL table the write targeted.
	Table string
	// ResourceType is the Auditable model's own AuditResourceType().
	ResourceType string
	// ResourceID is the affected row's primary-key "id" value, best-effort
	// extracted from the write's own field values (Create, Update) or its
	// WHERE clause (Delete) — see resourceIDFrom's doc comment for exactly
	// what shapes it recognizes. Empty when it could not be determined.
	ResourceID string
	// Operation is "create", "update" or "delete".
	Operation string
	// Before is the row's prior column values, keyed by DB column name.
	// The automatic capture mechanism never reads a pre-write snapshot of
	// its own — doing so would cost every audited write an extra SELECT —
	// so this is always nil for Create and Update. A caller wanting a real
	// before/after diff supplies one explicitly through audit.Emit's
	// Input.Changes instead; this field exists on the wire shape because a
	// future capture path (or a caller publishing this event type itself)
	// may populate it, and because it makes the "no diff was captured"
	// case explicit rather than ambiguous with an empty map.
	Before map[string]any
	// After is the row's column values following the write, keyed by DB
	// column name. Populated for Create and Update (where the model's own
	// field values are available); nil for Delete, since GORM's delete
	// callback does not repopulate the destination from the deleted row.
	//
	// For an Update that restricted which columns it wrote — an explicit
	// Select list, or an Omit list; Repository[T]'s soft-delete and Restore
	// writes are the canonical two-column-SELECT shape — After carries
	// exactly the columns the statement assigned, never a value for a
	// column the write did not touch: a Select-restricted update's payload
	// holds only the restricted columns, while an Omit-restricted struct
	// update's payload can carry values for columns the zero-value skip
	// leaves unwritten — either way, capturing the payload whole would
	// fabricate values for untouched columns whose real row values are
	// still in place (scopeAfterToWrittenColumns mirrors GORM's own
	// assignment decisions). For an unrestricted full-record Update, After
	// is the whole row.
	After map[string]any
	// OccurredAt is when the write was captured.
	OccurredAt time.Time
}

// Auditable marks a GORM model as eligible for dbkit's automatic
// write-capture plugin (installed by Open when Options.AuditBus is
// non-nil): every Create, Update or Delete against a model implementing it
// publishes a WriteCapturedEvent. It is a marker interface in the same
// spirit as TenantScoped — AuditResourceType is read once per write to
// label the resulting event, never used for anything else.
//
// A model that does not implement Auditable is completely unaffected by
// the plugin, exactly as a model that does not implement TenantScoped is
// unaffected by tenantScopePlugin: no callback so much as looks at it.
type Auditable interface {
	// AuditResourceType names the kind of resource this model represents
	// for the audit trail, for example "note" or "org.member". It has no
	// closed enumeration — every business module names its own resources.
	AuditResourceType() string
}

// newAuditCapturePlugin returns a ready-to-install auditCapturePlugin
// publishing to bus. bus must not be nil; Open only installs this plugin
// when Options.AuditBus is set.
func newAuditCapturePlugin(bus pkgcore.EventBus) *auditCapturePlugin {
	return &auditCapturePlugin{bus: bus}
}

// auditCapturePlugin is a gorm.Plugin that publishes a WriteCapturedEvent
// after every successful Create, Update or Delete against a model
// implementing Auditable. It is the automatic-first half of
// docs/internal/10-compliance-and-audit.md's collection design; the
// declarative-secondary half is go/dbkit/audit's Emit.
//
// It is unexported: Open is the one place that installs it, gated on
// Options.AuditBus being non-nil — nil (the zero value every pre-existing
// caller already has) means no capture is installed, so adding this field
// to Options is 100% backward compatible with every call site that existed
// before this plugin did.
//
// Registered callbacks run After the corresponding "gorm:*" callback, so
// db.RowsAffected and the model's own field values are available — but a
// write inside dbkit.WithTenantSession's transaction (every Repository[T]
// write, and the documented raw-SQL escape hatch) is not necessarily done
// resolving at that point: the real commit or rollback does not happen
// until the whole fn passed to WithTenantSession returns — arbitrarily many
// statements later, entirely outside this Process's own callback chain, and
// GORM's own BeginTransaction/CommitOrRollbackTransaction callbacks are
// no-ops for a write already running inside an open transaction (confirmed
// by reading gorm.io/gorm@v1.31.2/finisher_api.go's Begin and
// callbacks/transaction.go while designing this fix), so nothing in this
// Process's own chain — including its own "gorm:commit_or_rollback_
// transaction" step — reflects whether that surrounding transaction ever
// actually commits. Building the event to publish at capture time is
// therefore safe (every field it needs is already resolved), but actually
// publishing it here, synchronously, would be publishing before the write
// is known to have durably happened at all — exactly the bug this
// package's own history records (see docs 10's stale note on this file
// predicting the fix, and go/dbkit/AGENTS.md's "Audit trail collection"
// section).
//
// So capture (below) never publishes directly. It either appends the built
// event to a per-transaction *auditBuffer carried on the write's own
// context — installed by WithTenantSession, which drains and publishes the
// buffer itself only after its own db.Transaction call has returned nil,
// i.e. only once the surrounding transaction has genuinely committed — or,
// when no such buffer is present (a bare Create/Update/Delete against
// Open's plain *gorm.DB, relying on GORM's own implicit per-statement
// transaction rather than WithTenantSession), stashes the event on the
// current statement's GORM instance map for this plugin's own
// After("gorm:commit_or_rollback_transaction") callback (publishPending) to
// read back and publish. For this bare-write shape specifically, investigating
// GORM's own callback sort (see Initialize's doc comment) while designing
// this fix found that an After-only registration like either of this
// plugin's two per-Process callbacks in fact resolves to running at the very
// end of the compiled chain — after the real per-statement commit or
// rollback already happened — so capture's own pre-existing
// "if db.Error != nil { return }" guard already prevents building (let alone
// publishing) an event for a bare write whose own chain later fails. The
// two-callback split does not depend on that GORM-internal sort behavior to
// be correct, though: it makes "publish only once this Process's real
// transaction outcome is known" an explicit, named position
// (publishPending, After the commit-or-rollback step) rather than an
// accident of where an unqualified After("gorm:create") callback happens to
// land, and it is what makes the WithTenantSession-buffered path correct at
// all — GORM's own per-statement machinery has nothing to say about a
// transaction it never opened.
//
// A publish failure can therefore never roll anything back — by the time
// either path calls Publish, there is nothing left to roll back — so it is
// reported as a structured alert instead of a db.AddError injection (see
// auditPublishFailed), per docs/internal/10-compliance-and-audit.md's rule
// that an audit-write failure must alert and never be silently dropped.
type auditCapturePlugin struct {
	bus pkgcore.EventBus
}

// auditBufferCtxKey is the unexported context key WithTenantSession installs
// a *auditBuffer under. A dedicated unexported type, rather than a bare
// string, keeps this key from colliding with a key set by another package,
// mirroring go/pkgcore/tenant.go's own ctxKey pattern.
type auditBufferCtxKey struct{}

// auditBuffer accumulates the pkgcore.Event values captured during one
// WithTenantSession transaction, so they can be published only once that
// transaction has genuinely committed — never before, and never at all
// when it rolls back. It is safe for concurrent use, though nothing in
// this codebase drives one *gorm.DB transaction handle from more than one
// goroutine at a time; the mutex is cheap insurance, not a load-bearing
// requirement.
//
// The buffer is additionally savepoint-aware: GORM resolves an inner
// transaction block (tx.Transaction nested inside WithTenantSession's own
// transaction, or a manual tx.SavePoint/tx.RollbackTo pair) to a SAVEPOINT
// on the same connection (gorm.io/gorm@v1.31.2/finisher_api.go's
// Transaction), and such an inner block can roll back — discarding its
// writes — while the outer transaction still goes on to commit. Events
// captured inside a region a ROLLBACK TO SAVEPOINT later discards describe
// writes that never took effect, so publishing them after the outer commit
// would fabricate audit rows exactly like the pre-buffer phantom-event bug
// this mechanism exists to close. beginSavepoint/rollbackToSavepoint (fed by
// the plugin's raw-statement observer, see afterSavepointSQL) mark the
// event index each savepoint opened at and prune everything appended since
// a discarded savepoint, so only events whose writes survive the outer
// commit are ever drained.
type auditBuffer struct {
	mu         sync.Mutex
	events     []pkgcore.Event
	savepoints []auditSavepointMark
}

// auditSavepointMark records that a SAVEPOINT named name is open, and that
// eventsAt is the length of the buffer when it opened — every event
// appended at or after eventsAt belongs to the savepoint's region and must
// be pruned if a rollback-to discards the savepoint. Frames are ordered
// oldest first (a stack), since SQL savepoints nest: rolling back to one
// discards every later savepoint too.
type auditSavepointMark struct {
	name     string
	eventsAt int
}

// withAuditBuffer returns a copy of ctx carrying a fresh *auditBuffer, and
// that buffer itself, so a caller (WithTenantSession) can later drain
// exactly the events captured against contexts derived from the returned
// one — every statement issued against the *gorm.DB a db.Transaction
// closure receives, since GORM propagates the same context.Context to every
// such statement (confirmed by reading gorm.DB.Begin/WithContext/Session
// while designing this).
func withAuditBuffer(ctx context.Context) (context.Context, *auditBuffer) {
	buf := &auditBuffer{}
	return context.WithValue(ctx, auditBufferCtxKey{}, buf), buf
}

// auditBufferFromContext returns the *auditBuffer ctx carries, if any. The
// second result is false for a context WithTenantSession never derived —
// in particular, a bare write against Open's plain *gorm.DB.
func auditBufferFromContext(ctx context.Context) (*auditBuffer, bool) {
	buf, ok := ctx.Value(auditBufferCtxKey{}).(*auditBuffer)
	return buf, ok
}

// add appends evt to the buffer. Safe for concurrent use.
func (b *auditBuffer) add(evt pkgcore.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, evt)
}

// drain returns every event the buffer holds and empties it, so a second
// drain (there should never be one, but this makes it harmless rather than
// a duplicate-publish hazard) returns nothing. Whatever savepoint frames
// remain open are dropped with the events: WithTenantSession only drains
// after its transaction committed, and no rollback-to can target a
// savepoint after the transaction is over.
func (b *auditBuffer) drain() []pkgcore.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.events
	b.events = nil
	b.savepoints = nil
	return out
}

// beginSavepoint records that a SAVEPOINT named name opened on the
// transaction this buffer belongs to. Every event appended from now on is
// inside the savepoint's region until a matching rollback-to (or the
// transaction's end).
//
// Opening a savepoint with the name of one already open releases the older
// one first — both dialects' SAVEPOINT semantics — so any existing frame
// with the same name is dropped before the new frame is pushed: work done
// between the two savepoints belongs to no live region and stays captured
// until a rollback-to of some savepoint that opened before it.
func (b *auditBuffer) beginSavepoint(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.savepoints) - 1; i >= 0; i-- {
		if b.savepoints[i].name == name {
			b.savepoints = append(b.savepoints[:i], b.savepoints[i+1:]...)
			break
		}
	}
	b.savepoints = append(b.savepoints, auditSavepointMark{name: name, eventsAt: len(b.events)})
}

// rollbackToSavepoint prunes every event appended since the most recent
// open savepoint named name, and closes that savepoint and every savepoint
// opened after it: a ROLLBACK TO SAVEPOINT discards all work done since the
// target savepoint opened, inner savepoints included, and events describing
// discarded work must never be published. It mirrors the SQL semantics for
// a rollback-to of a savepoint that no longer exists (rolled back already,
// or never opened): the database ignores it, so the buffer prunes nothing.
func (b *auditBuffer) rollbackToSavepoint(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := len(b.savepoints) - 1; i >= 0; i-- {
		if b.savepoints[i].name != name {
			continue
		}
		cut := b.savepoints[i].eventsAt
		for j := cut; j < len(b.events); j++ {
			b.events[j] = pkgcore.Event{}
		}
		b.events = b.events[:cut]
		b.savepoints = b.savepoints[:i]
		return
	}
}

// publishBuffered publishes every event buf holds, draining it in the
// process. It is called by WithTenantSession, and only after its own
// db.Transaction call has returned nil — i.e. only for a transaction that
// genuinely committed. A Publish failure here can never roll anything
// back (the transaction already committed), so it is reported through
// auditPublishFailed instead of returned to the caller: WithTenantSession
// itself must still report success for the business write, which did
// durably commit.
func (p *auditCapturePlugin) publishBuffered(ctx context.Context, buf *auditBuffer) {
	for _, evt := range buf.drain() {
		if err := p.bus.Publish(ctx, evt); err != nil {
			auditPublishFailed(ctx, evt, err)
		}
	}
}

// pendingAuditEventInstanceKey is the db.InstanceSet/InstanceGet key capture
// stashes a built event under, for this plugin's own
// After("gorm:commit_or_rollback_transaction") callback (publishPending) to
// read back within the same statement's callback chain — GORM's own
// InstanceSet namespaces this by the current *gorm.Statement pointer, so
// two unrelated top-level calls never collide even though they share this
// literal key (see gorm.DB.InstanceSet's doc comment).
const pendingAuditEventInstanceKey = "dbkit:audit_capture:pending_event"

// savepointBeginSQLPrefix and savepointRollbackSQLPrefix are the exact
// statement texts dbkit's two supported dialect drivers execute for GORM's
// SavePoint and RollbackTo methods. v1.31.2 delegates both to the
// dialector's SavePointerDialectorInterface, and both drivers implement it
// the same way — github.com/glebarez/sqlite@v1.11.0 and
// gorm.io/driver/postgres@v1.6.2 each run
// tx.Exec("SAVEPOINT " + name) and tx.Exec("ROLLBACK TO SAVEPOINT " + name)
// (the postgres driver's own statement text is identical, so a single
// prefix match covers both dialects) — which routes them through the raw
// processor, where this plugin's afterSavepointSQL observes them. The
// statement text is case-sensitive by construction: the drivers generate
// exactly these prefixes, and a hand-written lowercase "savepoint x"
// statement is not a shape GORM's own SavePoint/RollbackTo produce.
const (
	savepointBeginSQLPrefix    = "SAVEPOINT "
	savepointRollbackSQLPrefix = "ROLLBACK TO SAVEPOINT "
)

// auditPublishFailed reports evt as unpublishable, after the write it
// describes has already durably committed — cause is the bus's own Publish
// error. There is nothing left here to roll back or fail loudly through
// the triggering Create/Update/Delete call (it has already returned, or is
// past the point where its result can change), so this is a structured
// alert rather than an error return, per
// docs/internal/10-compliance-and-audit.md's rule that an audit-write
// failure must alert and never be silently dropped: an operator reading
// this line has every field needed to reconstruct and manually re-publish
// or investigate.
//
// dbkit cannot depend on go/observability (the two sit at the same depth
// in the module dependency graph — pkgcore -> dbkit / observability ->
// ... — so an import would run against the bottom-up rule; see
// go/dbkit/AGENTS.md's "One dependency, and why there is only one"), so
// this reaches for log/slog directly rather than the context-aware
// obs.FromContext wrapper every downstream module uses for its own
// logging — the identical reasoning go/pkgcore/registry.go's
// warnIfNotDurable documents for the same constraint one tier down. The
// message is a constant string and every variable goes into key-value
// attributes, snake_case and shared with the rest of the codebase's
// logging convention (table, resource_type, operation, tenant_id, error),
// mirroring go/jobs/worker.go's own dispatch-failure logging.
func auditPublishFailed(ctx context.Context, evt pkgcore.Event, cause error) {
	payload, _ := evt.Payload.(WriteCapturedEvent)
	slog.Default().ErrorContext(ctx, "dbkit: audit event publish failed after commit",
		"table", payload.Table,
		"resource_type", payload.ResourceType,
		"resource_id", payload.ResourceID,
		"operation", payload.Operation,
		"tenant_id", payload.TenantID,
		"error", cause,
	)
}

// auditTenantMismatch reports, as a structured warning, that a captured
// write's model argument declared a tenant (modelTenantID, read off its
// GetTenantID()) different from evt.TenantID -- the write's ctx tenant,
// already stamped onto evt by the time this is called, and the value
// stampTenantID's own doc comment explains is the trustworthy one of the
// two. This can never fail or roll back the write -- by the time capture
// runs the write has already succeeded -- so, mirroring
// auditPublishFailed's alert-not-fail idiom and for the identical reason
// (dbkit cannot depend on go/observability; see that function's doc
// comment), it is reported through log/slog directly rather than returned
// to any caller. Seeing this warning fire is not expected on the sanctioned
// dbkit.Repository[T] path (see stampTenantID's own doc comment for why),
// so it points at code holding a bare *gorm.DB directly with a decoupled or
// stale Model argument -- worth a human's attention.
func auditTenantMismatch(ctx context.Context, evt WriteCapturedEvent, modelTenantID string) {
	slog.Default().WarnContext(ctx, "dbkit: captured write's model argument tenant differs from context tenant",
		"table", evt.Table,
		"resource_type", evt.ResourceType,
		"resource_id", evt.ResourceID,
		"operation", evt.Operation,
		"tenant_id", evt.TenantID,
		"model_tenant_id", modelTenantID,
	)
}

// Name returns the plugin's identifier, satisfying gorm.Plugin.
func (p *auditCapturePlugin) Name() string { return auditCapturePluginName }

// Initialize registers the write-capture callbacks on db, satisfying
// gorm.Plugin. Each Process (Create, Update, Delete) gets two registrations,
// in this order: one After the matching "gorm:*" callback (capture, so
// db.RowsAffected and the model's own field values are available), then one
// After "gorm:commit_or_rollback_transaction" (publishPending) — always
// present in the compiled chain, since dbkit's Open never sets
// gorm.Config.SkipDefaultTransaction. Registration order is what actually
// guarantees capture runs before publishPending for the same write, not the
// specific "gorm:*" names each names as its own anchor: GORM's callback sort
// (sortCallbacks, callbacks.go) appends an After-only registration to the
// end of whatever has already been sorted at the time it is processed, and
// every one of GORM's own default callbacks for a Process (begin_transaction
// through commit_or_rollback_transaction) carries no ordering constraint of
// its own, so all of them are already sorted before this plugin's two
// registrations — themselves added later, via db.Use, after gorm.Open's own
// RegisterDefaultCallbacks — are processed at all. Confirmed by reading the
// algorithm and empirically with a temporary debug print while designing
// this fix (see audit_capture_test.go's
// TestAuditCapturePlugin_BareWrite_RollbackAfterCapture_PublishesNothing,
// whose own doc comment has the detail this comment summarizes).
func (p *auditCapturePlugin) Initialize(db *gorm.DB) error {
	if err := db.Callback().Create().After("gorm:create").
		Register(auditCapturePluginName+":create", p.afterCreate); err != nil {
		return err
	}
	if err := db.Callback().Create().After("gorm:commit_or_rollback_transaction").
		Register(auditCapturePluginName+":create_commit", p.publishPending); err != nil {
		return err
	}
	if err := db.Callback().Update().After("gorm:update").
		Register(auditCapturePluginName+":update", p.afterUpdate); err != nil {
		return err
	}
	if err := db.Callback().Update().After("gorm:commit_or_rollback_transaction").
		Register(auditCapturePluginName+":update_commit", p.publishPending); err != nil {
		return err
	}
	if err := db.Callback().Delete().After("gorm:delete").
		Register(auditCapturePluginName+":delete", p.afterDelete); err != nil {
		return err
	}
	if err := db.Callback().Delete().After("gorm:commit_or_rollback_transaction").
		Register(auditCapturePluginName+":delete_commit", p.publishPending); err != nil {
		return err
	}
	// GORM's own SavePoint/RollbackTo methods never run a write processor's
	// callback chain — v1.31.2 executes them straight through the dialector
	// (SavePointerDialectorInterface), and both dbkit-supported drivers
	// implement that interface by Exec-ing the SAVEPOINT statement, which
	// does route through the raw processor. Registering After("gorm:raw")
	// is therefore the one callback-chain hook at which a savepoint opening
	// or a rollback-to is observable at all (see afterSavepointSQL for why
	// the buffer needs to know). The observer only acts when the statement's
	// context carries an audit buffer, and only for the two statement texts
	// the drivers emit, so this registration is inert for every other raw
	// statement and for every db without the plugin.
	if err := db.Callback().Raw().After("gorm:raw").
		Register(auditCapturePluginName+":savepoint", p.afterSavepointSQL); err != nil {
		return err
	}
	return nil
}

// compile-time check that auditCapturePlugin satisfies gorm.Plugin.
var _ gorm.Plugin = (*auditCapturePlugin)(nil)

func (p *auditCapturePlugin) afterCreate(db *gorm.DB) { p.capture(db, "create") }
func (p *auditCapturePlugin) afterUpdate(db *gorm.DB) { p.capture(db, "update") }
func (p *auditCapturePlugin) afterDelete(db *gorm.DB) { p.capture(db, "delete") }

// publishPending is registered After("gorm:commit_or_rollback_transaction")
// on every Process this plugin instruments. It reads back the event (if
// any) capture stashed via db.InstanceSet for this exact statement — never
// present at all for a WithTenantSession-buffered write, which
// auditBufferFromContext already routed to the buffer in capture instead —
// and, only when db.Error is still nil, publishes it: db.Error nil here
// means every callback this Process's own chain ran, including the real
// commit or rollback GORM's own commit-or-rollback step performs for a bare
// top-level write, left no error behind. When db.Error is non-nil, this
// Process's own write did not durably succeed (whether that surfaced before
// capture ran at all — in which case capture's own guard already skipped
// building an event, and InstanceGet above returns ok=false — or, in
// principle, between capture and this callback), so nothing is published:
// publishing here would be exactly the phantom-audit-row bug this mechanism
// exists to close.
func (p *auditCapturePlugin) publishPending(db *gorm.DB) {
	val, ok := db.InstanceGet(pendingAuditEventInstanceKey)
	if !ok {
		return
	}
	evt, ok := val.(pkgcore.Event)
	if !ok {
		return
	}
	if db.Error != nil {
		return
	}
	if err := p.bus.Publish(db.Statement.Context, evt); err != nil {
		auditPublishFailed(db.Statement.Context, evt, err)
	}
}

// afterSavepointSQL is registered After("gorm:raw") on every Process chain
// (see Initialize), where it watches for the SAVEPOINT and ROLLBACK TO
// SAVEPOINT statements both dialect drivers execute for GORM's
// SavePoint/RollbackTo methods, and keeps the audit buffer's savepoint
// bookkeeping in step with the database (see auditBuffer's doc comment for
// why rolled-back savepoint regions must prune their events). It is a pure
// observer: it never touches db.Error and never fails a statement, and
// without an audit buffer on the statement's context — every raw statement
// outside a WithTenantSession transaction — it does nothing at all.
//
// The statement's savepoint name is everything after the prefix, trimmed of
// surrounding whitespace. Both drivers concatenate the name verbatim into
// the SQL (GORM's nested Transaction generates sp<64-bit hash> names; a
// manual tx.SavePoint passes the caller's own name through unchanged), and
// the trim only ever removes incidental whitespace, never part of a name —
// an identifier containing whitespace would not survive into SQL in the
// first place.
func (p *auditCapturePlugin) afterSavepointSQL(db *gorm.DB) {
	buf, ok := auditBufferFromContext(db.Statement.Context)
	if !ok {
		return
	}
	sql := db.Statement.SQL.String()
	switch {
	case strings.HasPrefix(sql, savepointBeginSQLPrefix):
		buf.beginSavepoint(strings.TrimSpace(strings.TrimPrefix(sql, savepointBeginSQLPrefix)))
	case strings.HasPrefix(sql, savepointRollbackSQLPrefix):
		buf.rollbackToSavepoint(strings.TrimSpace(strings.TrimPrefix(sql, savepointRollbackSQLPrefix)))
	}
}

// capture builds a WriteCapturedEvent for db.Statement, unless the
// statement's model is not Auditable, the write already failed, or the
// write matched no row at all — the RowsAffected guard below. It never
// publishes directly; see the type's own doc comment for why, and for
// exactly where the built event goes instead (a per-transaction
// *auditBuffer when db.Statement.Context carries one, or this statement's
// own GORM instance map otherwise).
func (p *auditCapturePlugin) capture(db *gorm.DB, operation string) {
	if db.Error != nil {
		return
	}
	if db.RowsAffected == 0 {
		// A zero-row write is a write that matched nothing — a not-found
		// Update or Delete, a double soft-delete, an RLS- or
		// scope-filtered row — so no row state changed and there is
		// nothing to capture: publishing would fabricate an affirmative
		// After ("after the write, the row is …") for a row the write
		// never touched — a double soft-delete would publish a second,
		// false deletion record carrying a fresh deleted_at the real row
		// does not have. The Repository layer reports the same situation
		// to its caller as ErrRecordNotFound, so no signal is lost. Both
		// supported engines count rows *matched* here — PostgreSQL's
		// UPDATE/DELETE command tag and SQLite's changes() since 3.35 —
		// so a full-record save whose values happen not to change still
		// reports a matched row and still captures normally.
		return
	}
	auditable, ok := auditableOf(db.Statement)
	if !ok {
		return
	}

	evt := WriteCapturedEvent{
		Table:        db.Statement.Table,
		ResourceType: auditable.AuditResourceType(),
		Operation:    operation,
		OccurredAt:   time.Now(),
	}
	fields := fieldValuesMap(db.Statement)
	if operation != "delete" {
		evt.After = fields
		if operation == "update" {
			evt.After = scopeAfterToWrittenColumns(db.Statement, fields)
		}
	}
	if id, ok := fields[idColumn]; ok {
		evt.ResourceID, _ = id.(string)
	}
	if evt.ResourceID == "" {
		evt.ResourceID = resourceIDFromWhere(db.Statement)
	}
	if actor, ok := pkgcore.ActorFromContext(db.Statement.Context); ok {
		evt.Actor = actor
	}
	if onBehalfOf, ok := pkgcore.OnBehalfOfFromContext(db.Statement.Context); ok {
		copyOf := onBehalfOf
		evt.OnBehalfOf = &copyOf
	}
	stampTenantID(db.Statement, &evt)

	pkgEvt := pkgcore.Event{
		Type:     EventWriteCaptured,
		TenantID: pkgcore.TenantID(evt.TenantID),
		Payload:  evt,
	}

	if buf, ok := auditBufferFromContext(db.Statement.Context); ok {
		// This write is running inside a WithTenantSession transaction:
		// buffer the event rather than publish it. WithTenantSession
		// itself drains and publishes the buffer, but only once its own
		// db.Transaction call has returned nil — i.e. only once this
		// transaction has genuinely committed. A later statement in the
		// same transaction failing, or the commit itself failing, must
		// never publish this event; leaving it in the buffer (rather than
		// publishing here) is what guarantees that.
		buf.add(pkgEvt)
		return
	}

	// No buffer: this write is a bare Create/Update/Delete against Open's
	// plain *gorm.DB, relying on GORM's own implicit per-statement
	// transaction rather than WithTenantSession. Stash the event instead
	// of publishing it directly here: this callback is only guaranteed to
	// run once db.Error already reflects the write's full outcome (see
	// Initialize's own doc comment on exactly where GORM's callback sort
	// places an After-only registration like this one), never in front of
	// publishPending's own check of it, so publishPending — never this
	// call site — is the single place that decides whether to publish.
	db.InstanceSet(pendingAuditEventInstanceKey, pkgEvt)
}

// stampTenantID sets evt.TenantID for a captured write.
//
// The short version: for a model implementing TenantScoped, evt.TenantID is
// the write's ctx tenant -- never the model argument's own GetTenantID()
// value -- with a disagreement between the two logged as a warning
// (auditTenantMismatch) rather than acted on. For a model that does not
// implement TenantScoped at all, evt.TenantID is left empty, full stop, no
// ctx fallback.
//
// The longer version, and why ctx -- not the model's own field -- is the
// trustworthy signal here, contrary to the naive "the row is the fact being
// audited, prefer its own declared value" framing this fix started from:
// this plugin (auditCapturePlugin) is only ever installed by Open, on the
// exact same *gorm.DB that Open also unconditionally installs
// tenantScopePlugin on (tenant_scope.go) -- there is no code path in this
// codebase that attaches one without the other. tenantScopePlugin fails
// every create/update/delete of a TenantScoped model closed
// (ErrMissingTenantContext) when ctx carries no tenant, forces the tenant_id
// column to ctx's tenant on every Create (tenantScopeBeforeCreate's
// SetColumn, overwriting whatever the caller populated), and appends
// "WHERE tenant_id = <ctx's tenant>" to every Update/Delete
// (tenantScopeBeforeUpdate/Delete) -- rejecting outright
// (ErrTenantIDImmutable) any Update payload that tries to write a different
// tenant_id in its own SET clause. So whenever capture() is reached with a
// TenantScoped model and db.Error == nil and db.RowsAffected > 0 (its own
// existing guards, above), tenantScopePlugin has already, unconditionally,
// guaranteed that ctx's tenant is the real, enforced tenant of whichever row
// was actually created, matched or affected -- proven empirically while
// designing this fix by constructing every Repository[T] write shape
// (Create, full-record Update, Delete, softDelete) and confirming ctx's
// tenant is what the WHERE clause or the forced column bound in every case.
//
// The model argument's own GetTenantID(), by contrast, is not reliably that
// same value at all: dbkit.Repository[T]'s own sanctioned Delete and
// softDelete build a bare "var zero T" (or an m populated only for the two
// columns the mark-delete UPDATE selects) and never touch the argument
// struct's TenantID field, so GetTenantID() there is simply empty --
// confirmed by TestAuditCapturePlugin_HardDelete_ClassifiesAsDelete, which
// pins TenantID = ctx's tenant even though the model argument's own field
// carries nothing. More importantly, it can be actively wrong: a caller
// holding a bare *gorm.DB directly (outside Repository[T] -- the
// raw-SQL/WithTenantSession-direct escape hatch backend-coding-standards
// SKILL.md §3.2 documents, generalized to any bare-GORM call) can write
// db.Model(&Widget{ID: "w1", TenantID: "tenant-b"}).Updates(map[string]any{"name": "x"})
// under a ctx carrying "tenant-a": the map payload never itself sets
// tenant_id, so tenantScopeBeforeUpdate's immutability guard -- which
// inspects only the payload (stmt.Dest), never the decoupled .Model()
// argument -- never fires, and the WHERE clause it appends still correctly
// scopes the write to ctx's real "tenant-a" row. GetTenantID() read off
// stmt.Model here returns "tenant-b" -- a value with nothing to do with the
// row actually matched, since .Model() here is merely a query selector
// decoupled from Updates()'s own payload, not "the row" in any meaningful
// sense. TestAuditCapturePlugin_ModelArgumentTenantDiffersFromContext_TrustsContextAndWarns
// proves this reachable and pins ctx ("tenant-a") -- not the stale Model
// argument ("tenant-b") -- as what gets captured.
//
// A disagreement between ctx's tenant and a non-empty GetTenantID() is
// therefore treated as option (ii) from this fix's own design brief: a
// signal of a deeper bug (a decoupled or stale Model argument) worth
// surfacing loudly, via auditTenantMismatch, rather than a reason to prefer
// the model's value over ctx's -- investigation here found no real,
// reachable case in this codebase where the model's own field is a *more*
// truthful answer than ctx, and at least one reachable case (above) where
// it is actively less truthful.
//
// ctx carrying no tenant at all should not be reachable for a captured
// TenantScoped write, per the guarantee above -- but as defense in depth,
// rather than silently leaving evt.TenantID empty for a row that may well
// have declared a real one, this falls back to a non-empty GetTenantID()
// in that case.
//
// For a model that does not implement TenantScoped at all -- a platform- or
// identity-domain Auditable model (root CLAUDE.md's four-data-domain
// table) -- evt.TenantID is left empty, with no ctx fallback of any kind:
// tenantScopePlugin gives no guarantee whatsoever for a model it never
// scopes, so a platform write can run under any tenant's ctx for reasons
// entirely unrelated to the row itself (a job or admin operation that
// rebuilt tenant ctx for an unrelated purpose), and stamping that tenant
// onto the event would misattribute it in the audit trail -- exactly
// dbkit-tenancy P2-2. The acting identity is still fully captured, just
// under Actor/OnBehalfOf rather than TenantID, which is sufficient: a
// platform write's "who did this, from where" is Actor/OnBehalfOf's job,
// and TenantID empty is the truthful answer to "which tenant does this row
// belong to" for a row that belongs to none.
func stampTenantID(stmt *gorm.Statement, evt *WriteCapturedEvent) {
	ts, ok := tenantScopedOf(stmt)
	if !ok {
		return
	}

	rowTenant := ts.GetTenantID()

	ctxTenant, ctxHasTenant := pkgcore.TenantFromContext(stmt.Context)
	if !ctxHasTenant {
		// Defense in depth only: should not be reachable for a captured
		// TenantScoped write (see doc comment), since tenantScopePlugin
		// fails such a write closed before it ever reaches capture().
		if rowTenant != "" {
			evt.TenantID = string(rowTenant)
		}
		return
	}

	evt.TenantID = string(ctxTenant)
	if rowTenant != "" && rowTenant != ctxTenant {
		auditTenantMismatch(stmt.Context, *evt, string(rowTenant))
	}
}

// tenantScopedOf reports whether stmt's model implements TenantScoped,
// mirroring auditableOf's exact Model-before-Dest precedence (see that
// function's doc comment for why Model must be checked first: GORM's Count
// finisher briefly seeds Model from Dest, then overwrites Dest with the
// *int64 result pointer before callbacks run).
func tenantScopedOf(stmt *gorm.Statement) (TenantScoped, bool) {
	if ts, ok := stmt.Model.(TenantScoped); ok {
		return ts, true
	}
	if ts, ok := stmt.Dest.(TenantScoped); ok {
		return ts, true
	}
	return nil, false
}

// auditableOf reports whether stmt's model implements Auditable, checking
// Model first and falling back to Dest — mirroring
// isTenantScopedStatement's identical Model-before-Dest precedence and its
// documented reason (GORM's Count finisher reuses Dest to seed Model, then
// overwrites Dest with a *int64 result pointer before callbacks run).
//
// Unlike isTenantScopedValue, this needs an actual value to call
// AuditResourceType() on, not merely a type test, so it does not attempt
// the slice-element unwrap isTenantScopedValue performs: a batch
// Create/Update/Delete over a slice is not captured by this plugin in M1 —
// every Repository[T] write in this codebase operates on one record at a
// time, so this is not a gap in the sanctioned data-access path, only in a
// hand-rolled batch write, which a caller wanting audit capture for should
// use audit.Emit explicitly instead.
func auditableOf(stmt *gorm.Statement) (Auditable, bool) {
	if a, ok := stmt.Model.(Auditable); ok {
		return a, true
	}
	if a, ok := stmt.Dest.(Auditable); ok {
		return a, true
	}
	return nil, false
}

// auditRedactedFieldValue replaces the captured value of any GORM
// serializer field — dbkit's own encrypted-field mechanism
// (RegisterEncryptedSerializer, `gorm:"serializer:<name>"`) included — in a
// WriteCapturedEvent's Before/After maps, mirroring go/config's identical
// "[redacted]" convention for its own Sensitive item change events.
//
// Two independent problems make this necessary, not just desirable. First,
// field.ValueOf on a serializer field does not return the field's plain
// value at all: GORM wraps it in a *schema.serializer that embeds a
// self-referential *schema.Field (function-typed ValueOf/Set/ReflectValueOf
// fields), which json.Marshal cannot encode ("encountered a cycle via
// *schema.Field") — fatal to both onward paths, RedisEventBus.Publish's
// whole-payload marshal in distributed mode and audit.changesJSON's
// Diff marshal in standalone mode. Second, even if it could be unwrapped,
// the raw pre-serialization struct value is the *plaintext* — the very
// thing dbkit's encrypted serializer exists to keep off disk — so writing
// it into the audit trail unencrypted would be a PII leak this repository's
// own "do not write plaintext PII into logs, traces or API responses" rule
// forbids. Redacting is therefore the only capture this plugin may take of
// a serializer field on its own, with no cipher key of its own to decrypt
// or re-derive anything from.
const auditRedactedFieldValue = "[redacted]"

// fieldValuesMap returns every schema field of stmt's ReflectValue, keyed
// by DB column name. It returns nil when stmt carries no schema or its
// ReflectValue is not (or does not unwrap to) a single struct — in
// particular, a slice destination (a batch write) yields nil here, which
// is consistent with auditableOf never matching a slice element in the
// first place.
//
// A field with an empty DBName — a gorm:"-" field, whose whole point is a
// value deliberately kept off the schema (often plaintext the model does
// not want persisted, which must not leak into the audit trail either), or
// an association field, which GORM resolves through the related table's
// rows, never through a column of its own — is skipped outright: no column
// exists whose post-write value After could truthfully claim, and capturing
// its value under the shared "" key would both collide with every other
// such field and put a non-column value on the wire. GORM itself keeps
// these fields in schema.Fields with DBName == "" (schema.Parse derives a
// column name only when DataType is non-empty), so the empty DBName is the
// exact discriminator.
//
// A field carrying a GORM Serializer (see auditRedactedFieldValue's doc
// comment for why) is captured as auditRedactedFieldValue instead of its
// real value. A serializer field always has a real column, so the empty-
// DBName skip above never interacts with the redaction.
func fieldValuesMap(stmt *gorm.Statement) map[string]any {
	if stmt.Schema == nil {
		return nil
	}
	v := stmt.ReflectValue
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}

	out := make(map[string]any, len(stmt.Schema.Fields))
	for _, field := range stmt.Schema.Fields {
		if field.DBName == "" {
			// No database column backs this field (a gorm:"-" field or an
			// association field): nothing truthful to capture under its
			// column name, and no column name exists to key it by — see the
			// function's doc comment.
			continue
		}
		if field.Serializer != nil {
			out[field.DBName] = auditRedactedFieldValue
			continue
		}
		value, _ := field.ValueOf(stmt.Context, v)
		out[field.DBName] = value
	}
	return out
}

// scopeAfterToWrittenColumns trims after — the full-payload capture
// fieldValuesMap produced — down to the columns the executed statement's
// SQL actually assigned, for an Update statement that restricted its
// columns. It exists because a column-restricted update's payload cannot
// be trusted as the written set: a Select-restricted payload holds only
// the restricted columns and zeroes for every other, while an
// Omit-restricted struct payload can carry real values for columns the
// zero-value skip left unwritten — either way, capturing the payload
// whole would fabricate values for columns the write never touched,
// values that contradict the real row (still in the database with its
// pre-write state intact on those columns). After is documented as the
// row's column values *following the write* — it must never claim a value
// for a column the write did not assign.
//
// GORM deletes its SET clause from db.Statement.Clauses before after-*
// callbacks run (callbacks/update.go), so the written set cannot be read
// back from the statement's final state; it is reconstructed from the
// statement's Select/Omit lists instead, mirroring how GORM itself decided
// the assignments (ConvertToAssignments):
//
//   - No Select list and no Omit list — or a Select("*") with no Omit
//     list: an unrestricted, full-record write, where the payload is the
//     whole row. after is returned unchanged.
//
//   - An explicit Select list (Repository[T]'s soft-delete and Restore
//     writes are the canonical shape: Select("DeletedAt", "DeletedBy") on
//     a freshly built struct): the write assigned exactly the resolved
//     select columns plus every auto-update-time column GORM force-adds
//     (field.AutoUpdateTime > 0, hooks not skipped via stmt.SkipHooks),
//     minus the resolved Omit list.
//
//   - An Omit list without a Select list — GORM's "update the whole record
//     except the omitted columns" shape. Which columns the statement
//     actually assigned depends on the payload kind, so the written set
//     mirrors ConvertToAssignments' own per-payload decisions rather than
//     assuming the whole schema minus the omitted columns (which would
//     over-claim: see below).
//
//     map[string]any payload: exactly the map's own keys, minus the
//     omitted ones — a map update assigns its keys with no zero-value
//     skip, so a zero map value really is written and stays captured —
//     plus every auto-update-time column GORM force-adds when hooks run
//     and the map carries that column under neither its Go field name nor
//     its DB name.
//
//     struct payload: Updates with a struct and no Select list is GORM's
//     classic "only non-zero fields" update, so the written set is every
//     schema column that is not omitted and that GORM's own assignment
//     test judged written — updatable (field.Updatable), non-zero on the
//     payload under schema.Field.ValueOf (the same zero test GORM itself
//     applies), and not a primary key in the canonical Model == Dest
//     shape, where the payload's primary-key values select the row
//     instead of entering the SET — plus every auto-update-time column
//     GORM force-writes when hooks run (field.AutoUpdateTime > 0 and
//     !stmt.SkipHooks), zero payload or not.
//
//     This mirror is exact for the canonical Model == Dest shape, which is
//     the shape every Repository[T] write uses. When Model and Dest
//     differ, after describes the Model's fields (SetupUpdateReflectValue
//     resets ReflectValue to the Model), which may not be the payload the
//     assignments came from — the known limitation the Select shape above
//     already carries (see go/dbkit/AGENTS.md's Model == Dest discussion);
//     the written set is computed against those same Model fields, and
//     scoping it is no less truthful than before this branch existed.
//
// Select/Omit entries are resolved the way GORM itself resolves them
// (LookUpField, statement.go's SelectAndOmitColumns): a Go field name like
// "DeletedAt" maps to its DB column name, an unresolvable name is kept
// verbatim (harmless here — after is keyed by DB column name, so a verbatim
// entry that names no column selects nothing). Values for the columns that
// survive the scope are truthful, which is what makes this a fix rather
// than a loss: GORM writes each assignment's final value back into the
// statement's ReflectValue before capture runs (callbacks/update.go —
// SetupUpdateReflectValue keeps ReflectValue on the payload in the
// canonical Model == Dest shape, and ConvertToAssignments' assignValue
// writes every assignment into it via field.Set), so fieldValuesMap
// already read the real written values — the scope only drops what the
// statement never wrote.
func scopeAfterToWrittenColumns(stmt *gorm.Statement, after map[string]any) map[string]any {
	if stmt.Schema == nil || len(after) == 0 {
		return after
	}

	resolve := func(name string) string {
		if field := stmt.Schema.LookUpField(name); field != nil && field.DBName != "" {
			return field.DBName
		}
		return name
	}

	written := make(map[string]bool, len(stmt.Schema.Fields))
	all := false
	hasSelect := false
	for _, name := range stmt.Selects {
		hasSelect = true
		switch resolved := resolve(name); resolved {
		case "*":
			all = true
		default:
			written[resolved] = true
		}
	}

	omitted := make(map[string]bool, len(stmt.Omits))
	for _, name := range stmt.Omits {
		omitted[resolve(name)] = true
	}

	switch {
	case !hasSelect && len(omitted) == 0:
		// No Select list and no Omit list: an unrestricted, full-record
		// write, where the payload is the whole row.
		return after

	case all:
		// Select("*") is an unrestricted write: every column was
		// assigned.
		if len(omitted) == 0 {
			return after
		}
		for _, field := range stmt.Schema.Fields {
			written[field.DBName] = true
		}

	case hasSelect:
		// An explicit Select list: GORM force-adds auto-update-time
		// columns to a restricted struct-update's assignments whenever
		// hooks run — mirror that here, or an UpdatedAt a model has but
		// did not list would be dropped from After despite the SQL really
		// having written it.
		if !stmt.SkipHooks {
			for _, field := range stmt.Schema.Fields {
				if field.AutoUpdateTime > 0 && !field.PrimaryKey && field.Updatable {
					written[field.DBName] = true
				}
			}
		}

	default:
		// An Omit list without a Select list — see the doc comment above
		// for the per-payload-kind rules this mirrors.
		dest := reflect.ValueOf(stmt.Dest)
		for dest.IsValid() && dest.Kind() == reflect.Pointer && !dest.IsNil() {
			dest = dest.Elem()
		}
		switch {
		case dest.IsValid() && dest.Kind() == reflect.Map:
			if m, ok := dest.Interface().(map[string]any); ok {
				for key := range m {
					written[resolve(key)] = true
				}
				if !stmt.SkipHooks {
					for _, field := range stmt.Schema.Fields {
						if field.AutoUpdateTime > 0 && !field.PrimaryKey && field.Updatable {
							if _, hasGoName := m[field.Name]; !hasGoName {
								if _, hasDBName := m[field.DBName]; !hasDBName {
									written[field.DBName] = true
								}
							}
						}
					}
				}
			}
		case dest.IsValid() && dest.Kind() == reflect.Struct:
			payload := stmt.ReflectValue
			for payload.IsValid() && payload.Kind() == reflect.Pointer {
				payload = payload.Elem()
			}
			if !payload.IsValid() || payload.Kind() != reflect.Struct {
				// after was non-empty, so fieldValuesMap must already
				// have unwrapped ReflectValue to a struct; nothing to
				// scope against otherwise.
				break
			}
			primaryKeysSelectTheRow := dest.CanAddr() && stmt.Dest == stmt.Model
			for _, field := range stmt.Schema.Fields {
				if field.DBName == "" || omitted[field.DBName] || !field.Updatable {
					continue
				}
				if field.PrimaryKey && primaryKeysSelectTheRow {
					continue
				}
				if !stmt.SkipHooks && field.AutoUpdateTime > 0 {
					written[field.DBName] = true
					continue
				}
				if _, zero := field.ValueOf(stmt.Context, payload); !zero {
					written[field.DBName] = true
				}
			}
		}
	}

	out := make(map[string]any, len(written))
	for dbName, value := range after {
		if written[dbName] && !omitted[dbName] {
			out[dbName] = value
		}
	}
	if len(out) == len(after) {
		return after
	}
	return out
}

// resourceIDFromWhere best-effort extracts an id value from stmt's WHERE
// clause, for the shape dbkit.Repository[T]'s own Update, Delete and
// FindByID methods always build:
// Where(idColumn+" = ?", id).Where(tenantIDColumn+" = ?", tenant) — which
// GORM compiles to two clause.Expr entries with literal SQL text, since
// they were built from a raw SQL string plus args rather than a
// column-comparison helper. It recognizes exactly that literal
// "id = ?" text (case- and whitespace-insensitive around it) and returns
// the first bound argument as a string; any other WHERE shape — a
// hand-written condition using clause.Eq, a composite key, a multi-column
// filter — is simply not recognized, and this returns "".
//
// This is a deliberately narrow, best-effort convenience for the plugin's
// ResourceID field, not a general WHERE-clause parser: a caller building
// custom conditions and wanting a reliable ResourceID should not rely on
// this, and can use audit.Emit's explicit Resource instead.
func resourceIDFromWhere(stmt *gorm.Statement) string {
	c, ok := stmt.Clauses["WHERE"]
	if !ok {
		return ""
	}
	where, ok := c.Expression.(clause.Where)
	if !ok {
		return ""
	}
	return firstIDFromExprs(where.Exprs)
}

// firstIDFromExprs walks exprs (recursing into any nested clause.Where or
// clause.AndConditions, since dbkit's own tenant-scoping plugin wraps a
// caller's existing WHERE conditions in exactly that shape — see
// tenant_scope.go's groupExistingWhereConditions) looking for the first
// literal "id = ?" clause.Expr, returning its bound argument as a string.
func firstIDFromExprs(exprs []clause.Expression) string {
	for _, e := range exprs {
		switch cond := e.(type) {
		case clause.Expr:
			sql := strings.ToLower(strings.TrimSpace(cond.SQL))
			if sql == idColumn+" = ?" && len(cond.Vars) == 1 {
				if s, ok := cond.Vars[0].(string); ok {
					return s
				}
			}
		case clause.Where:
			if id := firstIDFromExprs(cond.Exprs); id != "" {
				return id
			}
		case clause.AndConditions:
			if id := firstIDFromExprs(cond.Exprs); id != "" {
				return id
			}
		}
	}
	return ""
}
