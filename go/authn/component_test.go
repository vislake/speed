package authn

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/authn/internal/testutil"
)

// TestComponentWellFormed pins the descriptor's contract: the naming
// convention, the relation between the component name and the module it
// implements, the configuration schema's decodability and the token shapes,
// exactly as componenttest asserts them for a component package.
func TestComponentWellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, component())
}

// TestComponent_InitDeclaresThroughTheGate drives the descriptor through a
// real assembly: Prepare registers the PII serializer over the declared
// cipher-key material, New reads the declared blind-index key, and Init --
// the one stage whose seats accept writes -- runs the module's Register, so
// every declaration lands in the assembly's own seats and the assembly's
// Init-closing beat registers the module's system purpose.
func TestComponent_InitDeclaresThroughTheGate(t *testing.T) {
	db := testutil.NewDB(t)
	reg := pkgcore.NewComponentRegistry()
	reg.Put(testBootstrapMaterial(t))
	bus := pkgcore.NewMemoryEventBus()
	kv := pkgcore.NewMemoryKVStore()
	if err := componenttest.RunInit(t, reg, component(), db, testutil.NewKeySource(t, "component-test"), bus, kv); err != nil {
		t.Fatalf("RunInit: %v", err)
	}
	m, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembly's product: %v", err)
	}
	if m.Service() == nil {
		t.Fatal("Register did not build the module's service")
	}
	if !slices.Contains(reg.Permissions.Permissions(), PermissionSSOManage) {
		t.Errorf("Permissions seat = %v, want the SSO permission", reg.Permissions.Permissions())
	}
	if actions := reg.AuditActions.Actions(); len(actions) == 0 {
		t.Error("AuditActions seat is empty, want the module's audit vocabulary")
	}
	var keys []string
	for _, item := range reg.Config.Items() {
		keys = append(keys, item.Key)
	}
	if !slices.Contains(keys, ConfigKeyPasswordMinLength) || !slices.Contains(keys, ConfigKeyGoogleClientID) {
		t.Errorf("Config seat = %v, want the module's configuration items", keys)
	}
	var flags []string
	for _, flag := range reg.Features.Flags() {
		flags = append(flags, flag.Key)
	}
	if !slices.Contains(flags, FeatureFlagPasswordLogin) {
		t.Errorf("Features seat = %v, want the password-login flag", flags)
	}
	if routes := reg.Routes.Routes(); len(routes) != 1 || routes[0].Path != apiPath {
		t.Fatalf("Init mounted %v, want exactly the %s mount", routes, apiPath)
	}

	// The assembly's Init-closing beat registered the module's declared
	// system purpose.
	for _, purpose := range component().SystemPurposes {
		if _, err := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{Actor: "test", Purpose: purpose}); err != nil {
			t.Errorf("WithSystemContext(%q) = %v, want the purpose registered by the assembly", purpose, err)
		}
	}
}

// testBootstrapMaterial is the module's own declared key material, the
// shape the loader resolves and publishes before anything is constructed.
func testBootstrapMaterial(t *testing.T) *pkgcore.BootstrapMaterial {
	t.Helper()
	return pkgcore.NewBootstrapMaterial([]pkgcore.BootstrapMaterialEntry{
		{KeyPath: PIICipherKeyPath, Value: bytes.Repeat([]byte{0x11}, 32)},
		{KeyPath: BlindIndexKeyPath, Value: bytes.Repeat([]byte{0x22}, 32)},
	})
}

// TestComponentConstructionCoversTheOptionSurface drives New over the whole
// configuration schema and its failure paths: every field's option is
// applied, an unknown revocation mode and a partially zero password block
// are refused, a missing key source and an ambiguous optional sender are
// refused -- and a complete wiring still ends in the key-material deferral,
// so no path smuggles a keyless module out of the construction.
func TestComponentConstructionCoversTheOptionSurface(t *testing.T) {
	ctx := context.Background()
	full := pkgcore.NewComponentConfig(map[string]any{
		"revocation_mode":   "immediate",
		"trusted_proxies":   []string{"203.0.113.10"},
		"sms_code_ttl":      "5m",
		"issuer":            "https://issuer.example.com",
		"access_token_ttl":  "15m",
		"refresh_token_ttl": "720h",
		"session_ttl":       "720h",
		"password": map[string]any{
			"memory":      19456,
			"iterations":  2,
			"parallelism": 1,
			"salt_length": 16,
			"key_length":  32,
		},
	})

	// wired returns a registry carrying the database, key-source and
	// key-material values the construction reads, the shape the assembly's
	// products and the loader's material source provide.
	wired := func(t *testing.T) *pkgcore.ComponentRegistry {
		t.Helper()
		reg := pkgcore.NewComponentRegistry()
		reg.Put(testutil.NewDB(t))
		reg.Put(testutil.NewKeySource(t, "component-test"))
		reg.Put(testBootstrapMaterial(t))
		return reg
	}

	t.Run("full configuration constructs the module", func(t *testing.T) {
		reg := wired(t)
		reg.Put(pkgcore.NewConsoleSMSSender(io.Discard))
		m, err := component().New(ctx, reg, full)
		if err != nil {
			t.Fatalf("construction error = %v, want a fully wired module", err)
		}
		if m.(*Module).Service() != nil {
			t.Error("construction built the service; the service belongs to Register")
		}
	})

	t.Run("unknown revocation mode", func(t *testing.T) {
		_, err := component().New(ctx, wired(t), pkgcore.NewComponentConfig(map[string]any{"revocation_mode": "bogus"}))
		if err == nil || !strings.Contains(err.Error(), "revocation_mode") {
			t.Fatalf("construction error = %v, want the unknown revocation mode refusal", err)
		}
	})

	t.Run("password block with a zero parameter", func(t *testing.T) {
		_, err := component().New(ctx, wired(t), pkgcore.NewComponentConfig(map[string]any{
			"password": map[string]any{"iterations": 2, "parallelism": 1, "salt_length": 16, "key_length": 32},
		}))
		if err == nil || !strings.Contains(err.Error(), "argon2id") {
			t.Fatalf("construction error = %v, want the zero-parameter refusal", err)
		}
	})

	t.Run("missing key source", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		reg.Put(testutil.NewDB(t))
		reg.Put(testBootstrapMaterial(t))
		_, err := component().New(ctx, reg, pkgcore.NewComponentConfig(nil))
		if err == nil {
			t.Fatal("construction proceeded without a key source")
		}
	})

	t.Run("no material source", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		reg.Put(testutil.NewDB(t))
		reg.Put(testutil.NewKeySource(t, "component-test"))
		_, err := component().New(ctx, reg, pkgcore.NewComponentConfig(nil))
		if err == nil || !strings.Contains(err.Error(), "bootstrap material") {
			t.Fatalf("construction error = %v, want the missing-material-source refusal", err)
		}
	})

	t.Run("material source without the declared key", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		reg.Put(testutil.NewDB(t))
		reg.Put(testutil.NewKeySource(t, "component-test"))
		reg.Put(pkgcore.NewBootstrapMaterial(nil))
		_, err := component().New(ctx, reg, pkgcore.NewComponentConfig(nil))
		if err == nil || !strings.Contains(err.Error(), BlindIndexKeyPath) {
			t.Fatalf("construction error = %v, want the refusal naming %q", err, BlindIndexKeyPath)
		}
	})

	t.Run("ambiguous sender", func(t *testing.T) {
		reg := wired(t)
		reg.Put(pkgcore.NewConsoleSMSSender(io.Discard))
		reg.Put(pkgcore.NewConsoleSMSSender(io.Discard))
		_, err := component().New(ctx, reg, pkgcore.NewComponentConfig(nil))
		if err == nil {
			t.Fatal("construction picked one of two sender values instead of refusing the ambiguity")
		}
	})
}
