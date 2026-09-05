package admin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
)

// TestExportService_Enqueue_EmptyTenantID_Refused pins the up-front
// validation: an empty tenantID never reaches jobs.Queue.Enqueue at all.
func TestExportService_Enqueue_EmptyTenantID_Refused(t *testing.T) {
	env := buildTestAdminModule(t)

	_, err := env.Admin.Export().Enqueue(context.Background(), "")
	if !isCode(err, ErrTenantIDRequired.Code) {
		t.Fatalf("Enqueue() with empty tenantID error = %v, want %s", err, ErrTenantIDRequired.Code)
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

	jobID, err := env.Admin.Export().Enqueue(context.Background(), string(tenant))
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
