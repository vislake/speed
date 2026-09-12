package aigateway

// provider_components.go registers the components carrying this module's
// two built-in provider implementations: "chat.openai-compatible" and
// "image.openai-compatible", the descriptors a composition configuration
// selects as the "chat" and "image" modules' members. They live beside the
// implementations they adapt, the same file-locality the module's other
// component descriptors keep.
//
// # The route's logical name is the component name
//
// A model route's Provider field is a provider's name: the string
// a host writes in WithModelRoute, the key CredentialService.Resolve looks
// a credential up under, and the component name the provider carries.
// The components declared here carry exactly those names, so the
// resolution from a route's logical name to the component that carries the
// provider is the identity -- owned entirely inside this module, where a
// host's existing route strings, credential rows and composition selections
// need no translation and gain no new configuration obligation.
// TestProviderComponentNames_MatchProviderNamesAndCapabilities pins the
// identity against the descriptors' own name, module and capability
// declarations, so a rename or redeclaration fails a test instead of
// silently breaking a host's routes; should the names ever diverge, this
// file is where the mapping belongs.

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"
)

// openAICompatibleComponentConfig is the configuration schema both provider
// components share: the same two keys the registry adapters read from a
// flat pkgcore.Config, so a composition block and a flat Config spell the
// same settings. At assembly time the block carries the provider's
// assembly-wide endpoint and credential; the per-request path builds fresh
// instances through pkgcore.Build's override instead (see the descriptors'
// own doc comments).
type openAICompatibleComponentConfig struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

// chatProviderComponent is the component descriptor for
// "chat.openai-compatible": the OpenAI-compatible chat provider over the
// configuration the component's own block spells out, carrying the same
// MultiReplicaSafe|SurvivesRestart|Stateless declaration the seam
// registration carries (the provider holds no connection and no
// process-local state -- every call is an independent HTTP request). Its
// New funnels through openaiCompatibleFromConfig -- the package's one
// construction path -- so the component face and the seam face cannot
// diverge on validation or on the coded config refusal. Gateway.Chat /
// ChatStream reach a provider per request, and the component-face shape of
// that is Build with an override carrying the credential just resolved, so
// the assembly-time construction here is one instance of the same
// constructor, not a cached singleton.
var chatProviderComponent = pkgcore.Component{
	Name:         ProviderOpenAICompatible,
	Module:       "chat",
	Provides:     []any{(*ChatProvider)(nil)},
	Capabilities: pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart | pkgcore.Stateless,
	ConfigSchema: (*openAICompatibleComponentConfig)(nil),
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c openAICompatibleComponentConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		return openaiCompatibleFromConfig(pkgcore.Config{
			"base_url": c.BaseURL,
			"api_key":  c.APIKey,
		})
	},
}

// imageProviderComponent is the component descriptor for
// "image.openai-compatible", the image-generation counterpart of
// chatProviderComponent with the identical shape: the same two-key schema,
// the same capability declaration, and New funneling through
// openaiCompatibleImageFromConfig, the image side's one construction path.
var imageProviderComponent = pkgcore.Component{
	Name:         ProviderOpenAICompatibleImage,
	Module:       "image",
	Provides:     []any{(*ImageProvider)(nil)},
	Capabilities: pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart | pkgcore.Stateless,
	ConfigSchema: (*openAICompatibleComponentConfig)(nil),
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c openAICompatibleComponentConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		return openaiCompatibleImageFromConfig(pkgcore.Config{
			"base_url": c.BaseURL,
			"api_key":  c.APIKey,
		})
	},
}

func init() {
	pkgcore.MustRegister(chatProviderComponent)
	pkgcore.MustRegister(imageProviderComponent)
}
