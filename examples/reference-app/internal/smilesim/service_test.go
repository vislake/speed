package smilesim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// enqueueErr, when non-nil, is what Enqueue answers instead of jobID --
// standing in for a real queue's enqueue failure at a deterministic point
// of Simulate's own flow. onEnqueue, when non-nil, runs at the top of
// every Enqueue call, before enqueueErr is consulted: the canceled-request
// regression tests use it to cancel the request context at exactly the
// moment the real gateway's enqueue happens -- after PreDeduct has already
// committed and before Simulate's own post-enqueue writes -- which is the
// one interleaving a real client disconnect can produce that this package
// could not otherwise drive deterministically from outside a single
// synchronous Simulate call.
//
// jobs, when populated via setJob, is what ReconcileOutstandingCredits'
// own Get calls observe -- standing in for a real jobs.Queue's persisted
// job status the way a real StandaloneQueue would report it, without this
// test double actually running any job. A jobID never given to setJob
// answers (nil, nil) from Get, exactly as the zero-value map lookup
// already did before setJob existed, so every pre-existing test that never
// calls setJob is unaffected. A jobID recorded via ageOut answers
// jobs.ErrJobNotFound instead -- standing in for a real queue whose task
// retention has deleted a completed job (the distributed queue's
// DefaultCompletedRetention window; see ListSimulationsByPhoto's own
// "Recorded limitation" section for what that means to a listing).
type recordingQueue struct {
	lastTask     jobs.Task
	jobID        jobs.JobID
	enqueueCalls int
	enqueueErr   error
	onEnqueue    func()
	jobs         map[jobs.JobID]*jobs.Job
	notFound     map[jobs.JobID]bool
}

func (q *recordingQueue) Enqueue(_ context.Context, task jobs.Task, _ ...jobs.EnqueueOption) (jobs.JobID, error) {
	q.enqueueCalls++
	q.lastTask = task
	if q.onEnqueue != nil {
		q.onEnqueue()
	}
	if q.enqueueErr != nil {
		return "", q.enqueueErr
	}
	return q.jobID, nil
}

func (q *recordingQueue) Get(_ context.Context, id jobs.JobID) (*jobs.Job, error) {
	if q.notFound[id] {
		return nil, jobs.ErrJobNotFound
	}
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

// ageOut records jobID as no longer on file -- a later Get answers
// jobs.ErrJobNotFound, the shape a real queue reports once the completed
// task's retention window has passed.
func (q *recordingQueue) ageOut(jobID jobs.JobID) {
	if q.notFound == nil {
		q.notFound = make(map[jobs.JobID]bool)
	}
	q.notFound[jobID] = true
}

// compile-time check that *recordingQueue satisfies jobs.Queue.
var _ jobs.Queue = (*recordingQueue)(nil)

// newTestCreditServiceWithDB is newTestCreditService's own implementation,
// parametrized on db instead of always creating a fresh one -- letting a
// test that must fail billing calls selectively (the refund-failure
// regression test closes db's underlying sql.DB at a deterministic point
// of Simulate's flow) build the service over a handle it controls.
func newTestCreditServiceWithDB(t *testing.T, db *gorm.DB) *billing.CreditService {
	t.Helper()

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

// newTestCreditService returns a billing.CreditService backed by a fresh,
// per-test SQLite database carrying go/billing's real migrations --
// mirroring newTestService's own "apply the real, versioned migration
// files from zero" rule, applied here to go/billing's tables instead of
// go/ai-gateway's.
func newTestCreditService(t *testing.T) *billing.CreditService {
	t.Helper()
	return newTestCreditServiceWithDB(t, dbtest.NewSQLite(t))
}

// newTestService returns a Service backed by a fresh, per-test SQLite
// database carrying ai-gateway's real migrations, with provider registered
// as the sole ImageProviderRegistry entry LogicalModel routes to. queue
// records what Service.Simulate causes Gateway.GenerateImage to enqueue,
// and credits (nil is legal -- see Service's own doc comment on its
// credits field) is the CreditService Simulate reserves against and
// settleCredit settles. entitlements, when given (at most one), is wired
// BOTH into the built gateway's WithEntitlements gate and into the
// Service's own pre-flight seam -- the same one-seam-for-both shape
// cmd/server's own wiring uses -- so the entitlement-refusal regressions
// can drive the refusal through the same composition production runs
// under. This is the ordinary case, one fresh database per Service; see
// newTestServiceWithDB for the "same database, two Service instances"
// shape a process-restart proof needs.
func newTestService(t *testing.T, provider aigateway.ImageProvider, queue *recordingQueue, credits *billing.CreditService, entitlements ...aigateway.Entitlements) *Service {
	t.Helper()
	return newTestServiceWithDB(t, dbtest.NewSQLite(t), provider, queue, credits, entitlements...)
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
func newTestServiceWithDB(t *testing.T, db *gorm.DB, provider aigateway.ImageProvider, queue *recordingQueue, credits *billing.CreditService, entitlements ...aigateway.Entitlements) *Service {
	t.Helper()
	var entitlementSeam aigateway.Entitlements
	if len(entitlements) > 0 {
		entitlementSeam = entitlements[0]
	}
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

	gatewayOptions := []aigateway.GatewayOption{
		aigateway.WithModelRoute(LogicalModel, fakeImageProviderName, "vendor-model-x"),
		aigateway.WithImageProviderRegistry(registry),
		aigateway.WithImageGeneration(queue, storageModule.ObjectService()),
	}
	if entitlementSeam != nil {
		gatewayOptions = append(gatewayOptions, aigateway.WithEntitlements(entitlementSeam))
	}
	gateway := aigateway.NewGateway(credentials, gatewayOptions...)

	store := NewReservationStore(db)
	if err := store.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}

	simulations := NewSimulationStore(db)
	if err := simulations.EnsureSchema(context.Background()); err != nil {
		t.Fatalf("EnsureSchema (simulation index): %v", err)
	}

	return NewService(gateway, credits, pkgcore.NewMemoryEventBus(), queue, store, simulations, entitlementSeam)
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
	svc := NewService(nil, nil, bus, nil, nil, nil, nil)
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
	svc := NewService(nil, nil, bus, nil, nil, nil, nil)
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
	svc := NewService(nil, nil, bus, nil, nil, nil, nil)
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
	svc := NewService(nil, nil, bus, nil, nil, nil, nil)
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
	svc := NewService(nil, nil, nil, nil, nil, nil, nil)
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
	svc := NewService(nil, nil, nil, nil, nil, nil, nil)
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
	simulations := NewSimulationStore(db)
	if schemaErr := simulations.EnsureSchema(context.Background()); schemaErr != nil {
		t.Fatalf("EnsureSchema (simulation index): %v", schemaErr)
	}
	serviceB := NewService(nil, credits, pkgcore.NewMemoryEventBus(), queue, store, simulations, nil)

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
	svc := NewService(nil, nil, nil, nil, nil, nil, nil)
	stop := svc.StartReconciler(context.Background(), time.Millisecond)
	stop()
}

// TestService_Simulate_InvalidOptions_RefusedBeforeReservationOrEnqueue
// pins the refusal ordering this round's parameterization promises: an
// option set with any out-of-vocabulary or out-of-range value is refused
// with its coded smilesim.* error BEFORE any credit is reserved and before
// Gateway.GenerateImage -- and therefore go/ai-gateway, and any real
// vendor -- is ever reached, and it leaves no record behind for the
// per-photo index.
func TestService_Simulate_InvalidOptions_RefusedBeforeReservationOrEnqueue(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	queue := &recordingQueue{jobID: "job-should-never-run"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	invalidAttempts := []struct {
		name    string
		options []SimulateOption
		wantErr string
	}{
		{name: "unknown smile style", options: []SimulateOption{WithSmileStyle("dazzling")}, wantErr: "smilesim.unsupported_smile_style"},
		{name: "unknown tooth shade", options: []SimulateOption{WithToothShade("glittering")}, wantErr: "smilesim.unsupported_tooth_shade"},
		{name: "strength above the upper bound", options: []SimulateOption{WithStrength(1.5)}, wantErr: "smilesim.strength_out_of_range"},
		{name: "strength at the excluded lower bound", options: []SimulateOption{WithStrength(0)}, wantErr: "smilesim.strength_out_of_range"},
		{name: "valid style with an invalid shade", options: []SimulateOption{WithSmileStyle(SmileStyleBright), WithToothShade("grey")}, wantErr: "smilesim.unsupported_tooth_shade"},
	}
	for _, attempt := range invalidAttempts {
		t.Run(attempt.name, func(t *testing.T) {
			jobID, err := svc.Simulate(ctx, "photo-1", "", attempt.options...)
			if err == nil {
				t.Fatalf("Simulate with %v succeeded (job %q), want a coded error", attempt.options, jobID)
			}
			if jobID != "" {
				t.Errorf("Simulate returned job id %q on refusal, want empty", jobID)
			}
			appErr, ok := apperr.As(err)
			if !ok || appErr.Code != attempt.wantErr {
				t.Fatalf("Simulate error = %v, want coded %s", err, attempt.wantErr)
			}
		})
	}

	if queue.enqueueCalls != 0 {
		t.Errorf("queue.enqueueCalls = %d, want 0 -- Gateway.GenerateImage must never be reached for an invalid option set", queue.enqueueCalls)
	}
	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100 || bal.Reserved != 0 {
		t.Errorf("balance after refused simulations = %+v, want Available 100 Reserved 0 -- validation must run before any PreDeduct", bal)
	}
	sims, err := svc.ListSimulationsByPhoto(ctx, "photo-1")
	if err != nil {
		t.Fatalf("ListSimulationsByPhoto: %v", err)
	}
	if len(sims) != 0 {
		t.Errorf("ListSimulationsByPhoto after refused simulations returned %d entries, want 0 -- a refused simulation must leave no per-photo record", len(sims))
	}

	// The same Service still works for a legal option set afterwards --
	// the refusals above must not have wedged anything.
	jobID, err := svc.Simulate(ctx, "photo-1", "", WithSmileStyle(SmileStyleBright))
	if err != nil {
		t.Fatalf("Simulate with a legal option set: %v", err)
	}
	if jobID != queue.jobID {
		t.Fatalf("Simulate returned job id %q, want the queue's %q", jobID, queue.jobID)
	}
	if queue.enqueueCalls != 1 {
		t.Errorf("queue.enqueueCalls = %d, want exactly 1 after the legal simulation", queue.enqueueCalls)
	}
}

// TestService_Simulate_SamePhotoSameOptions_IsANewGeneration pins this
// round's regenerate decision (see the package doc comment's
// "Parameterized simulation options" section): calling Simulate again with
// the SAME photo and the SAME options enqueues a genuinely NEW generation --
// a fresh job id, a fresh credit reservation, a second enqueue -- and
// records both in the per-photo index; an option differing in any dimension
// is likewise always a new generation.
func TestService_Simulate_SamePhotoSameOptions_IsANewGeneration(t *testing.T) {
	credits := newTestCreditService(t)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := credits.Grant(ctx, billing.GrantInput{Amount: 100, Reason: "test:seed"}); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	queue := &recordingQueue{}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits)

	const photo = "photo-1"
	nextJob := 0
	simulate := func(opts ...SimulateOption) jobs.JobID {
		t.Helper()
		nextJob++
		queue.jobID = jobs.JobID(fmt.Sprintf("job-same-%d", nextJob))
		jobID, err := svc.Simulate(ctx, photo, "", opts...)
		if err != nil {
			t.Fatalf("Simulate: %v", err)
		}
		// The job exists in the queue's world so the per-photo listing can
		// resolve it (the recording queue double answers what setJob gave
		// it).
		queue.setJob(&jobs.Job{ID: jobID, TenantID: "tenant-acme", Status: jobs.StatusPending})
		return jobID
	}

	first := simulate(WithSmileStyle(SmileStyleBright), WithToothShade(ToothShadeWhite), WithStrength(0.5))
	second := simulate(WithSmileStyle(SmileStyleBright), WithToothShade(ToothShadeWhite), WithStrength(0.5))
	if first == second {
		t.Fatalf("two same-photo same-options Simulate calls returned the same job id %q -- a regenerate must be a NEW generation", first)
	}
	if queue.enqueueCalls != 2 {
		t.Fatalf("queue.enqueueCalls = %d, want 2 -- each same-option call must really enqueue", queue.enqueueCalls)
	}

	bal, err := credits.Balance(ctx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Reserved != 2*CreditsPerSimulation {
		t.Errorf("Reserved = %d, want %d -- each same-option generation must carry its own credit reservation", bal.Reserved, 2*CreditsPerSimulation)
	}

	third := simulate(WithSmileStyle(SmileStyleSubtle), WithToothShade(ToothShadeWhite), WithStrength(0.5))
	if third == first || third == second {
		t.Fatalf("a different-options call returned an earlier job id -- different options must always be a new generation")
	}

	sims, err := svc.ListSimulationsByPhoto(ctx, photo)
	if err != nil {
		t.Fatalf("ListSimulationsByPhoto: %v", err)
	}
	if len(sims) != 3 {
		t.Fatalf("ListSimulationsByPhoto returned %d entries, want 3", len(sims))
	}
	byJob := map[jobs.JobID]SimulationOutcome{}
	for _, sim := range sims {
		byJob[sim.JobID] = sim
	}
	for _, jobID := range []jobs.JobID{first, second, third} {
		if _, ok := byJob[jobID]; !ok {
			t.Fatalf("per-photo index missing job %q -- every generation, same-option or not, must be recorded", jobID)
		}
	}
	firstOutcome := byJob[first]
	wantOptions := SimulationOptions{SmileStyle: SmileStyleBright, ToothShade: ToothShadeWhite, Strength: 0.5}
	if firstOutcome.Options != wantOptions {
		t.Errorf("first generation's recorded options = %+v, want %+v", firstOutcome.Options, wantOptions)
	}
	if byJob[third].Options.SmileStyle != SmileStyleSubtle {
		t.Errorf("third generation's recorded smile style = %q, want %q", byJob[third].Options.SmileStyle, SmileStyleSubtle)
	}
}

// TestService_Simulate_DefaultOptions_AreRecordedExplicitly pins that a
// Simulate call naming no options records the DOCUMENTED defaults in the
// per-photo index -- never an empty option set -- so a later read of what
// produced a default generation answers the real option set.
func TestService_Simulate_DefaultOptions_AreRecordedExplicitly(t *testing.T) {
	queue := &recordingQueue{jobID: "job-defaults-1"}
	svc := newTestService(t, &fakeImageProvider{}, queue, nil)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	jobID, err := svc.Simulate(ctx, "photo-defaults", "")
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	queue.setJob(&jobs.Job{ID: jobID, TenantID: "tenant-acme", Status: jobs.StatusPending})

	sims, err := svc.ListSimulationsByPhoto(ctx, "photo-defaults")
	if err != nil {
		t.Fatalf("ListSimulationsByPhoto: %v", err)
	}
	if len(sims) != 1 {
		t.Fatalf("ListSimulationsByPhoto returned %d entries, want 1", len(sims))
	}
	if sims[0].Options != DefaultSimulationOptions() {
		t.Errorf("recorded options = %+v, want the documented defaults %+v -- a defaulted generation must be recorded with its effective options", sims[0].Options, DefaultSimulationOptions())
	}
}

// TestService_OptionsAndListing_SurviveRestart is this round's restart
// proof for the per-photo index: a generation recorded through one Service
// instance -- photo, effective options and job id -- is still enumerated,
// with its options and its live job status/output, by a brand new Service
// instance sharing nothing in memory (the restart shape
// TestService_CreditReservation_SurvivesRestart already establishes), over
// the same database and the same (queue-double-modeled) persisted queue.
func TestService_OptionsAndListing_SurviveRestart(t *testing.T) {
	db := dbtest.NewSQLite(t)
	queue := &recordingQueue{jobID: "job-restart-sim-1"}
	serviceA := newTestServiceWithDB(t, db, &fakeImageProvider{}, queue, nil)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	wantOptions := SimulationOptions{SmileStyle: SmileStyleBright, ToothShade: ToothShadeWhite, Strength: 0.25}
	jobID, err := serviceA.Simulate(ctx, "photo-1", "",
		WithSmileStyle(SmileStyleBright), WithToothShade(ToothShadeWhite), WithStrength(0.25))
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}

	// The "restart": a fresh Service over the same database with its own
	// fresh store instances -- gateway nil because nothing here calls
	// Simulate on serviceB, only reads. The job finished while the
	// process was down; the queue double records that terminal state the
	// way the real persisted queue would.
	simulations := NewSimulationStore(db)
	if schemaErr := simulations.EnsureSchema(context.Background()); schemaErr != nil {
		t.Fatalf("EnsureSchema (simulation index): %v", schemaErr)
	}
	queue.setJob(newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-9"))
	serviceB := NewService(nil, nil, pkgcore.NewMemoryEventBus(), queue, nil, simulations, nil)

	sims, err := serviceB.ListSimulationsByPhoto(ctx, "photo-1")
	if err != nil {
		t.Fatalf("ListSimulationsByPhoto after restart: %v", err)
	}
	if len(sims) != 1 {
		t.Fatalf("ListSimulationsByPhoto after restart returned %d entries, want 1 -- the recorded index row must survive the restart", len(sims))
	}
	sim := sims[0]
	if sim.JobID != jobID {
		t.Errorf("enumerated job id = %q, want %q", sim.JobID, jobID)
	}
	if sim.PhotoObjectID != "photo-1" {
		t.Errorf("enumerated photo = %q, want photo-1", sim.PhotoObjectID)
	}
	if sim.Options != wantOptions {
		t.Errorf("enumerated options = %+v, want %+v -- the options must survive the restart with their exact values", sim.Options, wantOptions)
	}
	if sim.Status != jobs.StatusSucceeded {
		t.Errorf("enumerated status = %q, want %q -- the live job status must be read through the queue", sim.Status, jobs.StatusSucceeded)
	}
	if sim.OutputObjectID != "object-out-9" {
		t.Errorf("enumerated output object id = %q, want object-out-9", sim.OutputObjectID)
	}
	if sim.CreatedAt.IsZero() {
		t.Error("enumerated created_at is zero")
	}

	gotOptions, has, err := serviceB.OptionsForJob(ctx, jobID)
	if err != nil {
		t.Fatalf("OptionsForJob after restart: %v", err)
	}
	if !has {
		t.Fatal("OptionsForJob after restart found no record for the simulated job")
	}
	if gotOptions != wantOptions {
		t.Errorf("OptionsForJob options = %+v, want %+v", gotOptions, wantOptions)
	}

	// A photo that was never simulated, and a job that was never recorded,
	// both answer "nothing" -- never an error, never another tenant's row.
	others, err := serviceB.ListSimulationsByPhoto(ctx, "photo-never-simulated")
	if err != nil {
		t.Fatalf("ListSimulationsByPhoto (unknown photo): %v", err)
	}
	if len(others) != 0 {
		t.Errorf("ListSimulationsByPhoto (unknown photo) returned %d entries, want 0", len(others))
	}
	if _, has, err := serviceB.OptionsForJob(ctx, "job-never-recorded"); err != nil || has {
		t.Errorf("OptionsForJob (unknown job) = has %v err %v, want false nil", has, err)
	}
}

// TestService_PerPhotoIndex_TenantScoped pins the index's isolation: two
// tenants may simulate the SAME photo object id (a string collision that is
// meaningless across tenants' storage), and each tenant's enumeration and
// OptionsForJob see exactly its own rows -- the tenant filter lives in the
// query, never in a post-read filter. A listing context carrying no tenant
// at all is refused rather than listing across tenants.
func TestService_PerPhotoIndex_TenantScoped(t *testing.T) {
	queue := &recordingQueue{}
	svc := newTestService(t, &fakeImageProvider{}, queue, nil)

	acmeCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	bcorpCtx := pkgcore.WithTenant(context.Background(), "tenant-bcorp")

	queue.jobID = "job-acme-1"
	acmeJob, err := svc.Simulate(acmeCtx, "photo-shared", "")
	if err != nil {
		t.Fatalf("Simulate (acme): %v", err)
	}
	queue.setJob(&jobs.Job{ID: acmeJob, TenantID: "tenant-acme", Status: jobs.StatusPending})

	queue.jobID = "job-bcorp-1"
	bcorpJob, err := svc.Simulate(bcorpCtx, "photo-shared", "")
	if err != nil {
		t.Fatalf("Simulate (bcorp): %v", err)
	}
	queue.setJob(&jobs.Job{ID: bcorpJob, TenantID: "tenant-bcorp", Status: jobs.StatusPending})

	acmeSims, err := svc.ListSimulationsByPhoto(acmeCtx, "photo-shared")
	if err != nil {
		t.Fatalf("ListSimulationsByPhoto (acme): %v", err)
	}
	if len(acmeSims) != 1 || acmeSims[0].JobID != acmeJob {
		t.Fatalf("acme's enumeration of the shared photo id = %+v, want exactly its own job %q", acmeSims, acmeJob)
	}
	bcorpSims, err := svc.ListSimulationsByPhoto(bcorpCtx, "photo-shared")
	if err != nil {
		t.Fatalf("ListSimulationsByPhoto (bcorp): %v", err)
	}
	if len(bcorpSims) != 1 || bcorpSims[0].JobID != bcorpJob {
		t.Fatalf("bcorp's enumeration of the shared photo id = %+v, want exactly its own job %q", bcorpSims, bcorpJob)
	}

	// OptionsForJob is tenant-scoped the same way: acme's record is not
	// visible to bcorp, even though both asked over the same photo id.
	if _, has, err := svc.OptionsForJob(bcorpCtx, acmeJob); err != nil || has {
		t.Errorf("OptionsForJob (bcorp asking for acme's job) = has %v err %v, want false nil", has, err)
	}
	if _, has, err := svc.OptionsForJob(acmeCtx, acmeJob); err != nil || !has {
		t.Errorf("OptionsForJob (acme asking for its own job) = has %v err %v, want true nil", has, err)
	}

	if _, err := svc.ListSimulationsByPhoto(context.Background(), "photo-shared"); !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Errorf("ListSimulationsByPhoto with no tenant context error = %v, want pkgcore.ErrNoTenant", err)
	}
}

// TestService_PerPhotoIndex_NilWiring_IsANoOp mirrors the optional-seam
// nil-safety tests this file applies to every other method: a Service
// built with no simulation store and no queue must not panic, and its
// per-photo reads answer "nothing".
func TestService_PerPhotoIndex_NilWiring_IsANoOp(t *testing.T) {
	svc := NewService(nil, nil, nil, nil, nil, nil, nil)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	sims, err := svc.ListSimulationsByPhoto(ctx, "photo-1")
	if err != nil {
		t.Fatalf("ListSimulationsByPhoto with nil wiring error = %v, want nil", err)
	}
	if len(sims) != 0 {
		t.Errorf("ListSimulationsByPhoto with nil wiring returned %d entries, want 0", len(sims))
	}
	if _, has, err := svc.OptionsForJob(ctx, "job-1"); err != nil || has {
		t.Errorf("OptionsForJob with nil wiring = has %v err %v, want false nil", has, err)
	}
}

// TestService_ListSimulationsByPhoto_AgedOutJob_DoesNotFailTheAlbum is the
// P3-refapp-13 regression: before the fix, ONE row whose job the queue no
// longer has on file -- a completed task whose retention window passed
// (the distributed queue answers jobs.ErrJobNotFound once go/jobs/queue/
// asynq's DefaultCompletedRetention has elapsed), or a queue that answers
// (nil, nil) -- failed the ENTIRE enumeration, so the photo's album
// listing answered not-found over one simulation the index itself still
// records. The listing must tolerate an unreadable row by omitting it
// (the method's own "Recorded limitation" section documents why nothing
// truthful remains to list for an aged-out job), while the photo's other,
// still-readable simulations keep listing -- and a genuine queue failure
// (any error other than not-found) must still fail the enumeration, since
// an outage is transient where retention is permanent.
func TestService_ListSimulationsByPhoto_AgedOutJob_DoesNotFailTheAlbum(t *testing.T) {
	queue := &recordingQueue{}
	svc := newTestService(t, &fakeImageProvider{}, queue, nil)
	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	// Three generations of the same photo: one still on file, one whose
	// completed task aged out of the queue's retention window, and one the
	// queue answers (nil, nil) for -- the two spellings of "no longer on
	// file" this method must tolerate.
	queue.jobID = "job-fresh"
	freshJob, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate (fresh): %v", err)
	}
	queue.jobID = "job-aged-out"
	agedJob, err := svc.Simulate(ctx, "photo-1", "")
	if err != nil {
		t.Fatalf("Simulate (aged): %v", err)
	}
	queue.jobID = "job-lost"
	if _, lostErr := svc.Simulate(ctx, "photo-1", ""); lostErr != nil {
		t.Fatalf("Simulate (lost): %v", lostErr)
	}

	queue.setJob(newSimulateResultJob(t, freshJob, "tenant-acme", jobs.StatusSucceeded, "object-out-fresh"))
	queue.ageOut(agedJob) // completed 25 hours ago: the retention window has passed

	sims, err := svc.ListSimulationsByPhoto(ctx, "photo-1")
	if err != nil {
		t.Fatalf("ListSimulationsByPhoto with an aged-out simulation = %v -- one aged-out job must not fail its photo's whole album", err)
	}
	if len(sims) != 1 {
		t.Fatalf("enumeration returned %d entries, want 1 (the fresh job; the aged-out and the lost jobs are omitted per the recorded limitation): %+v", len(sims), sims)
	}
	if sims[0].JobID != freshJob {
		t.Errorf("enumerated job id = %q, want the still-readable %q", sims[0].JobID, freshJob)
	}
	if sims[0].Status != jobs.StatusSucceeded || sims[0].OutputObjectID != "object-out-fresh" {
		t.Errorf("enumerated fresh outcome = status %q output %q, want succeeded %q -- the readable row must keep its live outcome", sims[0].Status, sims[0].OutputObjectID, "object-out-fresh")
	}

	// The durable rows themselves are untouched: only the live enumeration
	// omits what it cannot read.
	rows, err := svc.simulations.listByPhoto(ctx, "photo-1")
	if err != nil {
		t.Fatalf("listByPhoto: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("index rows for the photo = %d, want 3 -- the index is append-only and must not lose the aged-out row", len(rows))
	}
}

// TestService_NotifyOnCompletion_DeliveredEntriesAreForgotten is the
// P3-refapp-14 regression: before the fix, recipients and notified kept
// one entry per job forever -- every job Simulate ran with a recipient
// and every delivery it latched accumulated across the process's life,
// unbounded by anything. The two maps must be bounded by outstanding
// notifications instead: once a job's terminal outcome is fully processed
// -- an accepted publish on the success path, and a terminal job observed
// by a bus-less Service that can never deliver -- both entries are
// deleted, and the once-only guarantee holds across the deletion (a later
// poll must not publish again for a job whose delivery is done).
func TestService_NotifyOnCompletion_DeliveredEntriesAreForgotten(t *testing.T) {
	bus := pkgcore.NewMemoryEventBus()
	svc := NewService(nil, nil, bus, nil, nil, nil, nil)
	events := subscribeSimulationCompleted(bus)

	ctx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	const completed = 3
	for i := 0; i < completed; i++ {
		jobID := jobs.JobID(fmt.Sprintf("job-delivered-%d", i))
		svc.recipients[jobID] = "user-7"
		job := newSimulateResultJob(t, jobID, "tenant-acme", jobs.StatusSucceeded, "object-out-1")
		if err := svc.NotifyOnCompletion(ctx, job); err != nil {
			t.Fatalf("NotifyOnCompletion (job %s): %v", jobID, err)
		}
	}
	if got := events(); len(got) != completed {
		t.Fatalf("published %d events, want %d", len(got), completed)
	}

	// After N completed and delivered simulations the maps hold at most N
	// entries -- here, none at all: every delivered job's bookkeeping was
	// cleaned up on the success path.
	if len(svc.recipients) != 0 || len(svc.notified) != 0 {
		t.Errorf("maps after %d delivered simulations: recipients %d notified %d entries, want 0 each -- delivered bookkeeping must be cleaned up, not accumulated", completed, len(svc.recipients), len(svc.notified))
	}

	// The once-only guarantee survives the cleanup: a repeated poll of an
	// already-delivered job must not publish again.
	repeatPoll := newSimulateResultJob(t, jobs.JobID("job-delivered-0"), "tenant-acme", jobs.StatusSucceeded, "object-out-1")
	if err := svc.NotifyOnCompletion(ctx, repeatPoll); err != nil {
		t.Fatalf("NotifyOnCompletion (repeat poll): %v", err)
	}
	if got := events(); len(got) != completed {
		t.Errorf("published %d events after a repeat poll of a delivered job, want still %d -- cleanup must not re-arm the delivery", len(got), completed)
	}

	// A bus-less Service forgets a terminal job's recipient too: no
	// deliverer is wired, so no delivery can ever exist for it, and the
	// entry has no future (NotifyOnCompletion's nil-bus path).
	busless := NewService(nil, nil, nil, nil, nil, nil, nil)
	busless.recipients[jobs.JobID("job-no-bus")] = "user-7"
	if err := busless.NotifyOnCompletion(ctx, newSimulateResultJob(t, jobs.JobID("job-no-bus"), "tenant-acme", jobs.StatusSucceeded, "object-out-1")); err != nil {
		t.Fatalf("NotifyOnCompletion (bus-less): %v", err)
	}
	if len(busless.recipients) != 0 {
		t.Errorf("bus-less Service kept %d recipient entries after a terminal job, want 0 -- a delivery that can never exist must not be remembered", len(busless.recipients))
	}
}

// TestService_Simulate_EntitlementRefusal_WritesNoLedgerRow is the
// P3-refapp-17 regression: before the fix, Simulate reserved credits
// before calling Gateway.GenerateImage, whose own entitlement gate then
// refused the request -- leaving the reservation opened and immediately
// refunded (the PreDeduct/Refund pair: one ledger row under the
// reservation's own reason, plus the two balance moves) for a request no
// job ever ran for. The model-access gate must be pre-flighted BEFORE the
// reservation opens -- through the very seam the gateway gates on, so the
// two checks cannot disagree about the same state -- and a deterministically
// refused request must be refused with the gateway's own coded answer and
// no ledger row at all.
func TestService_Simulate_EntitlementRefusal_WritesNoLedgerRow(t *testing.T) {
	billingDB := dbtest.NewSQLite(t)
	credits := newTestCreditServiceWithDB(t, billingDB)
	grantTestCredits(t, credits)
	liveCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")

	// A seam that denies every request, the way a tenant with no Active
	// subscription answers -- wired into both the gateway's gate and the
	// Service's pre-flight by newTestService, exactly as cmd/server's own
	// one-closure-for-both wiring does.
	denied := aigateway.EntitlementsFunc(func(context.Context, string, int64) (aigateway.Decision, error) {
		return aigateway.Decision{Allowed: false, Reason: "no_subscription"}, nil
	})
	queue := &recordingQueue{jobID: "job-should-never-run"}
	svc := newTestService(t, &fakeImageProvider{}, queue, credits, denied)

	jobID, err := svc.Simulate(liveCtx, "photo-1", "")
	if err == nil {
		t.Fatalf("Simulate with a refusing entitlement gate succeeded (job %q), want aigateway.entitlement_denied", jobID)
	}
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != aigateway.ErrEntitlementDenied.Code {
		t.Fatalf("Simulate error = %v, want the gateway's own coded aigateway.entitlement_denied answer", err)
	}
	if queue.enqueueCalls != 0 {
		t.Errorf("queue.enqueueCalls = %d, want 0 -- the refusal must precede any enqueue", queue.enqueueCalls)
	}

	// The ledger carries nothing for the refused request -- before the
	// fix, the reservation opened and its compensating refund left one
	// row (status refunded) under creditReasonSimulate behind.
	var reservations []billing.CreditTransaction
	queryErr := billingDB.WithContext(liveCtx).Where("reason = ?", creditReasonSimulate).Find(&reservations).Error
	if queryErr != nil {
		t.Fatalf("read ledger: %v", queryErr)
	}
	if len(reservations) != 0 {
		t.Errorf("ledger holds %d smilesim reservation rows after a refused request, want 0 -- a request the gateway refuses must never write the PreDeduct/Refund pair", len(reservations))
	}

	bal, err := credits.Balance(liveCtx)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if bal.Available != 100 || bal.Reserved != 0 {
		t.Errorf("balance after the refused request = %+v, want Available 100 Reserved 0 -- no reservation may open for a refused request", *bal)
	}
}
