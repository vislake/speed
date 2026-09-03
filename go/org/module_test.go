package org

import (
	"context"
	"embed"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestModule_Identity(t *testing.T) {
	m := NewModule(nil)

	if got := m.Name(); got != "org" {
		t.Errorf("Name() = %q, want %q", got, "org")
	}
	if got := m.DependsOn(); got != nil {
		t.Errorf("DependsOn() = %v, want nil -- org depends on no other pkgcore.Module, authn least of all", got)
	}
	if got := m.OpenAPISpec(); got != nil {
		t.Errorf("OpenAPISpec() = %q, want nil until the module's spec fragment lands", got)
	}
}

// TestModule_Migrations_ExposesBothDialectsAtTheEmbedRoot pins the layout
// dbkit.MigrationRegistry.Apply requires: a postgres/ and a sqlite/
// subdirectory at the FS root, each holding the same file names. A migration
// added to one dialect and forgotten in the other fails here rather than at a
// deployment that happens to run the other engine.
func TestModule_Migrations_ExposesBothDialectsAtTheEmbedRoot(t *testing.T) {
	fs := NewModule(nil).Migrations()

	names := map[string][]string{}
	for _, dialect := range []string{"postgres", "sqlite"} {
		entries, err := fs.ReadDir(dialect)
		if err != nil {
			t.Fatalf("ReadDir(%q): %v", dialect, err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s/ holds no migration files", dialect)
		}
		for _, e := range entries {
			names[dialect] = append(names[dialect], e.Name())
		}
	}
	if len(names["postgres"]) != len(names["sqlite"]) {
		t.Fatalf("dialect file counts differ: postgres %v, sqlite %v", names["postgres"], names["sqlite"])
	}
	for i, name := range names["postgres"] {
		if names["sqlite"][i] != name {
			t.Errorf("migration %q exists in postgres/ but sqlite/ has %q at the same position", name, names["sqlite"][i])
		}
	}
}

// TestModule_Locales_ShipsBothLanguages pins the i18n rule at the module
// boundary: exactly the two languages the catalog serves, no more and no
// fewer, since Kernel.Bootstrap rejects a module that ships a file for a
// language the others do not.
func TestModule_Locales_ShipsBothLanguages(t *testing.T) {
	entries, err := NewModule(nil).Locales().ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, want := range []string{"zh-CN.toml", "en-US.toml"} {
		if !got[want] {
			t.Errorf("locales are missing %s", want)
		}
	}
	if len(entries) != 2 {
		t.Errorf("locales hold %d files, want exactly zh-CN.toml and en-US.toml", len(entries))
	}
}

// TestModule_Register_DeclaresItsSurface bootstraps org through the real
// kernel -- the same path a host takes -- and asserts every declaration
// arrives on the registry. Bootstrapping rather than calling Register against
// a hand-built Registry is deliberate: it also proves org's locale files
// survive i18n.Builder.AddModule's parity validation, which only runs there.
func TestModule_Register_DeclaresItsSurface(t *testing.T) {
	reg, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).
		Bootstrap(context.Background(), NewModule(nil))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	t.Run("permissions", func(t *testing.T) {
		assertContainsAll(t, reg.Permissions.Permissions(), []string{
			PermissionRead, PermissionManage, PermissionInviteMember, PermissionRemoveMember,
		})
	})

	t.Run("audit actions", func(t *testing.T) {
		assertContainsAll(t, reg.AuditActions.Actions(), []string{
			AuditActionNodeCreate, AuditActionNodeRename, AuditActionNodeMove, AuditActionNodeDelete,
		})
	})

	t.Run("published events", func(t *testing.T) {
		var types []string
		for _, decl := range reg.Events.Published() {
			types = append(types, decl.Type)
			if decl.PayloadType == "" || decl.Description == "" {
				t.Errorf("event %q is declared without a payload type or description", decl.Type)
			}
		}
		assertContainsAll(t, types, []string{EventNodeCreated, EventNodeMoved, EventNodeDeleted})
	})

	t.Run("no routes are mounted before the spec fragment lands", func(t *testing.T) {
		if got := reg.Routes.Routes(); len(got) != 0 {
			t.Errorf("Register mounted %d route(s); org's HTTP surface is spec-first and lands with its fragment", len(got))
		}
	})

	t.Run("no configuration schema is declared", func(t *testing.T) {
		// org honours its bounds as package constants rather than declaring
		// a dynamic-config schema it cannot read back (it must not import
		// config). Declaring a schema nothing honours would be a lying
		// schema, so the absence is asserted rather than left implicit.
		if got := reg.Config.Items(); len(got) != 0 {
			t.Errorf("Register declared %d config item(s); org declares none it cannot honour", len(got))
		}
	})
}

// TestModule_Register_DoesNotDeclareAuthnsEvent is the guard for the trap
// that would only surface once the authn module ships: declaring another
// module's event type collides at bootstrap, so org must subscribe to
// authn.user.created without ever declaring it.
func TestModule_Register_DoesNotDeclareAuthnsEvent(t *testing.T) {
	reg, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).
		Bootstrap(context.Background(), NewModule(nil))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	for _, decl := range reg.Events.Published() {
		if decl.Type == "authn.user.created" {
			t.Fatal("org declared authn.user.created; only authn may declare it, or both modules collide at bootstrap")
		}
		if got := decl.Type[:4]; got != "org." {
			t.Errorf("org declared event %q, which is not in org's own namespace", decl.Type)
		}
	}
}

// TestModule_Register_CoexistsWithAnotherModule proves org bootstraps
// alongside a module that declares its own permissions, audit actions and
// events -- the real host shape -- rather than only in isolation.
func TestModule_Register_CoexistsWithAnotherModule(t *testing.T) {
	reg, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).
		Bootstrap(context.Background(), NewModule(nil), neighbourModule{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	assertContainsAll(t, reg.Permissions.Permissions(), []string{PermissionRead, "neighbour:read"})
}

// TestModule_Register_PerformsNoIO calls Register with a nil database, which
// is what pkgcore.Module's "it only declares" contract requires to be safe:
// any database call inside Register would panic here.
func TestModule_Register_PerformsNoIO(t *testing.T) {
	m := NewModule(nil)
	reg := pkgcore.NewRegistry(
		pkgcore.NewMemoryEventBus(),
		pkgcore.NewMemoryKVStore(),
		pkgcore.NewConsoleMailer(),
	)
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register against a nil database: %v", err)
	}
}

func TestModule_Tree_ReturnsAUsableService(t *testing.T) {
	m := NewModule(newTestDB(t))
	ctx := tenantCtx("tenant-a")

	root, err := m.Tree().CreateRoot(ctx, "Acme Dental", "group")
	if err != nil {
		t.Fatalf("CreateRoot through the module's tree: %v", err)
	}
	if root.ID == "" {
		t.Error("the module's tree produced a node with no id")
	}
	first, second := m.Tree(), m.Tree()
	if first != second {
		t.Error("Tree() returns a different service on each call; hosts hold onto it")
	}
}

// neighbourModule stands in for any other module a host bootstraps next to
// org. It declares a disjoint surface, so a collision would mean org claimed
// a name outside its namespace.
type neighbourModule struct{}

func (neighbourModule) Name() string         { return "neighbour" }
func (neighbourModule) DependsOn() []string  { return nil }
func (neighbourModule) Migrations() embed.FS { return embed.FS{} }
func (neighbourModule) Locales() embed.FS    { return embed.FS{} }
func (neighbourModule) OpenAPISpec() []byte  { return nil }
func (neighbourModule) Register(reg *pkgcore.Registry) error {
	if err := reg.Permissions.Add("neighbour:read"); err != nil {
		return err
	}
	return reg.AuditActions.Add("neighbour.thing.do")
}

var _ pkgcore.Module = neighbourModule{}

// assertContainsAll fails t unless got holds every entry in want.
func assertContainsAll(t *testing.T, got, want []string) {
	t.Helper()
	set := make(map[string]bool, len(got))
	for _, v := range got {
		set[v] = true
	}
	for _, v := range want {
		if !set[v] {
			t.Errorf("%q is missing from %v", v, got)
		}
	}
}
