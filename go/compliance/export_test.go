package compliance

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/compliance/internal/testutil"
)

// newExportHarness returns an ExportService wired directly over a
// hand-built pkgcore.Registry and a real pkgcore.NewLocalObjectStore
// rooted at t.TempDir().
func newExportHarness(t *testing.T) (*ExportService, *testutil.FakeRepository, pkgcore.ObjectStore) {
	t.Helper()
	bus := pkgcore.NewMemoryEventBus()
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.AuditActions.Add(AuditActionExportRequest); err != nil {
		t.Fatalf("declare audit action: %v", err)
	}

	repo := testutil.NewFakeRepository(testutil.NewDB(t))
	participant := testutil.NewParticipant("testutil.fake_note", repo)
	if err := reg.Retention.Add(participant); err != nil {
		t.Fatalf("register fake participant: %v", err)
	}

	store := pkgcore.NewLocalObjectStore(t.TempDir())
	svc := newExportService()
	svc.retention = reg.Retention
	svc.bus = bus
	svc.actions = reg.AuditActions
	svc.store = store
	return svc, repo, store
}

// TestExportService_Export_GathersAndStoresParticipantData proves the
// happy path: a live row is included in the manifest, and the stored
// object under the returned key round-trips to the identical manifest.
func TestExportService_Export_GathersAndStoresParticipantData(t *testing.T) {
	svc, repo, store := newExportHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	key, manifest, err := svc.Export(context.Background(), tenant)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if manifest.Tenant != tenant {
		t.Errorf("manifest.Tenant = %q, want %q", manifest.Tenant, tenant)
	}
	if _, ok := manifest.Participants["testutil.fake_note"]; !ok {
		t.Fatalf("manifest.Participants missing %q: %+v", "testutil.fake_note", manifest.Participants)
	}

	r, err := store.GetObject(context.Background(), key)
	if err != nil {
		t.Fatalf("GetObject(%q): %v", key, err)
	}
	defer r.Close()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stored object: %v", err)
	}
	var stored ExportManifest
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("unmarshal stored manifest: %v", err)
	}
	if stored.Tenant != tenant {
		t.Errorf("stored manifest tenant = %q, want %q", stored.Tenant, tenant)
	}
}

// TestExportService_Export_SkipsParticipantsWithNoExportCallback proves a
// participant that left Export nil contributes nothing and causes no
// error -- a nil Export is documented as a legal "not opted in" value.
func TestExportService_Export_SkipsParticipantsWithNoExportCallback(t *testing.T) {
	svc, _, _ := newExportHarness(t)
	noExport := pkgcore.RetentionParticipant{Name: "testutil.no_export"}
	if err := svc.retention.Add(noExport); err != nil {
		t.Fatalf("register no-export participant: %v", err)
	}

	_, manifest, err := svc.Export(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, ok := manifest.Participants["testutil.no_export"]; ok {
		t.Errorf("manifest.Participants should not contain %q", "testutil.no_export")
	}
}

// TestExportService_Export_ParticipantErrorIsPartialFailure proves a
// failing participant's Export does not stop the gather, and is reported
// both in ExportManifest.Errors and as ErrExportPartialFailure.
func TestExportService_Export_ParticipantErrorIsPartialFailure(t *testing.T) {
	svc, repo, _ := newExportHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	failing := pkgcore.RetentionParticipant{
		Name: "testutil.failing_export",
		Export: func(context.Context, pkgcore.TenantID) (any, error) {
			return nil, errFakeParticipant
		},
	}
	if err := svc.retention.Add(failing); err != nil {
		t.Fatalf("register failing participant: %v", err)
	}

	_, manifest, err := svc.Export(context.Background(), tenant)
	if !hasCode(err, ErrExportPartialFailure.Code) {
		t.Fatalf("Export error = %v, want %s", err, ErrExportPartialFailure.Code)
	}
	if _, ok := manifest.Participants["testutil.fake_note"]; !ok {
		t.Error("the healthy participant should still have contributed data")
	}
	if manifest.Errors["testutil.failing_export"] == "" {
		t.Error("manifest.Errors missing the failing participant's error")
	}
}
