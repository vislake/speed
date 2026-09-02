package notes

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// newMigratedRepository returns a Repository backed by a fresh, per-test
// SQLite database already migrated with this module's own real
// Migrations() -- not a hand-written schema shortcut -- so a test failure
// here can never be explained away as "the test fixture's schema diverged
// from the real migration files".
func newMigratedRepository(t *testing.T) *Repository {
	t.Helper()

	db := dbtest.NewSQLite(t)

	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(NewModule(db)); err != nil {
		t.Fatalf("register notes module for migrations: %v", err)
	}
	if err := registry.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply notes migrations: %v", err)
	}

	return NewRepository(db)
}

// TestRepository_AssertIsolated runs the mandatory tenant-isolation suite
// (root CLAUDE.md's multi-tenant isolation rule; backend coding standard
// §3.3/§13; go/dbkit/AGENTS.md's "Known limitations" section, which named
// this exact obligation before tenancytest existed to fulfill it) against
// notes' real dbkit.Repository[Note] usage -- this is that suite's first
// real caller anywhere in the repository. Note is tenant data, not
// identity or platform data (docs/internal/04-data-and-tenancy.md's
// data-domain table), so AssertIsolated is the correct half of the
// AssertIsolated/AssertNotTenantScoped pair, never both.
func TestRepository_AssertIsolated(t *testing.T) {
	repo := newMigratedRepository(t)

	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *Note {
		return &Note{
			ID:          uuid.NewString(),
			TenantModel: dbkit.TenantModel{TenantID: string(tenant)},
			Text:        "isolation test note",
		}
	})
}
