package aigateway

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestModule_Name(t *testing.T) {
	m := NewModule(newTestDB(t))
	if m.Name() != "ai-gateway" {
		t.Fatalf("Name() = %q, want %q", m.Name(), "ai-gateway")
	}
}

func TestModule_DependsOn_Nil(t *testing.T) {
	m := NewModule(newTestDB(t))
	if deps := m.DependsOn(); deps != nil {
		t.Fatalf("DependsOn() = %v, want nil", deps)
	}
}

func TestModule_Register_DeclaresSystemPurpose(t *testing.T) {
	m := NewModule(newTestDB(t))
	bus := pkgcore.NewMemoryEventBus()
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Register must have declared SystemPurposeCredentialWrite -- proven by
	// WithSystemContext now accepting it where it would otherwise refuse an
	// unregistered purpose.
	if _, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
		Actor:   "test",
		Purpose: SystemPurposeCredentialWrite,
	}); err != nil {
		t.Fatalf("WithSystemContext after Register: %v, want SystemPurposeCredentialWrite to be registered", err)
	}
}

func TestModule_GatewayAndCredentials_EndToEnd(t *testing.T) {
	db := newTestDB(t)
	provider := &fakeChatProvider{chatResp: ChatResponse{Message: ChatMessage{Role: RoleAssistant, Content: "hi there"}}}
	m := NewModule(db,
		WithModelRoute("chat:default", fakeProviderName, "vendor-model-x"),
		WithChatProviderRegistry(newFakeGatewayRegistry(t, provider)),
	)

	sysCtx, err := pkgcore.WithSystemContext(context.Background(), systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if err = m.Credentials().SetPlatformCredential(sysCtx, fakeProviderName, "sk-test", ""); err != nil {
		t.Fatalf("SetPlatformCredential: %v", err)
	}

	resp, err := m.Gateway().Chat(context.Background(), ChatRequest{
		Model:    "chat:default",
		Messages: []ChatMessage{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Message.Content != "hi there" {
		t.Fatalf("Chat response = %+v", resp)
	}
}
