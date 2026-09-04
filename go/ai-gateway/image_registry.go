package aigateway

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
)

// ProviderOpenAICompatibleImage is the ImageProviderRegistry name
// OpenAICompatibleImageProvider self-registers under, mirroring
// ProviderOpenAICompatible's naming for the chat side ("chat." vs
// "image." prefix).
const ProviderOpenAICompatibleImage = "image.openai-compatible"

// ImageProviderRegistry is the package-level
// pkgcore.SeamRegistry[ImageProvider] every host resolves a named
// ImageProvider implementation through -- the image-generation counterpart
// of ChatProviderRegistry, built for the identical reason (see
// ChatProviderRegistry's own doc comment): a future vendor-SDK-backed
// provider self-registers into this registry from its own init() without
// touching this round's code, and Gateway.resolveImage calls Build fresh on
// every job execution, passing the credential CredentialService.Resolve
// just looked up as the Config -- OpenAICompatibleImageProvider, like its
// chat sibling, performs no I/O at construction, so building one fresh per
// job has no caching problem to solve.
//
// ProviderOpenAICompatibleImage is registered below, in this package's own
// init(), for the identical reason ProviderOpenAICompatible is: it is this
// module's zero-vendor-SDK-dependency default image provider, not an
// optional add-on.
var ImageProviderRegistry = pkgcore.NewSeamRegistry[ImageProvider]()

func init() {
	mustRegisterImageProvider(pkgcore.Registration[ImageProvider]{
		Name: ProviderOpenAICompatibleImage,
		// OpenAICompatibleImageProvider holds no connection and no
		// process-local state of its own -- every call is an independent
		// HTTP request -- so it genuinely satisfies all three capability
		// bits, exactly like its chat sibling.
		Capabilities: pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart | pkgcore.Stateless,
		New:          openaiCompatibleImageFromConfig,
	})
}

// mustRegisterImageProvider adds r to ImageProviderRegistry and panics if
// that fails, mirroring mustRegisterChatProvider's identical reasoning: the
// one name this file controls can only fail to register on a programming
// error in this file itself.
func mustRegisterImageProvider(r pkgcore.Registration[ImageProvider]) {
	if err := ImageProviderRegistry.Register(r); err != nil {
		panic(fmt.Sprintf("aigateway: builtin image implementation registration failed: %v", err))
	}
}

// openaiCompatibleImageFromConfig adapts a flat pkgcore.Config onto
// NewOpenAICompatibleImageProvider, mirroring openaiCompatibleFromConfig's
// identical shape for the chat side.
func openaiCompatibleImageFromConfig(cfg pkgcore.Config) (ImageProvider, error) {
	baseURL := cfg["base_url"]
	apiKey := cfg["api_key"]
	if baseURL == "" || apiKey == "" {
		return nil, fmt.Errorf("aigateway: builtin %s seam: %w: requires \"base_url\" and \"api_key\"", ProviderOpenAICompatibleImage, pkgcore.ErrMissingSeamConfig)
	}
	return NewOpenAICompatibleImageProvider(baseURL, apiKey), nil
}
