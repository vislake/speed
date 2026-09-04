package aigateway

import (
	"context"
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// fakeChatProvider is a ChatProvider test double that records every call it
// receives and answers with fixed, injectable results.
type fakeChatProvider struct {
	chatCalls   int
	streamCalls int
	lastReq     ChatRequest

	chatResp  ChatResponse
	chatErr   error
	streamOut []ChatChunk
	streamErr error
}

func (f *fakeChatProvider) Chat(_ context.Context, req ChatRequest) (ChatResponse, error) {
	f.chatCalls++
	f.lastReq = req
	return f.chatResp, f.chatErr
}

func (f *fakeChatProvider) ChatStream(_ context.Context, req ChatRequest) (<-chan ChatChunk, error) {
	f.streamCalls++
	f.lastReq = req
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	ch := make(chan ChatChunk, len(f.streamOut))
	for _, c := range f.streamOut {
		ch <- c
	}
	close(ch)
	return ch, nil
}

var _ ChatProvider = (*fakeChatProvider)(nil)

const fakeProviderName = "chat.fake-test-provider"

// newFakeGatewayRegistry returns a *pkgcore.SeamRegistry[ChatProvider]
// containing only provider, registered under fakeProviderName -- isolating
// a test from the process-global ChatProviderRegistry's real registrations.
func newFakeGatewayRegistry(t *testing.T, provider ChatProvider) *pkgcore.SeamRegistry[ChatProvider] {
	t.Helper()
	reg := pkgcore.NewSeamRegistry[ChatProvider]()
	if err := reg.Register(pkgcore.Registration[ChatProvider]{
		Name:         fakeProviderName,
		Capabilities: pkgcore.Stateless,
		New:          func(pkgcore.Config) (ChatProvider, error) { return provider, nil },
	}); err != nil {
		t.Fatalf("register fake provider: %v", err)
	}
	return reg
}

// gatewayTestFixture builds a Gateway wired to a fake provider (routed
// under "chat:default"), a real CredentialService over a fresh test
// database, and a stored platform credential -- the common setup every
// pipeline test below starts from.
func gatewayTestFixture(t *testing.T, provider *fakeChatProvider, opts ...GatewayOption) *Gateway {
	t.Helper()
	credentials := NewCredentialService(newTestDB(t))
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if err := credentials.SetPlatformCredential(sysCtx, fakeProviderName, "sk-test", ""); err != nil {
		t.Fatalf("SetPlatformCredential: %v", err)
	}

	allOpts := append([]GatewayOption{
		WithModelRoute("chat:default", fakeProviderName, "vendor-model-x"),
		WithChatProviderRegistry(newFakeGatewayRegistry(t, provider)),
	}, opts...)
	return NewGateway(credentials, allOpts...)
}

func chatReq() ChatRequest {
	return ChatRequest{Model: "chat:default", Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}}}
}

// --- routing ----------------------------------------------------------------

func TestGateway_Chat_UnroutedModel_Refused(t *testing.T) {
	provider := &fakeChatProvider{}
	g := gatewayTestFixture(t, provider)
	_, err := g.Chat(context.Background(), ChatRequest{Model: "chat:unrouted", Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}}})
	if got, ok := apperrCode(err); !ok || got != ErrUnroutedModel.Code {
		t.Fatalf("Chat err = %v, want ErrUnroutedModel", err)
	}
	if provider.chatCalls != 0 {
		t.Fatalf("provider was called %d times for an unrouted model, want 0", provider.chatCalls)
	}
}

func TestGateway_Chat_RewritesLogicalModelToVendorModel(t *testing.T) {
	provider := &fakeChatProvider{chatResp: ChatResponse{Message: ChatMessage{Role: RoleAssistant, Content: "ok"}}}
	g := gatewayTestFixture(t, provider)
	if _, err := g.Chat(context.Background(), chatReq()); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if provider.lastReq.Model != "vendor-model-x" {
		t.Fatalf("provider saw Model = %q, want the resolved vendor model %q", provider.lastReq.Model, "vendor-model-x")
	}
}

// --- entitlements -------------------------------------------------------

func TestGateway_Chat_EntitlementDenied_NeverCallsProviderOrResolvesCredential(t *testing.T) {
	provider := &fakeChatProvider{}
	denied := EntitlementsFunc(func(context.Context, string, int64) (Decision, error) {
		return Decision{Allowed: false, Reason: "no_subscription"}, nil
	})
	g := gatewayTestFixture(t, provider, WithEntitlements(denied))

	_, err := g.Chat(context.Background(), chatReq())
	// The error identity itself proves the ordering: if credential
	// resolution had run first (or at all), an unrelated failure shape
	// would surface instead -- ErrEntitlementDenied specifically means the
	// gate fired before anything else was attempted.
	if got, ok := apperrCode(err); !ok || got != ErrEntitlementDenied.Code {
		t.Fatalf("Chat err = %v, want ErrEntitlementDenied", err)
	}
	if provider.chatCalls != 0 {
		t.Fatalf("provider was called %d times for a denied entitlement, want 0 -- a refused caller must never be billed", provider.chatCalls)
	}
}

func TestGateway_Chat_EntitlementAllowed_ProceedsToProvider(t *testing.T) {
	provider := &fakeChatProvider{chatResp: ChatResponse{Message: ChatMessage{Role: RoleAssistant, Content: "ok"}}}
	allowed := EntitlementsFunc(func(context.Context, string, int64) (Decision, error) {
		return Decision{Allowed: true}, nil
	})
	g := gatewayTestFixture(t, provider, WithEntitlements(allowed))

	if _, err := g.Chat(context.Background(), chatReq()); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if provider.chatCalls != 1 {
		t.Fatalf("provider was called %d times, want 1", provider.chatCalls)
	}
}

func TestGateway_Chat_EntitlementCheckError_Propagates(t *testing.T) {
	provider := &fakeChatProvider{}
	wantErr := errors.New("entitlements backend down")
	failing := EntitlementsFunc(func(context.Context, string, int64) (Decision, error) { return Decision{}, wantErr })
	g := gatewayTestFixture(t, provider, WithEntitlements(failing))

	_, err := g.Chat(context.Background(), chatReq())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Chat err = %v, want %v", err, wantErr)
	}
	if provider.chatCalls != 0 {
		t.Fatalf("provider was called %d times, want 0", provider.chatCalls)
	}
}

func TestGateway_Chat_NoEntitlementsWired_ProceedsUnconditionally(t *testing.T) {
	provider := &fakeChatProvider{chatResp: ChatResponse{Message: ChatMessage{Role: RoleAssistant, Content: "ok"}}}
	g := gatewayTestFixture(t, provider) // no WithEntitlements
	if _, err := g.Chat(context.Background(), chatReq()); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if provider.chatCalls != 1 {
		t.Fatalf("provider was called %d times, want 1", provider.chatCalls)
	}
}

// --- usage recording ------------------------------------------------------

// fakeUsageRecorder is a UsageRecorder test double.
type fakeUsageRecorder struct {
	events []UsageEvent
}

func (f *fakeUsageRecorder) Record(_ context.Context, event UsageEvent) error {
	f.events = append(f.events, event)
	return nil
}

func TestGateway_Chat_ReportsUsageAfterSuccess(t *testing.T) {
	provider := &fakeChatProvider{chatResp: ChatResponse{
		Message: ChatMessage{Role: RoleAssistant, Content: "ok"},
		Usage:   Usage{PromptTokens: 3, CompletionTokens: 4, TotalTokens: 7},
	}}
	recorder := &fakeUsageRecorder{}
	g := gatewayTestFixture(t, provider, WithUsageRecorder(recorder))

	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := g.Chat(tenantCtx, chatReq()); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(recorder.events) != 1 {
		t.Fatalf("got %d usage events, want 1", len(recorder.events))
	}
	got := recorder.events[0]
	if got.TenantID != "tenant-acme" || got.Feature != usageFeatureChatTokens || got.Quantity != 7 {
		t.Fatalf("usage event = %+v, want tenant-acme / ai.chat_tokens / 7", got)
	}
	if got.Metadata["model"] != "chat:default" {
		t.Fatalf("usage event metadata = %+v, want model=chat:default", got.Metadata)
	}
}

func TestGateway_Chat_NoTenantInContext_ReportsNoUsage(t *testing.T) {
	provider := &fakeChatProvider{chatResp: ChatResponse{
		Message: ChatMessage{Role: RoleAssistant, Content: "ok"},
		Usage:   Usage{TotalTokens: 7},
	}}
	recorder := &fakeUsageRecorder{}
	g := gatewayTestFixture(t, provider, WithUsageRecorder(recorder))

	if _, err := g.Chat(context.Background(), chatReq()); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(recorder.events) != 0 {
		t.Fatalf("got %d usage events with no tenant in context, want 0", len(recorder.events))
	}
}

func TestGateway_Chat_ProviderError_NeverReportsUsage(t *testing.T) {
	provider := &fakeChatProvider{chatErr: errors.New("vendor is down")}
	recorder := &fakeUsageRecorder{}
	g := gatewayTestFixture(t, provider, WithUsageRecorder(recorder))

	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	if _, err := g.Chat(tenantCtx, chatReq()); err == nil {
		t.Fatal("Chat succeeded, want the provider's error")
	}
	if len(recorder.events) != 0 {
		t.Fatalf("got %d usage events after a failed call, want 0", len(recorder.events))
	}
}

// --- ChatStream: usage reported only on the terminal success chunk ---------

func TestGateway_ChatStream_ReportsUsageOnlyOnTerminalChunk(t *testing.T) {
	usage := Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	provider := &fakeChatProvider{streamOut: []ChatChunk{
		{Delta: "Hel"},
		{Delta: "lo"},
		{Usage: &usage},
	}}
	recorder := &fakeUsageRecorder{}
	g := gatewayTestFixture(t, provider, WithUsageRecorder(recorder))

	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	ch, err := g.ChatStream(tenantCtx, chatReq())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var got []ChatChunk
	for chunk := range ch {
		got = append(got, chunk)
		// The moment the terminal usage chunk is observed on the RELAYED
		// channel, the recorder must already have been called -- proving
		// usage is reported before/at forwarding, never deferred past the
		// point a consumer could have stopped reading.
		if chunk.Usage != nil && len(recorder.events) != 1 {
			t.Fatalf("recorder saw %d events by the time the terminal chunk was relayed, want 1", len(recorder.events))
		}
	}
	if len(got) != 3 {
		t.Fatalf("got %d relayed chunks, want 3", len(got))
	}
	if len(recorder.events) != 1 {
		t.Fatalf("got %d usage events total, want exactly 1", len(recorder.events))
	}
	if recorder.events[0].Quantity != 3 {
		t.Fatalf("usage event quantity = %v, want 3", recorder.events[0].Quantity)
	}
}

func TestGateway_ChatStream_ErrorChunk_NeverReportsUsage(t *testing.T) {
	provider := &fakeChatProvider{streamOut: []ChatChunk{
		{Delta: "Hel"},
		{Err: ErrProviderRequestFailed},
	}}
	recorder := &fakeUsageRecorder{}
	g := gatewayTestFixture(t, provider, WithUsageRecorder(recorder))

	tenantCtx := pkgcore.WithTenant(context.Background(), "tenant-acme")
	ch, err := g.ChatStream(tenantCtx, chatReq())
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	var got []ChatChunk
	for chunk := range ch {
		got = append(got, chunk)
	}
	if len(got) != 2 || got[1].Err == nil {
		t.Fatalf("got %+v, want a content chunk then a terminal error chunk", got)
	}
	if len(recorder.events) != 0 {
		t.Fatalf("got %d usage events for a stream that ended in error, want 0", len(recorder.events))
	}
}

func TestGateway_ChatStream_UnroutedModel_Refused(t *testing.T) {
	provider := &fakeChatProvider{}
	g := gatewayTestFixture(t, provider)
	_, err := g.ChatStream(context.Background(), ChatRequest{Model: "chat:unrouted", Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}}})
	if got, ok := apperrCode(err); !ok || got != ErrUnroutedModel.Code {
		t.Fatalf("ChatStream err = %v, want ErrUnroutedModel", err)
	}
	if provider.streamCalls != 0 {
		t.Fatalf("provider was called %d times for an unrouted model, want 0", provider.streamCalls)
	}
}
