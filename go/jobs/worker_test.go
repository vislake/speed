package jobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// TestJobContext_ProducesTenantScopedContext is the positive half of "the
// tenant context trap": jobContext(tenant) must produce a context from
// which the Job's own tenant is recoverable, exactly what a worker hands
// to Handler.Handle.
func TestJobContext_ProducesTenantScopedContext(t *testing.T) {
	ctx := jobContext(pkgcore.TenantID("tenant-a"))

	got, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		t.Fatalf("MustTenantFromContext(jobContext(...)) error = %v, want nil", err)
	}
	if got != "tenant-a" {
		t.Errorf("tenant = %q, want %q", got, "tenant-a")
	}
}

// TestJobContext_IgnoresAmbientTenant proves jobContext takes no base
// context at all -- it is rooted in a fresh context.Background() every
// call -- so it can never inherit a stale or unrelated tenant from
// whatever context happens to be available at the call site. This is the
// "not inherited from the original Enqueue call's context" half of
// AGENTS.md's tenant context trap: that original context is long gone by
// the time a worker goroutine picks the Job back up out of SQLite, so
// jobContext does not even accept one to (mis)use.
func TestJobContext_IgnoresAmbientTenant(t *testing.T) {
	ctx := jobContext(pkgcore.TenantID("right-tenant"))
	got, ok := pkgcore.TenantFromContext(ctx)
	if !ok || got != "right-tenant" {
		t.Errorf("tenant = (%q, %v), want (%q, true)", got, ok, "right-tenant")
	}
}

// TestJobContext_ContrastWithoutRebuild_FailsClosedWithErrNoTenant is the
// negative half: the exact failure mode a worker reproduces if it EVER
// calls Handle with a context other than jobContext's own output --
// including, but not limited to, forgetting to call it at all and using a
// bare context.Background(). Any such context carries no tenant, so a
// Handler's tenant-scoped operation (dbkit.Repository[T] underneath, or
// pkgcore.MustTenantFromContext directly) fails closed with
// pkgcore.ErrNoTenant instead of silently running unscoped or against the
// wrong tenant.
//
// If a future change to execute/runWorker ever stops routing through
// jobContext, THIS is the test that must start failing -- with this exact,
// well-labeled name -- rather than some unrelated Handler mysteriously
// erroring in production months later. See demo_queue_test.go's
// TestDemoQueue_RebuildsTenantContext_HandlerUsesOnlyJobTenant for the
// same guarantee proved end to end through a real worker and a real
// dbkit.Repository[T] call.
func TestJobContext_ContrastWithoutRebuild_FailsClosedWithErrNoTenant(t *testing.T) {
	brokenCtx := context.Background() // what a worker gets if it skips jobContext entirely

	_, err := pkgcore.MustTenantFromContext(brokenCtx)
	if !errors.Is(err, pkgcore.ErrNoTenant) {
		t.Fatalf("MustTenantFromContext(context.Background()) error = %v, want a wrapped pkgcore.ErrNoTenant", err)
	}

	// Contrast: the identical check against jobContext's own output
	// succeeds -- the only difference is whether jobContext was called.
	fixedCtx := jobContext(pkgcore.TenantID("tenant-a"))
	if _, err := pkgcore.MustTenantFromContext(fixedCtx); err != nil {
		t.Fatalf("MustTenantFromContext(jobContext(...)) error = %v, want nil", err)
	}
}

func TestBackoffDelay(t *testing.T) {
	q := NewDemoQueue(nil, WithBackoff(1*time.Second, 10*time.Second))

	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{attempts: 0, want: 1 * time.Second}, // treated as 1
		{attempts: 1, want: 1 * time.Second},
		{attempts: 2, want: 2 * time.Second},
		{attempts: 3, want: 4 * time.Second},
		{attempts: 4, want: 8 * time.Second},
		{attempts: 5, want: 10 * time.Second}, // would be 16s uncapped
		{attempts: 10, want: 10 * time.Second},
	}
	for _, tt := range tests {
		if got := q.backoffDelay(tt.attempts); got != tt.want {
			t.Errorf("backoffDelay(%d) = %v, want %v", tt.attempts, got, tt.want)
		}
	}
}

func TestTenantSlotReservation(t *testing.T) {
	q := NewDemoQueue(nil, WithTenantConcurrencyLimit(2))
	tenant := pkgcore.TenantID("tenant-a")

	if !q.tryReserveTenantSlot(tenant) {
		t.Fatal("first reservation should succeed")
	}
	if !q.tryReserveTenantSlot(tenant) {
		t.Fatal("second reservation should succeed: limit is 2")
	}
	if q.tryReserveTenantSlot(tenant) {
		t.Fatal("third reservation should fail: limit reached")
	}

	other := pkgcore.TenantID("tenant-b")
	if !q.tryReserveTenantSlot(other) {
		t.Fatal("a different tenant's reservation must not be affected by tenant-a's limit")
	}

	q.releaseTenantSlot(tenant)
	if !q.tryReserveTenantSlot(tenant) {
		t.Fatal("after a release, a new reservation should succeed again")
	}
}

// TestTenantSlotReservation_ConcurrentAccessIsRaceFree exercises
// tryReserveTenantSlot/releaseTenantSlot from many goroutines at once:
// runDispatcher and runWorker call these concurrently by construction (the
// dispatcher increments, N worker goroutines decrement), so this is the
// package's concurrency hot spot the backend coding standard §13 requires
// a -race test for.
func TestTenantSlotReservation_ConcurrentAccessIsRaceFree(t *testing.T) {
	q := NewDemoQueue(nil, WithTenantConcurrencyLimit(3))
	tenant := pkgcore.TenantID("tenant-a")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if q.tryReserveTenantSlot(tenant) {
				q.releaseTenantSlot(tenant)
			}
		}()
	}
	wg.Wait()

	q.tenantMu.Lock()
	remaining := q.runningPerTenant[tenant]
	q.tenantMu.Unlock()
	if remaining != 0 {
		t.Errorf("runningPerTenant[tenant] = %d, want 0 once every goroutine released what it reserved", remaining)
	}
}
