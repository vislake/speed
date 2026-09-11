package aigateway

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
)

// ProviderOpenAICompatible is the ChatProviderRegistry name
// OpenAICompatibleProvider self-registers under.
const ProviderOpenAICompatible = "chat.openai-compatible"

// ChatProviderRegistry is the package-level pkgcore.SeamRegistry[ChatProvider]
// every host resolves a named ChatProvider implementation through, after
// the database/sql driver-registration pattern: an additional vendor-SDK-
// backed provider (a hypothetical go/ai-gateway/provider/anthropic
// subpackage using a real SDK) self-registers into this same registry from
// its own init(), and a host that never imports that subpackage never
// resolves its name -- resolving an unimported one at Gateway.Chat time
// fails with an error wrapping pkgcore.ErrUnknownImplementation naming it,
// the same "unknown driver" cost database/sql's own drivers accept.
//
// Unlike pkgcore's own seam values, resolved once per assembly, resolving
// a ChatProvider is not a one-shot,
// process-lifetime construction: Gateway.Chat/ChatStream call Build fresh
// on every request, passing the credential CredentialService.Resolve just
// looked up (base_url, api_key) as the Config -- a tenant's own BYOK
// credential can therefore select a completely different provider instance
// per call with no caching problem to solve, because building an
// OpenAICompatibleProvider does no I/O of its own (it only sets fields);
// see openaiCompatibleFromConfig.
//
// ProviderOpenAICompatible is registered below, in this package's own
// init(), rather than through a subpackage: OpenAICompatibleProvider
// already lives in go/ai-gateway's root package -- it is this module's
// zero-external-dependency default, not an optional add-on.
var ChatProviderRegistry = pkgcore.NewSeamRegistry[ChatProvider]()

func init() {
	mustRegisterChatProvider(pkgcore.Registration[ChatProvider]{
		Name: ProviderOpenAICompatible,
		// OpenAICompatibleProvider holds no connection and no process-local
		// state of its own -- every call is an independent HTTP request --
		// so it genuinely satisfies all three capability bits, though
		// nothing validates them the way the assembly validates the
		// four assembly-resolved seams: this registry is ai-gateway's own private
		// mechanism, not one of pkgcore's four deployment-mode-validated
		// seams.
		Capabilities: pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart | pkgcore.Stateless,
		New:          openaiCompatibleFromConfig,
	})
}

// mustRegisterChatProvider adds r to ChatProviderRegistry and panics if
// that fails. It is only ever called here, against the one name this file
// controls, so a failure -- a duplicate name -- is a programming error in
// this file, not a condition a caller could hit or would want to recover
// from.
func mustRegisterChatProvider(r pkgcore.Registration[ChatProvider]) {
	if err := ChatProviderRegistry.Register(r); err != nil {
		panic(fmt.Sprintf("aigateway: builtin implementation registration failed: %v", err))
	}
}

// openaiCompatibleFromConfig adapts a flat pkgcore.Config onto
// NewOpenAICompatibleProvider: Gateway.resolve calls
// ChatProviderRegistry.Build(route.Provider, pkgcore.Config{"base_url":
// cred.BaseURL, "api_key": cred.APIKey}) with the credential it just
// resolved for the current call, so this constructor performs no I/O at
// all; it only validates and assigns fields.
//
// The refusal of a config without base_url (or api_key) is this
// constructor's own declaration that OpenAICompatibleProvider has no
// default endpoint of its own: it comes back as the coded, Invalid-
// classified ErrProviderConfigInvalid, so a caller resolving a route onto
// a credential stored without a base URL sees a distinguishable
// configuration error rather than an uncoded one. pkgcore.ErrMissingSeamConfig
// stays attached as the cause, so errors.Is-based registry callers keep
// recognizing the refusal unchanged.
func openaiCompatibleFromConfig(cfg pkgcore.Config) (ChatProvider, error) {
	baseURL := cfg["base_url"]
	apiKey := cfg["api_key"]
	if baseURL == "" || apiKey == "" {
		return nil, ErrProviderConfigInvalid.
			WithParam("provider", ProviderOpenAICompatible).
			WithParam("reason", "credential carries no base_url or api_key")
	}
	return NewOpenAICompatibleProvider(baseURL, apiKey), nil
}
