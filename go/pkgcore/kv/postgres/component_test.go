package postgres

import (
	"context"
	"embed"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// postgresDSN is a well-formed DSN pointing nowhere: the pool parses it at
// construction and dials lazily.
const postgresDSN = "postgres://assembly:assembly@127.0.0.1:5432/assembly?sslmode=disable"

// TestComponent_WellFormed runs the descriptor through the component
// contract: the naming convention, the decodable schema, the typed tokens.
func TestComponent_WellFormed(t *testing.T) {
	t.Parallel()
	componenttest.AssertWellFormed(t, kvPostgresComponent)
}

// TestComponent_DeclaresTheExportedCapabilities pins the descriptor's
// declaration to the package's exported constant, so the bits a host reads
// off this package and the bits the assembly validates cannot drift.
func TestComponent_DeclaresTheExportedCapabilities(t *testing.T) {
	t.Parallel()
	if kvPostgresComponent.Capabilities != Capabilities {
		t.Errorf("component capabilities = %v, want the exported Capabilities constant %v", kvPostgresComponent.Capabilities, Capabilities)
	}
}

// TestComponent_CarriesTheEntryTableMigrations pins the asset declaration:
// the component carries this package's postgres-only migration set, so the
// assembly's database component applies the entry table from the component
// itself instead of a host calling EnsureSchema by hand.
func TestComponent_CarriesTheEntryTableMigrations(t *testing.T) {
	t.Parallel()
	var zeroFS embed.FS
	if kvPostgresComponent.Migrations == zeroFS {
		t.Error("component carries no Migrations, want the package's entry-table migration set")
	}
}

// TestComponent_ConstructsThroughTheRegistry selects the component in a real
// assembly and drives Prepare and Construct: nothing is dialed (pgxpool
// connects lazily), the product resolves through Get, the entry-table
// migration set is visible to the database component through Assets, and
// Close releases the pool the component built.
func TestComponent_ConstructsThroughTheRegistry(t *testing.T) {
	ctx := context.Background()
	reg := pkgcore.NewComponentRegistry()
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"kv.postgres": map[string]any{"dsn": postgresDSN},
		},
	}))

	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := reg.Construct(ctx); err != nil {
		t.Fatalf("Construct() error = %v", err)
	}
	if _, err := pkgcore.Get[pkgcore.KVStore](reg); err != nil {
		t.Errorf("Get[pkgcore.KVStore] error = %v, want the constructed store", err)
	}
	var zeroFS embed.FS
	found := false
	for _, asset := range pkgcore.Assets(reg) {
		if asset.Name == "kv.postgres" && asset.Migrations != zeroFS {
			found = true
		}
	}
	if !found {
		t.Error("Assets(reg) carries no migrations for kv.postgres, want the entry-table set")
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := reg.Close(closeCtx); err != nil {
		t.Fatalf("Close() error = %v, want the built pool released", err)
	}
}
