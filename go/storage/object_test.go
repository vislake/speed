package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/storage/internal/testutil"
)

// This file tests ObjectService, the upload-transfer runtime: Create, Upload,
// Complete, Get, OpenContent and List driven end to end over the real
// repository (so every test is also a proof the migrations apply) against
// file-local fakes standing in for the host's infrastructure -- the ObjectStore
// and EventBus the registry would have resolved (fakeHost), and the jobs.Queue
// Register would have required (recordingQueue). The fakes are inspectable, so
// a test can assert exactly what reached the store (bytes under which key), the
// queue (which task, with which tenant and idempotency key) and the bus (which
// event, with which payload). The revalidation checks themselves are pinned
// in validate_test.go and sanitize_test.go; here they are exercised in the
// pipeline's settled order, as whole lifecycles.
//
// The harness attaches the fakes by assigning svc.host directly: attach() is
// typed to *pkgcore.Registry, and module_test.go proves the real attachment
// through Module.Register. White-box access is deliberate -- the service's
// policy (cfg) and host seams are unexported by design.

// fakeStore is an in-memory pkgcore.ObjectStore whose map a test can inspect
// and seed directly. Its GetObject misses report pkgcore.ErrObjectNotFound
// exactly as the real stores do (errors.Is, never a wrapped variant), and the
// optional putErr/getErr fields make a store-level failure injectable for the
// store_error paths.
type fakeStore struct {
	objects map[string][]byte
	putErr  error
	getErr  error
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}}
}

func (s *fakeStore) PutObject(_ context.Context, key string, r io.Reader) error {
	if s.putErr != nil {
		return s.putErr
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.objects[key] = raw
	return nil
}

func (s *fakeStore) GetObject(_ context.Context, key string) (io.ReadCloser, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	raw, ok := s.objects[key]
	if !ok {
		return nil, pkgcore.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (s *fakeStore) DeleteObject(_ context.Context, key string) error {
	delete(s.objects, key)
	return nil
}

// bytes returns a copy of what the store holds under key, and whether the key
// exists at all -- the assertion primitive for "the bytes landed" and for "the
// rejected attempt cleaned up after itself".
func (s *fakeStore) bytes(key string) ([]byte, bool) {
	raw, ok := s.objects[key]
	return append([]byte(nil), raw...), ok
}

var _ pkgcore.ObjectStore = (*fakeStore)(nil)

// recordingQueue is a jobs.Queue that records every task it accepted, for
// assertions on what the completion pipeline enqueues (type, tenant,
// idempotency key, payload). A non-nil fail makes Enqueue refuse, for the
// warn-and-stand path.
type recordingQueue struct {
	fail  error
	tasks []jobs.Task
}

func (q *recordingQueue) Enqueue(_ context.Context, task jobs.Task, _ ...jobs.EnqueueOption) (jobs.JobID, error) {
	if q.fail != nil {
		return "", q.fail
	}
	q.tasks = append(q.tasks, task)
	return "", nil
}

func (q *recordingQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) { return nil, nil }
func (q *recordingQueue) Cancel(context.Context, jobs.JobID) error           { return nil }

var _ jobs.Queue = (*recordingQueue)(nil)

// recordingBus is a pkgcore.EventBus that records every event published on
// it, for assertions on what the completion pipeline announces. A non-nil
// fail makes Publish refuse, for the warn-and-stand path.
type recordingBus struct {
	fail   error
	events []pkgcore.Event
}

func newRecordingBus() *recordingBus { return &recordingBus{} }

func (b *recordingBus) Publish(_ context.Context, evt pkgcore.Event) error {
	if b.fail != nil {
		return b.fail
	}
	b.events = append(b.events, evt)
	return nil
}

func (b *recordingBus) Subscribe(string, pkgcore.EventHandler) {}

var _ pkgcore.EventBus = (*recordingBus)(nil)

// fakeHost stands in for the bootstrapped *pkgcore.Registry the service reads
// its seams from (hostSeams). attach() is typed to the registry itself; module
// tests prove that path, this file proves the service's behaviour against the
// interface slice it actually depends on.
type fakeHost struct {
	store pkgcore.ObjectStore
	bus   pkgcore.EventBus
}

func (h *fakeHost) ObjectStore() pkgcore.ObjectStore { return h.store }
func (h *fakeHost) EventBus() pkgcore.EventBus       { return h.bus }

var _ hostSeams = (*fakeHost)(nil)

// serviceCtx returns a tenant context carrying a discard logger, so the
// service's structured logging (observability.FromContext, which falls back
// to slog.Default on a logger-less context) is exercised without printing to
// stderr on every Info and Warn.
func serviceCtx(tenant pkgcore.TenantID) context.Context {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return observability.WithLogger(tenantCtx(tenant), logger)
}

// testServiceConfig returns the policy the lifecycle tests run under: a 1 MiB
// upload ceiling (the largest fixture below is a few hundred bytes), generous
// pixel and retention bounds, and the module-default upload window. The
// allowlist is left nil so every test exercises the default-resolution path
// unless it replaces the list on purpose.
func testServiceConfig() serviceConfig {
	return serviceConfig{
		maxUploadBytes:    1 << 20,
		maxImagePixels:    40_000_000,
		uploadTTL:         30 * time.Minute,
		maxObjectLifetime: 90 * 24 * time.Hour,
	}
}

// newTestService returns an ObjectService enforcing testServiceConfig (as
// mutated) over a fresh migrated database, with the host seams attached to
// fresh fakes. Tests drive the lifecycle through the service and assert on
// the fakes; the repository itself stays reachable as svc.objects for
// seeding rows the service may not create (completed rows, expired windows).
func newTestService(t *testing.T, mutate func(*serviceConfig)) (*ObjectService, *fakeStore, *recordingQueue, *recordingBus) {
	t.Helper()
	cfg := testServiceConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	store := newFakeStore()
	queue := &recordingQueue{}
	bus := newRecordingBus()
	svc := newObjectService(NewObjectRepository(newTestDB(t)), queue, cfg)
	svc.host = &fakeHost{store: store, bus: bus}
	return svc, store, queue, bus
}

// createAndUpload drives a full declaration and byte transfer for body: it
// creates the object with the declared type and the body's length, streams
// the body, and returns the created row.
func createAndUpload(t *testing.T, svc *ObjectService, ctx context.Context, body []byte, declaredType string) Object {
	t.Helper()
	row, err := svc.Create(ctx, CreateParams{
		DeclaredSize: int64(len(body)),
		DeclaredType: declaredType,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Upload(ctx, row.ID, nil, bytes.NewReader(body)); err != nil {
		t.Fatalf("Upload(%s): %v", row.ID, err)
	}
	return row
}

// jpegWithExif returns a decodable 48x32 JPEG carrying a genuine EXIF APP1
// segment (a GPS-bearing TIFF profile) -- the canonical carrier of location
// metadata the completion pipeline must strip.
func jpegWithExif(t *testing.T) []byte {
	t.Helper()
	return insertAPPSegment(t, testutil.JPEG(t, 48, 32), jpegApp1, exifPayload())
}

// pngWithExif returns a decodable 32x24 PNG carrying an eXIf chunk holding
// GPS-bearing profile bytes.
func pngWithExif(t *testing.T) []byte {
	t.Helper()
	return insertPNGChunk(t, testutil.PNG(t, 32, 24), string(pngChunkExif),
		[]byte("II*\x00\x08\x00\x00\x00GPSLatitudeRef-travels-here"))
}

// assertParam fails t unless err's apperr decoration carries key with exactly
// the value want.
func assertParam(t *testing.T, err error, key string, want any) {
	t.Helper()
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error %v is not an *apperr.Error", err)
	}
	got, ok := appErr.Params[key]
	if !ok {
		t.Fatalf("error %v (%s) carries no %q parameter", err, appErr.Code, key)
	}
	if got != want {
		t.Fatalf("%s param = %v (%T), want %v (%T)", key, got, got, want, want)
	}
}

// assertStillUploading fails t unless the row is still in ObjectStateUploading
// -- the proof that a refused pipeline step advanced nothing.
func assertStillUploading(t *testing.T, svc *ObjectService, ctx context.Context, id string) {
	t.Helper()
	row, err := svc.objects.FindByID(ctx, id)
	if err != nil {
		t.Fatalf("FindByID(%s) after refusal: %v", id, err)
	}
	if row.State != ObjectStateUploading {
		t.Fatalf("row %s state = %q after refusal, want %q", id, row.State, ObjectStateUploading)
	}
}

func TestObjectService_NewService_ResolvesNilAllowlistToModuleDefault(t *testing.T) {
	svc := newObjectService(NewObjectRepository(newTestDB(t)), nil, serviceConfig{})
	if len(svc.cfg.allowedTypes) != len(defaultAllowedTypes) {
		t.Fatalf("resolved allowlist = %v, want the module default %v", svc.cfg.allowedTypes, defaultAllowedTypes)
	}
	for i := range defaultAllowedTypes {
		if svc.cfg.allowedTypes[i] != defaultAllowedTypes[i] {
			t.Errorf("resolved allowlist[%d] = %q, want %q", i, svc.cfg.allowedTypes[i], defaultAllowedTypes[i])
		}
	}
}

func TestObjectService_Create_ValidatesTheDeclaration(t *testing.T) {
	validChecksum := strings.Repeat("0123456789abcdef", 4)

	cases := []struct {
		name   string
		params CreateParams
		want   *apperr.Error
		key    string
		value  any
	}{
		{
			name:   "zero declared size",
			params: CreateParams{DeclaredSize: 0},
			want:   ErrInvalidSize,
		},
		{
			name:   "negative declared size",
			params: CreateParams{DeclaredSize: -1},
			want:   ErrInvalidSize,
		},
		{
			name:   "declared size over the ceiling",
			params: CreateParams{DeclaredSize: 1<<20 + 1},
			want:   ErrObjectTooLarge,
			key:    "max_bytes",
			value:  int64(1 << 20),
		},
		{
			name:   "malformed checksum",
			params: CreateParams{DeclaredSize: 10, DeclaredChecksum: "not-a-digest"},
			want:   ErrInvalidChecksum,
		},
		{
			name:   "uppercase checksum refused",
			params: CreateParams{DeclaredSize: 10, DeclaredChecksum: strings.ToUpper(validChecksum)},
			want:   ErrInvalidChecksum,
		},
		{
			name:   "declared type outside the allowlist",
			params: CreateParams{DeclaredSize: 10, DeclaredType: "image/gif"},
			want:   ErrTypeNotAllowed,
			key:    "allowed",
			value:  "image/jpeg,image/png",
		},
		{
			name:   "retention past the module maximum",
			params: CreateParams{DeclaredSize: 10, Retention: 91 * 24 * time.Hour},
			want:   ErrInvalidExpiry,
			key:    "max_lifetime_days",
			value:  int64(90),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, _, _ := newTestService(t, nil)
			_, err := svc.Create(serviceCtx("tenant-a"), tc.params)
			assertCode(t, err, tc.want.Code)
			if tc.key != "" {
				assertParam(t, err, tc.key, tc.value)
			}
		})
	}
}

func TestObjectService_Create_ReservesUploadRowAndCanonicalizesTheType(t *testing.T) {
	svc, _, _, _ := newTestService(t, nil)
	ctx := serviceCtx("tenant-a")
	now := time.Now()

	row, err := svc.Create(ctx, CreateParams{
		DeclaredSize: 4096,
		// A free-form declaration (a browser-derived header, parameters and
		// case included) must be stored in its canonical form.
		DeclaredType: "Image/Jpeg;foo=bar",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(row.ID) != objectIDLen {
		t.Errorf("reserved id %q has length %d, want %d (the UUID grammar)", row.ID, len(row.ID), objectIDLen)
	}
	if row.State != ObjectStateUploading {
		t.Errorf("state = %q, want %q", row.State, ObjectStateUploading)
	}
	if row.TenantID != "tenant-a" {
		t.Errorf("tenant = %q, want %q", row.TenantID, "tenant-a")
	}
	wantKey, err := ObjectKey("tenant-a", row.ID)
	if err != nil {
		t.Fatalf("ObjectKey: %v", err)
	}
	if row.Key != wantKey {
		t.Errorf("key = %q, want %q -- the internal grammar, never exposed through an API", row.Key, wantKey)
	}
	if row.DeclaredSize != 4096 {
		t.Errorf("declared size = %d, want 4096", row.DeclaredSize)
	}
	if row.DeclaredType != "image/jpeg" {
		t.Errorf("declared type = %q, want the canonical %q", row.DeclaredType, "image/jpeg")
	}
	if row.DeclaredChecksum != "" {
		t.Errorf("declared checksum = %q, want empty", row.DeclaredChecksum)
	}
	if !row.UploadExpiresAt.After(now) || !row.UploadExpiresAt.Before(now.Add(31*time.Minute)) {
		t.Errorf("upload window expires at %v, want roughly now+30m", row.UploadExpiresAt)
	}
	if row.ExpiresAt != nil {
		t.Errorf("ExpiresAt = %v with no requested retention, want nil", *row.ExpiresAt)
	}

	// A requested retention inside the maximum lands on the row as an expiry;
	// a retention exactly at the maximum is admitted.
	kept, err := svc.Create(ctx, CreateParams{DeclaredSize: 4096, Retention: 10 * 24 * time.Hour})
	if err != nil {
		t.Fatalf("Create with retention: %v", err)
	}
	if kept.ExpiresAt == nil || !kept.ExpiresAt.After(time.Now()) {
		t.Fatalf("ExpiresAt = %v, want a future expiry for the requested retention", kept.ExpiresAt)
	}
	if _, err := svc.Create(ctx, CreateParams{DeclaredSize: 4096, Retention: 90 * 24 * time.Hour}); err != nil {
		t.Fatalf("retention exactly at the maximum refused: %v", err)
	}
}

func TestObjectService_Create_RefusesAContextWithoutTenant(t *testing.T) {
	svc, _, _, _ := newTestService(t, nil)
	_, err := svc.Create(context.Background(), CreateParams{DeclaredSize: 4096})
	if !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Errorf("error = %v, want errors.Is(err, pkgcore.ErrNoTenant)", err)
	}
	assertCode(t, err, ErrInternal.Code)
}

func TestObjectService_Upload_StreamsExactlyTheDeclaredBytes(t *testing.T) {
	svc, store, _, _ := newTestService(t, nil)
	ctx := serviceCtx("tenant-a")
	row, err := svc.Create(ctx, CreateParams{DeclaredSize: 4096})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	body := bytes.Repeat([]byte("01234567"), 512) // exactly 4096 bytes
	if err := svc.Upload(ctx, row.ID, nil, bytes.NewReader(body)); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	stored, ok := store.bytes(row.Key)
	if !ok {
		t.Fatalf("no bytes stored under %s", row.Key)
	}
	if !bytes.Equal(stored, body) {
		t.Fatalf("stored %d bytes, want the %d uploaded", len(stored), len(body))
	}
	// The row itself is untouched by Upload: the declaration still stands
	// until Complete runs the revalidation pipeline.
	assertStillUploading(t, svc, ctx, row.ID)
}

func TestObjectService_Upload_RefusesADifferingContentLength(t *testing.T) {
	svc, store, _, _ := newTestService(t, nil)
	ctx := serviceCtx("tenant-a")
	row, err := svc.Create(ctx, CreateParams{DeclaredSize: 4096})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for _, length := range []int64{0, 4095, 4097, 1 << 20} {
		t.Run(fmt.Sprintf("content-length %d", length), func(t *testing.T) {
			contentLength := length
			err := svc.Upload(ctx, row.ID, &contentLength, bytes.NewReader(bytes.Repeat([]byte("x"), 4096)))
			assertCode(t, err, ErrContentLengthMismatch.Code)
			assertParam(t, err, "declared", int64(4096))
			if _, ok := store.bytes(row.Key); ok {
				t.Fatal("bytes were stored although the transport length contradicted the declaration")
			}
		})
	}
}

func TestObjectService_Upload_RemovesShortOrOversizeBodies(t *testing.T) {
	body := bytes.Repeat([]byte("data"), 1024) // exactly 4096 bytes, the declared size

	cases := []struct {
		name       string
		send       []byte
		wantActual int64
	}{
		{"short body", body[:1000], 1000},
		{"nil body", nil, 0},
		{"oversize body", bytes.Repeat([]byte("x"), 2*len(body)), int64(len(body) + 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, store, _, _ := newTestService(t, nil)
			ctx := serviceCtx("tenant-a")
			row, err := svc.Create(ctx, CreateParams{DeclaredSize: int64(len(body))})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			// No transport-observed length: Upload reconciles by byte count
			// alone, and the bounded read caps the count at declared+1.
			err = svc.Upload(ctx, row.ID, nil, bytes.NewReader(tc.send))
			assertCode(t, err, ErrSizeMismatch.Code)
			assertParam(t, err, "declared", int64(len(body)))
			assertParam(t, err, "actual", tc.wantActual)
			if _, ok := store.bytes(row.Key); ok {
				t.Fatal("rejected body was left behind in the store")
			}
			assertStillUploading(t, svc, ctx, row.ID)
		})
	}
}

// TestObjectService_StateAndWindowGates exercises the refusals that protect
// the upload window and the state machine: a row outside ObjectStateUploading
// is untouchable by Upload and Complete, and a row whose window has passed is
// treated as never-arriving -- content_missing, the same answer Complete
// gives -- even when bytes happen to sit under its key.
func TestObjectService_StateAndWindowGates(t *testing.T) {
	t.Run("upload and complete on a completed row", func(t *testing.T) {
		svc, _, _, _ := newTestService(t, nil)
		ctx := serviceCtx("tenant-a")
		row := createAndUpload(t, svc, ctx, testutil.JPEG(t, 8, 8), "image/jpeg")
		if _, err := svc.Complete(ctx, row.ID); err != nil {
			t.Fatalf("Complete: %v", err)
		}
		err := svc.Upload(ctx, row.ID, nil, bytes.NewReader([]byte("late bytes")))
		assertCode(t, err, ErrObjectNotUploading.Code)
		assertParam(t, err, "id", row.ID)
		if _, err := svc.Complete(ctx, row.ID); !hasCode(err, ErrObjectNotUploading.Code) {
			t.Fatalf("second Complete error = %v, want %s", err, ErrObjectNotUploading.Code)
		}
	})

	t.Run("state gates on a deleting row", func(t *testing.T) {
		svc, _, _, _ := newTestService(t, nil)
		ctx := serviceCtx("tenant-a")
		deleting := newCompleted("obj-deleting", "tenant-a", time.Now())
		deleting.State = ObjectStateDeleting
		seedObject(t, svc.objects, ctx, deleting)
		if err := svc.Upload(ctx, "obj-deleting", nil, nil); !hasCode(err, ErrObjectNotUploading.Code) {
			t.Fatalf("Upload error = %v, want %s", err, ErrObjectNotUploading.Code)
		}
		if _, err := svc.Complete(ctx, "obj-deleting"); !hasCode(err, ErrObjectNotUploading.Code) {
			t.Fatalf("Complete error = %v, want %s", err, ErrObjectNotUploading.Code)
		}
	})

	t.Run("expired window refuses upload and complete, store untouched", func(t *testing.T) {
		svc, store, _, _ := newTestService(t, nil)
		ctx := serviceCtx("tenant-a")
		expired := newUpload("obj-expired", "tenant-a", time.Now().Add(-2*time.Hour))
		seedObject(t, svc.objects, ctx, expired)
		// Bytes that arrived after the window are beside the point: the window
		// governs, and the refusal fires before any store write -- the
		// pre-placed bytes must still be there afterwards.
		store.objects[expired.Key] = []byte("content that outlived its window")
		if err := svc.Upload(ctx, "obj-expired", nil, bytes.NewReader([]byte("late"))); !hasCode(err, ErrContentMissing.Code) {
			t.Fatalf("Upload error = %v, want %s", err, ErrContentMissing.Code)
		}
		if _, err := svc.Complete(ctx, "obj-expired"); !hasCode(err, ErrContentMissing.Code) {
			t.Fatalf("Complete error = %v, want %s", err, ErrContentMissing.Code)
		}
		if _, ok := store.bytes(expired.Key); !ok {
			t.Fatal("the expired row's stored bytes were touched")
		}
	})

	t.Run("unknown id reads as not found", func(t *testing.T) {
		svc, _, _, _ := newTestService(t, nil)
		err := svc.Upload(serviceCtx("tenant-a"), "00000000-0000-4000-8000-000000000000", nil, nil)
		assertCode(t, err, ErrObjectNotFound.Code)
	})
}

func TestObjectService_Complete_RequiresStoredBytes(t *testing.T) {
	svc, _, _, _ := newTestService(t, nil)
	ctx := serviceCtx("tenant-a")
	row, err := svc.Create(ctx, CreateParams{DeclaredSize: 4096, DeclaredType: "image/png"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err = svc.Complete(ctx, row.ID)
	assertCode(t, err, ErrContentMissing.Code)
	assertParam(t, err, "id", row.ID)
	assertStillUploading(t, svc, ctx, row.ID)
}

// TestObjectService_Complete_HappyPath_JpegStripsExifAndFinalizes is the
// transfer lifecycle's spine: a JPEG carrying a GPS-bearing EXIF profile is
// declared, uploaded byte for byte, and completed -- and the completion
// pipeline strips the profile from the stored bytes, keeps the pixels intact,
// and finalizes the row with metadata that describes the bytes actually
// stored: the sanitized length, the sanitized digest, and the decoded
// dimensions.
func TestObjectService_Complete_HappyPath_JpegStripsExifAndFinalizes(t *testing.T) {
	svc, store, _, _ := newTestService(t, nil)
	ctx := serviceCtx("tenant-a")
	base := testutil.JPEG(t, 48, 32)
	withExif := jpegWithExif(t)
	row := createAndUpload(t, svc, ctx, withExif, "image/jpeg")
	if got, _ := store.bytes(row.Key); !bytes.Equal(got, withExif) {
		t.Fatalf("uploaded bytes differ from what was stored")
	}

	completed, err := svc.Complete(ctx, row.ID)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	stored, ok := store.bytes(row.Key)
	if !ok {
		t.Fatal("stored bytes vanished at Complete")
	}
	if bytes.Contains(stored, exifSignature) {
		t.Fatal("exif signature survives the pipeline")
	}
	if len(stored) >= len(withExif) {
		t.Fatalf("sanitized bytes are not shorter than the exif-bearing upload (%d >= %d)", len(stored), len(withExif))
	}
	assertDecodesEqual(t, "service jpeg strip", stored, base)

	if completed.State != ObjectStateCompleted {
		t.Errorf("state = %q, want %q", completed.State, ObjectStateCompleted)
	}
	if completed.MIME == nil || *completed.MIME != "image/jpeg" {
		t.Errorf("mime = %v, want image/jpeg", completed.MIME)
	}
	if completed.Size == nil || *completed.Size != int64(len(stored)) {
		t.Errorf("size = %v, want %d (the sanitized length)", completed.Size, len(stored))
	}
	if completed.Width == nil || *completed.Width != 48 || completed.Height == nil || *completed.Height != 32 {
		t.Errorf("dimensions = %vx%v, want 48x32", completed.Width, completed.Height)
	}
	if completed.ChecksumSHA256 == nil || *completed.ChecksumSHA256 != sha256HexDigest(stored) {
		t.Errorf("digest = %v, want the digest of the stored bytes", completed.ChecksumSHA256)
	}

	// The completed object is now visible through the read side, and the row
	// the metadata repository holds agrees with what the service returned.
	got, err := svc.Get(ctx, row.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != ObjectStateCompleted {
		t.Errorf("Get state = %q", got.State)
	}
	again, err := svc.objects.FindByID(ctx, row.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if again.ChecksumSHA256 == nil || *again.ChecksumSHA256 != *completed.ChecksumSHA256 {
		t.Errorf("repository row digest diverges from the completed metadata")
	}
}

// TestObjectService_Complete_HappyPath_PngStripsExifChunkAndFinalizes is the
// PNG mirror of the JPEG spine above: the eXIf chunk (GPS profile included)
// leaves the stored bytes, the pixels decode unchanged, and the row finalizes
// with the sanitized metadata.
func TestObjectService_Complete_HappyPath_PngStripsExifChunkAndFinalizes(t *testing.T) {
	svc, store, _, _ := newTestService(t, nil)
	ctx := serviceCtx("tenant-a")
	base := testutil.PNG(t, 32, 24)
	withExif := pngWithExif(t)
	row := createAndUpload(t, svc, ctx, withExif, "image/png")

	completed, err := svc.Complete(ctx, row.ID)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	stored, ok := store.bytes(row.Key)
	if !ok {
		t.Fatal("stored bytes vanished at Complete")
	}
	if bytes.Contains(stored, pngChunkExif) {
		t.Fatal("eXIf chunk survives the pipeline")
	}
	if bytes.Contains(stored, []byte("GPSLatitudeRef")) {
		t.Fatal("gps profile bytes survive the pipeline")
	}
	assertDecodesEqual(t, "service png strip", stored, base)
	if completed.MIME == nil || *completed.MIME != "image/png" {
		t.Errorf("mime = %v, want image/png", completed.MIME)
	}
	if completed.Width == nil || *completed.Width != 32 || completed.Height == nil || *completed.Height != 24 {
		t.Errorf("dimensions = %vx%v, want 32x24", completed.Width, completed.Height)
	}
	if completed.ChecksumSHA256 == nil || *completed.ChecksumSHA256 != sha256HexDigest(stored) {
		t.Errorf("digest = %v, want the digest of the stored bytes", completed.ChecksumSHA256)
	}
}

// TestObjectService_Complete_FinalizedDigestDescribesTheStoredBytes pins the
// checksum semantics of a sanitizer rewrite: the declared checksum is
// reconciled against the bytes as uploaded (the check runs before the strip),
// and the finalized digest describes the bytes as stored (after the strip) --
// so the two differ exactly when the upload carried metadata that was
// removed, and that divergence is the point, not an inconsistency.
func TestObjectService_Complete_FinalizedDigestDescribesTheStoredBytes(t *testing.T) {
	svc, store, _, _ := newTestService(t, nil)
	ctx := serviceCtx("tenant-a")
	withExif := jpegWithExif(t)
	declared := sha256HexDigest(withExif) // the digest of the bytes as uploaded
	row, err := svc.Create(ctx, CreateParams{
		DeclaredSize:     int64(len(withExif)),
		DeclaredType:     "image/jpeg",
		DeclaredChecksum: declared,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err = svc.Upload(ctx, row.ID, nil, bytes.NewReader(withExif)); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	completed, err := svc.Complete(ctx, row.ID)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	stored, _ := store.bytes(row.Key)
	if *completed.ChecksumSHA256 == declared {
		t.Fatal("finalized digest equals the declared digest of the stripped upload")
	}
	if *completed.ChecksumSHA256 != sha256HexDigest(stored) {
		t.Fatal("finalized digest does not describe the stored bytes")
	}
}

func TestObjectService_Complete_RefusesAChecksumMismatch(t *testing.T) {
	svc, store, _, _ := newTestService(t, nil)
	ctx := serviceCtx("tenant-a")
	body := testutil.JPEG(t, 8, 8)
	row, err := svc.Create(ctx, CreateParams{
		DeclaredSize:     int64(len(body)),
		DeclaredType:     "image/jpeg",
		DeclaredChecksum: sha256HexDigest([]byte("bytes that never arrive")),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err = svc.Upload(ctx, row.ID, nil, bytes.NewReader(body)); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	_, err = svc.Complete(ctx, row.ID)
	assertCode(t, err, ErrChecksumMismatch.Code)
	assertStillUploading(t, svc, ctx, row.ID)
	if got, _ := store.bytes(row.Key); !bytes.Equal(got, body) {
		t.Fatal("refused completion removed the stored bytes")
	}
}

// TestObjectService_Complete_ProbedTypeGoverns pins the probe-first order:
// bytes that probe as something outside the allowlist are refused for what
// they are (type_not_allowed), never for contradicting their declaration
// (type_mismatch) -- the probe, not the header, is the trust anchor.
func TestObjectService_Complete_ProbedTypeGoverns(t *testing.T) {
	svc, _, _, _ := newTestService(t, nil)
	ctx := serviceCtx("tenant-a")
	html := []byte("<!doctype html><html><body>not an image, whatever the declaration says</body></html>")
	row := createAndUpload(t, svc, ctx, html, "image/png") // allowlisted claim, html reality

	_, err := svc.Complete(ctx, row.ID)
	assertCode(t, err, ErrTypeNotAllowed.Code)
	assertParam(t, err, "allowed", "image/jpeg,image/png")
	assertStillUploading(t, svc, ctx, row.ID)
}

func TestObjectService_Complete_DeclaredTypeMustAgreeWithTheProbe(t *testing.T) {
	svc, _, _, _ := newTestService(t, nil)
	ctx := serviceCtx("tenant-a")
	row := createAndUpload(t, svc, ctx, testutil.PNG(t, 8, 8), "image/jpeg") // png reality

	_, err := svc.Complete(ctx, row.ID)
	assertCode(t, err, ErrTypeMismatch.Code)
	assertStillUploading(t, svc, ctx, row.ID)
}

func TestObjectService_Complete_PixelCeilingEnforced(t *testing.T) {
	svc, _, _, _ := newTestService(t, func(cfg *serviceConfig) {
		cfg.maxImagePixels = 5000
	})
	ctx := serviceCtx("tenant-a")
	// 100x100 = 10,000 pixels, twice the configured ceiling.
	row := createAndUpload(t, svc, ctx, testutil.PNG(t, 100, 100), "image/png")

	_, err := svc.Complete(ctx, row.ID)
	assertCode(t, err, ErrPixelLimitExceeded.Code)
	assertParam(t, err, "max_pixels", int64(5000))
	assertStillUploading(t, svc, ctx, row.ID)
}

func TestObjectService_Complete_RefusesUndecodableImages(t *testing.T) {
	t.Run("truncated jpeg", func(t *testing.T) {
		svc, _, _, _ := newTestService(t, nil)
		ctx := serviceCtx("tenant-a")
		full := testutil.JPEG(t, 48, 32)
		if len(full) < 8 || full[0] != 0xFF || full[1] != 0xD8 || full[2] != 0xFF {
			t.Fatal("test fixture is not a JPEG that starts with SOI and a length-bearing segment")
		}
		// The first segment's length field (big endian at full[4:6],
		// self-inclusive) promises the segment runs to full[4+segLen]; the
		// cut ends the file one byte inside that span, so the truncated
		// file is structure-broken at its head -- where the header-only
		// config decode reads -- no matter where its entropy scan data
		// falls.
		segLen := int(full[4])<<8 | int(full[5])
		cut := 4 + segLen - 1
		if cut <= 6 || cut >= len(full) {
			t.Fatalf("first segment has an implausible length: %d bytes in a %d-byte fixture", segLen, len(full))
		}
		row := createAndUpload(t, svc, ctx, full[:cut], "image/jpeg")

		_, err := svc.Complete(ctx, row.ID)
		assertCode(t, err, ErrImageUnreadable.Code)
		assertStillUploading(t, svc, ctx, row.ID)
	})

	t.Run("png with corrupt chunk crc", func(t *testing.T) {
		svc, _, _, _ := newTestService(t, nil)
		ctx := serviceCtx("tenant-a")
		full := testutil.PNG(t, 32, 24)
		idat := bytes.Index(full, []byte("IDAT"))
		if idat < 8 {
			t.Fatal("test fixture PNG has no IDAT chunk")
		}
		// The chunk header before the type tells us where IDAT's data
		// starts, so the corrupted byte is provably inside the chunk. The
		// header-only config decode never reads IDAT, but sanitizePNG
		// CRC-verifies every chunk before carrying it, so a flipped data
		// byte refuses deterministically.
		if chunkLen := binary.BigEndian.Uint32(full[idat-4 : idat]); chunkLen < 2 {
			t.Fatalf("IDAT chunk too small to corrupt: %d bytes", chunkLen)
		}
		corrupt := append([]byte(nil), full...)
		corrupt[idat+4] ^= 0x01
		row := createAndUpload(t, svc, ctx, corrupt, "image/png")

		_, err := svc.Complete(ctx, row.ID)
		assertCode(t, err, ErrImageUnreadable.Code)
		assertStillUploading(t, svc, ctx, row.ID)
	})
}

// TestObjectService_Complete_AllowlistedNonImage passes a non-image media type
// through the whole pipeline: the PDF is allowlisted, declared and probed as
// application/pdf, and completes with no image work at all -- no dimension
// columns, no sanitizer rewrite, no derive task.
func TestObjectService_Complete_AllowlistedNonImage(t *testing.T) {
	svc, store, queue, bus := newTestService(t, func(cfg *serviceConfig) {
		cfg.allowedTypes = []string{"application/pdf"}
	})
	ctx := serviceCtx("tenant-a")
	pdf := []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog >>\nendobj\n%%EOF")
	row := createAndUpload(t, svc, ctx, pdf, "application/pdf")

	completed, err := svc.Complete(ctx, row.ID)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	stored, _ := store.bytes(row.Key)
	if !bytes.Equal(stored, pdf) {
		t.Fatal("non-image bytes were rewritten")
	}
	if completed.MIME == nil || *completed.MIME != "application/pdf" {
		t.Errorf("mime = %v, want application/pdf", completed.MIME)
	}
	if completed.Size == nil || *completed.Size != int64(len(pdf)) {
		t.Errorf("size = %v, want %d", completed.Size, len(pdf))
	}
	if completed.Width != nil || completed.Height != nil {
		t.Errorf("dimensions = %vx%v on a non-image, want none", completed.Width, completed.Height)
	}
	if len(queue.tasks) != 0 {
		t.Errorf("derive task enqueued for a non-image: %v", queue.tasks)
	}
	if len(bus.events) != 1 {
		t.Fatalf("events = %d, want exactly the completion event", len(bus.events))
	}
	if bus.events[0].Payload.(ObjectCompletedPayload).MIME != "application/pdf" {
		t.Errorf("event payload = %+v, want MIME application/pdf", bus.events[0].Payload)
	}
}

// TestObjectService_Complete_SideEffectFailuresDoNotFailTheFinalize pins the
// warn-and-stand contract: once the row is completed, neither a queue that
// refuses the derive task nor a bus that refuses the event can turn the
// completion into a failure -- the object is done, and the side effects are
// rendition and announcement, not the finalize.
func TestObjectService_Complete_SideEffectFailuresDoNotFailTheFinalize(t *testing.T) {
	t.Run("thumbnail enqueue refused", func(t *testing.T) {
		svc, _, queue, bus := newTestService(t, nil)
		queue.fail = errors.New("queue unavailable")
		ctx := serviceCtx("tenant-a")
		row := createAndUpload(t, svc, ctx, testutil.JPEG(t, 16, 16), "image/jpeg")

		completed, err := svc.Complete(ctx, row.ID)
		if err != nil {
			t.Fatalf("Complete with a failing queue: %v", err)
		}
		if completed.State != ObjectStateCompleted {
			t.Fatalf("state = %q, want completed", completed.State)
		}
		if len(queue.tasks) != 0 {
			t.Errorf("refused queue recorded %d tasks", len(queue.tasks))
		}
		if len(bus.events) != 1 {
			t.Errorf("events = %d, want the completion event despite the queue failure", len(bus.events))
		}
	})

	t.Run("completion event publish refused", func(t *testing.T) {
		svc, _, queue, bus := newTestService(t, nil)
		bus.fail = errors.New("bus unavailable")
		ctx := serviceCtx("tenant-a")
		row := createAndUpload(t, svc, ctx, testutil.JPEG(t, 16, 16), "image/jpeg")

		completed, err := svc.Complete(ctx, row.ID)
		if err != nil {
			t.Fatalf("Complete with a failing bus: %v", err)
		}
		if completed.State != ObjectStateCompleted {
			t.Fatalf("state = %q, want completed", completed.State)
		}
		if len(bus.events) != 0 {
			t.Errorf("refused bus recorded %d events", len(bus.events))
		}
		if len(queue.tasks) != 1 {
			t.Errorf("tasks = %d, want the derive task despite the bus failure", len(queue.tasks))
		}
	})
}

// TestObjectService_Complete_PublishesTheEventAndEnqueuesDerivation pins the
// two side effects of a completed image object: the EventObjectCompleted
// announcement (tenant, payload) and the thumbnail-derive task (type, tenant
// from the row, deterministic idempotency key, JSON payload naming only the
// object id).
func TestObjectService_Complete_PublishesTheEventAndEnqueuesDerivation(t *testing.T) {
	svc, store, queue, bus := newTestService(t, nil)
	ctx := serviceCtx("tenant-a")
	row := createAndUpload(t, svc, ctx, testutil.JPEG(t, 24, 24), "image/jpeg")
	if _, err := svc.Complete(ctx, row.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if len(bus.events) != 1 {
		t.Fatalf("events = %d, want exactly one", len(bus.events))
	}
	evt := bus.events[0]
	if evt.Type != EventObjectCompleted {
		t.Errorf("event type = %q, want %q", evt.Type, EventObjectCompleted)
	}
	if evt.TenantID != pkgcore.TenantID("tenant-a") {
		t.Errorf("event tenant = %q, want %q", evt.TenantID, "tenant-a")
	}
	payload, ok := evt.Payload.(ObjectCompletedPayload)
	if !ok {
		t.Fatalf("event payload = %T, want ObjectCompletedPayload", evt.Payload)
	}
	stored, _ := store.bytes(row.Key)
	if payload.ObjectID != row.ID || payload.Size != int64(len(stored)) || payload.MIME != "image/jpeg" {
		t.Errorf("event payload = %+v, want object %s, size %d, mime image/jpeg", payload, row.ID, len(stored))
	}

	if len(queue.tasks) != 1 {
		t.Fatalf("tasks = %d, want exactly one", len(queue.tasks))
	}
	task := queue.tasks[0]
	if task.Type != taskTypeDeriveThumbnail {
		t.Errorf("task type = %q, want %q", task.Type, taskTypeDeriveThumbnail)
	}
	if task.TenantID != pkgcore.TenantID("tenant-a") {
		t.Errorf("task tenant = %q, want %q -- taken from the row, never assumed from the caller", task.TenantID, "tenant-a")
	}
	if task.IdempotencyKey != thumbnailDeriveIdempotencyKey(row.ID) {
		t.Errorf("idempotency key = %q, want %q", task.IdempotencyKey, thumbnailDeriveIdempotencyKey(row.ID))
	}
	var derivePayload deriveThumbnailTaskPayload
	if err := json.Unmarshal(task.Payload, &derivePayload); err != nil {
		t.Fatalf("task payload is not the derive JSON: %v", err)
	}
	if derivePayload.ObjectID != row.ID {
		t.Errorf("task payload object_id = %q, want %q", derivePayload.ObjectID, row.ID)
	}
}

// TestObjectService_Reads_CompletedRowsOnly pins the visibility rule Get and
// OpenContent share: an object that is not completed reads exactly like an
// object that does not exist -- uploading and deleting rows included -- and
// rows of another tenant stay invisible too.
func TestObjectService_Reads_CompletedRowsOnly(t *testing.T) {
	svc, store, _, _ := newTestService(t, nil)
	ctx := serviceCtx("tenant-a")
	now := time.Now()
	seedObject(t, svc.objects, ctx, newCompleted("obj-done", "tenant-a", now))
	seedObject(t, svc.objects, ctx, newUpload("obj-up", "tenant-a", now))
	deleting := newCompleted("obj-del", "tenant-a", now)
	deleting.State = ObjectStateDeleting
	seedObject(t, svc.objects, ctx, deleting)
	store.objects["tenant-a/obj-done/original"] = []byte("the completed bytes")

	for _, id := range []string{"obj-up", "obj-del", "no-such-object"} {
		t.Run("Get on "+id, func(t *testing.T) {
			_, err := svc.Get(ctx, id)
			assertCode(t, err, ErrObjectNotFound.Code)
			assertParam(t, err, "id", id)
		})
	}
	t.Run("Get on a foreign tenant's row", func(t *testing.T) {
		_, err := svc.Get(serviceCtx("tenant-b"), "obj-done")
		assertCode(t, err, ErrObjectNotFound.Code)
	})

	row, rc, err := svc.OpenContent(ctx, "obj-done")
	if err != nil {
		t.Fatalf("OpenContent on the completed row: %v", err)
	}
	if row.ID != "obj-done" {
		t.Errorf("OpenContent row = %q", row.ID)
	}
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("reading the opened content: %v", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatalf("closing the opened content: %v", err)
	}
	if string(raw) != "the completed bytes" {
		t.Errorf("opened content = %q", raw)
	}

	for _, id := range []string{"obj-up", "obj-del", "no-such-object"} {
		t.Run("OpenContent on "+id, func(t *testing.T) {
			_, rc, err := svc.OpenContent(ctx, id)
			assertCode(t, err, ErrObjectNotFound.Code)
			if rc != nil {
				t.Fatal("OpenContent returned a reader with its not-found error")
			}
		})
	}

	t.Run("OpenContent on a completed row whose store lost the bytes", func(t *testing.T) {
		// A completed row without bytes is an anomaly -- its metadata was
		// finalized from them -- so it reads as store_error, never as a
		// plausible missing object.
		orphan := newCompleted("obj-orphan", "tenant-a", now)
		seedObject(t, svc.objects, ctx, orphan) // completed, no bytes under its key
		_, _, err := svc.OpenContent(ctx, "obj-orphan")
		assertCode(t, err, ErrStoreError.Code)
		if !errors.Is(err, pkgcore.ErrObjectNotFound) {
			t.Errorf("store_error does not carry the store's not-found cause: %v", err)
		}
	})
}

// TestObjectService_List_CompletedOnlyCursorPages pins the listing contract:
// completed objects of the caller's tenant, newest first, in keyset pages of
// at most the default page size (50), with uploading and deleting rows
// invisible even when they are the newest rows of all, and a cursor that
// names no row of this listing (unknown, foreign, or left the state) reading
// as object_not_found rather than a silently resumed page.
func TestObjectService_List_CompletedOnlyCursorPages(t *testing.T) {
	svc, _, _, _ := newTestService(t, nil)
	ctx := serviceCtx("tenant-a")
	base := time.Now().Add(-2 * time.Hour)
	for i := 0; i < 52; i++ {
		seedObject(t, svc.objects, ctx, newCompleted(fmt.Sprintf("c-%02d", i), "tenant-a", base.Add(time.Duration(i)*time.Minute)))
	}
	seedObject(t, svc.objects, ctx, newUpload("u-newest", "tenant-a", base.Add(52*time.Minute)))
	deleting := newCompleted("d-newest", "tenant-a", base.Add(53*time.Minute))
	deleting.State = ObjectStateDeleting
	seedObject(t, svc.objects, ctx, deleting)

	wantFirst := make([]string, 0, defaultListPageSize)
	for i := 51; i >= 2; i-- {
		wantFirst = append(wantFirst, fmt.Sprintf("c-%02d", i))
	}
	first, err := svc.List(ctx, 0, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(first) != defaultListPageSize {
		t.Fatalf("first page holds %d rows, want the default page size %d", len(first), defaultListPageSize)
	}
	assertObjectIDs(t, first, wantFirst...)

	second, err := svc.List(ctx, 0, first[len(first)-1].ID)
	if err != nil {
		t.Fatalf("List after the cursor: %v", err)
	}
	assertObjectIDs(t, second, "c-01", "c-00")

	third, err := svc.List(ctx, 0, second[len(second)-1].ID)
	if err != nil {
		t.Fatalf("List past the last row: %v", err)
	}
	if len(third) != 0 {
		t.Fatalf("page past the last row holds %d rows, want none", len(third))
	}

	tail, err := svc.List(ctx, 2, "")
	if err != nil {
		t.Fatalf("List with a small limit: %v", err)
	}
	assertObjectIDs(t, tail, "c-51", "c-50")

	for _, cursor := range []string{"u-newest", "d-newest", "00000000-0000-4000-8000-000000000000"} {
		t.Run("cursor "+cursor, func(t *testing.T) {
			_, err := svc.List(ctx, 0, cursor)
			assertCode(t, err, ErrObjectNotFound.Code)
			assertParam(t, err, "id", cursor)
		})
	}
}

// TestObjectService_StoreFailuresAreStoreError pins the boundary between the
// store's vocabulary and the module's: a store that refuses a Put or fails a
// Get (other than its own not-found answer) surfaces as storage.store_error
// with the store's error as the cause.
func TestObjectService_StoreFailuresAreStoreError(t *testing.T) {
	t.Run("upload put refused", func(t *testing.T) {
		svc, store, _, _ := newTestService(t, nil)
		ctx := serviceCtx("tenant-a")
		row, err := svc.Create(ctx, CreateParams{DeclaredSize: 10})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		store.putErr = errors.New("s3 unavailable")
		err = svc.Upload(ctx, row.ID, nil, bytes.NewReader(bytes.Repeat([]byte("x"), 10)))
		assertCode(t, err, ErrStoreError.Code)
		if !errors.Is(err, store.putErr) {
			t.Errorf("store_error does not carry the store's cause: %v", err)
		}
		if _, ok := store.bytes(row.Key); ok {
			t.Fatal("a refused put left bytes behind")
		}
	})

	t.Run("complete get refused", func(t *testing.T) {
		svc, store, _, _ := newTestService(t, nil)
		ctx := serviceCtx("tenant-a")
		row := createAndUpload(t, svc, ctx, []byte("bytes for the store"), "")
		store.getErr = errors.New("store read failed")
		_, err := svc.Complete(ctx, row.ID)
		assertCode(t, err, ErrStoreError.Code)
		if !errors.Is(err, store.getErr) {
			t.Errorf("store_error does not carry the store's cause: %v", err)
		}
	})
}

// TestObjectService_FailsClosedWithoutAnAttachedHost pins the service's
// answer before Module.Register attaches the registry: methods that need the
// store fail with storage.store_unavailable rather than panicking on a nil
// seam -- metadata operations that need no store keep working.
func TestObjectService_FailsClosedWithoutAnAttachedHost(t *testing.T) {
	svc := newObjectService(NewObjectRepository(newTestDB(t)), &recordingQueue{}, testServiceConfig())
	ctx := serviceCtx("tenant-a")

	row, err := svc.Create(ctx, CreateParams{DeclaredSize: 10, DeclaredType: "image/png"})
	if err != nil {
		t.Fatalf("Create without a host: %v", err)
	}
	err = svc.Upload(ctx, row.ID, nil, bytes.NewReader([]byte("0123456789")))
	assertCode(t, err, ErrStoreUnavailable.Code)
	_, err = svc.Complete(ctx, row.ID)
	assertCode(t, err, ErrStoreUnavailable.Code)

	seedObject(t, svc.objects, ctx, newCompleted("obj-done", "tenant-a", time.Now()))
	if _, err = svc.Get(ctx, "obj-done"); err != nil {
		t.Errorf("Get needs no store but failed: %v", err)
	}
	_, rc, err := svc.OpenContent(ctx, "obj-done")
	assertCode(t, err, ErrStoreUnavailable.Code)
	if rc != nil {
		t.Fatal("OpenContent returned a reader with its store_unavailable error")
	}
}
