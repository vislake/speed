package dbkit

import (
	"reflect"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// softDeleteScopeColumn is the database column the soft-delete auto-scope
// plugin filters query callbacks on. It duplicates repository.go's
// deletedAtColumn constant in value, deliberately: tenant_scope.go's
// tenantScopeColumn and repository.go's tenantIDColumn are already two
// separate constants sharing one value, and this file follows that same,
// already-established per-file convention rather than "fixing" it by
// unifying the two.
const softDeleteScopeColumn = "deleted_at"

// softDeleteScopePluginName identifies softDeleteScopePlugin to GORM
// (gorm.Plugin.Name and db.Plugins) and prefixes the callback it registers,
// mirroring tenantScopePluginName's and auditCapturePluginName's identical
// convention so callback names can never collide with GORM's own "gorm:*"
// callbacks or another plugin's.
const softDeleteScopePluginName = "dbkit:soft_delete_scope"

// ErrNotSoftDeletable is returned by Repository[T].Restore when T does not
// implement SoftDeletable — Restore has nothing to clear for a model whose
// Delete is, and always was, a physical DELETE.
var ErrNotSoftDeletable = apperr.Invalid("dbkit.not_soft_deletable")

// SoftDeletable marks a GORM model as opting into dbkit's mark-delete
// (soft-delete) capability: Repository[T].Delete against such a T issues an
// UPDATE setting deleted_at/deleted_by instead of a physical DELETE, and the
// GORM plugin installed here auto-appends "deleted_at IS NULL" to that
// model's query callbacks. A model that does not implement SoftDeletable is
// completely unaffected — Delete keeps today's physical-delete behavior,
// and this plugin never so much as looks at it — so soft-delete is a
// per-model, explicitly declared capability, never an implicit new default
// (docs/internal/04-data-and-tenancy.md's delete-semantics section, §1).
//
// Like TenantScoped, GetDeletedAt is a single-getter marker never actually
// called by the plugin or by Repository[T] itself: the interface only says
// "this model opts into soft deletion". The field *writes* — DeletedAt and
// DeletedBy — go through reflection on fixed Go field names ("DeletedAt",
// "DeletedBy"), exactly mirroring setTenantID's/idOf's existing convention
// in repository.go, not a new pattern introduced here.
//
// A model implementing SoftDeletable is required to carry a DeletedAt
// *time.Time and a DeletedBy string field by those exact names — Go
// generics cannot express "an exported field named X" as a constraint, so
// this, like Repository[T]'s own ID/TenantID convention, is documented
// rather than compiler-checked; a T missing either field fails the first
// Delete/Restore call with a descriptive error instead of failing to
// compile.
//
// This package's soft-delete support is scoped to mark-delete only; the
// physical-erasure half of the delete semantics is the deliberately
// separate Repository[T].HardDelete (hard_delete.go) — the irreversible,
// system-context-gated compliance-erasure path
// docs/internal/04-data-and-tenancy.md's §3 describes, landed in the
// round after this capability. A soft-deleted row is NOT a security
// boundary and is NOT compliance-grade deletion: it remains a real,
// plaintext-present row (encrypted fields excepted) until a HardDelete
// actually removes it. See AGENTS.md's "Soft deletion" and "Hard
// deletion" sections for the full picture.
type SoftDeletable interface {
	GetDeletedAt() *time.Time
}

// newSoftDeleteScopePlugin returns a ready-to-install softDeleteScopePlugin.
func newSoftDeleteScopePlugin() *softDeleteScopePlugin {
	return &softDeleteScopePlugin{}
}

// softDeleteScopePlugin is a gorm.Plugin that auto-appends
// "deleted_at IS NULL" ahead of every query callback (Find/First/Take/Count
// and similar reads) against a model implementing SoftDeletable, hiding
// soft-deleted rows from ordinary reads.
//
// It is deliberately narrower than tenantScopePlugin: it registers on the
// two read processors — Before("gorm:query") and Before("gorm:row") — never
// on create/update/delete. The design doc's literal text says the
// auto-scope belongs on the query callback only, not on
// create/update/delete; the row-processor registration extends that same
// auto-scope to the reads GORM routes through its Row processor
// (Scan/Row/Rows — finisher_api.go's Scan delegates to Rows, which executes
// callbacks.Row()), the identical finisher-to-processor reasoning
// tenantScopePlugin's row registration and auditCapturePlugin's
// raw-processor registration record: a GORM operation is protected at the
// processor its finisher executes, never at the finisher name, and without
// the row registration every scan-shaped read of a SoftDeletable model
// surfaced soft-deleted rows with a nil error.
// Repository[T].Delete, Update and Restore build their own explicit
// "deleted_at IS NULL" / "deleted_at IS NOT NULL" WHERE clauses instead of
// relying on this plugin (see repository.go) — Update's guard being the
// fix for the stale-model hazard that used to leave a pre-delete in-memory
// copy free to silently clear the mark (AGENTS.md's Soft deletion section
// records the closed state).
//
// A caller wanting to see soft-deleted rows in a query sets
// db.Unscoped() — GORM's own general query-scope bypass, reused here
// rather than inventing a second one — which this plugin checks and skips
// entirely, giving any future "admin sees soft-deleted rows" read path a
// route in without touching this plugin again.
//
// It is unexported and installed unconditionally by Open, mirroring
// tenantScopePlugin's installation: soft-delete is a per-model opt-in
// marker interface, not a global switch, so installing the plugin
// unconditionally costs nothing for a model that doesn't implement
// SoftDeletable — no callback so much as looks at it.
type softDeleteScopePlugin struct{}

// Name returns the plugin's identifier, satisfying gorm.Plugin.
func (p *softDeleteScopePlugin) Name() string { return softDeleteScopePluginName }

// Initialize registers the soft-delete scope callback on both of db's read
// processors, satisfying gorm.Plugin. The query-processor registration sits
// Before("gorm:query"), matching tenantScopePlugin's own registration point,
// so both scopes' WHERE conditions are in place before GORM's own
// SQL-building step runs; the row-processor registration sits
// Before("gorm:row") for the same reason. The row registration exists
// because GORM routes Scan/Row/Rows through its Row processor, a separate
// callback chain from the query processor, so a query-only registration
// left every scan-shaped read of a SoftDeletable model surfacing
// soft-deleted rows with a nil error. Row/Rows/Scan are the only finishers
// the row processor runs, so this one registration closes all three (the
// identical finisher-to-processor reasoning tenantScopePlugin's Initialize
// records).
func (p *softDeleteScopePlugin) Initialize(db *gorm.DB) error {
	if err := db.Callback().Query().Before("gorm:query").
		Register(softDeleteScopePluginName+":query", softDeleteScopeBeforeQuery); err != nil {
		return err
	}
	return db.Callback().Row().Before("gorm:row").
		Register(softDeleteScopePluginName+":row", softDeleteScopeBeforeQuery)
}

// compile-time check that softDeleteScopePlugin satisfies gorm.Plugin.
var _ gorm.Plugin = (*softDeleteScopePlugin)(nil)

// softDeleteScopeBeforeQuery appends "deleted_at IS NULL" ahead of every
// read of a SoftDeletable model — Find and its kin on the query processor,
// Scan/Row/Rows on the row processor (see Initialize) — unless the
// statement is Unscoped (db.Statement.Unscoped), GORM's own general bypass
// mechanism.
//
// Like tenantScopeBeforeQuery/Update/Delete, it groups whatever WHERE
// expressions the caller already attached (groupExistingWhereConditions)
// before appending its predicate, so the soft-delete filter always binds to
// the caller's entire pre-existing condition as one AND'd sibling, never
// only to the last top-level branch of it. Left alone, gorm's
// clause.Where.MergeClause (see gorm.io/gorm/clause/where.go) merges the
// predicate onto the end of the caller's top-level conditions, and SQL gives
// AND strictly higher precedence than OR — a caller query built with the
// chained .Or(...) method, e.g. db.Where("name = ?", "x").Or("name = ?", "y"),
// would compile to "name = ? OR (name = ? AND deleted_at IS NULL)", leaving
// its first OR branch to match soft-deleted rows exactly like live ones and
// breaching the filter's whole purpose. See groupExistingWhereConditions'
// own doc comment in tenant_scope.go for the full analysis.
func softDeleteScopeBeforeQuery(db *gorm.DB) {
	if db.Statement.Unscoped {
		return
	}
	if !isSoftDeletableStatement(db.Statement) {
		return
	}
	groupExistingWhereConditions(db.Statement)
	db.Statement.Where(softDeleteScopeColumn + " IS NULL")
}

// softDeletableType is the reflect.Type of the SoftDeletable interface. It
// lets isSoftDeletableValue test a slice's element type reflectively,
// mirroring tenantScopedType's identical role for TenantScoped.
var softDeletableType = reflect.TypeOf((*SoftDeletable)(nil)).Elem()

// isSoftDeletableStatement reports whether stmt's model opts into
// soft-delete scoping. It checks db.Statement.Model first, falling back to
// db.Statement.Dest only when Model is nil — the identical Model-before-Dest
// precedence isTenantScopedStatement uses, and for the identical reason
// (GORM's Count finisher briefly seeds Model from Dest, then overwrites
// Dest with a *int64 result pointer before query callbacks run).
func isSoftDeletableStatement(stmt *gorm.Statement) bool {
	if isSoftDeletableValue(stmt.Model) {
		return true
	}
	return isSoftDeletableValue(stmt.Dest)
}

// isSoftDeletableValue reports whether v refers to a type implementing
// SoftDeletable, mirroring isTenantScopedValue's identical pointer/slice
// unwrapping so this function recognizes the same shapes GORM callbacks
// hand it: a pointer to a struct, a pointer to a slice of structs, or a
// slice of pointers to structs.
//
// It works purely on types — never reflect.ValueOf, never indexes into v,
// never calls GetDeletedAt — so a nil slice or a typed nil pointer is
// inspected safely, exactly like isTenantScopedValue.
func isSoftDeletableValue(v interface{}) bool {
	if v == nil {
		return false
	}
	if _, ok := v.(SoftDeletable); ok {
		return true
	}

	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
		elem := t.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		t = elem
	}

	return t.Implements(softDeletableType) || reflect.PointerTo(t).Implements(softDeletableType)
}
