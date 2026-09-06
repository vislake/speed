// Package testutil holds the pki module's own test helpers: the
// dual-dialect migrated-database constructors backend-coding-standards §13
// asks every module to provide, so this module's tests never duplicate the
// dbkit.MigrationRegistry wiring inline -- and the migration-0008
// duplicate-ledger upgrade scenario (migration_dedupe.go) shared by the
// SQLite unit leg and the PostgreSQL integration tier. See db.go and
// migration_dedupe.go.
package testutil
