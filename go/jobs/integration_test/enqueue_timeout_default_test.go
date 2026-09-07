//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	asynqlib "github.com/hibiken/asynq"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/queue/asynq"
)

// This file pins the "one seam, one default timeout" property across the
// two deployment-mode Queue implementations: a Job enqueued with NO
// WithTimeout option must be bounded by the same jobs.DefaultTimeout on
// both sides. The standalone side applies it through ResolveEnqueueOptions'
// fallback and the row's timeout_nanos (worker.go's execute reads it back);
// the distributed side must stamp the same bound on the task record asynq's
// processor enforces. asynq's own client (hibiken/asynq v0.26.0 client.go)
// applies its own internal 30-minute defaultTimeout whenever a task is
// enqueued "if neither deadline nor timeout are set" -- so the property
// holds only as long as the enqueue option list ALWAYS passes an explicit
// asynqlib.Timeout for an omitted jobs option. The suite is named for the
// behaviour it verifies, per the backend coding standard's test-naming
// rule.

// taskTimeout reads back the per-task timeout asynq's processor will
// enforce for id, probing the three fixed priority tier queues the same
// way queue/asynq's own findTaskInfo does (queue/asynq/store.go's
// queueCritical/queueDefault/queueLow). TaskInfo.Timeout is asynq's own
// reconstruction of the record's timeout field, so nothing here depends on
// the wire format.
func taskTimeout(t *testing.T, connOpt asynqlib.RedisConnOpt, id jobs.JobID) time.Duration {
	t.Helper()
	inspector := asynqlib.NewInspector(connOpt)
	t.Cleanup(func() { _ = inspector.Close() })
	for _, qn := range []string{"critical", "default", "low"} {
		info, err := inspector.GetTaskInfo(qn, string(id))
		if err == nil {
			return info.Timeout
		}
		if !errors.Is(err, asynqlib.ErrTaskNotFound) && !errors.Is(err, asynqlib.ErrQueueNotFound) {
			t.Fatalf("GetTaskInfo(%s, %s) error = %v", qn, id, err)
		}
	}
	t.Fatalf("task %s not found in any tier queue", id)
	return 0
}

func TestAsynqQueue_Enqueue_NoTimeoutOption_PinsJobsDefaultTimeout(t *testing.T) {
	ctx := context.Background()

	t.Run("omitted option lands on jobs.DefaultTimeout", func(t *testing.T) {
		connOpt := startRedisContainer(t, ctx)
		q := asynq.NewQueue(connOpt)
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = q.Close(cctx)
		})

		id, err := q.Enqueue(ctx, jobs.Task{Type: "probe.type", TenantID: "tenant-a"})
		if err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
		if got := taskTimeout(t, connOpt, id); got != jobs.DefaultTimeout {
			t.Errorf("task timeout = %v, want jobs.DefaultTimeout (%v) -- asynq's own 30-minute client default must never bound a Job whose host omitted the option, or the same Job is killed at 5 minutes in standalone mode and allowed to run for 30 in distributed mode", got, jobs.DefaultTimeout)
		}
	})

	t.Run("queue-configured default still applies when the option is omitted", func(t *testing.T) {
		connOpt := startRedisContainer(t, ctx)
		q := asynq.NewQueue(connOpt, asynq.WithJobTimeout(90*time.Second))
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = q.Close(cctx)
		})

		id, err := q.Enqueue(ctx, jobs.Task{Type: "probe.type", TenantID: "tenant-a"})
		if err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
		if got := taskTimeout(t, connOpt, id); got != 90*time.Second {
			t.Errorf("task timeout = %v, want the WithJobTimeout-configured 90s", got)
		}
	})

	t.Run("explicit WithTimeout still wins over both defaults", func(t *testing.T) {
		connOpt := startRedisContainer(t, ctx)
		q := asynq.NewQueue(connOpt, asynq.WithJobTimeout(90*time.Second))
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = q.Close(cctx)
		})

		id, err := q.Enqueue(ctx, jobs.Task{Type: "probe.type", TenantID: "tenant-a"}, jobs.WithTimeout(45*time.Second))
		if err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
		if got := taskTimeout(t, connOpt, id); got != 45*time.Second {
			t.Errorf("task timeout = %v, want the explicitly-passed 45s", got)
		}
	})
}
