//go:build integration

package jobs_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// TestRedisQueue_Enqueue_LogsSingleCorrectTenantID is Queue's half of
// the "job enqueued" duplicate/conflicting tenant_id regression: the same
// bug StandaloneQueue had (see go/jobs's own standalone_queue_test.go
// TestEnqueue_LogsSingleCorrectTenantID_EvenWhenCtxTenantDiffers, in the
// parent package and so not importable from here) also existed in
// Queue.Enqueue's own, separate obs.FromContext(ctx).Info("job
// enqueued", ...) call (go/jobs/queue/asynq's queue.go): an explicit "tenant_id" kv for
// task.TenantID logged on top of whatever obs.FromContext(ctx) already
// auto-attaches from ctx's own ambient tenant. AGENTS.md documents the
// "platform-level scheduler enqueuing one cleanup Task per tenant in a
// loop" pattern as equally legitimate for Queue.Enqueue and
// StandaloneQueue.Enqueue alike, so whenever ctx's ambient tenant differs from
// task.TenantID, the pre-fix line carried both side by side --
// slog.TextHandler does not deduplicate repeated attribute keys.
//
// Run against a real Redis-backed Queue (not StandaloneQueue) because the
// fix lives in Queue's own Enqueue method, with its own independent
// call to obs.FromContext -- fixing StandaloneQueue's copy does not, by itself,
// prove anything about this one.
func TestRedisQueue_Enqueue_LogsSingleCorrectTenantID(t *testing.T) {
	ctx := context.Background()
	q := startTestAsynqQueue(t, ctx)

	// ctx's own ambient tenant deliberately differs from the Task being
	// enqueued, mirroring a platform scheduler that itself runs under one
	// context while looping over many tenants' own Tasks.
	enqueueCtx := pkgcore.WithTenant(context.Background(), "scheduler-tenant")
	var buf bytes.Buffer
	enqueueCtx = obs.WithLogger(enqueueCtx, slog.New(slog.NewTextHandler(&buf, nil)))

	if _, err := q.Enqueue(enqueueCtx, jobs.Task{Type: "cleanup", TenantID: "tenant-b"}); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	out := buf.String()
	if n := strings.Count(out, "tenant_id="); n != 1 {
		t.Fatalf("log line has %d tenant_id attributes, want exactly 1; got: %s", n, out)
	}
	if want := "tenant_id=tenant-b"; !strings.Contains(out, want) {
		t.Errorf("log line missing %q (the Job's own owning tenant); got: %s", want, out)
	}
	if strings.Contains(out, "tenant_id=scheduler-tenant") {
		t.Errorf("log line leaked ctx's ambient tenant instead of task.TenantID; got: %s", out)
	}
}
