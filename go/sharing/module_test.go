package sharing

import (
	"bytes"
	"context"
	"embed"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestModule_Identity(t *testing.T) {
	m := NewModule(nil)
	if got := m.Name(); got != moduleName {
		t.Errorf("Name() = %q, want %q", got, moduleName)
	}
	if got := m.DependsOn(); got != nil {
		t.Errorf("DependsOn() = %v, want nil -- sharing depends on no other pkgcore.Module in the bootstrap set", got)
	}
	spec := m.OpenAPISpec()
	if len(spec) == 0 {
		t.Errorf("OpenAPISpec() is empty, want sharing's embedded api/openapi.yaml fragment")
	}
	if !bytes.Contains(spec, []byte(PathAccess)) {
		t.Errorf("OpenAPISpec() does not mention PathAccess (%q); it should be this fragment's one operation path", PathAccess)
	}
}

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

// TestModule_Register_DeclaresItsSurface bootstraps sharing through the
// real kernel -- the same path a host takes -- and asserts every
// declaration arrives on the registry. Bootstrapping rather than calling
// Register against a hand-built Registry is deliberate: it also proves
// sharing's locale files survive i18n.Builder.AddModule's parity
// validation, which only runs there.
func TestModule_Register_DeclaresItsSurface(t *testing.T) {
	db := newTestDB(t)
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), NewModule(db))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	t.Run("permissions", func(t *testing.T) {
		assertContainsAll(t, reg.Permissions.Permissions(), []string{PermissionRead, PermissionCreate, PermissionRevoke})
	})

	t.Run("audit actions", func(t *testing.T) {
		assertContainsAll(t, reg.AuditActions.Actions(), []string{AuditActionSensitiveShareCreate})
	})

	t.Run("events", func(t *testing.T) {
		var types []string
		for _, e := range reg.Events.Published() {
			types = append(types, e.Type)
		}
		assertContainsAll(t, types, []string{EventShareCreated, EventShareAccessed, EventShareRevoked})
	})

	t.Run("config items", func(t *testing.T) {
		var keys []string
		for _, item := range reg.Config.Items() {
			keys = append(keys, item.Key)
			if item.Description == "" {
				t.Errorf("config item %q is declared without a description", item.Key)
			}
			if item.Sensitive {
				t.Errorf("config item %q is marked Sensitive; sharing declares no sensitive items", item.Key)
			}
		}
		assertContainsAll(t, keys, []string{ConfigDefaultExpiry})
	})

	t.Run("jobs handler registered", func(t *testing.T) {
		handlers := reg.Jobs.Handlers()
		if _, ok := handlers[taskTypeExpirySweep]; !ok {
			t.Errorf("expiry-sweep handler (%q) was not registered on reg.Jobs; got keys %v", taskTypeExpirySweep, handlers)
		}
	})

	t.Run("the public access route is mounted", func(t *testing.T) {
		routes := reg.Routes.Routes()
		if len(routes) != 1 {
			t.Fatalf("Register mounted %d route(s), want exactly 1 (PathAccess)", len(routes))
		}
		if routes[0].Path != PathAccess {
			t.Errorf("mounted route path = %q, want %q", routes[0].Path, PathAccess)
		}
		if routes[0].Handler == nil {
			t.Errorf("mounted route carries a nil Handler")
		}
	})
}

// TestModule_Register_MountsHandlerBuiltAfterOptions proves Register builds
// Handler from the module's final, post-Option state -- WithResourceResolver
// included -- not from whatever NewModule saw before every Option had run.
// A resolver injected this way must be the one a request reaching the
// mounted route actually uses.
func TestModule_Register_MountsHandlerBuiltAfterOptions(t *testing.T) {
	db := newTestDB(t)
	resolver := fakeResourceResolver{mime: "text/plain", body: "hello"}
	m := NewModule(db, WithResourceResolver(resolver))
	if _, err := pkgcore.NewKernel().Bootstrap(context.Background(), m); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if m.Handler() == nil {
		t.Fatalf("Handler() = nil after Register")
	}

	tenantCtx := pkgcore.WithTenant(context.Background(), pkgcore.TenantID("tenant-a"))
	created, err := m.Service().Create(tenantCtx, CreateParams{ResourceRef: "ref-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, PathAccess+"?token="+url.QueryEscape(created.Token), nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200 (the mounted Handler should use the resolver WithResourceResolver injected)", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "hello" {
		t.Errorf("body = %q, want %q", got, "hello")
	}
}

// TestModule_Register_CoexistsWithAnotherModule proves sharing bootstraps
// alongside a module that declares its own permissions and audit actions.
func TestModule_Register_CoexistsWithAnotherModule(t *testing.T) {
	reg, err := pkgcore.NewKernel().Bootstrap(context.Background(), NewModule(newTestDB(t)), neighbourModule{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	assertContainsAll(t, reg.Permissions.Permissions(), []string{PermissionCreate, "neighbour:read"})
}

// TestModule_Register_PerformsNoIO calls Register with a nil database,
// which is what pkgcore.Module's "it only declares" contract requires to be
// safe: any database call inside Register would panic here.
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

// TestModule_WithTenantConfigReader proves the Option wires through to the
// Service the module builds.
func TestModule_WithTenantConfigReader(t *testing.T) {
	cfg := fakeTenantConfigReader{d: 1234, ok: true}
	m := NewModule(newTestDB(t), WithTenantConfigReader(cfg))
	if m.Service().cfg == nil {
		t.Fatalf("Service().cfg is nil, want the wired TenantConfigReader")
	}
}

// assertContainsAll fails the test for every want element missing from got.
func assertContainsAll(t *testing.T, got []string, want []string) {
	t.Helper()
	set := make(map[string]bool, len(got))
	for _, s := range got {
		set[s] = true
	}
	for _, w := range want {
		if !set[w] {
			t.Errorf("missing %q; got %v", w, got)
		}
	}
}

// neighbourModule is a minimal second pkgcore.Module, used only to prove
// sharing bootstraps correctly alongside another module's own declarations.
type neighbourModule struct{}

func (neighbourModule) Name() string         { return "neighbour" }
func (neighbourModule) DependsOn() []string  { return nil }
func (neighbourModule) Migrations() embed.FS { return embed.FS{} }
func (neighbourModule) Locales() embed.FS    { return embed.FS{} }
func (neighbourModule) OpenAPISpec() []byte  { return nil }
func (neighbourModule) Register(reg *pkgcore.Registry) error {
	return reg.Permissions.Add("neighbour:read")
}

var _ pkgcore.Module = neighbourModule{}
