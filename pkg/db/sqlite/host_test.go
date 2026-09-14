package sqlite_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/vislake/speed/pkg/config"
	jsonformat "github.com/vislake/speed/pkg/config/format/json"
	filesource "github.com/vislake/speed/pkg/config/source/file"
	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/db"
	"github.com/vislake/speed/pkg/db/sqlite"
)

// hostIdentityPrefix names the environment variables a case's configuration
// would come from. Each case writes its own JSON primary source; the prefix
// keeps it from picking up a variable meant for another assembly.
const hostIdentityPrefix = "SPEEDDBSQLITETEST"

// createWidgets is a migration that fails loudly if it is ever applied twice:
// there is no IF NOT EXISTS on it.
const createWidgets = `CREATE TABLE widgets (id INTEGER NOT NULL, PRIMARY KEY (id))`

// TestTheSQLiteModuleDeliversTheCapability starts a real assembly around this
// subpackage's module and looks at what it delivered.
//
// Everything about it is real: the configuration loader over a JSON primary
// source, the registry, the lifecycle driver and a dependant that resolves the
// capability the way one does. Going through all of it is the point — a
// descriptor whose callbacks are wired to each other, whose dialect names the
// migration subdirectory and whose driver reads the configured locator cannot
// be told from one that only looks right by any case that examines the
// descriptor alone, and the difference shows up as tables that never appear.
//
// The migration is declared under the dialect's own subdirectory, so a module
// reporting the wrong dialect applies nothing and is caught here rather than at
// a host's first start.
func TestTheSQLiteModuleDeliversTheCapability(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "delivered.db")
	body := fmt.Sprintf(`{"db":{"sqlite":{"dsn":%q}}}`, dsn)

	probe := newProbe()
	reg := assembly(t, body,
		sqlite.Module(),
		core.Module{Name: "catalog", Resources: []any{db.Migrations{FS: fstest.MapFS{
			"sqlite/0001_create_widgets.sql": &fstest.MapFile{Data: []byte(createWidgets)},
		}}}},
		probe.module(),
	)

	// The loader reads os.Args unconditionally and a test binary always
	// carries arguments of its own, so they are taken away for the run. The
	// cases here therefore do not run in parallel.
	previous := os.Args
	os.Args = []string{"sqlite.test"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reg.Run(ctx) }()

	var instance db.Database
	select {
	case instance = <-probe.delivered:
	case err := <-done:
		os.Args = previous
		t.Fatalf("the assembly did not deliver the database capability: %v", err)
	}
	select {
	case err := <-done:
		os.Args = previous
		t.Fatalf("the assembly shut down before its Init stage: %v", err)
	case <-probe.migrated:
	}
	os.Args = previous

	if got := instance.Dialect(); got != db.SQLite {
		t.Errorf("the delivered capability reports the dialect %q, want %q", got, db.SQLite)
	}
	if _, err := os.Stat(dsn); err != nil {
		t.Errorf("no database was opened at the configured locator: %v", err)
	}
	if !instance.DB().Migrator().HasTable("widgets") {
		t.Error("the migration this assembly declared under the sqlite subdirectory was not applied, " +
			"so either the migration stage is not wired to this module or its dialect does not name " +
			"that subdirectory")
	}

	close(probe.gate)
	cancel()
	if err := <-done; err != nil {
		t.Errorf("the assembly shut down with an error: %v", err)
	}
}

// load builds the reader a real assembly hands its modules: the actual
// configuration loader over a JSON primary source holding body.
//
// It is the loader itself rather than a stand-in, because what the case above
// it observes belongs to the loader — the manifest accepts exactly the keys the
// module declares, and an undeclared one is refused there. A stand-in reader
// handing back a struct the test wrote would agree with any declaration at all.
func load(t *testing.T, body string, modules ...core.Module) (config.Reader, error) {
	t.Helper()
	reg := assembly(t, body, modules...)

	previous := os.Args
	os.Args = []string{"sqlite.test"}
	defer func() { os.Args = previous }()

	instance, err := config.Module().New(t.Context(), reg)
	if err != nil {
		return nil, err
	}
	reader, ok := instance.(config.Reader)
	if !ok {
		t.Fatalf("the config module produced %T, want a config.Reader", instance)
	}
	return reader, nil
}

// assembly builds the registry a case runs on: the configuration loader, the
// host identity that locates its primary source, and the modules the case adds.
func assembly(t *testing.T, body string, modules ...core.Module) *core.Registry {
	t.Helper()
	locator := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(locator, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the primary config source: %v", err)
	}

	reg := core.New()
	reg.Register(config.Module())
	reg.Register(jsonformat.Module())
	reg.Register(filesource.Module())
	reg.Register(core.Module{
		Name:      "host",
		Resources: []any{config.HostIdentity{Prefix: hostIdentityPrefix, DefaultLocator: "file://" + locator}},
	})
	for _, module := range modules {
		reg.Register(module)
	}
	return reg
}

// probe is a module that takes up the database capability the way a dependant
// does, and holds the lifecycle open while a case looks at what it took up.
//
// It takes the capability in its own New, which is where a dependant's first
// use of it sits, and reports reaching Init: Init runs after Migrate, so a host
// whose Init has been reached has its migrations applied, and a case that
// looked earlier would be racing the stage it means to observe.
type probe struct {
	delivered chan db.Database
	migrated  chan struct{}
	gate      chan struct{}
}

func newProbe() *probe {
	return &probe{
		delivered: make(chan db.Database, 1),
		migrated:  make(chan struct{}),
		gate:      make(chan struct{}),
	}
}

func (p *probe) module() core.Module {
	return core.Module{
		Name:     "probe",
		Requires: []core.Requirement{{Token: (*db.Database)(nil)}},
		New: func(_ context.Context, reg *core.Registry) (any, error) {
			instance, err := core.Resolve[db.Database](reg)
			if err != nil {
				return nil, err
			}
			p.delivered <- instance
			return nil, nil
		},
		Init: func(context.Context, *core.Registry, any) error {
			close(p.migrated)
			<-p.gate
			return nil
		},
	}
}
