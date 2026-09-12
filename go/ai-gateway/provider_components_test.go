package aigateway

import (
	"context"
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// providerComponentSettings returns the settings the provider components'
// composition blocks carry: the same key set and values the construction
// path reads from a flat pkgcore.Config.
func providerComponentSettings() pkgcore.Config {
	return pkgcore.Config{
		"base_url": "https://upstream.example.test/v1",
		"api_key":  "sk-assembly",
	}
}

// componentBlock converts a flat Config into the map shape a composition
// block carries: the same keys, the same values.
func componentBlock(cfg pkgcore.Config) map[string]any {
	block := make(map[string]any, len(cfg))
	for key, value := range cfg {
		block[key] = value
	}
	return block
}

// TestProviderComponentsWellFormed runs the component descriptor contract
// every component package's suite asserts for both provider descriptors.
func TestProviderComponentsWellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, chatProviderComponent)
	componenttest.AssertWellFormed(t, imageProviderComponent)
}

// TestProviderComponentNames_MatchProviderNamesAndCapabilities pins the
// module-internal mapping the route resolution rests on: each component
// carries the name constant verbatim -- the same string a route's Provider
// field holds and a credential row is keyed by -- under its own module
// directory, and declares the three capability bits. A rename or
// redeclaration fails here instead of silently breaking a host's route
// strings.
func TestProviderComponentNames_MatchProviderNamesAndCapabilities(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	registered := make(map[string]pkgcore.Component)
	for _, c := range pkgcore.RegisteredComponents(reg) {
		registered[c.Name] = c
	}

	want := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart | pkgcore.Stateless

	chat, ok := registered[ProviderOpenAICompatible]
	if !ok {
		t.Fatalf("component %q is not globally registered; the provider component's init registration is missing", ProviderOpenAICompatible)
	}
	if chat.Capabilities != want {
		t.Errorf("component %q declares %v, want %v", ProviderOpenAICompatible, chat.Capabilities, want)
	}
	if chat.Module != "chat" {
		t.Errorf("component %q module = %q, want %q", ProviderOpenAICompatible, chat.Module, "chat")
	}

	image, ok := registered[ProviderOpenAICompatibleImage]
	if !ok {
		t.Fatalf("component %q is not globally registered; the provider component's init registration is missing", ProviderOpenAICompatibleImage)
	}
	if image.Capabilities != want {
		t.Errorf("component %q declares %v, want %v", ProviderOpenAICompatibleImage, image.Capabilities, want)
	}
	if image.Module != "image" {
		t.Errorf("component %q module = %q, want %q", ProviderOpenAICompatibleImage, image.Module, "image")
	}
}

// TestProviderComponentsAssembleAndBuildPerCall drives both descriptors
// through a real assembly and then through the per-call construction shape
// Gateway.Chat/GenerateImage will resolve providers with: pkgcore.Build
// with an override carrying the credential just resolved, producing a fresh
// instance instead of reusing the assembly-time product.
func TestProviderComponentsAssembleAndBuildPerCall(t *testing.T) {
	ctx := context.Background()
	settings := providerComponentSettings()

	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			ProviderOpenAICompatible:      componentBlock(settings),
			ProviderOpenAICompatibleImage: componentBlock(settings),
		},
	}))
	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v, want both provider components selected", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v, want both provider components constructed", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()) })

	chat, err := pkgcore.Get[ChatProvider](reg)
	if err != nil {
		t.Fatalf("Get[ChatProvider] error = %v, want the chat component's product", err)
	}
	if _, ok := chat.(*OpenAICompatibleProvider); !ok {
		t.Errorf("Get[ChatProvider] = %T, want *OpenAICompatibleProvider", chat)
	}
	image, err := pkgcore.Get[ImageProvider](reg)
	if err != nil {
		t.Fatalf("Get[ImageProvider] error = %v, want the image component's product", err)
	}
	if _, ok := image.(*OpenAICompatibleImageProvider); !ok {
		t.Errorf("Get[ImageProvider] = %T, want *OpenAICompatibleImageProvider", image)
	}

	caps, err := pkgcore.ComponentCapabilities(reg, ProviderOpenAICompatible)
	if err != nil || !caps.Has(pkgcore.MultiReplicaSafe|pkgcore.SurvivesRestart|pkgcore.Stateless) {
		t.Errorf("ComponentCapabilities(%s) = (%v, %v), want the declared MultiReplicaSafe|SurvivesRestart|Stateless", ProviderOpenAICompatible, caps, err)
	}

	// The per-call shape: an override carries the credential resolved for
	// this call, and the built instance is the caller's, not the
	// assembly-time product.
	override := pkgcore.NewComponentConfig(map[string]any{
		"base_url": "https://tenant-upstream.example.test/v1",
		"api_key":  "sk-per-call",
	})
	perCall, err := pkgcore.Build[*OpenAICompatibleProvider](ctx, reg, ProviderOpenAICompatible, &override)
	if err != nil {
		t.Fatalf("Build(%s, override) error = %v", ProviderOpenAICompatible, err)
	}
	if perCall.baseURL != "https://tenant-upstream.example.test/v1" || perCall.apiKey != "sk-per-call" {
		t.Errorf("Build(override) produced baseURL=%q apiKey=%q, want the override's credential", perCall.baseURL, perCall.apiKey)
	}
	if perCall == chat {
		t.Error("Build(override) returned the assembly-time product; a per-call construction must produce a fresh instance")
	}
}

// TestProviderComponents_EmptyConfigRefused pins the refusal for a
// credential carrying no base_url or api_key: the component's construction
// fails with pkgcore.ErrMissingSeamConfig attached as the cause, so a
// caller resolving a route onto such a credential sees a distinguishable,
// coded configuration error rather than an uncoded one.
func TestProviderComponents_EmptyConfigRefused(t *testing.T) {
	ctx := context.Background()

	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{ProviderOpenAICompatible: map[string]any{"base_url": "https://upstream.example.test/v1"}},
	}))
	if prepareErr := reg.Prepare(ctx); prepareErr != nil {
		t.Fatalf("Prepare() error = %v", prepareErr)
	}
	err := reg.Construct(ctx)
	if !errors.Is(err, pkgcore.ErrMissingSeamConfig) {
		t.Errorf("Construct with a base_url-only block = %v, want it to wrap ErrMissingSeamConfig", err)
	}
	_ = reg.Close(context.Background())
}
