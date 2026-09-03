package rbac

import (
	"context"
	"embed"
	"reflect"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/i18n"
)

// newRBACTestDB returns a private SQLite database with this module's
// migrations applied from zero to head through the same
// dbkit.MigrationRegistry a host uses. It is this package's one shared
// test fixture; every test file in the package uses it rather than opening
// a connection of its own.
func newRBACTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbtest.NewSQLite(t)
	reg := dbkit.NewMigrationRegistry()
	if err := reg.Register(NewModule(db)); err != nil {
		t.Fatalf("registering the rbac migrations: %v", err)
	}
	if err := reg.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("applying the rbac migrations: %v", err)
	}
	return db
}

// newPlainRegistry returns a registry over throwaway in-memory seams, the
// way a host that never wired a real bus or KV store does.
func newPlainRegistry() *pkgcore.Registry {
	return pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
}

// declaringModule is a minimal pkgcore.Module standing in for a business
// module that declares permissions of its own. It is what makes the
// "Attach snapshots EVERY module's permissions, not just rbac's" assertion
// meaningful.
type declaringModule struct {
	name  string
	perms []string
}

var _ pkgcore.Module = (*declaringModule)(nil)

func (d *declaringModule) Name() string         { return d.name }
func (d *declaringModule) DependsOn() []string  { return nil }
func (d *declaringModule) Migrations() embed.FS { return embed.FS{} }
func (d *declaringModule) Locales() embed.FS    { return embed.FS{} }
func (d *declaringModule) OpenAPISpec() []byte  { return nil }
func (d *declaringModule) Register(reg *pkgcore.Registry) error {
	return reg.Permissions.Add(d.perms...)
}

func TestModule_Name_And_DependsOn(t *testing.T) {
	m := NewModule(nil)
	if m.Name() != "rbac" {
		t.Fatalf("Name() = %q, want %q", m.Name(), "rbac")
	}
	// The defining boundary rule of this module: authorization never
	// depends on authentication. A DependsOn entry naming authn would
	// invert the design, so the empty list is asserted rather than assumed.
	if got := m.DependsOn(); len(got) != 0 {
		t.Fatalf("DependsOn() = %v, want no dependencies (never authn)", got)
	}
}

func TestModule_Register_DeclaresItsOwnPermissions(t *testing.T) {
	reg := newPlainRegistry()
	if err := NewModule(nil).Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	want := []string{PermissionManage, PermissionRead} // the registrar returns them sorted
	if got := reg.Permissions.Permissions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("declared permissions = %v, want %v", got, want)
	}
}

func TestModule_Register_DeclaresItsEventsAndAuditActions(t *testing.T) {
	reg := newPlainRegistry()
	if err := NewModule(nil).Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	published := reg.Events.Published()
	if len(published) != 3 {
		t.Fatalf("declared %d events, want 3", len(published))
	}
	wantTypes := []string{EventRoleBindingAssigned, EventRoleBindingRevoked, EventRoleChanged}
	for i, want := range wantTypes {
		if published[i].Type != want {
			t.Fatalf("event %d = %q, want %q", i, published[i].Type, want)
		}
		if published[i].PayloadType == "" {
			t.Fatalf("event %q declares no payload type", want)
		}
	}

	wantActions := []string{AuditActionRoleAssign, AuditActionRoleDefine, AuditActionRoleRevoke} // sorted
	if got := reg.AuditActions.Actions(); !reflect.DeepEqual(got, wantActions) {
		t.Fatalf("declared audit actions = %v, want %v", got, wantActions)
	}
}

func TestModule_Register_MountsNoRoutes(t *testing.T) {
	// rbac deliberately exposes no HTTP surface of its own: role
	// management belongs to the admin console and the flat permission list
	// belongs to authn's /me. A route appearing here would mean one of
	// those deferrals was quietly reversed.
	reg := newPlainRegistry()
	if err := NewModule(nil).Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := reg.Routes.Routes(); len(got) != 0 {
		t.Fatalf("Register mounted %d routes, want none", len(got))
	}
	if spec := NewModule(nil).OpenAPISpec(); spec != nil {
		t.Fatalf("OpenAPISpec() = %d bytes, want nil", len(spec))
	}
}

func TestModule_Register_PerformsNoIO(t *testing.T) {
	// pkgcore.Module's contract: Register declares, it never performs I/O.
	// A nil database is the sharpest possible proof -- any query would
	// panic rather than merely fail.
	if err := NewModule(nil).Register(newPlainRegistry()); err != nil {
		t.Fatalf("Register on a nil-database module: %v", err)
	}
}

func TestModule_Attach_FreezesEveryModulesPermissions(t *testing.T) {
	// The catalog must be the WHOLE host's declaration, not rbac's own:
	// a grant of "notes:read" is legal exactly because the notes module
	// declared it. Bootstrap is what guarantees every module has
	// registered by the time Attach reads the registrar.
	m := NewModule(newRBACTestDB(t))
	host := &declaringModule{name: "notes", perms: []string{"notes:read", "notes:write"}}

	reg, err := pkgcore.NewKernel(pkgcore.DeploymentModeStandalone).Bootstrap(context.Background(), m, host)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	svc, err := m.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	for _, perm := range []string{"notes:read", "notes:write", PermissionRead, PermissionManage} {
		if !svc.catalog.Has(perm) {
			t.Fatalf("the frozen catalog does not know %q, which a module declared", perm)
		}
	}
	if svc.catalog.Has("notes:delete") {
		t.Fatal("the frozen catalog knows a permission no module declared")
	}
}

func TestModule_Attach_TwiceReportsAlreadyAttached(t *testing.T) {
	// The catalog freezes at the first Attach. A second snapshot could
	// silently differ, and for the set that decides whether a grant is
	// legal, that is a security difference rather than a cosmetic one.
	m := NewModule(newRBACTestDB(t))
	reg := newPlainRegistry()
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := m.Attach(reg); err != nil {
		t.Fatalf("first Attach: %v", err)
	}

	svc, err := m.Attach(reg)
	if svc != nil {
		t.Fatal("the second Attach returned a Service alongside its error")
	}
	assertErrorCode(t, err, "rbac.already_attached")
}

func TestModule_Attach_RejectsMissingWiring(t *testing.T) {
	if _, err := NewModule(newRBACTestDB(t)).Attach(nil); err == nil {
		t.Fatal("Attach with a nil registry succeeded")
	}
	m := NewModule(nil)
	reg := newPlainRegistry()
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := m.Attach(reg); err == nil {
		t.Fatal("Attach with a nil database succeeded")
	}
}

// stubResolver is a SubtreeResolver that answers nothing. It exists only
// to prove the option carries the host's implementation onto the Service;
// what a resolver's answers mean is the evaluation block's concern.
type stubResolver struct{}

func (stubResolver) NodePath(context.Context, string) (string, bool, error) { return "", false, nil }

func TestModule_Options_ReachTheService(t *testing.T) {
	resolver := stubResolver{}
	m := NewModule(newRBACTestDB(t), WithSubtreeResolver(resolver), WithCacheTTL(5*time.Second))
	reg := newPlainRegistry()
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	svc, err := m.Attach(reg)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	if svc.subtree != SubtreeResolver(resolver) {
		t.Fatalf("the wired SubtreeResolver did not reach the Service (got %#v)", svc.subtree)
	}
	if svc.cacheTTL != 5*time.Second {
		t.Fatalf("cacheTTL = %v, want 5s", svc.cacheTTL)
	}
	if svc.bus == nil {
		t.Fatal("the Service carries no event bus")
	}
	if svc.roles == nil || svc.rolePermissions == nil || svc.bindings == nil {
		t.Fatal("the Service is missing one of its three repositories")
	}
}

func TestModule_NoSubtreeResolver_IsASupportedConfiguration(t *testing.T) {
	// A host with no organization module must still boot. Only node-scoped
	// bindings need a resolver, and those deny without one -- they never
	// widen to the tenant.
	m := NewModule(newRBACTestDB(t))
	reg := newPlainRegistry()
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	svc, err := m.Attach(reg)
	if err != nil {
		t.Fatalf("Attach without a resolver: %v", err)
	}
	if svc.subtree != nil {
		t.Fatalf("subtree = %#v, want nil when no host wired one", svc.subtree)
	}
}

func TestWithCacheTTL_IgnoresNonPositiveValues(t *testing.T) {
	// A zero passed by accident must leave the safe default in place
	// rather than producing a cache that never expires.
	for _, ttl := range []time.Duration{0, -time.Second} {
		m := NewModule(nil, WithCacheTTL(ttl))
		if m.cacheTTL != DefaultCacheTTL {
			t.Fatalf("WithCacheTTL(%v) set cacheTTL = %v, want the default %v", ttl, m.cacheTTL, DefaultCacheTTL)
		}
	}
}

func TestModule_Migrations_ApplyFromZeroToHeadOnSQLite(t *testing.T) {
	db := newRBACTestDB(t)
	for _, table := range []string{"rbac_roles", "rbac_role_permissions", "rbac_role_bindings"} {
		if !db.Migrator().HasTable(table) {
			t.Fatalf("table %q does not exist after applying the migrations from zero", table)
		}
	}

	// Applying again must be a no-op, not a duplicate-table error: a host
	// runs Apply on every start.
	reg := dbkit.NewMigrationRegistry()
	if err := reg.Register(NewModule(db)); err != nil {
		t.Fatalf("re-registering: %v", err)
	}
	if err := reg.Apply(context.Background(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("re-applying the migrations: %v", err)
	}
}

func TestModule_Locales_ParityAndCoverage(t *testing.T) {
	// The parity rule enforced through the very mechanism Kernel.Bootstrap
	// uses: AddModule fails with ErrParityMismatch when the two language
	// files' id sets differ, and with ErrUnsupportedShape on a grouping
	// section or an id missing the module prefix.
	builder := i18n.NewBuilder()
	if err := builder.AddModule(moduleName, NewModule(nil).Locales()); err != nil {
		t.Fatalf("merging rbac's locale files: %v", err)
	}
	catalog := builder.Build()

	// Every exported error code must resolve in every language the
	// catalog serves. A code with no message renders as the raw code in
	// the UI, which is exactly what the i18n rule exists to prevent.
	codes := []string{
		ErrUnknownPermission.Code,
		ErrRoleNotFound.Code,
		ErrDuplicateRole.Code,
		ErrBindingNotFound.Code,
		ErrSubjectRequired.Code,
		ErrPermissionDenied.Code,
		ErrSubtreeUnresolved.Code,
		ErrServiceNotAttached.Code,
		ErrAlreadyAttached.Code,
		ErrStorage.Code,
	}
	locales := catalog.Locales()
	if len(locales) == 0 {
		t.Fatal("the catalog serves no locales")
	}
	for _, locale := range locales {
		for _, code := range codes {
			msg, err := catalog.Lookup(locale, code, nil)
			if err != nil {
				t.Fatalf("Lookup(%q, %q): %v", locale, code, err)
			}
			if msg == "" {
				t.Fatalf("Lookup(%q, %q) returned an empty message", locale, code)
			}
		}
	}
}

func TestErrors_CodesArePrefixedWithTheModuleName(t *testing.T) {
	// Backend coding standard §6.2: every error code is "<module>.<reason>".
	// The i18n contract depends on it too -- pkgcore/i18n rejects a message
	// id that does not start with the module name.
	errs := []*apperr.Error{
		ErrUnknownPermission, ErrRoleNotFound, ErrDuplicateRole, ErrBindingNotFound,
		ErrSubjectRequired, ErrPermissionDenied, ErrSubtreeUnresolved,
		ErrServiceNotAttached, ErrAlreadyAttached, ErrStorage,
	}
	seen := make(map[string]bool, len(errs))
	for _, err := range errs {
		if !strings.HasPrefix(err.Code, moduleName+".") {
			t.Fatalf("error code %q is not prefixed with %q", err.Code, moduleName+".")
		}
		if seen[err.Code] {
			t.Fatalf("error code %q is declared twice", err.Code)
		}
		seen[err.Code] = true
		if err.Status == 0 {
			t.Fatalf("error %q carries no HTTP status", err.Code)
		}
	}
}
