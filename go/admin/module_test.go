package admin

import (
	"context"
	"embed"
	"testing"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/notification"
	notificationmigrations "github.com/vislake/speed/go/notification/migrations"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
)

// fakeUserAddressResolver reports no addresses for anyone -- enough for
// these tests, which never need a delivery to actually reach a transport,
// only for Dispatch to enqueue successfully.
type fakeUserAddressResolver struct{}

func (fakeUserAddressResolver) Resolve(context.Context, string) (notification.UserAddresses, error) {
	return notification.UserAddresses{}, nil
}

// notificationMigrationModule mirrors orgMigrationModule for notification's
// own migration files.
type notificationMigrationModule struct{}

func (notificationMigrationModule) Name() string                     { return "notification" }
func (notificationMigrationModule) DependsOn() []string              { return nil }
func (notificationMigrationModule) Migrations() embed.FS             { return notificationmigrations.FS }
func (notificationMigrationModule) Locales() embed.FS                { return embed.FS{} }
func (notificationMigrationModule) OpenAPISpec() []byte              { return nil }
func (notificationMigrationModule) Register(*pkgcore.Registry) error { return nil }

// buildTestAdminModule wires a full, real module graph (authn, org,
// compliance, notification, admin) over one shared database and returns
// the bootstrapped registry together with admin's own Module -- the
// end-to-end proof that Register's four ErrXRequired checks are satisfied
// by a correctly wired host, and that every declaration (permissions,
// audit actions, the notification type, the event subscription) succeeds
// together.
func buildTestAdminModule(t *testing.T) (*pkgcore.Registry, *Module, *org.Module, *jobs.StandaloneQueue) {
	t.Helper()
	pkgcore.RegisterSystemPurpose(SystemPurposeAdminCrossTenant)

	db := authnTestDB(t) // already carries admin's own migrations too (testutil.NewDB is layered underneath).

	registry := dbkit.NewMigrationRegistry()
	for _, m := range []pkgcore.Module{orgMigrationModule{}, auditMigrationModule{}, notificationMigrationModule{}} {
		if err := registry.Register(m); err != nil {
			t.Fatalf("register %s migrations: %v", m.Name(), err)
		}
	}
	if err := registry.Apply(t.Context(), db, dbkit.DialectSQLite); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	authnModule, err := authn.NewModule(db,
		authn.WithBlindIndexKey(testBlindIndexKey),
		authn.WithKeySource(noopKeySource{}),
	)
	if err != nil {
		t.Fatalf("authn.NewModule() error = %v", err)
	}

	emailIndexer, err := dbkit.NewBlindIndexer("org_email_index", testBlindIndexKey, dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("build org email indexer: %v", err)
	}
	orgModule := org.NewModule(db, org.WithEmailIndexer(emailIndexer), org.WithInvitationEmailDisabled())

	auditRepo := audit.NewRepository(db)
	queue := jobs.NewStandaloneQueue(db)
	if startErr := queue.Start(t.Context()); startErr != nil {
		t.Fatalf("queue.Start() error = %v", startErr)
	}
	t.Cleanup(func() { _ = queue.Close(t.Context()) })

	complianceModule := compliance.NewModule(auditRepo, compliance.WithQueue(queue))

	contactEmailIndexer, err := dbkit.NewBlindIndexer("contact_email_index", testBlindIndexKey, dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("build contact email indexer: %v", err)
	}
	contactPhoneIndexer, err := dbkit.NewBlindIndexer("contact_phone_index", testBlindIndexKey, dbkit.NormalizePhoneE164)
	if err != nil {
		t.Fatalf("build contact phone indexer: %v", err)
	}
	notificationModule := notification.NewModule(db,
		notification.WithSMSSender(notification.NewConsoleSMSSender(nil)),
		notification.WithMailFrom("notifications@admin-test.example"),
		notification.WithContactEmailIndexer(contactEmailIndexer),
		notification.WithContactPhoneIndexer(contactPhoneIndexer),
		notification.WithDeliveryQueue(queue),
		notification.WithUserAddressResolver(fakeUserAddressResolver{}),
	)

	adminModule := NewModule(db,
		WithAuthn(authnModule),
		WithOrg(orgModule),
		WithCompliance(complianceModule),
		WithNotification(notificationModule),
	)

	reg, err := pkgcore.NewKernel().Bootstrap(t.Context(), authnModule, orgModule, complianceModule, notificationModule, adminModule)
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}
	return reg, adminModule, orgModule, queue
}

// TestModule_Register_DeclaresPermissionsAuditActionsAndNotificationType
// is the end-to-end wiring proof: a full Bootstrap over real authn, org,
// compliance and notification modules succeeds, and every declaration
// admin's Register makes actually lands on the shared registry.
func TestModule_Register_DeclaresPermissionsAuditActionsAndNotificationType(t *testing.T) {
	reg, adminModule, _, _ := buildTestAdminModule(t)

	for _, perm := range []string{PermissionAccess, PermissionTenantsManage, PermissionSearchUsers, PermissionImpersonate, PermissionAuditRead} {
		found := false
		for _, p := range reg.Permissions.Permissions() {
			if p == perm {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("permission %q not declared", perm)
		}
	}

	for _, action := range []string{AuditActionTenantStatusChanged, AuditActionImpersonationStarted, AuditActionImpersonationEnded} {
		if !contains(reg.AuditActions.Actions(), action) {
			t.Errorf("audit action %q not declared", action)
		}
	}

	foundType := false
	for _, nt := range reg.Notifications.Types() {
		if nt.Key == NotificationTypeImpersonationStarted {
			foundType = true
			if !nt.Unsubscribable {
				t.Error("NotificationTypeImpersonationStarted is not Unsubscribable")
			}
		}
	}
	if !foundType {
		t.Error("NotificationTypeImpersonationStarted not declared")
	}

	if adminModule.Search() == nil {
		t.Error("Search() is nil after Register, want a wired SearchService")
	}
}

// TestModule_Register_OrgNodeCreatedSubscriptionFires proves the full
// wiring, not just the declaration: a real org.Module creating a real
// root node -- through the SAME registry Bootstrap gave every module --
// reaches admin's subscriber and lazily registers the tenant, end to end.
func TestModule_Register_OrgNodeCreatedSubscriptionFires(t *testing.T) {
	_, adminModule, orgModule, _ := buildTestAdminModule(t)

	if _, err := adminModule.Tenants().Get(context.Background(), "tenant-wired"); !isCode(err, ErrTenantNotFound.Code) {
		t.Fatalf("Get() before any org node exists = %v, want ErrTenantNotFound", err)
	}

	orgTenantCtx := pkgcore.WithTenant(context.Background(), "tenant-wired")
	if _, err := orgModule.Tree().CreateRoot(orgTenantCtx, "Acme", "workspace"); err != nil {
		t.Fatalf("CreateRoot() error = %v", err)
	}

	got, err := adminModule.Tenants().Get(context.Background(), "tenant-wired")
	if err != nil {
		t.Fatalf("Get() after CreateRoot() error = %v, want the lazily-registered ledger row", err)
	}
	if got.Status != TenantStatusActive {
		t.Fatalf("Get().Status = %q, want active", got.Status)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
