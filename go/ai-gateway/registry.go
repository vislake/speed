package aigateway

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
)

// ProviderOpenAICompatible is the ChatProviderRegistry name
// OpenAICompatibleProvider self-registers under.
const ProviderOpenAICompatible = "chat.openai-compatible"

// ChatProviderRegistry is the package-level pkgcore.SeamRegistry[ChatProvider]
// every host resolves a named ChatProvider implementation through,
// mirroring the database/sql driver-registration pattern go/pki's
// SignerRegistry follows (go/pki/signer_registry.go) for the identical
// reason: a future vendor-SDK-backed provider (a hypothetical
// go/ai-gateway/provider/anthropic subpackage using a real SDK) self-
// registers into this same registry from its own init(), without touching
// this round's code, and a host that never imports that subpackage never
// resolves its name -- resolving an unimported one at Gateway.Chat time
// fails with an error wrapping pkgcore.ErrUnknownImplementation naming it,
// the same "unknown driver" cost database/sql's own drivers accept.
//
// Unlike pkgcore's own EventBusRegistry/KVStoreRegistry/MailerRegistry/
// ObjectStoreRegistry, resolving a ChatProvider is not a one-shot,
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
// zero-external-dependency default, not an optional add-on, exactly the
// reasoning go/pki/signer_registry.go's own doc comment gives for keeping
// "signer.local" un-split.
var ChatProviderRegistry = pkgcore.NewSeamRegistry[ChatProvider]()

func init() {
	mustRegisterChatProvider(pkgcore.Registration[ChatProvider]{
		Name: ProviderOpenAICompatible,
		// OpenAICompatibleProvider holds no connection and no process-local
		// state of its own -- every call is an independent HTTP request --
		// so it genuinely satisfies all three capability bits, though
		// nothing in this round's Gateway validates them the way
		// Kernel.Bootstrap validates the four kernel seams: this registry
		// is ai-gateway's own private mechanism, not one of pkgcore's four
		// deployment-mode-validated seams.
		Capabilities: pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart | pkgcore.Stateless,
		New:          openaiCompatibleFromConfig,
	})
}

// mustRegisterChatProvider adds r to ChatProviderRegistry and panics if
// that fails. It is only ever called here, against the one name this file
// controls, so a failure -- a duplicate name -- is a programming error in
// this file, not a condition a caller could hit or would want to recover
// from, mirroring pkgcore's own unexported mustRegister helper and
// go/pki/signer_registry.go's identical copy.
func mustRegisterChatProvider(r pkgcore.Registration[ChatProvider]) {
	if err := ChatProviderRegistry.Register(r); err != nil {
		panic(fmt.Sprintf("aigateway: builtin implementation registration failed: %v", err))
	}
}

// openaiCompatibleFromConfig adapts a flat pkgcore.Config onto
// NewOpenAICompatibleProvider: Gateway.resolve calls
// ChatProviderRegistry.Build(route.Provider, pkgcore.Config{"base_url":
// cred.BaseURL, "api_key": cred.APIKey}) with the credential it just
// resolved for the current call, so this constructor -- unlike
// go/pki/signer_registry.go's localSignerFromConfig, which opens a real
// database connection -- performs no I/O at all; it only validates and
// assigns fields.
func openaiCompatibleFromConfig(cfg pkgcore.Config) (ChatProvider, error) {
	baseURL := cfg["base_url"]
	apiKey := cfg["api_key"]
	if baseURL == "" || apiKey == "" {
		return nil, fmt.Errorf("aigateway: builtin %s seam: %w: requires \"base_url\" and \"api_key\"", ProviderOpenAICompatible, pkgcore.ErrMissingSeamConfig)
	}
	return NewOpenAICompatibleProvider(baseURL, apiKey), nil
}
