package dbkit

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
)

// tenantSessionGUCName is the PostgreSQL custom session variable (GUC)
// WithTenantSession sets, and the name a production Row-Level Security
// policy's USING/WITH CHECK clause must reference via
// current_setting(tenantSessionGUCName, true) to be driven by it. See
// integration_test/postgres_tenant_session_test.go, which proves a real RLS
// policy keyed on exactly this name is engaged end to end by
// WithTenantSession itself (as opposed to integration_test/postgres_rls_test.go,
// which proves only that the underlying database mechanism works when the
// session variable is set by hand).
const tenantSessionGUCName = "app.current_tenant"

// setTenantSessionGUCSQL sets tenantSessionGUCName for the current
// transaction only, with its value bound as an ordinary query parameter.
//
// This reads "SELECT set_config(...)", not the more obvious
// "SET LOCAL app.current_tenant = ?": PostgreSQL's SET and SET LOCAL
// statements do not accept a bound query parameter for the value being
// set — "SET LOCAL app.current_tenant = $1" is rejected outright by the
// server with a syntax error (SQLSTATE 42601, "syntax error at or near
// "$1""), confirmed against a live PostgreSQL 16 server while writing this
// function, independent of driver or ORM. That is a hard limitation of
// PostgreSQL's own grammar for SET, not a gorm or pgx quirk to work around
// differently.
//
// set_config(setting_name, new_value, is_local), by contrast, is an
// ordinary SQL function call, so new_value is a normal bound parameter like
// any other function argument — which is what keeps a tenant id out of the
// SQL text and safe from injection here, with no bespoke escaping of its
// own to get right. Its third argument, is_local = true, was also confirmed
// against a live server to give it the exact same transaction-scoped
// semantics SET LOCAL has: current_setting reads the value back inside the
// transaction, and reads back empty again immediately after both a COMMIT
// and a ROLLBACK. So this is not an approximation of "SET LOCAL app.current_tenant
// = <tenant>" — it is that statement's parameter-binding-capable equivalent,
// with the identical revert-at-end-of-transaction guarantee WithTenantSession's
// own doc comment depends on.
//
// Do not "simplify" this back to a literal SET LOCAL statement with the
// tenant id interpolated into the SQL string: that would reintroduce both
// the injection risk set_config's parameter binding avoids and a statement
// shape that is one refactor away from someone reasonably trying to
// parameterize it again and reintroducing the syntax error above.
const setTenantSessionGUCSQL = "SELECT set_config('" + tenantSessionGUCName + "', ?, true)"

// WithTenantSession runs fn inside a transaction. When db is connected to
// PostgreSQL, it first sets the session-local app.current_tenant GUC inside
// that same transaction (so it cannot outlive the transaction or leak across
// a pooled connection to a later, unrelated request), enabling PostgreSQL
// Row-Level Security policies that reference current_setting('app.current_tenant', true)
// as a database-level defense layer independent of and beneath this
// package's Go-side tenant filtering. For SQLite (no RLS support), this is a
// plain transaction wrapper with no GUC step.
//
// This is exported so the documented raw-SQL / reporting-query escape hatch
// (backend-coding-standards SKILL.md §3.2) can opt into the same protection
// instead of reinventing it.
//
// WithTenantSession resolves the tenant from ctx itself, via
// pkgcore.MustTenantFromContext, and fails closed — before db.Transaction
// is ever called, so no transaction is opened at all for a call that is
// guaranteed to fail — when ctx carries none. Every Repository[T] method
// already makes this exact same check before ever calling WithTenantSession
// (see repository.go); the duplication is intentional defense in depth, not
// a bug to remove from either side, so that WithTenantSession is just as
// safe to call directly (from the raw-SQL escape hatch above) as it is from
// Repository.
//
// Which dialect db speaks is read from db.Name() (gorm's own dialect-name
// accessor, backed by the Dialector each driver package supplies) rather
// than any dbkit-specific field, so this also works for a *gorm.DB built
// outside dbkit.Open — confirmed to return exactly "postgres"
// (gorm.io/driver/postgres) and "sqlite" (github.com/glebarez/sqlite),
// matching the DialectPostgres and DialectSQLite constants in open.go
// exactly, by opening each dialector directly and calling Name() on it
// while writing this function.
//
// If the GUC-setting step fails, WithTenantSession aborts the whole
// transaction and returns that error without ever calling fn: proceeding
// anyway would silently mean RLS is not actually engaged for this
// operation, which must never happen without at minimum a loud, immediate
// error — the same fail-closed philosophy every other tenant check in this
// package follows.
func WithTenantSession(ctx context.Context, db *gorm.DB, fn func(tx *gorm.DB) error) error {
	tid, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return err
	}

	isPostgres := db.Name() == string(DialectPostgres)

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if isPostgres {
			if err := tx.Exec(setTenantSessionGUCSQL, string(tid)).Error; err != nil {
				return fmt.Errorf("dbkit: set %s session GUC: %w", tenantSessionGUCName, err)
			}
		}
		return fn(tx)
	})
}
