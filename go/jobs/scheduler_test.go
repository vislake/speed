package jobs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// recordingQueue is the Queue fake the scheduler tests drive: it records
// every Enqueue call (the task and the context it arrived on) and nothing
// else, since the scheduler only ever enqueues.
type recordingQueue struct {
	mu     sync.Mutex
	tasks  []Task
	ctxs   []context.Context
	failOn error
}

func (q *recordingQueue) Enqueue(ctx context.Context, task Task, _ ...EnqueueOption) (JobID, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failOn != nil {
		return "", q.failOn
	}
	q.tasks = append(q.tasks, task)
	q.ctxs = append(q.ctxs, ctx)
	return JobID("job-1"), nil
}

func (q *recordingQueue) Get(context.Context, JobID) (*Job, error) { return nil, ErrJobNotFound }

func (q *recordingQueue) Cancel(context.Context, JobID) error { return ErrJobNotFound }

func (q *recordingQueue) recorded() []Task {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]Task(nil), q.tasks...)
}

func (q *recordingQueue) recordedCtxs() []context.Context {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]context.Context(nil), q.ctxs...)
}

// recordingLister is the TenantLister fake: a fixed tenant set, an optional
// error, and a call count.
type recordingLister struct {
	mu      sync.Mutex
	tenants []pkgcore.TenantID
	err     error
	calls   int
}

func (l *recordingLister) ListTenants(context.Context) ([]pkgcore.TenantID, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return l.tenants, l.err
}

func (l *recordingLister) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

// newTestSchedules returns a real Registry's Schedules seat carrying decls,
// so the tests exercise the seat the scheduler actually reads.
func newTestSchedules(t *testing.T, decls ...pkgcore.PeriodicTask) pkgcore.PeriodicTaskRegistrar {
	t.Helper()
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.Schedules.Add(decls...); err != nil {
		t.Fatalf("seeding the Schedules seat: %v", err)
	}
	return reg.Schedules
}

// fixedClock returns a clock function frozen at instant.
func fixedClock(instant time.Time) func() time.Time {
	return func() time.Time { return instant }
}

func TestScheduler_Tick_PlatformDeclarationEnqueuesOnceUnderItsSentinel(t *testing.T) {
	instant := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	decl := pkgcore.PeriodicTask{
		Type:           "pki.expiry_scan",
		Every:          time.Hour,
		Scope:          pkgcore.PeriodicScopePlatform,
		KeyPrefix:      "pki.expiry_scan:",
		PlatformTenant: "_pki_platform_scan",
	}
	schedules := newTestSchedules(t, decl)

	q := &recordingQueue{}
	s := NewScheduler(q, WithSchedules(schedules))
	s.now = fixedClock(instant)

	s.tick(context.Background(), schedules.Declarations())

	tasks := q.recorded()
	if len(tasks) != 1 {
		t.Fatalf("tick enqueued %d tasks, want 1", len(tasks))
	}
	if tasks[0].Type != decl.Type {
		t.Errorf("task Type = %q, want %q", tasks[0].Type, decl.Type)
	}
	if tasks[0].TenantID != decl.PlatformTenant {
		t.Errorf("task TenantID = %q, want the declaration's sentinel %q", tasks[0].TenantID, decl.PlatformTenant)
	}
	if want := "pki.expiry_scan:2026-03-04T05:00:00Z"; tasks[0].IdempotencyKey != want {
		t.Errorf("task IdempotencyKey = %q, want the window-scoped %q", tasks[0].IdempotencyKey, want)
	}
}

func TestScheduler_Tick_PerTenantExpandsThroughTheLister(t *testing.T) {
	instant := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	decl := pkgcore.PeriodicTask{
		Type:      "compliance.retention_sweep",
		Every:     time.Hour,
		Scope:     pkgcore.PeriodicScopePerTenant,
		KeyPrefix: "compliance.retention_sweep:",
	}
	schedules := newTestSchedules(t, decl)

	lister := &recordingLister{tenants: []pkgcore.TenantID{"acme", "clinic-1"}}
	q := &recordingQueue{}
	s := NewScheduler(q, WithSchedules(schedules), WithTenantLister(lister))
	s.now = fixedClock(instant)

	s.tick(context.Background(), schedules.Declarations())

	if got := lister.callCount(); got != 1 {
		t.Errorf("ListTenants called %d times in one tick, want 1 (one universe resolution per pass)", got)
	}
	tasks := q.recorded()
	if len(tasks) != 2 {
		t.Fatalf("tick enqueued %d tasks, want one per tenant (2)", len(tasks))
	}
	want := []struct {
		tenant pkgcore.TenantID
		key    string
	}{
		{"acme", "compliance.retention_sweep:acme:2026-03-04T05:00:00Z"},
		{"clinic-1", "compliance.retention_sweep:clinic-1:2026-03-04T05:00:00Z"},
	}
	for i, w := range want {
		if tasks[i].Type != decl.Type {
			t.Errorf("task %d Type = %q, want %q", i, tasks[i].Type, decl.Type)
		}
		if tasks[i].TenantID != w.tenant {
			t.Errorf("task %d TenantID = %q, want %q", i, tasks[i].TenantID, w.tenant)
		}
		if tasks[i].IdempotencyKey != w.key {
			t.Errorf("task %d IdempotencyKey = %q, want %q", i, tasks[i].IdempotencyKey, w.key)
		}
	}

	// Each enqueue arrives on its own tenant's context.
	ctxs := q.recordedCtxs()
	for i, w := range want {
		if got, ok := pkgcore.TenantFromContext(ctxs[i]); !ok || got != w.tenant {
			t.Errorf("task %d enqueue context tenant = %q (ok=%t), want %q", i, got, ok, w.tenant)
		}
	}
}

func TestScheduler_Tick_SameWindowTicksResolveOneKeyAndTheNextWindowRunsAgain(t *testing.T) {
	decl := pkgcore.PeriodicTask{
		Type:      "storage.expiry_sweep",
		Every:     time.Hour,
		Scope:     pkgcore.PeriodicScopePerTenant,
		KeyPrefix: "storage.sweep:",
	}
	schedules := newTestSchedules(t, decl)

	lister := &recordingLister{tenants: []pkgcore.TenantID{"acme"}}
	q := &recordingQueue{}
	s := NewScheduler(q, WithSchedules(schedules), WithTenantLister(lister))

	decls := schedules.Declarations()
	s.now = fixedClock(time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC))
	s.tick(context.Background(), decls)
	s.now = fixedClock(time.Date(2026, 3, 4, 5, 59, 59, 0, time.UTC))
	s.tick(context.Background(), decls)

	tasks := q.recorded()
	if len(tasks) != 2 {
		t.Fatalf("two same-window ticks enqueued %d tasks, want 2", len(tasks))
	}
	if tasks[0].IdempotencyKey != tasks[1].IdempotencyKey {
		t.Errorf("same-window ticks resolved keys %q and %q, want one key for the window", tasks[0].IdempotencyKey, tasks[1].IdempotencyKey)
	}

	s.now = fixedClock(time.Date(2026, 3, 4, 6, 1, 0, 0, time.UTC))
	s.tick(context.Background(), decls)
	tasks = q.recorded()
	if len(tasks) != 3 {
		t.Fatalf("the next window's tick left %d tasks, want 3", len(tasks))
	}
	if tasks[2].IdempotencyKey == tasks[1].IdempotencyKey {
		t.Errorf("the next window resolved the same key %q, want a fresh one so the sweep runs again", tasks[2].IdempotencyKey)
	}
}

func TestScheduler_Tick_ListerFailureSkipsPerTenantDeclarationsAndRetriesNextTick(t *testing.T) {
	platform := pkgcore.PeriodicTask{
		Type:           "pki.expiry_scan",
		Every:          time.Hour,
		Scope:          pkgcore.PeriodicScopePlatform,
		KeyPrefix:      "pki.expiry_scan:",
		PlatformTenant: "_pki_platform_scan",
	}
	perTenant := pkgcore.PeriodicTask{
		Type:      "compliance.retention_sweep",
		Every:     time.Hour,
		Scope:     pkgcore.PeriodicScopePerTenant,
		KeyPrefix: "compliance.retention_sweep:",
	}
	schedules := newTestSchedules(t, platform, perTenant)

	lister := &recordingLister{err: errors.New("tenant directory unavailable")}
	q := &recordingQueue{}
	s := NewScheduler(q, WithSchedules(schedules), WithTenantLister(lister))
	s.now = fixedClock(time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC))

	decls := schedules.Declarations()
	s.tick(context.Background(), decls)

	tasks := q.recorded()
	if len(tasks) != 1 || tasks[0].Type != platform.Type {
		t.Fatalf("a failing lister left tasks %v, want exactly the platform declaration's task", tasks)
	}

	// The next tick retries the listing and enqueues the per-tenant task.
	lister.mu.Lock()
	lister.err = nil
	lister.tenants = []pkgcore.TenantID{"acme"}
	lister.mu.Unlock()
	s.tick(context.Background(), decls)

	tasks = q.recorded()
	if len(tasks) != 3 {
		t.Fatalf("after the lister recovered, %d tasks were recorded, want 3 (platform twice, sweep once)", len(tasks))
	}
	if tasks[2].Type != perTenant.Type || tasks[2].TenantID != "acme" {
		t.Errorf("recovered tick's third task = %+v, want %q for acme", tasks[2], perTenant.Type)
	}
}

func TestScheduler_Tick_EnqueueFailureIsLeftForTheNextTick(t *testing.T) {
	decl := pkgcore.PeriodicTask{
		Type:      "storage.expiry_sweep",
		Every:     time.Hour,
		Scope:     pkgcore.PeriodicScopePerTenant,
		KeyPrefix: "storage.sweep:",
	}
	schedules := newTestSchedules(t, decl)
	lister := &recordingLister{tenants: []pkgcore.TenantID{"acme"}}

	q := &recordingQueue{failOn: errors.New("queue unavailable")}
	s := NewScheduler(q, WithSchedules(schedules), WithTenantLister(lister))
	s.now = fixedClock(time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC))

	decls := schedules.Declarations()
	s.tick(context.Background(), decls)
	if got := q.recorded(); len(got) != 0 {
		t.Fatalf("a failing enqueue recorded %d tasks, want 0", len(got))
	}

	q.mu.Lock()
	q.failOn = nil
	q.mu.Unlock()
	s.tick(context.Background(), decls)
	if got := q.recorded(); len(got) != 1 {
		t.Fatalf("the next tick retained nothing after the queue recovered: %d tasks, want 1", len(got))
	}
}

func TestScheduler_Tick_ProcessesDeclarationsInOrder(t *testing.T) {
	first := pkgcore.PeriodicTask{
		Type:           "pki.crl_regenerate",
		Every:          time.Hour,
		Scope:          pkgcore.PeriodicScopePlatform,
		KeyPrefix:      "pki.crl_regenerate:",
		PlatformTenant: "_pki_platform_crl",
	}
	second := pkgcore.PeriodicTask{
		Type:           "pki.expiry_scan",
		Every:          time.Hour,
		Scope:          pkgcore.PeriodicScopePlatform,
		KeyPrefix:      "pki.expiry_scan:",
		PlatformTenant: "_pki_platform_scan",
	}
	schedules := newTestSchedules(t, first, second)

	q := &recordingQueue{}
	s := NewScheduler(q, WithSchedules(schedules))
	s.now = fixedClock(time.Date(2026, 3, 4, 5, 6, 0, 0, time.UTC))

	s.tick(context.Background(), schedules.Declarations())

	tasks := q.recorded()
	if len(tasks) != 2 || tasks[0].Type != first.Type || tasks[1].Type != second.Type {
		t.Errorf("tick order = %v, want declaration order [%q %q]", tasks, first.Type, second.Type)
	}
}

func TestScheduleWindowStart_TruncatesOnTheAbsoluteClock(t *testing.T) {
	tests := []struct {
		name   string
		now    time.Time
		window time.Duration
		want   time.Time
	}{
		{
			name:   "an hour window truncates to the hour boundary",
			now:    time.Date(2026, 3, 4, 5, 37, 12, 500_000_000, time.UTC),
			window: time.Hour,
			want:   time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC),
		},
		{
			name:   "a 15-minute window truncates to its own boundary",
			now:    time.Date(2026, 3, 4, 5, 37, 12, 500_000_000, time.UTC),
			window: 15 * time.Minute,
			want:   time.Date(2026, 3, 4, 5, 30, 0, 0, time.UTC),
		},
		{
			name:   "an instant already on a boundary is its own window start",
			now:    time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC),
			window: time.Hour,
			want:   time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ScheduleWindowStart(tt.now, tt.window); !got.Equal(tt.want) {
				t.Errorf("ScheduleWindowStart(%s, %s) = %s, want %s", tt.now, tt.window, got, tt.want)
			}
		})
	}

	// The truncation is on the absolute clock: the same instant expressed in
	// another location lands in the same window, never a local calendar cut.
	utcInstant := time.Date(2026, 3, 4, 23, 37, 0, 0, time.UTC)
	tokyo := time.FixedZone("UTC+9", 9*60*60)
	if got, want := ScheduleWindowStart(utcInstant.In(tokyo), time.Hour), ScheduleWindowStart(utcInstant, time.Hour); !got.Equal(want) {
		t.Errorf("the same instant in UTC+9 truncated to %s, want %s -- truncation must not be timezone-local", got, want)
	}
}

func TestScheduleIdempotencyKey_Derivation(t *testing.T) {
	windowStart := time.Date(2026, 3, 4, 5, 0, 0, 0, time.UTC)

	if got, want := ScheduleIdempotencyKey("storage.sweep:", "acme", windowStart), "storage.sweep:acme:2026-03-04T05:00:00Z"; got != want {
		t.Errorf("ScheduleIdempotencyKey = %q, want %q", got, want)
	}
	if got, want := SchedulePlatformIdempotencyKey("pki.crl_regenerate:", windowStart), "pki.crl_regenerate:2026-03-04T05:00:00Z"; got != want {
		t.Errorf("SchedulePlatformIdempotencyKey = %q, want %q", got, want)
	}

	// The window stamp is normalized to UTC: a window start expressed in
	// another location spells the same key.
	tokyo := time.FixedZone("UTC+9", 9*60*60)
	if got, want := ScheduleIdempotencyKey("storage.sweep:", "acme", windowStart.In(tokyo)), "storage.sweep:acme:2026-03-04T05:00:00Z"; got != want {
		t.Errorf("a UTC+9 window start spelled %q, want the same UTC stamp %q", got, want)
	}
}

func TestScheduler_Start_RefusesPerTenantDeclarationWithoutLister(t *testing.T) {
	platform := pkgcore.PeriodicTask{
		Type:           "pki.expiry_scan",
		Every:          time.Hour,
		Scope:          pkgcore.PeriodicScopePlatform,
		KeyPrefix:      "pki.expiry_scan:",
		PlatformTenant: "_pki_platform_scan",
	}
	perTenant := pkgcore.PeriodicTask{
		Type:      "compliance.retention_sweep",
		Every:     time.Hour,
		Scope:     pkgcore.PeriodicScopePerTenant,
		KeyPrefix: "compliance.retention_sweep:",
	}
	schedules := newTestSchedules(t, platform, perTenant)

	q := &recordingQueue{}
	s := NewScheduler(q, WithSchedules(schedules), WithInterval(5*time.Millisecond))
	err := s.Start(context.Background())
	if !errors.Is(err, ErrTenantListerRequired) {
		t.Fatalf("Start without a lister = %v, want an error wrapping ErrTenantListerRequired", err)
	}
	if !strings.Contains(err.Error(), perTenant.Type) {
		t.Errorf("Start error %q does not name the per-tenant declaration %q", err, perTenant.Type)
	}
	if strings.Contains(err.Error(), platform.Type) {
		t.Errorf("Start error %q names the platform declaration %q, which needs no lister", err, platform.Type)
	}
	// A refused Start started nothing: no tick ever runs.
	time.Sleep(20 * time.Millisecond)
	if got := q.recorded(); len(got) != 0 {
		t.Fatalf("a refused Start still enqueued %d tasks", len(got))
	}
	s.Stop()

	// With a lister wired, the same declarations start and tick.
	lister := &recordingLister{tenants: []pkgcore.TenantID{"acme"}}
	ok := NewScheduler(q, WithSchedules(schedules), WithTenantLister(lister), WithInterval(5*time.Millisecond))
	if err := ok.Start(context.Background()); err != nil {
		t.Fatalf("Start with a lister wired = %v, want nil", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(q.recorded()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	ok.Stop()
	if got := q.recorded(); len(got) == 0 {
		t.Fatal("a started scheduler never enqueued a tick within the deadline")
	}
}

func TestScheduler_Lifecycle(t *testing.T) {
	schedules := newTestSchedules(t)
	q := &recordingQueue{}
	s := NewScheduler(q, WithSchedules(schedules), WithInterval(5*time.Millisecond))

	// Stop before Start is a no-op.
	s.Stop()

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("second Start = %v, want nil (Start is idempotent)", err)
	}
	s.Stop()
	s.Stop() // idempotent

	if err := s.Start(context.Background()); err == nil {
		t.Fatal("Start after Stop = nil, want a refusal: the lifecycle is one Start and one Stop")
	}
	var nilCtx context.Context
	if err := s.Start(nilCtx); err == nil {
		t.Fatal("Start with a nil context = nil, want a refusal")
	}
}

func TestScheduler_Stop_HaltsTicking(t *testing.T) {
	schedules := newTestSchedules(t, pkgcore.PeriodicTask{
		Type:           "pki.expiry_scan",
		Every:          time.Hour,
		Scope:          pkgcore.PeriodicScopePlatform,
		KeyPrefix:      "pki.expiry_scan:",
		PlatformTenant: "_pki_platform_scan",
	})
	q := &recordingQueue{}
	s := NewScheduler(q, WithSchedules(schedules), WithInterval(5*time.Millisecond))

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start = %v, want nil", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(q.recorded()) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := q.recorded(); len(got) == 0 {
		t.Fatal("the scheduler never ticked within the deadline")
	}

	s.Stop()
	settled := len(q.recorded())
	time.Sleep(20 * time.Millisecond)
	if got := q.recorded(); len(got) != settled {
		t.Fatalf("enqueues kept arriving after Stop: %d -> %d", settled, len(got))
	}
}

func TestNewScheduler_DefaultsAndOptionValidation(t *testing.T) {
	s := NewScheduler(&recordingQueue{})
	if s.interval != DefaultScheduleInterval {
		t.Errorf("default interval = %s, want DefaultScheduleInterval %s", s.interval, DefaultScheduleInterval)
	}
	if s.now == nil {
		t.Error("default clock is nil")
	}

	tests := []struct {
		name  string
		build func()
	}{
		{"nil queue", func() { NewScheduler(nil) }},
		{"nil schedules registrar", func() { NewScheduler(&recordingQueue{}, WithSchedules(nil)) }},
		{"nil tenant lister", func() { NewScheduler(&recordingQueue{}, WithTenantLister(nil)) }},
		{"zero interval", func() { NewScheduler(&recordingQueue{}, WithInterval(0)) }},
		{"negative interval", func() { NewScheduler(&recordingQueue{}, WithInterval(-time.Second)) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("NewScheduler with %s did not panic", tt.name)
				}
			}()
			tt.build()
		})
	}
}
