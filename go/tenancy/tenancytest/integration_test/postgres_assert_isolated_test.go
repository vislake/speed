//go:build integration

// Package tenancytest_test holds tenancytest's integration tier: the
// postgres-dialect leg of tenancytest's own self-tests (AssertIsolated and
// AssertNotTenantScoped proving themselves end to end against a real
// dbkit.Repository[T] / *gorm.DB). It is physically separate from
// tenancytest's unit tests (assert_isolated_test.go,
// assert_not_tenant_scoped_test.go, and friends, all in package tenancytest
// itself, one directory up) and carries the "integration" build tag, per
// the backend coding standard's testing layout rule (§13): a plain
// "go test ./..." never compiles or runs anything in this directory; it is
// invoked explicitly with "go test -tags=integration ./...". This mirrors
// go/dbkit/integration_test's own package-doc comment and directory shape
// one module over.
//
// Every test here starts its own disposable PostgreSQL container via
// testutil.Dialects()'s index-1 entry (dbtest.NewPostgres) and requires a
// working Docker (or Docker-API-compatible) daemon; there is no fallback or
// skip-on-missing-Docker path beyond what dbtest.NewPostgres itself already
// provides (t.Skip when no daemon is reachable) -- the SQLite leg of each of
// these same scenarios already runs unconditionally, in the unit tier, from
// the corresponding non-integration-tagged test named in each function's
// doc comment below.
package tenancytest_test

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
	"github.com/vislake/speed/go/tenancy/tenancytest/internal/testutil"
)

// sprocket mirrors the unexported fixture of the same name in
// assert_isolated_test.go (package tenancytest, one directory up). This
// package is a separate compiled package rooted in a different directory,
// so it cannot reach that one's unexported type -- tenancytest's own doc
// comment on AssertIsolated tells every caller, including this one, to
// define its own small fixture directly rather than share one across
// packages.
type sprocket struct {
	ID       string `gorm:"column:id;primaryKey;size:64"`
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`
	Label    string `gorm:"column:label;size:255"`
}

// GetTenantID satisfies dbkit.TenantScoped.
func (s sprocket) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(s.TenantID) }

// compile-time check that sprocket satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = sprocket{}

// createSprocketsTableSQL matches assert_isolated_test.go's table of the
// same name exactly, so the postgres and sqlite legs of this scenario stay
// identical apart from dialect.
const createSprocketsTableSQL = `CREATE TABLE sprockets (
	id VARCHAR(64) NOT NULL,
	tenant_id VARCHAR(64) NOT NULL,
	label VARCHAR(255) NOT NULL DEFAULT '',
	PRIMARY KEY (tenant_id, id)
)`

// sprocketIDSeq gives every sprocket fixture record a distinct id across an
// entire test binary run, satisfying AssertIsolated's requirement that
// newRecord never repeat an id.
var sprocketIDSeq atomic.Uint64

// newSprocket returns a factory suitable for AssertIsolated's newRecord
// parameter: every call returns a sprocket with a fresh, unique id.
func newSprocket(tenant pkgcore.TenantID) *sprocket {
	return &sprocket{
		ID:       fmt.Sprintf("sprocket-%d", sprocketIDSeq.Add(1)),
		TenantID: string(tenant),
		Label:    "gadget",
	}
}

// TestAssertIsolated_Sprocket_Postgres is the postgres-dialect leg of
// tenancytest.TestAssertIsolated_Sprocket (assert_isolated_test.go, package
// tenancytest); see that test's doc comment for what it proves and this
// package's own doc comment above for why the postgres leg lives here
// instead of there.
func TestAssertIsolated_Sprocket_Postgres(t *testing.T) {
	db := testutil.Dialects()[1].NewDB(t) // postgres: see this package's doc comment
	if err := db.Exec(createSprocketsTableSQL).Error; err != nil {
		t.Fatalf("create sprockets table: %v", err)
	}
	repo := dbkit.NewRepository[sprocket](db)

	tenancytest.AssertIsolated(t, repo, newSprocket)
}
