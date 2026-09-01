package dbkit

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// Column and struct-field names every T managed by Repository[T] is expected
// to follow. Repository cannot express this convention through T's type
// parameter beyond the TenantScoped method — Go generics have no way to
// require "an exported string field named X" — so it is a documented
// convention instead, the same one every tenant-scoped table in this
// codebase already follows (see internal/testutil's Widget, and the
// Subscription example in the backend coding standard's data-models
// section):
//
//	type Model struct {
//	    ID       string `gorm:"primaryKey;size:26"`
//	    TenantID string `gorm:"primaryKey;size:26"`
//	    ...
//	}
//
// idColumn and tenantIDColumn name the SQL columns Repository's own WHERE
// clauses reference. idFieldName and tenantIDFieldName name the
// corresponding Go struct fields Repository reaches through reflection in
// the one place a SQL condition cannot do the job instead: writing the
// tenant onto a model before Create or Update, since TenantScoped exposes
// only a getter.
//
// TenantScoped itself — the marker interface Repository[T] constrains its
// type parameter to — is declared in tenant_scope.go, not here, since
// dbkit's tenant-isolation GORM plugin shares the exact same interface (both
// need "a model that says which tenant it belongs to"; there is only one
// such concept in this package, not two that happen to coincide). This file
// additionally expects every implementation to declare the exported "ID"
// and "TenantID" string fields the constants below name the columns for:
// TenantScoped cannot demand that in the type system, since it requires
// only the GetTenantID accessor, so a T that omits either field is caught
// at run time instead — the affected Repository method returns an error the
// first time it is used, rather than failing to compile.
const (
	idColumn          = "id"
	tenantIDColumn    = "tenant_id"
	idFieldName       = "ID"
	tenantIDFieldName = "TenantID"
)

// ErrRecordNotFound is returned by FindByID, Update, and Delete when no row
// matches both the given id and the caller's tenant. This includes the case
// where a row with that id exists but belongs to a different tenant:
// Repository deliberately never distinguishes the two outcomes in what it
// returns, because doing so — for example by surfacing a permission-style
// error only when the id turns out to exist — would let a caller in one
// tenant learn that a given id exists in another tenant at all, which is
// itself a (smaller, but real) cross-tenant information leak.
var ErrRecordNotFound = apperr.NotFound("dbkit.record_not_found")

// ErrMissingID is returned by Update when the model it was given has an
// empty id. Update saves the model's full state over an existing row
// identified by that id, so an empty one is a caller error, not a
// not-found: there is no row this call could ever have meant.
var ErrMissingID = apperr.Invalid("dbkit.missing_id")

// Repository is the generic, tenant-scoped data-access base every business
// module embeds instead of holding a raw *gorm.DB directly (backend coding
// standard, section 3.2, "Data access goes through Repository only").
//
// It is the second of the design's three tenant-isolation layers (see
// docs/internal/04-data-and-tenancy.md's "multi-tenant isolation: triple
// protection" section):
//
//  1. The GORM plugin dbkit.Open installs, which auto-appends
//     "WHERE tenant_id = ?" to query/update/delete callbacks for any model
//     implementing TenantScoped, reading the tenant from context.
//  2. Repository[T], implemented here: resolves the tenant from context
//     itself, before ever touching the database, and fails closed if there
//     is none — it does not trust the plugin above to catch a missing
//     tenant. It also scopes every query it issues by tenant_id explicitly,
//     and re-verifies the tenant on every row it hands back or acts on, so
//     isolation holds even if layer 1 were ever absent, misconfigured, or
//     bypassed by a *gorm.DB not built through dbkit.Open.
//  3. PostgreSQL row-level security in production, as a final backstop
//     below the Go layer entirely.
//
// Layer 3 is wired in below: every method's actual database call routes
// through WithTenantSession (tenant_session.go) instead of calling
// r.db.WithContext(ctx) directly. WithTenantSession runs the call inside an
// explicit transaction and, when db is PostgreSQL, sets the session-local
// app.current_tenant GUC inside that same transaction first — the setting a
// production RLS policy's current_setting('app.current_tenant', true)
// reads — before the call itself ever runs. This still assumes production
// operates the database side independently (a restricted, non-BYPASSRLS
// application role plus such a policy on every tenant-scoped table); dbkit
// supplies the session-variable wiring, not the role or the policy. See
// tenant_session.go's own doc comment for why that step is a
// "SELECT set_config(...)" call rather than a literal SET LOCAL statement,
// integration_test/postgres_rls_test.go for confirmation that the
// underlying database mechanism works at all, and
// integration_test/postgres_tenant_session_test.go for confirmation that
// WithTenantSession's own GUC step is what a real RLS policy actually
// responds to end to end.
//
// This costs a real, deliberately accepted tradeoff: every single
// Repository call now opens its own transaction (a BEGIN plus a COMMIT or
// ROLLBACK), including a plain FindByID or List that would otherwise run as
// one bare statement with no transaction at all. That overhead is paid on
// purpose, in exchange for layer 3 actually being reachable through
// Repository — the one sanctioned data-access path every business module
// uses — rather than only through hand-written raw-SQL code that remembers
// to call WithTenantSession itself.
//
// db is expected to come from dbkit.Open, which installs layer 1 above.
// Repository still enforces tenant scoping correctly given a *gorm.DB built
// another way — its own checks are complete on their own by design, since
// the whole point of a second, independent layer is that it does not
// depend on the first — and this package's own tests do exactly that,
// running Repository against internal/testutil.NewTestSQLite's plain,
// plugin-less connection. That is a valid way to unit-test Repository; it
// is not a valid way to run it in production, which must still go through
// dbkit.Open for layer 1.
type Repository[T TenantScoped] struct {
	db *gorm.DB
}

// NewRepository builds a Repository managing model type T, backed by db.
// See the Repository doc comment above for what db is expected to be.
func NewRepository[T TenantScoped](db *gorm.DB) *Repository[T] {
	return &Repository[T]{db: db}
}

// Create resolves the tenant from ctx and inserts m under it.
//
// It overwrites m's TenantID field with the resolved tenant before
// inserting, regardless of what m held on entry: the caller cannot cause a
// row to be created under a different tenant than ctx by any value it sets
// there, which is what makes the tenant column trustworthy for every other
// method to key off later.
//
// When ctx carries no tenant, Create returns pkgcore's error unmodified
// (see pkgcore.MustTenantFromContext; test with errors.Is against
// pkgcore.ErrNoTenant) before the database is touched at all.
func (r *Repository[T]) Create(ctx context.Context, m *T) error {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return err
	}
	if err := setTenantID(m, tenant); err != nil {
		return err
	}
	return WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Create(m).Error
	})
}

// FindByID resolves the tenant from ctx and returns the row with the given
// id, scoped to that tenant. It returns ErrRecordNotFound — never gorm's own
// not-found error, and never any error a caller could use to tell "no such
// id" apart from "that id belongs to another tenant" — when nothing
// matches. See the ErrRecordNotFound doc comment for why the two cases are
// deliberately indistinguishable from the outside.
//
// FindByID assumes T's table has a primary-key column named "id" that the
// id parameter is compared against directly; see the column-convention
// comment above the idColumn/tenantIDColumn constants in this file.
//
// When ctx carries no tenant, FindByID returns pkgcore's error unmodified
// before the database is touched at all.
func (r *Repository[T]) FindByID(ctx context.Context, id string) (*T, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	var m T
	err = WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where(idColumn+" = ?", id).
			Where(tenantIDColumn+" = ?", tenant).
			First(&m).Error
	})
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, ErrRecordNotFound.WithParam("id", id)
	case err != nil:
		return nil, err
	}

	// Defense in depth: verify the row we actually got back belongs to the
	// caller's tenant, independent of the WHERE clause above catching it.
	// See the Repository doc comment's layering explanation.
	if m.GetTenantID() != tenant {
		return nil, ErrRecordNotFound.WithParam("id", id)
	}
	return &m, nil
}

// Update resolves the tenant from ctx and saves m's full state — every
// field, not a partial patch — over the existing row identified by m's id,
// scoped to that tenant. Like Create, it overwrites m's TenantID field with
// the resolved tenant before writing, so Update can never move a row into,
// or write over a row under, a different tenant than ctx.
//
// Update reads m's id through the same "ID" field convention documented
// above the idColumn/tenantIDColumn constants, since TenantScoped exposes
// no accessor for it, and rejects an empty one with ErrMissingID before
// going anywhere near the database: gorm's Save silently inserts a new row
// instead of updating when a model's primary-key field looks unset, and an
// empty id is exactly that.
//
// The write itself pre-selects every field ("Select(\"*\")") before calling
// Save. That is load-bearing, not decorative: gorm's Save, whenever the
// update it issues matches zero rows, otherwise falls back to an
// upsert-style Create — which would silently insert a brand new row the
// moment the tenant_id condition below excludes the real one, turning a
// cross-tenant Update attempt into a cross-tenant Create. Pre-selecting
// every field disables that fallback (see gorm's finisher_api.go), which
// also happens to be exactly the "full record, not partial patch" behavior
// this method promises regardless.
//
// Update returns ErrRecordNotFound, not a generic gorm error and not a
// silently-created row, when nothing matches — including when m's id exists
// under a different tenant.
//
// When ctx carries no tenant, Update returns pkgcore's error unmodified
// before the database is touched at all.
func (r *Repository[T]) Update(ctx context.Context, m *T) error {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return err
	}

	id, ok := idOf(m)
	if !ok {
		return fmt.Errorf("dbkit: %T must have an exported %q string field for Repository to manage it", *m, idFieldName)
	}
	if id == "" {
		return ErrMissingID.WithParam("type", fmt.Sprintf("%T", *m))
	}

	if err := setTenantID(m, tenant); err != nil {
		return err
	}

	var rowsAffected int64
	if err := WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.
			Where(idColumn+" = ?", id).
			Where(tenantIDColumn+" = ?", tenant).
			Select("*").
			Save(m)
		rowsAffected = res.RowsAffected
		return res.Error
	}); err != nil {
		return err
	}
	if rowsAffected == 0 {
		return ErrRecordNotFound.WithParam("id", id)
	}
	return nil
}

// Delete resolves the tenant from ctx and deletes the row with the given
// id, scoped to that tenant. It returns ErrRecordNotFound, not a generic
// gorm error and not a silent no-op success, when nothing matches —
// including when id exists under a different tenant.
//
// When ctx carries no tenant, Delete returns pkgcore's error unmodified
// before the database is touched at all.
func (r *Repository[T]) Delete(ctx context.Context, id string) error {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return err
	}

	var rowsAffected int64
	err = WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		var zero T
		res := tx.
			Where(idColumn+" = ?", id).
			Where(tenantIDColumn+" = ?", tenant).
			Delete(&zero)
		rowsAffected = res.RowsAffected
		return res.Error
	})
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return ErrRecordNotFound.WithParam("id", id)
	}
	return nil
}

// List resolves the tenant from ctx and returns every row belonging to it.
// It takes no filter or pagination parameters yet — deliberately minimal;
// later modules extend it as their query needs grow.
//
// When ctx carries no tenant, List returns pkgcore's error unmodified
// before the database is touched at all.
func (r *Repository[T]) List(ctx context.Context) ([]T, error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return nil, err
	}

	var out []T
	if err := WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where(tenantIDColumn+" = ?", tenant).Find(&out).Error
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// setTenantID sets T's "TenantID" field (see the column-convention comment
// above the idColumn/tenantIDColumn constants) to tenant through
// reflection, since TenantScoped exposes only a getter. It is how Create
// and Update back-fill the tenant column themselves instead of trusting the
// value on the caller's struct literal.
func setTenantID[T any](m *T, tenant pkgcore.TenantID) error {
	f := reflect.ValueOf(m).Elem().FieldByName(tenantIDFieldName)
	if !f.IsValid() || !f.CanSet() || f.Kind() != reflect.String {
		return fmt.Errorf("dbkit: %T must have an exported %q string field for Repository to manage it", *m, tenantIDFieldName)
	}
	f.SetString(string(tenant))
	return nil
}

// idOf reads T's "ID" field (see the column-convention comment above the
// idColumn/tenantIDColumn constants) through reflection, since TenantScoped
// exposes no accessor for it. ok is false when T has no such exported
// string field.
func idOf[T any](m *T) (id string, ok bool) {
	f := reflect.ValueOf(m).Elem().FieldByName(idFieldName)
	if !f.IsValid() || f.Kind() != reflect.String {
		return "", false
	}
	return f.String(), true
}
