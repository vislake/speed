package app

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	pkgconfig "github.com/vislake/speed/go/pkgcore/config"
)

// TestNew_LoadsTheHostTargetAndTheDeclaredKeyMaterial pins the configuration
// stage end to end: one loader fills the host's own key from the injected
// environment and resolves a declared component key from the declared path's
// environment spelling (the prefix, then the path with each nesting level
// marked by a double underscore), publishing the value as bootstrap material.
func TestNew_LoadsTheHostTargetAndTheDeclaredKeyMaterial(t *testing.T) {
	var host testHostConfig
	opts := testBaseOptions(t, &host)

	wantKey := testKey(0x77)
	t.Setenv("TEST_PORT", "4321")
	t.Setenv("TEST_CONFIG__CIPHER_KEY", hex.EncodeToString(wantKey))

	a, err := New(context.Background(), opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	if host.Port != "4321" {
		t.Errorf("host Port = %q, want the injected TEST_PORT value", host.Port)
	}
	material := materialOf(t, a)
	if got, ok := material.Material(testCipherKeyPath); !ok || !slices.Equal(got, wantKey) {
		t.Errorf("material %s = %x (present %v), want the injected value", testCipherKeyPath, got, ok)
	}
}

// TestNew_DeclaredKeysAreNotReachableThroughHostStructSpellings pins the
// declaration's own reading surface: a declared key resolves only from the
// variable derived from its declared path, so a spelling a host struct would
// have produced reaches nothing.
func TestNew_DeclaredKeysAreNotReachableThroughHostStructSpellings(t *testing.T) {
	var host testHostConfig
	opts := testBaseOptions(t, &host)
	tableKey, _ := testDevDefaults()[testCipherKeyPath]

	t.Setenv("TEST_PLATFORMCONFIG__CONFIG__CIPHER_KEY", hex.EncodeToString(testKey(0x99)))

	a, err := New(context.Background(), opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	got, ok := materialOf(t, a).Material(testCipherKeyPath)
	if !ok || !slices.Equal(got, tableKey) {
		t.Error("a prefixed struct spelling of the declared path reached the key material; only the declared path's own spelling may")
	}
}

// TestNew_DerivesKeyMaterialFromTheRootKey pins the derivation wiring: with a
// root key and the platform derivation installed, a declared key no source
// supplies explicitly resolves to the platform composition's answer for its
// declared path -- the derivation outranks the declared defaults table.
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

	a, err := New(context.Background(), opts...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close(context.Background()) })

	material := materialOf(t, a)
	want, err := dbkit.DeriveBootstrapKey(rootKey, testCipherKeyPath)
	if err != nil {
		t.Fatalf("derive the expectation for %s: %v", testCipherKeyPath, err)
	}
	if got, ok := material.Material(testCipherKeyPath); !ok || !slices.Equal(got, want) {
		t.Errorf("material at %s was not derived from the root key", testCipherKeyPath)
	}
}

// TestNew_RefusesAMissingCipherMaterial pins the infrastructure step's
// refusal when nothing supplies the key the platform cipher is built from:
// the error names the declared path, and it fails before the database opens.
func TestNew_RefusesAMissingCipherMaterial(t *testing.T) {
	var host testHostConfig
	_, err := New(context.Background(),
		// An empty table: with no environment variable, no root key and no
		// table entry, the declared key resolves to nothing.
		testConfigOption(&host, ConfigDevDefaults(nil)),
		WithDatabase(testDatabaseSpec(t)),
	)
	if err == nil {
		t.Fatal("New() with no cipher material error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), testCipherKeyPath) {
		t.Fatalf("refusal = %v, want it to name the declared key path %q", err, testCipherKeyPath)
	}
}

// TestNew_RefusesAMalformedPlatformCipher pins how a bad material is
// reported: the declared defaults table's own entry is validated where it is
// read, the refusal names the declared key path and the table, and the boot
// never reaches the database.
func TestNew_RefusesAMalformedPlatformCipher(t *testing.T) {
	var host testHostConfig
	_, err := New(context.Background(),
		testConfigOption(&host, ConfigDevDefaults(map[string][]byte{testCipherKeyPath: []byte("too short")})),
		WithDatabase(testDatabaseSpec(t)),
	)
	if err == nil {
		t.Fatal("New() with a malformed platform cipher error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "config.cipher_key") {
		t.Fatalf("cipher refusal does not name the declared key path: %v", err)
	}
	if !strings.Contains(err.Error(), "declared defaults table") {
		t.Fatalf("cipher refusal does not name the table the entry came from: %v", err)
	}
	if !errors.Is(err, pkgconfig.ErrInvalidValue) {
		t.Fatalf("cipher refusal = %v, want it to wrap config.ErrInvalidValue", err)
	}
}

// TestNew_RefusesAMalformedRootKeyVariable pins the loader's root-key refusal
// as the engine's wiring delivers it: the variable the option named is the
// one the error names.
func TestNew_RefusesAMalformedRootKeyVariable(t *testing.T) {
	var host testHostConfig
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
// same dotted key paths the flag and environment sources do, for both the
// host's own keys and the declared keys.
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
	if got, ok := materialOf(t, a).Material(testCipherKeyPath); !ok || !slices.Equal(got, fileKey) {
		t.Error("the config file's cipher material did not reach the declared key")
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
		ConfigSpec{Host: &host},
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
