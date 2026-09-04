package s3

import (
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestInit_RegistersObjectStoreS3OnTheSharedRegistry proves this package's
// init() really lands "objectstore.s3" on pkgcore's shared
// ObjectStoreRegistry with the capability the distributed deployment mode
// requires -- the same assertion pkgcore's own
// builtin_implementations_test.go made before this implementation moved out
// of its package, now owned by the package that performs the registration.
// pkgcore's PresetDistributed already names this implementation for the
// "objectstore" seam (preset_test.go pins the name itself); this test is
// what proves the name actually resolves once this package is imported.
func TestInit_RegistersObjectStoreS3OnTheSharedRegistry(t *testing.T) {
	s3Cfg := pkgcore.Config{"endpoint": "s3.example.com", "bucket": "objects", "access_key": "ak", "secret_key": "sk"}
	impl, caps, err := pkgcore.ObjectStoreRegistry.Build("objectstore.s3", s3Cfg)
	if err != nil {
		t.Fatalf("Build(%q) error = %v, want nil", "objectstore.s3", err)
	}
	if impl == nil {
		t.Error("Build(\"objectstore.s3\") returned a nil ObjectStore")
	}
	if want := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart; caps != want {
		t.Errorf("Build(%q) capabilities = %v, want %v", "objectstore.s3", caps, want)
	}
}

// TestInit_EmptyConfigRequiresConfig pins the documented gap
// pkgcore.PresetDistributed's own doc comment names: this seam has no safe
// default credentials, so resolving it with an empty Config fails with
// pkgcore.ErrMissingSeamConfig instead of silently building an unusable
// store.
func TestInit_EmptyConfigRequiresConfig(t *testing.T) {
	if _, _, err := pkgcore.ObjectStoreRegistry.Build("objectstore.s3", pkgcore.Config{}); err == nil {
		t.Error("Build() with an empty Config succeeded, want ErrMissingSeamConfig")
	}
}

// TestObjectStoreFromConfig_MissingFieldReturnsErrMissingSeamConfig pins
// each individually-missing field, mirroring pkgcore's own
// smtpMailerFromConfig tests for the SMTP seam.
func TestObjectStoreFromConfig_MissingFieldReturnsErrMissingSeamConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  pkgcore.Config
	}{
		{name: "missing everything", cfg: pkgcore.Config{}},
		{name: "missing bucket", cfg: pkgcore.Config{"endpoint": "s3.example.com", "access_key": "ak", "secret_key": "sk"}},
		{name: "missing access_key", cfg: pkgcore.Config{"endpoint": "s3.example.com", "bucket": "objects", "secret_key": "sk"}},
		{name: "missing secret_key", cfg: pkgcore.Config{"endpoint": "s3.example.com", "bucket": "objects", "access_key": "ak"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := objectStoreFromConfig(tt.cfg)
			if !errors.Is(err, pkgcore.ErrMissingSeamConfig) {
				t.Fatalf("objectStoreFromConfig(%v) error = %v, want it to wrap ErrMissingSeamConfig", tt.cfg, err)
			}
		})
	}
}
