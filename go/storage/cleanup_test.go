package storage

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/storage/internal/testutil"
)

// This file tests LifecycleService, the deletion and expiry runtime: Delete
// and Sweep driven over the real repository (so every test is also a proof
// the migrations apply) against the same file-local fakes object_test.go
// uses. Most tests seed rows directly -- completed rows, expired windows,
// rows already in deleting -- because the service's inputs are states, not
// pipelines; one test drives the full transfer-and-derive journey so the
// delete protocol is proven against the module's real end state.
//
// The fakes extend object_test.go's set by one seam: failStore, a
// fakeStore whose next DeleteObject calls can be made to fail, which is the
// crash injection the protocol-resume tests need -- a store that dies
// between two byte deletions must leave the row deleting with part of its
// bytes already gone, and the next run must finish the protocol, not
// duplicate or lose it.

// failStore wraps a fakeStore and makes exactly one DeleteObject call --
// the failOn-th one, counted from 1 -- fail with failErr, all other calls
// passing through. That is the crash injection the protocol-resume tests
// need: a store that dies between two byte deletions leaves the row in
// deleting with part of its bytes already gone, and the next run finishes
// the protocol, not duplicates or loses it.
type failStore struct {
	*fakeStore
	failErr error
	failOn  int // 1-based index of the DeleteObject call to fail; 0 = never
	call    int
}

func (s *failStore) DeleteObject(ctx context.Context, key string) error {
	s.call++
	if s.failOn != 0 && s.call == s.failOn {
		return s.failErr
	}
	return s.fakeStore.DeleteObject(ctx, key)
}

var _ pkgcore.ObjectStore = (*failStore)(nil)

// newCleanupHarness returns a LifecycleService, an ObjectService and a
// DeriveService sharing one migrated database, one store, one queue and one
// bus -- the module's real composition, minus the registry. The transfer
// and derive services drive the real end states (a completed object with a
// derived thumbnail); the lifecycle service deletes and sweeps them.
func newCleanupHarness(t *testing.T) (*LifecycleService, *ObjectService, *DeriveService, *fakeStore, *recordingQueue, *recordingBus) {
	t.Helper()
	cfg := testServiceConfig()
	store := newFakeStore()
	queue := &recordingQueue{}
	bus := newRecordingBus()
	host := &fakeHost{store: store, bus: bus}
	db := newTestDB(t)
	objects := NewObjectRepository(db)
	life := newLifecycleService(objects, NewDerivativeRepository(db), queue)
	life.host = host
	svc := newObjectService(objects, queue, cfg)
	svc.host = host
	derive := newDeriveService(objects, NewDerivativeRepository(db), 0, cfg.maxImagePixels)
	derive.host = host
	return life, svc, derive, store, queue, bus
}

// seedCompletedWithBytes seeds one completed row of tenant with 64 bytes
// under its canonical original key -- the minimal delete target whose byte
// removal a test can assert on.
func seedCompletedWithBytes(t *testing.T, life *LifecycleService, store *fakeStore, id string, tenant pkgcore.TenantID) Object {
	t.Helper()
	row := newCompleted(id, tenant, time.Now().Add(-2*time.Hour))
	seedObject(t, life.objects, tenantCtx(tenant), row)
	store.objects[row.Key] = bytes.Repeat([]byte{0xAB}, 64)
	return row
}

// seedDerivativeBytes seeds one thumbnail derivative row and its bytes for
// objectID -- a delete target whose derivative-bytes removal a test asserts
// on without running the derive pipeline.
func seedDerivativeBytes(t *testing.T, life *LifecycleService, store *fakeStore, objectID string, tenant pkgcore.TenantID) {
	t.Helper()
	key := thumbKey(t, tenant, objectID)
	d := ObjectDerivative{
		ID:          uuid.NewString(),
		TenantModel: dbkit.TenantModel{TenantID: string(tenant)},
		ObjectID:    objectID,
		Kind:        DerivativeKindThumbnail,
		Key:         key,
		MIME:        "image/png",
		Size:        64,
	}
	seedDerivative(t, life.derivatives, tenantCtx(tenant), d)
	store.objects[key] = bytes.Repeat([]byte{0xCD}, 64)
}

// assertObjectGone fails t unless the row no longer exists for its own
// tenant -- every test's "the delete committed" assertion.
func assertObjectGone(t *testing.T, repo *ObjectRepository, ctx context.Context, id string) {
	t.Helper()
	_, err := repo.FindByID(ctx, id)
	assertCode(t, err, dbkit.ErrRecordNotFound.Code)
}

// TestLifecycleService_Delete_RemovesRowsBytesAndDerivativesAndPublishesOnce
// drives the full delete journey against the module's real end state: an
// object created, uploaded, completed and derived through the services, so
// the original bytes, the thumbnail bytes and the derivative row all exist.
// Delete must remove all three, announce the deletion once, and converge
// silently when the same object is deleted again.
func TestLifecycleService_Delete_RemovesRowsBytesAndDerivativesAndPublishesOnce(t *testing.T) {
	life, svc, derive, store, _, bus := newCleanupHarness(t)
	ctx := serviceCtx("tenant-a")
	row := createAndUpload(t, svc, ctx, testutil.PNG(t, 400, 300), "image/png")
	if _, err := svc.Complete(ctx, row.ID); err != nil {
		t.Fatalf("Complete(%s): %v", row.ID, err)
	}
	if err := derive.DeriveThumbnail(ctx, row.ID); err != nil {
		t.Fatalf("DeriveThumbnail(%s): %v", row.ID, err)
	}
	thumbKey := thumbKey(t, "tenant-a", row.ID)
	if _, ok := store.bytes(row.Key); !ok {
		t.Fatalf("original bytes missing before the delete")
	}
	if _, ok := store.bytes(thumbKey); !ok {
		t.Fatalf("thumbnail bytes missing before the delete")
	}

	if err := life.Delete(ctx, row.ID); err != nil {
		t.Fatalf("Delete(%s): %v", row.ID, err)
	}

	// The rows are gone: the object row and the derivative row alike.
	assertObjectGone(t, life.objects, ctx, row.ID)
	derivatives, err := life.derivatives.listByObject(ctx, row.ID)
	if err != nil {
		t.Fatalf("listByObject(%s): %v", row.ID, err)
	}
	if len(derivatives) != 0 {
		t.Errorf("object %s still has %d derivative rows after the delete", row.ID, len(derivatives))
	}

	// The bytes are gone, original and derivative alike.
	if _, ok := store.bytes(row.Key); ok {
		t.Errorf("original bytes still under %q after the delete", row.Key)
	}
	if _, ok := store.bytes(thumbKey); ok {
		t.Errorf("thumbnail bytes still under %q after the delete", thumbKey)
	}

	// The deletion was announced exactly once -- among everything the shared
	// bus has carried, of which the journey's completion event is the other.
	deleted := deletedEventsOfType(bus, EventObjectDeleted)
	if len(deleted) != 1 {
		t.Fatalf("object-deleted events = %d, want exactly one", len(deleted))
	}
	evt := deleted[0]
	if evt.TenantID != pkgcore.TenantID("tenant-a") {
		t.Errorf("event tenant = %q, want %q", evt.TenantID, "tenant-a")
	}
	payload, ok := evt.Payload.(ObjectDeletedPayload)
	if !ok {
		t.Fatalf("event payload = %T, want ObjectDeletedPayload", evt.Payload)
	}
	if payload.ObjectID != row.ID {
		t.Errorf("event payload = %+v, want object %s", payload, row.ID)
	}

	// Deleting the same object again converges on "already gone" and
	// announces nothing a second time.
	if err := life.Delete(ctx, row.ID); err != nil {
		t.Fatalf("second Delete(%s): %v", row.ID, err)
	}
	if got := len(deletedEventsOfType(bus, EventObjectDeleted)); got != 1 {
		t.Errorf("object-deleted events after the second delete = %d, want still exactly one", got)
	}
}

// deletedEventsOfType returns the bus events of one type. Tests that share
// the bus with an earlier journey (a completion announcing itself) count
// the delete protocol's own announcements through it.
func deletedEventsOfType(bus *recordingBus, eventType string) []pkgcore.Event {
	var got []pkgcore.Event
	for _, evt := range bus.events {
		if evt.Type == eventType {
			got = append(got, evt)
		}
	}
	return got
}

// TestLifecycleService_Delete_ConvergesOnAnObjectItCannotSee pins the
// cross-tenant shape of "already gone": a delete of an id that names
// another tenant's object must converge on nil -- indistinguishable from an
// id that never existed -- and must not touch the row it cannot see.
func TestLifecycleService_Delete_ConvergesOnAnObjectItCannotSee(t *testing.T) {
	life, _, _, store, _, bus := newCleanupHarness(t)
	row := seedCompletedWithBytes(t, life, store, "obj-a", "tenant-a")

	if err := life.Delete(serviceCtx("tenant-b"), row.ID); err != nil {
		t.Fatalf("Delete(%s) from another tenant: %v", row.ID, err)
	}

	// The row and its bytes survive, owned by tenant-a exactly as before.
	got, err := life.objects.FindByID(tenantCtx("tenant-a"), row.ID)
	if err != nil {
		t.Fatalf("FindByID(%s) after the foreign delete: %v", row.ID, err)
	}
	if got.State != ObjectStateCompleted {
		t.Errorf("row %s state = %q, want %q -- a foreign delete must not mark it", row.ID, got.State, ObjectStateCompleted)
	}
	if _, ok := store.bytes(row.Key); !ok {
		t.Errorf("bytes under %q vanished after a delete that could not see them", row.Key)
	}
	if len(bus.events) != 0 {
		t.Errorf("events = %d, want none -- a converged delete announces nothing", len(bus.events))
	}
}

// TestLifecycleService_Delete_RefusesAnUploadingObject pins the one state a
// delete refuses: an upload in flight belongs to the transfer runtime and
// may still complete, so Delete must refuse it untouched -- row, bytes and
// state -- and only the sweep reclaims uploading rows, once their window
// closes.
func TestLifecycleService_Delete_RefusesAnUploadingObject(t *testing.T) {
	life, svc, _, store, _, bus := newCleanupHarness(t)
	ctx := serviceCtx("tenant-a")
	row := createAndUpload(t, svc, ctx, testutil.PNG(t, 32, 24), "image/png")

	err := life.Delete(ctx, row.ID)
	assertCode(t, err, ErrObjectUploading.Code)
	assertParam(t, err, "id", row.ID)

	got, err := life.objects.FindByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("FindByID(%s) after the refusal: %v", row.ID, err)
	}
	if got.State != ObjectStateUploading {
		t.Errorf("row %s state = %q after the refusal, want %q", row.ID, got.State, ObjectStateUploading)
	}
	if _, ok := store.bytes(row.Key); !ok {
		t.Errorf("uploaded bytes under %q vanished after a refused delete", row.Key)
	}
	if len(bus.events) != 0 {
		t.Errorf("events = %d, want none after a refused delete", len(bus.events))
	}
}

// TestLifecycleService_Delete_NoStoreFailsClosedAndTheMarkLetsItFinishLater
// pins the fail-closed shape of a store-less delete: the mark has already
// committed (the row reads deleting, so readers see nothing), the byte
// removal is refused with storage.store_unavailable, and the deletion
// finishes when a store arrives -- which is exactly the sweep's first
// phase re-running the protocol.
func TestLifecycleService_Delete_NoStoreFailsClosedAndTheMarkLetsItFinishLater(t *testing.T) {
	life, _, _, store, _, bus := newCleanupHarness(t)
	row := seedCompletedWithBytes(t, life, store, "obj-a", "tenant-a")
	life.host = &fakeHost{store: nil, bus: bus}
	ctx := serviceCtx("tenant-a")

	err := life.Delete(ctx, row.ID)
	assertCode(t, err, ErrStoreUnavailable.Code)

	got, err := life.objects.FindByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("FindByID(%s) after the refused delete: %v", row.ID, err)
	}
	if got.State != ObjectStateDeleting {
		t.Errorf("row %s state = %q, want %q -- the mark commits even when the store is missing",
			row.ID, got.State, ObjectStateDeleting)
	}
	if len(bus.events) != 0 {
		t.Errorf("events = %d, want none -- no store, no deletion announced", len(bus.events))
	}

	// The store arrives (a host wiring fix); the protocol resumes from the
	// mark and finishes.
	life.host = &fakeHost{store: store, bus: bus}
	if err := life.Delete(ctx, row.ID); err != nil {
		t.Fatalf("Delete(%s) after the store arrived: %v", row.ID, err)
	}
	assertObjectGone(t, life.objects, ctx, row.ID)
	if _, ok := store.bytes(row.Key); ok {
		t.Errorf("bytes still under %q after the resumed delete", row.Key)
	}
	if len(bus.events) != 1 {
		t.Errorf("events = %d, want exactly one after the resumed delete", len(bus.events))
	}
}

// TestLifecycleService_Delete_StoreFailureLeavesTheProtocolResumable pins
// the crash-convergent shape between two byte deletions: the store dies
// after the original bytes are gone but before the derivative's, and the
// row must be left in deleting -- with the surviving derivative bytes and
// the derivative row intact -- for the next run to finish, removing the
// remaining bytes and announcing the deletion exactly once.
func TestLifecycleService_Delete_StoreFailureLeavesTheProtocolResumable(t *testing.T) {
	life, _, _, store, _, bus := newCleanupHarness(t)
	ctx := serviceCtx("tenant-a")
	row := seedCompletedWithBytes(t, life, store, "obj-a", "tenant-a")
	seedDerivativeBytes(t, life, store, row.ID, "tenant-a")
	thumbKey := thumbKey(t, "tenant-a", row.ID)

	// The protocol's store calls run original-bytes first, then the
	// derivative's; failing the second call is the crash between two byte
	// deletions the protocol must survive.
	failing := &failStore{fakeStore: store, failErr: errors.New("store on fire"), failOn: 2}
	life.host = &fakeHost{store: failing, bus: bus}

	err := life.Delete(ctx, row.ID)
	assertCode(t, err, ErrStoreError.Code)

	// The original bytes were already removed before the store died; the
	// row is marked deleting, the derivative row and its bytes survive, and
	// nothing was announced.
	if _, ok := store.bytes(row.Key); ok {
		t.Errorf("original bytes still under %q after the failed delete", row.Key)
	}
	got, err := life.objects.FindByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("FindByID(%s) after the failed delete: %v", row.ID, err)
	}
	if got.State != ObjectStateDeleting {
		t.Errorf("row %s state = %q, want %q", row.ID, got.State, ObjectStateDeleting)
	}
	if _, ok := store.bytes(thumbKey); !ok {
		t.Errorf("derivative bytes vanished while the protocol failed before reaching them")
	}
	derivatives, err := life.derivatives.listByObject(ctx, row.ID)
	if err != nil {
		t.Fatalf("listByObject(%s): %v", row.ID, err)
	}
	if len(derivatives) != 1 {
		t.Errorf("object %s has %d derivative rows after the failed delete, want 1", row.ID, len(derivatives))
	}
	if len(bus.events) != 0 {
		t.Errorf("events = %d, want none after the failed delete", len(bus.events))
	}

	// The store recovers; the next run resumes from the mark, removes the
	// surviving derivative bytes and rows, and announces the deletion once.
	life.host = &fakeHost{store: store, bus: bus}
	if err := life.Delete(ctx, row.ID); err != nil {
		t.Fatalf("Delete(%s) after the store recovered: %v", row.ID, err)
	}
	assertObjectGone(t, life.objects, ctx, row.ID)
	if _, ok := store.bytes(thumbKey); ok {
		t.Errorf("derivative bytes still under %q after the resumed delete", thumbKey)
	}
	if len(bus.events) != 1 {
		t.Errorf("events = %d, want exactly one after the resumed delete", len(bus.events))
	}
}

// TestLifecycleService_Delete_AnnouncesWarnAndStandWhenTheBusFails pins the
// announcement's warn-and-stand shape: the deletion is durable before the
// bus is touched, so a bus that refuses the event must not fail the delete
// -- the caller gets nil and the row is gone. (The warning itself goes to
// the discard logger serviceCtx carries; the observable contract is that
// the delete succeeds.)
func TestLifecycleService_Delete_AnnouncesWarnAndStandWhenTheBusFails(t *testing.T) {
	life, _, _, store, _, bus := newCleanupHarness(t)
	ctx := serviceCtx("tenant-a")
	row := seedCompletedWithBytes(t, life, store, "obj-a", "tenant-a")
	bus.fail = errors.New("bus on fire")

	if err := life.Delete(ctx, row.ID); err != nil {
		t.Fatalf("Delete(%s) with a failing bus: %v", row.ID, err)
	}
	assertObjectGone(t, life.objects, ctx, row.ID)
	if len(bus.events) != 0 {
		t.Errorf("events = %d, want none -- the failing bus recorded nothing", len(bus.events))
	}
}

// TestLifecycleService_Sweep_ResumesInterruptedDeletions pins phase 1:
// rows in deleting exist only because a Delete did not finish, and a sweep
// re-runs the protocol over each of them -- removing the bytes that
// survive, removing the rows, and announcing each deletion exactly once.
func TestLifecycleService_Sweep_ResumesInterruptedDeletions(t *testing.T) {
	life, _, _, store, _, bus := newCleanupHarness(t)
	ctx := serviceCtx("tenant-a")

	// Two interrupted deletions: one whose original bytes survived the
	// crash, one whose bytes were already removed.
	d1 := newCompleted("del-1", "tenant-a", time.Now().Add(-3*time.Hour))
	d1.State = ObjectStateDeleting
	seedObject(t, life.objects, ctx, d1)
	store.objects[d1.Key] = bytes.Repeat([]byte{0xAB}, 64)
	d2 := newCompleted("del-2", "tenant-a", time.Now().Add(-2*time.Hour))
	d2.State = ObjectStateDeleting
	seedObject(t, life.objects, ctx, d2)

	if err := life.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	assertObjectGone(t, life.objects, ctx, d1.ID)
	assertObjectGone(t, life.objects, ctx, d2.ID)
	if _, ok := store.bytes(d1.Key); ok {
		t.Errorf("bytes still under %q after the sweep resumed the deletion", d1.Key)
	}
	if len(bus.events) != 2 {
		t.Fatalf("events = %d, want two -- one per resumed deletion", len(bus.events))
	}
	for i, want := range []string{d1.ID, d2.ID} {
		evt := bus.events[i]
		if evt.Type != EventObjectDeleted {
			t.Errorf("event %d type = %q, want %q", i, evt.Type, EventObjectDeleted)
			continue
		}
		payload := evt.Payload.(ObjectDeletedPayload)
		if payload.ObjectID != want {
			t.Errorf("event %d payload = %+v, want object %s (the sweep's deterministic order)", i, payload, want)
		}
	}
}

// TestLifecycleService_Sweep_ReclaimsExpiredUploadsSilently pins phase 2:
// uploading rows whose window closed are reclaimed -- rows and bytes
// removed, no event, because nothing ever read them -- while an upload
// whose window is still open is left to the transfer runtime.
func TestLifecycleService_Sweep_ReclaimsExpiredUploadsSilently(t *testing.T) {
	life, _, _, store, _, bus := newCleanupHarness(t)
	ctx := serviceCtx("tenant-a")

	u1 := newUpload("up-1", "tenant-a", time.Now().Add(-31*time.Minute))
	seedObject(t, life.objects, ctx, u1)
	store.objects[u1.Key] = bytes.Repeat([]byte{0x11}, 32)
	u2 := newUpload("up-2", "tenant-a", time.Now().Add(-45*time.Minute))
	seedObject(t, life.objects, ctx, u2)
	u3 := newUpload("up-3", "tenant-a", time.Now())
	seedObject(t, life.objects, ctx, u3)
	store.objects[u3.Key] = bytes.Repeat([]byte{0x22}, 32)

	if err := life.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	assertObjectGone(t, life.objects, ctx, u1.ID)
	assertObjectGone(t, life.objects, ctx, u2.ID)
	if _, ok := store.bytes(u1.Key); ok {
		t.Errorf("bytes still under %q after the reclaim", u1.Key)
	}
	if _, ok := store.bytes(u2.Key); ok {
		t.Errorf("bytes still under %q after the reclaim", u2.Key)
	}

	// The live upload is untouched: the window is still open, so its
	// declaration belongs to the transfer runtime.
	got, err := life.objects.FindByID(ctx, u3.ID)
	if err != nil {
		t.Fatalf("FindByID(%s): %v", u3.ID, err)
	}
	if got.State != ObjectStateUploading {
		t.Errorf("row %s state = %q, want %q", u3.ID, got.State, ObjectStateUploading)
	}
	if _, ok := store.bytes(u3.Key); !ok {
		t.Errorf("live upload bytes under %q vanished", u3.Key)
	}
	if len(bus.events) != 0 {
		t.Errorf("events = %d, want none -- reclaiming never-completed uploads announces nothing", len(bus.events))
	}
}

// TestLifecycleService_Sweep_DeletesExpiredCompletedObjects pins phase 3:
// completed objects whose retention deadline passed are deleted through the
// full protocol -- bytes, rows, event -- while one still inside its
// retention and one that never expires survive untouched.
func TestLifecycleService_Sweep_DeletesExpiredCompletedObjects(t *testing.T) {
	life, _, _, store, _, bus := newCleanupHarness(t)
	ctx := serviceCtx("tenant-a")

	expired := newCompleted("exp-1", "tenant-a", time.Now().Add(-3*time.Hour))
	past := time.Now().Add(-time.Hour)
	expired.ExpiresAt = &past
	seedObject(t, life.objects, ctx, expired)
	store.objects[expired.Key] = bytes.Repeat([]byte{0xAB}, 64)

	live := newCompleted("live-1", "tenant-a", time.Now().Add(-2*time.Hour))
	future := time.Now().Add(time.Hour)
	live.ExpiresAt = &future
	seedObject(t, life.objects, ctx, live)
	store.objects[live.Key] = bytes.Repeat([]byte{0xCD}, 64)

	never := newCompleted("never-1", "tenant-a", time.Now().Add(-2*time.Hour))
	seedObject(t, life.objects, ctx, never)
	store.objects[never.Key] = bytes.Repeat([]byte{0xEF}, 64)

	if err := life.Sweep(ctx); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	assertObjectGone(t, life.objects, ctx, expired.ID)
	if _, ok := store.bytes(expired.Key); ok {
		t.Errorf("bytes still under %q after the expiry delete", expired.Key)
	}
	for _, kept := range []Object{live, never} {
		got, err := life.objects.FindByID(ctx, kept.ID)
		if err != nil {
			t.Fatalf("FindByID(%s): %v", kept.ID, err)
		}
		if got.State != ObjectStateCompleted {
			t.Errorf("row %s state = %q, want %q -- it was not due for expiry", kept.ID, got.State, ObjectStateCompleted)
		}
		if _, ok := store.bytes(kept.Key); !ok {
			t.Errorf("bytes under %q vanished although the object was not due for expiry", kept.Key)
		}
	}
	if len(bus.events) != 1 {
		t.Fatalf("events = %d, want exactly one -- the expired object's", len(bus.events))
	}
	payload, ok := bus.events[0].Payload.(ObjectDeletedPayload)
	if !ok || payload.ObjectID != expired.ID {
		t.Errorf("event payload = %+v, want object %s", bus.events[0].Payload, expired.ID)
	}
}

// TestLifecycleService_Sweep_FailsFastAndLetsTheNextRunFinish pins the
// sweep's fail-fast contract: a store failure on one expired object stops
// the pass with that row left in deleting -- bytes intact -- and the rows
// later in the listing untouched; the next run finishes both, announcing
// each deletion exactly once.
func TestLifecycleService_Sweep_FailsFastAndLetsTheNextRunFinish(t *testing.T) {
	life, _, _, store, _, bus := newCleanupHarness(t)
	ctx := serviceCtx("tenant-a")

	// Two expired completed objects; the listing order is created_at ASC,
	// so a = the earlier one fails first.
	a := newCompleted("exp-a", "tenant-a", time.Now().Add(-3*time.Hour))
	past := time.Now().Add(-2 * time.Hour)
	a.ExpiresAt = &past
	seedObject(t, life.objects, ctx, a)
	store.objects[a.Key] = bytes.Repeat([]byte{0xAB}, 64)
	b := newCompleted("exp-b", "tenant-a", time.Now().Add(-2*time.Hour))
	b.ExpiresAt = &past
	seedObject(t, life.objects, ctx, b)
	store.objects[b.Key] = bytes.Repeat([]byte{0xCD}, 64)

	// The sweep's first store call is a's byte removal; failing it is the
	// fail-fast crash this test pins.
	failing := &failStore{fakeStore: store, failErr: errors.New("store on fire"), failOn: 1}
	life.host = &fakeHost{store: failing, bus: bus}

	err := life.Sweep(ctx)
	assertCode(t, err, ErrStoreError.Code)

	// The first object's delete failed at its byte removal: marked deleting,
	// bytes intact. The second was never reached.
	got, err := life.objects.FindByID(ctx, a.ID)
	if err != nil {
		t.Fatalf("FindByID(%s): %v", a.ID, err)
	}
	if got.State != ObjectStateDeleting {
		t.Errorf("row %s state = %q, want %q after its byte removal failed", a.ID, got.State, ObjectStateDeleting)
	}
	if _, ok := store.bytes(a.Key); !ok {
		t.Errorf("bytes under %q vanished although the store failed before removing them", a.Key)
	}
	gotB, err := life.objects.FindByID(ctx, b.ID)
	if err != nil {
		t.Fatalf("FindByID(%s): %v", b.ID, err)
	}
	if gotB.State != ObjectStateCompleted {
		t.Errorf("row %s state = %q, want %q -- the failed pass must not touch rows after the failing one", b.ID, gotB.State, ObjectStateCompleted)
	}

	// The store recovers; the next run resumes a's deletion and finishes
	// b's, announcing both.
	life.host = &fakeHost{store: store, bus: bus}
	if err := life.Sweep(ctx); err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	assertObjectGone(t, life.objects, ctx, a.ID)
	assertObjectGone(t, life.objects, ctx, b.ID)
	if _, ok := store.bytes(a.Key); ok {
		t.Errorf("bytes still under %q after the second sweep", a.Key)
	}
	if _, ok := store.bytes(b.Key); ok {
		t.Errorf("bytes still under %q after the second sweep", b.Key)
	}
	if len(bus.events) != 2 {
		t.Errorf("events = %d, want two -- one per deletion the second sweep finished", len(bus.events))
	}
}

// TestLifecycleService_EnqueueExpirySweep_ShapesTheTask pins what the
// schedule point puts on the queue: one task of the expiry-sweep type for
// the tenant in context, with no payload and with the per-tenant
// idempotency key that collapses concurrent enqueues into one job.
func TestLifecycleService_EnqueueExpirySweep_ShapesTheTask(t *testing.T) {
	life, _, _, _, queue, _ := newCleanupHarness(t)
	ctx := serviceCtx("tenant-a")

	if err := life.EnqueueExpirySweep(ctx); err != nil {
		t.Fatalf("EnqueueExpirySweep: %v", err)
	}
	if len(queue.tasks) != 1 {
		t.Fatalf("tasks = %d, want exactly one", len(queue.tasks))
	}
	task := queue.tasks[0]
	if task.Type != taskTypeExpirySweep {
		t.Errorf("task type = %q, want %q", task.Type, taskTypeExpirySweep)
	}
	if task.TenantID != pkgcore.TenantID("tenant-a") {
		t.Errorf("task tenant = %q, want %q -- taken from the context", task.TenantID, "tenant-a")
	}
	if task.Payload != nil {
		t.Errorf("task payload = %v, want nil -- the sweep reads the rows and the clock when it runs", task.Payload)
	}
	if task.IdempotencyKey != expirySweepIdempotencyKey("tenant-a") {
		t.Errorf("idempotency key = %q, want %q", task.IdempotencyKey, expirySweepIdempotencyKey("tenant-a"))
	}
}

// TestLifecycleService_EnqueueExpirySweep_NoTenantInContextFails pins the
// schedule point's one refusal: a tenant-less context is a wiring error --
// nothing in this service may guess a tenant -- and reports
// storage.internal_error with the context failure as the cause.
func TestLifecycleService_EnqueueExpirySweep_NoTenantInContextFails(t *testing.T) {
	life, _, _, _, _, _ := newCleanupHarness(t)
	err := life.EnqueueExpirySweep(context.Background())
	assertCode(t, err, ErrInternal.Code)
}

// TestLifecycleService_EnqueueExpirySweep_NoQueueIsAPlainError pins the
// no-queue answer: sweeping is optional work, so a host that wired no queue
// gets a plain error from the enqueue point -- not the module's
// boot-refusing queue requirement, which is about work the module already
// promised.
func TestLifecycleService_EnqueueExpirySweep_NoQueueIsAPlainError(t *testing.T) {
	life, _, _, _, _, _ := newCleanupHarness(t)
	life.queue = nil
	err := life.EnqueueExpirySweep(serviceCtx("tenant-a"))
	if err == nil || !strings.Contains(err.Error(), "no queue wired") {
		t.Errorf("EnqueueExpirySweep without a queue = %v, want the no-queue-wired error", err)
	}
}

// TestExpirySweepHandler_EmptyPayloadRunsTheSweep proves the handler on the
// happy path: a payload-less task runs a full sweep on the tenant context
// the worker rebuilt, succeeding with an empty result.
func TestExpirySweepHandler_EmptyPayloadRunsTheSweep(t *testing.T) {
	life, _, _, _, _, _ := newCleanupHarness(t)
	ctx := serviceCtx("tenant-a")
	// An expired upload and an expired completed object make the sweep do
	// real work, so the handler is proven against a non-empty pass.
	u := newUpload("up-1", "tenant-a", time.Now().Add(-31*time.Minute))
	seedObject(t, life.objects, ctx, u)
	c := newCompleted("exp-1", "tenant-a", time.Now().Add(-3*time.Hour))
	past := time.Now().Add(-time.Hour)
	c.ExpiresAt = &past
	seedObject(t, life.objects, ctx, c)

	h := expirySweepHandler{svc: life}
	result, err := h.Handle(ctx, &jobs.Job{
		Type:     taskTypeExpirySweep,
		TenantID: "tenant-a",
	}, nil)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if result.Data != nil {
		t.Errorf("result = %+v, want the empty result", result)
	}
	assertObjectGone(t, life.objects, ctx, u.ID)
	assertObjectGone(t, life.objects, ctx, c.ID)
}

// TestExpirySweepHandler_RejectsAPayload pins the task-shape rule: a sweep
// takes its inputs from the rows and the clock at run time, so a task
// carrying a payload has nothing to say and fails the job -- it can never
// succeed by re-running.
func TestExpirySweepHandler_RejectsAPayload(t *testing.T) {
	life, _, _, _, _, _ := newCleanupHarness(t)
	h := expirySweepHandler{svc: life}
	_, err := h.Handle(serviceCtx("tenant-a"), &jobs.Job{
		Type:     taskTypeExpirySweep,
		TenantID: "tenant-a",
		Payload:  []byte(`{"unexpected": true}`),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "unexpected payload") {
		t.Errorf("Handle with a payload = %v, want the unexpected-payload error", err)
	}
}
