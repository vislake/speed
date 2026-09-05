package admin

import (
	"context"
	"embed"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/billing"
	billingmigrations "github.com/vislake/speed/go/billing/migrations"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/metering"
	meteringmigrations "github.com/vislake/speed/go/metering/migrations"
	"github.com/vislake/speed/go/notification"
	notificationmigrations "github.com/vislake/speed/go/notification/migrations"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
	rbacmigrations "github.com/vislake/speed/go/rbac/migrations"
	"github.com/vislake/speed/go/sharing"
	sharingmigrations "github.com/vislake/speed/go/sharing/migrations"
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

// rbacMigrationModule mirrors orgMigrationModule for rbac's own migration
// files -- D8's tests need a real, migrated rbac.Module to Attach.
type rbacMigrationModule struct{}

func (rbacMigrationModule) Name() string                     { return "rbac" }
func (rbacMigrationModule) DependsOn() []string              { return nil }
func (rbacMigrationModule) Migrations() embed.FS             { return rbacmigrations.FS }
func (rbacMigrationModule) Locales() embed.FS                { return embed.FS{} }
func (rbacMigrationModule) OpenAPISpec() []byte              { return nil }
func (rbacMigrationModule) Register(*pkgcore.Registry) error { return nil }

// sharingMigrationModule mirrors orgMigrationModule for sharing's own
// migration files -- D7's export-leg tests need a real sharing.Module for
// compliance.WithSharing to deliver through.
type sharingMigrationModule struct{}

func (sharingMigrationModule) Name() string                     { return "sharing" }
func (sharingMigrationModule) DependsOn() []string              { return nil }
func (sharingMigrationModule) Migrations() embed.FS             { return sharingmigrations.FS }
func (sharingMigrationModule) Locales() embed.FS                { return embed.FS{} }
func (sharingMigrationModule) OpenAPISpec() []byte              { return nil }
func (sharingMigrationModule) Register(*pkgcore.Registry) error { return nil }

// meteringMigrationModule mirrors orgMigrationModule for metering's own
// migration files -- D9's usage-dashboard tests need a real
// metering.Module.
type meteringMigrationModule struct{}

func (meteringMigrationModule) Name() string                     { return "metering" }
func (meteringMigrationModule) DependsOn() []string              { return nil }
func (meteringMigrationModule) Migrations() embed.FS             { return meteringmigrations.FS }
func (meteringMigrationModule) Locales() embed.FS                { return embed.FS{} }
func (meteringMigrationModule) OpenAPISpec() []byte              { return nil }
func (meteringMigrationModule) Register(*pkgcore.Registry) error { return nil }

// billingMigrationModule mirrors orgMigrationModule for billing's own
// migration files -- D9's usage-dashboard tests need a real
// billing.Module.
type billingMigrationModule struct{}

func (billingMigrationModule) Name() string                     { return "billing" }
func (billingMigrationModule) DependsOn() []string              { return nil }
func (billingMigrationModule) Migrations() embed.FS             { return billingmigrations.FS }
func (billingMigrationModule) Locales() embed.FS                { return embed.FS{} }
func (billingMigrationModule) OpenAPISpec() []byte              { return nil }
func (billingMigrationModule) Register(*pkgcore.Registry) error { return nil }

// testAdminEnv is buildTestAdminModule's full return value: every handle
// a round-2 test (D7/D8/D9/D10) might need alongside round-1's own
// registry/admin/org/queue tuple, gathered into one struct so adding a
// module here does not force every existing two-call-site destructuring
// assignment to grow another blank identifier.
type testAdminEnv struct {
	Registry     *pkgcore.Registry
	Admin        *Module
	Org          *org.Module
	Notification *notification.Module
	Queue        *jobs.StandaloneQueue

	// DB is the shared *gorm.DB every module above was opened against --
	// useful for a test that needs to write a fixture row directly
	// through a downstream module's own exported repository type
	// (notification.NewSendRecordRepository, say) rather than driving
	// its full business pipeline.
	DB *gorm.DB

	// RBAC is the real, Attach()-ed *rbac.Service D8's tests wire onto
	// adminModule.AttachRBAC -- nil until buildTestAdminModule's caller
	// does so; buildTestAdminModule itself does not call AttachRBAC,
	// since round-1 tests must keep observing RoleService's fail-closed
	// ErrRBACServiceRequired contract exactly as before.
	RBAC *rbac.Service

	// Sharing is the real *sharing.Module compliance.WithSharing delivers
	// D7's exports through.
	Sharing *sharing.Module

	// Metering and Billing are real modules, NOT wired into adminModule
	// by default (D9's WithMetering/WithBilling are optional -- see
	// Module's own doc comments): a D9 test wires either or both itself
	// via a second admin.NewModule call over the same db, mirroring how
	// D8's tests call AttachRBAC themselves rather than having this
	// builder do it for them.
	Metering *metering.Module
	Billing  *billing.Module
}

// buildTestAdminModule wires a full, real module graph (authn, org,
// compliance, notification, admin) over one shared database and returns
// the bootstrapped registry together with admin's own Module -- the
// end-to-end proof that Register's mandatory ErrXRequired checks are
// satisfied by a correctly wired host, and that every declaration
// (permissions, audit actions, the notification type, the event
// subscription) succeeds together. It additionally constructs (but does
// not always wire into adminModule -- see testAdminEnv's own field
// comments) real rbac, sharing, metering and billing modules over the
// SAME database, for round 2's D7/D8/D9 tests to build on without each
// standing up its own parallel module graph.
func buildTestAdminModule(t *testing.T) testAdminEnv {
	t.Helper()
	pkgcore.RegisterSystemPurpose(SystemPurposeAdminCrossTenant)

	db := authnTestDB(t) // already carries admin's own migrations too (testutil.NewDB is layered underneath).

	registry := dbkit.NewMigrationRegistry()
	for _, m := range []pkgcore.Module{
		orgMigrationModule{},
		auditMigrationModule{},
		notificationMigrationModule{},
		rbacMigrationModule{},
		sharingMigrationModule{},
		meteringMigrationModule{},
		billingMigrationModule{},
	} {
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

	sharingModule := sharing.NewModule(db)
	complianceModule := compliance.NewModule(auditRepo, compliance.WithQueue(queue), compliance.WithSharing(sharingModule.Service()))

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

	rbacModule := rbac.NewModule(db)
	meteringModule := metering.NewModule(db)
	billingModule := billing.NewModule(db, meteringModule.Aggregator())

	adminModule := NewModule(db,
		WithAuthn(authnModule),
		WithOrg(orgModule),
		WithCompliance(complianceModule),
		WithNotification(notificationModule),
		WithQueue(queue),
	)

	reg, err := pkgcore.NewKernel().Bootstrap(t.Context(),
		authnModule, orgModule, complianceModule, notificationModule, adminModule,
		rbacModule, sharingModule, meteringModule, billingModule,
	)
	if err != nil {
		t.Fatalf("Bootstrap() error = %v", err)
	}

	rbacService, err := rbacModule.Attach(reg)
	if err != nil {
		t.Fatalf("rbacModule.Attach() error = %v", err)
	}
	t.Cleanup(func() { _ = rbacService.Close() })

	return testAdminEnv{
		Registry:     reg,
		Admin:        adminModule,
		Org:          orgModule,
		Notification: notificationModule,
		Queue:        queue,
		DB:           db,
		RBAC:         rbacService,
		Sharing:      sharingModule,
		Metering:     meteringModule,
		Billing:      billingModule,
	}
}

// TestModule_Register_DeclaresPermissionsAuditActionsAndNotificationType
// is the end-to-end wiring proof: a full Bootstrap over real authn, org,
// compliance and notification modules succeeds, and every declaration
// admin's Register makes actually lands on the shared registry.
func TestModule_Register_DeclaresPermissionsAuditActionsAndNotificationType(t *testing.T) {
	env := buildTestAdminModule(t)
	reg, adminModule := env.Registry, env.Admin

	for _, perm := range []string{
		PermissionAccess, PermissionTenantsManage, PermissionSearchUsers, PermissionImpersonate, PermissionAuditRead,
		PermissionAuditExport, PermissionRolesManage, PermissionUsageRead, PermissionNotificationsRead,
	} {
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
	env := buildTestAdminModule(t)
	adminModule, orgModule := env.Admin, env.Org

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
