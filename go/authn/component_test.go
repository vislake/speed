package authn

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/authn/internal/testutil"
)

// testDBComponent is a stand-in for the database component a real assembly
// selects: it declares the *gorm.DB product and constructs the test's
// migrated handle, so the descriptor's declared database dependency resolves
// exactly as it will against the real db component.
func testDBComponent(db *gorm.DB) pkgcore.Component {
	return pkgcore.Component{
		Name:     "test.db",
		Module:   "test",
		Provides: []any{(*gorm.DB)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return db, nil
		},
	}
}

// testKeySourceComponent is a stand-in for the signing-key lifecycle a real
// assembly selects: the structural KeySource testutil's fake implements, so
// the required token resolves without this package ever importing the module
// that satisfies it in production.
func testKeySourceComponent(t testing.TB) pkgcore.Component {
	source := testutil.NewKeySource(t, "component-test")
	return pkgcore.Component{
		Name:     "test.keysource",
		Module:   "test",
		Provides: []any{(*KeySource)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return source, nil
		},
	}
}

// TestComponentWellFormed pins the descriptor's contract: the naming
// convention, the relation between the component name and the module it
// implements, the configuration schema's decodability and the token shapes,
// exactly as componenttest asserts them for a component package.
func TestComponentWellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, component())
}

// TestComponentConstructionAwaitsKeyMaterial pins the deliberate deferral:
// every declared dependency resolves, the configuration decodes, and the
// construction still fails naming the bootstrap key whose material nothing
// in the assembly publishes yet -- the failure a host sees is the missing
// piece, never a silently keyless module.
func TestComponentConstructionAwaitsKeyMaterial(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(testDBComponent(testutil.NewDB(t))); err != nil {
		t.Fatalf("registering the database stand-in: %v", err)
	}
	if err := reg.Register(testKeySourceComponent(t)); err != nil {
		t.Fatalf("registering the key-source stand-in: %v", err)
	}
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"deployment": "standalone",
		"components": map[string]any{
			"authn":          map[string]any{},
			"test.db":        nil,
			"test.keysource": nil,
		},
	}))

	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	err := reg.Construct(ctx)
	if err == nil {
		t.Fatal("construct succeeded, want the key-material deferral")
	}
	if !errors.Is(err, pkgcore.ErrComponentFailed) {
		t.Fatalf("construct error %v does not carry ErrComponentFailed", err)
	}
	if !strings.Contains(err.Error(), "blind-index key") {
		t.Fatalf("construct error %v does not name the missing key material", err)
	}
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

	// wired returns a registry carrying the database and key-source values
	// the construction reads, the shape the assembly's products provide.
	wired := func(t *testing.T) *pkgcore.ComponentRegistry {
		t.Helper()
		reg := pkgcore.NewComponentRegistry()
		reg.Put(testutil.NewDB(t))
		reg.Put(testutil.NewKeySource(t, "component-test"))
		return reg
	}

	t.Run("full configuration reaches the key-material deferral", func(t *testing.T) {
		reg := wired(t)
		reg.Put(pkgcore.NewConsoleSMSSender(io.Discard))
		_, err := component().New(ctx, reg, full)
		if err == nil || !strings.Contains(err.Error(), "blind-index key") {
			t.Fatalf("construction error = %v, want the key-material deferral", err)
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
		_, err := component().New(ctx, reg, pkgcore.NewComponentConfig(nil))
		if err == nil {
			t.Fatal("construction proceeded without a key source")
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
