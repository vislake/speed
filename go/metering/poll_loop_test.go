package metering

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// pollLoopStarted reports whether p currently holds a loop generation, for
// the lifecycle pins across this package's suites.
func pollLoopStarted(p *pollLoop) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.started
}

// TestPollLoop_Stop_WaitsForTheBodyGoroutineToExit pins the wait half of
// stop's contract: closing the stop channel is a request, and stop must
// not return until the generation's goroutine has actually exited -- a
// body can be mid-cycle (the Dispatcher's body may be inside a delivery
// attempt, the recorder's inside an Ingest) and a Stop that returned
// early would let its caller race a still-running loop.
func TestPollLoop_Stop_WaitsForTheBodyGoroutineToExit(t *testing.T) {
	var p pollLoop
	entered := make(chan struct{})
	release := make(chan struct{})
	p.start(context.Background(), nil, func(ctx context.Context, stop <-chan struct{}) {
		close(entered)
		<-release // hold the generation alive the way a mid-cycle body does
		<-stop    // then exit through the stop path
	})

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop body never ran after start")
	}

	stopped := make(chan struct{})
	go func() {
		p.stop(nil)
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("stop returned before the loop body exited")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return after the loop body exited")
	}
}

// TestPollLoop_StartAfterStop_RunsAFreshGeneration pins that a completed
// stop leaves the loop restartable: stop must clear the started flag for
// the generation it waited on, or a later start would be a permanent
// no-op and the host's rows (or buffered events) would sit unserviced
// forever.
func TestPollLoop_StartAfterStop_RunsAFreshGeneration(t *testing.T) {
	var p pollLoop
	var generations atomic.Int64
	body := func(ctx context.Context, stop <-chan struct{}) {
		generations.Add(1)
		<-stop
	}

	p.start(context.Background(), nil, body)
	waitFor(t, func() bool { return generations.Load() == 1 })
	p.stop(nil)
	if pollLoopStarted(&p) {
		t.Fatal("stop returned with the started flag still set: a later start would be a permanent no-op")
	}

	p.start(context.Background(), nil, body)
	waitFor(t, func() bool { return generations.Load() == 2 })
	p.stop(nil)
}

// TestPollLoop_CanceledContext_ExitsAndLeavesItRestartable pins the exit
// contract on the cancellation path (the one with no stop call to clean
// up after it): a generation whose ctx is canceled must exit on its own
// and clear its own started flag, so a host can Start a fresh loop with a
// new ctx -- the shape the cancel-restart regressions in both host suites
// depend on.
func TestPollLoop_CanceledContext_ExitsAndLeavesItRestartable(t *testing.T) {
	var p pollLoop
	var generations atomic.Int64
	body := func(ctx context.Context, stop <-chan struct{}) {
		generations.Add(1)
		select {
		case <-ctx.Done():
		case <-stop:
		}
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	p.start(ctx1, nil, body)
	waitFor(t, func() bool { return generations.Load() == 1 })
	cancel1()

	// The canceled generation must clear the started flag on exit, or the
	// next start would be a permanent no-op.
	waitFor(t, func() bool { return !pollLoopStarted(&p) })

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	p.start(ctx2, nil, body)
	waitFor(t, func() bool { return generations.Load() == 2 })
	p.stop(nil)
}

// TestPollLoop_StartWhileRunning_IsANoOp pins the one-generation-at-a-time
// rule: a start while a generation is running must touch nothing, or a
// host would end up with two loops racing the same buffer (or the same
// outbox rows).
func TestPollLoop_StartWhileRunning_IsANoOp(t *testing.T) {
	var p pollLoop
	var generations atomic.Int64
	body := func(ctx context.Context, stop <-chan struct{}) {
		generations.Add(1)
		<-stop
	}

	p.start(context.Background(), nil, body)
	waitFor(t, func() bool { return generations.Load() == 1 })

	p.start(context.Background(), nil, body)
	if got := generations.Load(); got != 1 {
		t.Fatalf("generations = %d after a start while running, want 1 (the running generation must not be duplicated)", got)
	}
	p.stop(nil)
}

// TestPollLoop_Stop_BeforeStartAndRepeated_IsSafe pins the either-order
// call contract both hosts promise their own callers: a stop before any
// start must not block, panic, or consume anything (a later start's
// generation must remain fully stoppable), and a repeated stop must not
// panic on a second close of the same generation's stop channel.
func TestPollLoop_Stop_BeforeStartAndRepeated_IsSafe(t *testing.T) {
	var p pollLoop
	p.stop(nil) // before any start: must not block or consume anything

	body := func(ctx context.Context, stop <-chan struct{}) { <-stop }
	p.start(context.Background(), nil, body)
	p.stop(nil)
	p.stop(nil) // repeated: must not panic or block
}

// TestPollLoop_Hooks_RunAsTheHostsExpect pins the two host-state hooks:
// onStop runs under stop's lock even when no generation is running (the
// recorder latches per Stop call, not per running loop), and onSpawn runs
// exactly once per spawned generation -- not on a no-op start, whose early
// return must leave the running generation's host state untouched.
func TestPollLoop_Hooks_RunAsTheHostsExpect(t *testing.T) {
	var p pollLoop
	var spawns, stops atomic.Int64

	p.stop(func() { stops.Add(1) })
	if got := stops.Load(); got != 1 {
		t.Fatalf("onStop ran %d times with no loop running, want 1", got)
	}

	var generations atomic.Int64
	body := func(ctx context.Context, stop <-chan struct{}) {
		generations.Add(1)
		<-stop
	}
	spawn := func() { spawns.Add(1) }

	p.start(context.Background(), spawn, body)
	waitFor(t, func() bool { return generations.Load() == 1 })

	p.start(context.Background(), spawn, body) // no-op: one generation already running
	if got := spawns.Load(); got != 1 {
		t.Fatalf("onSpawn ran %d times across two starts with one generation, want 1", got)
	}

	p.stop(func() { stops.Add(1) })
	if got := stops.Load(); got != 2 {
		t.Fatalf("onStop ran %d times across two stops, want 2", got)
	}
}
