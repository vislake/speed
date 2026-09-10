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
// not a bare one it would have to wire up itself. Neither applies any
// migration: this package is imported by tests belonging to modules dbkit
// has never heard of, each with its own model(s) and its own migration
// source, so baking in any particular schema here — even dbkit's own
// internal/testutil.Widget fixture — would be wrong for every caller
// outside dbkit itself. A caller migrates the returned connection itself
// before using it, either with Migrate — which applies the migration sets
// it names (Migration{Module, FS} values composed from the caller's own
// migrations package) through the real dbkit.MigrationRegistry, the same
// mechanism production boot runs, and is also the shape a staged-upgrade
// test uses (the frozen older set first, the full set second) — or, for a
// lightweight, non-production test, with a plain db.Exec of a fixture's
// migration SQL. Each module's own internal/testutil keeps that
// migrate-after-open composition as a module-local wrapper (see e.g.
// go/pki/internal/testutil), so its tests spell the two steps nowhere.
package dbtest
