//go:build integration

package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/jobs/queue/asynq"
	"github.com/vislake/speed/go/jobs/queuetest"
)

// TestAsynqQueue_FailsClosedOnUnreadableCancellationState is the
// distributed deployment mode's leg of the queuetest injectable-fault tier:
// it runs queuetest.AssertFailsClosedOnUnreadableCancellationState — the
// suite proving a Queue whose cancellation-state read fails fails closed on
// its reporting surfaces — against asynq's Queue with a REAL injected read
// failure. asynq's Queue keeps a Job's cancellation state in its own Redis
// marker (queue/asynq/queue.go's cancelMarkerKey, an "asynqjobs:cancelled:"
// key written by Cancel and read by Get/DeadLetterJobs and the dispatch
// check alike), a second state source that can fail while asynq's own
// task machinery keeps working — the exact shape in which the divergence
// commit c26b058b corrected happened. The leg's adapter therefore
// sabotages the marker key itself, the destructive injection the finding
// names: the marker (a string) is deleted and replaced with a Redis LIST,
// so every read of it answers a real WRONGTYPE error on a real Redis, and
// Repair deletes the sabotage and restores the captured marker value — the
// outage delays the report, it does not lose the cancellation.
//
// The c26b058b round's own hand-written regressions (marker_read_fail_
// closed_test.go) proved the same direction against the same sabotage for
// asynq alone, before this tier existed; this leg is the shared-suite form
// of that proof — the same two checks (Get on a possibly-cancelled Job, a
// DeadLetterJobs listing) that StandaloneQueue's leg in go/jobs's unit tier
// runs against its own real injection, so the failure direction is measured
// by ONE suite across both implementations instead of by two independently
// hand-maintained files.
func TestAsynqQueue_FailsClosedOnUnreadableCancellationState(t *testing.T) {
	ctx := context.Background()
	queuetest.AssertFailsClosedOnUnreadableCancellationState(t, func() queuetest.FaultRunnable {
		connOpt := startRedisContainer(t, ctx)
		q := asynq.NewQueue(connOpt, testAsynqDefaultOpts...)

		// A raw client on the SAME Redis instance, for the sabotage: the
		// queue's own rdb is private (and must stay so) — the identical
		// arrangement marker_read_fail_closed_test.go uses.
		raw, ok := connOpt.MakeRedisClient().(redis.UniversalClient)
		if !ok {
			t.Fatalf("MakeRedisClient() = %T, want a redis.UniversalClient", connOpt.MakeRedisClient())
		}
		t.Cleanup(func() { _ = raw.Close() })
		return &asynqFaultQueue{Queue: q, raw: raw, captured: make(map[string]string)}
	})
}

// asynqFaultQueue is the asynq leg's test-side adapter: the concrete queue
// embedded (so its RegisterHandler/Start/Close/DeadLetterJobs promote
// unchanged) plus a raw client on the same Redis, implementing
// queuetest.CancellationStateFault by corrupting the Job's cancellation
// marker into a LIST and restoring it.
type asynqFaultQueue struct {
	*asynq.Queue
	raw redis.UniversalClient

	// captured remembers, per sabotaged id, the marker value the sabotage
	// destroyed ("" when no marker existed), so Repair can restore exactly
	// what was there — including the cancellation a Get check depends on.
	captured map[string]string
}

// cancelMarkerKeyForFault is queue/asynq/queue.go's cancelMarkerKey
// ("asynqjobs:cancelled:" + id), reproduced verbatim exactly like
// marker_read_fail_closed_test.go reproduces it. If it ever drifts, the
// sabotage stops failing the read and the checks time out loudly rather
// than passing silently.
func cancelMarkerKeyForFault(id jobs.JobID) string {
	return "asynqjobs:cancelled:" + string(id)
}

// Sabotage implements queuetest.CancellationStateFault: the marker (a
// string, if one exists) is captured, deleted, and replaced with a LIST,
// so every marker read answers WRONGTYPE until Repair — a real read failure
// on the real Redis the queue reads through.
func (a *asynqFaultQueue) Sabotage(ctx context.Context, id jobs.JobID) error {
	key := cancelMarkerKeyForFault(id)
	val, err := a.raw.Get(ctx, key).Result()
	switch {
	case errors.Is(err, redis.Nil):
		a.captured[string(id)] = "" // no marker existed; Repair only deletes the sabotage
	case err != nil:
		return fmt.Errorf("sabotage (read marker before corrupting it): %w", err)
	default:
		a.captured[string(id)] = val
	}
	if err := a.raw.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("sabotage (del marker): %w", err)
	}
	if err := a.raw.RPush(ctx, key, "sabotage").Err(); err != nil {
		return fmt.Errorf("sabotage (list marker): %w", err)
	}
	return nil
}

// Repair implements queuetest.CancellationStateFault: the sabotage is
// deleted and the captured marker value restored — a Redis value's type
// cannot be swapped in place, so restoration is a delete-then-set, exactly
// like marker_read_fail_closed_test.go's repair.
func (a *asynqFaultQueue) Repair(ctx context.Context, id jobs.JobID) error {
	key := cancelMarkerKeyForFault(id)
	if err := a.raw.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("repair (del sabotage): %w", err)
	}
	if val, ok := a.captured[string(id)]; ok && val != "" {
		if err := a.raw.Set(ctx, key, val, 0).Err(); err != nil {
			return fmt.Errorf("repair (restore marker): %w", err)
		}
	}
	delete(a.captured, string(id))
	return nil
}
