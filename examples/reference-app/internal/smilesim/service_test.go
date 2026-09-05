package smilesim

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/storage"
)

// testCredentialCipherKey is a fixed, recognizable 32-byte AES-GCM key for
// registerCredentialSerializerOnce below, exactly like consult's own
// service_test.go's identical fixture.
var testCredentialCipherKey = []byte{
	0xe0, 0xe1, 0xe2, 0xe3, 0xe4, 0xe5, 0xe6, 0xe7,
	0xe8, 0xe9, 0xea, 0xeb, 0xec, 0xed, 0xee, 0xef,
	0xf0, 0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7,
	0xf8, 0xf9, 0xfa, 0xfb, 0xfc, 0xfd, 0xfe, 0xff,
}

var registerCredentialSerializerOnce sync.Once

// registerCredentialSerializer registers ai-gateway's
// CredentialAPIKeySerializerName exactly once for this test binary, before
// any migration touches the ai_gateway_credentials table -- the same
// ordering rule consult's own service_test.go documents.
func registerCredentialSerializer() {
	registerCredentialSerializerOnce.Do(func() {
		cipher, err := dbkit.NewCipher(testCredentialCipherKey)
		if err != nil {
			panic("smilesim test: NewCipher on the fixed 32-byte fixture key: " + err.Error())
		}
		dbkit.RegisterEncryptedSerializer(aigateway.CredentialAPIKeySerializerName, cipher)
	})
}

// fakeImageProviderName is the ImageProviderRegistry name fakeImageProvider
// below self-registers under, isolated from the real, process-global
// aigateway.ImageProviderRegistry through the per-test registry
// newTestService builds.
const fakeImageProviderName = "image.fake-smilesim-provider"

// testSystemPurpose is the SystemPurpose newTestService's platform-
// credential write grants itself, mirroring consult's own fixture.
const testSystemPurpose pkgcore.SystemPurpose = "smilesim.test.credential_write"

// fakeImageProvider is a minimal aigateway.ImageProvider test double
// recording the last ImageToImage request it received. This package's
// Service only ever calls ImageToImage (Simulate's own fixed Operation),
// so TextToImage and Inpaint are never exercised by this file's tests but
// still required to satisfy the interface.
type fakeImageProvider struct {
	lastReq aigateway.ImageToImageRequest
}

func (f *fakeImageProvider) TextToImage(context.Context, aigateway.TextToImageRequest) (aigateway.ImageResult, error) {
	return aigateway.ImageResult{}, nil
}

func (f *fakeImageProvider) ImageToImage(_ context.Context, req aigateway.ImageToImageRequest) (aigateway.ImageResult, error) {
	f.lastReq = req
	return aigateway.ImageResult{}, nil
}

func (f *fakeImageProvider) Inpaint(context.Context, aigateway.InpaintRequest) (aigateway.ImageResult, error) {
	return aigateway.ImageResult{}, nil
}

// compile-time check that *fakeImageProvider satisfies aigateway.ImageProvider.
var _ aigateway.ImageProvider = (*fakeImageProvider)(nil)

// recordingQueue is a jobs.Queue test double recording the last Task it was
// asked to Enqueue and answering every Enqueue with a fixed JobID -- this
// package's Service.Simulate never calls the queue itself (that is entirely
// Gateway.GenerateImage's own job, already proven by go/ai-gateway's own
// gateway_test.go/image_gateway_test.go), so this double exists only to let
// GenerateImage's own enqueue succeed; nothing here runs the resulting job.
type recordingQueue struct {
	lastTask jobs.Task
	jobID    jobs.JobID
}

func (q *recordingQueue) Enqueue(_ context.Context, task jobs.Task, _ ...jobs.EnqueueOption) (jobs.JobID, error) {
	q.lastTask = task
	return q.jobID, nil
}
func (q *recordingQueue) Get(context.Context, jobs.JobID) (*jobs.Job, error) { return nil, nil }
func (q *recordingQueue) Cancel(context.Context, jobs.JobID) error           { return nil }

// compile-time check that *recordingQueue satisfies jobs.Queue.
var _ jobs.Queue = (*recordingQueue)(nil)

// newTestService returns a Service backed by a fresh, per-test SQLite
// database carrying ai-gateway's real migrations, with provider registered
// as the sole ImageProviderRegistry entry LogicalModel routes to. queue
// records what Service.Simulate causes Gateway.GenerateImage to enqueue.
//
// The storage.ObjectService handed to WithImageGeneration is real but never
// actually reads or writes bytes in this file's tests: Service.Simulate
// only reaches Gateway.GenerateImage's enqueue path, never the job handler
// that would call it -- go/ai-gateway's own image_gateway_test.go is where
// that handler's storage I/O is proven.
func newTestService(t *testing.T, provider aigateway.ImageProvider, queue *recordingQueue) *Service {
	t.Helper()
	registerCredentialSerializer()

	db := dbtest.NewSQLite(t)

	migrations := dbkit.NewMigrationRegistry()
	if err := migrations.Register(aigateway.NewModule(db)); err != nil {
		t.Fatalf("register ai-gateway migrations: %v", err)
	}
	if err := migrations.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	registry := pkgcore.NewSeamRegistry[aigateway.ImageProvider]()
	if err := registry.Register(pkgcore.Registration[aigateway.ImageProvider]{
		Name:         fakeImageProviderName,
		Capabilities: pkgcore.Stateless,
		New:          func(pkgcore.Config) (aigateway.ImageProvider, error) { return provider, nil },
	}); err != nil {
		t.Fatalf("register fake image provider: %v", err)
	}

	credentials := aigateway.NewCredentialService(db)
	pkgcore.RegisterSystemPurpose(testSystemPurpose)
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor: "test-actor", Purpose: testSystemPurpose,
	})
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if err := credentials.SetPlatformCredential(sysCtx, fakeImageProviderName, "sk-test", ""); err != nil {
		t.Fatalf("SetPlatformCredential: %v", err)
	}

	// storageModule is never bootstrapped through the real kernel here --
	// this file's tests never reach a job handler that would need a real
	// ObjectStore -- so ObjectService() is real but inert (its host seams
	// are attached only by Module.Register, which nothing here calls).
	storageModule := storage.NewModule(db, storage.WithQueue(queue))

	gateway := aigateway.NewGateway(credentials,
		aigateway.WithModelRoute(LogicalModel, fakeImageProviderName, "vendor-model-x"),
		aigateway.WithImageProviderRegistry(registry),
		aigateway.WithImageGeneration(queue, storageModule.ObjectService()),
	)

	return NewService(gateway, pkgcore.NewMemoryEventBus())
}

func TestService_Simulate_EnqueuesImageToImageUnderTheLogicalModel(t *testing.T) {
	provider := &fakeImageProvider{}
	queue := &recordingQueue{jobID: "job-123"}
	svc := newTestService(t, provider, queue)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	jobID, err := svc.Simulate(ctx, "photo-object-1", "user-1")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	if jobID != queue.jobID {
		t.Fatalf("Simulate returned job id %q, want the queue's %q", jobID, queue.jobID)
	}

	if queue.lastTask.Type != aigateway.TaskTypeImageGenerate {
		t.Fatalf("enqueued task type = %q, want %q", queue.lastTask.Type, aigateway.TaskTypeImageGenerate)
	}
	if queue.lastTask.TenantID != "tenant-acme" {
		t.Fatalf("enqueued task tenant = %q, want %q", queue.lastTask.TenantID, "tenant-acme")
	}
}

func TestService_Simulate_MissingTenant_Refused(t *testing.T) {
	svc := newTestService(t, &fakeImageProvider{}, &recordingQueue{jobID: "job-unused"})

	if _, err := svc.Simulate(context.Background(), "photo-object-1", "user-1"); err == nil {
		t.Fatal("Simulate with no tenant context succeeded, want an error")
	}
}

func TestService_Simulate_EmptyPhotoObjectID_Refused(t *testing.T) {
	svc := newTestService(t, &fakeImageProvider{}, &recordingQueue{jobID: "job-unused"})

	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := svc.Simulate(ctx, "", "user-1"); err == nil {
		t.Fatal("Simulate with an empty photo object id succeeded, want an error -- ImageOperationImageToImage requires InputObjectID")
	}
}

// subscribeSimulationCompleted subscribes a recorder to
// EventSimulationCompleted on bus and returns a func reading back every
// SimulationCompletedPayload received so far, in order.
func subscribeSimulationCompleted(bus pkgcore.EventBus) func() []SimulationCompletedPayload {
	var mu sync.Mutex
	var got []SimulationCompletedPayload
	bus.Subscribe(EventSimulationCompleted, func(_ context.Context, evt pkgcore.Event) error {
		payload, ok := evt.Payload.(SimulationCompletedPayload)
		if !ok {
			return nil
		}
		mu.Lock()
		got = append(got, payload)
		mu.Unlock()
		return nil
	})
	return func() []SimulationCompletedPayload {
		mu.Lock()
		defer mu.Unlock()
		return append([]SimulationCompletedPayload(nil), got...)
	}
}

func TestService_NotifyOnCompletion_PublishesOnceForASucceededJobWithARecipient(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	svc := NewService(nil, bus)
	events := subscribeSimulationCompleted(bus)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	const jobID = jobs.JobID("job-1")
	svc.recipients[jobID] = "user-7"

	result, err := json.Marshal(aigateway.ImageJobResult{OutputObjectID: "object-out-1"})
	if err != nil {
		t.Fatalf("marshal ImageJobResult: %v", err)
	}
	job := &jobs.Job{
		ID:       jobID,
		TenantID: "tenant-acme",
		Status:   jobs.StatusSucceeded,
		Result:   &jobs.Result{Data: result},
	}

	if err := svc.NotifyOnCompletion(ctx, job); err != nil {
		t.Fatalf("NotifyOnCompletion: %v", err)
	}
	// A second call for the SAME job (a repeated poll) must not publish
	// again.
	if err := svc.NotifyOnCompletion(ctx, job); err != nil {
		t.Fatalf("NotifyOnCompletion (second call): %v", err)
	}

	got := events()
	if len(got) != 1 {
		t.Fatalf("published %d events, want exactly 1 (repeated polls must not double-publish): %+v", len(got), got)
	}
	if got[0].RecipientUserID != "user-7" || got[0].TenantID != "tenant-acme" || got[0].ImageJobID != string(jobID) {
		t.Errorf("payload = %+v, want recipient=user-7 tenant=tenant-acme job=%s", got[0], jobID)
	}
	if !got[0].Succeeded {
		t.Error("Succeeded = false, want true for a StatusSucceeded job")
	}
	if got[0].OutputObjectID != "object-out-1" {
		t.Errorf("OutputObjectID = %q, want %q", got[0].OutputObjectID, "object-out-1")
	}
}

func TestService_NotifyOnCompletion_DeadLetterReportsFailureWithNoOutputObject(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	svc := NewService(nil, bus)
	events := subscribeSimulationCompleted(bus)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	const jobID = jobs.JobID("job-2")
	svc.recipients[jobID] = "user-7"

	job := &jobs.Job{ID: jobID, TenantID: "tenant-acme", Status: jobs.StatusDeadLetter}
	if err := svc.NotifyOnCompletion(ctx, job); err != nil {
		t.Fatalf("NotifyOnCompletion: %v", err)
	}

	got := events()
	if len(got) != 1 {
		t.Fatalf("published %d events, want 1: %+v", len(got), got)
	}
	if got[0].Succeeded {
		t.Error("Succeeded = true, want false for a StatusDeadLetter job")
	}
	if got[0].OutputObjectID != "" {
		t.Errorf("OutputObjectID = %q, want empty for a failed job", got[0].OutputObjectID)
	}
}

func TestService_NotifyOnCompletion_NoRecipientOnFile_NeverPublishes(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	svc := NewService(nil, bus)
	events := subscribeSimulationCompleted(bus)

	job := &jobs.Job{ID: "job-no-recipient", TenantID: "tenant-acme", Status: jobs.StatusSucceeded}
	if err := svc.NotifyOnCompletion(context.Background(), job); err != nil {
		t.Fatalf("NotifyOnCompletion: %v", err)
	}
	if got := events(); len(got) != 0 {
		t.Errorf("published %d events for a job with no recipient on file, want 0: %+v", len(got), got)
	}
}

func TestService_NotifyOnCompletion_NonTerminalStatus_NeverPublishes(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	svc := NewService(nil, bus)
	events := subscribeSimulationCompleted(bus)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	const jobID = jobs.JobID("job-3")
	svc.recipients[jobID] = "user-7"

	for _, status := range []jobs.Status{jobs.StatusPending, jobs.StatusRunning, jobs.StatusRetrying} {
		job := &jobs.Job{ID: jobID, TenantID: "tenant-acme", Status: status}
		if err := svc.NotifyOnCompletion(ctx, job); err != nil {
			t.Fatalf("NotifyOnCompletion(%s): %v", status, err)
		}
	}
	if got := events(); len(got) != 0 {
		t.Errorf("published %d events for a non-terminal job, want 0: %+v", len(got), got)
	}
}

func TestService_NotifyOnCompletion_NilBus_IsANoOp(t *testing.T) {
	svc := NewService(nil, nil)
	job := &jobs.Job{ID: "job-x", TenantID: "tenant-acme", Status: jobs.StatusSucceeded}
	// Never given a recipient, so this would be a no-op regardless, but the
	// point is that a nil bus must not panic even when it IS reached.
	svc.recipients[job.ID] = "user-7"
	if err := svc.NotifyOnCompletion(context.Background(), job); err != nil {
		t.Fatalf("NotifyOnCompletion with a nil bus error = %v, want nil", err)
	}
}
