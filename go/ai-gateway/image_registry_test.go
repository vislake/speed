package aigateway

import (
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestImageProviderRegistry_BuiltinRegistered(t *testing.T) {
	provider, caps, err := ImageProviderRegistry.Build(ProviderOpenAICompatibleImage, pkgcore.Config{
		"base_url": "https://api.openai.com/v1",
		"api_key":  "sk-test",
	})
	if err != nil {
		t.Fatalf("Build(%q): %v", ProviderOpenAICompatibleImage, err)
	}
	if provider == nil {
		t.Fatal("Build returned a nil ImageProvider")
	}
	want := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart | pkgcore.Stateless
	if caps != want {
		t.Fatalf("Build capabilities = %v, want %v", caps, want)
	}
}

func TestImageProviderRegistry_Build_MissingConfig_Refused(t *testing.T) {
	tests := map[string]pkgcore.Config{
		"missing base_url": {"api_key": "sk-test"},
		"missing api_key":  {"base_url": "https://api.openai.com/v1"},
		"empty config":     {},
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ImageProviderRegistry.Build(ProviderOpenAICompatibleImage, cfg); !errors.Is(err, pkgcore.ErrMissingSeamConfig) {
				t.Fatalf("Build(%v) err = %v, want ErrMissingSeamConfig", cfg, err)
			}
		})
	}
}

func TestImageProviderRegistry_Build_UnknownName_Refused(t *testing.T) {
	if _, _, err := ImageProviderRegistry.Build("image.unregistered-vendor", pkgcore.Config{}); !errors.Is(err, pkgcore.ErrUnknownImplementation) {
		t.Fatalf("Build(unknown) err = %v, want ErrUnknownImplementation", err)
	}
}
