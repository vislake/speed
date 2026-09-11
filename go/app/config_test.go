package app

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	pkgconfig "github.com/vislake/speed/go/pkgcore/config"
)

// TestPlatformConfig_DeclaresExactlyThePlatformKeyPaths pins the declaration
// two ways: its field names spell the six declared key paths character for
// character (which is what the loader's key-path derivation reads), and the
// loader's own verification accepts those paths against the type.
func TestPlatformConfig_DeclaresExactlyThePlatformKeyPaths(t *testing.T) {
	var paths []string
	var walk func(reflect.Type, string)
	walk = func(t2 reflect.Type, prefix string) {
		for i := 0; i < t2.NumField(); i++ {
			f := t2.Field(i)
			name := prefix + strings.ToLower(f.Name)
			if f.Type.Kind() == reflect.Struct {
				walk(f.Type, name+".")
				continue
			}
			if !strings.Contains(f.Tag.Get(pkgconfig.TagName), "derive") {
				t.Errorf("field %s carries no derive tag option", name)
			}
			paths = append(paths, name)
		}
	}
	walk(reflect.TypeOf(PlatformConfig{}), "")

	if !slices.Equal(paths, platformKeyPaths) {
		t.Fatalf("PlatformConfig field paths = %v, want the declared key paths %v", paths, platformKeyPaths)
	}
	if err := pkgconfig.Verify(&PlatformConfig{}, platformKeyPaths); err != nil {
		t.Fatalf("the declaration does not bind its own key paths: %v", err)
	}
}

// TestNew_LoadsTheHostTargetAndThePlatformKeyMaterial pins the configuration
// stage end to end: one loader fills the host's own key from the injected
// environment and the platform material from the declared path's environment
// spelling (the prefix, then the path with each nesting level marked by a
// double underscore).
func TestNew_LoadsTheHostTargetAndThePlatformKeyMaterial(t *testing.T) {
	var host testHostConfig
	opts := testBaseOptions(t, &host)

	wantKey := testKey(0x77)
	t.Setenv("TEST_PORT", "4321")
	t.Setenv("TEST_AUTHN__BLIND_INDEX_KEY", hex.EncodeToString(wantKey))

	a, err := New(context.Background(), opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if host.Port != "4321" {
		t.Errorf("host Port = %q, want the injected TEST_PORT value", host.Port)
	}
	if !slices.Equal(host.Authn.Blind_Index_Key, wantKey) {
		t.Errorf("loaded authn blind-index material does not equal the injected value")
	}
	if slices.Equal(host.Config.Cipher_Key, wantKey) {
		t.Error("the config cipher material took the authn key's value")
	}
}

// TestNew_TheHostTargetCannotShadowThePlatformKeyPaths pins the embedding
// rule: the host target's platform field is skipped, so the loader's walk of
// the host target cannot resolve a prefixed spelling of a platform key. A
// host that forgot the skip tag would otherwise resolve each key twice, under
// two different spellings, from the same environment.
func TestNew_TheHostTargetCannotShadowThePlatformKeyPaths(t *testing.T) {
	var host testHostConfig
	opts := testBaseOptions(t, &host)
	defaultKey := slices.Clone(host.Config.Cipher_Key)

	t.Setenv("TEST_PLATFORMCONFIG__CONFIG__CIPHER_KEY", hex.EncodeToString(testKey(0x99)))

	a, err := New(context.Background(), opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if !slices.Equal(host.Config.Cipher_Key, defaultKey) {
		t.Error("a prefixed spelling of the platform path reached the platform key material; the host target's platform field must stay skipped")
	}
}

// TestNew_DerivesKeyMaterialFromTheRootKey pins the derivation wiring: with a
// root key and the platform derivation installed, material no source supplies
// explicitly resolves to the platform composition's answer for the field's
// declared path.
func TestNew_DerivesKeyMaterialFromTheRootKey(t *testing.T) {
	var host testHostConfig
	rootKey := testKey(0xAB)

	opts := []Option{
		testConfigOption(&host,
			ConfigRootKey(rootKey),
			ConfigKeyDerivation(dbkit.DeriveBootstrapKey),
		),
		WithDatabase(testDatabaseSpec(t)),
	}
	host.Config.Cipher_Key = nil
	host.Authn.Blind_Index_Key = nil
	host.Authn.PII_Cipher_Key = nil

	a, err := New(context.Background(), opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	for _, tc := range []struct {
		path string
		got  []byte
	}{
		{"config.cipher_key", host.Config.Cipher_Key},
		{"authn.blind_index_key", host.Authn.Blind_Index_Key},
		{"authn.pii_cipher_key", host.Authn.PII_Cipher_Key},
	} {
		want, err := dbkit.DeriveBootstrapKey(rootKey, tc.path)
		if err != nil {
			t.Fatalf("derive the expectation for %s: %v", tc.path, err)
		}
		if !slices.Equal(tc.got, want) {
			t.Errorf("material at %s was not derived from the root key", tc.path)
		}
	}
}

// TestNew_RefusesAMalformedRootKeyVariable pins the loader's root-key refusal
// as the engine's wiring delivers it: the variable the option named is the
// one the error names.
func TestNew_RefusesAMalformedRootKeyVariable(t *testing.T) {
	var host testHostConfig
	host.PlatformConfig = testPlatformConfig()
	opts := []Option{
		testConfigOption(&host, ConfigRootKeyEnv("TEST_ROOT_KEY")),
		WithDatabase(testDatabaseSpec(t)),
	}
	t.Setenv("TEST_ROOT_KEY", "not-a-64-hex-character-key")

	_, err := New(context.Background(), opts...)
	if err == nil {
		t.Fatal("New() with a malformed root key error = nil, want a refusal")
	}
	if !errors.Is(err, pkgconfig.ErrInvalidRootKey) || !strings.Contains(err.Error(), "TEST_ROOT_KEY") {
		t.Fatalf("root-key refusal = %v, want it to wrap ErrInvalidRootKey and name TEST_ROOT_KEY", err)
	}
}

// TestNew_LoadsFromAConfigFile pins the file source: a config file spells the
// same dotted key paths the flag and environment sources do, for both
// targets.
func TestNew_LoadsFromAConfigFile(t *testing.T) {
	var host testHostConfig
	fileKey := testKey(0x5A)
	path := filepath.Join(t.TempDir(), "app.yaml")
	contents := "port: \"7777\"\nconfig:\n  cipher_key: \"" + hex.EncodeToString(fileKey) + "\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write the config file: %v", err)
	}

	opts := []Option{
		testConfigOption(&host, ConfigFile(path)),
		WithDatabase(testDatabaseSpec(t)),
	}
	a, err := New(context.Background(), opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if host.Port != "7777" {
		t.Errorf("host Port = %q, want the config file's value", host.Port)
	}
	if !slices.Equal(host.Config.Cipher_Key, fileKey) {
		t.Error("the config file's cipher material did not reach the platform target")
	}
}

// TestNew_RefusesAnUnreadableConfigFile pins the fail-fast half of the file
// source: a file that exists but cannot be parsed fails the load rather than
// being skipped.
func TestNew_RefusesAnUnreadableConfigFile(t *testing.T) {
	var host testHostConfig
	path := filepath.Join(t.TempDir(), "app.yaml")
	if err := os.WriteFile(path, []byte("port: [unterminated\n"), 0o600); err != nil {
		t.Fatalf("write the config file: %v", err)
	}

	host.PlatformConfig = testPlatformConfig()
	_, err := New(context.Background(),
		testConfigOption(&host, ConfigFile(path)),
		WithDatabase(testDatabaseSpec(t)),
	)
	if err == nil || !errors.Is(err, pkgconfig.ErrSourceUnreadable) {
		t.Fatalf("New() with an unparseable config file error = %v, want it to wrap ErrSourceUnreadable", err)
	}
}

// TestNew_FlagsWinOverTheEnvironment pins the priority chain as the engine
// wires it: the highest source (the command line) beats the environment.
func TestNew_FlagsWinOverTheEnvironment(t *testing.T) {
	var host testHostConfig
	host.PlatformConfig = testPlatformConfig()
	opts := []Option{
		testConfigOption(&host, ConfigArgs([]string{"--port=9999"})),
		WithDatabase(testDatabaseSpec(t)),
	}
	t.Setenv("TEST_PORT", "4321")

	a, err := New(context.Background(), opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if host.Port != "9999" {
		t.Errorf("host Port = %q, want the flag's value to win over the environment", host.Port)
	}
}

// TestWithConfig_KeepsTheLoaderOptionsInOrder pins the option plumbing every
// other test depends on: the loader options reach the load, so a later
// ConfigArgs replaces an earlier one rather than being dropped.
func TestWithConfig_KeepsTheLoaderOptionsInOrder(t *testing.T) {
	var host testHostConfig
	cfg := &engineConfig{}
	WithConfig(
		ConfigSpec{Host: &host, Platform: &host.PlatformConfig},
		ConfigArgs([]string{"--port=1111"}),
		ConfigArgs([]string{"--port=2222"}),
	)(cfg)

	loader := pkgconfig.New(cfg.configOptions...)
	if err := loader.Load(cfg.configSpec.Host); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if host.Port != "2222" {
		t.Fatalf("host Port = %q, want the later option to win", host.Port)
	}
}
