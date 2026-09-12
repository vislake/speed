package app

// bootstrap_material_test.go pins the declared key material an assembly
// resolves, without constructing anything: the six declared key paths'
// environment spellings (the APP_ prefix, then the path with each nesting
// level marked by a double underscore), the three-tier precedence -- an
// explicitly set variable over the APP_ROOT_KEY derivation over the
// documented development defaults -- the refusal a malformed explicit value
// gets, and the guarantee that the package-level dev keys are never written
// through.
//
// The resolution driven here is the assembly's own: declaredKeyOptions() is
// the option list assemble's LoadSpec carries, and the declarations come off
// the components this binary's imports register, exactly as a boot's do.
// Constructing the assembly would add SQLite files and module wiring to every
// case without touching the resolution under test.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/testutil"
	speedapp "github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/config"
)

// declaredKeyPaths lists the six declared key paths the composed modules'
// components carry, in the order the material assertions walk them.
var declaredKeyPaths = []string{
	"authn.blind_index_key",
	"authn.pii_cipher_key",
	"config.cipher_key",
	"notification.contact_index_key",
	"org.invitation_email_index_key",
	"pki.local_key_cipher_key",
}

// declaredKeyEnvNames spells each declared key path the way the environment
// carries it under this app's prefix.
func declaredKeyEnvNames() map[string]string {
	names := make(map[string]string, len(declaredKeyPaths))
	for _, path := range declaredKeyPaths {
		names[path] = config.EnvName(EnvPrefix, path)
	}
	return names
}

// resolveDeclaredMaterial runs the assembly's own resolution over this
// binary's registered components and returns the published material.
func resolveDeclaredMaterial(t *testing.T) *pkgcore.BootstrapMaterial {
	t.Helper()

	b := newServerBuild(ServerConfig{DeploymentMode: pkgcore.DeploymentModeStandalone})
	reg := pkgcore.NewComponentRegistry()
	if err := speedapp.Load(context.Background(), reg, speedapp.LoadSpec{
		Host:    &b.hostConfig,
		Options: declaredKeyOptions(),
		Args:    []string{},
	}); err != nil {
		t.Fatalf("Load(): %v", err)
	}
	material, err := pkgcore.BootstrapMaterialOf(reg)
	if err != nil {
		t.Fatalf("BootstrapMaterialOf(): %v", err)
	}
	return material
}

// devDefaultByPath maps each declared key path to the development default
// BootstrapDevDefaults carries for it.
func devDefaultByPath() map[string][]byte {
	table := BootstrapDevDefaults()
	byPath := make(map[string][]byte, len(declaredKeyPaths))
	for _, path := range declaredKeyPaths {
		byPath[path] = table[path]
	}
	return byPath
}

// TestDeclaredMaterial_EnvSpellingAndThreeTierPrecedence pins the resolution
// end to end over the real option list: the explicit variable wins, the
// APP_ROOT_KEY derivation stands next, and the documented development default
// is what remains when neither is set -- each key read from exactly the
// variable its declared path derives to.
func TestDeclaredMaterial_EnvSpellingAndThreeTierPrecedence(t *testing.T) {
	t.Run("no source: the documented development default stands", func(t *testing.T) {
		testutil.ClearBootstrapEnv(t)

		material := resolveDeclaredMaterial(t)
		for path, want := range devDefaultByPath() {
			got, ok := material.Material(path)
			if !ok || !bytes.Equal(got, want) {
				t.Errorf("material %s = %x (present %v), want the development default %x", path, got, ok, want)
			}
		}
	})

	t.Run("APP_ROOT_KEY derives every declared key", func(t *testing.T) {
		testutil.ClearBootstrapEnv(t)
		rootKey := bytes.Repeat([]byte{0x11}, 32)
		t.Setenv(rootKeyEnv, hex.EncodeToString(rootKey))

		material := resolveDeclaredMaterial(t)
		for _, path := range declaredKeyPaths {
			want, err := dbkit.DeriveBootstrapKey(rootKey, path)
			if err != nil {
				t.Fatalf("DeriveBootstrapKey(%q): %v", path, err)
			}
			got, ok := material.Material(path)
			if !ok || !bytes.Equal(got, want) {
				t.Errorf("material %s = %x (present %v), want the derivation %x", path, got, ok, want)
			}
		}
	})

	t.Run("an explicit variable wins over the derivation", func(t *testing.T) {
		testutil.ClearBootstrapEnv(t)
		t.Setenv(rootKeyEnv, hex.EncodeToString(bytes.Repeat([]byte{0x22}, 32)))

		explicit := bytes.Repeat([]byte{0x33}, 32)
		names := declaredKeyEnvNames()
		t.Setenv(names["config.cipher_key"], hex.EncodeToString(explicit))

		material := resolveDeclaredMaterial(t)
		got, ok := material.Material("config.cipher_key")
		if !ok || !bytes.Equal(got, explicit) {
			t.Errorf("material config.cipher_key = %x (present %v), want the explicit %s value %x",
				got, ok, names["config.cipher_key"], explicit)
		}
		// The keys nothing overrides still derive.
		otherWant, err := dbkit.DeriveBootstrapKey(bytes.Repeat([]byte{0x22}, 32), "authn.pii_cipher_key")
		if err != nil {
			t.Fatalf("DeriveBootstrapKey: %v", err)
		}
		if got, ok := material.Material("authn.pii_cipher_key"); !ok || !bytes.Equal(got, otherWant) {
			t.Errorf("material authn.pii_cipher_key = %x (present %v), want it to still derive", got, ok)
		}
	})

	t.Run("every declared path reads its own derived variable", func(t *testing.T) {
		testutil.ClearBootstrapEnv(t)

		names := declaredKeyEnvNames()
		want := make(map[string][]byte, len(declaredKeyPaths))
		for i, path := range declaredKeyPaths {
			key := bytes.Repeat([]byte{byte(0x40 + i)}, 32)
			want[path] = key
			t.Setenv(names[path], hex.EncodeToString(key))
		}

		material := resolveDeclaredMaterial(t)
		for path, key := range want {
			got, ok := material.Material(path)
			if !ok || !bytes.Equal(got, key) {
				t.Errorf("material %s = %x (present %v), want the value set as %s", path, got, ok, names[path])
			}
		}
	})
}

// TestDeclaredMaterial_RefusesAMalformedExplicitValue pins the loader's
// refusal as the assembly's own resolution delivers it: a malformed explicit
// value fails the load wrapping config.ErrInvalidValue and names the variable
// the operator must fix.
func TestDeclaredMaterial_RefusesAMalformedExplicitValue(t *testing.T) {
	testutil.ClearBootstrapEnv(t)
	envName := declaredKeyEnvNames()["authn.pii_cipher_key"]
	t.Setenv(envName, "not-64-hex-characters")

	b := newServerBuild(ServerConfig{DeploymentMode: pkgcore.DeploymentModeStandalone})
	reg := pkgcore.NewComponentRegistry()
	err := speedapp.Load(context.Background(), reg, speedapp.LoadSpec{
		Host:    &b.hostConfig,
		Options: declaredKeyOptions(),
		Args:    []string{},
	})
	if err == nil {
		t.Fatal("Load() with a malformed declared key value error = nil, want a refusal")
	}
	if !errors.Is(err, config.ErrInvalidValue) {
		t.Errorf("Load() error = %v, want it to wrap config.ErrInvalidValue", err)
	}
	if !strings.Contains(err.Error(), envName) {
		t.Errorf("refusal does not name %s: %v", envName, err)
	}
}

// TestDeclaredMaterial_NeverWritesThroughTheDevDefaults pins the hazard the
// table's clones guard: the resolved material must share no backing array
// with the package-level Dev* constants, so no later write -- a consumer
// mutating what it read included -- can poison the defaults every zero-setup
// load in this process falls back to.
func TestDeclaredMaterial_NeverWritesThroughTheDevDefaults(t *testing.T) {
	testutil.ClearBootstrapEnv(t)

	defaults := devDefaultByPath()
	before := make(map[string][]byte, len(defaults))
	for path, key := range defaults {
		before[path] = bytes.Clone(key)
	}

	material := resolveDeclaredMaterial(t)
	for path, key := range defaults {
		if !bytes.Equal(key, before[path]) {
			t.Errorf("the development default for %s was rewritten in place: got %x, want %x", path, key, before[path])
		}
		got, ok := material.Material(path)
		if !ok {
			t.Fatalf("material %s is absent, want the development default", path)
		}
		got[0] ^= 0xff
		if !bytes.Equal(key, before[path]) {
			t.Errorf("writing the resolved material of %s changed the development default: got %x, want %x", path, key, before[path])
		}
	}
}
