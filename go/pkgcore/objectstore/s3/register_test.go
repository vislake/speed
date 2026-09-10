package s3

import (
	"errors"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestInit_RegistersObjectStoreS3OnTheSharedRegistry proves this package's
// init() really lands "objectstore.s3" on pkgcore's shared
// ObjectStoreRegistry with the capability the distributed deployment mode
// requires -- the registration this package itself performs, verified
// from the consuming side.
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
	if caps != Capabilities {
		t.Errorf("Build(%q) capabilities = %v, want the exported Capabilities constant %v the host reads off this package", "objectstore.s3", caps, Capabilities)
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

// TestParseBucketLookup pins the "bucket_lookup" key's string grammar: the
// two spellings of the default (unset and "auto", case and whitespace
// ignored) select BucketLookupAuto, the two other legal values select their
// enum members, and anything else comes back as an error naming the allowed
// set rather than a zero value.
func TestParseBucketLookup(t *testing.T) {
	ok := []struct {
		raw  string
		want BucketLookupType
	}{
		{"", BucketLookupAuto},
		{"auto", BucketLookupAuto},
		{" Auto ", BucketLookupAuto},
		{"path", BucketLookupPath},
		{"PATH", BucketLookupPath},
		{"virtual_host", BucketLookupVirtualHost},
	}
	for _, tt := range ok {
		got, err := parseBucketLookup(tt.raw)
		if err != nil {
			t.Errorf("parseBucketLookup(%q) error = %v, want nil", tt.raw, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseBucketLookup(%q) = %d, want %d", tt.raw, got, tt.want)
		}
	}

	for _, bad := range []string{"virtual-host", "virtualhost", "path_style", "dns", "true"} {
		got, err := parseBucketLookup(bad)
		if err == nil {
			t.Errorf("parseBucketLookup(%q) = %d, want an error", bad, got)
			continue
		}
		for _, want := range []string{`"auto"`, `"path"`, `"virtual_host"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("parseBucketLookup(%q) error = %q, want it to name %s", bad, err, want)
			}
		}
	}
}

// TestObjectStoreFromConfig_BucketLookup drives the key through the seam
// the way a host does: with the key set to each legal value, and with the
// key absent, Build resolves a store; with the key naming something else,
// Build returns an error naming the allowed values -- never a panic, the
// failure mode a Build call documented to return an error must not have.
func TestObjectStoreFromConfig_BucketLookup(t *testing.T) {
	base := func() pkgcore.Config {
		return pkgcore.Config{
			"endpoint":   "s3.example.com:9000",
			"bucket":     "objects",
			"access_key": "ak",
			"secret_key": "sk",
		}
	}

	for _, raw := range []string{"", "auto", "path", "virtual_host"} {
		t.Run("legal value "+raw, func(t *testing.T) {
			cfg := base()
			if raw != "" {
				cfg["bucket_lookup"] = raw
			}
			impl, _, err := pkgcore.ObjectStoreRegistry.Build("objectstore.s3", cfg)
			if err != nil {
				t.Fatalf("Build(%q) with bucket_lookup %q error = %v, want nil", "objectstore.s3", raw, err)
			}
			if impl == nil {
				t.Errorf("Build(%q) with bucket_lookup %q returned a nil ObjectStore", "objectstore.s3", raw)
			}
		})
	}

	t.Run("an unknown value is an error, not a panic", func(t *testing.T) {
		cfg := base()
		cfg["bucket_lookup"] = "path_style"

		var impl pkgcore.ObjectStore
		var err error
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("Build panicked on an unknown bucket_lookup (%v), want an error back", recovered)
				}
			}()
			impl, _, err = pkgcore.ObjectStoreRegistry.Build("objectstore.s3", cfg)
		}()
		if err == nil {
			t.Fatalf("Build with bucket_lookup %q error = nil, want an error", "path_style")
		}
		if !strings.Contains(err.Error(), "bucket_lookup") {
			t.Errorf("Build error = %q, want it to name the key", err)
		}
		if impl != nil {
			t.Errorf("Build returned %v alongside the error, want nil", impl)
		}
	})
}

// TestRegistration_WrapsTheHostAssembledConfig pins the factory contract a
// host follows on the name-registration path: the Registration carries the
// name the host chose and the package's own exported Capabilities, and its
// New hands back the bare store over the Config the host assembled -- no
// Close() error method, so Kernel.Shutdown never touches it, the same
// ownership pkgcore.WithObjectStore records. New ignores a preset entry's
// Config: the empty Config below would fail with ErrMissingSeamConfig if
// the factory consulted it.
func TestRegistration_WrapsTheHostAssembledConfig(t *testing.T) {
	r := Registration("objectstore.s3.prod", Config{
		Endpoint:  "s3.example.com",
		Bucket:    "objects",
		AccessKey: "ak",
		SecretKey: "sk",
	})
	if r.Name != "objectstore.s3.prod" {
		t.Errorf("Registration().Name = %q, want the host-chosen name", r.Name)
	}
	if r.Capabilities != Capabilities {
		t.Errorf("Registration().Capabilities = %v, want the exported constant %v", r.Capabilities, Capabilities)
	}

	impl, err := r.New(pkgcore.Config{})
	if err != nil {
		t.Fatalf("Registration().New() error = %v, want nil: the factory ignores the preset entry's Config", err)
	}
	if impl == nil {
		t.Fatal("Registration().New() returned a nil ObjectStore")
	}
	if _, ok := impl.(interface{ Close() error }); ok {
		t.Error("Registration().New() returned a value carrying Close() error, want the bare store the host owns")
	}
}
