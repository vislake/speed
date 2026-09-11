package aigateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/storage"
)

// fakeImageProvider is an ImageProvider test double that records every
// call it receives and answers with fixed, injectable results.
type fakeImageProvider struct {
	textToImageCalls, imageToImageCalls, inpaintCalls int
	lastTextToImage                                   TextToImageRequest
	lastImageToImage                                  ImageToImageRequest
	lastInpaint                                       InpaintRequest

	result ImageResult
	err    error
}

func (f *fakeImageProvider) TextToImage(_ context.Context, req TextToImageRequest) (ImageResult, error) {
	f.textToImageCalls++
	f.lastTextToImage = req
	return f.result, f.err
}

func (f *fakeImageProvider) ImageToImage(_ context.Context, req ImageToImageRequest) (ImageResult, error) {
	f.imageToImageCalls++
	f.lastImageToImage = req
	return f.result, f.err
}

func (f *fakeImageProvider) Inpaint(_ context.Context, req InpaintRequest) (ImageResult, error) {
	f.inpaintCalls++
	f.lastInpaint = req
	return f.result, f.err
}

var _ ImageProvider = (*fakeImageProvider)(nil)

const fakeImageProviderName = "image.fake-test-provider"

// newFakeImageRegistry returns a *pkgcore.SeamRegistry[ImageProvider]
// containing only provider, registered under fakeImageProviderName --
// isolating a test from the process-global ImageProviderRegistry's real
// registrations, mirroring newFakeGatewayRegistry's identical role for
// chat.
func newFakeImageRegistry(t *testing.T, provider ImageProvider) *pkgcore.SeamRegistry[ImageProvider] {
	t.Helper()
	reg := pkgcore.NewSeamRegistry[ImageProvider]()
	if err := reg.Register(pkgcore.Registration[ImageProvider]{
		Name:         fakeImageProviderName,
		Capabilities: pkgcore.Stateless,
		New:          func(pkgcore.Config) (ImageProvider, error) { return provider, nil },
	}); err != nil {
		t.Fatalf("register fake image provider: %v", err)
	}
	return reg
}

// recordingImageQueue is a jobs.Queue test double recording the last Task
// Enqueue received and answering with a fixed JobID. This package's own
// pipeline tests never need a real queue -- the job handler is invoked
// directly in this file's tests -- so this double exists only to prove
// GenerateImage's own enqueue call.
type recordingImageQueue struct {
	calls    int
	lastTask jobs.Task
	jobID    jobs.JobID
	err      error
}

func (q *recordingImageQueue) Enqueue(_ context.Context, task jobs.Task, _ ...jobs.EnqueueOption) (jobs.JobID, error) {
	q.calls++
	q.lastTask = task
	return q.jobID, q.err
}
func (q *recordingImageQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) { return nil, nil }
func (q *recordingImageQueue) Cancel(context.Context, jobs.JobID) error           { return nil }

var _ jobs.Queue = (*recordingImageQueue)(nil)

// newTestStorageObjectService returns a real, fully bootstrapped
// *storage.ObjectService over a fresh, per-test SQLite database -- a real
// local ObjectStore included (via pkgcore.NewKernel().Bootstrap), so the
// job handler's readImageObject/writeImageObject actually exercise real
// storage I/O rather than a fake. The storage module's own queue is a
// no-op: this package's tests only care about the thumbnail-derive task
// storage's own Complete enqueues never actually running.
type noopStorageQueue struct{}

func (noopStorageQueue) Enqueue(context.Context, jobs.Task, ...jobs.EnqueueOption) (jobs.JobID, error) {
	return "", nil
}
func (noopStorageQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) { return nil, nil }
func (noopStorageQueue) Cancel(context.Context, jobs.JobID) error           { return nil }

func newTestStorageObjectService(t *testing.T) *storage.ObjectService {
	t.Helper()
	return newTestStorageObjectServiceWithStore(t, pkgcore.NewLocalObjectStore(t.TempDir()))
}

// newTestStorageObjectServiceWithStore is newTestStorageObjectService's own
// generalization: store backs the storage.ObjectService the job handler's
// readImageObject/writeImageObject actually exercises, instead of the
// kernel's own default local store -- letting a test inject an
// pkgcore.ObjectStore double (failNPutObjectStore below) for the
// after-vendor-success failure path RetryAfterVendorSuccess tests need,
// which a real, unconditionally-succeeding local store cannot express.
func newTestStorageObjectServiceWithStore(t *testing.T, store pkgcore.ObjectStore) *storage.ObjectService {
	t.Helper()

	dsn := "file:aigateway_image_test_" + t.Name() + "?mode=memory&cache=shared"
	db, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	storageModule := storage.NewModule(db, storage.WithQueue(noopStorageQueue{}))
	migrations := dbkit.NewMigrationRegistry()
	if err := migrations.Register(storageModule); err != nil {
		t.Fatalf("register storage migrations: %v", err)
	}
	if err := migrations.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply storage migrations: %v", err)
	}
	// capabilities 0: the identical value pkgcore's own "objectstore.local"
	// builtin registers (objectstore_registry.go) -- irrelevant here
	// regardless, since these tests never set WithDeploymentMode and a
	// single-process deployment excludes no implementation.
	if _, err := pkgcore.NewKernel(pkgcore.WithObjectStore(store, 0)).Bootstrap(context.Background(), storageModule); err != nil {
		t.Fatalf("bootstrap storage module: %v", err)
	}
	return storageModule.ObjectService()
}

// failNPutObjectStore wraps a real pkgcore.ObjectStore, failing exactly the
// first n calls to PutObject (storage.ObjectService.Upload's own seam call)
// and delegating every other call, and every other method, unchanged. It
// lets a test deterministically reproduce "the vendor call succeeded, then
// the storage write failed" -- the failure imageGenerateHandler.Handle's
// idempotency invariant exists for -- without a wall-clock sleep or real
// SQLITE_BUSY contention: any go/storage write failure after a successful
// vendor call is the same failure from Handle's point of view, and
// PutObject is the natural, minimal seam to inject one at.
type failNPutObjectStore struct {
	inner pkgcore.ObjectStore
	n     int

	calls int
}

func (s *failNPutObjectStore) PutObject(ctx context.Context, key string, r io.Reader) error {
	s.calls++
	if s.calls <= s.n {
		return fmt.Errorf("aigateway test: injected storage write failure (call %d of %d)", s.calls, s.n)
	}
	return s.inner.PutObject(ctx, key, r)
}

func (s *failNPutObjectStore) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.inner.GetObject(ctx, key)
}

func (s *failNPutObjectStore) DeleteObject(ctx context.Context, key string) error {
	return s.inner.DeleteObject(ctx, key)
}

var _ pkgcore.ObjectStore = (*failNPutObjectStore)(nil)

// imageGatewayTestFixture builds a Gateway wired to a fake ImageProvider
// (routed under "image:default"), a real CredentialService over a fresh
// test database, a stored platform credential, a recordingImageQueue and a
// real storage.ObjectService -- the common setup every image pipeline test
// below starts from, mirroring gatewayTestFixture's identical role for
// chat.
func imageGatewayTestFixture(t *testing.T, provider *fakeImageProvider, opts ...GatewayOption) (*Gateway, *recordingImageQueue, *storage.ObjectService) {
	t.Helper()
	return imageGatewayTestFixtureWithObjects(t, provider, newTestStorageObjectService(t), opts...)
}

// imageGatewayTestFixtureWithObjects is imageGatewayTestFixture's own
// generalization: objects backs the Gateway's WithImageGeneration wiring
// directly, letting a test pass one built over
// newTestStorageObjectServiceWithStore's injectable store instead of
// imageGatewayTestFixture's own always-real one.
func imageGatewayTestFixtureWithObjects(t *testing.T, provider *fakeImageProvider, objects *storage.ObjectService, opts ...GatewayOption) (*Gateway, *recordingImageQueue, *storage.ObjectService) {
	t.Helper()
	credentials := NewCredentialService(newTestDB(t))
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if err := credentials.SetPlatformCredential(sysCtx, fakeImageProviderName, "sk-test", ""); err != nil {
		t.Fatalf("SetPlatformCredential: %v", err)
	}

	queue := &recordingImageQueue{jobID: "job-fixture-1"}

	allOpts := append([]GatewayOption{
		WithModelRoute("image:default", fakeImageProviderName, "vendor-model-x"),
		WithImageProviderRegistry(newFakeImageRegistry(t, provider)),
		WithImageGeneration(queue, objects),
	}, opts...)
	return NewGateway(credentials, allOpts...), queue, objects
}

func imageReq() ImageRequest {
	return ImageRequest{Model: "image:default", Operation: ImageOperationTextToImage, Prompt: "a bright smile"}
}

// --- GenerateImage: routing, entitlements, wiring ---------------------------

func TestGateway_GenerateImage_UnroutedModel_Refused(t *testing.T) {
	provider := &fakeImageProvider{}
	g, queue, _ := imageGatewayTestFixture(t, provider)

	// The call carries a tenant: the tenant requirement is the pipeline's
	// first check (a tenantless call is refused with ErrImageRequiresTenant
	// before routing is ever consulted -- the dedicated tenantless tests
	// pin that), so this test drives the routing refusal on a tenantful
	// call.
	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	_, err := g.GenerateImage(tenantCtx, ImageRequest{
		Model: "image:unrouted", Operation: ImageOperationTextToImage, Prompt: "x",
	})
	if got, ok := apperrCode(err); !ok || got != ErrUnroutedModel.Code {
		t.Fatalf("GenerateImage err = %v, want ErrUnroutedModel", err)
	}
	if queue.calls != 0 {
		t.Fatalf("queue was called %d times for an unrouted model, want 0", queue.calls)
	}
}

func TestGateway_GenerateImage_EntitlementDenied_NeverEnqueues(t *testing.T) {
	provider := &fakeImageProvider{}
	denied := EntitlementsFunc(func(context.Context, string, int64) (Decision, error) {
		return Decision{Allowed: false, Reason: "no_subscription"}, nil
	})
	g, queue, _ := imageGatewayTestFixture(t, provider, WithEntitlements(denied))

	// Tenantful, for the identical reason the unrouted-model test above
	// states: this test isolates the entitlement gate, not the tenant
	// requirement.
	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	_, err := g.GenerateImage(tenantCtx, imageReq())
	if got, ok := apperrCode(err); !ok || got != ErrEntitlementDenied.Code {
		t.Fatalf("GenerateImage err = %v, want ErrEntitlementDenied", err)
	}
	if queue.calls != 0 {
		t.Fatalf("queue was called %d times for a denied entitlement, want 0 -- a refused caller must never be billed or enqueued", queue.calls)
	}
}

func TestGateway_GenerateImage_NoImageGenerationWired_Refused(t *testing.T) {
	provider := &fakeImageProvider{}
	credentials := NewCredentialService(newTestDB(t))
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if setErr := credentials.SetPlatformCredential(sysCtx, fakeImageProviderName, "sk-test", ""); setErr != nil {
		t.Fatalf("SetPlatformCredential: %v", setErr)
	}
	// No WithImageGeneration: a Gateway built for chat-only use.
	g := NewGateway(credentials,
		WithModelRoute("image:default", fakeImageProviderName, "vendor-model-x"),
		WithImageProviderRegistry(newFakeImageRegistry(t, provider)),
	)

	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	_, err = g.GenerateImage(tenantCtx, imageReq())
	if got, ok := apperrCode(err); !ok || got != ErrImageGenerationUnavailable.Code {
		t.Fatalf("GenerateImage err = %v, want ErrImageGenerationUnavailable", err)
	}
}

func TestGateway_GenerateImage_NoTenant_Refused(t *testing.T) {
	provider := &fakeImageProvider{}
	g, queue, _ := imageGatewayTestFixture(t, provider)

	_, err := g.GenerateImage(context.Background(), imageReq())
	if got, ok := apperrCode(err); !ok || got != ErrImageRequiresTenant.Code {
		t.Fatalf("GenerateImage err = %v, want ErrImageRequiresTenant", err)
	}
	if queue.calls != 0 {
		t.Fatalf("queue was called %d times with no tenant, want 0", queue.calls)
	}
}

func TestGateway_GenerateImage_Success_EnqueuesTaskAndReturnsJobID(t *testing.T) {
	provider := &fakeImageProvider{}
	g, queue, _ := imageGatewayTestFixture(t, provider)

	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	jobID, err := g.GenerateImage(tenantCtx, imageReq())
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if jobID != queue.jobID {
		t.Fatalf("GenerateImage returned %q, want the queue's job id %q", jobID, queue.jobID)
	}
	if queue.calls != 1 {
		t.Fatalf("queue was called %d times, want 1", queue.calls)
	}
	if queue.lastTask.Type != TaskTypeImageGenerate {
		t.Fatalf("enqueued task type = %q, want %q", queue.lastTask.Type, TaskTypeImageGenerate)
	}
	if queue.lastTask.TenantID != "tenant-acme" {
		t.Fatalf("enqueued task tenant = %q, want %q", queue.lastTask.TenantID, "tenant-acme")
	}
	if provider.textToImageCalls != 0 {
		t.Fatalf("provider was called %d times synchronously by GenerateImage itself, want 0 -- image generation is async-only", provider.textToImageCalls)
	}

	var payload imageGenerateTaskPayload
	if err := json.Unmarshal(queue.lastTask.Payload, &payload); err != nil {
		t.Fatalf("decode enqueued payload: %v", err)
	}
	if payload.Model != "image:default" || payload.Operation != string(ImageOperationTextToImage) || payload.Prompt != "a bright smile" {
		t.Fatalf("enqueued payload = %+v, want the request's own fields", payload)
	}
}

// --- The job handler: real storage I/O, provider dispatch, usage ----------

// createCompletedObject declares, uploads and completes one object through
// objects, returning its id -- the test's own stand-in for a caller having
// already uploaded a photo through storage's real HTTP surface.
func createCompletedObject(t *testing.T, ctx context.Context, objects *storage.ObjectService, content []byte, mime string) string {
	t.Helper()
	created, err := objects.Create(ctx, storage.CreateParams{DeclaredSize: int64(len(content)), DeclaredType: mime})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := objects.Upload(ctx, created.ID, nil, bytes.NewReader(content)); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if _, err := objects.Complete(ctx, created.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	return created.ID
}

// tinyValidJPEG is a minimal real JPEG (a 4x4 image encoded with the
// stdlib encoder), so storage's own revalidation pipeline (which decodes
// image bytes) accepts it -- unlike an arbitrary byte string.
var tinyValidJPEG = func() []byte {
	// Encoded once at package init so every test in this file that needs a
	// real, decodable JPEG shares the same bytes.
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 60), G: uint8(y * 60), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		panic("aigateway test: encode tiny fixture jpeg: " + err.Error())
	}
	return buf.Bytes()
}()

func runImageGenerateJob(t *testing.T, g *Gateway, tenant pkgcore.TenantID, payload imageGenerateTaskPayload) (jobs.Result, error) {
	t.Helper()
	handler, ok := g.imageJobHandler()
	if !ok {
		t.Fatal("imageJobHandler() reported image generation not wired")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal task payload: %v", err)
	}
	ctx := pkgcore.WithTenant(context.Background(), tenant)
	job := &jobs.Job{ID: "job-under-test", Type: TaskTypeImageGenerate, TenantID: tenant, Payload: raw}
	return handler.Handle(ctx, job, func(int, string) {})
}

func TestImageGenerateHandler_TextToImage_WritesNewObjectAndReportsUsage(t *testing.T) {
	provider := &fakeImageProvider{result: ImageResult{
		Image: ImageBytes{Content: tinyPNG, MIME: "image/png"},
		Usage: ImageUsage{ImageCount: 1, Steps: 28, ResolutionTier: "1024x1024"},
	}}
	var recordedEvents []UsageEvent
	recorder := UsageRecorderFunc(func(_ context.Context, event UsageEvent) error {
		recordedEvents = append(recordedEvents, event)
		return nil
	})
	g, _, objects := imageGatewayTestFixture(t, provider, WithUsageRecorder(recorder))

	result, err := runImageGenerateJob(t, g, "tenant-acme", imageGenerateTaskPayload{
		Model: "image:default", Operation: string(ImageOperationTextToImage), Prompt: "a bright smile",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if provider.textToImageCalls != 1 {
		t.Fatalf("provider.TextToImage called %d times, want 1", provider.textToImageCalls)
	}
	if provider.lastTextToImage.Model != "vendor-model-x" {
		t.Fatalf("provider saw Model = %q, want the resolved vendor model", provider.lastTextToImage.Model)
	}

	var jobResult ImageJobResult
	if decErr := json.Unmarshal(result.Data, &jobResult); decErr != nil {
		t.Fatalf("decode ImageJobResult: %v", decErr)
	}
	if jobResult.OutputObjectID == "" {
		t.Fatal("ImageJobResult carries no OutputObjectID")
	}
	if jobResult.Usage != (ImageUsage{ImageCount: 1, Steps: 28, ResolutionTier: "1024x1024"}) {
		t.Fatalf("ImageJobResult.Usage = %+v", jobResult.Usage)
	}

	// The output object is a real, completed storage object.
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	obj, err := objects.Get(ctx, jobResult.OutputObjectID)
	if err != nil {
		t.Fatalf("Get(output object): %v", err)
	}
	if obj.State != storage.ObjectStateCompleted {
		t.Fatalf("output object state = %q, want completed", obj.State)
	}
	if obj.MIME == nil || *obj.MIME != "image/png" {
		t.Fatalf("output object mime = %v, want image/png", obj.MIME)
	}

	// Usage was reported under the image dimensions, never the chat one.
	var sawCount, sawSteps bool
	for _, e := range recordedEvents {
		if e.Feature == usageFeatureImageCount {
			sawCount = true
			if e.Quantity != 1 {
				t.Fatalf("image_count usage quantity = %v, want 1", e.Quantity)
			}
			if e.Metadata["resolution_tier"] != "1024x1024" {
				t.Fatalf("image_count usage metadata = %+v, want resolution_tier=1024x1024", e.Metadata)
			}
		}
		if e.Feature == usageFeatureImageSteps {
			sawSteps = true
			if e.Quantity != 28 {
				t.Fatalf("image_steps usage quantity = %v, want 28", e.Quantity)
			}
		}
		if e.Feature == usageFeatureChatTokens {
			t.Fatal("image generation must never report the chat token dimension")
		}
	}
	if !sawCount || !sawSteps {
		t.Fatalf("recorded events = %+v, want both %q and %q", recordedEvents, usageFeatureImageCount, usageFeatureImageSteps)
	}
}

func TestImageGenerateHandler_ImageToImage_ReadsInputWritesNewOutput(t *testing.T) {
	provider := &fakeImageProvider{result: ImageResult{
		Image: ImageBytes{Content: tinyPNG, MIME: "image/png"},
		Usage: ImageUsage{ImageCount: 1},
	}}
	g, _, objects := imageGatewayTestFixture(t, provider)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	inputID := createCompletedObject(t, ctx, objects, tinyValidJPEG, "image/jpeg")

	result, err := runImageGenerateJob(t, g, "tenant-acme", imageGenerateTaskPayload{
		Model: "image:default", Operation: string(ImageOperationImageToImage),
		Prompt: "simulate a smile", InputObjectID: inputID,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if provider.imageToImageCalls != 1 {
		t.Fatalf("provider.ImageToImage called %d times, want 1", provider.imageToImageCalls)
	}
	if provider.lastImageToImage.Input.MIME != "image/jpeg" {
		t.Fatalf("provider saw input MIME = %q, want image/jpeg", provider.lastImageToImage.Input.MIME)
	}
	if len(provider.lastImageToImage.Input.Content) == 0 {
		t.Fatal("provider saw an empty input image")
	}

	var jobResult ImageJobResult
	if err := json.Unmarshal(result.Data, &jobResult); err != nil {
		t.Fatalf("decode ImageJobResult: %v", err)
	}
	if jobResult.OutputObjectID == inputID {
		t.Fatal("OutputObjectID must never equal InputObjectID -- the job handler must never overwrite the input")
	}
	if _, err := objects.Get(ctx, jobResult.OutputObjectID); err != nil {
		t.Fatalf("the output object must be readable back: %v", err)
	}
	if _, err := objects.Get(ctx, inputID); err != nil {
		t.Fatalf("the input object must still be readable, unmodified: %v", err)
	}
}

func TestImageGenerateHandler_Inpaint_ReadsInputAndMask(t *testing.T) {
	provider := &fakeImageProvider{result: ImageResult{Image: ImageBytes{Content: tinyPNG, MIME: "image/png"}}}
	g, _, objects := imageGatewayTestFixture(t, provider)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	inputID := createCompletedObject(t, ctx, objects, tinyValidJPEG, "image/jpeg")
	maskID := createCompletedObject(t, ctx, objects, tinyPNG, "image/png")

	_, err := runImageGenerateJob(t, g, "tenant-acme", imageGenerateTaskPayload{
		Model: "image:default", Operation: string(ImageOperationInpaint),
		Prompt: "fix the teeth", InputObjectID: inputID, MaskObjectID: maskID,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if provider.inpaintCalls != 1 {
		t.Fatalf("provider.Inpaint called %d times, want 1", provider.inpaintCalls)
	}
	if provider.lastInpaint.Mask.MIME != "image/png" {
		t.Fatalf("provider saw mask MIME = %q, want image/png", provider.lastInpaint.Mask.MIME)
	}
	if len(provider.lastInpaint.Input.Content) == 0 || len(provider.lastInpaint.Mask.Content) == 0 {
		t.Fatal("provider saw an empty input or mask")
	}
}

func TestImageGenerateHandler_MissingInputObject_Refused(t *testing.T) {
	provider := &fakeImageProvider{result: ImageResult{Image: ImageBytes{Content: tinyPNG, MIME: "image/png"}}}
	g, _, _ := imageGatewayTestFixture(t, provider)

	_, err := runImageGenerateJob(t, g, "tenant-acme", imageGenerateTaskPayload{
		Model: "image:default", Operation: string(ImageOperationImageToImage),
		Prompt: "x", InputObjectID: "does-not-exist",
	})
	if got, ok := apperrCode(err); !ok || got != ErrImageObjectUnavailable.Code {
		t.Fatalf("Handle err = %v, want ErrImageObjectUnavailable", err)
	}
	if provider.imageToImageCalls != 0 {
		t.Fatalf("provider was called %d times for a missing input object, want 0", provider.imageToImageCalls)
	}
}

func TestImageGenerateHandler_MalformedPayload_Refused(t *testing.T) {
	provider := &fakeImageProvider{}
	g, _, _ := imageGatewayTestFixture(t, provider)

	handler, ok := g.imageJobHandler()
	if !ok {
		t.Fatal("imageJobHandler() reported image generation not wired")
	}
	job := &jobs.Job{ID: "job-bad", Type: TaskTypeImageGenerate, TenantID: "tenant-acme", Payload: []byte("not json")}
	_, err := handler.Handle(pkgcore.WithTenant(context.Background(), "tenant-acme"), job, func(int, string) {})
	if err == nil {
		t.Fatal("Handle with a malformed payload succeeded, want an error")
	}
}

func TestImageGenerateHandler_ProviderError_Refused(t *testing.T) {
	provider := &fakeImageProvider{err: ErrProviderRequestFailed}
	g, _, _ := imageGatewayTestFixture(t, provider)

	_, err := runImageGenerateJob(t, g, "tenant-acme", imageGenerateTaskPayload{
		Model: "image:default", Operation: string(ImageOperationTextToImage), Prompt: "x",
	})
	if got, ok := apperrCode(err); !ok || got != ErrProviderRequestFailed.Code {
		t.Fatalf("Handle err = %v, want ErrProviderRequestFailed", err)
	}
}

// --- Job idempotency: a retry must never re-bill the vendor or double-
// record usage ----------------------------------------------

// TestImageGenerateHandler_RetryAfterVendorSuccess_DoesNotRecallVendorOrDoubleRecordUsage
// pins the job-idempotency invariant: it reproduces "the vendor call
// succeeds, then the storage write fails" (via failNPutObjectStore, the
// same failure shape the marker table exists for), drives the queue's own
// retry by calling Handle a second time for the SAME *jobs.Job (job.ID
// never changes between attempts -- jobs.Job.ID's own doc comment), and
// asserts the vendor was invoked exactly once and usage was recorded
// exactly once across both attempts.
//
// The assertion is load-bearing: without the job-idempotency marker, the
// second Handle call would unconditionally re-resolve the route and call
// ImageProvider.TextToImage again, making provider.textToImageCalls 2
// after both attempts (this test wants 1).
func TestImageGenerateHandler_RetryAfterVendorSuccess_DoesNotRecallVendorOrDoubleRecordUsage(t *testing.T) {
	provider := &fakeImageProvider{result: ImageResult{
		Image: ImageBytes{Content: tinyPNG, MIME: "image/png"},
		Usage: ImageUsage{ImageCount: 1, Steps: 20, ResolutionTier: "512x512"},
	}}
	var recordedEvents []UsageEvent
	recorder := UsageRecorderFunc(func(_ context.Context, event UsageEvent) error {
		recordedEvents = append(recordedEvents, event)
		return nil
	})
	failingStore := &failNPutObjectStore{inner: pkgcore.NewLocalObjectStore(t.TempDir()), n: 1}
	objects := newTestStorageObjectServiceWithStore(t, failingStore)
	g, _, _ := imageGatewayTestFixtureWithObjects(t, provider, objects, WithUsageRecorder(recorder))

	handler, ok := g.imageJobHandler()
	if !ok {
		t.Fatal("imageJobHandler() reported image generation not wired")
	}
	raw, err := json.Marshal(imageGenerateTaskPayload{
		Model: "image:default", Operation: string(ImageOperationTextToImage), Prompt: "a bright smile",
	})
	if err != nil {
		t.Fatalf("marshal task payload: %v", err)
	}
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	job := &jobs.Job{ID: "job-retry-1", Type: TaskTypeImageGenerate, TenantID: "tenant-acme", Payload: raw}

	// First attempt: the vendor call succeeds, but the storage write fails
	// (the injected PutObject failure) -- exactly the audited scenario: a
	// failure AFTER a successful vendor call.
	if _, firstErr := handler.Handle(ctx, job, func(int, string) {}); firstErr == nil {
		t.Fatal("first attempt succeeded, want the injected storage failure to surface")
	}
	if provider.textToImageCalls != 1 {
		t.Fatalf("provider called %d times after the first attempt, want 1", provider.textToImageCalls)
	}
	if len(recordedEvents) != 0 {
		t.Fatalf("usage recorded after a failed attempt (before the object was ever written) = %+v, want none", recordedEvents)
	}

	// The queue retries the SAME job -- job.ID is never regenerated across
	// a retry (jobs.Job.ID's own doc comment); Attempts incrementing is
	// this test mirroring go/jobs' own bookkeeping, not something Handle
	// itself reads.
	job.Attempts++
	result, err := handler.Handle(ctx, job, func(int, string) {})
	if err != nil {
		t.Fatalf("retry attempt: %v", err)
	}

	// The invariant itself.
	if provider.textToImageCalls != 1 {
		t.Fatalf("provider called %d times across both attempts, want exactly 1 -- a retry after a successful vendor call must never call the vendor again", provider.textToImageCalls)
	}
	var imageCountEvents, imageStepsEvents int
	var imageCountKey, imageStepsKey string
	for _, e := range recordedEvents {
		switch e.Feature {
		case usageFeatureImageCount:
			imageCountEvents++
			imageCountKey = e.IdempotencyKey
		case usageFeatureImageSteps:
			imageStepsEvents++
			imageStepsKey = e.IdempotencyKey
		}
	}
	if imageCountEvents != 1 || imageStepsEvents != 1 {
		t.Fatalf("recorded usage events = %+v (image_count=%d, image_steps=%d), want exactly one of each -- a retry must never double-record usage", recordedEvents, imageCountEvents, imageStepsEvents)
	}
	if imageCountKey == "" || imageStepsKey == "" {
		t.Fatal("recorded usage events carry an empty IdempotencyKey")
	}
	if imageCountKey == imageStepsKey {
		t.Fatalf("image_count and image_steps events share one IdempotencyKey %q, want distinct keys per dimension", imageCountKey)
	}

	var jobResult ImageJobResult
	if err := json.Unmarshal(result.Data, &jobResult); err != nil {
		t.Fatalf("decode ImageJobResult: %v", err)
	}
	if jobResult.OutputObjectID == "" {
		t.Fatal("ImageJobResult carries no OutputObjectID after the successful retry")
	}
	if jobResult.Usage != (ImageUsage{ImageCount: 1, Steps: 20, ResolutionTier: "512x512"}) {
		t.Fatalf("ImageJobResult.Usage after the successful retry = %+v", jobResult.Usage)
	}
}

// TestImageGenerateHandler_HandleCalledAgainAfterSuccess_SkipsVendorAndUsage
// covers the OTHER half of the marker state machine the retry test above
// does not reach: a Handle call for a job that has already fully
// succeeded (status "completed"). This is a stronger property than "a
// retry after a mid-flight failure" -- it proves the short-circuit at the
// very top of Handle answers from the row alone, with no vendor call, no
// storage write and no usage record, for a job nothing has any reason to
// retry at all.
func TestImageGenerateHandler_HandleCalledAgainAfterSuccess_SkipsVendorAndUsage(t *testing.T) {
	provider := &fakeImageProvider{result: ImageResult{
		Image: ImageBytes{Content: tinyPNG, MIME: "image/png"},
		Usage: ImageUsage{ImageCount: 1, Steps: 5},
	}}
	var recordedEvents []UsageEvent
	recorder := UsageRecorderFunc(func(_ context.Context, event UsageEvent) error {
		recordedEvents = append(recordedEvents, event)
		return nil
	})
	g, _, _ := imageGatewayTestFixture(t, provider, WithUsageRecorder(recorder))

	payload := imageGenerateTaskPayload{Model: "image:default", Operation: string(ImageOperationTextToImage), Prompt: "a bright smile"}

	first, err := runImageGenerateJob(t, g, "tenant-acme", payload)
	if err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	second, err := runImageGenerateJob(t, g, "tenant-acme", payload)
	if err != nil {
		t.Fatalf("second Handle (redelivery of an already-succeeded job): %v", err)
	}

	if provider.textToImageCalls != 1 {
		t.Fatalf("provider called %d times across two Handle calls for the same already-succeeded job, want 1", provider.textToImageCalls)
	}
	var imageCountEvents int
	for _, e := range recordedEvents {
		if e.Feature == usageFeatureImageCount {
			imageCountEvents++
		}
	}
	if imageCountEvents != 1 {
		t.Fatalf("image_count usage recorded %d times across two Handle calls, want 1", imageCountEvents)
	}
	if !bytes.Equal(first.Data, second.Data) {
		t.Fatalf("Handle's second (redelivered) result = %s, want the same as the first %s", second.Data, first.Data)
	}
}

// TestImageUsageIdempotencyKey_StablePerJobAndFeature is the direct,
// job-machinery-free proof of the key-derivation contract: the same job id
// and feature always converge on the same key (the property a
// UsageRecorder-side dedup needs), while either input changing changes the
// key.
func TestImageUsageIdempotencyKey_StablePerJobAndFeature(t *testing.T) {
	k1 := imageUsageIdempotencyKey("job-1", usageFeatureImageCount)
	k2 := imageUsageIdempotencyKey("job-1", usageFeatureImageCount)
	if k1 != k2 {
		t.Fatalf("imageUsageIdempotencyKey(%q, %q) = %q then %q, want the same value both times", "job-1", usageFeatureImageCount, k1, k2)
	}
	if k1 == "" {
		t.Fatal("imageUsageIdempotencyKey returned an empty key")
	}

	if got := imageUsageIdempotencyKey("job-1", usageFeatureImageSteps); got == k1 {
		t.Fatalf("imageUsageIdempotencyKey(%q, %q) = %q, want it to differ from the %q dimension's key", "job-1", usageFeatureImageSteps, got, usageFeatureImageCount)
	}
	if got := imageUsageIdempotencyKey("job-2", usageFeatureImageCount); got == k1 {
		t.Fatalf("imageUsageIdempotencyKey(%q, ...) = %q, want it to differ from job-1's key", "job-2", got)
	}
}

// --- Adversarial idempotency audit: a transient claim-write failure and a
// concurrent redelivery must never let the vendor be called twice --------

// TestImageGenerateHandler_ClaimInsertFails_RetryCallsVendorExactlyOnce
// reproduces an ordinary, non-crash transient failure of the FIRST write
// this package's idempotency mechanism makes -- claimPending's own INSERT
// -- standing in for real SQLITE_BUSY-class contention, via a SQLite
// trigger that aborts every INSERT into ai_gateway_image_jobs for the
// whole duration of the first attempt.
//
// The claim-before-vendor-call ordering (image_job_store.go's own doc
// comment) is what makes a blocked claim a clean first-attempt failure:
// claimPending runs BEFORE ImageProvider is ever called, so the FIRST
// attempt fails before reaching the vendor at all -- a marker written only
// after a successful vendor call would let a transient failure of that
// write reopen the double-vendor-call window. The invariant this test
// cares about: across every attempt for one job, the vendor is called AT
// MOST once.
func TestImageGenerateHandler_ClaimInsertFails_RetryCallsVendorExactlyOnce(t *testing.T) {
	provider := &fakeImageProvider{result: ImageResult{
		Image: ImageBytes{Content: tinyPNG, MIME: "image/png"},
		Usage: ImageUsage{ImageCount: 1, Steps: 20, ResolutionTier: "512x512"},
	}}
	g, _, _ := imageGatewayTestFixture(t, provider)

	// Block every INSERT into the marker table -- standing in for an
	// ordinary transient write failure on claimPending's own INSERT.
	if err := g.imageJobs.db.Exec(
		`CREATE TRIGGER block_image_job_insert BEFORE INSERT ON ai_gateway_image_jobs
		 BEGIN SELECT RAISE(ABORT, 'aigateway audit: injected claimPending failure'); END;`,
	).Error; err != nil {
		t.Fatalf("install blocking trigger: %v", err)
	}

	handler, ok := g.imageJobHandler()
	if !ok {
		t.Fatal("imageJobHandler() reported image generation not wired")
	}
	raw, err := json.Marshal(imageGenerateTaskPayload{
		Model: "image:default", Operation: string(ImageOperationTextToImage), Prompt: "a bright smile",
	})
	if err != nil {
		t.Fatalf("marshal task payload: %v", err)
	}
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	job := &jobs.Job{ID: "job-claim-fail-1", Type: TaskTypeImageGenerate, TenantID: "tenant-acme", Payload: raw}

	// First attempt: the claim itself is blocked, so this attempt must
	// fail BEFORE ever calling the vendor.
	if _, firstErr := handler.Handle(ctx, job, func(int, string) {}); firstErr == nil {
		t.Fatal("first attempt succeeded, want the injected claimPending failure to surface")
	}
	if provider.textToImageCalls != 0 {
		t.Fatalf("provider called %d times after the first (claim-blocked) attempt, want 0 -- a claim failure must happen before any vendor call", provider.textToImageCalls)
	}

	// Unblock the table -- the underlying transient condition has cleared,
	// mirroring how a real SQLITE_BUSY/lock-contention window closes on
	// its own before the queue's next retry runs.
	if err := g.imageJobs.db.Exec(`DROP TRIGGER block_image_job_insert;`).Error; err != nil {
		t.Fatalf("remove blocking trigger: %v", err)
	}

	job.Attempts++
	if _, err := handler.Handle(ctx, job, func(int, string) {}); err != nil {
		t.Fatalf("retry attempt: %v", err)
	}

	if provider.textToImageCalls != 1 {
		t.Fatalf("provider called %d times across both attempts, want exactly 1 -- "+
			"a claimPending INSERT failure (an ordinary transient DB error, not a crash) "+
			"must never cause a double vendor call", provider.textToImageCalls)
	}
}

// concurrentImageProvider is a minimal, concurrency-safe ImageProvider test
// double that blocks inside TextToImage until release is closed, letting a
// test force two goroutines to overlap deterministically instead of relying
// on a wall-clock sleep.
type concurrentImageProvider struct {
	mu      sync.Mutex
	calls   int
	release chan struct{}
}

func (p *concurrentImageProvider) TextToImage(context.Context, TextToImageRequest) (ImageResult, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	<-p.release
	return ImageResult{
		Image: ImageBytes{Content: tinyPNG, MIME: "image/png"},
		Usage: ImageUsage{ImageCount: 1, Steps: 1},
	}, nil
}

func (p *concurrentImageProvider) ImageToImage(context.Context, ImageToImageRequest) (ImageResult, error) {
	return ImageResult{}, nil
}

func (p *concurrentImageProvider) Inpaint(context.Context, InpaintRequest) (ImageResult, error) {
	return ImageResult{}, nil
}

var _ ImageProvider = (*concurrentImageProvider)(nil)

// TestImageGenerateHandler_ConcurrentHandleForSameJob_OnlyOneVendorCall
// reproduces two overlapping Handle calls for the SAME job.ID -- the
// redelivery shape a lease-based distributed queue can produce (a worker's
// visibility timeout expiring while a slow vendor call is still in flight,
// or two workers racing a poll). The claim-before-vendor-call ordering
// image_job_store.go's own doc comment explains closes it: only one
// caller's claimPending INSERT can ever commit for one job id, so the
// loser must refuse to call the vendor at all -- there is no unlocked "no
// marker yet" read to race through.
func TestImageGenerateHandler_ConcurrentHandleForSameJob_OnlyOneVendorCall(t *testing.T) {
	provider := &concurrentImageProvider{release: make(chan struct{})}
	g, _, _ := imageGatewayTestFixture(t, &fakeImageProvider{}, WithImageProviderRegistry(newFakeImageRegistry(t, provider)))

	handler, ok := g.imageJobHandler()
	if !ok {
		t.Fatal("imageJobHandler() reported image generation not wired")
	}
	raw, err := json.Marshal(imageGenerateTaskPayload{
		Model: "image:default", Operation: string(ImageOperationTextToImage), Prompt: "a bright smile",
	})
	if err != nil {
		t.Fatalf("marshal task payload: %v", err)
	}
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	job := &jobs.Job{ID: "job-concurrent-1", Type: TaskTypeImageGenerate, TenantID: "tenant-acme", Payload: raw}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = handler.Handle(ctx, job, func(int, string) {})
	}()
	go func() {
		defer wg.Done()
		_, _ = handler.Handle(ctx, job, func(int, string) {})
	}()

	// Release the vendor call (if any goroutine reaches it) after a bounded
	// deadline, so this test can never hang even if the claim's own
	// serialization means only one goroutine -- or, on an unlucky
	// schedule, neither yet -- has entered TextToImage.
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(provider.release)
	}()

	wg.Wait()

	provider.mu.Lock()
	calls := provider.calls
	provider.mu.Unlock()
	if calls != 1 {
		t.Fatalf("provider called %d times for two concurrent Handle invocations of the SAME job, want exactly 1 -- "+
			"claimPending's primary-key INSERT must serialize concurrent claims so only one caller ever reaches the vendor", calls)
	}
}

// stagedPutObjectStore wraps a real pkgcore.ObjectStore and releases its
// PutObject calls one at a time, in arrival order, each only when the test
// closes that call's own release channel -- letting a test interleave two
// overlapping Handle calls deterministically instead of relying on a
// wall-clock sleep: the first caller can be held mid-storage-write while
// the second caller completes its own marker read and starts (and parks
// in) its storage write, which is exactly the interleaving a lease-based
// queue's concurrent redelivery produces.
type stagedPutObjectStore struct {
	inner pkgcore.ObjectStore

	mu       sync.Mutex
	entered  int
	enteredN chan int
	release  []chan struct{}
}

func newStagedPutObjectStore(inner pkgcore.ObjectStore) *stagedPutObjectStore {
	return &stagedPutObjectStore{
		inner:    inner,
		enteredN: make(chan int, 2),
		release:  []chan struct{}{make(chan struct{}), make(chan struct{})},
	}
}

func (s *stagedPutObjectStore) PutObject(ctx context.Context, key string, r io.Reader) error {
	s.mu.Lock()
	s.entered++
	n := s.entered
	s.mu.Unlock()
	s.enteredN <- n
	if n > len(s.release) {
		return fmt.Errorf("aigateway test: unexpected %dth PutObject call", n)
	}
	<-s.release[n-1]
	return s.inner.PutObject(ctx, key, r)
}

func (s *stagedPutObjectStore) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	return s.inner.GetObject(ctx, key)
}

func (s *stagedPutObjectStore) DeleteObject(ctx context.Context, key string) error {
	return s.inner.DeleteObject(ctx, key)
}

var _ pkgcore.ObjectStore = (*stagedPutObjectStore)(nil)

// TestImageGenerateHandler_ConcurrentRedelivery_AgreesOnOneOutputObjectID
// reproduces a concurrent redelivery of a job whose marker already says
// "generated": two overlapping Handle calls for the same job both read that
// state before either finishes (a lease-based queue redelivering a job
// whose first attempt completed its vendor call and marker but whose
// storage write is still in flight). Both attempts therefore write their
// own output object; markCompleted's guarded transition lets exactly one of
// the two through. The losing attempt must answer from the marker's own
// completed object id -- the winner's -- never its own freshly minted (and
// now orphaned) one, so any two Handle runs for one job agree on the id the
// caller reads out of Job.Result. A losing attempt that returned the orphan
// object id it just wrote would make the two results disagree, one naming
// an object the completed marker row never recorded.
func TestImageGenerateHandler_ConcurrentRedelivery_AgreesOnOneOutputObjectID(t *testing.T) {
	provider := &fakeImageProvider{}
	var recordedEvents []UsageEvent
	recorder := UsageRecorderFunc(func(_ context.Context, event UsageEvent) error {
		recordedEvents = append(recordedEvents, event)
		return nil
	})
	store := newStagedPutObjectStore(pkgcore.NewLocalObjectStore(t.TempDir()))
	objects := newTestStorageObjectServiceWithStore(t, store)
	g, _, _ := imageGatewayTestFixtureWithObjects(t, provider, objects, WithUsageRecorder(recorder))

	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	jobID := "job-concurrent-redelivery-1"
	img := ImageBytes{Content: tinyPNG, MIME: "image/png"}
	usage := ImageUsage{ImageCount: 1, Steps: 20, ResolutionTier: "512x512"}

	// Seed the marker at "generated" -- an earlier attempt already recorded
	// the vendor's answer, exactly the state a redelivery whose Handle
	// overlaps that attempt's storage write finds.
	if claimed, err := g.imageJobs.claimPending(ctx, jobID); err != nil || !claimed {
		t.Fatalf("claimPending: claimed=%v err=%v", claimed, err)
	}
	if err := g.imageJobs.markGenerated(ctx, jobID, fakeImageProviderName, img, usage); err != nil {
		t.Fatalf("markGenerated: %v", err)
	}

	handler, ok := g.imageJobHandler()
	if !ok {
		t.Fatal("imageJobHandler() reported image generation not wired")
	}
	raw, err := json.Marshal(imageGenerateTaskPayload{
		Model: "image:default", Operation: string(ImageOperationTextToImage), Prompt: "a bright smile",
	})
	if err != nil {
		t.Fatalf("marshal task payload: %v", err)
	}
	job := &jobs.Job{ID: jobs.JobID(jobID), Type: TaskTypeImageGenerate, TenantID: "tenant-acme", Payload: raw}

	results := make([]jobs.Result, 2)
	handleErrs := make([]error, 2)

	// Attempt 1 parks inside its own storage write (the first PutObject).
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		results[0], handleErrs[0] = handler.Handle(ctx, job, func(int, string) {})
	}()
	if n := <-store.enteredN; n != 1 {
		t.Fatalf("first PutObject reported as call %d, want 1", n)
	}

	// Attempt 2 starts while attempt 1 is still parked: it reads the marker
	// ("generated" -- attempt 1 has not completed yet), then parks inside
	// its own storage write (the second PutObject).
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		results[1], handleErrs[1] = handler.Handle(ctx, job, func(int, string) {})
	}()
	if n := <-store.enteredN; n != 2 {
		t.Fatalf("second PutObject reported as call %d, want 2", n)
	}

	// Let attempt 1 finish first: its markCompleted is the guarded
	// transition's winner, so the marker now durably names attempt 1's
	// object id.
	close(store.release[0])
	<-firstDone
	if handleErrs[0] != nil {
		t.Fatalf("first Handle: %v", handleErrs[0])
	}

	// Let the losing attempt finish: it must answer from the marker's own
	// completed id, never its own freshly written orphan's.
	close(store.release[1])
	<-secondDone
	if handleErrs[1] != nil {
		t.Fatalf("second (redelivered) Handle: %v", handleErrs[1])
	}

	var first, second ImageJobResult
	if decErr := json.Unmarshal(results[0].Data, &first); decErr != nil {
		t.Fatalf("decode first result: %v", decErr)
	}
	if decErr := json.Unmarshal(results[1].Data, &second); decErr != nil {
		t.Fatalf("decode second result: %v", decErr)
	}
	if first.OutputObjectID == "" || second.OutputObjectID == "" {
		t.Fatalf("results carry empty OutputObjectIDs: first=%+v second=%+v", first, second)
	}
	if first.OutputObjectID != second.OutputObjectID {
		t.Fatalf("two Handle runs for one job returned different OutputObjectIDs (%q vs %q) -- the losing attempt must return the marker's completed id, never its own orphan's", first.OutputObjectID, second.OutputObjectID)
	}

	marker, err := g.imageJobs.get(ctx, jobID)
	if err != nil {
		t.Fatalf("read back marker row: %v", err)
	}
	if marker == nil || marker.Status != imageJobStatusCompleted {
		t.Fatalf("marker row after both attempts = %+v, want status %q", marker, imageJobStatusCompleted)
	}
	if marker.OutputObjectID != first.OutputObjectID || marker.OutputObjectID != second.OutputObjectID {
		t.Fatalf("marker row names object %q, but the two attempts returned %q and %q -- every attempt must agree with the marker", marker.OutputObjectID, first.OutputObjectID, second.OutputObjectID)
	}

	// The vendor was never re-called (the answer was reused from the
	// marker), and usage was recorded exactly once, by the winning attempt
	// alone.
	if provider.textToImageCalls != 0 {
		t.Fatalf("provider called %d times for a job whose marker already held the vendor answer, want 0", provider.textToImageCalls)
	}
	var imageCountEvents int
	for _, e := range recordedEvents {
		if e.Feature == usageFeatureImageCount {
			imageCountEvents++
		}
	}
	if imageCountEvents != 1 {
		t.Fatalf("image_count usage recorded %d times across the two overlapping attempts, want 1", imageCountEvents)
	}
}
