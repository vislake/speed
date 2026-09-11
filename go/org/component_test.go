package org

import (
	"context"
	"net/http"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	"github.com/vislake/speed/go/org/internal/testutil"
	"github.com/vislake/speed/go/org/migrations"
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

// TestComponentWellFormed pins the descriptor's contract: the naming
// convention, the relation between the component name and the module it
// implements, the configuration schema's decodability and the token shapes,
// exactly as componenttest asserts them for a component package.
func TestComponentWellFormed(t *testing.T) {
	componenttest.AssertWellFormed(t, component())
}

// TestComponentAssemblesThroughRegistry drives the registered descriptor
// through the assembly's stages the way a host would: selection from a
// composition configuration (every schema field set), construction from the
// database, gate and resolver products in the by-type context, the closing
// validation, and the shutdown sequence. It proves the declared
// dependencies, assets and configuration schema line up with what New
// actually consumes.
func TestComponentAssemblesThroughRegistry(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(testDBComponent(testutil.NewSQLite(t, moduleName, migrations.FS))); err != nil {
		t.Fatalf("registering the database stand-in: %v", err)
	}
	reg.Put(FeatureGateFunc(func(context.Context, string) (bool, error) { return true, nil }))
	reg.Put(SubjectResolverFunc(func(*http.Request) (string, bool) { return "user-1", true }))
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"deployment": "standalone",
		"components": map[string]any{
			"org": map[string]any{
				"mail_from":      "invitations@example.com",
				"reply_to":       "invitations-reply@example.com",
				"invitation_ttl": "72h",
				"max_depth":      5,
			},
			"test.db": nil,
		},
	}))

	for _, stage := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"prepare", reg.Prepare},
		{"construct", reg.Construct},
		{"verify", reg.Verify},
		{"init", reg.Init},
		{"start", reg.Start},
	} {
		if err := stage.run(ctx); err != nil {
			t.Fatalf("%s: %v", stage.name, err)
		}
	}

	module, err := pkgcore.Get[*Module](reg)
	if err != nil {
		t.Fatalf("the assembled product is not reachable: %v", err)
	}
	if module == nil {
		t.Fatal("the assembled product is nil")
	}
	if names := pkgcore.MemberNames(reg, moduleName); len(names) != 1 || names[0] != moduleName {
		t.Fatalf("MemberNames = %v, want [%s]", names, moduleName)
	}
	if assets := pkgcore.Assets(reg); len(assets) != 1 || assets[0].Name != moduleName {
		t.Fatalf("Assets = %v, want exactly the %s entry", assets, moduleName)
	}

	if err := reg.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := reg.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestComponentConstructionRefusesBadInputs pins New's failure paths: an
// undeclared configuration key, a missing database product and an ambiguous
// optional dependency each fail the construction with an error rather than a
// half-built module.
func TestComponentConstructionRefusesBadInputs(t *testing.T) {
	ctx := context.Background()
	empty := pkgcore.NewComponentConfig(nil)

	t.Run("undeclared configuration key", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		if err := reg.Register(testDBComponent(testutil.NewSQLite(t, moduleName, migrations.FS))); err != nil {
			t.Fatalf("registering the database stand-in: %v", err)
		}
		_, err := component().New(ctx, reg, pkgcore.NewComponentConfig(map[string]any{"bogus": "x"}))
		if err == nil {
			t.Fatal("construction accepted an undeclared configuration key")
		}
	})

	t.Run("missing database", func(t *testing.T) {
		_, err := component().New(ctx, pkgcore.NewComponentRegistry(), empty)
		if err == nil {
			t.Fatal("construction proceeded without a database product")
		}
	})

	t.Run("ambiguous gate", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		if err := reg.Register(testDBComponent(testutil.NewSQLite(t, moduleName, migrations.FS))); err != nil {
			t.Fatalf("registering the database stand-in: %v", err)
		}
		reg.Put(FeatureGateFunc(func(context.Context, string) (bool, error) { return true, nil }))
		reg.Put(FeatureGateFunc(func(context.Context, string) (bool, error) { return false, nil }))
		_, err := component().New(ctx, reg, empty)
		if err == nil {
			t.Fatal("construction picked one of two gate values instead of refusing the ambiguity")
		}
	})

	t.Run("ambiguous resolver", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		if err := reg.Register(testDBComponent(testutil.NewSQLite(t, moduleName, migrations.FS))); err != nil {
			t.Fatalf("registering the database stand-in: %v", err)
		}
		reg.Put(SubjectResolverFunc(func(*http.Request) (string, bool) { return "user-1", true }))
		reg.Put(SubjectResolverFunc(func(*http.Request) (string, bool) { return "user-2", true }))
		_, err := component().New(ctx, reg, empty)
		if err == nil {
			t.Fatal("construction picked one of two resolver values instead of refusing the ambiguity")
		}
	})
}
