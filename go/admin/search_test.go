package admin

import (
	"context"
	"crypto"
	"embed"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/admin/internal/testutil"
	"github.com/vislake/speed/go/authn"
	authnmigrations "github.com/vislake/speed/go/authn/migrations"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/org"
	orgmigrations "github.com/vislake/speed/go/org/migrations"
	"github.com/vislake/speed/go/pkgcore"
)

// testBlindIndexKey is a fixed 32-byte key for these tests' own blind
// indexers -- not a secret, since nothing here persists across processes.
var testBlindIndexKey = []byte("admin-test-blind-index-key-32byt")

// noopKeySource is an authn.KeySource that is never actually called by
// anything these tests exercise (Register and SearchUsers mint no
// tokens): authn.NewModule requires a non-nil KeySource at construction,
// so this satisfies that requirement without needing go/pki or a real
// signing key.
type noopKeySource struct{}

func (noopKeySource) EnsurePurpose(context.Context, string, string, time.Duration) error {
	return errors.New("noopKeySource: unexpectedly called")
}

func (noopKeySource) ActiveSigner(context.Context, string) (string, string, func(context.Context, []byte) ([]byte, error), error) {
	return "", "", nil, errors.New("noopKeySource: unexpectedly called")
}

func (noopKeySource) VerificationKeys(context.Context, string) ([]struct {
	KID       string
	Algorithm string
	Public    crypto.PublicKey
}, error,
) {
	return nil, errors.New("noopKeySource: unexpectedly called")
}

// orgMigrationModule is the minimal pkgcore.Module these tests feed to
// dbkit.MigrationRegistry to apply org's real migrations, mirroring
// go/authn/internal/testutil's identical migrationModule idiom -- building
// a real org.Module here would not itself apply migrations (that is
// always the host's job, per every module's own Register doc comment).
type orgMigrationModule struct{}

func (orgMigrationModule) Name() string                     { return "org" }
func (orgMigrationModule) DependsOn() []string              { return nil }
func (orgMigrationModule) Migrations() embed.FS             { return orgmigrations.FS }
func (orgMigrationModule) Locales() embed.FS                { return embed.FS{} }
func (orgMigrationModule) OpenAPISpec() []byte              { return nil }
func (orgMigrationModule) Register(*pkgcore.Registry) error { return nil }

// authnMigrationModule mirrors orgMigrationModule for authn's own
// migration files.
type authnMigrationModule struct{}

func (authnMigrationModule) Name() string                     { return "authn" }
func (authnMigrationModule) DependsOn() []string              { return nil }
func (authnMigrationModule) Migrations() embed.FS             { return authnmigrations.FS }
func (authnMigrationModule) Locales() embed.FS                { return embed.FS{} }
func (authnMigrationModule) OpenAPISpec() []byte              { return nil }
func (authnMigrationModule) Register(*pkgcore.Registry) error { return nil }

// authnPIICipherOnce registers authn's PII serializer exactly once per test
// binary: dbkit's serializer registry is process-global, and GORM resolves
// a model's serializer while parsing its schema, so this must run before
// the first authnTestDB call opens a handle.
var authnPIICipherOnce = func() error {
	cipher, err := dbkit.NewCipher(testBlindIndexKey)
	if err != nil {
		return err
	}
	return authn.RegisterPIISerializer(cipher)
}()

// authnTestDB returns a fresh in-memory SQLite database with authn's real
// migrations applied from zero.
func authnTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	if authnPIICipherOnce != nil {
		t.Fatalf("register authn's PII serializer: %v", authnPIICipherOnce)
	}
	db := testutil.NewDB(t)
	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(authnMigrationModule{}); err != nil {
		t.Fatalf("register authn's migrations: %v", err)
	}
	if err := registry.Apply(t.Context(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply authn's migrations: %v", err)
	}
	return db
}

// newTestOrgModule returns a real *org.Module (no HTTP, no invitations --
// none of these tests need them) over a fresh database with org's own
// migrations applied.
func newTestOrgModule(t *testing.T) *org.Module {
	t.Helper()
	db := testutil.NewDB(t) // admin's own migrations -- irrelevant here, but a valid dbkit.Open-shaped *gorm.DB works for any dialect-agnostic schema; org needs its OWN tables too.
	registry := dbkit.NewMigrationRegistry()
	if err := registry.Register(orgMigrationModule{}); err != nil {
		t.Fatalf("register org's migrations: %v", err)
	}
	if err := registry.Apply(t.Context(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply org's migrations: %v", err)
	}
	return org.NewModule(db)
}

// newTestAuthnService returns a real *authn.Service backed by its own
// fresh database, for SearchService.Users' passthrough proof.
func newTestAuthnService(t *testing.T) *authn.Service {
	t.Helper()
	db := authnTestDB(t)
	module, err := authn.NewModule(db,
		authn.WithBlindIndexKey(testBlindIndexKey),
		authn.WithKeySource(noopKeySource{}),
	)
	if err != nil {
		t.Fatalf("authn.NewModule() error = %v", err)
	}
	// Service() is nil until Register has run (it needs the event bus and
	// key-value store the registry carries) -- see Module.Service's own
	// doc comment.
	if err := module.Register(newTestRegistry()); err != nil {
		t.Fatalf("authn Register() error = %v", err)
	}
	return module.Service()
}

func TestSearchService_Users_DelegatesToAuthn(t *testing.T) {
	authnSvc := newTestAuthnService(t)
	orgModule := newTestOrgModule(t)
	tenants := NewTenantService(NewTenantRepository(testutil.NewDB(t)))
	search := NewSearchService(authnSvc, orgModule.Members(), tenants)

	ctx := context.Background()
	if _, err := authnSvc.Register(ctx, authn.RegisterInput{
		Email: "search-target@example.com", Password: "a perfectly fine passphrase", DisplayName: "Search Target",
	}); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	got, err := search.Users(ctx, authn.UserSearchQuery{Email: "search-target@example.com"})
	if err != nil {
		t.Fatalf("Users() error = %v", err)
	}
	if len(got) != 1 || got[0].DisplayName != "Search Target" {
		t.Fatalf("Users() = %+v, want exactly Search Target", got)
	}
}

// TestSearchService_MembershipsOf_ListsOnlyTenantsWithActiveMembership is
// D6+D2's core proof: the answer is drawn from admin's own D3 ledger, and
// only tenants where the user actually has a membership are reported.
func TestSearchService_MembershipsOf_ListsOnlyTenantsWithActiveMembership(t *testing.T) {
	pkgcore.RegisterSystemPurpose(SystemPurposeAdminCrossTenant)

	orgModule := newTestOrgModule(t)
	tenantDB := testutil.NewDB(t)
	tenantRepo := NewTenantRepository(tenantDB)
	tenants := NewTenantService(tenantRepo)
	reg := newTestRegistry()

	search := NewSearchService(nil, orgModule.Members(), tenants)
	search.attach(reg.EventBus())

	ctx := context.Background()
	// tenant-a: the user IS a member.
	if err := tenantRepo.Create(ctx, &Tenant{TenantID: "tenant-a"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	tenantACtx := pkgcore.WithTenant(ctx, "tenant-a")
	root, err := orgModule.Tree().CreateRoot(tenantACtx, "Acme", "workspace")
	if err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}
	if _, addErr := orgModule.Members().Add(tenantACtx, "user-1", root.ID); addErr != nil {
		t.Fatalf("Members().Add() error = %v", addErr)
	}

	// tenant-b: registered in the ledger, but the user has NO membership.
	if createErr := tenantRepo.Create(ctx, &Tenant{TenantID: "tenant-b"}); createErr != nil {
		t.Fatalf("Create() error = %v", createErr)
	}
	tenantBCtx := pkgcore.WithTenant(ctx, "tenant-b")
	if _, rootErr := orgModule.Tree().CreateRoot(tenantBCtx, "Globex", "workspace"); rootErr != nil {
		t.Fatalf("CreateRoot() error = %v", rootErr)
	}

	got, err := search.MembershipsOf(ctx, "user-1", "operator-1")
	if err != nil {
		t.Fatalf("MembershipsOf() error = %v", err)
	}
	if len(got) != 1 || got[0] != "tenant-a" {
		t.Fatalf("MembershipsOf() = %+v, want exactly [tenant-a]", got)
	}
}
