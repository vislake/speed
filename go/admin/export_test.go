package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// TestExportService_Enqueue_EmptyTenantID_Refused pins the up-front
// validation: an empty tenantID never reaches jobs.Queue.Enqueue at all.
func TestExportService_Enqueue_EmptyTenantID_Refused(t *testing.T) {
	env := buildTestAdminModule(t)

	_, err := env.Admin.Export().Enqueue(context.Background(), "", "operator-1")
	if !isCode(err, ErrTenantIDRequired.Code) {
		t.Fatalf("Enqueue() with empty tenantID error = %v, want %s", err, ErrTenantIDRequired.Code)
	}
}

// TestExportService_Enqueue_EmptyOperatorUserID_Refused pins P1-2's other
// half of Enqueue's up-front validation: an empty operator id never
// reaches jobs.Queue.Enqueue either, since a Job with nothing to
// attribute it to would leave the export unattributable from the moment
// it is created.
func TestExportService_Enqueue_EmptyOperatorUserID_Refused(t *testing.T) {
	env := buildTestAdminModule(t)

	_, err := env.Admin.Export().Enqueue(context.Background(), "some-tenant", "")
	if !isCode(err, ErrExportOperatorRequired.Code) {
		t.Fatalf("Enqueue() with empty operatorUserID error = %v, want %s", err, ErrExportOperatorRequired.Code)
	}
}

// TestExportService_Enqueue_RunsRealExport_DeliversThroughSharing is D7's
// export-leg end-to-end proof: enqueuing a real job runs a real
// compliance.ExportService.Export against a real go/sharing.Service
// (buildTestAdminModule's own compliance.WithSharing wiring), and the
// job's Result carries the minted share id and token -- never run
// synchronously inside the call that enqueues it (Enqueue returns before
// the worker has necessarily even claimed the job).
func TestExportService_Enqueue_RunsRealExport_DeliversThroughSharing(t *testing.T) {
	env := buildTestAdminModule(t)

	// The worker only dispatches to handlers registered on the queue
	// itself (StandaloneQueue.RegisterHandler) -- reg.Jobs.Handlers()
	// (what Module.Register populated) is what a host loops over to wire
	// this in production (examples/reference-app/cmd/server/server.go);
	// this test performs the identical one-handler registration by hand.
	if err := env.Queue.RegisterHandler(env.Admin.Export()); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-export-flow")
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Export Flow Co", "workspace"); err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}

	jobID, err := env.Admin.Export().Enqueue(context.Background(), string(tenant), "operator-1")
	if err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	if jobID == "" {
		t.Fatal("Enqueue() returned an empty job id")
	}

	systemCtx, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor: "test", Purpose: SystemPurposeAdminCrossTenant,
	})
	if err != nil {
		t.Fatalf("WithSystemContext() error = %v", err)
	}

	var job *jobs.Job
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err = env.Queue.Get(systemCtx, jobID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusDeadLetter {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.Status != jobs.StatusSucceeded {
		t.Fatalf("job status = %s (error=%q), want %s", job.Status, job.Error, jobs.StatusSucceeded)
	}
	if job.Result == nil {
		t.Fatal("job.Result is nil after a succeeded export job")
	}

	var result exportJobResult
	if err := json.Unmarshal(job.Result.Data, &result); err != nil {
		t.Fatalf("decode job.Result.Data: %v", err)
	}
	if result.ShareID == "" || result.Token == "" {
		t.Errorf("export job result = %+v, want a non-empty ShareID and Token", result)
	}
	if result.ObjectKey == "" {
		t.Errorf("export job result = %+v, want a non-empty ObjectKey", result)
	}
}

// TestHandler_AdminExportAuditEvents_AttributesOperatorAndEmitsAuditAction
// is P1-2's THE scenario: a real audit export driven through admin's own
// real, composed HTTP handler (never a bare service-level call) must
// attribute the calling operator through the whole flow and must itself
// leave an admin.audit_export row naming that operator as Actor -- neither
// of which held on unfixed main, where AdminExportAuditEvents never even
// read the caller's Principal.
func TestHandler_AdminExportAuditEvents_AttributesOperatorAndEmitsAuditAction(t *testing.T) {
	env := buildTestAdminModule(t)
	if err := env.Queue.RegisterHandler(env.Admin.Export()); err != nil {
		t.Fatalf("RegisterHandler() error = %v", err)
	}

	const tenant = pkgcore.TenantID("tenant-export-attribution")
	if _, err := env.Org.Tree().CreateRoot(pkgcore.WithTenant(context.Background(), tenant), "Export Attribution Co", "workspace"); err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}

	var recorded []audit.RecordedEvent
	env.Registry.EventBus().Subscribe(audit.EventRecorded, func(_ context.Context, evt pkgcore.Event) error {
		if rec, ok := evt.Payload.(audit.RecordedEvent); ok {
			recorded = append(recorded, rec)
		}
		return nil
	})

	const operatorID = "operator-attributed-42"
	body := strings.NewReader(`{"tenantId":"` + string(tenant) + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/audit-events/export", body)
	req = req.WithContext(authn.WithPrincipal(req.Context(), authn.Principal{UserID: operatorID}))
	w := httptest.NewRecorder()

	env.Admin.handler.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s, want %d", w.Code, w.Body.String(), http.StatusAccepted)
	}
	var resp struct {
		JobID string `json:"jobId"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.JobID == "" {
		t.Fatal("response carries no jobId")
	}

	systemCtx, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor: "test", Purpose: SystemPurposeAdminCrossTenant,
	})
	if err != nil {
		t.Fatalf("WithSystemContext() error = %v", err)
	}
	var job *jobs.Job
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err = env.Queue.Get(systemCtx, jobs.JobID(resp.JobID))
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusDeadLetter {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.Status != jobs.StatusSucceeded {
		t.Fatalf("job status = %s (error=%q), want %s", job.Status, job.Error, jobs.StatusSucceeded)
	}

	found := false
	for _, evt := range recorded {
		if evt.Action != AuditActionAuditExport {
			continue
		}
		found = true
		if evt.Actor.ID != operatorID {
			t.Errorf("Actor.ID = %q, want the calling operator %q", evt.Actor.ID, operatorID)
		}
		if evt.Resource.ID != string(tenant) {
			t.Errorf("Resource.ID = %q, want the exported tenant %q", evt.Resource.ID, tenant)
		}
	}
	if !found {
		t.Fatalf("no %q audit event recorded (recorded=%+v) -- the export left no attributable trace of itself", AuditActionAuditExport, recorded)
	}
}
