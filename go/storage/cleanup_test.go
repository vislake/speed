package storage

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

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

// TestLifecycleService_Sweep_OneRowSFailureDoesNotStarveThePass pins the
// sweep's partial-failure contract, the shape compliance's SweepTenant sets
// for its own participants: one object whose deletion fails permanently
// must not starve the rest of the tenant's expiry pass. The sweep records
// the failing row (its protocol stays resumable -- the row sits in deleting
// with its bytes intact for a later run) and still runs every row that
// follows it, then answers the coded partial-failure error so the caller
// knows the pass was not clean. Before this contract the sweep failed fast:
// the first refusing row stopped the pass, and since the listing order is
// deterministic a row that failed on every pass starved every row after it
// indefinitely -- later expiries that a plow-through pass would have
// converged sat un-reaped forever behind the poison row.
func TestLifecycleService_Sweep_OneRowSFailureDoesNotStarveThePass(t *testing.T) {
	life, _, _, store, _, bus := newCleanupHarness(t)
	ctx := serviceCtx("tenant-a")

	// Two expired completed objects; the listing order is created_at ASC,
	// so a = the earlier one is the poison row.
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
	// poison this test pins. The failure is permanent: every pass re-lists a
	// first and every pass would hit the same refusal.
	failing := &failStore{fakeStore: store, failErr: errors.New("store on fire"), failOn: 1}
	life.host = &fakeHost{store: failing, bus: bus}

	err := life.Sweep(ctx)
	assertCode(t, err, ErrSweepPartialFailure.Code)

	// The poison row's delete failed at its byte removal: marked deleting,
	// bytes intact -- the state the next run resumes. The row after it was
	// still swept in the same pass: a healthy object's expiry never waits on
	// a poisoned neighbour, and b's deletion announced itself.
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
	assertObjectGone(t, life.objects, ctx, b.ID)
	if _, ok := store.bytes(b.Key); ok {
		t.Errorf("bytes still under %q although b's expiry delete ran in the same pass", b.Key)
	}
	if len(bus.events) != 1 {
		t.Errorf("events = %d, want exactly one -- b's deletion, committed in the same pass as a's failure", len(bus.events))
	}
	payload, ok := bus.events[0].Payload.(ObjectDeletedPayload)
	if !ok || payload.ObjectID != b.ID {
		t.Errorf("event payload = %+v, want object %s", bus.events[0].Payload, b.ID)
	}

	// The store recovers; the next run resumes a's deletion and converges,
	// announcing it exactly once more.
	life.host = &fakeHost{store: store, bus: bus}
	if err := life.Sweep(ctx); err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	assertObjectGone(t, life.objects, ctx, a.ID)
	if _, ok := store.bytes(a.Key); ok {
		t.Errorf("bytes still under %q after the second sweep", a.Key)
	}
	if len(bus.events) != 2 {
		t.Errorf("events = %d, want two -- one per deletion", len(bus.events))
	}
}

// TestLifecycleService_EnqueueExpirySweep_ShapesTheTask pins what the
// schedule point puts on the queue: one task of the expiry-sweep type for
// the tenant in context, with no payload and with the window-scoped
// idempotency key that collapses one expirySweepWindowSize window's
// concurrent enqueues into one job -- the enqueue's key naming the window
// (jobs.ScheduleWindowStart) its clock places it in.
func TestLifecycleService_EnqueueExpirySweep_ShapesTheTask(t *testing.T) {
	life, _, _, _, queue, _ := newCleanupHarness(t)
	now := time.Date(2026, 9, 7, 10, 30, 0, 0, time.UTC)
	life.now = func() time.Time { return now }
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
	want := "storage.sweep:tenant-a:2026-09-07T10:00:00Z"
	if task.IdempotencyKey != want {
		t.Errorf("idempotency key = %q, want %q (the enqueue's own window, not a tenant-only key)", task.IdempotencyKey, want)
	}
}

// TestExpirySweepKeyMatchesTheSchedulerDerivation pins the schedule
// migration's key identity: the same (task type, tenant, window) must
// resolve one idempotency key through the module's own schedule point and
// through the jobs.Scheduler's derivation over the module's declaration,
// or a scheduler tick and a manual enqueue landing in one window would
// run the sweep twice. The key literals below are the pinned strings.
func TestExpirySweepKeyMatchesTheSchedulerDerivation(t *testing.T) {
	windowStart := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)

	// Path one: the module's own schedule point, through the real enqueue.
	life, _, _, _, queue, _ := newCleanupHarness(t)
	life.now = func() time.Time { return windowStart.Add(30 * time.Minute) }
	if err := life.EnqueueExpirySweep(serviceCtx("tenant-a")); err != nil {
		t.Fatalf("EnqueueExpirySweep: %v", err)
	}
	if len(queue.tasks) != 1 {
		t.Fatalf("tasks = %d, want exactly one", len(queue.tasks))
	}
	manual := queue.tasks[0].IdempotencyKey
	if want := "storage.sweep:tenant-a:2026-09-07T10:00:00Z"; manual != want {
		t.Fatalf("the manual path resolved key %q, want the pinned %q", manual, want)
	}

	// Path two: the scheduler's own derivation over the declaration, for
	// the same (tenant, window).
	decl := expirySweepSchedule
	if decl.Type != taskTypeExpirySweep || decl.Every != expirySweepWindowSize {
		t.Errorf("declaration = %+v, want the site's own type %q and window %s", decl, taskTypeExpirySweep, expirySweepWindowSize)
	}
	if decl.Scope != pkgcore.PeriodicScopePerTenant {
		t.Errorf("declaration scope = %q, want %q", decl.Scope, pkgcore.PeriodicScopePerTenant)
	}
	if got := jobs.ScheduleIdempotencyKey(decl.KeyPrefix, pkgcore.TenantID("tenant-a"), windowStart); got != manual {
		t.Errorf("the scheduler-derived key %q != the manual key %q -- one window would run twice", got, manual)
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

	h := jobs.NewEmptyPayloadHandler(taskTypeExpirySweep, life.Sweep)
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
	h := jobs.NewEmptyPayloadHandler(taskTypeExpirySweep, life.Sweep)
	_, err := h.Handle(serviceCtx("tenant-a"), &jobs.Job{
		Type:     taskTypeExpirySweep,
		TenantID: "tenant-a",
		Payload:  []byte(`{"unexpected": true}`),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "unexpected payload") {
		t.Errorf("Handle with a payload = %v, want the unexpected-payload error", err)
	}
}

// The tests below pin the window semantics of the expiry-sweep
// idempotency key (jobs.ScheduleIdempotencyKey): enqueues inside one
// expirySweepWindowSize
// window collapse into one job (the concurrency protection the key exists
// for, preserved), enqueues in a later window become new jobs and sweep
// again (periodicity), and a sweep job that dead-letters poisons only its
// own window, never its tenant's later windows. All three run against a
// REAL jobs.StandaloneQueue over a real SQLite database -- the dedupe
// behaviour under test lives in jobs' partial unique index and row
// semantics, which a fake queue cannot exercise. Tests (b) and (c) fail
// under a tenant-only key (no window): the later enqueue returns the first
// job's id and no second sweep ever runs.
//
// EnqueueExpirySweep returns no job id (it is a fire-and-forget schedule
// point), so the tests observe the queue's own database -- the same
// *gorm.DB the queue was started over -- for row counts and ids, plus the
// registered handler's run channel for executions.

// startSweepWindowQueue starts a real StandaloneQueue over its own fresh
// database with fast intervals, registering cleanup, and returns both the
// queue and its database.
func startSweepWindowQueue(t *testing.T) (*jobs.StandaloneQueue, *gorm.DB) {
	t.Helper()
	db := newTestDB(t)
	q := jobs.NewStandaloneQueue(db,
		jobs.WithPollInterval(5*time.Millisecond),
		jobs.WithWorkerCount(1),
		jobs.WithBackoff(5*time.Millisecond, 50*time.Millisecond),
	)
	if err := q.Start(context.Background()); err != nil {
		t.Fatalf("StandaloneQueue.Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := q.Close(ctx); err != nil {
			t.Errorf("StandaloneQueue.Close() error = %v", err)
		}
	})
	return q, db
}

// windowA and windowB are two points in time in two different
// expirySweepWindowSize windows (10:15 and 11:15 UTC), windowB exactly one
// window later than windowA.
var (
	windowA = time.Date(2026, 9, 7, 10, 15, 0, 0, time.UTC)
	windowB = windowA.Add(expirySweepWindowSize)
)

// sweepRowCount counts the expiry-sweep rows in the queue's own database:
// one per idempotency-key window resolved, whether the row is pending,
// running or already settled.
func sweepRowCount(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	if err := db.Table("jobs").Where("type = ?", taskTypeExpirySweep).Count(&n).Error; err != nil {
		t.Fatalf("count expiry-sweep rows: %v", err)
	}
	return n
}

// firstSweepJobID returns the id of the database's only expiry-sweep row.
func firstSweepJobID(t *testing.T, db *gorm.DB) jobs.JobID {
	t.Helper()
	if n := sweepRowCount(t, db); n != 1 {
		t.Fatalf("expiry-sweep rows = %d, want exactly 1 before reading the first job id", n)
	}
	var ids []string
	if err := db.Table("jobs").Where("type = ?", taskTypeExpirySweep).Pluck("id", &ids).Error; err != nil {
		t.Fatalf("read first expiry-sweep job id: %v", err)
	}
	return jobs.JobID(ids[0])
}

// waitForRun waits until a sweep handler run lands on runs and returns its
// job id, failing the test after timeout. runs must be a buffered channel
// the handler fills once per Handle call.
func waitForRun(t *testing.T, runs chan jobs.JobID, what string) jobs.JobID {
	t.Helper()
	select {
	case id := <-runs:
		return id
	case <-time.After(20 * time.Second):
		t.Fatalf("%s: no sweep run within 20s", what)
		return ""
	}
}

// TestEnqueueExpirySweep_SameWindowEnqueuesCollapseIntoOneJob pins
// regression (a): the concurrency protection the sweep key exists for must
// survive the windowing -- two enqueues for one tenant inside the same
// expirySweepWindowSize window collapse into the first job (the real
// queue's idempotent Enqueue resolves the key to the existing row: one
// row, one run), so two scheduler replicas ticking in one window still
// never sweep the tenant twice at once.
func TestEnqueueExpirySweep_SameWindowEnqueuesCollapseIntoOneJob(t *testing.T) {
	life, _, _, _, _, _ := newCleanupHarness(t)
	q, db := startSweepWindowQueue(t)
	life.queue = q
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypeExpirySweep, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := serviceCtx("tenant-a")
	life.now = func() time.Time { return windowA }
	if err := life.EnqueueExpirySweep(ctx); err != nil {
		t.Fatalf("first EnqueueExpirySweep: %v", err)
	}
	// A second replica's tick ten minutes later -- still inside windowA's
	// expirySweepWindowSize window.
	life.now = func() time.Time { return windowA.Add(10 * time.Minute) }
	if err := life.EnqueueExpirySweep(ctx); err != nil {
		t.Fatalf("second EnqueueExpirySweep: %v", err)
	}

	if n := sweepRowCount(t, db); n != 1 {
		t.Fatalf("expiry-sweep rows = %d, want 1 -- a same-window duplicate enqueue must resolve the first job, never insert a second row", n)
	}
	first := waitForRun(t, runs, "the collapsed sweep")
	if first != firstSweepJobID(t, db) {
		t.Errorf("sweep run job id = %s, want the row's id %s", first, firstSweepJobID(t, db))
	}
	// Exactly one run: the collapse produced one job, so no second run may
	// ever arrive.
	select {
	case extra := <-runs:
		t.Errorf("sweep ran a second time (job %s) after the same-window collapse", extra)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestEnqueueExpirySweep_LaterWindowEnqueuesNewJobAndSweepsAgain pins
// regression (b): an enqueue in a later window is a NEW job and the sweep
// runs again. Fails under a tenant-only key (no window), where the later
// enqueue resolves the first job's id -- the first-ever sweep's permanent
// dedupe -- so no second row is ever created and nothing ever runs again.
func TestEnqueueExpirySweep_LaterWindowEnqueuesNewJobAndSweepsAgain(t *testing.T) {
	life, _, _, _, _, _ := newCleanupHarness(t)
	q, db := startSweepWindowQueue(t)
	life.queue = q
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypeExpirySweep, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := serviceCtx("tenant-a")
	life.now = func() time.Time { return windowA }
	if err := life.EnqueueExpirySweep(ctx); err != nil {
		t.Fatalf("first EnqueueExpirySweep: %v", err)
	}
	first := waitForRun(t, runs, "window A's sweep")

	// The scheduler's tick an hour later: windowB, a different window.
	life.now = func() time.Time { return windowB }
	if err := life.EnqueueExpirySweep(ctx); err != nil {
		t.Fatalf("second EnqueueExpirySweep: %v", err)
	}

	if n := sweepRowCount(t, db); n != 2 {
		t.Fatalf("expiry-sweep rows = %d, want 2 -- the later window's enqueue must create a NEW job (fails on the tenant-only key, which resolves the first row forever)", n)
	}
	second := waitForRun(t, runs, "window B's sweep")
	if second == first {
		t.Errorf("window B's run job id = %s, the same as window A's -- the later-window enqueue must run its own sweep", second)
	}
}

// TestEnqueueExpirySweep_DeadLetteredWindowDoesNotPoisonLaterOnes pins
// regression (c): a sweep job that dead-letters poisons only its own
// window. Fails under a tenant-only key (no window), where the dead job's
// idempotency key stays resolved forever -- every later enqueue returns the
// dead job's id, no second row is ever created and the tenant is never
// swept again.
func TestEnqueueExpirySweep_DeadLetteredWindowDoesNotPoisonLaterOnes(t *testing.T) {
	life, _, _, _, _, _ := newCleanupHarness(t)
	q, db := startSweepWindowQueue(t)
	life.queue = q

	// succeed is flipped only after window A's job has dead-lettered; until
	// then every Handle fails permanently, which is what dead-letters it
	// (with DefaultMaxRetries 3, the fourth attempt exhausts the budget).
	var succeed bool
	runs := make(chan jobs.JobID, 4)
	if err := q.RegisterHandler(jobs.NewHandlerFunc(taskTypeExpirySweep, func(_ context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
		if !succeed {
			return jobs.Result{}, errors.New("storage: injected sweep failure")
		}
		runs <- job.ID
		return jobs.Result{}, nil
	})); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	ctx := serviceCtx("tenant-a")
	getCtx := tenantCtx(pkgcore.TenantID("tenant-a"))
	life.now = func() time.Time { return windowA }
	if err := life.EnqueueExpirySweep(ctx); err != nil {
		t.Fatalf("first EnqueueExpirySweep: %v", err)
	}
	first := firstSweepJobID(t, db)

	// Window A's sweep exhausts its retries and dead-letters. Wait for the
	// terminal state rather than counting attempts: the worker may have
	// claimed the row before or after any particular write, but the
	// always-failing handler guarantees the terminal state either way.
	deadline := time.Now().Add(20 * time.Second)
	for {
		job, err := q.Get(getCtx, first)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", first, err)
		}
		if job.Status == jobs.StatusDeadLetter {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("window A's sweep never dead-lettered within 20s (status %v)", job.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The next window's enqueue must run its own sweep, whatever happened
	// to window A's.
	succeed = true
	life.now = func() time.Time { return windowB }
	if err := life.EnqueueExpirySweep(ctx); err != nil {
		t.Fatalf("second EnqueueExpirySweep: %v", err)
	}

	if n := sweepRowCount(t, db); n != 2 {
		t.Fatalf("expiry-sweep rows = %d, want 2 -- the dead-lettered window's key must not keep resolving for later windows (fails on the tenant-only key)", n)
	}
	second := waitForRun(t, runs, "window B's sweep after window A dead-lettered")
	if second == first {
		t.Errorf("window B's run job id = %s, the same as window A's dead-lettered job -- a dead-lettered window must not poison the tenant's later windows", second)
	}
}
