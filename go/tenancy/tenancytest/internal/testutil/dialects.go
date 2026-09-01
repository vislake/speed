// Package testutil holds test-only helpers shared across tenancytest's own
// _test.go files (backend coding standard §13: a helper shared by more than
// one _test.go file belongs in a dedicated internal/testutil package, never
// duplicated across files or defined inline in one that another needs).
//
// It is unexported to every module outside tenancy itself. Callers writing
// their own module's tests never import this package — they use
// github.com/vislake/speed/go/dbkit/dbtest directly, the publicly
// importable dual-dialect helper this package's Dialects merely arranges
// into a table this package's own tests address positionally — see
// Dialects's own doc comment for the rule governing which index a plain,
// non-integration-tagged _test.go file may use.
package testutil

import (
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit/dbtest"
)

// Dialect names one *gorm.DB constructor from dbkit/dbtest, paired with a
// label suitable for t.Run.
type Dialect struct {
	// Name identifies the dialect for t.Run subtest names.
	Name string
	// NewDB constructs a fresh, ready-to-migrate *gorm.DB for this dialect.
	NewDB func(t *testing.T) *gorm.DB
}

// Dialects returns the dual-dialect matrix (backend coding standard §13):
// index 0 is always SQLite, index 1 is always PostgreSQL. Order is part of
// this function's contract -- every caller in this package addresses an
// entry positionally rather than by name.
//
// Do NOT range over the full result, or otherwise reach index 1, from a
// plain (non-integration-tagged) _test.go file. Index 1's NewDB is
// dbtest.NewPostgres, which -- on every call it is not skipped for lack of
// Docker -- starts a brand-new, disposable PostgreSQL container via
// testcontainers-go: exactly the real Docker/Postgres I/O the backend
// coding standard's testing layout rule (§13) requires to live behind a
// package-level integration_test/ directory guarded by
// //go:build integration, specifically so a plain "go test ./..." never
// triggers it -- mirroring dbkit's own integration_test/ precedent one
// module over. This package's own unit-tier callers (assert_isolated_test.go
// and friends, in the tenancytest package two directories up) use only
// Dialects()[0]; the postgres leg of those same scenarios runs instead from
// the tenancytest/integration_test package, behind that build tag, which is
// the only place index 1 may be used.
func Dialects() []Dialect {
	return []Dialect{
		{Name: "sqlite", NewDB: dbtest.NewSQLite},     // index 0: safe from any plain _test.go file
		{Name: "postgres", NewDB: dbtest.NewPostgres}, // index 1: tenancytest/integration_test only -- see doc comment above
	}
}
