package dbkit

import (
	"context"
	"errors"
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

// ErrNestedTenantSession is returned by WithTenantSession when db already
// represents an open transaction — a *gorm.DB obtained from inside another
// WithTenantSession call's own fn, or from a bare db.Begin() — rather than a
// fresh connection or connection pool.
//
// This is not merely a style objection to nesting. WithTenantSession's own
// audit-publish step (see the type's doc comment on auditCapturePlugin in
// audit_capture.go) treats its own db.Transaction call returning nil as
// proof the real, outermost transaction has genuinely committed, and
// publishes every event this call's writes buffered only on the strength of
// that proof. GORM's db.Transaction (gorm.io/gorm@v1.31.2/finisher_api.go)
// does not make that distinction itself: called against a *gorm.DB whose
// Statement.ConnPool already is a gorm.TxCommitter, it issues a SAVEPOINT
// instead of a real BEGIN, and returns nil the instant that inner savepoint
// is released — regardless of whether the real, still-open outer
// transaction goes on to commit or roll back. A nested WithTenantSession
// call would read that nil exactly like a top-level one, and publish its
// buffered events immediately: a real, reproduced phantom audit event
// surviving a real rollback of the enclosing transaction (see
// tenant_session_test.go's
// TestWithTenantSession_Nested_RefusesRatherThanPublishingBeforeOuterCommits,
// which fails with exactly that outcome against a version of this function
// that omits this check).
//
// Rather than special-case "nested, but only when audit capture happens to
// be enabled" — a distinction a caller has no way to see from the outside,
// and one more accident away from being wrong again the next time this
// function grows a new commit-triggered side effect — WithTenantSession
// refuses every nested call outright: fails closed before opening any
// transaction, even a savepoint, and never calls fn at all, exactly like
// the missing-tenant-context check just above this one. No caller in this
// codebase nests WithTenantSession calls today: every real call site —
// every Repository[T] method, and the documented raw-SQL escape hatch —
// passes the base *gorm.DB dbkit.Open returns, never a tx handle obtained
// from inside another WithTenantSession call's own fn.
var ErrNestedTenantSession = errors.New("dbkit: WithTenantSession called with an already-open transaction")

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
// When db carries the audit write-capture plugin (Options.AuditBus was set
// on the Open call db came from), this is also the one place that plugin's
// captured events for this transaction ever actually get published: fn runs
// under a context carrying a fresh per-transaction *auditBuffer, every
// Auditable write inside fn appends to that buffer instead of publishing
// directly (audit_capture.go's capture), and once — only once — the
// transaction below has returned nil (genuinely committed, never on a
// rollback), every buffered event is published for real. This is why an
// Auditable model written through Repository[T] (which always calls this
// function) never has its audit trail published for a write that later
// rolled back, and a publish failure at that point can only be reported as
// an alert (auditPublishFailed), never fail this call — the transaction has
// already committed, so there is nothing left here to roll back.
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
//
// WithTenantSession refuses outright — before opening any transaction, even
// a nested one, and without ever calling fn — when db already represents an
// open transaction (see ErrNestedTenantSession's doc comment for why: the
// audit-publish mechanism above depends on this call's own db.Transaction
// returning nil meaning the real, outermost transaction genuinely
// committed, which is not what a nested call's own nil return means).
func WithTenantSession(ctx context.Context, db *gorm.DB, fn func(tx *gorm.DB) error) error {
	tid, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return err
	}

	if committer, ok := db.Statement.ConnPool.(gorm.TxCommitter); ok && committer != nil {
		return ErrNestedTenantSession
	}

	isPostgres := db.Name() == string(DialectPostgres)

	// Only a db carrying the audit write-capture plugin (Options.AuditBus
	// was set) needs a buffer at all — this type-asserts db.Plugins, the
	// map every db.Use registration lands in, rather than adding a new
	// parameter to this function or to Repository[T]: the plugin is
	// already reachable from the exact *gorm.DB every caller already
	// passes here. A db with no such plugin (the common case before this
	// mechanism existed, and every caller that never sets AuditBus) takes
	// the pre-existing code path unchanged: ctx flows through untouched,
	// with no buffer allocated and no extra work done.
	plugin, auditEnabled := db.Plugins[auditCapturePluginName].(*auditCapturePlugin)

	txCtx := ctx
	var buf *auditBuffer
	if auditEnabled {
		txCtx, buf = withAuditBuffer(ctx)
	}

	if err := db.WithContext(txCtx).Transaction(func(tx *gorm.DB) error {
		if isPostgres {
			if err := tx.Exec(setTenantSessionGUCSQL, string(tid)).Error; err != nil {
				return fmt.Errorf("dbkit: set %s session GUC: %w", tenantSessionGUCName, err)
			}
		}
		return fn(tx)
	}); err != nil {
		return err
	}

	// The transaction above has now genuinely committed. Publish whatever
	// this transaction's own writes buffered — never before this point,
	// and never at all had the transaction returned a non-nil error above.
	if auditEnabled {
		plugin.publishBuffered(ctx, buf)
	}
	return nil
}
