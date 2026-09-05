package aigateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/ratelimit"
)

// scriptedLimiter is a minimal, in-test ratelimit.Limiter double whose every
// Allow call answers the same fixed Decision, mirroring go/sharing's
// identically-named test double (sharing/ratelimit_test.go) so a test can
// drive Gateway's rate-limit checks deterministically without waiting out
// perTenantWindow's real duration.
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

// TestGateway_RateLimiter_NoHostAttached_IsANoOp proves a Gateway built with
// NewGateway alone (never attached to a registry by Module.Register --
// gatewayTestFixture's own construction) enforces no rate limit at all,
// unlike go/sharing's Service (whose rateLimiter fails closed with no host)
// -- see rateLimiter's own doc comment for why the two modules differ here.
func TestGateway_RateLimiter_NoHostAttached_IsANoOp(t *testing.T) {
	g := gatewayTestFixture(t, &fakeChatProvider{})
	if _, ok := g.rateLimiter(); ok {
		t.Error("rateLimiter() ok = true with no host attached, want false")
	}
}

// TestGateway_RateLimiter_PrefersInjectedOverride mirrors go/sharing's
// TestService_RateLimiter_PrefersInjectedOverride: the test-injected
// g.limiter wins even when host is also set.
func TestGateway_RateLimiter_PrefersInjectedOverride(t *testing.T) {
	g := gatewayTestFixture(t, &fakeChatProvider{})
	want := scriptedLimiter{allowed: true}
	g.limiter = want
	got, ok := g.rateLimiter()
	if !ok {
		t.Fatal("rateLimiter() ok = false, want true")
	}
	if got != ratelimit.Limiter(want) {
		t.Error("rateLimiter() did not return the injected override")
	}
}

// TestGateway_Chat_RateLimited_RefusesWithErrRateLimited proves a denied
// Allow decision refuses Chat with ErrRateLimited BEFORE checkEntitlement
// or the provider is ever reached.
func TestGateway_Chat_RateLimited_RefusesWithErrRateLimited(t *testing.T) {
	provider := &fakeChatProvider{chatResp: ChatResponse{Message: ChatMessage{Role: RoleAssistant, Content: "ok"}}}
	g := gatewayTestFixture(t, provider)
	g.limiter = scriptedLimiter{allowed: false, resetAfter: 42 * time.Second}

	_, err := g.Chat(context.Background(), chatReq())
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != ErrRateLimited.Code {
		t.Fatalf("Chat() error = %v, want ErrRateLimited", err)
	}
	if got := appErr.Params["retry_after_seconds"]; got != 42 {
		t.Errorf("retry_after_seconds param = %v, want 42", got)
	}
	if provider.chatCalls != 0 {
		t.Errorf("provider was called %d times while rate-limited, want 0", provider.chatCalls)
	}
}

// TestGateway_Chat_NotRateLimited_Succeeds proves an allowed Allow decision
// lets Chat proceed through the rest of the pipeline unchanged.
func TestGateway_Chat_NotRateLimited_Succeeds(t *testing.T) {
	provider := &fakeChatProvider{chatResp: ChatResponse{Message: ChatMessage{Role: RoleAssistant, Content: "ok"}}}
	g := gatewayTestFixture(t, provider)
	g.limiter = scriptedLimiter{allowed: true}

	if _, err := g.Chat(context.Background(), chatReq()); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if provider.chatCalls != 1 {
		t.Errorf("provider was called %d times, want 1", provider.chatCalls)
	}
}

// TestGateway_ChatStream_RateLimited_RefusesBeforeCallingTheProvider proves
// ChatStream's own rate-limit check runs before it ever calls the
// provider's ChatStream method.
func TestGateway_ChatStream_RateLimited_RefusesBeforeCallingTheProvider(t *testing.T) {
	provider := &fakeChatProvider{}
	g := gatewayTestFixture(t, provider)
	g.limiter = scriptedLimiter{allowed: false, resetAfter: 7 * time.Second}

	_, err := g.ChatStream(context.Background(), chatReq())
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != ErrRateLimited.Code {
		t.Fatalf("ChatStream() error = %v, want ErrRateLimited", err)
	}
	if provider.streamCalls != 0 {
		t.Errorf("provider was called %d times while rate-limited, want 0", provider.streamCalls)
	}
}

// TestGateway_GenerateImage_RateLimited_RefusesBeforeEnqueuing proves
// GenerateImage's own rate-limit check runs before it enqueues anything --
// the identical "refused caller never consumes downstream capacity"
// property TestGateway_GenerateImage_EntitlementDenied_NeverEnqueues already
// pins for the entitlement check.
func TestGateway_GenerateImage_RateLimited_RefusesBeforeEnqueuing(t *testing.T) {
	provider := &fakeImageProvider{}
	g, queue, _ := imageGatewayTestFixture(t, provider)
	g.limiter = scriptedLimiter{allowed: false, resetAfter: 5 * time.Second}

	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	_, err := g.GenerateImage(tenantCtx, imageReq())
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != ErrRateLimited.Code {
		t.Fatalf("GenerateImage() error = %v, want ErrRateLimited", err)
	}
	if queue.calls != 0 {
		t.Errorf("queue was called %d times while rate-limited, want 0", queue.calls)
	}
}

// TestGateway_RateLimiter_UnderlyingStoreError_WrapsAsInternal proves a
// Limiter failure (a KVStore outage) fails Chat closed with
// ErrRateLimitCheckFailed rather than silently allowing the call through,
// mirroring go/sharing's identical
// TestService_RateLimiter_UnderlyingStoreError_WrapsAsInternal.
func TestGateway_RateLimiter_UnderlyingStoreError_WrapsAsInternal(t *testing.T) {
	provider := &fakeChatProvider{chatResp: ChatResponse{Message: ChatMessage{Role: RoleAssistant, Content: "ok"}}}
	g := gatewayTestFixture(t, provider)
	g.limiter = scriptedLimiter{err: errors.New("kv store unavailable")}

	_, err := g.Chat(context.Background(), chatReq())
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != ErrRateLimitCheckFailed.Code {
		t.Fatalf("Chat() error = %v, want ErrRateLimitCheckFailed", err)
	}
	if provider.chatCalls != 0 {
		t.Errorf("provider was called %d times on a rate-limiter store error, want 0", provider.chatCalls)
	}
}

// TestModule_Register_AttachesGatewayHost proves Module.Register wires the
// registry onto Gateway.host, so a real Bootstrap gives Chat/ChatStream/
// GenerateImage a working per-tenant rate limiter with no extra option a
// host could forget -- mirroring go/sharing's own Module.Register wiring
// proof for its identical hostSeams field.
func TestModule_Register_AttachesGatewayHost(t *testing.T) {
	m := NewModule(newTestDB(t))
	if _, err := pkgcore.NewKernel().Bootstrap(context.Background(), m); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if m.Gateway().host == nil {
		t.Error("Gateway.host is nil after Bootstrap, want the registry attached")
	}
	if _, ok := m.Gateway().rateLimiter(); !ok {
		t.Error("rateLimiter() ok = false after Bootstrap, want a real Limiter over the resolved KVStore")
	}
}
