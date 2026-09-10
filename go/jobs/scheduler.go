package jobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// DefaultScheduleInterval is the tick granularity a Scheduler uses when no
// WithInterval option is given. One minute is the cadence the schedule
// windows this scheduler drives are sized against (a one-hour window leaves
// a 60:1 ratio), so the cost of the interval is at most one enqueue attempt
// per declaration per minute, with the window-scoped idempotency key
// collapsing same-window ticks onto a single job.
const DefaultScheduleInterval = time.Minute

// ErrTenantListerRequired is returned by Scheduler.Start when a
// PerTenant-scope declaration exists but no TenantLister was wired through
// WithTenantLister. The error names every declaration that would have been
// skipped: silently not sweeping is the failure this refusal exists to
// prevent.
var ErrTenantListerRequired = errors.New("jobs: scheduler has per-tenant declarations but no tenant lister")

// TenantLister is the host-supplied, structurally typed seam a Scheduler
// expands PerTenant declarations through: the tenants the scheduler
// enqueues a declaration's task for, one task per tenant per window.
//
// It is an interface with one method so that a host implementation already
// built for another module satisfies it without change -- the shape is
// go/compliance's own TenantLister (RetentionService.SweepAllTenants's
// seam) word for word, and a host needs exactly one such implementation
// wired into both.
type TenantLister interface {
	// ListTenants returns every tenant a periodic task should be
	// enqueued for. ctx carries no tenant of its own -- this is
	// inherently a cross-tenant enumeration -- so an implementation must
	// not require one. A returned error makes the current tick skip every
	// per-tenant declaration and retry on the next tick: the schedule
	// never stops on a listing failure.
	ListTenants(ctx context.Context) ([]pkgcore.TenantID, error)
}

// SchedulerOptions configure a Scheduler before it starts.
type SchedulerOption func(*Scheduler)

// WithSchedules wires the seat the Scheduler reads its declarations from --
// normally the bootstrapped Registry's Schedules registrar. Declarations
// are read once, at Start.
func WithSchedules(schedules pkgcore.PeriodicTaskRegistrar) SchedulerOption {
	return func(s *Scheduler) {
		if schedules == nil {
			panic("jobs: WithSchedules requires a non-nil pkgcore.PeriodicTaskRegistrar: a nil value would silently schedule nothing, hiding a wiring mistake")
		}
		s.schedules = schedules
	}
}

// WithTenantLister wires the lister the Scheduler expands PerTenant
// declarations through. Without it, Start refuses a declaration set
// containing any PerTenant declaration (ErrTenantListerRequired) rather
// than silently scheduling none of them.
func WithTenantLister(lister TenantLister) SchedulerOption {
	return func(s *Scheduler) {
		if lister == nil {
			panic("jobs: WithTenantLister requires a non-nil TenantLister: a nil value would silently skip every per-tenant declaration, hiding a wiring mistake")
		}
		s.lister = lister
	}
}

// WithInterval overrides DefaultScheduleInterval: the tick granularity of
// the Scheduler's loop. It does not change any declaration's window -- the
// window is the declaration's own Every -- it only decides how often the
// scheduler asks. A non-positive duration panics.
func WithInterval(d time.Duration) SchedulerOption {
	return func(s *Scheduler) {
		if d <= 0 {
			panic("jobs: WithInterval requires a positive duration")
		}
		s.interval = d
	}
}

// Scheduler drives the periodic tasks a host's modules declared on the
// Registry's Schedules seat: on each of its ticks it walks the declarations
// in declaration order and enqueues every Platform-scope declaration once
// (under that declaration's sentinel tenant) and every PerTenant-scope
// declaration once per tenant its TenantLister returns, each enqueue under
// the tenant's own context.
//
// # Keys and cadence
//
// Every enqueue carries a window-scoped idempotency key derived by this
// package (ScheduleIdempotencyKey / SchedulePlatformIdempotencyKey): the
// declaration's KeyPrefix, the tenant segment where the scope has one, and
// the window start -- the tick's instant truncated to the declaration's
// Every on the absolute clock (ScheduleWindowStart). Ticks inside one
// window therefore resolve one key and collapse onto one job, whatever the
// host's replica count; the first tick of a later window resolves a fresh
// key and the task runs again; and a dead-lettered task poisons only its
// own window. The derivation is deliberately identical to the one every
// declaring module's own Enqueue* method performs, so a scheduler tick and
// a manual enqueue landing in the same window dedupe onto one job rather
// than running the window twice.
//
// # Composition
//
// A Scheduler holds no state of its own and only ever calls Queue.Enqueue,
// so it runs unchanged on every deployment mode's queue. Multiple replicas
// starting a scheduler each are safe for the same reason multiple replicas
// of a hand-written host ticker were: the window-scoped key is the
// convergence point, not any leadership election. A replica that only
// schedules (never draining its own queue -- a host that did not call
// StandaloneQueue.Start) is a legal composition; so is the ordinary one
// where the same replica schedules and executes.
//
// # Failure semantics
//
// The tick body is synchronous: every enqueue completes before the next
// tick can fire, and a slow tick delays the next one rather than piling up.
// A failed enqueue, and a failed tenant listing, are logged and left for
// the next tick to retry -- enqueues are durable row inserts and a listing
// failure repeats, so nothing is lost by skipping the rest of a tick.
type Scheduler struct {
	queue     Queue
	schedules pkgcore.PeriodicTaskRegistrar
	lister    TenantLister
	interval  time.Duration

	// now is the clock the window start is derived from. It is a field
	// rather than a time.Now() call at the use site so that the window a
	// tick's enqueues land in is deterministic in tests -- the same
	// clock-seam pattern the scheduling modules' own services use -- while
	// defaulting to the real clock for every production call.
	now func() time.Time

	mu      sync.Mutex
	started bool
	stopped bool
	stopCh  chan struct{}
	doneCh  chan struct{}
}

// NewScheduler returns a Scheduler that enqueues the declarations wired
// through WithSchedules onto q. A nil queue panics: a scheduler without a
// queue has nowhere to enqueue and would silently do nothing. The returned
// scheduler does nothing until Start is called.
func NewScheduler(q Queue, opts ...SchedulerOption) *Scheduler {
	if q == nil {
		panic("jobs: NewScheduler requires a non-nil Queue")
	}
	s := &Scheduler{
		queue:    q,
		interval: DefaultScheduleInterval,
		now:      time.Now,
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Start launches the scheduler's tick loop and returns immediately. It is
// idempotent: a second Start on a started scheduler is a no-op returning
// nil.
//
// ctx is the context every tick's enqueues run under. It should be a
// long-lived context (context.Background() in the ordinary host), not one a
// shutdown sequence cancels before calling Stop: a cancelled ctx would fail
// the final tick's enqueues. The loop itself stops only through Stop.
//
// Start refuses, without starting anything, a declaration set that
// contains a PerTenant-scope declaration while no TenantLister was wired,
// with an error wrapping ErrTenantListerRequired that names those
// declarations.
func (s *Scheduler) Start(ctx context.Context) error {
	if ctx == nil {
		return errors.New("jobs: Scheduler.Start requires a non-nil context")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return errors.New("jobs: Scheduler.Start after Stop: a scheduler's lifecycle is one Start and one Stop")
	}
	if s.started {
		return nil
	}

	var decls []pkgcore.PeriodicTask
	if s.schedules != nil {
		decls = s.schedules.Declarations()
	}
	var missing []string
	for _, decl := range decls {
		if decl.Scope == pkgcore.PeriodicScopePerTenant {
			missing = append(missing, decl.Type)
		}
	}
	if len(missing) > 0 && s.lister == nil {
		return fmt.Errorf("%w: %s", ErrTenantListerRequired, strings.Join(missing, ", "))
	}

	s.started = true
	go s.run(ctx, decls)
	return nil
}

// run is the scheduler's loop. It exits when stopCh closes, which Stop does
// after the in-flight tick (if any) has returned; doneCh then tells Stop no
// enqueue is still running.
func (s *Scheduler) run(ctx context.Context, decls []pkgcore.PeriodicTask) {
	defer close(s.doneCh)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.tick(ctx, decls)
		}
	}
}

// Stop stops the tick loop and blocks until the in-flight tick (if any) has
// returned, so a caller knows no enqueue is still running against the queue
// it is about to close. It is idempotent, and a no-op on a scheduler that
// was never started.
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.started || s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	close(s.stopCh)
	s.mu.Unlock()
	<-s.doneCh
}

// tick performs one scheduler pass over decls: one platform enqueue per
// platform-scope declaration, then one enqueue per tenant for each
// per-tenant-scope declaration. The instant the window starts are derived
// from is read once per pass, so every enqueue of one tick shares one
// instant. The tenant list is resolved once per pass too (the first
// per-tenant declaration triggers it), mirroring the single universe
// resolution a hand-written host ticker performs; a listing failure skips
// the pass's per-tenant declarations and retries next tick.
func (s *Scheduler) tick(ctx context.Context, decls []pkgcore.PeriodicTask) {
	log := obs.FromContext(ctx)
	now := s.now()

	var tenants []pkgcore.TenantID
	var tenantsErr error
	tenantsResolved := false

	for _, decl := range decls {
		switch decl.Scope {
		case pkgcore.PeriodicScopePlatform:
			windowStart := ScheduleWindowStart(now, decl.Every)
			_, err := s.queue.Enqueue(ctx, Task{
				Type:           decl.Type,
				TenantID:       decl.PlatformTenant,
				IdempotencyKey: SchedulePlatformIdempotencyKey(decl.KeyPrefix, windowStart),
			})
			if err != nil {
				log.Warn("jobs: scheduler enqueue failed, will retry on the next tick",
					"task_type", decl.Type, "error", err)
			}
		case pkgcore.PeriodicScopePerTenant:
			if !tenantsResolved {
				tenantsResolved = true
				tenants, tenantsErr = s.lister.ListTenants(ctx)
				if tenantsErr != nil {
					log.Warn("jobs: scheduler tenant listing failed, will retry on the next tick",
						"error", tenantsErr)
				}
			}
			if tenantsErr != nil {
				continue
			}
			windowStart := ScheduleWindowStart(now, decl.Every)
			for _, tenant := range tenants {
				// The tenant travels on the task itself, and the enqueue
				// context carries it too, mirroring the shape the modules'
				// own schedule points use: the worker rebuilds the task's
				// tenant onto the handler context either way.
				tenantCtx := pkgcore.WithTenant(ctx, tenant)
				_, err := s.queue.Enqueue(tenantCtx, Task{
					Type:           decl.Type,
					TenantID:       tenant,
					IdempotencyKey: ScheduleIdempotencyKey(decl.KeyPrefix, tenant, windowStart),
				})
				if err != nil {
					log.Warn("jobs: scheduler enqueue failed, will retry on the next tick",
						"task_type", decl.Type, "tenant_id", tenant, "error", err)
				}
			}
		}
	}
}

// ScheduleWindowStart returns the start of the schedule window the instant
// now falls in: now truncated to window. Truncation is on the absolute
// clock, never a timezone-local calendar cut, so every replica of a
// multi-replica host agrees on the boundary regardless of its own location.
//
// It is the window start every periodic task's idempotency key is composed
// from (ScheduleIdempotencyKey / SchedulePlatformIdempotencyKey), and it is
// the same derivation the declaring modules' own Enqueue* methods perform
// with their own window constants -- the two must agree, or one window
// would run twice.
func ScheduleWindowStart(now time.Time, window time.Duration) time.Time {
	return now.Truncate(window)
}

// ScheduleIdempotencyKey derives the idempotency key one PerTenant-scope
// schedule enqueue is made under: prefix + the tenant segment + the window
// start as UTC RFC 3339. prefix is the declaring site's own established key
// prefix (pkgcore.PeriodicTask.KeyPrefix), so an enqueue this derivation
// produces and one the module's own Enqueue* method produces for the same
// (task type, tenant, window) are the same key and dedupe onto one job.
func ScheduleIdempotencyKey(prefix string, tenant pkgcore.TenantID, windowStart time.Time) string {
	return prefix + string(tenant) + ":" + windowStart.UTC().Format(time.RFC3339)
}

// SchedulePlatformIdempotencyKey derives the idempotency key one
// Platform-scope schedule enqueue is made under: prefix + the window start
// as UTC RFC 3339, with no tenant segment -- a platform-wide task belongs to
// no single tenant, so its key names the window alone. The same
// agreement-with-the-module contract ScheduleIdempotencyKey documents
// applies.
func SchedulePlatformIdempotencyKey(prefix string, windowStart time.Time) string {
	return prefix + windowStart.UTC().Format(time.RFC3339)
}
