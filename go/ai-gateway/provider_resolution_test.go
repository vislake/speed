package aigateway

// This suite pins the provider resolution every Gateway call runs: a route's
// provider name resolves as a selected member of the "chat"/"image"
// directories through the component registry the Module attached, and
// nothing else ever answers a request. A name the selection does not carry
// -- or a Gateway with no registry attached at all -- refuses at resolution,
// before the provider call, with the fixture's own provider never invoked.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// faceProbeChatName and faceProbeImageName are the component names the
// refusal tests route to: deliberately outside the fixtures' selection, so
// a request routed to one names a provider the registry cannot resolve.
const (
	faceProbeChatName  = "chat.resolution-probe"
	faceProbeImageName = "image.resolution-probe"
)

// selectOnly assembles a component registry whose composition selects
// exactly the components handed in with no configuration block, returning
// the constructed registry.
func selectOnly(t *testing.T, ctx context.Context, components ...pkgcore.Component) *pkgcore.ComponentRegistry {
	t.Helper()
	return selectBlocks(t, ctx, components, nil)
}

// selectBlocks is selectOnly for the tests that select a real descriptor
// whose New decodes its configuration block: a component named in blocks is
// selected with that block, every other component with none. A component
// already present in the registry's seed set (a globally self-registered
// descriptor) is selected without being registered again.
func selectBlocks(t *testing.T, ctx context.Context, components []pkgcore.Component, blocks map[string]map[string]any) *pkgcore.ComponentRegistry {
	t.Helper()
	reg := pkgcore.NewComponentRegistry()
	known := make(map[string]bool)
	for _, c := range pkgcore.RegisteredComponents(reg) {
		known[c.Name] = true
	}
	selected := make(map[string]any, len(components))
	for _, c := range components {
		if !known[c.Name] {
			if err := reg.Register(c); err != nil {
				t.Fatalf("register component %q: %v", c.Name, err)
			}
		}
		selected[c.Name] = blocks[c.Name]
	}
	reg.Put(pkgcore.NewComponentConfig(map[string]any{"components": selected}))
	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v, want the selected component resolved", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	return reg
}

// selectRealChat selects the module's real "chat.openai-compatible"
// descriptor with a placeholder block: the tests exercising the real
// construction path (its coded refusals, the guarded client) feed the
// credential through the per-call override, so the assembly-time block only
// has to satisfy the descriptor's schema.
func selectRealChat(t *testing.T) *pkgcore.ComponentRegistry {
	t.Helper()
	return selectBlocks(t, context.Background(), []pkgcore.Component{chatProviderComponent},
		map[string]map[string]any{ProviderOpenAICompatible: {"base_url": "https://placeholder.example.test/v1", "api_key": "sk-placeholder"}})
}

// selectRealImage is selectRealChat's image-family counterpart.
func selectRealImage(t *testing.T) *pkgcore.ComponentRegistry {
	t.Helper()
	return selectBlocks(t, context.Background(), []pkgcore.Component{imageProviderComponent},
		map[string]map[string]any{ProviderOpenAICompatibleImage: {"base_url": "https://placeholder.example.test/v1", "api_key": "sk-placeholder"}})
}

// TestGateway_Chat_UnselectedName_Refused pins the single resolution source
// from the refusal side: a route naming a provider the attached selection
// does not carry fails with ErrUnknownComponent before any provider call --
// there is no package-level fallback that could serve it.
func TestGateway_Chat_UnselectedName_Refused(t *testing.T) {
	ctx := context.Background()
	provider := &fakeChatProvider{chatResp: ChatResponse{Message: ChatMessage{Role: RoleAssistant, Content: "unused"}}}

	g := gatewayTestFixture(t, provider)
	g.routes["chat:default"] = ModelRoute{Provider: faceProbeChatName, VendorModel: "vendor-model-x"}
	sysCtx, err := pkgcore.WithSystemContext(ctx, systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if credErr := g.credentials.SetPlatformCredential(sysCtx, faceProbeChatName, "sk-probe", ""); credErr != nil {
		t.Fatalf("SetPlatformCredential: %v", credErr)
	}

	_, err = g.Chat(ctx, chatReq())
	if !errors.Is(err, pkgcore.ErrUnknownComponent) {
		t.Fatalf("Chat err = %v, want it to wrap pkgcore.ErrUnknownComponent", err)
	}
	if provider.chatCalls != 0 {
		t.Errorf("the fixture provider served %d calls for an unselected name, want 0: resolution must refuse, never fall back", provider.chatCalls)
	}
}

// TestGateway_Chat_NoRegistryAttached_Refused pins the wiring contract from
// the other side: a Gateway constructed directly, with no component
// registry attached, refuses every provider resolution -- naming the
// missing attachment -- rather than serving the request.
func TestGateway_Chat_NoRegistryAttached_Refused(t *testing.T) {
	credentials := NewCredentialService(newTestDB(t))
	sysCtx, err := pkgcore.WithSystemContext(context.Background(), systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if setErr := credentials.SetPlatformCredential(sysCtx, fakeProviderName, "sk-test", ""); setErr != nil {
		t.Fatalf("SetPlatformCredential: %v", setErr)
	}
	g := NewGateway(credentials, WithModelRoute("chat:default", fakeProviderName, "vendor-model-x"))

	_, err = g.Chat(context.Background(), chatReq())
	if err == nil || !strings.Contains(err.Error(), "no component registry attached") {
		t.Fatalf("Chat err = %v, want a refusal naming the missing component registry", err)
	}
}

// TestGateway_Image_UnselectedName_Refused is the image-family counterpart,
// observed at the resolution seam GenerateImage and the job handler share.
func TestGateway_Image_UnselectedName_Refused(t *testing.T) {
	ctx := context.Background()
	g, _, _ := imageGatewayTestFixture(t, &fakeImageProvider{})
	g.routes["image:default"] = ModelRoute{Provider: faceProbeImageName, VendorModel: "vendor-image-model"}
	sysCtx, err := pkgcore.WithSystemContext(ctx, systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if credErr := g.credentials.SetPlatformCredential(sysCtx, faceProbeImageName, "sk-probe", ""); credErr != nil {
		t.Fatalf("SetPlatformCredential: %v", credErr)
	}

	_, _, err = g.resolveImage(ctx, "image:default")
	if !errors.Is(err, pkgcore.ErrUnknownComponent) {
		t.Fatalf("resolveImage() error = %v, want it to wrap pkgcore.ErrUnknownComponent", err)
	}
}
