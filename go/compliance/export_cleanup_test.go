package compliance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
)

// newManifestCleanupHarness returns a RetentionService wired directly over
// a hand-built pkgcore.Registry carrying ONLY the module's own
// export-manifests cleanup participant, plus the real *audit.Repository
// (over a freshly migrated audit_events table) and the real
// pkgcore.LocalObjectStore the participant reads and reaps through -- the
// same seams Module.Register wires it with. SweepTenant is driven through
// its public surface so the participant runs under the sweep's real
// system-context and audit machinery.
func newManifestCleanupHarness(t *testing.T) (*RetentionService, *audit.Repository, pkgcore.ObjectStore) {
	t.Helper()
	bus := pkgcore.NewMemoryEventBus()
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.AuditActions.Add(AuditActionRetentionSweep); err != nil {
		t.Fatalf("declare audit action: %v", err)
	}
	pkgcore.RegisterSystemPurpose(SystemPurposeRetentionSweep)

	auditRepo := audit.NewRepository(newTestAuditDB(t))
	store := pkgcore.NewLocalObjectStore(t.TempDir())
	if err := reg.Retention.Add(exportManifestsParticipant(auditRepo, store)); err != nil {
		t.Fatalf("register export-manifests participant: %v", err)
	}

	svc := newRetentionService()
	svc.retention = reg.Retention
	svc.bus = bus
	svc.actions = reg.AuditActions
	return svc, auditRepo, store
}

// insertExportAuditRow inserts one AuditActionExportRequest audit row as a
// completed Export would leave it: Success true, Changes a Diff-shaped
// document whose After carries object_key and share_expires_at -- exactly
// the shape emitExportAudit writes and changesJSON stores.
func insertExportAuditRow(t *testing.T, repo *audit.Repository, tenant pkgcore.TenantID, id, key string, shareExpiresAt time.Time) {
	t.Helper()
	changes, err := json.Marshal(audit.Diff{After: map[string]any{
		"object_key":       key,
		"share_id":         "share-" + id,
		"share_expires_at": shareExpiresAt,
		"participants":     []string{},
	}})
	if err != nil {
		t.Fatalf("marshal audit changes: %v", err)
	}
	evt := &audit.AuditEvent{
		ID:         "evt-" + id,
		TenantID:   string(tenant),
		Action:     AuditActionExportRequest,
		OccurredAt: time.Now().Add(-time.Hour),
		Changes:    datatypes.JSON(changes),
	}
	evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeSystem, ID: "compliance.export", DisplayName: "compliance.export"})
	evt.SetResource(audit.Resource{Type: "compliance.tenant", ID: string(tenant), DisplayName: string(tenant)})
	evt.SetResult(audit.Result{Success: true})
	if err := repo.Insert(context.Background(), evt); err != nil {
		t.Fatalf("insert export audit event: %v", err)
	}
}

// seedExportDelivery puts one manifest object under tenant's own
// exportObjectKey namespace and records the audit row a completed Export
// would leave for it (see insertExportAuditRow). Returns the object key.
func seedExportDelivery(t *testing.T, repo *audit.Repository, store pkgcore.ObjectStore, tenant pkgcore.TenantID, id string, shareExpiresAt time.Time) string {
	t.Helper()
	key := exportObjectKey(tenant, id)
	payload := []byte(`{"tenant":"` + string(tenant) + `","id":"` + id + `"}`)
	if err := store.PutObject(context.Background(), key, bytes.NewReader(payload)); err != nil {
		t.Fatalf("store manifest object %q: %v", key, err)
	}
	insertExportAuditRow(t, repo, tenant, id, key, shareExpiresAt)
	return key
}

// manifestObjectExists reports whether the store still holds key.
func manifestObjectExists(t *testing.T, store pkgcore.ObjectStore, key string) bool {
	t.Helper()
	r, err := store.GetObject(context.Background(), key)
	if errors.Is(err, pkgcore.ErrObjectNotFound) {
		return false
	}
	if err != nil {
		t.Fatalf("GetObject(%q): %v", key, err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close GetObject(%q) reader: %v", key, err)
	}
	return true
}

// TestExportManifestCleanup_SweepReapsOnlyExpiredDeliveries is finding
// P1-6's cleanup-half regression: a stored export manifest whose delivery
// share has expired past the tenant's retention-window cutoff is reaped by
// the module's own retention sweep (the export-manifests participant),
// while a manifest whose share is still live, an expired manifest of
// another tenant, an object a tampered event names outside the swept
// tenant's own compliance/exports/ prefix, and an expired event whose
// object an earlier pass already reaped all survive untouched -- and a
// second sweep over the same rows converges to 0, the documented retry
// contract. The behavior this test pins against had no cleanup mechanism
// at all: every successfully delivered manifest stayed in the object store
// forever, accumulating unbounded.
func TestExportManifestCleanup_SweepReapsOnlyExpiredDeliveries(t *testing.T) {
	svc, auditRepo, store := newManifestCleanupHarness(t)

	expiredKey := seedExportDelivery(t, auditRepo, store, "tenant-a", "expired", time.Now().Add(-40*24*time.Hour))
	liveKey := seedExportDelivery(t, auditRepo, store, "tenant-a", "live", time.Now().Add(defaultExportDeliveryExpiry))
	otherTenantKey := seedExportDelivery(t, auditRepo, store, "tenant-b", "expired-b", time.Now().Add(-40*24*time.Hour))

	// An expired event whose object is already gone (reaped by an earlier
	// pass, or cleaned up at Export time) must not be counted on a re-run.
	ghostKey := seedExportDelivery(t, auditRepo, store, "tenant-a", "ghost", time.Now().Add(-40*24*time.Hour))
	if err := store.DeleteObject(context.Background(), ghostKey); err != nil {
		t.Fatalf("delete ghost object: %v", err)
	}

	// An audit row naming an object OUTSIDE the swept tenant's own
	// compliance/exports/ prefix (a malformed or hostile event in its own
	// trail) must never reach that object: the sweep's prefix guard
	// confines it to the tenant's own namespace.
	foreignKey := "storage/objects/tenant-a/foreign.json"
	if err := store.PutObject(context.Background(), foreignKey, bytes.NewReader([]byte(`{"foreign":true}`))); err != nil {
		t.Fatalf("store foreign object: %v", err)
	}
	insertExportAuditRow(t, auditRepo, "tenant-a", "foreign", foreignKey, time.Now().Add(-40*24*time.Hour))

	result, err := svc.SweepTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("SweepTenant: %v", err)
	}
	if result.HasErrors() {
		t.Fatalf("SweepTenant errors = %v, want none", result.Errors)
	}
	if got := result.Reaped[exportManifestsParticipantName]; got != 1 {
		t.Fatalf("SweepTenant reaped %s = %d, want exactly 1 (only tenant-a's expired live-share manifest)", exportManifestsParticipantName, got)
	}

	if manifestObjectExists(t, store, expiredKey) {
		t.Error("tenant-a's expired-delivery manifest should have been reaped")
	}
	if !manifestObjectExists(t, store, liveKey) {
		t.Error("tenant-a's still-live manifest must survive the sweep")
	}
	if !manifestObjectExists(t, store, otherTenantKey) {
		t.Error("tenant-b's manifest must survive a sweep of tenant-a")
	}
	if !manifestObjectExists(t, store, foreignKey) {
		t.Error("an object outside the swept tenant's compliance/exports/ prefix must never be touched")
	}

	// A re-run over the same rows converges: the reaped event still sits
	// in the append-only trail, but its object is gone, so the second pass
	// reports 0 -- the participant's documented retry semantics.
	again, err := svc.SweepTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("second SweepTenant: %v", err)
	}
	if got := again.Reaped[exportManifestsParticipantName]; got != 0 {
		t.Fatalf("second SweepTenant reaped %s = %d, want 0 -- already-reaped manifests must not be recounted", exportManifestsParticipantName, got)
	}
}
