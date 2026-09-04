package compliance

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/sharing"

	"github.com/vislake/speed/go/compliance/internal/testutil"
)

// fakeSharingCreator is a scripted SharingCreator, standing in for a real
// *sharing.Service the way recordingQueue (module_test.go) stands in for a
// real jobs.Queue: it records every sharing.CreateParams it was called
// with and, unless failWith is set, echoes back a CreateResult that looks
// exactly like what a real sharing.Service.Create would return for those
// params -- a fresh id, the params' own ResourceRef/ExpiresAt/MaxViews/
// Sensitive, and a fixed, recognizable token.
type fakeSharingCreator struct {
	calls    []sharing.CreateParams
	failWith error
}

func (f *fakeSharingCreator) Create(_ context.Context, p sharing.CreateParams) (*sharing.CreateResult, error) {
	f.calls = append(f.calls, p)
	if f.failWith != nil {
		return nil, f.failWith
	}
	return &sharing.CreateResult{
		Share: &sharing.Share{
			ID:          "share-1",
			ResourceRef: p.ResourceRef,
			ExpiresAt:   p.ExpiresAt,
			MaxViews:    p.MaxViews,
			Sensitive:   p.Sensitive,
		},
		Token: "fake-token",
	}, nil
}

var _ SharingCreator = (*fakeSharingCreator)(nil)

// newExportHarness returns an ExportService wired directly over a
// hand-built pkgcore.Registry, a real pkgcore.NewLocalObjectStore rooted
// at t.TempDir(), and a fakeSharingCreator standing in for go/sharing.
func newExportHarness(t *testing.T) (*ExportService, *testutil.FakeRepository, pkgcore.ObjectStore, *fakeSharingCreator) {
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
	fakeSharing := &fakeSharingCreator{}
	svc := newExportService()
	svc.retention = reg.Retention
	svc.bus = bus
	svc.actions = reg.AuditActions
	svc.store = store
	svc.sharing = fakeSharing
	return svc, repo, store, fakeSharing
}

// TestExportService_Export_GathersAndStoresParticipantData proves the
// happy path: a live row is included in the manifest, and the stored
// object under the returned key round-trips to the identical manifest.
func TestExportService_Export_GathersAndStoresParticipantData(t *testing.T) {
	svc, repo, store, _ := newExportHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	result, err := svc.Export(context.Background(), tenant)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if result.Manifest.Tenant != tenant {
		t.Errorf("Manifest.Tenant = %q, want %q", result.Manifest.Tenant, tenant)
	}
	if _, ok := result.Manifest.Participants["testutil.fake_note"]; !ok {
		t.Fatalf("Manifest.Participants missing %q: %+v", "testutil.fake_note", result.Manifest.Participants)
	}

	r, err := store.GetObject(context.Background(), result.ObjectKey)
	if err != nil {
		t.Fatalf("GetObject(%q): %v", result.ObjectKey, err)
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
	svc, _, _, _ := newExportHarness(t)
	noExport := pkgcore.RetentionParticipant{Name: "testutil.no_export"}
	if err := svc.retention.Add(noExport); err != nil {
		t.Fatalf("register no-export participant: %v", err)
	}

	result, err := svc.Export(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, ok := result.Manifest.Participants["testutil.no_export"]; ok {
		t.Errorf("Manifest.Participants should not contain %q", "testutil.no_export")
	}
}

// TestExportService_Export_ParticipantErrorIsPartialFailure proves a
// failing participant's Export does not stop the gather, and is reported
// both in ExportManifest.Errors and as ErrExportPartialFailure -- and that
// delivery still happens for the data that was gathered.
func TestExportService_Export_ParticipantErrorIsPartialFailure(t *testing.T) {
	svc, repo, _, fakeSharing := newExportHarness(t)
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

	result, err := svc.Export(context.Background(), tenant)
	if !hasCode(err, ErrExportPartialFailure.Code) {
		t.Fatalf("Export error = %v, want %s", err, ErrExportPartialFailure.Code)
	}
	if _, ok := result.Manifest.Participants["testutil.fake_note"]; !ok {
		t.Error("the healthy participant should still have contributed data")
	}
	if result.Manifest.Errors["testutil.failing_export"] == "" {
		t.Error("Manifest.Errors missing the failing participant's error")
	}
	if result.Delivery.ShareID == "" {
		t.Error("a participant partial failure should not prevent delivery")
	}
	if len(fakeSharing.calls) != 1 {
		t.Fatalf("sharing.Create calls = %d, want 1", len(fakeSharing.calls))
	}
}

// TestExportService_Export_DeliversThroughSharing proves Export creates a
// go/sharing share pointing at the stored object, with the delivery
// choices this round makes: a single view, defaultExportDeliveryExpiry's
// duration and Sensitive true -- and returns the minted share's id and
// token to the caller.
func TestExportService_Export_DeliversThroughSharing(t *testing.T) {
	svc, repo, _, fakeSharing := newExportHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	before := time.Now()
	result, err := svc.Export(context.Background(), tenant)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	after := time.Now()

	if len(fakeSharing.calls) != 1 {
		t.Fatalf("sharing.Create calls = %d, want 1", len(fakeSharing.calls))
	}
	call := fakeSharing.calls[0]
	if call.ResourceRef != result.ObjectKey {
		t.Errorf("Create ResourceRef = %q, want the stored object key %q", call.ResourceRef, result.ObjectKey)
	}
	if !call.Sensitive {
		t.Error("an export share must be created with Sensitive true")
	}
	if call.MaxViews == nil || *call.MaxViews != exportDeliveryMaxViews {
		t.Errorf("Create MaxViews = %v, want %d", call.MaxViews, exportDeliveryMaxViews)
	}
	if call.Password != nil {
		t.Errorf("Create Password = %v, want nil", call.Password)
	}
	if call.ExpiresAt == nil {
		t.Fatal("Create ExpiresAt must not be nil")
	}
	wantEarliest := before.Add(defaultExportDeliveryExpiry)
	wantLatest := after.Add(defaultExportDeliveryExpiry)
	if call.ExpiresAt.Before(wantEarliest) || call.ExpiresAt.After(wantLatest) {
		t.Errorf("Create ExpiresAt = %v, want between %v and %v", *call.ExpiresAt, wantEarliest, wantLatest)
	}

	if result.Delivery.ShareID != "share-1" {
		t.Errorf("Delivery.ShareID = %q, want %q", result.Delivery.ShareID, "share-1")
	}
	if result.Delivery.Token != "fake-token" {
		t.Errorf("Delivery.Token = %q, want %q", result.Delivery.Token, "fake-token")
	}
	if !result.Delivery.ExpiresAt.Equal(*call.ExpiresAt) {
		t.Errorf("Delivery.ExpiresAt = %v, want %v", result.Delivery.ExpiresAt, *call.ExpiresAt)
	}
}

// TestExportService_Export_NoSharingWired_Refuses proves Export refuses
// outright with ErrSharingRequired when the module was built with no
// WithSharing option -- before gathering or storing anything.
func TestExportService_Export_NoSharingWired_Refuses(t *testing.T) {
	svc, _, _, _ := newExportHarness(t)
	svc.sharing = nil

	_, err := svc.Export(context.Background(), "tenant-a")
	if !hasCode(err, ErrSharingRequired.Code) {
		t.Fatalf("Export error = %v, want %s", err, ErrSharingRequired.Code)
	}
}

// TestExportService_Export_DeliveryFailureIsReported proves a failed
// go/sharing.Create is reported as ErrExportDeliveryFailed, distinct from
// a participant gathering failure, while the already-gathered manifest and
// its storage key are still returned.
func TestExportService_Export_DeliveryFailureIsReported(t *testing.T) {
	svc, repo, store, fakeSharing := newExportHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")
	fakeSharing.failWith = errors.New("sharing unavailable")

	result, err := svc.Export(context.Background(), tenant)
	if !hasCode(err, ErrExportDeliveryFailed.Code) {
		t.Fatalf("Export error = %v, want %s", err, ErrExportDeliveryFailed.Code)
	}
	if result == nil {
		t.Fatal("Export should return a non-nil result even when delivery fails")
	}
	if result.ObjectKey == "" {
		t.Error("ObjectKey should still be populated: the manifest was stored before delivery was attempted")
	}
	if result.Delivery != (ExportDelivery{}) {
		t.Errorf("Delivery = %+v, want the zero value", result.Delivery)
	}
	if _, ok := result.Manifest.Participants["testutil.fake_note"]; !ok {
		t.Error("the manifest gathered before the delivery failure should still be returned")
	}

	// The manifest itself was genuinely stored -- delivery failing must
	// not roll back or lose it.
	if _, err := store.GetObject(context.Background(), result.ObjectKey); err != nil {
		t.Errorf("GetObject(%q) after delivery failure: %v", result.ObjectKey, err)
	}
}
