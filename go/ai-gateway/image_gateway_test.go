package aigateway

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

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
	if _, err := pkgcore.NewKernel().Bootstrap(context.Background(), storageModule); err != nil {
		t.Fatalf("bootstrap storage module: %v", err)
	}
	return storageModule.ObjectService()
}

// imageGatewayTestFixture builds a Gateway wired to a fake ImageProvider
// (routed under "image:default"), a real CredentialService over a fresh
// test database, a stored platform credential, a recordingImageQueue and a
// real storage.ObjectService -- the common setup every image pipeline test
// below starts from, mirroring gatewayTestFixture's identical role for
// chat.
func imageGatewayTestFixture(t *testing.T, provider *fakeImageProvider, opts ...GatewayOption) (*Gateway, *recordingImageQueue, *storage.ObjectService) {
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
	objects := newTestStorageObjectService(t)

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

	_, err := g.GenerateImage(context.Background(), ImageRequest{
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

	_, err := g.GenerateImage(context.Background(), imageReq())
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

// --- Module wiring: image job handler registration --------------------------

func newTestRegistry() *pkgcore.Registry {
	return pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
}

func TestModule_Register_ImageJobHandler_RegisteredOnlyWhenWired(t *testing.T) {
	db := newTestDB(t)
	moduleWithImages := NewModule(db, WithImageGeneration(&recordingImageQueue{}, newTestStorageObjectService(t)))
	reg := newTestRegistry()
	if err := moduleWithImages.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Jobs.Handlers()[TaskTypeImageGenerate]; !ok {
		t.Fatalf("Register with WithImageGeneration did not claim %q on reg.Jobs", TaskTypeImageGenerate)
	}

	chatOnlyDB := newTestDB(t)
	chatOnlyModule := NewModule(chatOnlyDB)
	chatOnlyReg := newTestRegistry()
	if err := chatOnlyModule.Register(chatOnlyReg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := chatOnlyReg.Jobs.Handlers()[TaskTypeImageGenerate]; ok {
		t.Fatal("a chat-only Module (no WithImageGeneration) must not claim an image job handler")
	}
}
