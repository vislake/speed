package smilesim

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// newTestSimulationStore returns a SimulationStore backed by a fresh,
// per-test SQLite database whose table and per-photo index were created by
// the real EnsureSchema path -- the same path cmd/server's wiring runs at
// boot, never a hand-written schema shortcut -- so an isolation failure
// here can never be explained away as "the test fixture's schema diverged
// from the real DDL".
func newTestSimulationStore(t *testing.T) *SimulationStore {
	t.Helper()
	db := dbtest.NewSQLite(t)
	store := NewSimulationStore(db)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	return store
}

// isoTenantA and isoTenantB are the two tenants TestSimulationStore_
// AssertIsolated exercises. Fixed values, not derived from the test name:
// the suite runs against a fresh per-test database whose only rows are the
// ones it writes, so no cross-test collision is possible (unlike the
// tenancytest suite, which must derive its tenants because it cannot assume
// an empty table).
const (
	isoTenantA = "smilesim-iso-tenant-a"
	isoTenantB = "smilesim-iso-tenant-b"
)

// isoJobID builds one simulation record's job id: unique per call (the
// row's primary key), self-describing in failure output.
func isoJobID(tenant string, n int) jobs.JobID {
	return jobs.JobID(fmt.Sprintf("smilesim-iso-%s-job-%d", tenant, n))
}

// TestSimulationStore_AssertIsolated is SimulationStore's equivalent of
// the mandatory tenancytest.AssertIsolated suite (the multi-tenant
// isolation rule that suite and tools/check_repo_isolation.py enforce for
// every tenant-data repository). It cannot run the tenancytest suite
// itself: AssertIsolated
// reflects T's exported "ID" field and queries the "id" column, while
// simulationRecord's primary key is the go/jobs queue's application-
// generated JobID -- the exact documented reason this record embeds
// dbkit.TenantModel and this store embeds dbkit.Repository[simulationRecord]
// for Create only (see simulation_store.go's own doc comments). The suite
// below therefore asserts, through SimulationStore's real surface, every
// isolation property tenancytest.AssertIsolated pins that this
// Create-plus-tenant-scoped-lookup shape has: rows saved under one tenant
// are visible to that tenant's get/listByPhoto and to no other tenant's;
// the promoted Repository.Create overwrites a caller-forged TenantID with
// the context tenant; and a context carrying no tenant fails closed with
// pkgcore.ErrNoTenant before the database is touched. This is the suite
// named TestSimulationStore_AssertIsolated that check_repo_isolation.py
// recognizes as coverage for the SimulationStore embedding.
func TestSimulationStore_AssertIsolated(t *testing.T) {
	store := newTestSimulationStore(t)

	ctxA := pkgcore.WithTenant(context.Background(), pkgcore.TenantID(isoTenantA))
	ctxB := pkgcore.WithTenant(context.Background(), pkgcore.TenantID(isoTenantB))

	// Two tenants save records over the SAME photo object id -- the
	// collision that is meaningless across tenants' storage and the
	// sharpest shape for the isolation assertions below (a shared photo id
	// is exactly what two dental clinics could both simulate).
	const photoID = "photo-shared-across-tenants"

	aJobs := make([]jobs.JobID, 0, 2)
	for n := 1; n <= 2; n++ {
		jobID := isoJobID(isoTenantA, n)
		if err := store.save(ctxA, jobID, photoID, SimulationOptions{}); err != nil {
			t.Fatalf("save(tenant %q, record %d) error = %v", isoTenantA, n, err)
		}
		aJobs = append(aJobs, jobID)
	}
	bJobs := make([]jobs.JobID, 0, 2)
	for n := 1; n <= 2; n++ {
		jobID := isoJobID(isoTenantB, n)
		if err := store.save(ctxB, jobID, photoID, SimulationOptions{}); err != nil {
			t.Fatalf("save(tenant %q, record %d) error = %v", isoTenantB, n, err)
		}
		bJobs = append(bJobs, jobID)
	}

	t.Run("list_by_photo_scopes_to_the_calling_tenant", func(t *testing.T) {
		t.Helper()
		assertListScopedTo(t, store, ctxA, isoTenantA, isoTenantB, photoID, aJobs, bJobs)
		assertListScopedTo(t, store, ctxB, isoTenantB, isoTenantA, photoID, bJobs, aJobs)
	})

	t.Run("get_finds_the_owning_tenants_row_only", func(t *testing.T) {
		t.Helper()
		for _, jobID := range aJobs {
			row, has, err := store.get(ctxA, jobID)
			if err != nil {
				t.Fatalf("get(owning tenant %q, %q) error = %v", isoTenantA, jobID, err)
			}
			if !has {
				t.Fatalf("get(owning tenant %q, %q) = no record, want the row the owner saved", isoTenantA, jobID)
			}
			if row.GetTenantID() != pkgcore.TenantID(isoTenantA) {
				t.Errorf("get(owning tenant %q, %q).GetTenantID() = %q, want %q",
					isoTenantA, jobID, row.GetTenantID(), isoTenantA)
			}
		}
		for _, jobID := range bJobs {
			_, has, err := store.get(ctxA, jobID)
			if err != nil {
				t.Fatalf("get(tenant %q asking for %q's row) error = %v", isoTenantA, jobID, err)
			}
			if has {
				t.Errorf("get(tenant %q, %q) = a record, want none -- %q's row must be invisible across tenants",
					isoTenantA, jobID, isoTenantB)
			}
		}
	})

	t.Run("create_overwrites_a_forged_tenant_id_with_the_context_tenant", func(t *testing.T) {
		t.Helper()
		// A caller forging TenantID on the struct it hands to Create must
		// not be able to land a row under the forged tenant: the promoted
		// Repository.Create backfills TenantID from ctx (the same property
		// tenancytest.AssertIsolated's own forged-tenant check proves for
		// the generic base).
		jobID := isoJobID(isoTenantA, 99)
		forged := &simulationRecord{
			JobID:         string(jobID),
			TenantModel:   dbkit.TenantModel{TenantID: isoTenantB}, // forged
			PhotoObjectID: photoID,
			OptionsJSON:   "{}",
		}
		if err := store.Create(ctxA, forged); err != nil {
			t.Fatalf("Create(tenant %q, forged-TenantID record) error = %v", isoTenantA, err)
		}

		row, has, err := store.get(ctxA, jobID)
		if err != nil || !has {
			t.Fatalf("get(tenant %q, forged-record job %q) = has %v err %v, want the row under the CONTEXT tenant", isoTenantA, jobID, has, err)
		}
		if row.GetTenantID() != pkgcore.TenantID(isoTenantA) {
			t.Errorf("forged record landed under tenant %q, want the context tenant %q", row.GetTenantID(), isoTenantA)
		}
		if _, has, err := store.get(ctxB, jobID); err != nil || has {
			t.Errorf("get(forged tenant %q, forged-record job %q) = has %v err %v, want none -- the forged tenant must never see the row", isoTenantB, jobID, has, err)
		}
	})

	t.Run("no_tenant_in_context_fails_closed", func(t *testing.T) {
		t.Helper()
		noTenant := context.Background()

		if err := store.save(noTenant, isoJobID(isoTenantA, 100), photoID, SimulationOptions{}); !errors.Is(err, pkgcore.ErrNoTenant) {
			t.Errorf("save(no tenant in context) error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
		}
		if _, _, err := store.get(noTenant, aJobs[0]); !errors.Is(err, pkgcore.ErrNoTenant) {
			t.Errorf("get(no tenant in context) error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
		}
		if _, err := store.listByPhoto(noTenant, photoID); !errors.Is(err, pkgcore.ErrNoTenant) {
			t.Errorf("listByPhoto(no tenant in context) error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
		}
	})
}

// assertListScopedTo asserts that store.listByPhoto(ctx, photoID), called
// for owner, returns every job id in ownJobs and none of otherJobs --
// containment rather than exact-length matching, so the assertion never
// assumes the table was empty before the suite ran.
func assertListScopedTo(t *testing.T, store *SimulationStore, ctx context.Context, owner, otherTenant, photoID string, ownJobs, otherJobs []jobs.JobID) {
	t.Helper()
	rows, err := store.listByPhoto(ctx, photoID)
	if err != nil {
		t.Fatalf("listByPhoto(tenant %q) error = %v", owner, err)
	}
	present := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row.GetTenantID() != pkgcore.TenantID(owner) {
			t.Errorf("listByPhoto(tenant %q) returned row %q owned by tenant %q",
				owner, row.JobID, row.GetTenantID())
		}
		present[row.JobID] = true
	}
	for _, jobID := range ownJobs {
		if !present[string(jobID)] {
			t.Errorf("listByPhoto(tenant %q) is missing job %q, saved under that same tenant", owner, jobID)
		}
	}
	for _, jobID := range otherJobs {
		if present[string(jobID)] {
			t.Errorf("listByPhoto(tenant %q) unexpectedly contains job %q, which belongs to tenant %q", owner, jobID, otherTenant)
		}
	}
}
