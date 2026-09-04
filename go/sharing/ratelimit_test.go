package sharing

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/ratelimit"
)

// scriptedLimiter is a minimal, in-test ratelimit.Limiter double whose
// every Allow call answers the same fixed Decision, so a test can drive
// Service's rate-limit checks deterministically without waiting out
// createPerTenantWindow/accessPerIPWindow/accessPerTokenWindow's real
// durations.
type scriptedLimiter struct {
	allowed    bool
	resetAfter time.Duration
	err        error
}

func (s scriptedLimiter) Allow(context.Context, string, ratelimit.Limit) (ratelimit.Decision, error) {
	if s.err != nil {
		return ratelimit.Decision{}, s.err
	}
	return ratelimit.Decision{Allowed: s.allowed, ResetAfter: s.resetAfter}, nil
}

func TestService_RateLimiter_FallsBackToHostKVStore(t *testing.T) {
	svc, _ := newTestService(t, nil)
	limiter, err := svc.rateLimiter()
	if err != nil {
		t.Fatalf("rateLimiter: %v", err)
	}
	if limiter == nil {
		t.Fatalf("rateLimiter() = nil, want a real ratelimit.Limiter over the host's KVStore")
	}
}

func TestService_RateLimiter_PrefersInjectedOverride(t *testing.T) {
	svc, _ := newTestService(t, nil)
	want := scriptedLimiter{allowed: true}
	svc.limiter = want
	got, err := svc.rateLimiter()
	if err != nil {
		t.Fatalf("rateLimiter: %v", err)
	}
	if got != ratelimit.Limiter(want) {
		t.Errorf("rateLimiter() did not return the injected override")
	}
}

// TestService_RateLimiter_NoHostAttached_FailsClosed proves a Service
// built with NewService alone (never attached to a registry) cannot
// silently skip rate limiting -- it reports the wiring error, which
// checkCreateRateLimit/checkAccessRateLimit both wrap as ErrInternal, never
// "allow".
func TestService_RateLimiter_NoHostAttached_FailsClosed(t *testing.T) {
	svc := NewService(newTestDB(t), nil)
	if _, err := svc.rateLimiter(); !errors.Is(err, errShareNoHostRegistry) {
		t.Errorf("rateLimiter() error = %v, want errShareNoHostRegistry", err)
	}
	if _, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"}); !hasCode(err, ErrInternal.Code) {
		t.Errorf("Create() error = %v, want ErrInternal (rate limiter unavailable must fail closed, not silently allow)", err)
	}
}

func TestService_Create_RateLimited_RefusesWithErrRateLimited(t *testing.T) {
	svc, _ := newTestService(t, nil)
	svc.limiter = scriptedLimiter{allowed: false, resetAfter: 42 * time.Second}

	_, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"})
	assertCode(t, err, ErrRateLimited.Code)

	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error is not an *apperr.Error: %v", err)
	}
	if got := appErr.Params["dimension"]; got != "tenant" {
		t.Errorf("dimension param = %v, want %q", got, "tenant")
	}
	if got := appErr.Params["retry_after_seconds"]; got != 42 {
		t.Errorf("retry_after_seconds param = %v, want 42", got)
	}
}

func TestService_Create_NotRateLimited_Succeeds(t *testing.T) {
	svc, _ := newTestService(t, nil)
	svc.limiter = scriptedLimiter{allowed: true}

	if _, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestService_AccessPublic_RateLimited_RefusesBeforeResolvingTenant(t *testing.T) {
	svc, _ := newTestService(t, nil)
	created, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	svc.limiter = scriptedLimiter{allowed: false, resetAfter: 7 * time.Second}

	_, err = svc.AccessPublic(context.Background(), created.Token, AccessParams{IP: "203.0.113.1"})
	assertCode(t, err, ErrRateLimited.Code)
}

func TestService_RateLimiter_UnderlyingStoreError_WrapsAsInternal(t *testing.T) {
	svc, _ := newTestService(t, nil)
	svc.limiter = scriptedLimiter{err: errors.New("kv store unavailable")}

	_, err := svc.Create(testCtx(), CreateParams{ResourceRef: "r"})
	assertCode(t, err, ErrInternal.Code)
}
