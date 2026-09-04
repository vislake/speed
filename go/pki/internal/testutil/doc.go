// Package testutil holds the pki module's own test helpers: the
// dual-dialect migrated-database constructors backend-coding-standards §13
// asks every module to provide, so this module's tests never duplicate the
// dbkit.MigrationRegistry wiring inline. See db.go.
package testutil
