// Package testutil provides fixtures shared across dbkit's test files: a
// minimal tenant-scoped GORM model (Widget) and a helper that opens an
// in-memory SQLite database with that model's migration already applied.
//
// It is internal because it is a test aid, not part of dbkit's public API:
// only dbkit's own _test.go files and integration tests import it.
package testutil
