package compliance

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/sharing"
	sharingmigrations "github.com/vislake/speed/go/sharing/migrations"

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
	svc, repo, store, fakeSharing, _ := newExportHarnessWith(t)
	return svc, repo, store, fakeSharing
}

// newExportHarnessWith is newExportHarness plus two things the export
// audit-content tests need: the caller's own extra participants registered
// alongside the shared testutil.FakeNote one, and a subscriber capturing
// every audit.RecordedEvent published on the bus (dbkit/audit.Emit's own
// EventRecorded), so a test can assert an export's audit event without a
// real database-backed audit.Repository -- the same captured-events shape
// erasure_test.go's newErasureServiceWith provides for the erasure path.
func newExportHarnessWith(t *testing.T, extra ...pkgcore.RetentionParticipant) (*ExportService, *testutil.FakeRepository, pkgcore.ObjectStore, *fakeSharingCreator, *[]audit.RecordedEvent) {
	t.Helper()
	return newExportHarnessSeamed(t, nil, nil, extra...)
}

// newExportHarnessSeamed is newExportHarnessWith over injected seams: a
// nil bus or store means the ordinary defaults (a fresh memory bus, a
// local store at a fresh temp dir); an injected scripted bus or store lets
// a test drive the failure branches a healthy memory bus and local store
// can never reach -- the audit publish failing, the store refusing to
// store or delete the manifest object.
func newExportHarnessSeamed(t *testing.T, bus pkgcore.EventBus, store pkgcore.ObjectStore, extra ...pkgcore.RetentionParticipant) (*ExportService, *testutil.FakeRepository, pkgcore.ObjectStore, *fakeSharingCreator, *[]audit.RecordedEvent) {
	t.Helper()
	if bus == nil {
		bus = pkgcore.NewMemoryEventBus()
	}
	if store == nil {
		store = pkgcore.NewLocalObjectStore(t.TempDir())
	}
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.AuditActions.Add(AuditActionExportRequest); err != nil {
		t.Fatalf("declare audit action: %v", err)
	}

	captured := &[]audit.RecordedEvent{}
	bus.Subscribe(audit.EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		if rec, ok := evt.Payload.(audit.RecordedEvent); ok {
			*captured = append(*captured, rec)
		}
		return nil
	})

	repo := testutil.NewFakeRepository(testutil.NewDB(t))
	participant := testutil.NewParticipant("testutil.fake_note", repo)
	if err := reg.Retention.Add(participant); err != nil {
		t.Fatalf("register fake participant: %v", err)
	}
	if err := reg.Retention.Add(extra...); err != nil {
		t.Fatalf("register extra participants: %v", err)
	}

	fakeSharing := &fakeSharingCreator{}
	svc := newExportService()
	svc.retention = reg.Retention
	svc.bus = bus
	svc.actions = reg.AuditActions
	svc.store = store
	svc.sharing = fakeSharing
	return svc, repo, store, fakeSharing, captured
}

// TestExportService_Export_GathersAndStoresParticipantData proves the
// happy path: a live row is included in the manifest, and the stored
// object under the returned key round-trips to the identical manifest.
func TestExportService_Export_GathersAndStoresParticipantData(t *testing.T) {
	svc, repo, store, _ := newExportHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	result, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
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
	noExport := pkgcore.RetentionParticipant{
		// NoopSweep and NoopErase satisfy the registrar's mandatory-Sweep
		// and mandatory-Erase rules; the export service under test never
		// invokes either. What this double is really proving is the
		// nil-Export skip.
		Name:  "testutil.no_export",
		Sweep: testutil.NoopSweep,
		Erase: testutil.NoopErase,
	}
	if err := svc.retention.Add(noExport); err != nil {
		t.Fatalf("register no-export participant: %v", err)
	}

	result, err := svc.Export(pkgcore.WithTenant(context.Background(), "tenant-a"), "tenant-a")
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
		// NoopSweep and NoopErase satisfy the registrar's mandatory-Sweep
		// and mandatory-Erase rules; the export service under test never
		// invokes either.
		Name:  "testutil.failing_export",
		Sweep: testutil.NoopSweep,
		Erase: testutil.NoopErase,
		Export: func(context.Context, pkgcore.TenantID) (any, error) {
			return nil, errFakeParticipant
		},
	}
	if err := svc.retention.Add(failing); err != nil {
		t.Fatalf("register failing participant: %v", err)
	}

	result, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
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
// choices of the module: a single view, defaultExportDeliveryExpiry's
// duration and Sensitive true -- and returns the minted share's id and
// token to the caller.
func TestExportService_Export_DeliversThroughSharing(t *testing.T) {
	svc, repo, _, fakeSharing := newExportHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	before := time.Now()
	result, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
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

// fakeExportDeliveryExpiryReader is a minimal, in-test
// ExportDeliveryExpiryReader double, mirroring go/sharing's own
// fakeTenantConfigReader exactly.
type fakeExportDeliveryExpiryReader struct {
	d   time.Duration
	ok  bool
	err error
}

func (f fakeExportDeliveryExpiryReader) ExportDeliveryExpiry(context.Context, pkgcore.TenantID) (time.Duration, bool, error) {
	return f.d, f.ok, f.err
}

// TestExportService_Export_UsesTenantConfiguredExpiry proves a wired
// ExportDeliveryExpiryReader's answer is honored over
// defaultExportDeliveryExpiry when the tenant has configured one.
func TestExportService_Export_UsesTenantConfiguredExpiry(t *testing.T) {
	svc, repo, _, fakeSharing := newExportHarness(t)
	svc.cfg = fakeExportDeliveryExpiryReader{d: 2 * time.Hour, ok: true}
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	before := time.Now()
	result, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	after := time.Now()

	call := fakeSharing.calls[0]
	if call.ExpiresAt == nil {
		t.Fatal("Create ExpiresAt must not be nil")
	}
	wantEarliest := before.Add(2 * time.Hour)
	wantLatest := after.Add(2 * time.Hour)
	if call.ExpiresAt.Before(wantEarliest) || call.ExpiresAt.After(wantLatest) {
		t.Errorf("Create ExpiresAt = %v, want between %v and %v (the tenant-configured 2 hours)", *call.ExpiresAt, wantEarliest, wantLatest)
	}
	if !result.Delivery.ExpiresAt.Equal(*call.ExpiresAt) {
		t.Errorf("Delivery.ExpiresAt = %v, want %v", result.Delivery.ExpiresAt, *call.ExpiresAt)
	}
}

// TestExportService_Export_ConfigReaderReportingUnconfigured_FallsBackToDefault
// proves ok == false (the tenant configured nothing) behaves exactly as if
// no ExportDeliveryExpiryReader were wired at all: Export still uses
// defaultExportDeliveryExpiry.
func TestExportService_Export_ConfigReaderReportingUnconfigured_FallsBackToDefault(t *testing.T) {
	svc, repo, _, fakeSharing := newExportHarness(t)
	svc.cfg = fakeExportDeliveryExpiryReader{ok: false}
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	before := time.Now()
	if _, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant); err != nil {
		t.Fatalf("Export: %v", err)
	}
	after := time.Now()

	call := fakeSharing.calls[0]
	if call.ExpiresAt == nil {
		t.Fatal("Create ExpiresAt must not be nil")
	}
	wantEarliest := before.Add(defaultExportDeliveryExpiry)
	wantLatest := after.Add(defaultExportDeliveryExpiry)
	if call.ExpiresAt.Before(wantEarliest) || call.ExpiresAt.After(wantLatest) {
		t.Errorf("Create ExpiresAt = %v, want between %v and %v (defaultExportDeliveryExpiry)", *call.ExpiresAt, wantEarliest, wantLatest)
	}
}

// TestExportService_Export_ConfigReaderError_ReportsDeliveryFailed proves a
// genuine ExportDeliveryExpiryReader read failure is reported as
// ErrExportDeliveryFailed -- the identical bucket a failed go/sharing.Create
// itself reports through, since neither leaves a usable delivery behind.
func TestExportService_Export_ConfigReaderError_ReportsDeliveryFailed(t *testing.T) {
	svc, repo, _, _ := newExportHarness(t)
	svc.cfg = fakeExportDeliveryExpiryReader{err: errors.New("config read failed")}
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	_, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
	if !hasCode(err, ErrExportDeliveryFailed.Code) {
		t.Fatalf("Export error = %v, want %s", err, ErrExportDeliveryFailed.Code)
	}
}

// TestExportService_Export_ConfigReaderReportingNonPositive_FallsBackToDefault
// proves a wired ExportDeliveryExpiryReader answering a non-positive
// duration with ok == true can never mint an already-expired (or already
// long-past) delivery link: a zero or negative "configured" value is
// nonsense as a link lifetime -- it would hand the subject a share that is
// dead the instant it is created -- so exportDeliveryExpiry clamps it to
// defaultExportDeliveryExpiry, the same <= 0 guard
// RetentionService.RetentionWindow already applies to a configured
// retention window. The minted share must land within the
// defaultExportDeliveryExpiry window of the call, never at (or before) the
// instant of the call itself.
func TestExportService_Export_ConfigReaderReportingNonPositive_FallsBackToDefault(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Hour} {
		t.Run(d.String(), func(t *testing.T) {
			svc, repo, _, fakeSharing := newExportHarness(t)
			svc.cfg = fakeExportDeliveryExpiryReader{d: d, ok: true}
			tenant := pkgcore.TenantID("tenant-a")
			seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

			before := time.Now()
			if _, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant); err != nil {
				t.Fatalf("Export: %v", err)
			}
			after := time.Now()

			call := fakeSharing.calls[0]
			if call.ExpiresAt == nil {
				t.Fatal("Create ExpiresAt must not be nil")
			}
			if !call.ExpiresAt.After(time.Now()) {
				t.Errorf("Create ExpiresAt = %v, which is already in the past: a (d=%v, ok=true) reader answer must not mint an expired link", *call.ExpiresAt, d)
			}
			wantEarliest := before.Add(defaultExportDeliveryExpiry)
			wantLatest := after.Add(defaultExportDeliveryExpiry)
			if call.ExpiresAt.Before(wantEarliest) || call.ExpiresAt.After(wantLatest) {
				t.Errorf("Create ExpiresAt = %v, want between %v and %v (defaultExportDeliveryExpiry, not the reader's non-positive %v)", *call.ExpiresAt, wantEarliest, wantLatest, d)
			}
		})
	}
}

// TestExportService_Export_ConfigReaderBeyondSharingCeiling_Clamped proves
// the upper clamp of exportDeliveryExpiry: a wired
// ExportDeliveryExpiryReader answering a duration LONGER than go/sharing's
// explicit-expiry ceiling (sharing.MaxExplicitShareLifetime) must not be
// handed to sharing.Create as an explicit ExpiresAt -- sharing refuses an
// explicit expiry beyond its ceiling, so honoring the answer as given
// would let one host configuration break every export. The window is
// clamped DOWN to the sharing ceiling (the closest mintable duration to
// what the operator configured -- the mirror image of the `<= 0`
// fallback's "nonsense resolves to the honest default"), never minted at
// the configured length and never silently dropped to
// defaultExportDeliveryExpiry.
func TestExportService_Export_ConfigReaderBeyondSharingCeiling_Clamped(t *testing.T) {
	svc, repo, _, fakeSharing := newExportHarness(t)
	svc.cfg = fakeExportDeliveryExpiryReader{d: sharing.MaxExplicitShareLifetime + 30*24*time.Hour, ok: true}
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	before := time.Now()
	if _, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant); err != nil {
		t.Fatalf("Export: %v", err)
	}
	after := time.Now()

	call := fakeSharing.calls[0]
	if call.ExpiresAt == nil {
		t.Fatal("Create ExpiresAt must not be nil")
	}
	wantEarliest := before.Add(sharing.MaxExplicitShareLifetime)
	wantLatest := after.Add(sharing.MaxExplicitShareLifetime)
	if call.ExpiresAt.Before(wantEarliest) || call.ExpiresAt.After(wantLatest) {
		t.Errorf("Create ExpiresAt = %v, want between %v and %v -- a reader answer beyond the sharing ceiling must be clamped to the ceiling, never minted at its own length", *call.ExpiresAt, wantEarliest, wantLatest)
	}
}

// TestExportService_Export_DeliveryWindowBeyondSharingCeiling_RealSharingStillSucceeds
// is the end-to-end counterpart of the clamp test above, wired to a REAL
// *sharing.Service (newRealSharingService) instead of the scripted fake:
// a delivery window beyond sharing's explicit-expiry ceiling must not
// break every export. Before the clamp, exportDeliveryExpiry returned the
// configured 60 days unchanged, deliverExport minted an explicit ExpiresAt
// 60 days out, and the real sharing.Service refused it with
// sharing.expiry_out_of_range -- so every export for a tenant configured
// with a long delivery window failed with ErrExportDeliveryFailed. With
// the clamp the minted window is the sharing ceiling, which sharing
// accepts for every tenant, and the export completes end to end.
func TestExportService_Export_DeliveryWindowBeyondSharingCeiling_RealSharingStillSucceeds(t *testing.T) {
	svc, repo, _, _ := newExportHarness(t)
	svc.cfg = fakeExportDeliveryExpiryReader{d: 60 * 24 * time.Hour, ok: true}
	realSharing := newRealSharingService(t)
	svc.sharing = realSharing
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	result, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
	if err != nil {
		t.Fatalf("Export: %v -- a delivery window beyond the sharing ceiling must be clamped, not break the export", err)
	}
	if result.Delivery.ShareID == "" || result.Delivery.Token == "" {
		t.Fatalf("Export result.Delivery = %+v, want a real minted share id and token", result.Delivery)
	}
	if result.Delivery.ExpiresAt.After(time.Now().Add(sharing.MaxExplicitShareLifetime)) {
		t.Errorf("minted delivery ExpiresAt = %v, want within the sharing ceiling of now -- the clamp must have bounded the window", result.Delivery.ExpiresAt)
	}

	// The minted share is genuinely live through the real service.
	share, err := realSharing.Access(pkgcore.WithTenant(context.Background(), tenant), result.Delivery.Token, sharing.AccessParams{})
	if err != nil {
		t.Fatalf("real sharing.Service.Access(minted token): %v", err)
	}
	if share.ID != result.Delivery.ShareID {
		t.Errorf("Access share ID = %q, want %q", share.ID, result.Delivery.ShareID)
	}
}

// TestExportService_Export_NoTenantContext_Refused proves Export refuses a
// ctx that carries no tenant. The ctx tenant is the single data boundary an
// export may ever read through -- every participant's Export callback reads
// repo.List(ctx) -- so a bare or background ctx must never become a license
// to pick any tenant via the tenant argument. An unconditional re-scope to
// the tenant argument would let this call export tenant-a's rows from a
// background context.
func TestExportService_Export_NoTenantContext_Refused(t *testing.T) {
	svc, _, _, fakeSharing := newExportHarness(t)

	_, err := svc.Export(context.Background(), "tenant-a")
	if !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Fatalf("Export error = %v, want pkgcore.ErrNoTenant", err)
	}
	if len(fakeSharing.calls) != 0 {
		t.Errorf("sharing.Create calls = %d, want 0: a refused export must deliver nothing", len(fakeSharing.calls))
	}
}

// TestExportService_Export_TenantMismatch_Refused proves Export refuses when
// the tenant ctx carries differs from the tenant argument. Export is
// documented to read the SAME tenant the caller's own ctx is scoped to, so
// the argument may only echo the ctx tenant back, never name a wider one --
// an unconditional re-scope would let this call export tenant-a's rows
// while the ctx says tenant-b.
func TestExportService_Export_TenantMismatch_Refused(t *testing.T) {
	svc, _, _, fakeSharing := newExportHarness(t)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-b")
	_, err := svc.Export(ctx, "tenant-a")
	if !hasCode(err, ErrExportTenantMismatch.Code) {
		t.Fatalf("Export error = %v, want %s", err, ErrExportTenantMismatch.Code)
	}
	if len(fakeSharing.calls) != 0 {
		t.Errorf("sharing.Create calls = %d, want 0: a refused export must deliver nothing", len(fakeSharing.calls))
	}
}

// TestExportService_Export_NoSharingWired_Refuses proves Export refuses
// outright with ErrSharingRequired when the module was built with no
// WithSharing option -- before gathering or storing anything.
func TestExportService_Export_NoSharingWired_Refuses(t *testing.T) {
	svc, _, _, _ := newExportHarness(t)
	svc.sharing = nil

	_, err := svc.Export(pkgcore.WithTenant(context.Background(), "tenant-a"), "tenant-a")
	if !hasCode(err, ErrSharingRequired.Code) {
		t.Fatalf("Export error = %v, want %s", err, ErrSharingRequired.Code)
	}
}

// TestExportService_Export_DeliveryFailureIsReported proves a failed
// go/sharing.Create is reported as ErrExportDeliveryFailed, distinct from
// a participant gathering failure, while the already-gathered manifest and
// its storage key are still returned -- and that the stored object itself
// is deleted before Export returns: a manifest no share can ever
// reference is an un-shareable copy of the tenant's complete data, so a
// failed delivery must not leave it behind and an admin's retried Export
// calls must not accumulate one dump per attempt. Three retried failing
// attempts must leave zero objects behind.
func TestExportService_Export_DeliveryFailureIsReported(t *testing.T) {
	svc, repo, store, fakeSharing := newExportHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")
	fakeSharing.failWith = errors.New("sharing unavailable")

	var keys []string
	for attempt := 1; attempt <= 3; attempt++ {
		result, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
		if !hasCode(err, ErrExportDeliveryFailed.Code) {
			t.Fatalf("Export attempt %d error = %v, want %s", attempt, err, ErrExportDeliveryFailed.Code)
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
		keys = append(keys, result.ObjectKey)
	}

	// None of the failed attempts' manifests may persist: each attempt
	// cleans up its own un-shareable object, so the store holds nothing
	// for an admin retry to pile more dumps onto.
	for _, key := range keys {
		if _, err := store.GetObject(context.Background(), key); !errors.Is(err, pkgcore.ErrObjectNotFound) {
			t.Errorf("GetObject(%q) after delivery failure = %v, want pkgcore.ErrObjectNotFound -- an undelivered manifest must not persist", key, err)
		}
	}
}

// sharingModuleStub feeds go/sharing's own embedded migrations to
// dbkit.MigrationRegistry, mirroring module_test.go's fakeAuditModule --
// only Name and Migrations are ever read by MigrationRegistry.Apply here.
type sharingModuleStub struct{}

func (sharingModuleStub) Name() string                     { return "sharing" }
func (sharingModuleStub) DependsOn() []string              { return nil }
func (sharingModuleStub) Migrations() embed.FS             { return sharingmigrations.FS }
func (sharingModuleStub) Locales() embed.FS                { return embed.FS{} }
func (sharingModuleStub) OpenAPISpec() []byte              { return nil }
func (sharingModuleStub) Register(*pkgcore.Registry) error { return nil }

var _ pkgcore.Module = sharingModuleStub{}

// newRealSharingService returns a *sharing.Service wired the way a real
// host wires one: go/sharing's own real, versioned migration files applied
// from zero through the real dbkit.MigrationRegistry -- the identical
// construction newTestAuditDB (module_test.go) uses for dbkit/audit --
// then sharing.NewModule(db).Register attached against a real
// pkgcore.Registry, exactly as Kernel.Bootstrap would attach it for a real
// host. Going through Register (rather than calling sharing.NewService
// directly) is deliberate: it exercises sharing's actual Create/Access
// implementation fully attached, so this test's event-publish and
// sensitive-audit calls behave as a real deployment's would instead of
// logging Service's documented "no host registry wired" fallback.
func newRealSharingService(t *testing.T) *sharing.Service {
	t.Helper()
	db := dbtest.NewSQLite(t)
	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(sharingModuleStub{}); err != nil {
		t.Fatalf("register sharing migrations: %v", err)
	}
	if err := registry.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply sharing migrations: %v", err)
	}

	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	sharingModule := sharing.NewModule(db)
	if err := sharingModule.Register(reg); err != nil {
		t.Fatalf("sharing.Module.Register: %v", err)
	}
	return sharingModule.Service()
}

// TestExportService_Export_DeliversThroughRealSharingService is the
// non-scripted counterpart to TestExportService_Export_DeliversThroughSharing
// above: every other Export test in this file runs against
// fakeSharingCreator, a double that just echoes back whatever
// sharing.CreateParams it was called with, so none of them prove a minted
// Share actually round-trips through a real *sharing.Service. This test
// wires ExportService.sharing to a real sharing.NewService over a real,
// migrated database, then proves the round trip end to end: the token
// Export hands back resolves through the real Service.Access to a Share
// whose ResourceRef names the export's own stored object key, and reading
// that key back from the same ObjectStore Export wrote it to yields the
// identical manifest Export gathered. (go/sharing's Access does not itself
// resolve ResourceRef into bytes, so this test reads the object directly
// through the ObjectStore -- the same seam an HTTP layer resolving a share
// to its bytes would use.)
func TestExportService_Export_DeliversThroughRealSharingService(t *testing.T) {
	svc, repo, store, _ := newExportHarness(t)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	realSharing := newRealSharingService(t)
	svc.sharing = realSharing

	result, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if result.Delivery.ShareID == "" || result.Delivery.Token == "" {
		t.Fatalf("Export result.Delivery = %+v, want a real minted share id and token", result.Delivery)
	}

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	share, err := realSharing.Access(ctx, result.Delivery.Token, sharing.AccessParams{})
	if err != nil {
		t.Fatalf("real sharing.Service.Access(minted token): %v", err)
	}
	if share.ID != result.Delivery.ShareID {
		t.Errorf("Access share ID = %q, want %q", share.ID, result.Delivery.ShareID)
	}
	if share.ResourceRef != result.ObjectKey {
		t.Errorf("Access share ResourceRef = %q, want the stored export object key %q", share.ResourceRef, result.ObjectKey)
	}

	r, err := store.GetObject(context.Background(), share.ResourceRef)
	if err != nil {
		t.Fatalf("GetObject(%q) (the ResourceRef a real caller would resolve the minted share to): %v", share.ResourceRef, err)
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
	if _, ok := stored.Participants["testutil.fake_note"]; !ok {
		t.Errorf("stored manifest missing participant %q: %+v", "testutil.fake_note", stored.Participants)
	}
}

// TestExportService_Export_ParticipantErrorClassifiedNeverRawText pins the
// manifest-content rule for the export path: a participant whose Export
// callback failed must appear in the gathered ExportManifest.Errors as a
// classification -- the participant's name keyed to participantErrorMarker
// -- never with the callback's error text verbatim. The manifest is the
// export deliverable: Export marshals it to JSON, stores it through the
// ObjectStore and delivers the stored object over an unauthenticated,
// single-view go/sharing link, so raw failure text would travel to exactly
// the audience not entitled to platform-internal diagnostics -- the link
// holder, who is entitled to read the export's data but not internal
// failure text that can name other subjects, internal object keys or
// infrastructure details (the audience argument). The text's home is the
// structured log at the gather site, behind go/observability's redaction
// layer; neither the returned manifest nor the stored bytes may carry it
// -- writing exportErr.Error() into the manifest would fail both
// assertions.
func TestExportService_Export_ParticipantErrorClassifiedNeverRawText(t *testing.T) {
	// The failure text names an internal object key -- the class of
	// platform-internal content the deliverable must never carry.
	internal := errors.New("gather compliance/exports/tenant-a/obj.json failed: object store timeout")
	failing := pkgcore.RetentionParticipant{
		// NoopSweep and NoopErase satisfy the registrar's mandatory-Sweep
		// and mandatory-Erase rules; the export service under test never
		// invokes either.
		Name:  "testutil.carving_export",
		Sweep: testutil.NoopSweep,
		Erase: testutil.NoopErase,
		Export: func(context.Context, pkgcore.TenantID) (any, error) {
			return nil, internal
		},
	}
	svc, repo, store, fakeSharing, _ := newExportHarnessWith(t, failing)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	result, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
	if !hasCode(err, ErrExportPartialFailure.Code) {
		t.Fatalf("Export error = %v, want %s", err, ErrExportPartialFailure.Code)
	}
	if got := result.Manifest.Errors["testutil.carving_export"]; got != participantErrorMarker {
		t.Errorf("Manifest.Errors[%q] = %q, want the classification marker %q -- never the error text", "testutil.carving_export", got, participantErrorMarker)
	}
	raw, marshalErr := json.Marshal(result.Manifest)
	if marshalErr != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", marshalErr)
	}
	if strings.Contains(string(raw), internal.Error()) {
		t.Errorf("the participant error text %q was serialized into the returned export manifest: %s", internal.Error(), raw)
	}

	// The stored object -- the exact bytes a delivered share link would
	// hand its holder -- must not carry the text either.
	r, err := store.GetObject(context.Background(), result.ObjectKey)
	if err != nil {
		t.Fatalf("GetObject(%q): %v", result.ObjectKey, err)
	}
	defer r.Close()
	storedRaw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read stored manifest: %v", err)
	}
	if strings.Contains(string(storedRaw), internal.Error()) {
		t.Errorf("the participant error text %q was serialized into the STORED export manifest: %s", internal.Error(), storedRaw)
	}
	if len(fakeSharing.calls) != 1 {
		t.Fatalf("sharing.Create calls = %d, want 1 -- a partial export still delivers", len(fakeSharing.calls))
	}
}

// TestExportService_Export_AuditChangesClassifyParticipantErrorNeverText
// pins the audit-trail content rule on the export path: a participant
// whose Export callback failed must appear in the export audit event's
// Changes as a classification (the participant's name keyed to
// participantErrorMarker) -- never with the callback's error text
// verbatim. The changes column is the one place on the audit table from
// which nothing can ever be removed (dbkit/audit/emit.go's Diff content
// contract: "Anything written here is effectively permanent"), and the
// text can name other subjects and internal object keys the export bundle
// itself was gathering. The error text's homes are the structured log at
// the gather site (behind go/observability's redaction layer) and the
// returned in-process result -- never the permanent audit record. emitExportAudit must never write the whole raw-text map into
// Changes["errors"]; a verbatim write fails the assertions below.
func TestExportService_Export_AuditChangesClassifyParticipantErrorNeverText(t *testing.T) {
	// The failure text names another subject whose rows the gather was
	// touching -- the class of content the permanent record must never
	// carry.
	internal := errors.New("gather failed for rows subject-9 owns: database connection refused")
	failing := pkgcore.RetentionParticipant{
		Name:  "testutil.carving_export",
		Sweep: testutil.NoopSweep,
		Erase: testutil.NoopErase,
		Export: func(context.Context, pkgcore.TenantID) (any, error) {
			return nil, internal
		},
	}
	svc, repo, _, _, captured := newExportHarnessWith(t, failing)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	_, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
	if !hasCode(err, ErrExportPartialFailure.Code) {
		t.Fatalf("Export error = %v, want %s", err, ErrExportPartialFailure.Code)
	}

	events := *captured
	if len(events) != 1 {
		t.Fatalf("captured audit events = %d, want 1", len(events))
	}
	if events[0].Action != AuditActionExportRequest {
		t.Fatalf("audit event action = %q, want %q", events[0].Action, AuditActionExportRequest)
	}
	changes := events[0].Changes
	if changes == nil {
		t.Fatal("audit event Changes = nil, want the participants/errors breakdown")
	}
	errs, ok := changes.After["errors"].(map[string]string)
	if !ok {
		t.Fatalf("Changes.After[\"errors\"] = %T, want map[string]string -- the classification map, never the raw errors", changes.After["errors"])
	}
	if got := errs["testutil.carving_export"]; got != participantErrorMarker {
		t.Errorf("Changes errors[%q] = %q, want the classification marker %q", "testutil.carving_export", got, participantErrorMarker)
	}
	raw, marshalErr := json.Marshal(changes)
	if marshalErr != nil {
		t.Fatalf("json.Marshal(Changes) error = %v", marshalErr)
	}
	if strings.Contains(string(raw), internal.Error()) {
		t.Errorf("the participant error text %q was carved into the audit trail Changes: %s", internal.Error(), raw)
	}
	if events[0].Result.Success {
		t.Error("audit Result.Success = true, want false -- the participant error must still mark the export failed")
	}
	if !strings.Contains(events[0].Result.FailureReason, "testutil.carving_export") {
		t.Errorf("audit Result.FailureReason = %q, want it to name the failing participant", events[0].Result.FailureReason)
	}
	if strings.Contains(events[0].Result.FailureReason, internal.Error()) {
		t.Errorf("the participant error text %q was carved into audit Result.FailureReason: %q", internal.Error(), events[0].Result.FailureReason)
	}
}

// TestExportService_Export_DeliveryFailureAuditClassifiesReasonNeverText
// pins the transport half of the same rule: a failure to deliver the
// already-stored manifest through go/sharing is audited with the
// classification "delivery failed" -- never the transport error's text,
// which can name internal object keys or infrastructure endpoints. The
// text's legitimate homes are the returned error (Export wraps the
// transport error as ErrExportDeliveryFailed's own cause, asserted below)
// and the structured log at the failure site, behind go/observability's
// redaction layer; the audit record -- effectively permanent -- carries
// only the classification: the delivery-failure audit event's
// FailureReason must never be fmt.Sprintf("delivery failed: %s",
// deliverErr.Error()) -- a verbatim write fails the assertions below.
func TestExportService_Export_DeliveryFailureAuditClassifiesReasonNeverText(t *testing.T) {
	transport := errors.New("create share for compliance/exports/tenant-a/x.json failed: sharing database unavailable")
	svc, repo, _, fakeSharing, captured := newExportHarnessWith(t)
	fakeSharing.failWith = transport
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	_, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
	if !hasCode(err, ErrExportDeliveryFailed.Code) {
		t.Fatalf("Export error = %v, want %s", err, ErrExportDeliveryFailed.Code)
	}
	if !errors.Is(err, transport) {
		t.Errorf("Export error = %v, want the transport error preserved as the returned error's cause -- its in-process home", err)
	}

	events := *captured
	if len(events) != 1 {
		t.Fatalf("captured audit events = %d, want 1", len(events))
	}
	if events[0].Action != AuditActionExportRequest {
		t.Fatalf("audit event action = %q, want %q", events[0].Action, AuditActionExportRequest)
	}
	if events[0].Result.Success {
		t.Error("audit Result.Success = true, want false -- the delivery failure must mark the export failed")
	}
	if got := events[0].Result.FailureReason; got != "delivery failed" {
		t.Errorf("audit Result.FailureReason = %q, want the classification %q -- never the transport error text", got, "delivery failed")
	}
	if strings.Contains(events[0].Result.FailureReason, transport.Error()) {
		t.Errorf("the transport error text %q was carved into audit Result.FailureReason: %q", transport.Error(), events[0].Result.FailureReason)
	}
	raw, marshalErr := json.Marshal(events[0])
	if marshalErr != nil {
		t.Fatalf("json.Marshal(event) error = %v", marshalErr)
	}
	if strings.Contains(string(raw), transport.Error()) {
		t.Errorf("the transport error text %q was carved into the audit event: %s", transport.Error(), raw)
	}
}

// scriptedStore is a pkgcore.ObjectStore delegating to a real local store
// whose three operations can each be scripted to fail. It stands in for a
// store backend that genuinely refuses an operation (a full disk, a
// broken bucket), which a healthy local store cannot produce; closeErr
// additionally scripts the probe reader Export's cleanup sweep closes, so
// a close failure on the probe can be driven too.
type scriptedStore struct {
	pkgcore.ObjectStore
	failPut     error
	failGet     error
	failDelete  error
	closeErr    error
	deleteCalls int
}

func (s *scriptedStore) PutObject(ctx context.Context, key string, r io.Reader) error {
	if s.failPut != nil {
		return s.failPut
	}
	return s.ObjectStore.PutObject(ctx, key, r)
}

func (s *scriptedStore) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	if s.failGet != nil {
		return nil, s.failGet
	}
	rc, err := s.ObjectStore.GetObject(ctx, key)
	if err != nil {
		return nil, err
	}
	if s.closeErr != nil {
		return &failingReadCloser{ReadCloser: rc, err: s.closeErr}, nil
	}
	return rc, nil
}

func (s *scriptedStore) DeleteObject(ctx context.Context, key string) error {
	s.deleteCalls++
	if s.failDelete != nil {
		return s.failDelete
	}
	return s.ObjectStore.DeleteObject(ctx, key)
}

var _ pkgcore.ObjectStore = (*scriptedStore)(nil)

// failingReadCloser wraps a real reader whose Close fails -- the probe
// reader shape a store can legitimately return when its close path errors.
type failingReadCloser struct {
	io.ReadCloser
	err error
}

func (f *failingReadCloser) Close() error { return f.err }

// errScriptedStoreRefusal is the one error every scripted store failure
// returns, so assertions can tell a scripted refusal apart from any other
// failure the code under test might produce.
var errScriptedStoreRefusal = errors.New("scripted store refuses")

// TestExportService_Export_UnmarshalableParticipantValueFailsClosedBeforeStoring
// proves the manifest-marshal gate: a participant whose Export callback
// returns a value JSON cannot represent (a channel -- a misbehaving
// participant, since the participant contract promises a JSON-serializable
// value) fails the whole export before anything is stored or delivered.
// An un-marshalable manifest must never become a half-written object with
// no delivery to reference it.
func TestExportService_Export_UnmarshalableParticipantValueFailsClosedBeforeStoring(t *testing.T) {
	bad := pkgcore.RetentionParticipant{
		Name:   "testutil.unmarshalable",
		Sweep:  testutil.NoopSweep,
		Erase:  testutil.NoopErase,
		Export: func(context.Context, pkgcore.TenantID) (any, error) { return make(chan int), nil },
	}
	svc, repo, _, fakeSharing, _ := newExportHarnessWith(t, bad)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	result, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
	if err == nil {
		t.Fatal("Export = nil error, want the manifest marshal failure")
	}
	if !strings.Contains(err.Error(), "marshal export manifest") {
		t.Errorf("Export error = %q, want the marshal failure named", err)
	}
	if result != nil {
		t.Errorf("Export result = %+v, want nil -- nothing was stored or delivered", result)
	}
	if len(fakeSharing.calls) != 0 {
		t.Errorf("sharing.Create calls = %d, want 0 -- delivery must never be attempted for an un-marshalable manifest", len(fakeSharing.calls))
	}
}

// TestExportService_Export_StorePutFailureFailsClosedBeforeDelivery proves
// the store gate: when the object store refuses the manifest, Export
// reports the failure and never attempts delivery -- a manifest that was
// never durably stored must not be handed to go/sharing as if it had
// been.
func TestExportService_Export_StorePutFailureFailsClosedBeforeDelivery(t *testing.T) {
	store := &scriptedStore{ObjectStore: pkgcore.NewLocalObjectStore(t.TempDir()), failPut: errScriptedStoreRefusal}
	svc, repo, _, fakeSharing, _ := newExportHarnessSeamed(t, nil, store)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	result, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
	if err == nil {
		t.Fatal("Export = nil error, want the store failure")
	}
	if !strings.Contains(err.Error(), "store export manifest") {
		t.Errorf("Export error = %q, want the store failure named", err)
	}
	if result != nil {
		t.Errorf("Export result = %+v, want nil -- nothing was stored or delivered", result)
	}
	if len(fakeSharing.calls) != 0 {
		t.Errorf("sharing.Create calls = %d, want 0 -- delivery must never be attempted for an unstored manifest", len(fakeSharing.calls))
	}
}

// TestExportService_Export_AuditRecordFailureAfterSuccessfulDeliverySurfaces
// proves the export path's audit-record contract on a fully successful
// run: gather, store and delivery all completed, but the one
// AuditActionExportRequest event recording it could not be published --
// Export returns the completed result (the stored object and the minted
// share stay valid, since the export itself is done) alongside
// ErrAuditRecordFailed, never erasing what already happened.
func TestExportService_Export_AuditRecordFailureAfterSuccessfulDeliverySurfaces(t *testing.T) {
	bus := &scriptedBus{EventBus: pkgcore.NewMemoryEventBus(), failFrom: 1, failErr: errors.New("scripted bus refuses")}
	svc, repo, store, _, _ := newExportHarnessSeamed(t, bus, nil)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	result, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
	if !hasCode(err, ErrAuditRecordFailed.Code) {
		t.Fatalf("Export error = %v, want %s", err, ErrAuditRecordFailed.Code)
	}
	if result == nil {
		t.Fatal("Export should return a non-nil result even when its audit record fails")
	}
	if result.ObjectKey == "" || result.Delivery.ShareID == "" || result.Delivery.Token == "" {
		t.Fatalf("result = %+v, want the stored object key and the minted share populated", result)
	}
	if _, err := store.GetObject(context.Background(), result.ObjectKey); err != nil {
		t.Errorf("GetObject(%q) after the audit failure = %v, want the stored manifest to survive", result.ObjectKey, err)
	}
}

// TestExportService_Export_DeliveryCleanupAndAuditFailuresSurface proves
// the compound-failure shape of the delivery-failed path: delivery
// refused, the cleanup delete of the un-shareable manifest also refused,
// and the audit record of it all refused -- Export reports the audit
// failure (ErrAuditRecordFailed) while returning the result with the
// stored object key still populated, and the store really was asked to
// delete the undeliverable object (the manifest is still there only
// because the store itself is broken). Every leg of the failure is
// surfaced rather than assumed away.
func TestExportService_Export_DeliveryCleanupAndAuditFailuresSurface(t *testing.T) {
	store := &scriptedStore{ObjectStore: pkgcore.NewLocalObjectStore(t.TempDir()), failDelete: errScriptedStoreRefusal}
	bus := &scriptedBus{EventBus: pkgcore.NewMemoryEventBus(), failFrom: 1, failErr: errors.New("scripted bus refuses")}
	svc, repo, _, fakeSharing, _ := newExportHarnessSeamed(t, bus, store)
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")
	fakeSharing.failWith = errors.New("sharing unavailable")

	result, err := svc.Export(pkgcore.WithTenant(context.Background(), tenant), tenant)
	if !hasCode(err, ErrAuditRecordFailed.Code) {
		t.Fatalf("Export error = %v, want %s (the audit failure takes the returned error)", err, ErrAuditRecordFailed.Code)
	}
	if result == nil {
		t.Fatal("Export should return a non-nil result even when delivery failed")
	}
	if result.ObjectKey == "" {
		t.Error("ObjectKey should still be populated: the manifest was stored before delivery was attempted")
	}
	if result.Delivery != (ExportDelivery{}) {
		t.Errorf("Delivery = %+v, want the zero value", result.Delivery)
	}
	if store.deleteCalls != 1 {
		t.Errorf("store.DeleteObject calls = %d, want 1 -- the undeliverable manifest's cleanup must be attempted", store.deleteCalls)
	}
	// The delete was refused, so the un-shareable dump is still stored --
	// and the returned error must let the operator know that possibility.
	if _, err := store.ObjectStore.GetObject(context.Background(), result.ObjectKey); err != nil {
		t.Errorf("GetObject(%q) = %v, want the refused delete to have left the manifest stored", result.ObjectKey, err)
	}
}
