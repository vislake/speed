// Package dbtest provides the dual-dialect test-database helpers every
// module's tests are expected to use (backend coding standard §13's
// "dual-dialect matrix": dbtest.NewPostgres(t) and dbtest.NewSQLite(t)):
// NewSQLite for a private, hermetic SQLite database, and NewPostgres for a
// real, disposable PostgreSQL instance started with testcontainers-go.
//
// It exists as its own publicly importable package, separate from dbkit's
// own internal/testutil, specifically so that OTHER modules' tests can
// import it too — internal/testutil is unexported and reachable only from
// within the dbkit module itself, which is why it could never be the thing
// the coding standard names.
//
// Both constructors return a *gorm.DB obtained through dbkit.Open, so a
// caller gets dbkit's full mandatory wiring (fixed connection-pool limits
// and the tenant-scoping GORM plugin) on top of the underlying connection,
// not a bare one it would have to wire up itself. Neither constructor
// applies any migration: this package is imported by tests belonging to
// modules dbkit has never heard of, each with its own model(s) and its own
// migration source, so baking in any particular schema here — even
// dbkit's own internal/testutil.Widget fixture — would be wrong for every
// caller outside dbkit itself. A caller migrates the returned connection
// itself before using it, typically by constructing its own
// dbkit.MigrationRegistry, registering its pkgcore.Module, and calling
// Apply(ctx, db, dialect) against it — the same shape dbkit/AGENTS.md's
// "Typical integration" section and dbkit's own example_test.go show for a
// business module — or with a plain db.Exec of a fixture's migration SQL
// for a lightweight, non-production test.
package dbtest
