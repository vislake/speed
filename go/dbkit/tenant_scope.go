package dbkit

import (
	"reflect"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// tenantScopeColumn is the database column every tenant-scoped model is
// filtered and populated on. It must stay in sync with TenantModel's gorm
// tag below, which spells the column name out literally because a struct
// tag cannot reference a Go constant.
const tenantScopeColumn = "tenant_id"

// tenantScopePluginName identifies tenantScopePlugin to GORM (gorm.Plugin.Name
// and db.Plugins) and prefixes the callback names it registers, so they can
// never collide with GORM's own "gorm:*" callbacks or another plugin's.
const tenantScopePluginName = "dbkit:tenant_scope"

// ErrMissingTenantContext is the *apperr.Error returned (with the underlying
// pkgcore error attached as its cause) when a tenant-scoped query, create,
// update or delete is attempted on a context that carries no tenant.
//
// This is dbkit's fail-closed default: a tenant-scoped statement never runs
// unfiltered, and never quietly returns zero rows in place of this error
// either — both look like "it worked" from the caller's side, and either one
// is exactly the shape of a cross-tenant data leak or an hours-long "why is
// my data missing" debugging session, which is precisely what this error
// exists to turn into an immediate, loud failure instead.
var ErrMissingTenantContext = apperr.Internal("dbkit.tenant_context_required")

// ErrTenantIDImmutable is the *apperr.Error returned when an Update or
// Updates payload against a tenant-scoped model tries to set tenant_id to a
// tenant other than the one in the request context. Moving a row from one
// tenant to another must never happen as a side effect of an ordinary
// update; a deliberate transfer needs its own explicit, audited operation.
var ErrTenantIDImmutable = apperr.Invalid("dbkit.tenant_id_immutable")

// TenantScoped marks a GORM model as belonging to a single tenant.
type TenantScoped interface {
	GetTenantID() pkgcore.TenantID
}

// TenantModel is the embeddable base for tenant-scoped models that do not
// need tenant_id to be part of their primary key. Embedding it satisfies
// TenantScoped (via the promoted GetTenantID below) and gives the
// isolation plugin a known "tenant_id" column to filter on and populate,
// without declaring that field by hand. See
// tenant_scope_tenantmodel_test.go for this working end to end through
// Repository[T] — Create, FindByID, and the tenant-override and
// cross-tenant-denial guarantees those methods promise, reached through a
// promoted field exactly as they would a directly-declared one.
//
// Its tag deliberately omits "primaryKey": a tenant-scoped table's primary
// key should be the composite (tenant_id, id) (backend coding standard
// §5), and TenantModel cannot supply that shape generically for every
// embedder. Resist the temptation to "fix" this by shadowing — redeclaring
// a same-named TenantID field directly on the embedding struct with your
// own primaryKey tag: Go field-selector rules make the shallower,
// shadowing field the one GORM actually writes and scans, but GetTenantID
// is a method promoted from TenantModel, so it unconditionally reads
// TenantModel's OWN embedded copy of TenantID instead — one GORM then
// never populates. The row's tenant_id column ends up correct; GetTenantID
// silently does not, which is worse: it makes Repository[T].FindByID deny
// even the row's legitimate owning tenant, since FindByID's own
// defense-in-depth check compares GetTenantID's (always empty) answer
// against ctx's tenant. TestTenantModel_ShadowingPromotedFieldToAddPrimaryKey_BreaksFindByIDForTheOwningTenant
// proves exactly this. A model that needs the composite key — which, per
// that standard, is every real tenant-scoped table in this codebase —
// declares TenantID directly instead, with whatever tag it needs, and does
// not embed TenantModel at all (as internal/testutil.Widget does).
// TenantModel is for the narrower case where a plain, non-key tenant_id
// column is genuinely enough.
type TenantModel struct {
	TenantID string `gorm:"column:tenant_id;not null;index:,priority:1"`
}

// GetTenantID returns m's tenant, satisfying TenantScoped.
func (m TenantModel) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(m.TenantID) }

// compile-time check that TenantModel satisfies TenantScoped.
var _ TenantScoped = TenantModel{}

// tenantScopePlugin is a gorm.Plugin that enforces tenant isolation for
// every model implementing TenantScoped: it injects "WHERE tenant_id = ?"
// ahead of every query, update and delete; it forces the tenant_id column to
// the request's tenant on every create, overwriting whatever the caller
// populated the struct with; and it rejects an update payload that tries to
// change tenant_id to a different tenant. A model that does not implement
// TenantScoped is completely unaffected — no callback so much as looks at
// it — so platform and identity tables never acquire an accidental tenant
// filter.
//
// It is unexported: Open (see open.go) is the only sanctioned way to obtain
// a *gorm.DB in this codebase, and it installs this plugin itself before
// ever returning a connection, so no other code needs to — or can — reach
// for this constructor directly.
//
// TenantScoped.GetTenantID is never called by the plugin itself: the
// interface is used purely as a marker — "this model opts into tenant
// scoping" — and the tenant to filter or populate by always comes from
// pkgcore.TenantFromContext(db.Statement.Context), never from the model.
// That is what makes the override in Create real: a caller cannot get a row
// inserted under a different tenant by populating the struct's TenantID
// field differently, because that field is never consulted.
//
// The plugin carries no state of its own and reads only db.Statement.Context
// on each call, so a single installed instance is safe to share across every
// request and goroutine: concurrent statements for different tenants never
// see, or interfere with, one another's filter.
//
// This is a defense-in-depth safety net, not a replacement for
// dbkit.Repository[T] — business code is still expected to go through
// Repository so a query is never built with a hand-written tenant filter in
// the first place — and it does not intercept raw SQL (db.Raw, db.Exec) or
// db.Row/db.Rows, which bypass the query/create/update/delete callbacks
// entirely; that is a separate, lint-enforced discipline (see the backend
// coding standards, section 3.2), not something a GORM callback can catch in
// general.
//
// It also does not implement the system-context cross-tenant escape hatch
// (pkgcore.WithSystemContext): when a tenant-scoped model is queried without
// a tenant in context, this plugin fails closed unconditionally, regardless
// of whether a SystemReason is present. Routing an authorized cross-tenant
// admin or job query around tenant filtering is left to a higher layer
// (expected to be dbkit.Repository[T]) that can make that decision
// deliberately and audit it, rather than this plugin guessing at it.
type tenantScopePlugin struct{}

// newTenantScopePlugin returns a ready-to-install tenantScopePlugin.
func newTenantScopePlugin() *tenantScopePlugin {
	return &tenantScopePlugin{}
}

// Name returns the plugin's identifier, satisfying gorm.Plugin.
func (p *tenantScopePlugin) Name() string { return tenantScopePluginName }

// Initialize registers the tenant-isolation callbacks on db, satisfying
// gorm.Plugin. Each callback is registered immediately before GORM's own
// SQL-building step for that operation ("gorm:query", "gorm:create",
// "gorm:update", "gorm:delete"), so it runs after any model-level hook
// (BeforeCreate and friends) has had its say and nothing after it can
// override its decision before the statement is built and executed. It
// performs no I/O, matching the Module.Register contract this plugin is
// typically wired from.
func (p *tenantScopePlugin) Initialize(db *gorm.DB) error {
	if err := db.Callback().Query().Before("gorm:query").
		Register(tenantScopePluginName+":query", tenantScopeBeforeQuery); err != nil {
		return err
	}
	if err := db.Callback().Create().Before("gorm:create").
		Register(tenantScopePluginName+":create", tenantScopeBeforeCreate); err != nil {
		return err
	}
	if err := db.Callback().Update().Before("gorm:update").
		Register(tenantScopePluginName+":update", tenantScopeBeforeUpdate); err != nil {
		return err
	}
	if err := db.Callback().Delete().Before("gorm:delete").
		Register(tenantScopePluginName+":delete", tenantScopeBeforeDelete); err != nil {
		return err
	}
	return nil
}

// compile-time check that tenantScopePlugin satisfies gorm.Plugin.
var _ gorm.Plugin = (*tenantScopePlugin)(nil)

// tenantScopeBeforeQuery injects "WHERE tenant_id = ?" ahead of every
// Find/First/Take/Count and similar reads of a tenant-scoped model, and
// fails the query closed when the statement's context carries no tenant.
// Registered Before "gorm:query", it applies uniformly to everything that
// routes through the query processor.
func tenantScopeBeforeQuery(db *gorm.DB) {
	if !isTenantScopedStatement(db.Statement) {
		return
	}
	tid, err := pkgcore.MustTenantFromContext(db.Statement.Context)
	if err != nil {
		_ = db.AddError(ErrMissingTenantContext.WithCause(err))
		return
	}
	groupExistingWhereConditions(db.Statement)
	db.Statement.Where(tenantScopeColumn+" = ?", string(tid))
}

// tenantScopeBeforeCreate forces the tenant_id column to the statement
// context's tenant on every insert of a tenant-scoped model, overwriting
// whatever the caller populated the struct's TenantID field with, and fails
// the create closed when the statement's context carries no tenant.
//
// SetColumn is called with fromCallbacks=true so that a batch create
// (Create on a slice) has every row forced, not only the one at
// stmt.CurDestIndex — at this point in the callback chain the per-row create
// loop has not started yet, so without fromCallbacks only the first row
// would be touched.
func tenantScopeBeforeCreate(db *gorm.DB) {
	if !isTenantScopedStatement(db.Statement) {
		return
	}
	tid, err := pkgcore.MustTenantFromContext(db.Statement.Context)
	if err != nil {
		_ = db.AddError(ErrMissingTenantContext.WithCause(err))
		return
	}
	db.Statement.SetColumn(tenantScopeColumn, string(tid), true)
}

// tenantScopeBeforeUpdate injects "WHERE tenant_id = ?" ahead of every
// Update/Updates against a tenant-scoped model, fails the update closed when
// the statement's context carries no tenant, and rejects an update payload
// that tries to change tenant_id to a tenant other than the context's —
// moving a row between tenants must never be a side effect of an ordinary
// update.
func tenantScopeBeforeUpdate(db *gorm.DB) {
	if !isTenantScopedStatement(db.Statement) {
		return
	}
	tid, err := pkgcore.MustTenantFromContext(db.Statement.Context)
	if err != nil {
		_ = db.AddError(ErrMissingTenantContext.WithCause(err))
		return
	}
	if value, present := updatePayloadTenantID(db.Statement); present && value != string(tid) {
		_ = db.AddError(ErrTenantIDImmutable.WithParam("attempted_tenant_id", value))
		return
	}
	groupExistingWhereConditions(db.Statement)
	db.Statement.Where(tenantScopeColumn+" = ?", string(tid))
}

// tenantScopeBeforeDelete injects "WHERE tenant_id = ?" ahead of every
// Delete against a tenant-scoped model, and fails the delete closed when the
// statement's context carries no tenant.
func tenantScopeBeforeDelete(db *gorm.DB) {
	if !isTenantScopedStatement(db.Statement) {
		return
	}
	tid, err := pkgcore.MustTenantFromContext(db.Statement.Context)
	if err != nil {
		_ = db.AddError(ErrMissingTenantContext.WithCause(err))
		return
	}
	groupExistingWhereConditions(db.Statement)
	db.Statement.Where(tenantScopeColumn+" = ?", string(tid))
}

// groupExistingWhereConditions collapses every WHERE expression the caller
// has already attached to stmt (if any) into a single AndConditions group,
// so that the tenant filter appended immediately afterward by
// tenantScopeBeforeQuery/Update/Delete always binds to the caller's entire
// pre-existing condition as one AND'd sibling, never only to the last
// top-level branch of it.
//
// Left alone, gorm's clause.Where.MergeClause (see gorm.io/gorm/clause/
// where.go) simply appends the tenant filter as one more top-level element
// of Where.Exprs, and SQL gives AND strictly higher precedence than OR:
// "a OR b AND tenant_id = ?" parses as "a OR (b AND tenant_id = ?)", not
// "(a OR b) AND tenant_id = ?". A caller query built with the chained
// .Or(...) method — e.g. db.Where("name = ?", x).Or("name = ?", y) — would
// therefore have its first OR branch left completely unfiltered by tenant:
// a cross-tenant read on the query path, and a cross-tenant mutation on the
// update/delete path. Grouping the existing expressions into one explicit
// AndConditions first, before the tenant condition is appended as a new
// sibling, turns the same merge into the intended
// "(a OR b) AND tenant_id = ?" instead — AndConditions.Build parenthesizes
// its contents whenever it holds more than one expression, and gorm's own
// raw-SQL heuristic (clause.buildExprs' Expr case, matched via the
// AndConditions case that wraps it) continues to parenthesize a single
// caller-supplied raw string that itself visibly contains " AND "/" OR ",
// exactly as it already did before this grouping was introduced.
//
// A statement with no existing WHERE clause — the overwhelmingly common
// case, since dbkit.Repository[T] never builds one before the plugin runs —
// is left untouched: db.Statement.Where's own merge already produces the
// correct, single-condition SQL with nothing to group.
func groupExistingWhereConditions(stmt *gorm.Statement) {
	c, ok := stmt.Clauses["WHERE"]
	if !ok {
		return
	}
	where, ok := c.Expression.(clause.Where)
	if !ok || len(where.Exprs) == 0 {
		return
	}
	c.Expression = clause.Where{Exprs: []clause.Expression{clause.AndConditions(where)}}
	stmt.Clauses["WHERE"] = c
}

// tenantScopedType is the reflect.Type of the TenantScoped interface. It
// lets isTenantScopedValue test a slice's element type reflectively, which a
// plain type assertion cannot do.
var tenantScopedType = reflect.TypeOf((*TenantScoped)(nil)).Elem()

// isTenantScopedStatement reports whether stmt's model opts into tenant
// scoping. It checks db.Statement.Model first, falling back to
// db.Statement.Dest only when Model is nil.
//
// Checking Model first is not an arbitrary preference: GORM's Count
// finisher briefly uses Dest to seed Model when Model is unset, then
// overwrites Dest with the *int64 result pointer before the query callbacks
// run — so for db.Model(&Widget{}).Count(&n), Dest is a *int64 by the time
// this function is called, and only Model still holds *Widget. Checking
// Dest first (or only Dest) would silently stop detecting Count on a
// tenant-scoped model.
func isTenantScopedStatement(stmt *gorm.Statement) bool {
	if isTenantScopedValue(stmt.Model) {
		return true
	}
	return isTenantScopedValue(stmt.Dest)
}

// isTenantScopedValue reports whether v refers to a type implementing
// TenantScoped. GORM hands callbacks the model in several shapes depending
// on how the call was written — a pointer to a struct (db.First(&w)), a
// pointer to a slice of structs (db.Find(&ws)), or a slice of pointers to
// structs (db.Find(&[]*Widget{})) — so this function unwraps any number of
// pointers and, for a slice or array, tests its element type (itself
// unwrapped of pointers) instead of the collection type.
//
// It works purely on types: it never calls reflect.ValueOf, never indexes
// into v, and never invokes GetTenantID, so a nil slice or a typed nil
// pointer is inspected safely. The only question answered here is "does
// this model opt into tenant scoping", never "what tenant does it belong
// to" — the tenant is always resolved from the context, never from the
// model, which is exactly what keeps tenantScopePlugin safe against a
// caller-populated tenant_id field.
func isTenantScopedValue(v interface{}) bool {
	if v == nil {
		return false
	}
	if _, ok := v.(TenantScoped); ok {
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

	return t.Implements(tenantScopedType) || reflect.PointerTo(t).Implements(tenantScopedType)
}

// asTenantIDString extracts a string form of an update-payload value that is
// expected to be a tenant id. It recognizes plain string (TenantModel's
// declared field type) and pkgcore.TenantID (the type callers most often
// hold a tenant id as, having just read it from context). Any other type
// resolves to the empty string, which can never match a real tenant id
// (pkgcore never reports an empty tenant as present) — so an update payload
// carrying a tenant_id of a shape this function cannot recognize is treated
// as a mismatch and rejected, rather than silently let through unchecked.
func asTenantIDString(v interface{}) string {
	switch s := v.(type) {
	case string:
		return s
	case pkgcore.TenantID:
		return string(s)
	default:
		return ""
	}
}

// updatePayloadTenantID reports the tenant_id value present in an Update or
// Updates payload (stmt.Dest), and whether the payload touches tenant_id at
// all. It understands both shapes GORM accepts for an update payload: a map
// keyed by either the column name ("tenant_id") or the struct field name
// ("TenantID"), and a struct or pointer-to-struct whose fields become the
// SET clause.
//
// A struct payload's tenant_id field left at its zero value is reported as
// absent, matching GORM's own struct-update convention that a zero-valued
// field is omitted from the SET clause rather than explicitly set to zero —
// there is no way to tell those two intentions apart from the struct alone.
// A map payload has no such ambiguity: a present key is always an explicit
// write, even one that happens to equal the context tenant (which is
// harmless and is not rejected by the caller of this function).
func updatePayloadTenantID(stmt *gorm.Statement) (value string, present bool) {
	if stmt.Schema == nil {
		return "", false
	}
	field := stmt.Schema.LookUpField(tenantScopeColumn)
	if field == nil {
		return "", false
	}

	switch dest := stmt.Dest.(type) {
	case map[string]interface{}:
		return mapTenantID(field, dest)
	case *map[string]interface{}:
		if dest == nil {
			return "", false
		}
		return mapTenantID(field, *dest)
	}

	destValue := reflect.ValueOf(stmt.Dest)
	for destValue.Kind() == reflect.Pointer {
		if destValue.IsNil() {
			return "", false
		}
		destValue = destValue.Elem()
	}
	if destValue.Kind() != reflect.Struct {
		return "", false
	}

	raw, isZero := field.ValueOf(stmt.Context, destValue)
	if isZero {
		return "", false
	}
	return asTenantIDString(raw), true
}

// mapTenantID looks up field's column name or Go field name as a key in an
// Update/Updates map payload.
func mapTenantID(field *schema.Field, m map[string]interface{}) (value string, present bool) {
	for _, key := range [...]string{field.DBName, field.Name} {
		if raw, ok := m[key]; ok {
			return asTenantIDString(raw), true
		}
	}
	return "", false
}
