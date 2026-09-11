package aigateway

import (
	"context"
	"slices"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
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

func TestModule_Register_DeclaresTheSurface(t *testing.T) {
	m := NewModule(newTestDB(t))
	bus := pkgcore.NewMemoryEventBus()
	reg := componenttest.NewRegistry()
	reg.Put(bus)
	if err := componenttest.DeclareInto(reg, m); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, want := range []string{PermissionRead, PermissionWrite, PermissionManagePlatform} {
		if !slices.Contains(reg.Permissions.Permissions(), want) {
			t.Errorf("Permissions = %v, want the %q declaration", reg.Permissions.Permissions(), want)
		}
	}
	if routes := reg.Routes.Routes(); len(routes) != 1 || routes[0].Path != apiPath {
		t.Fatalf("Register mounted %v, want exactly the %s mount", routes, apiPath)
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

// newTestRegistry returns a fresh *pkgcore.ComponentRegistry over in-memory
// infrastructure -- enough for Register to run against without a real
// event bus, KV store or mailer.
func newTestRegistry() *pkgcore.ComponentRegistry {
	return componenttest.NewRegistry()
}

func TestModule_Register_ImageJobHandler_RegisteredOnlyWhenWired(t *testing.T) {
	db := newTestDB(t)
	moduleWithImages := NewModule(db, WithImageGeneration(&recordingImageQueue{}, newTestStorageObjectService(t)))
	reg := newTestRegistry()
	if err := componenttest.DeclareInto(reg, moduleWithImages); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := reg.Jobs.Handlers()[TaskTypeImageGenerate]; !ok {
		t.Fatalf("Register with WithImageGeneration did not claim %q on reg.Jobs", TaskTypeImageGenerate)
	}

	chatOnlyDB := newTestDB(t)
	chatOnlyModule := NewModule(chatOnlyDB)
	chatOnlyReg := newTestRegistry()
	if err := componenttest.DeclareInto(chatOnlyReg, chatOnlyModule); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, ok := chatOnlyReg.Jobs.Handlers()[TaskTypeImageGenerate]; ok {
		t.Fatal("a chat-only Module (no WithImageGeneration) must not claim an image job handler")
	}
}
