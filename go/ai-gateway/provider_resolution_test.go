package aigateway

// This suite pins the provider-resolution switch: a Gateway attached to an
// assembly resolves a route's provider as a selected member of the
// "chat"/"image" directories through the component face (pkgcore.Build with
// the credential as the override), while a Gateway with no such attachment
// -- or a name the assembly's selection does not carry -- keeps resolving
// through the package-level seam registries. The two faces are two
// construction paths to the same implementations under the same names
// (provider_components.go), which is what makes the switch behavior-
// preserving for a composition that selects nothing.

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// faceProbeChatName and faceProbeImageName are the component names these
// tests select: deliberately outside both seam registries' name spaces, so
// a resolution that fell back to a registry could never serve a request
// routed to one.
const (
	faceProbeChatName  = "chat.resolution-probe"
	faceProbeImageName = "image.resolution-probe"
)

// selectOnly assembles a component registry whose composition selects
// exactly the components handed in, returning the constructed registry.
func selectOnly(t *testing.T, ctx context.Context, components ...pkgcore.Component) *pkgcore.ComponentRegistry {
	t.Helper()
	reg := pkgcore.NewComponentRegistry()
	selected := make(map[string]any, len(components))
	for _, c := range components {
		if err := reg.Register(c); err != nil {
			t.Fatalf("register component %q: %v", c.Name, err)
		}
		selected[c.Name] = nil
	}
	reg.Put(pkgcore.NewComponentConfig(map[string]any{"components": selected}))
	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v, want the probe component selected", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	return reg
}

// TestGateway_Chat_SelectedComponentServesTheRoute pins the component face:
// the route's provider name is a selected "chat" member, so the request is
// served by the component's own instance -- never by the seam registry's
// provider the fixture otherwise wires.
func TestGateway_Chat_SelectedComponentServesTheRoute(t *testing.T) {
	ctx := context.Background()
	probe := &fakeChatProvider{chatResp: ChatResponse{Message: ChatMessage{Role: RoleAssistant, Content: "component face"}}}
	seamProvider := &fakeChatProvider{chatResp: ChatResponse{Message: ChatMessage{Role: RoleAssistant, Content: "seam face"}}}

	g := gatewayTestFixture(t, seamProvider)
	g.routes["chat:default"] = ModelRoute{Provider: faceProbeChatName, VendorModel: "vendor-model-x"}
	sysCtx, err := pkgcore.WithSystemContext(ctx, systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if err := g.credentials.SetPlatformCredential(sysCtx, faceProbeChatName, "sk-probe", ""); err != nil {
		t.Fatalf("SetPlatformCredential: %v", err)
	}

	g.components = selectOnly(t, ctx, pkgcore.Component{
		Name:     faceProbeChatName,
		Module:   "chat",
		Provides: []any{(*ChatProvider)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return probe, nil
		},
	})

	resp, err := g.Chat(ctx, chatReq())
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if probe.chatCalls != 1 {
		t.Errorf("the selected component served %d calls, want 1: a route naming a selected member must resolve through the component face", probe.chatCalls)
	}
	if seamProvider.chatCalls != 0 {
		t.Errorf("the seam-registry provider served %d calls, want 0", seamProvider.chatCalls)
	}
	if resp.Message.Content != "component face" {
		t.Errorf("Chat() content = %q, want the selected component's reply", resp.Message.Content)
	}
}

// TestGateway_Chat_UnselectedNameKeepsTheSeamRegistry pins the fallback
// half of the switch: an assembly that selects no chat member leaves
// resolution exactly where it was, so no existing host gains a
// configuration obligation.
func TestGateway_Chat_UnselectedNameKeepsTheSeamRegistry(t *testing.T) {
	ctx := context.Background()
	seamProvider := &fakeChatProvider{chatResp: ChatResponse{Message: ChatMessage{Role: RoleAssistant, Content: "seam face"}}}

	g := gatewayTestFixture(t, seamProvider)
	g.components = selectOnly(t, ctx, pkgcore.Component{
		Name:     faceProbeChatName,
		Module:   "chat",
		Provides: []any{(*ChatProvider)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &fakeChatProvider{}, nil
		},
	})

	resp, err := g.Chat(ctx, chatReq())
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if seamProvider.chatCalls != 1 {
		t.Errorf("the seam-registry provider served %d calls, want 1: a name outside the assembly's selection must resolve as before", seamProvider.chatCalls)
	}
	if resp.Message.Content != "seam face" {
		t.Errorf("Chat() content = %q, want the seam registry provider's reply", resp.Message.Content)
	}
}

// TestGateway_Image_SelectedComponentServesTheRoute is the image-family
// counterpart, observed at the resolution seam GenerateImage and the job
// handler share: the provider a selected "image" member builds is the one
// the route resolves to, not the seam registry's.
func TestGateway_Image_SelectedComponentServesTheRoute(t *testing.T) {
	ctx := context.Background()
	probe := &fakeImageProvider{}

	g, _, _ := imageGatewayTestFixture(t, &fakeImageProvider{})
	g.routes["image:default"] = ModelRoute{Provider: faceProbeImageName, VendorModel: "vendor-image-model"}
	sysCtx, err := pkgcore.WithSystemContext(ctx, systemTestCtx(t))
	if err != nil {
		t.Fatalf("WithSystemContext: %v", err)
	}
	if err := g.credentials.SetPlatformCredential(sysCtx, faceProbeImageName, "sk-probe", ""); err != nil {
		t.Fatalf("SetPlatformCredential: %v", err)
	}

	g.components = selectOnly(t, ctx, pkgcore.Component{
		Name:     faceProbeImageName,
		Module:   "image",
		Provides: []any{(*ImageProvider)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return probe, nil
		},
	})

	provider, _, err := g.resolveImage(ctx, "image:default")
	if err != nil {
		t.Fatalf("resolveImage() error = %v", err)
	}
	if provider != ImageProvider(probe) {
		t.Errorf("resolveImage() = %T, want the selected member's own instance: a route naming a selected member must resolve through the component face", provider)
	}
}
