package dbkit

import (
	"reflect"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
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
	// TenantID is the write's tenant, or empty for a platform-level write
	// (a context with no tenant at all).
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
	// column the write did not touch: a column-restricted update's payload
	// holds only the restricted columns, so capturing the payload whole
	// would fabricate zero values for untouched columns whose real row
	// values are still in place (see scopeAfterToWrittenColumns). For an
	// unrestricted full-record Update, After is the whole row.
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
// Registered callbacks run AFTER the corresponding "gorm:*" callback, so
// only a write that actually succeeded is captured — db.Error is checked
// again defensively even so, since a later callback in the same chain
// could still have failed the statement. Per
// docs/internal/10-compliance-and-audit.md's rule that an audit-write
// failure must alert and never be silently dropped, a publish failure is
// reported back into the GORM callback chain with db.AddError, which
// surfaces it as the error of the very Create/Update/Delete call that
// triggered it — including, when the call runs inside
// dbkit.WithTenantSession's transaction (every Repository[T] write does),
// rolling that transaction back. A write whose audit trail could not be
// recorded is treated as a write that did not happen.
type auditCapturePlugin struct {
	bus pkgcore.EventBus
}

// Name returns the plugin's identifier, satisfying gorm.Plugin.
func (p *auditCapturePlugin) Name() string { return auditCapturePluginName }

// Initialize registers the write-capture callbacks on db, satisfying
// gorm.Plugin. Each is registered After the matching "gorm:*" callback, so
// it runs once the write itself has actually been executed.
func (p *auditCapturePlugin) Initialize(db *gorm.DB) error {
	if err := db.Callback().Create().After("gorm:create").
		Register(auditCapturePluginName+":create", p.afterCreate); err != nil {
		return err
	}
	if err := db.Callback().Update().After("gorm:update").
		Register(auditCapturePluginName+":update", p.afterUpdate); err != nil {
		return err
	}
	if err := db.Callback().Delete().After("gorm:delete").
		Register(auditCapturePluginName+":delete", p.afterDelete); err != nil {
		return err
	}
	return nil
}

// compile-time check that auditCapturePlugin satisfies gorm.Plugin.
var _ gorm.Plugin = (*auditCapturePlugin)(nil)

func (p *auditCapturePlugin) afterCreate(db *gorm.DB) { p.capture(db, "create") }
func (p *auditCapturePlugin) afterUpdate(db *gorm.DB) { p.capture(db, "update") }
func (p *auditCapturePlugin) afterDelete(db *gorm.DB) { p.capture(db, "delete") }

// capture builds and publishes a WriteCapturedEvent for db.Statement,
// unless the statement's model is not Auditable or the write already
// failed. See the type's own doc comment for the failure-handling
// contract.
func (p *auditCapturePlugin) capture(db *gorm.DB, operation string) {
	if db.Error != nil {
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
	if tenant, ok := pkgcore.TenantFromContext(db.Statement.Context); ok {
		evt.TenantID = string(tenant)
	}

	err := p.bus.Publish(db.Statement.Context, pkgcore.Event{
		Type:     EventWriteCaptured,
		TenantID: pkgcore.TenantID(evt.TenantID),
		Payload:  evt,
	})
	if err != nil {
		_ = db.AddError(apperr.Internal("dbkit.audit_capture_publish_failed").
			WithParam("resource_type", evt.ResourceType).
			WithParam("operation", operation).
			WithCause(err))
	}
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
// A field carrying a GORM Serializer (see auditRedactedFieldValue's doc
// comment for why) is captured as auditRedactedFieldValue instead of its
// real value.
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
// columns. It exists because a column-restricted update's payload holds
// only the restricted columns: every other field is zero on the payload,
// so capturing the payload whole would fabricate zero values for columns
// the write never touched, values that contradict the real row (still in
// the database with its pre-write state intact on those columns). After is
// documented as the row's column values *following the write* — it must
// never claim a value for a column the write did not assign.
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
//   - An explicit Select list (Repository[T]'s soft-delete and Restore
//     writes are the canonical shape: Select("DeletedAt", "DeletedBy") on
//     a freshly built struct): the write assigned exactly the resolved
//     select columns plus every auto-update-time column GORM force-adds
//     (field.AutoUpdateTime > 0, hooks not skipped via stmt.SkipHooks),
//     minus the resolved Omit list.
//   - An Omit list without a Select list: every column but the omitted
//     ones.
//
// Select/Omit entries are resolved the way GORM itself resolves them
// (LookUpField, statement.go's SelectAndOmitColumns): a Go field name like
// "DeletedAt" maps to its DB column name, an unresolvable name is kept
// verbatim (harmless here — after is keyed by DB column name, so a verbatim
// entry that names no column selects nothing). Values for the columns that
// survive the scope are truthful, which is what makes this a fix rather
// than a loss: GORM writes each assignment's final value back into the
// statement's ReflectValue before capture runs (callbacks/update.go's
// SetupUpdateReflectValue), so fieldValuesMap already read the real written
// values — the scope only drops the fabricated zeroes for untouched
// columns.
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
	for _, name := range stmt.Selects {
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

	if len(written) == 0 && len(omitted) == 0 {
		return after
	}
	if all {
		// Select("*") is an unrestricted write: every column was assigned.
		if len(omitted) == 0 {
			return after
		}
		for _, field := range stmt.Schema.Fields {
			written[field.DBName] = true
		}
	} else if !stmt.SkipHooks {
		// GORM force-adds auto-update-time columns to a restricted
		// struct-update's assignments whenever hooks run — mirror that
		// here, or an UpdatedAt a model has but did not list would be
		// dropped from After despite the SQL really having written it.
		for _, field := range stmt.Schema.Fields {
			if field.AutoUpdateTime > 0 && !field.PrimaryKey && field.Updatable {
				written[field.DBName] = true
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
