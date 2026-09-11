package metering

import (
	"context"
	"sync"
)

// pollLoop is the background-goroutine lifecycle Dispatcher's delivery
// loop and AnalyticsRecorder's flush loop share: at most one loop
// generation at a time, startable, stoppable and restartable, with the
// generation's exit and Stop's wait exchanged over channels.
//
// # One generation at a time
//
// start spawns a generation's goroutine, or is a no-op while one is
// already running; stop ends it, waiting for the goroutine to actually
// exit before returning. A start after the running generation has exited
// runs a fresh one. Every field below is guarded by mu, and the goroutine
// reads the generation's channels only through the values start passes it
// as arguments, so no lifecycle field is ever read off the shared struct
// by a caller mutating it.
//
// # A canceled ctx leaves the loop restartable
//
// A generation exits when its stop channel is closed or its ctx is done.
// On exit it clears the started flag for its own generation -- unless a
// newer start has already replaced the channels -- so a canceled ctx
// leaves start restartable rather than a permanent no-op. A stop that
// initiated the exit clears the flag itself after waiting on done; the
// clearing is idempotent between the two.
//
// # Host state rides the hooks
//
// start's onSpawn and stop's onStop run under mu as part of their call,
// for host state that must flip atomically with the loop lifecycle
// (AnalyticsRecorder's stopped latch). Both are optional.
type pollLoop struct {
	mu         sync.Mutex
	started    bool // a loop goroutine is running (spawned, not yet stopped)
	stopClosed bool // stopCh has been closed (at most once per loop generation)
	stopCh     chan struct{}
	doneCh     chan struct{}
}

// start runs body in a new goroutine as the loop's one generation, until
// the generation's stop channel is closed or ctx is done. A start while
// the loop is already running is a no-op, and a start after the running
// generation has exited runs a fresh generation. Calling start
// immediately after canceling the previous ctx, before the exiting
// generation has finished its own exit, is still a no-op by the
// one-generation-at-a-time rule; wait for the exit (stop, or the started
// flag clearing) before restarting.
//
// onSpawn, when non-nil, runs under the lifecycle lock as part of the
// spawn. The exit handling -- closing done, clearing the started flag for
// the generation's own exit -- belongs to this type, so body neither
// closes done nor clears the flag, and must return once stop is closed or
// ctx is done.
func (p *pollLoop) start(ctx context.Context, onSpawn func(), body func(ctx context.Context, stop <-chan struct{})) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started {
		return
	}
	if onSpawn != nil {
		onSpawn()
	}
	p.started = true
	p.stopClosed = false
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	stop, done := p.stopCh, p.doneCh
	go func() {
		defer close(done)
		body(ctx, stop)
		p.clearStartedIfCurrent(done)
	}()
}

// stop closes the running generation's stop channel -- once per
// generation, so a concurrent or repeated stop is safe -- waits for its
// goroutine to exit, and clears the started flag for the generation it
// waited on (a start that replaced the channels meanwhile keeps its own
// generation's flag). It is safe to call before start, or more than once:
// a stop while no generation is running returns without touching
// anything, and a start after a completed stop runs a fresh generation.
//
// onStop, when non-nil, runs under the lifecycle lock before the
// running-generation check -- for host state that must flip atomically
// with the shutdown (AnalyticsRecorder's stopped latch), whether or not a
// generation was running.
func (p *pollLoop) stop(onStop func()) {
	p.mu.Lock()
	if onStop != nil {
		onStop()
	}
	if !p.started {
		p.mu.Unlock()
		return
	}
	if !p.stopClosed {
		p.stopClosed = true
		close(p.stopCh)
	}
	done := p.doneCh
	p.mu.Unlock()

	<-done

	p.clearStartedIfCurrent(done)
}

// clearStartedIfCurrent clears the started flag when the generation that
// is exiting is still the live one, so a later start can run a fresh
// generation. A stop that initiated this exit clears the flag itself
// after waiting on done (see stop); the clearing here is idempotent with
// that, and is what makes a ctx-canceled generation leave start
// restartable.
func (p *pollLoop) clearStartedIfCurrent(done chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.doneCh == done {
		p.started = false
	}
}
