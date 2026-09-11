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
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/compliance/internal/testutil"
)

// newManifestCleanupHarness returns a RetentionService wired directly over
// a hand-built pkgcore.ComponentRegistry carrying ONLY the module's own
// export-manifests cleanup participant, plus the real *audit.Repository
// (over a freshly migrated audit_events table) and the real
// pkgcore.LocalObjectStore the participant reads and reaps through -- the
// same seams Module.Register wires it with. SweepTenant is driven through
// its public surface so the participant runs under the sweep's real
// system-context and audit machinery.
func newManifestCleanupHarness(t *testing.T) (*RetentionService, *audit.Repository, pkgcore.ObjectStore) {
	t.Helper()
	return newManifestCleanupHarnessSeamed(t, audit.NewRepository(newTestAuditDB(t)), pkgcore.NewLocalObjectStore(t.TempDir()))
}

// newManifestCleanupHarnessSeamed is newManifestCleanupHarness over
// injected repo and store seams: a test substitutes a repository whose
// underlying database is closed, or a scripted store whose operations
// fail, to drive the sweep's failure branches -- a genuinely broken read
// or store must be reported as the participant's error, never silently
// skipped.
func newManifestCleanupHarnessSeamed(t *testing.T, auditRepo *audit.Repository, store pkgcore.ObjectStore) (*RetentionService, *audit.Repository, pkgcore.ObjectStore) {
	t.Helper()
	bus := pkgcore.NewMemoryEventBus()
	reg := componenttest.NewRegistry()
	reg.Put(bus)
	if err := reg.AuditActions.Add(AuditActionRetentionSweep); err != nil {
		t.Fatalf("declare audit action: %v", err)
	}
	pkgcore.RegisterSystemPurpose(SystemPurposeRetentionSweep)

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
	insertExportAuditRowWithResult(t, repo, tenant, id, key, shareExpiresAt, true, "", nil)
}

// insertExportAuditRowWithResult inserts one AuditActionExportRequest audit
// row in any of the shapes emitExportAudit leaves: Success, FailureReason
// and any extra After entries (beyond object_key, share_id,
// share_expires_at and the participants list) supplied by the caller. A
// partial export's row, for example, reports Success false with a
// participants-failed reason and the per-participant errors map in After.
func insertExportAuditRowWithResult(t *testing.T, repo *audit.Repository, tenant pkgcore.TenantID, id, key string, shareExpiresAt time.Time, success bool, failureReason string, extraAfter map[string]any) {
	t.Helper()
	after := map[string]any{
		"object_key":       key,
		"share_id":         "share-" + id,
		"share_expires_at": shareExpiresAt,
		"participants":     []string{},
	}
	for k, v := range extraAfter {
		after[k] = v
	}
	changes, err := json.Marshal(audit.Diff{After: after})
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
	evt.SetResult(audit.Result{Success: success, FailureReason: failureReason})
	if err := repo.Insert(context.Background(), evt); err != nil {
		t.Fatalf("insert export audit event: %v", err)
	}
}

// seedPartialExportDelivery stores one manifest object under tenant's own
// exportObjectKey namespace and records the audit row a REAL partial
// export leaves for it -- Success false with the participants-failed
// reason, Changes.After carrying the contributing participants and the
// classification-only per-participant errors map (each failed participant
// keyed to participantErrorMarker, never the callback's error text)
// alongside object_key and share_expires_at (emitExportAudit's exact
// partial shape) -- the row whose stored object the sweep must reap once
// its share expires. Returns the object key.
func seedPartialExportDelivery(t *testing.T, repo *audit.Repository, store pkgcore.ObjectStore, tenant pkgcore.TenantID, id string, shareExpiresAt time.Time) string {
	t.Helper()
	key := exportObjectKey(tenant, id)
	payload := []byte(`{"tenant":"` + string(tenant) + `","id":"` + id + `"}`)
	if err := store.PutObject(context.Background(), key, bytes.NewReader(payload)); err != nil {
		t.Fatalf("store manifest object %q: %v", key, err)
	}
	insertExportAuditRowWithResult(t, repo, tenant, id, key, shareExpiresAt, false, "participants failed: testutil.failing_export", map[string]any{
		"participants": []string{"testutil.fake_note"},
		"errors":       map[string]string{"testutil.failing_export": participantErrorMarker},
	})
	return key
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

// TestExportManifestCleanup_SweepReapsOnlyExpiredDeliveries is the
// cleanup-half regression for the manifest retention story: a stored
// export manifest whose delivery share has expired past the tenant's
// retention-window cutoff is reaped by the module's own retention sweep
// (the export-manifests participant), while a manifest whose share is
// still live, an expired manifest of another tenant, an object a tampered
// event names outside the swept tenant's own compliance/exports/ prefix,
// and an expired event whose object an earlier pass already reaped all
// survive untouched -- and a second sweep over the same rows converges to
// 0, the documented retry contract. Without the cleanup mechanism, every
// successfully delivered manifest would stay in the object store forever,
// accumulating unbounded.
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

// TestExportManifestCleanup_SweepReapsExpiredPartialFailureExport is
// the partial-export half of the same regression: a PARTIAL export -- one
// participant's Export callback failed while others contributed -- is
// still gathered, stored and delivered, and its audit event records that
// as Success false (emitExportAudit's `success := !manifest.HasErrors()`),
// while still carrying the same object_key and share_expires_at a full
// export's event does. A candidate gate on Result.Success true --
// judging "is there something to reap" by "did the operation succeed" --
// would leave a partial export's stored manifest forever un-reaped, one
// stored bundle per partial export. The honest gate is the delivery
// share's own expiry: a partial export's manifest whose share expired
// past the retention cutoff is reaped exactly like a full export's, one
// whose share is still live survives untouched, and a re-run over the
// same rows converges to 0.
func TestExportManifestCleanup_SweepReapsExpiredPartialFailureExport(t *testing.T) {
	svc, auditRepo, store := newManifestCleanupHarness(t)

	expiredPartialKey := seedPartialExportDelivery(t, auditRepo, store, "tenant-a", "partial-expired", time.Now().Add(-40*24*time.Hour))
	livePartialKey := seedPartialExportDelivery(t, auditRepo, store, "tenant-a", "partial-live", time.Now().Add(defaultExportDeliveryExpiry))

	result, err := svc.SweepTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("SweepTenant: %v", err)
	}
	if result.HasErrors() {
		t.Fatalf("SweepTenant errors = %v, want none", result.Errors)
	}
	if got := result.Reaped[exportManifestsParticipantName]; got != 1 {
		t.Fatalf("SweepTenant reaped %s = %d, want exactly 1 (tenant-a's expired partial-failure manifest) -- on the unfixed code the Success gate skipped every partial export's stored dump", exportManifestsParticipantName, got)
	}
	if manifestObjectExists(t, store, expiredPartialKey) {
		t.Error("tenant-a's expired partial-failure manifest should have been reaped")
	}
	if !manifestObjectExists(t, store, livePartialKey) {
		t.Error("tenant-a's still-live partial-failure manifest must survive the sweep")
	}

	again, err := svc.SweepTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("second SweepTenant: %v", err)
	}
	if got := again.Reaped[exportManifestsParticipantName]; got != 0 {
		t.Fatalf("second SweepTenant reaped %s = %d, want 0 -- already-reaped partial manifests must not be recounted", exportManifestsParticipantName, got)
	}
}

// insertForeignActionAuditRow inserts one audit row for tenant whose
// Action is not AuditActionExportRequest but whose Changes carry the exact
// export-delivery shape (object_key, share_expires_at past) -- the row the
// sweep must skip on its action check, so an expired object it names must
// survive even though a same-shaped export row would have had it reaped.
func insertForeignActionAuditRow(t *testing.T, repo *audit.Repository, tenant pkgcore.TenantID, id, action, key string, shareExpiresAt time.Time) {
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
		Action:     action,
		OccurredAt: time.Now().Add(-time.Hour),
		Changes:    datatypes.JSON(changes),
	}
	evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeSystem, ID: "compliance.export", DisplayName: "compliance.export"})
	evt.SetResource(audit.Resource{Type: "compliance.tenant", ID: string(tenant), DisplayName: string(tenant)})
	evt.SetResult(audit.Result{Success: true})
	if err := repo.Insert(context.Background(), evt); err != nil {
		t.Fatalf("insert foreign-action audit event: %v", err)
	}
}

// insertUnreadableExportRow inserts one AuditActionExportRequest row whose
// Changes cannot be decoded -- the malformed record a sweep must skip
// rather than fail the whole tenant over.
func insertUnreadableExportRow(t *testing.T, repo *audit.Repository, tenant pkgcore.TenantID, id string) {
	t.Helper()
	evt := &audit.AuditEvent{
		ID:         "evt-" + id,
		TenantID:   string(tenant),
		Action:     AuditActionExportRequest,
		OccurredAt: time.Now().Add(-time.Hour),
		Changes:    datatypes.JSON([]byte(`{not valid json`)),
	}
	evt.SetActor(pkgcore.Actor{Type: pkgcore.ActorTypeSystem, ID: "compliance.export", DisplayName: "compliance.export"})
	evt.SetResource(audit.Resource{Type: "compliance.tenant", ID: string(tenant), DisplayName: string(tenant)})
	evt.SetResult(audit.Result{Success: true})
	if err := repo.Insert(context.Background(), evt); err != nil {
		t.Fatalf("insert unreadable export event: %v", err)
	}
}

// TestExportManifestCleanup_SweepSkipsForeignActionAndUnreadableRecords
// proves the sweep's row-level resilience: a row of some other action and
// a row whose Changes cannot be read are each skipped rather than reaped
// or fatal -- the one skips an expired object that must survive (the
// action check comes before any object is touched), the other names no
// re-claimable object at all, and neither may fail the whole tenant's
// sweep over one foreign or corrupt record in its own trail.
func TestExportManifestCleanup_SweepSkipsForeignActionAndUnreadableRecords(t *testing.T) {
	svc, auditRepo, store := newManifestCleanupHarness(t)

	goodKey := seedExportDelivery(t, auditRepo, store, "tenant-a", "good", time.Now().Add(-40*24*time.Hour))
	foreignKey := "compliance/exports/tenant-a/foreign.json"
	if err := store.PutObject(context.Background(), foreignKey, bytes.NewReader([]byte(`{"foreign":true}`))); err != nil {
		t.Fatalf("store foreign-action object: %v", err)
	}
	insertForeignActionAuditRow(t, auditRepo, "tenant-a", "foreign", "compliance.erasure.request", foreignKey, time.Now().Add(-40*24*time.Hour))
	insertUnreadableExportRow(t, auditRepo, "tenant-a", "unreadable")

	result, err := svc.SweepTenant(context.Background(), "tenant-a")
	if err != nil {
		t.Fatalf("SweepTenant: %v", err)
	}
	if result.HasErrors() {
		t.Fatalf("SweepTenant errors = %v, want none -- a foreign or unreadable row must not fail the sweep", result.Errors)
	}
	if got := result.Reaped[exportManifestsParticipantName]; got != 1 {
		t.Fatalf("SweepTenant reaped %s = %d, want exactly 1 (only the genuine expired export)", exportManifestsParticipantName, got)
	}
	if manifestObjectExists(t, store, goodKey) {
		t.Error("the genuine expired manifest should have been reaped")
	}
	if !manifestObjectExists(t, store, foreignKey) {
		t.Error("the expired object named only by a foreign-action row must survive -- the action check never reaches it")
	}
}

// TestExportManifestCleanup_SweepStoreFailuresReportedNotSkipped proves
// the sweep never treats a broken store as an empty one: a probe read that
// fails, a probe reader whose Close fails, and a delete that fails are all
// reported as the participant's error (and thus as the sweep's partial
// failure), with the object left in place -- an object whose reaping could
// not be verified or completed must not be counted as reaped, and the
// operator must hear about the broken store rather than watch the sweep
// report success over garbage it could not remove.
func TestExportManifestCleanup_SweepStoreFailuresReportedNotSkipped(t *testing.T) {
	cases := []struct {
		name   string
		script func(*scriptedStore)
	}{
		{"GetObject", func(s *scriptedStore) { s.failGet = errScriptedStoreRefusal }},
		{"ProbeClose", func(s *scriptedStore) { s.closeErr = errScriptedStoreRefusal }},
		{"DeleteObject", func(s *scriptedStore) { s.failDelete = errScriptedStoreRefusal }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := pkgcore.NewLocalObjectStore(t.TempDir())
			store := &scriptedStore{ObjectStore: base}
			svc, auditRepo, _ := newManifestCleanupHarnessSeamed(t, audit.NewRepository(newTestAuditDB(t)), store)
			key := seedExportDelivery(t, auditRepo, store, "tenant-a", "expired", time.Now().Add(-40*24*time.Hour))
			tc.script(store)

			result, err := svc.SweepTenant(context.Background(), "tenant-a")
			if !apperr.HasCode(err, ErrSweepPartialFailure.Code) {
				t.Fatalf("SweepTenant error = %v, want %s -- the store failure must be reported", err, ErrSweepPartialFailure.Code)
			}
			got := result.Errors[exportManifestsParticipantName]
			if got == nil {
				t.Fatalf("SweepResult.Errors = %v, want %q to carry the store failure", result.Errors, exportManifestsParticipantName)
			}
			if !errors.Is(got, errScriptedStoreRefusal) {
				t.Errorf("Errors[%q] = %v, want the store's own error preserved", exportManifestsParticipantName, got)
			}
			if !manifestObjectExists(t, base, key) {
				t.Error("the manifest must survive: the store failure left it un-reaped, and nothing may claim otherwise")
			}
		})
	}
}

// TestExportManifestCleanup_SweepListFailureReportedAsParticipantError
// proves a sweep whose audit-trail read itself fails reports that as the
// participant's error rather than silently reaping nothing: with the
// repository's database closed, the participant cannot even enumerate its
// candidates, and the sweep must say so -- a cleanup that cannot read its
// own ledger must not masquerade as a clean pass.
func TestExportManifestCleanup_SweepListFailureReportedAsParticipantError(t *testing.T) {
	db := newTestAuditDB(t)
	auditRepo := audit.NewRepository(db)
	svc, _, _ := newManifestCleanupHarnessSeamed(t, auditRepo, pkgcore.NewLocalObjectStore(t.TempDir()))

	sqlDB, dbErr := db.DB()
	if dbErr != nil {
		t.Fatalf("get sql.DB: %v", dbErr)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close sql.DB: %v", err)
	}

	result, err := svc.SweepTenant(context.Background(), "tenant-a")
	if !apperr.HasCode(err, ErrSweepPartialFailure.Code) {
		t.Fatalf("SweepTenant error = %v, want %s -- the unreadable audit trail must be reported", err, ErrSweepPartialFailure.Code)
	}
	if result.Errors[exportManifestsParticipantName] == nil {
		t.Errorf("SweepResult.Errors = %v, want %q to carry the read failure", result.Errors, exportManifestsParticipantName)
	}
}

// TestExportManifestCleanup_EraseAnswersNothingToErase proves the module's
// own declared erasure answer: a stored export manifest is a tenant-wide
// bundle, never a single subject's rows, so a right-to-erasure request for
// one subject must leave every manifest untouched and report exactly that
// -- an explicit (0, nil), never a silent skip and never a claim that the
// subject's data was erased.
func TestExportManifestCleanup_EraseAnswersNothingToErase(t *testing.T) {
	repo := testutil.NewFakeRepository(testutil.NewDB(t))
	auditRepo := audit.NewRepository(newTestAuditDB(t))
	store := pkgcore.NewLocalObjectStore(t.TempDir())
	svc, _ := newErasureServiceOn(t, pkgcore.NewMemoryEventBus(),
		exportManifestsParticipant(auditRepo, store), testutil.NewParticipant("testutil.fake_note", repo))
	tenant := pkgcore.TenantID("tenant-a")
	seedLiveFakeNote(t, repo, tenant, "note-1", "subject-1")

	ctx := pkgcore.WithTenant(context.Background(), tenant)
	result, err := svc.Erase(ctx, pkgcore.SubjectRef{TenantID: tenant, SubjectID: "subject-1"}, testErasureActor)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if result.HasErrors() {
		t.Fatalf("Erase errors = %v, want none", result.Errors)
	}
	if got := result.Erased[exportManifestsParticipantName]; got != 0 {
		t.Errorf("Erased[%q] = %d, want 0 -- a manifest is tenant-wide, never a subject's data to erase", exportManifestsParticipantName, got)
	}
	if got := result.Erased["testutil.fake_note"]; got != 1 {
		t.Errorf("Erased[testutil.fake_note] = %d, want the subject's own rows erased alongside", got)
	}
}
