package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// seedAPIKey inserts one API key row (with its hash-index row, exactly like
// Service.Create does) directly through the repository, bypassing Create's
// expiry validation -- tests need keys whose ExpiresAt is in the past,
// which no Service call can produce, and keys under tenants other than the
// test's own. It returns the stored hash so an assertion can check the
// paired hash-index row.
func seedAPIKey(t *testing.T, svc *Service, tenant pkgcore.TenantID, id string, expiresAt time.Time) string {
	t.Helper()
	_, prefix, hash, err := newAPIKeyToken()
	if err != nil {
		t.Fatalf("newAPIKeyToken: %v", err)
	}
	row := &APIKey{
		ID:        id,
		Prefix:    prefix,
		Hash:      hash,
		Scopes:    scopesJSON(nil),
		CreatedBy: "user-1",
		ExpiresAt: expiresAt,
	}
	if err := svc.repo.createWithHashIndex(ctxFor(tenant), row); err != nil {
		t.Fatalf("createWithHashIndex: %v", err)
	}
	return hash
}

// keyExists reports whether a key row for id still exists in tenant's
// scope, failing the test on any non-not-found error.
func keyExists(t *testing.T, svc *Service, tenant pkgcore.TenantID, id string) bool {
	t.Helper()
	_, err := svc.repo.FindByID(ctxFor(tenant), id)
	if err == nil {
		return true
	}
	found, ok := apperr.As(err)
	if ok && found.Code == dbkit.ErrRecordNotFound.Code {
		return false
	}
	t.Fatalf("FindByID(%s): %v", id, err)
	return false
}

// TestService_SweepExpiredAPIKeys_RemovesOnlyTheTenantsExpiredKeys pins the
// sweep's whole row lifecycle: expired keys are removed (a revoked one
// whose expiry passed along with a live-but-expired one -- both are dead
// weight past ExpiresAt), each removed key's hash-index row goes with it,
// and an unexpired key of the same tenant and an expired key of another
// tenant both survive -- the sweep is tenant-scoped like every other query
// in this module.
func TestService_SweepExpiredAPIKeys_RemovesOnlyTheTenantsExpiredKeys(t *testing.T) {
	svc := attachedService(t, withClock(func() time.Time { return fixedNow }))

	hashes := map[string]string{}
	hashes["expired"] = seedAPIKey(t, svc, testTenant, "expired", fixedNow.Add(-time.Hour))
	hashes["revoked-expired"] = seedAPIKey(t, svc, testTenant, "revoked-expired", fixedNow.Add(-time.Hour))
	hashes["live"] = seedAPIKey(t, svc, testTenant, "live", fixedNow.Add(24*time.Hour))

	const otherTenant pkgcore.TenantID = "tenant-2"
	hashes["other-expired"] = seedAPIKey(t, svc, otherTenant, "other-expired", fixedNow.Add(-time.Hour))

	// Revoke the second expired key through the real Service path, so the
	// sweep faces a genuinely revoked row, not a hand-written one.
	if err := svc.Revoke(ctxFor(testTenant), "revoked-expired"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	if err := svc.SweepExpiredAPIKeys(ctxFor(testTenant)); err != nil {
		t.Fatalf("SweepExpiredAPIKeys: %v", err)
	}

	for _, id := range []string{"expired", "revoked-expired"} {
		if keyExists(t, svc, testTenant, id) {
			t.Errorf("expired key %q still exists after the sweep", id)
		}
		// The pair went together: a swept key's hash-index row must no
		// longer resolve a tenant (see deleteWithHashIndex's doc comment).
		_, err := svc.repo.tenantForHash(context.Background(), hashes[id])
		if !apperrIs(err, ErrAuthenticationFailed) {
			t.Errorf("hash-index row of swept key %q still resolves a tenant (err = %v)", id, err)
		}
	}
	if !keyExists(t, svc, testTenant, "live") {
		t.Error("the unexpired key was removed by the sweep")
	}
	if _, err := svc.repo.tenantForHash(context.Background(), hashes["live"]); err != nil {
		t.Errorf("the unexpired key's hash-index row no longer resolves a tenant: %v", err)
	}
	if !keyExists(t, svc, otherTenant, "other-expired") {
		t.Error("another tenant's expired key was removed by the sweep")
	}
	if _, err := svc.repo.tenantForHash(context.Background(), hashes["other-expired"]); err != nil {
		t.Errorf("another tenant's expired key's hash-index row no longer resolves a tenant: %v", err)
	}
}

func TestService_SweepExpiredAPIKeys_NoTenantInContext_Refused(t *testing.T) {
	svc := attachedService(t)
	if err := svc.SweepExpiredAPIKeys(context.Background()); !apperrIs(err, ErrInternal) {
		t.Errorf("SweepExpiredAPIKeys without a tenant = %v, want ErrInternal", err)
	}
}

func TestService_EnqueueAPIKeyExpirySweep_ShapesTheTask(t *testing.T) {
	fq := &fakeQueue{}
	svc := attachedService(t, WithWebhookQueue(fq), withClock(func() time.Time { return fixedNow }))

	if err := svc.EnqueueAPIKeyExpirySweep(ctxFor(testTenant)); err != nil {
		t.Fatalf("EnqueueAPIKeyExpirySweep: %v", err)
	}
	if len(fq.tasks) != 1 {
		t.Fatalf("len(fq.tasks) = %d, want 1", len(fq.tasks))
	}
	task := fq.tasks[0]
	if task.Type != jobTypeAPIKeyExpirySweep {
		t.Errorf("task.Type = %q, want %q", task.Type, jobTypeAPIKeyExpirySweep)
	}
	if string(task.TenantID) != string(testTenant) {
		t.Errorf("task.TenantID = %q, want %q", task.TenantID, testTenant)
	}
	if len(task.Payload) != 0 {
		t.Errorf("task.Payload = %q, want empty (the sweep reads rows and the clock when it runs)", task.Payload)
	}
	wantKey := apiKeyExpirySweepIdempotencyKey(testTenant, apiKeyExpirySweepWindowStart(fixedNow))
	if task.IdempotencyKey != wantKey {
		t.Errorf("task.IdempotencyKey = %q, want %q (the windowed key)", task.IdempotencyKey, wantKey)
	}
}

// TestService_EnqueueAPIKeyExpirySweep_WindowKey_CollapsesSameWindow_OpensNext
// pins the window semantics that make the sweep periodic: two enqueues
// within one apiKeyExpirySweepWindowSize window derive the same idempotency
// key (the queue dedupes them onto one job -- a scheduler with two replicas
// never double-sweeps), and an enqueue whose clock has moved into a later
// window derives a new key and runs again -- which is also what keeps one
// dead-lettered sweep job from poisoning its tenant forever.
func TestService_EnqueueAPIKeyExpirySweep_WindowKey_CollapsesSameWindow_OpensNext(t *testing.T) {
	fq := &fakeQueue{}
	svc := attachedService(t, WithWebhookQueue(fq), withClock(func() time.Time { return fixedNow }))

	if err := svc.EnqueueAPIKeyExpirySweep(ctxFor(testTenant)); err != nil {
		t.Fatalf("EnqueueAPIKeyExpirySweep: %v", err)
	}
	// Same window, later instant: the same key.
	svc.now = func() time.Time { return fixedNow.Add(30 * time.Minute) }
	if err := svc.EnqueueAPIKeyExpirySweep(ctxFor(testTenant)); err != nil {
		t.Fatalf("EnqueueAPIKeyExpirySweep: %v", err)
	}
	if len(fq.tasks) != 2 {
		t.Fatalf("len(fq.tasks) = %d, want 2", len(fq.tasks))
	}
	if fq.tasks[0].IdempotencyKey != fq.tasks[1].IdempotencyKey {
		t.Errorf("same-window keys differ: %q vs %q", fq.tasks[0].IdempotencyKey, fq.tasks[1].IdempotencyKey)
	}

	// A later window: a fresh key.
	svc.now = func() time.Time { return fixedNow.Add(2 * time.Hour) }
	if err := svc.EnqueueAPIKeyExpirySweep(ctxFor(testTenant)); err != nil {
		t.Fatalf("EnqueueAPIKeyExpirySweep: %v", err)
	}
	if fq.tasks[2].IdempotencyKey == fq.tasks[0].IdempotencyKey {
		t.Errorf("later-window key equals the earlier window's: %q", fq.tasks[2].IdempotencyKey)
	}
}

func TestService_EnqueueAPIKeyExpirySweep_NoTenantInContext_Fails(t *testing.T) {
	fq := &fakeQueue{}
	svc := attachedService(t, WithWebhookQueue(fq))
	if err := svc.EnqueueAPIKeyExpirySweep(context.Background()); !apperrIs(err, ErrInternal) {
		t.Errorf("EnqueueAPIKeyExpirySweep without a tenant = %v, want ErrInternal", err)
	}
	if len(fq.tasks) != 0 {
		t.Errorf("len(fq.tasks) = %d, want 0 (nothing enqueued without a tenant)", len(fq.tasks))
	}
}

func TestService_EnqueueAPIKeyExpirySweep_NoQueueWired_IsAPlainError(t *testing.T) {
	svc := attachedService(t)
	err := svc.EnqueueAPIKeyExpirySweep(ctxFor(testTenant))
	if err == nil {
		t.Fatal("EnqueueAPIKeyExpirySweep with no queue wired = nil error, want the plain no-queue error")
	}
	if !strings.Contains(err.Error(), "no queue wired") {
		t.Errorf("error = %q, want it to name the missing queue wiring", err)
	}
}

// TestAPIKeyExpirySweepHandler_Handle_RunsTheSweep drives the sweep through
// the module wrapper a worker would dispatch to -- the type Register
// registers under jobTypeAPIKeyExpirySweep -- over an attached Module,
// proving the Register-time handler really reaches the Attach-time
// Service's sweep on the job's own tenant context.
func TestAPIKeyExpirySweepHandler_Handle_RunsTheSweep(t *testing.T) {
	m, svc := newWebhookTestService(t, withClock(func() time.Time { return fixedNow }))
	seedAPIKey(t, svc, testTenant, "handler-expired", fixedNow.Add(-time.Hour))
	seedAPIKey(t, svc, testTenant, "handler-live", fixedNow.Add(time.Hour))

	h := apiKeyExpirySweepHandler{module: m}
	if _, err := h.Handle(ctxFor(testTenant), &jobs.Job{ID: "sweep-1", Type: jobTypeAPIKeyExpirySweep, TenantID: testTenant}, nil); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if keyExists(t, svc, testTenant, "handler-expired") {
		t.Error("the expired key survived the handler-run sweep")
	}
	if !keyExists(t, svc, testTenant, "handler-live") {
		t.Error("the unexpired key was removed by the handler-run sweep")
	}
}

func TestAPIKeyExpirySweepHandler_Handle_UnexpectedPayload_FailsTheJob(t *testing.T) {
	m, _ := newWebhookTestService(t)
	h := apiKeyExpirySweepHandler{module: m}
	_, err := h.Handle(ctxFor(testTenant), &jobs.Job{
		ID: "sweep-2", Type: jobTypeAPIKeyExpirySweep, TenantID: testTenant, Payload: []byte(`{"x":1}`),
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "unexpected payload") {
		t.Errorf("Handle with a payload = %v, want the payload-refusal error", err)
	}
}

func TestAPIKeyExpirySweepHandler_Handle_BeforeAttach_FailsClosed(t *testing.T) {
	m := NewModule(newTestDB(t))
	h := apiKeyExpirySweepHandler{module: m}
	_, err := h.Handle(ctxFor(testTenant), &jobs.Job{ID: "sweep-3", Type: jobTypeAPIKeyExpirySweep, TenantID: testTenant}, nil)
	if err == nil || !strings.Contains(err.Error(), "before Module.Attach") {
		t.Errorf("Handle before Attach = %v, want the nil-Service refusal", err)
	}
}
