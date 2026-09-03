package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/storage/internal/testutil"
)

// newDeriveHarness returns a DeriveService and an ObjectService sharing one
// migrated database, one store, one queue and one bus -- the module's real
// composition, minus the registry. Tests drive an object through the
// transfer service (create, upload, complete), which is exactly the state
// the derive pipeline consumes, and assert on the fakes and on the shared
// repositories (reachable as svc.objects / derive.derivatives).
func newDeriveHarness(t *testing.T, mutate func(*serviceConfig)) (*DeriveService, *ObjectService, *fakeStore, *recordingQueue, *recordingBus) {
	t.Helper()
	cfg := testServiceConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	store := newFakeStore()
	queue := &recordingQueue{}
	bus := newRecordingBus()
	host := &fakeHost{store: store, bus: bus}
	db := newTestDB(t)
	objects := NewObjectRepository(db)
	derive := newDeriveService(objects, NewDerivativeRepository(db), 0, cfg.maxImagePixels)
	derive.host = host
	svc := newObjectService(objects, queue, cfg)
	svc.host = host
	return derive, svc, store, queue, bus
}

// thumbKey returns the canonical store key of objectID's thumbnail, failing
// the test when the grammar refuses to build one (it cannot for these ids).
func thumbKey(t *testing.T, tenant pkgcore.TenantID, objectID string) string {
	t.Helper()
	key, err := DerivativeKey(tenant, objectID, DerivativeKindThumbnail)
	if err != nil {
		t.Fatalf("DerivativeKey(%q, %q): %v", tenant, objectID, err)
	}
	return key
}

// assertThumbnailRow fails t unless objectID's object carries exactly one
// derivative row -- the thumbnail -- with the metadata want, and the store
// holds bytes of exactly the row's size under its key.
func assertThumbnailRow(t *testing.T, derive *DeriveService, store *fakeStore, ctx context.Context, objectID, mime string, w, h int) {
	t.Helper()
	rows, err := derive.derivatives.listByObject(ctx, objectID)
	if err != nil {
		t.Fatalf("listByObject(%s): %v", objectID, err)
	}
	if len(rows) != 1 {
		t.Fatalf("object %s has %d derivative rows, want exactly 1", objectID, len(rows))
	}
	d := rows[0]
	if d.ObjectID != objectID {
		t.Errorf("derivative row ObjectID = %q, want %q", d.ObjectID, objectID)
	}
	if d.Kind != DerivativeKindThumbnail {
		t.Errorf("derivative row Kind = %q, want %q", d.Kind, DerivativeKindThumbnail)
	}
	if d.Key != thumbKey(t, pkgcore.TenantID(d.TenantID), objectID) {
		t.Errorf("derivative row Key = %q, want the thumbnail grammar key", d.Key)
	}
	if d.MIME != mime {
		t.Errorf("derivative row MIME = %q, want %q", d.MIME, mime)
	}
	if d.Size <= 0 {
		t.Fatalf("derivative row Size = %d, want positive", d.Size)
	}
	if d.Width == nil || *d.Width != w {
		t.Errorf("derivative row Width = %v, want %d", d.Width, w)
	}
	if d.Height == nil || *d.Height != h {
		t.Errorf("derivative row Height = %v, want %d", d.Height, h)
	}
	raw, ok := store.bytes(d.Key)
	if !ok {
		t.Fatalf("store holds nothing under derivative key %q", d.Key)
	}
	if int64(len(raw)) != d.Size {
		t.Errorf("stored thumbnail is %d bytes, row claims %d", len(raw), d.Size)
	}
}

// decodeThumb returns the dimensions and format of stored thumbnail bytes,
// the assertion that the derived artifact is a decodable image of the
// expected format and size.
func decodeThumb(t *testing.T, raw []byte) (w, h int, format string) {
	t.Helper()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("DecodeConfig of the stored thumbnail: %v", err)
	}
	return cfg.Width, cfg.Height, format
}

// TestBoxDownscaleTo_ExactAreaAverage proves the resampler is an exact area
// average, not a sampler: a 4x4 source made of four 2x2 solid-color
// quadrants downscaled to 2x2 must yield each quadrant's color exactly --
// no interpolation between them, and no source pixel skipped or doubled.
func TestBoxDownscaleTo_ExactAreaAverage(t *testing.T) {
	quadrants := []color.NRGBA{
		{R: 200, G: 20, B: 20, A: 255},   // top-left: red
		{R: 20, G: 200, B: 20, A: 255},   // top-right: green
		{R: 20, G: 20, B: 200, A: 255},   // bottom-left: blue
		{R: 230, G: 230, B: 230, A: 255}, // bottom-right: near-white
	}
	src := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			q := (y/2)*2 + x/2
			src.SetNRGBA(x, y, quadrants[q])
		}
	}

	dst := boxDownscaleTo(src, 2, 2)
	if got := dst.Bounds(); got != image.Rect(0, 0, 2, 2) {
		t.Fatalf("downscaled bounds = %v, want 2x2", got)
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			want := quadrants[y*2+x]
			if got := dst.NRGBAAt(x, y); got != want {
				t.Errorf("output pixel (%d,%d) = %v, want the exact quadrant color %v", x, y, got, want)
			}
		}
	}
}

// TestBoxDownscaleTo_AveragesAlphaWeighted proves the average runs in
// premultiplied space: a transparent pixel contributes no color at all --
// not the RGB its row still stores, which is exactly why a transparent
// white pixel would smear a naive straight-color average toward white and
// wash out the result. Three of the four source pixels are fully
// transparent (one of them white); the only visible pixel is white, so the
// 1x1 output must be that white at a quarter opacity -- not a grey, and not
// a white halo.
func TestBoxDownscaleTo_AveragesAlphaWeighted(t *testing.T) {
	src := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	src.SetNRGBA(0, 0, color.NRGBA{R: 255, G: 255, B: 255, A: 255}) // visible white
	src.SetNRGBA(1, 0, color.NRGBA{R: 255, G: 255, B: 255, A: 0})   // transparent white
	src.SetNRGBA(0, 1, color.NRGBA{R: 0, G: 0, B: 0, A: 0})         // transparent black
	src.SetNRGBA(1, 1, color.NRGBA{R: 0, G: 0, B: 0, A: 0})         // transparent black

	dst := boxDownscaleTo(src, 1, 1)
	got := dst.NRGBAAt(0, 0)
	want := color.NRGBA{R: 255, G: 255, B: 255, A: 63} // one quarter of the white pixel's opacity
	if got != want {
		t.Errorf("1x1 average = %v, want %v (a white pixel over transparent black)", got, want)
	}
}

// TestDownscaleToMaxEdge_ReturnsTheSourceWhenItAlreadyFits proves the
// no-work path is a true no-op: the very image instance comes back, to be
// re-encoded by the caller.
func TestDownscaleToMaxEdge_ReturnsTheSourceWhenItAlreadyFits(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 200, 100))
	if got := downscaleToMaxEdge(src, 320); got != image.Image(src) {
		t.Errorf("a source that already fits came back as a different image")
	}
}

// TestDownscaleToMaxEdge_DimensionMath pins the two arithmetic rules: floor
// division preserves the aspect ratio on the scaled edge (10x4 at edge 3
// becomes 3x1, not 3x2), and a non-positive configured edge resolves to the
// module default (500x400 at default edge 320 becomes 320x256).
func TestDownscaleToMaxEdge_DimensionMath(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 10, 4))
	dst := downscaleToMaxEdge(src, 3)
	if w, h := dst.Bounds().Dx(), dst.Bounds().Dy(); w != 3 || h != 1 {
		t.Errorf("10x4 at edge 3 downscaled to %dx%d, want 3x1", w, h)
	}

	wide := image.NewRGBA(image.Rect(0, 0, 500, 400))
	dst = downscaleToMaxEdge(wide, 0)
	if w, h := dst.Bounds().Dx(), dst.Bounds().Dy(); w != 320 || h != 256 {
		t.Errorf("500x400 at the default edge downscaled to %dx%d, want 320x256", w, h)
	}
}

// TestDeriveService_DeriveThumbnail_DownscalesToTheConfiguredEdge runs the
// full pipeline over a completed JPEG and a completed PNG: the thumbnail
// bytes land in the store under the derivative key, in the source's own
// format, at 320x240 -- the default longer-edge bound -- and the derivative
// row records exactly those bytes.
func TestDeriveService_DeriveThumbnail_DownscalesToTheConfiguredEdge(t *testing.T) {
	for _, tt := range []struct {
		name      string
		fixture   func(t testing.TB) []byte
		declared  string
		wantMIME  string
		wantWidth int
	}{
		{
			name:      "jpeg source",
			fixture:   func(t testing.TB) []byte { return testutil.JPEG(t, 800, 600) },
			declared:  "image/jpeg",
			wantMIME:  "image/jpeg",
			wantWidth: 320,
		},
		{
			name:      "png source",
			fixture:   func(t testing.TB) []byte { return testutil.PNG(t, 400, 300) },
			declared:  "image/png",
			wantMIME:  "image/png",
			wantWidth: 320,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			derive, svc, store, _, _ := newDeriveHarness(t, nil)
			ctx := serviceCtx("tenant-a")
			row := createAndUpload(t, svc, ctx, tt.fixture(t), tt.declared)
			if _, err := svc.Complete(ctx, row.ID); err != nil {
				t.Fatalf("Complete(%s): %v", row.ID, err)
			}

			if err := derive.DeriveThumbnail(ctx, row.ID); err != nil {
				t.Fatalf("DeriveThumbnail(%s): %v", row.ID, err)
			}

			raw, ok := store.bytes(thumbKey(t, "tenant-a", row.ID))
			if !ok {
				t.Fatal("store holds nothing under the thumbnail key")
			}
			w, h, format := decodeThumb(t, raw)
			if format != tt.wantMIME[len("image/"):] {
				t.Errorf("thumbnail format = %q, want %q", format, tt.wantMIME)
			}
			if w != tt.wantWidth || h != 240 {
				t.Errorf("thumbnail is %dx%d, want %dx240", w, h, tt.wantWidth)
			}
			assertThumbnailRow(t, derive, store, ctx, row.ID, tt.wantMIME, tt.wantWidth, 240)
		})
	}
}

// TestDeriveService_DeriveThumbnail_ReEncodesSourcesAtTheirOwnSize proves
// the under-the-edge path: a source already within the bound is not
// upscaled, and its thumbnail keeps its own dimensions (the re-encode still
// runs -- a thumbnail is never the original bytes).
func TestDeriveService_DeriveThumbnail_ReEncodesSourcesAtTheirOwnSize(t *testing.T) {
	for _, tt := range []struct {
		name      string
		fixture   func(t testing.TB) []byte
		declared  string
		wantMIME  string
		srcWidth  int
		srcHeight int
	}{
		{
			name:      "jpeg under the edge",
			fixture:   func(t testing.TB) []byte { return testutil.JPEG(t, 300, 200) },
			declared:  "image/jpeg",
			wantMIME:  "image/jpeg",
			srcWidth:  300,
			srcHeight: 200,
		},
		{
			name:      "png under the edge",
			fixture:   func(t testing.TB) []byte { return testutil.PNG(t, 64, 48) },
			declared:  "image/png",
			wantMIME:  "image/png",
			srcWidth:  64,
			srcHeight: 48,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			derive, svc, store, _, _ := newDeriveHarness(t, nil)
			ctx := serviceCtx("tenant-a")
			row := createAndUpload(t, svc, ctx, tt.fixture(t), tt.declared)
			if _, err := svc.Complete(ctx, row.ID); err != nil {
				t.Fatalf("Complete(%s): %v", row.ID, err)
			}

			if err := derive.DeriveThumbnail(ctx, row.ID); err != nil {
				t.Fatalf("DeriveThumbnail(%s): %v", row.ID, err)
			}

			raw, ok := store.bytes(thumbKey(t, "tenant-a", row.ID))
			if !ok {
				t.Fatal("store holds nothing under the thumbnail key")
			}
			w, h, format := decodeThumb(t, raw)
			if format != tt.wantMIME[len("image/"):] {
				t.Errorf("thumbnail format = %q, want %q", format, tt.wantMIME)
			}
			if w != tt.srcWidth || h != tt.srcHeight {
				t.Errorf("thumbnail is %dx%d, want the source's own %dx%d", w, h, tt.srcWidth, tt.srcHeight)
			}
			assertThumbnailRow(t, derive, store, ctx, row.ID, tt.wantMIME, tt.srcWidth, tt.srcHeight)
		})
	}
}

// TestDeriveService_DeriveThumbnail_IsIdempotent proves re-running a
// finished derive is a no-op: the second run derives nothing new -- still
// one row, still the same stored bytes -- and reports no error. This is the
// shape a retried or re-driven task lands in.
func TestDeriveService_DeriveThumbnail_IsIdempotent(t *testing.T) {
	derive, svc, store, _, _ := newDeriveHarness(t, nil)
	ctx := serviceCtx("tenant-a")
	row := createAndUpload(t, svc, ctx, testutil.PNG(t, 64, 48), "image/png")
	if _, err := svc.Complete(ctx, row.ID); err != nil {
		t.Fatalf("Complete(%s): %v", row.ID, err)
	}

	if err := derive.DeriveThumbnail(ctx, row.ID); err != nil {
		t.Fatalf("first DeriveThumbnail(%s): %v", row.ID, err)
	}
	key := thumbKey(t, "tenant-a", row.ID)
	first, ok := store.bytes(key)
	if !ok {
		t.Fatal("store holds nothing under the thumbnail key after the first run")
	}

	if err := derive.DeriveThumbnail(ctx, row.ID); err != nil {
		t.Fatalf("second DeriveThumbnail(%s): %v", row.ID, err)
	}
	second, ok := store.bytes(key)
	if !ok {
		t.Fatal("store holds nothing under the thumbnail key after the second run")
	}
	if !bytes.Equal(first, second) {
		t.Error("the second run replaced the stored thumbnail bytes")
	}
	rows, err := derive.derivatives.listByObject(ctx, row.ID)
	if err != nil {
		t.Fatalf("listByObject(%s): %v", row.ID, err)
	}
	if len(rows) != 1 {
		t.Errorf("object %s has %d derivative rows after two derives, want 1", row.ID, len(rows))
	}
}

// TestDeriveService_DeriveThumbnail_ConvergesOnNothingToDerive proves the
// skip-on-nothing contract: every state a task can meet that has no
// thumbnail to produce ends in a nil error -- the queue must not re-run such
// a task -- and in no row and no stored bytes.
func TestDeriveService_DeriveThumbnail_ConvergesOnNothingToDerive(t *testing.T) {
	now := time.Now()
	derive, svc, store, _, _ := newDeriveHarness(t, nil)
	ctx := serviceCtx("tenant-a")
	uploading := newUpload("up-1", "tenant-a", now)
	seedObject(t, svc.objects, ctx, uploading)
	deleting := newCompleted("del-1", "tenant-a", now)
	seedObject(t, svc.objects, ctx, deleting)
	if _, err := svc.objects.markDeleting(ctx, deleting.ID); err != nil {
		t.Fatalf("markDeleting(%s): %v", deleting.ID, err)
	}
	notImage := newCompleted("pdf-1", "tenant-a", now)
	pdf := "application/pdf"
	notImage.MIME = &pdf
	seedObject(t, svc.objects, ctx, notImage)
	gif := newCompleted("gif-1", "tenant-a", now)
	gifMime := "image/gif"
	gif.MIME = &gifMime
	seedObject(t, svc.objects, ctx, gif)

	for _, tt := range []struct {
		name     string
		objectID string
	}{
		{name: "an object that never existed", objectID: "no-such-object"},
		{name: "an uploading object", objectID: uploading.ID},
		{name: "an object the delete protocol is removing", objectID: deleting.ID},
		{name: "a completed object that is not an image", objectID: notImage.ID},
		{name: "an image type the service has no encoder for", objectID: gif.ID},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := derive.DeriveThumbnail(ctx, tt.objectID); err != nil {
				t.Fatalf("DeriveThumbnail(%s) = %v, want nil", tt.objectID, err)
			}
			rows, err := derive.derivatives.listByObject(ctx, tt.objectID)
			if err != nil {
				t.Fatalf("listByObject(%s): %v", tt.objectID, err)
			}
			if len(rows) != 0 {
				t.Errorf("object %s gained %d derivative rows from a skipped derive", tt.objectID, len(rows))
			}
			if _, ok := store.bytes(thumbKey(t, "tenant-a", tt.objectID)); ok {
				t.Errorf("store gained thumbnail bytes for the skipped derive of %s", tt.objectID)
			}
		})
	}
}

// TestDeriveService_DeriveThumbnail_StoreFailuresReportStoreError proves a
// completed row whose bytes the store cannot produce -- whether the key is
// missing entirely or the store itself refuses -- is a store anomaly
// (storage.store_error), the error shape the queue retries. The missing-key
// case in particular must not leak as "object not found": the row exists and
// promises content; only its bytes are unaccounted for.
func TestDeriveService_DeriveThumbnail_StoreFailuresReportStoreError(t *testing.T) {
	now := time.Now()

	t.Run("a completed row with no stored bytes", func(t *testing.T) {
		derive, svc, _, _, _ := newDeriveHarness(t, nil)
		ctx := serviceCtx("tenant-a")
		orphaned := newCompleted("ghost-1", "tenant-a", now)
		seedObject(t, svc.objects, ctx, orphaned)

		err := derive.DeriveThumbnail(ctx, orphaned.ID)
		assertCode(t, err, ErrStoreError.Code)
	})

	t.Run("a store that refuses to serve bytes", func(t *testing.T) {
		derive, svc, store, _, _ := newDeriveHarness(t, nil)
		ctx := serviceCtx("tenant-a")
		row := createAndUpload(t, svc, ctx, testutil.PNG(t, 64, 48), "image/png")
		if _, err := svc.Complete(ctx, row.ID); err != nil {
			t.Fatalf("Complete(%s): %v", row.ID, err)
		}
		store.getErr = errors.New("store is down")

		err := derive.DeriveThumbnail(ctx, row.ID)
		assertCode(t, err, ErrStoreError.Code)
	})
}

// TestDeriveService_DeriveThumbnail_ReChecksThePixelCeiling proves the
// worker never decodes an image the transfer pipeline would have refused:
// the ceiling from the module configuration is enforced over the stored
// bytes before any full decode, however the completed row got there.
func TestDeriveService_DeriveThumbnail_ReChecksThePixelCeiling(t *testing.T) {
	derive, svc, store, _, _ := newDeriveHarness(t, func(cfg *serviceConfig) {
		cfg.maxImagePixels = 10_000
	})
	ctx := serviceCtx("tenant-a")
	raw := testutil.PNG(t, 120, 120) // 14,400 pixels, over the 10,000 ceiling
	row := newCompleted("big-1", "tenant-a", time.Now())
	size := int64(len(raw))
	row.Size = &size
	seedObject(t, svc.objects, ctx, row)
	store.objects[row.Key] = raw

	err := derive.DeriveThumbnail(ctx, row.ID)
	assertCode(t, err, ErrPixelLimitExceeded.Code)
	assertParam(t, err, "max_pixels", int64(10_000))
}

// hookedStore wraps a fakeStore and runs onPut after every successful byte
// write -- the seam the mid-run deletion race tests need: the delete
// protocol's row removal lands while DeriveThumbnail is between its own byte
// write and its convergence re-check.
type hookedStore struct {
	*fakeStore
	onPut func()
}

func (s *hookedStore) PutObject(ctx context.Context, key string, r io.Reader) error {
	if err := s.fakeStore.PutObject(ctx, key, r); err != nil {
		return err
	}
	if s.onPut != nil {
		s.onPut()
	}
	return nil
}

// TestDeriveService_DeriveThumbnail_DropsItsBytesWhenTheObjectDisappears
// proves the convergence re-check: when the delete protocol removes the
// object between this service's byte write and its row insert -- the full
// protocol in one interleaving, or the mark alone in the other -- the
// service drops the bytes it just wrote and converges on nil, leaving no
// derivative row and no orphaned bytes behind for the sweep to find.
func TestDeriveService_DeriveThumbnail_DropsItsBytesWhenTheObjectDisappears(t *testing.T) {
	for _, tt := range []struct {
		name  string
		onPut func(t *testing.T, svc *ObjectService, ctx context.Context, objectID string)
	}{
		{
			name: "the delete protocol finished before the re-check",
			onPut: func(t *testing.T, svc *ObjectService, ctx context.Context, objectID string) {
				t.Helper()
				if _, err := svc.objects.markDeleting(ctx, objectID); err != nil {
					t.Fatalf("markDeleting(%s): %v", objectID, err)
				}
				if err := svc.objects.deleteObjectRows(ctx, objectID); err != nil {
					t.Fatalf("deleteObjectRows(%s): %v", objectID, err)
				}
			},
		},
		{
			name: "the object is marked deleting before the re-check",
			onPut: func(t *testing.T, svc *ObjectService, ctx context.Context, objectID string) {
				t.Helper()
				if _, err := svc.objects.markDeleting(ctx, objectID); err != nil {
					t.Fatalf("markDeleting(%s): %v", objectID, err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			derive, svc, store, _, bus := newDeriveHarness(t, nil)
			ctx := serviceCtx("tenant-a")
			row := createAndUpload(t, svc, ctx, testutil.PNG(t, 400, 300), "image/png")
			if _, err := svc.Complete(ctx, row.ID); err != nil {
				t.Fatalf("Complete(%s): %v", row.ID, err)
			}

			// Re-point both services at a store that deletes the object the
			// moment the derive's own byte write lands.
			hooked := &hookedStore{fakeStore: store, onPut: func() {
				tt.onPut(t, svc, ctx, row.ID)
			}}
			host := &fakeHost{store: hooked, bus: bus}
			svc.host = host
			derive.host = host

			if err := derive.DeriveThumbnail(ctx, row.ID); err != nil {
				t.Fatalf("DeriveThumbnail(%s): %v", row.ID, err)
			}
			if _, ok := store.bytes(thumbKey(t, "tenant-a", row.ID)); ok {
				t.Error("the just-written thumbnail bytes were not dropped")
			}
			rows, err := derive.derivatives.listByObject(ctx, row.ID)
			if err != nil {
				t.Fatalf("listByObject(%s): %v", row.ID, err)
			}
			if len(rows) != 0 {
				t.Errorf("object %s gained %d derivative rows from a converged derive", row.ID, len(rows))
			}
		})
	}
}

// TestDeriveHandler_Handle_DerivesFromTheEnqueuedTask proves the module's
// task loop end to end at the unit tier: the completion pipeline enqueues a
// thumbnail-derive task, and the handler that task dispatches to -- driven
// with the task's own fields, as the queue would -- derives and stores the
// thumbnail. A task whose payload does not match the shape the pipeline
// enqueues fails the job.
func TestDeriveHandler_Handle_DerivesFromTheEnqueuedTask(t *testing.T) {
	derive, svc, store, queue, _ := newDeriveHarness(t, nil)
	ctx := serviceCtx("tenant-a")
	row := createAndUpload(t, svc, ctx, testutil.JPEG(t, 800, 600), "image/jpeg")
	if _, err := svc.Complete(ctx, row.ID); err != nil {
		t.Fatalf("Complete(%s): %v", row.ID, err)
	}
	if len(queue.tasks) != 1 {
		t.Fatalf("completion enqueued %d tasks, want 1", len(queue.tasks))
	}
	task := queue.tasks[0]
	if task.Type != taskTypeDeriveThumbnail {
		t.Fatalf("task type = %q, want %q", task.Type, taskTypeDeriveThumbnail)
	}
	var payload deriveThumbnailTaskPayload
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		t.Fatalf("task payload does not decode as a thumbnail-derive payload: %v", err)
	}
	if payload.ObjectID != row.ID {
		t.Fatalf("task payload names object %q, want %q", payload.ObjectID, row.ID)
	}

	job := &jobs.Job{Type: task.Type, TenantID: task.TenantID, Payload: task.Payload}
	h := deriveHandler{svc: derive}
	result, err := h.Handle(serviceCtx(task.TenantID), job, nil)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(result.Data) != 0 {
		t.Errorf("Handle returned %d result bytes, want none", len(result.Data))
	}
	assertThumbnailRow(t, derive, store, serviceCtx("tenant-a"), row.ID, "image/jpeg", 320, 240)
}

// TestDeriveHandler_Handle_RefusesAMalformedTask proves the two task-shape
// violations fail the job -- the queue retries and eventually dead-letters
// them, which is correct: such a task can never succeed by re-running, and
// the failure is a wiring bug worth surfacing, not a skip.
func TestDeriveHandler_Handle_RefusesAMalformedTask(t *testing.T) {
	derive, _, _, _, _ := newDeriveHarness(t, nil)
	h := deriveHandler{svc: derive}
	ctx := serviceCtx("tenant-a")

	if _, err := h.Handle(ctx, &jobs.Job{Type: taskTypeDeriveThumbnail, Payload: []byte("{not json")}, nil); err == nil ||
		!strings.Contains(err.Error(), "undecodable thumbnail-derive task payload") {
		t.Errorf("undecodable payload error = %v, want the undecodable-payload error", err)
	}

	if _, err := h.Handle(ctx, &jobs.Job{Type: taskTypeDeriveThumbnail, Payload: []byte(`{}`)}, nil); err == nil ||
		!strings.Contains(err.Error(), "empty object_id") {
		t.Errorf("empty object_id error = %v, want the empty-object_id error", err)
	}
}

// TestDeriveHandler_Handle_ConvergesWhenTheObjectIsDeletedBeforeTheRun
// proves the task loop's convergence: a task whose object the delete
// protocol removed between the enqueue and the dispatch completes cleanly --
// the handler must not fail the job for an object that was never going to
// have a thumbnail.
func TestDeriveHandler_Handle_ConvergesWhenTheObjectIsDeletedBeforeTheRun(t *testing.T) {
	derive, svc, store, queue, _ := newDeriveHarness(t, nil)
	ctx := serviceCtx("tenant-a")
	row := createAndUpload(t, svc, ctx, testutil.PNG(t, 64, 48), "image/png")
	if _, err := svc.Complete(ctx, row.ID); err != nil {
		t.Fatalf("Complete(%s): %v", row.ID, err)
	}
	task := queue.tasks[0]

	// The delete protocol, run to completion before the handler.
	if _, err := svc.objects.markDeleting(ctx, row.ID); err != nil {
		t.Fatalf("markDeleting(%s): %v", row.ID, err)
	}
	if err := svc.objects.deleteObjectRows(ctx, row.ID); err != nil {
		t.Fatalf("deleteObjectRows(%s): %v", row.ID, err)
	}
	if err := store.DeleteObject(ctx, row.Key); err != nil {
		t.Fatalf("DeleteObject(%s): %v", row.Key, err)
	}

	h := deriveHandler{svc: derive}
	if _, err := h.Handle(serviceCtx(task.TenantID), &jobs.Job{Type: task.Type, TenantID: task.TenantID, Payload: task.Payload}, nil); err != nil {
		t.Fatalf("Handle of a task whose object is gone: %v, want nil", err)
	}
	rows, err := derive.derivatives.listByObject(ctx, row.ID)
	if err != nil {
		t.Fatalf("listByObject(%s): %v", row.ID, err)
	}
	if len(rows) != 0 {
		t.Errorf("object %s gained %d derivative rows from a converged task", row.ID, len(rows))
	}
	if _, ok := store.bytes(thumbKey(t, "tenant-a", row.ID)); ok {
		t.Error("the converged task left thumbnail bytes behind")
	}
}
