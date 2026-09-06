package smilesim

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	aigateway "github.com/vislake/speed/go/ai-gateway"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
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
//
// jobs, when populated via setJob, is what ReconcileOutstandingCredits'
// own Get calls observe -- standing in for a real jobs.Queue's persisted
// job status the way a real StandaloneQueue would report it, without this
// test double actually running any job. A jobID never given to setJob
// answers (nil, nil) from Get, exactly as the zero-value map lookup
// already did before setJob existed, so every pre-existing test that never
// calls setJob is unaffected.
type recordingQueue struct {
	lastTask     jobs.Task
	jobID        jobs.JobID
	enqueueCalls int
	jobs         map[jobs.JobID]*jobs.Job
}

func (q *recordingQueue) Enqueue(_ context.Context, task jobs.Task, _ ...jobs.EnqueueOption) (jobs.JobID, error) {
	q.enqueueCalls++
	q.lastTask = task
	return q.jobID, nil
}

func (q *recordingQueue) Get(_ context.Context, id jobs.JobID) (*jobs.Job, error) {
	return q.jobs[id], nil
}
func (q *recordingQueue) Cancel(context.Context, jobs.JobID) error { return nil }

// setJob records job as what a later Get(job.ID) answers -- see this
// type's own doc comment.
func (q *recordingQueue) setJob(job *jobs.Job) {
	if q.jobs == nil {
		q.jobs = make(map[jobs.JobID]*jobs.Job)
	}
	q.jobs[job.ID] = job
}

// compile-time check that *recordingQueue satisfies jobs.Queue.
var _ jobs.Queue = (*recordingQueue)(nil)

// newTestCreditService returns a billing.CreditService backed by a fresh,
// per-test SQLite database carrying go/billing's real migrations --
// mirroring newTestService's own "apply the real, versioned migration
// files from zero" rule, applied here to go/billing's tables instead of
// go/ai-gateway's.
func newTestCreditService(t *testing.T) *billing.CreditService {
	t.Helper()

	db := dbtest.NewSQLite(t)
	billingModule := billing.NewModule(db, nil)

	migrations := dbkit.NewMigrationRegistry()
	if err := migrations.Register(billingModule); err != nil {
		t.Fatalf("register billing migrations: %v", err)
	}
	if err := migrations.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply billing migrations: %v", err)
	}
	return billingModule.Credits()
}

// newTestService returns a Service backed by a fresh, per-test SQLite
// database carrying ai-gateway's real migrations, with provider registered
// as the sole ImageProviderRegistry entry LogicalModel routes to. queue
// records what Service.Simulate causes Gateway.GenerateImage to enqueue,
// and credits (nil is legal -- see Service's own doc comment on its
// credits field) is the CreditService Simulate reserves against and
// settleCredit settles. This is the ordinary case, one fresh database per
// Service; see newTestServiceWithDB for the "same database, two Service
// instances" shape a process-restart proof needs.
func newTestService(t *testing.T, provider aigateway.ImageProvider, queue *recordingQueue, credits *billing.CreditService) *Service {
	t.Helper()
	return newTestServiceWithDB(t, dbtest.NewSQLite(t), provider, queue, credits)
}

// newTestServiceWithDB is newTestService's own implementation, parametrized
// on db instead of always creating a fresh one -- letting a test build TWO
// Service instances that share the same underlying database (and, by
// passing the same queue too, the same "queue survived" state) to prove a
// property that only holds across two separate Service values: that
// creditReservation's persisted row survives whatever in-memory state the
// first Service instance held, exactly as it would across a real process
// restart (see TestService_ReconcileOutstandingCredits and
// TestService_CreditReservation_SurvivesRestart below).
//
// The storage.ObjectService handed to WithImageGeneration is real but never
// actually reads or writes bytes in this file's tests: Service.Simulate
// only reaches Gateway.GenerateImage's enqueue path, never the job handler
// that would call it -- go/ai-gateway's own image_gateway_test.go is where
// that handler's storage I/O is proven.
func newTestServiceWithDB(t *testing.T, db *gorm.DB, provider aigateway.ImageProvider, queue *recordingQueue, credits *billing.CreditService) *Service {
	t.Helper()
	registerCredentialSerializer()

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

	store := NewReservationStore(db)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	return NewService(gateway, credits, pkgcore.NewMemoryEventBus(), queue, store)
}

func TestService_Simulate_EnqueuesImageToImageUnderTheLogicalModel(t *testing.T) {
	provider := &fakeImageProvider{}
	queue := &recordingQueue{jobID: "job-123"}
	svc := newTestService(t, provider, queue, nil)

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
	svc := newTestService(t, &fakeImageProvider{}, &recordingQueue{jobID: "job-unused"}, nil)

	if _, err := svc.Simulate(context.Background(), "photo-object-1", "user-1"); err == nil {
		t.Fatal("Simulate with no tenant context succeeded, want an error")
	}
}

func TestService_Simulate_EmptyPhotoObjectID_Refused(t *testing.T) {
	svc := newTestService(t, &fakeImageProvider{}, &recordingQueue{jobID: "job-unused"}, nil)

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
	svc := NewService(nil, nil, bus, nil, nil)
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
	svc := NewService(nil, nil, bus, nil, nil)
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
	svc := NewService(nil, nil, bus, nil, nil)
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
	svc := NewService(nil, nil, bus, nil, nil)
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

// TestService_Simulate_InsufficientCredits_RefusesBeforeAnyEnqueue is this
// round's mandated proof (root CLAUDE.md's "Reference App" section, and the
// task that opened this round): a tenant whose balance cannot cover
// CreditsPerSimulation is refused with billing.ErrInsufficientCredits
// BEFORE Gateway.GenerateImage ever reaches the queue -- queue.enqueueCalls
// stays at zero, proving go/ai-gateway (and, transitively, any real
// vendor) was never invoked.
func TestService_Simulate_InsufficientCredits_RefusesBeforeAnyEnqueue(t *testing.T) {
	credits := newTestCreditService(t)
	queue := &recordingQueue{jobID: "job-should-never-run"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	// tenant-acme's balance was never granted anything -- CreditService
	// materializes a fresh, all-zero balance on first read, so
	// CreditsPerSimulation (10) already exceeds it.
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err == nil {
		t.Fatalf("Simulate with an insufficient balance succeeded (job %q), want billing.ErrInsufficientCredits", jobID)
	}
	if jobID != "" {
		t.Errorf("Simulate returned job id %q on refusal, want empty", jobID)
	}
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != "billing.insufficient_credits" {
		t.Fatalf("Simulate error = %v, want a coded billing.insufficient_credits error", err)
	}
	if queue.enqueueCalls != 0 {
		t.Errorf("queue.enqueueCalls = %d, want 0 -- Gateway.GenerateImage (and go/ai-gateway) must never be reached on an insufficient balance", queue.enqueueCalls)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 0 || bal.Reserved != 0 {
		t.Errorf("balance after a refused reservation = %+v, want zero on both -- PreDeduct must leave no trace", bal)
	}
}

// TestService_Simulate_SufficientCredits_ReservesBeforeEnqueue proves the
// success half of the same ordering: a tenant with enough balance is
// debited into Reserved (never Available -> nothing, and never a second,
// unrelated bucket) BEFORE the enqueue happens, and the enqueue then
// genuinely runs.
func TestService_Simulate_SufficientCredits_ReservesBeforeEnqueue(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	queue := &recordingQueue{jobID: "job-99"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	if jobID != queue.jobID {
		t.Fatalf("Simulate returned job id %q, want the queue's %q", jobID, queue.jobID)
	}
	if queue.enqueueCalls != 1 {
		t.Fatalf("queue.enqueueCalls = %d, want exactly 1", queue.enqueueCalls)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100-CreditsPerSimulation {
		t.Errorf("Available = %d, want %d", bal.Available, 100-CreditsPerSimulation)
	}
	if bal.Reserved != CreditsPerSimulation {
		t.Errorf("Reserved = %d, want %d", bal.Reserved, CreditsPerSimulation)
	}
}

// newSimulateResultJob builds the *jobs.Job NotifyOnCompletion expects for
// a StatusSucceeded job carrying outputObjectID as its ImageJobResult.
func newSimulateResultJob(t *testing.T, jobID jobs.JobID, tenantID pkgcore.TenantID, status jobs.Status, outputObjectID string) *jobs.Job {
	t.Helper()
	job := &jobs.Job{ID: jobID, TenantID: tenantID, Status: status}
	if status == jobs.StatusSucceeded {
		data, err := json.Marshal(aigateway.ImageJobResult{OutputObjectID: outputObjectID})
		if err != nil {
			t.Fatalf("marshal ImageJobResult: %v", err)
		}
		job.Result = &jobs.Result{Data: data}
	}
	return job
}

// TestService_NotifyOnCompletion_Succeeded_ConfirmsReservation proves the
// Confirm half of settleCredit end to end: a succeeded job's reservation
// becomes a permanent spend (Reserved returns to zero, Available stays
// debited), and a second, repeated poll of the same already-terminal job
// settles again without error or double-applying -- the "provably safe
// under a retried job settlement" property the task that opened this round
// requires.
func TestService_NotifyOnCompletion_Succeeded_ConfirmsReservation(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	queue := &recordingQueue{jobID: "job-succeed-1"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)
	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}

	job := newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-1")
	if err = svc.NotifyOnCompletion(ctx, job); err != nil {
		t.Fatalf("NotifyOnCompletion: %v", err)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100-CreditsPerSimulation {
		t.Errorf("Available after confirm = %d, want %d", bal.Available, 100-CreditsPerSimulation)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after confirm = %d, want 0", bal.Reserved)
	}

	// A repeated poll of the same, already-settled job must settle again
	// without error and without moving the balance a second time.
	if err = svc.NotifyOnCompletion(ctx, job); err != nil {
		t.Fatalf("NotifyOnCompletion (repeated poll): %v", err)
	}
	balAgain, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance (after repeated poll): %v", err)
	}
	if *balAgain != *bal {
		t.Errorf("balance changed on a repeated settle of an already-confirmed job: first %+v, second %+v", *bal, *balAgain)
	}
}

// TestService_NotifyOnCompletion_DeadLetter_RefundsReservation proves the
// Refund half: a dead-lettered job's reservation is released back to
// Available in full, and a repeated poll settles again without error or
// double-refunding.
func TestService_NotifyOnCompletion_DeadLetter_RefundsReservation(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	queue := &recordingQueue{jobID: "job-fail-1"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)
	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}

	job := newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusDeadLetter, "")
	if err = svc.NotifyOnCompletion(ctx, job); err != nil {
		t.Fatalf("NotifyOnCompletion: %v", err)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100 {
		t.Errorf("Available after refund = %d, want 100 (back to the pre-reservation balance)", bal.Available)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after refund = %d, want 0", bal.Reserved)
	}

	if err = svc.NotifyOnCompletion(ctx, job); err != nil {
		t.Fatalf("NotifyOnCompletion (repeated poll): %v", err)
	}
	balAgain, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance (after repeated poll): %v", err)
	}
	if *balAgain != *bal {
		t.Errorf("balance changed on a repeated settle of an already-refunded job: first %+v, second %+v", *bal, *balAgain)
	}
}

func TestService_NotifyOnCompletion_NilBus_IsANoOp(t *testing.T) {
	svc := NewService(nil, nil, nil, nil, nil)
	job := &jobs.Job{ID: "job-x", TenantID: "tenant-acme", Status: jobs.StatusSucceeded}
	// Never given a recipient, so this would be a no-op regardless, but the
	// point is that a nil bus must not panic even when it IS reached.
	svc.recipients[job.ID] = "user-7"
	if err := svc.NotifyOnCompletion(context.Background(), job); err != nil {
		t.Fatalf("NotifyOnCompletion with a nil bus error = %v, want nil", err)
	}
}

// TestService_ReconcileOutstandingCredits_SettlesAJobNeverPolled is this
// round's own regression proof for the bug this round fixes: before it,
// settleCredit was reachable ONLY through NotifyOnCompletion, itself
// reachable only by a client polling the job-status route -- a reservation
// whose job finished while nobody was watching (a closed tab, a dropped
// connection, a caller that simply never checked back) stayed Reserved
// forever, with no other path to settle it. This test drives Simulate to
// open a real reservation, records the resulting job as StatusSucceeded
// directly on queue (standing in for "the job finished"), and calls ONLY
// ReconcileOutstandingCredits -- NotifyOnCompletion is never called at
// all -- proving the reservation still settles.
func TestService_ReconcileOutstandingCredits_SettlesAJobNeverPolled(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	queue := &recordingQueue{jobID: "job-recon-1"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance (after Simulate): %v", err)
	}
	if bal.Reserved != CreditsPerSimulation {
		t.Fatalf("Reserved after Simulate = %d, want %d", bal.Reserved, CreditsPerSimulation)
	}

	// The job finished -- recorded directly on the queue double, never
	// observed through NotifyOnCompletion (no poll ever happens in this
	// test).
	queue.setJob(&jobs.Job{ID: jobID, TenantID: "tenant-acme", Status: jobs.StatusSucceeded})

	settled, err := svc.ReconcileOutstandingCredits(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOutstandingCredits: %v", err)
	}
	if settled != 1 {
		t.Fatalf("settled = %d, want 1", settled)
	}

	bal, err = credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance (after reconcile): %v", err)
	}
	if bal.Available != 100-CreditsPerSimulation {
		t.Errorf("Available after reconcile = %d, want %d", bal.Available, 100-CreditsPerSimulation)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after reconcile = %d, want 0 -- the never-polled reservation must still be confirmed, not left stuck Reserved forever", bal.Reserved)
	}

	// The reservation row is gone now, so sweeping again finds nothing
	// left to settle.
	settledAgain, err := svc.ReconcileOutstandingCredits(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOutstandingCredits (second sweep): %v", err)
	}
	if settledAgain != 0 {
		t.Errorf("settled on second sweep = %d, want 0", settledAgain)
	}
}

// TestService_ReconcileOutstandingCredits_NonTerminalJob_LeavesReservationInPlace
// proves the sweep does not touch a job still in flight: settling a
// reservation before its job actually finishes would be a correctness bug
// of its own (an early Confirm on a job that later dead-letters, or an
// early Refund on one that later succeeds).
func TestService_ReconcileOutstandingCredits_NonTerminalJob_LeavesReservationInPlace(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	queue := &recordingQueue{jobID: "job-recon-2"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	queue.setJob(&jobs.Job{ID: jobID, TenantID: "tenant-acme", Status: jobs.StatusRunning})

	settled, err := svc.ReconcileOutstandingCredits(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOutstandingCredits: %v", err)
	}
	if settled != 0 {
		t.Fatalf("settled = %d, want 0 for a still-running job", settled)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Reserved != CreditsPerSimulation {
		t.Errorf("Reserved after sweeping a non-terminal job = %d, want unchanged %d", bal.Reserved, CreditsPerSimulation)
	}
}

// TestService_ReconcileOutstandingCredits_NilWiring_IsANoOp mirrors every
// other optional-seam nil-safety test in this file: a Service missing
// credits, store or queue must not panic, and must settle nothing.
func TestService_ReconcileOutstandingCredits_NilWiring_IsANoOp(t *testing.T) {
	svc := NewService(nil, nil, nil, nil, nil)
	settled, err := svc.ReconcileOutstandingCredits(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOutstandingCredits: %v", err)
	}
	if settled != 0 {
		t.Errorf("settled = %d, want 0", settled)
	}
}

// TestService_CreditReservation_SurvivesRestart is this round's other own
// regression proof: before it, the job-id-to-CreditTransaction-key mapping
// lived in a plain Go map (creditKeys), which a process restart wipes --
// so even a client that kept polling faithfully after a restart would find
// settleCredit's lookup come up empty and silently do nothing, leaking the
// reservation forever with no error and no distinguishing log line. This
// test builds a Service (serviceA), reserves credits through it, then
// builds a brand new, second Service instance (serviceB) sharing nothing
// in memory with serviceA -- no recipients, no notified, no in-process
// state of any kind -- but the SAME underlying database, exactly modeling
// a process restart in which only durable state (the database, and the
// jobs queue's own persisted rows, modeled here by reusing the same queue
// double) survives. serviceB alone is used to reconcile the reservation
// serviceA opened, proving the mapping itself -- not just the process that
// happened to create it -- is what makes settlement possible.
func TestService_CreditReservation_SurvivesRestart(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	db := dbtest.NewSQLite(t)
	queue := &recordingQueue{jobID: "job-restart-1"}
	serviceA := newTestServiceWithDB(t, db, &fakeImageProvider{}, queue, credits)

	jobID, err := serviceA.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}

	// The "restart": a second Service, its own fresh in-memory state, over
	// the same database and the same (queue-double-modeled) persisted
	// queue -- gateway is nil because nothing here calls Simulate on
	// serviceB, only settlement.
	store := NewReservationStore(db)
	if schemaErr := store.EnsureSchema(context.Background()); schemaErr != nil {
		t.Fatalf("EnsureSchema: %v", schemaErr)
	}
	serviceB := NewService(nil, credits, pkgcore.NewMemoryEventBus(), queue, store)

	queue.setJob(&jobs.Job{ID: jobID, TenantID: "tenant-acme", Status: jobs.StatusSucceeded})

	settled, err := serviceB.ReconcileOutstandingCredits(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOutstandingCredits: %v", err)
	}
	if settled != 1 {
		t.Fatalf("settled = %d, want 1 -- the reservation serviceA opened must still be visible to serviceB after the simulated restart", settled)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100-CreditsPerSimulation {
		t.Errorf("Available after restart-survived settlement = %d, want %d", bal.Available, 100-CreditsPerSimulation)
	}
	if bal.Reserved != 0 {
		t.Errorf("Reserved after restart-survived settlement = %d, want 0", bal.Reserved)
	}
}

// TestService_StartReconciler_AutomaticallySettlesWithoutAnyPoll proves
// StartReconciler's background ticker loop actually runs
// ReconcileOutstandingCredits on its own, with no test code ever calling
// NotifyOnCompletion or ReconcileOutstandingCredits directly -- the full,
// end-to-end shape of "settlement is reachable independent of any client
// polling" this round's fix provides.
func TestService_StartReconciler_AutomaticallySettlesWithoutAnyPoll(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	queue := &recordingQueue{jobID: "job-ticker-1"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	jobID, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	queue.setJob(&jobs.Job{ID: jobID, TenantID: "tenant-acme", Status: jobs.StatusSucceeded})

	stop := svc.StartReconciler(context.Background(), 10*time.Millisecond)
	defer stop()

	deadline := time.Now().Add(2 * time.Second)
	for {
		bal, balErr := credits.Balance(ctx)
		if balErr != nil {
			t.Fatalf("Balance: %v", balErr)
		}
		if bal.Reserved == 0 {
			if bal.Available != 100-CreditsPerSimulation {
				t.Fatalf("Available once settled = %d, want %d", bal.Available, 100-CreditsPerSimulation)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("reservation still Reserved after waiting for the background reconciler to tick -- balance = %+v", bal)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestService_StartReconciler_NilWiring_ReturnsAHarmlessStop mirrors every
// other optional-seam nil-safety test in this file: calling
// StartReconciler on a Service with nothing wired must not panic, and its
// returned stop func must be safe to call.
func TestService_StartReconciler_NilWiring_ReturnsAHarmlessStop(t *testing.T) {
	svc := NewService(nil, nil, nil, nil, nil)
	stop := svc.StartReconciler(context.Background(), time.Millisecond)
	stop()
}
