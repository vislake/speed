// This file lives in package unittest — this module's dedicated unit-test
// directory for unit-tier suites with no single source file as their target
// (the backend coding standard's testing-layout rule). The golden roster of
// this module's own components and the assembly smoke driving them all span
// the pkgcore root and eight implementation subpackages at once, so they
// have no single target; and they must be black-box against package pkgcore:
// the root's built-in descriptors are unexported, so the roster is read
// through the public GlobalComponents enumeration, and componenttest itself
// imports pkgcore (an internal test file importing it would be the import
// cycle mailer_conformance_test.go's own note records).
package unittest

import (
	"context"
	"embed"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"

	// The eight distributed implementation subpackages self-register their
	// components from their own init(): importing them here is what puts
	// them in this binary's golden roster.
	_ "github.com/vislake/speed/go/pkgcore/eventbus/nats"
	_ "github.com/vislake/speed/go/pkgcore/eventbus/postgres"
	_ "github.com/vislake/speed/go/pkgcore/eventbus/redis"
	_ "github.com/vislake/speed/go/pkgcore/kv/memcached"
	_ "github.com/vislake/speed/go/pkgcore/kv/nats"
	_ "github.com/vislake/speed/go/pkgcore/kv/postgres"
	_ "github.com/vislake/speed/go/pkgcore/kv/redis"
	_ "github.com/vislake/speed/go/pkgcore/objectstore/s3"
)

// assembledPostgresDSN is a well-formed PostgreSQL DSN pointing nowhere: the
// pgx pool parses it at construction and dials lazily, so constructing
// through it proves the component's wiring without a server.
const assembledPostgresDSN = "postgres://assembly:assembly@127.0.0.1:5432/assembly?sslmode=disable"

// builtinComponentNames is the golden roster of the components the pkgcore
// module's own packages self-register: the seven root built-ins plus the
// eight distributed subpackages this file imports. Deleting or renaming any
// of them fails the test below by name.
var builtinComponentNames = []string{
	"eventbus.memory",
	"kv.memory",
	"mailer.console",
	"mailer.smtp",
	"objectstore.local",
	"sms.console",
	"sms.http",
	"eventbus.redis",
	"eventbus.nats",
	"eventbus.postgres",
	"kv.redis",
	"kv.postgres",
	"kv.memcached",
	"kv.nats",
	"objectstore.s3",
}

// TestBuiltinComponents_GlobalRosterIsWellFormed pins the golden roster and
// runs every registered built-in through the component descriptor contract:
// each expected name must resolve through the public enumeration, and its
// descriptor must satisfy componenttest's checks (the naming convention, a
// decodable ConfigSchema, typed contract tokens).
func TestBuiltinComponents_GlobalRosterIsWellFormed(t *testing.T) {
	registered := make(map[string]pkgcore.Component)
	for _, c := range pkgcore.GlobalComponents() {
		registered[c.Name] = c
	}

	for _, name := range builtinComponentNames {
		c, ok := registered[name]
		if !ok {
			t.Errorf("component %q is not globally registered; this golden roster fails when a built-in is deleted or renamed", name)
			continue
		}
		componenttest.AssertWellFormed(t, c)
	}
}

// builtinAssemblies enumerates every built-in implementation but kv.nats
// (whose construction dials a NATS server synchronously and has no offline
// success path), each with the decodable configuration its block takes.
// One implementation per seam is an assembly's shape under the single-value
// delivery rule, so every entry is assembled on its own.
var builtinAssemblies = []struct {
	name       string
	cfg        any
	migrations bool // the implementation carries a support-table migration set
}{
	{name: "eventbus.memory"},
	{name: "eventbus.redis"},
	{name: "eventbus.nats"},
	{name: "eventbus.postgres", cfg: map[string]any{"dsn": assembledPostgresDSN, "replica_id": "assembly-replica"}, migrations: true},
	{name: "kv.memory"},
	{name: "kv.redis"},
	{name: "kv.postgres", cfg: map[string]any{"dsn": assembledPostgresDSN}, migrations: true},
	{name: "kv.memcached"},
	{name: "mailer.console"},
	{name: "mailer.smtp", cfg: map[string]any{"host": "relay.assembly.test"}},
	{name: "objectstore.local"},
	{name: "objectstore.s3", cfg: map[string]any{
		"endpoint":   "objects.assembly.test",
		"bucket":     "assembly",
		"access_key": "assembly-key",
		"secret_key": "assembly-secret",
	}},
	{name: "sms.console"},
	{name: "sms.http", cfg: map[string]any{"endpoint": "https://gateway.assembly.test/sms"}},
}

// TestBuiltinComponents_AssembleAndConstructAll drives the full roster
// through real assemblies: each implementation is selected with its
// decodable configuration, Prepare validates its block, capability and
// assets, and Construct runs its New. It is the end-to-end proof that every
// built-in resolves and builds through the component registry.
func TestBuiltinComponents_AssembleAndConstructAll(t *testing.T) {
	for _, impl := range builtinAssemblies {
		t.Run(impl.name, func(t *testing.T) {
			ctx := context.Background()
			reg := pkgcore.NewComponentRegistry()
			reg.Put(pkgcore.NewComponentConfig(map[string]any{
				"components": map[string]any{impl.name: impl.cfg},
			}))

			if err := reg.Prepare(ctx); err != nil {
				t.Fatalf("Prepare() error = %v, want %s selected and validated", err, impl.name)
			}
			if err := reg.Construct(ctx); err != nil {
				t.Fatalf("Construct() error = %v, want %s constructed", err, impl.name)
			}
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := reg.Close(closeCtx); err != nil {
				t.Errorf("Close() error = %v, want %s released", err, impl.name)
			}

			// The PostgreSQL-backed implementations carry their
			// support-table migration sets, so the database component's
			// Verify step has them to apply.
			if !impl.migrations {
				return
			}
			var zeroFS embed.FS
			carried := false
			for _, asset := range pkgcore.Assets(reg) {
				if asset.Name == impl.name && asset.Migrations != zeroFS {
					carried = true
				}
			}
			if !carried {
				t.Errorf("Assets(reg) carries no migrations for %q, want the package's support-table set", impl.name)
			}
		})
	}
}

// TestBuiltinComponents_SelectedProductsResolveByType pins the Provides
// declarations against the by-type context: one implementation per module is
// selected, constructed, and then read back through Get at the module's own
// contract type -- the shape every consumer of these components uses.
func TestBuiltinComponents_SelectedProductsResolveByType(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"eventbus.memory":   nil,
			"kv.memory":         nil,
			"mailer.smtp":       map[string]any{"host": "relay.assembly.test"},
			"objectstore.local": nil,
			"sms.console":       nil,
		},
	}))

	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = reg.Close(closeCtx)
	})

	if _, err := pkgcore.Get[pkgcore.EventBus](reg); err != nil {
		t.Errorf("Get[pkgcore.EventBus] error = %v, want the selected eventbus.memory product", err)
	}
	if _, err := pkgcore.Get[pkgcore.KVStore](reg); err != nil {
		t.Errorf("Get[pkgcore.KVStore] error = %v, want the selected kv.memory product", err)
	}
	if _, err := pkgcore.Get[pkgcore.Mailer](reg); err != nil {
		t.Errorf("Get[pkgcore.Mailer] error = %v, want the selected mailer.smtp product", err)
	}
	if _, err := pkgcore.Get[pkgcore.ObjectStore](reg); err != nil {
		t.Errorf("Get[pkgcore.ObjectStore] error = %v, want the selected objectstore.local product", err)
	}
	if _, err := pkgcore.Get[pkgcore.SMSSender](reg); err != nil {
		t.Errorf("Get[pkgcore.SMSSender] error = %v, want the selected sms.console product", err)
	}
}

// TestBuiltinComponents_DistributedModeCapabilityCheck pins the capability
// declarations through the mode comparison the Prepare stage runs: the
// in-process implementations are refused in a distributed composition,
// named by component and missing bit, while the shared-backend
// implementations of the same modules are accepted.
func TestBuiltinComponents_DistributedModeCapabilityCheck(t *testing.T) {
	t.Run("in-process implementation refused", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		reg.Put(pkgcore.NewComponentConfig(map[string]any{
			"deployment": "distributed",
			"components": map[string]any{"eventbus.memory": nil},
		}))

		err := reg.Prepare(context.Background())
		if !errors.Is(err, pkgcore.ErrCapabilityUnsatisfied) {
			t.Fatalf("Prepare() error = %v, want ErrCapabilityUnsatisfied for eventbus.memory in a distributed composition", err)
		}
		if got := err.Error(); !strings.Contains(got, "eventbus.memory") || !strings.Contains(got, "MultiReplicaSafe") || !strings.Contains(got, "distributed") {
			t.Errorf("Prepare() error = %q, want it to name the component, the missing bit and the mode", got)
		}
	})

	t.Run("shared-backend implementations accepted", func(t *testing.T) {
		reg := pkgcore.NewComponentRegistry()
		reg.Put(pkgcore.NewComponentConfig(map[string]any{
			"deployment": "distributed",
			"components": map[string]any{
				"eventbus.redis": nil,
				"kv.redis":       nil,
				"mailer.smtp":    map[string]any{"host": "relay.assembly.test"},
				"objectstore.s3": map[string]any{
					"endpoint":   "objects.assembly.test",
					"bucket":     "assembly",
					"access_key": "assembly-key",
					"secret_key": "assembly-secret",
				},
				"sms.http": map[string]any{"endpoint": "https://gateway.assembly.test/sms"},
			},
		}))

		if err := reg.Prepare(context.Background()); err != nil {
			t.Fatalf("Prepare() error = %v, want the shared-backend set accepted in a distributed composition", err)
		}
	})
}
