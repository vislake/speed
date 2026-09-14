package db_test

import (
	"embed"
	"io/fs"
	"testing"
	"testing/fstest"

	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/db"
)

// TestUnrelatedEmbedFSIsNotCollectedAsMigrations is why Migrations is a struct
// and not fs.FS.
//
// A resource query matches an interface target by assignability, so an fs.FS
// target would collect every embedded asset in the process: an i18n bundle and
// a template set are declared as embed.FS just like a migration set is. The
// wrapper makes the declaration deliberate, and this test is the observation
// that tells the two implementations apart — with the wrapper the unrelated
// assets are invisible here, with an fs.FS target they are collected and this
// module would try to apply them.
//
// The registry also holds one genuine Migrations declaration. Without it the
// test would pass on a query that returns nothing at all, and "collected
// nothing" would prove nothing.
func TestUnrelatedEmbedFSIsNotCollectedAsMigrations(t *testing.T) {
	// The type is what the query matches on, and this is the very type an
	// //go:embed declaration of an asset directory produces.
	var i18nAssets embed.FS

	reg := core.New()
	reg.Register(core.Module{
		Name:      "i18n",
		Resources: []any{i18nAssets},
	})
	reg.Register(core.Module{
		Name: "templates",
		Resources: []any{fstest.MapFS{
			"mail/welcome.tmpl":              &fstest.MapFile{Data: []byte("hello {{.Name}}")},
			"sqlite/0001_looks_like_one.sql": &fstest.MapFile{Data: []byte("CREATE TABLE t (id INTEGER);")},
		}},
	})
	reg.Register(core.Module{
		Name: "billing",
		Resources: []any{db.Migrations{FS: fstest.MapFS{
			"sqlite/0001_create_invoices.sql": &fstest.MapFile{Data: []byte("CREATE TABLE invoices (id INTEGER);")},
		}}},
	})

	found := core.Resources[db.Migrations](reg)
	if len(found) != 1 {
		names := make([]string, 0, len(found))
		for _, res := range found {
			names = append(names, res.Module)
		}
		t.Fatalf("collected %d migration sets from %v, want exactly the one \"billing\" declared: "+
			"an unrelated embedded asset was taken for a migration set", len(found), names)
	}
	if found[0].Module != "billing" {
		t.Errorf("migration set attributed to module %q, want \"billing\"", found[0].Module)
	}
	if _, err := fs.Stat(found[0].Value.FS, "sqlite/0001_create_invoices.sql"); err != nil {
		t.Errorf("the collected set is not the one billing declared: %v", err)
	}

	// What the rejected design would have collected from this same registry.
	// Without this, "collected only billing's" could be read as a property of
	// the assets rather than of the resource type; here the hazard is visible.
	byInterface := core.Resources[fs.FS](reg)
	if len(byInterface) != 2 {
		t.Errorf("an fs.FS target collects %d of these declarations, expected both unrelated assets: "+
			"the premise of the wrapper is that they are assignable to fs.FS and a migration set is not", len(byInterface))
	}
}

// probePlugin is a gorm.Plugin declared on its own, the way a module would
// declare it if the resource type were the interface instead of the wrapper.
type probePlugin struct{ name string }

func (p probePlugin) Name() string              { return p.name }
func (p probePlugin) Initialize(*gorm.DB) error { return nil }

// TestBareGormPluginIsNotCollectedAsPluginResource is the same observation for
// the other resource type. The strength differs — Initialize(*gorm.DB) error is
// not a signature something satisfies by accident — but the declaration shape
// is the contract, and a bare plugin value is not a declaration.
func TestBareGormPluginIsNotCollectedAsPluginResource(t *testing.T) {
	reg := core.New()
	reg.Register(core.Module{
		Name:      "audit",
		Resources: []any{probePlugin{name: "audit"}},
	})
	reg.Register(core.Module{
		Name:      "tenancy",
		Resources: []any{db.Plugin{Plugin: probePlugin{name: "tenancy"}}},
	})

	found := core.Resources[db.Plugin](reg)
	if len(found) != 1 {
		t.Fatalf("collected %d plugin declarations, want exactly the one \"tenancy\" declared", len(found))
	}
	if found[0].Module != "tenancy" {
		t.Errorf("plugin attributed to module %q, want \"tenancy\"", found[0].Module)
	}
	if got := found[0].Value.Plugin.Name(); got != "tenancy" {
		t.Errorf("collected plugin names itself %q, want \"tenancy\"", got)
	}
}

// TestDialectValuesAreUsableAsMigrationSubdirectories pins the other half of
// the Migrations contract: a dialect value is the name of the subdirectory its
// files live in, so it has to be a legal single path element for io/fs. A value
// like "PostgreSQL 17" or "engines/postgres" would compile and read fine and
// then find no migrations at all.
func TestDialectValuesAreUsableAsMigrationSubdirectories(t *testing.T) {
	cases := map[db.Dialect]string{
		db.Postgres: "postgres",
		db.SQLite:   "sqlite",
	}
	for dialect, want := range cases {
		if string(dialect) != want {
			t.Errorf("dialect value is %q, want %q", string(dialect), want)
		}
		if !fs.ValidPath(string(dialect)) {
			t.Errorf("dialect %q is not a valid io/fs path, so it cannot name a subdirectory", string(dialect))
		}
		set := fstest.MapFS{
			string(dialect) + "/0001_init.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
		}
		sub, err := fs.Sub(set, string(dialect))
		if err != nil {
			t.Errorf("dialect %q cannot be opened as a subdirectory: %v", string(dialect), err)
			continue
		}
		if _, err := fs.Stat(sub, "0001_init.sql"); err != nil {
			t.Errorf("dialect %q does not address its own subdirectory: %v", string(dialect), err)
		}
	}
	if db.Postgres == db.SQLite {
		t.Error("the two dialects share one value, so their migration subdirectories would collide")
	}
}
