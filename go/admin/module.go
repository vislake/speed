package admin

import (
	"embed"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/billing"
	"github.com/vislake/speed/go/compliance"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/metering"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"

	"github.com/vislake/speed/go/admin/locales"
	"github.com/vislake/speed/go/admin/migrations"
)

//go:embed api/openapi.yaml
var openAPISpecYAML []byte

// moduleName is admin's pkgcore.Module.Name(), and the key
// dbkit.MigrationRegistry.Register builds its dependency graph on.
const moduleName = "admin"

// apiPath is the common prefix admin's HTTP routes are mounted at. It must
// agree with the "paths:" keys of api/openapi.yaml.
const apiPath = "/api/v1/admin"

// The resource:action permissions admin contributes to the platform's
// permission catalog, all evaluated in rbac.SystemDomain -- "who may enter
// the admin console" is a permission like any other (D1), so enforcement is rbac's job (the
// reference app's own reg.Routes gate, mirroring how it gates notes and
// storage) and never this module's.
const (
	// PermissionAccess gates reading the tenant ledger (D3) -- the coarse
	// "may this person use the admin console at all" permission every
	// other admin operation is layered above.
	PermissionAccess = "admin:access"

	// PermissionTenantsManage gates PATCH /api/v1/admin/tenants/{id} --
	// renaming, suspending or resuming a ledger row (D3 + D4's
	// record-only half).
	PermissionTenantsManage = "admin:tenants_manage"

	// PermissionSearchUsers gates D6's cross-tenant user search and
	// membership composition.
	PermissionSearchUsers = "admin:search_users"

	// PermissionImpersonate gates D5's whole impersonation pipeline: start,
	// end and list.
	PermissionImpersonate = "admin:impersonate"

	// PermissionAuditRead gates D7's audit-query HTTP shell.
	PermissionAuditRead = "admin:audit_read"

	// PermissionAuditExport gates D7's export leg (POST
	// /api/v1/admin/audit-events/export) -- kept distinct from
	// PermissionAuditRead since exporting a tenant's complete audit
	// trail as a downloadable package is a materially stronger action
	// than merely reading it through the paginated query surface.
	PermissionAuditExport = "admin:audit_export"

	// PermissionRolesManage gates D8's whole role-management surface:
	// listing the declared-permission catalog, defining a role and
	// binding it to a user.
	PermissionRolesManage = "admin:roles_manage"

	// PermissionUsageRead gates D9's cross-tenant usage/billing dashboard.
	PermissionUsageRead = "admin:usage_read"

	// PermissionNotificationsRead gates D10's cross-tenant notification
	// send-record search.
	PermissionNotificationsRead = "admin:notifications_read"
)

// The audit actions admin contributes to the audit vocabulary --
// docs/internal/23-admin.md section 5's three round-1 actions.
// admin.role.assigned/revoked are deliberately NOT declared here: D8 (role
// management) is round 2's work, per this file's own Register doc comment.
const (
	AuditActionTenantStatusChanged  = "admin.tenant.status_changed"
	AuditActionImpersonationStarted = "admin.impersonation.started"
	AuditActionImpersonationEnded   = "admin.impersonation.ended"
)

// SystemPurposeAdminCrossTenant is the pkgcore.SystemPurpose admin
// registers for every cross-tenant operation it performs under D2's
// mechanism: D6's membership composition, D7's cross-tenant audit query,
// and D5's cross-tenant notification dispatch to an impersonation target.
// One purpose covers all three, since they are all instances of the same
// underlying operation D2 describes -- "admin acting across the tenant
// boundary it does not itself belong to" -- and docs/internal/23-admin.md's
// D2 section registers exactly one purpose for this module.
const SystemPurposeAdminCrossTenant pkgcore.SystemPurpose = "admin.cross_tenant"

// NotificationTypeImpersonationStarted is the notification type D5
// registers: the mandatory, non-unsubscribable security notification sent
// to the target user the moment an impersonation grant is started (see
// notifications.go for the full registration and its bilingual templates).
const NotificationTypeImpersonationStarted = "admin.impersonation_started"

// notificationGroupSecurity is the preference-matrix group
// NotificationTypeImpersonationStarted is filed under. It carries no
// meaning beyond grouping related types together in a UI; it is
// unsubscribable regardless of group.
const notificationGroupSecurity = "security"

// Module implements pkgcore.Module for go/admin: the operations-console
// backend docs/internal/23-admin.md designs -- D3's tenant ledger, D5's
// impersonation pipeline, D6's cross-tenant user search and D7's
// audit-query HTTP shell, this round's four in-scope decisions.
//
// admin sits at the top of the module dependency graph (root CLAUDE.md's
// own diagram: "... -> compliance -> admin"), so unlike most business
// modules it is explicitly permitted to import the concrete packages of
// every module below it directly, rather than through a structurally-typed,
// no-import seam -- see this file's own Register doc comment and this
// round's final report for exactly which imports that covers.
type Module struct {
	db *gorm.DB

	tenantRepo *TenantRepository
	grantRepo  *ImpersonationRepository

	tenants       *TenantService
	impersonation *ImpersonationService
	search        *SearchService
	auditSvc      *AuditService
	exportSvc     *ExportService
	roles         *RoleService
	usage         *UsageService

	authnModule        *authn.Module
	orgModule          *org.Module
	complianceModule   *compliance.Module
	notificationModule *notification.Module
	meteringModule     *metering.Module // optional, D9 -- see WithMetering
	billingModule      *billing.Module  // optional, D9 -- see WithBilling
	queue              jobs.Queue       // mandatory, D7's export leg -- see WithQueue

	handler *Handler
}

// Option configures a Module at construction time.
type Option func(*Module)

// WithAuthn wires the *authn.Module D6's cross-tenant user search reads
// through. Without it, Register returns ErrAuthnServiceRequired.
//
// This takes the *authn.Module, NOT its *authn.Service directly, and
// admin's own Register reads authnModule.Service() lazily, at Register
// time -- never here, at option-application time, which runs before
// Bootstrap and therefore before authn's own Register has ever built its
// Service (Module.Service's own doc comment: "nil until Register has
// run"). DependsOn() below declares "authn" so Bootstrap's dependency sort
// always runs authn's Register before admin's, exactly the ordering this
// lazy read depends on -- the same "read a host seam at call time, never
// capture it before Bootstrap has finished" idiom org's own hostSeams and
// the reference app's orgFeatureGate both apply for the identical reason.
func WithAuthn(authnModule *authn.Module) Option {
	return func(m *Module) { m.authnModule = authnModule }
}

// WithOrg wires the *org.Module D6's membership composition (D2's
// per-tenant loop over org.MemberService.Get) reads through. Without it,
// Register returns ErrOrgModuleRequired.
func WithOrg(orgModule *org.Module) Option {
	return func(m *Module) { m.orgModule = orgModule }
}

// WithCompliance wires the *compliance.Module D7's audit-query HTTP shell
// reads through (compliance.Module.AuditQuery()). Without it, Register
// returns ErrComplianceModuleRequired.
func WithCompliance(complianceModule *compliance.Module) Option {
	return func(m *Module) { m.complianceModule = complianceModule }
}

// WithNotification wires the *notification.Module D5's mandatory
// impersonation-started security notification dispatches through. Without
// it, Register returns ErrNotificationModuleRequired.
func WithNotification(notificationModule *notification.Module) Option {
	return func(m *Module) { m.notificationModule = notificationModule }
}

// WithQueue wires the jobs.Queue D7's export leg (POST
// /api/v1/admin/audit-events/export) enqueues onto -- mandatory, like the
// four options above: without it, Register returns ErrQueueRequired.
// compliance.ExportService.Export gathers, stores and delivers a tenant's
// complete audit export in one call, which does not belong inside an HTTP
// request's own timeout budget (root CLAUDE.md's asynchronous-work
// discipline), so this module needs a queue exactly as go/storage's and
// go/notification's own WithQueue/WithDeliveryQueue options do.
func WithQueue(queue jobs.Queue) Option {
	return func(m *Module) { m.queue = queue }
}

// WithMetering wires the *metering.Module D9's usage dashboard reads
// go/metering's per-tenant UsageSummary rows through
// (metering.Module.Summaries().List). OPTIONAL, unlike the five options
// above: a host that never calls this simply gets no metering dimension
// in D9's response rows (nil MeteringSummaries on every row) rather than
// failing Bootstrap -- see UsageService's own doc comment for why
// go/metering and go/billing are each independently optional rather than
// both mandatory the way authn/org/compliance/notification are.
func WithMetering(meteringModule *metering.Module) Option {
	return func(m *Module) { m.meteringModule = meteringModule }
}

// WithBilling wires the *billing.Module D9's usage dashboard reads
// go/billing's per-tenant CreditBalance and active Subscription through
// (billing.Module.Credits().Balance, billing.Module.Subscriptions().Active).
// OPTIONAL, mirroring WithMetering's own doc comment exactly, the other
// side of the same design choice.
func WithBilling(billingModule *billing.Module) Option {
	return func(m *Module) { m.billingModule = billingModule }
}

// NewModule returns a Module whose two platform-data tables live in db.
// Constructing a Module performs no I/O: opening and migrating db is the
// host's responsibility, done once at startup before Bootstrap ever calls
// Register, exactly like every other module in this codebase.
func NewModule(db *gorm.DB, opts ...Option) *Module {
	tenantRepo := NewTenantRepository(db)
	grantRepo := NewImpersonationRepository(db)
	tenants := NewTenantService(tenantRepo)
	m := &Module{
		db:            db,
		tenantRepo:    tenantRepo,
		grantRepo:     grantRepo,
		tenants:       tenants,
		impersonation: NewImpersonationService(grantRepo),
		roles:         NewRoleService(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Tenants returns the module's TenantService (D3). It also implements
// tenancy.TenantStatusResolver (D4) -- a host wires
// tenancy.WithTenantStatusResolver(adminModule.Tenants()) into its own
// tenancy.Middleware call to give tenant suspension real teeth.
func (m *Module) Tenants() *TenantService { return m.tenants }

// Impersonation returns the module's ImpersonationService (D5).
func (m *Module) Impersonation() *ImpersonationService { return m.impersonation }

// Search returns the module's SearchService (D6). Nil until Register has
// run.
func (m *Module) Search() *SearchService { return m.search }

// Roles returns the module's RoleService (D8). Every method on it fails
// closed with ErrRBACServiceRequired until the host calls AttachRBAC --
// see that method's own doc comment for when to call it.
func (m *Module) Roles() *RoleService { return m.roles }

// Usage returns the module's UsageService (D9). Nil until Register has
// run.
func (m *Module) Usage() *UsageService { return m.usage }

// Export returns the module's ExportService (D7's export leg). Nil until
// Register has run.
func (m *Module) Export() *ExportService { return m.exportSvc }

// AttachRBAC gives the module's RoleService (D8) the *rbac.Service every
// one of its methods delegates to. The host calls this exactly once,
// immediately after its own rbacModule.Attach(registry) succeeds -- a
// call that, by rbac's own documented contract, must run strictly AFTER
// pkgcore.Kernel.Bootstrap returns (Attach freezes the snapshot of every
// permission every module declared, so it cannot run any earlier without
// risking an incomplete catalog).
//
// This is why rbac is NOT wired through a WithXxx(*rbac.Module)
// construction-time Option the way authn, org, compliance and
// notification are: admin's own Module.Register runs DURING Bootstrap,
// strictly before the host's own post-Bootstrap rbacModule.Attach call,
// so a *rbac.Module handed to admin at construction time would have no
// Service to read yet at the one point (Register) admin could read it
// from. AttachRBAC is therefore a distinct, later wiring step the host
// performs itself -- see role.go's RoleService doc comment for the full
// reasoning. Calling this before Bootstrap, or not at all, leaves every
// RoleService method failing closed with ErrRBACServiceRequired rather
// than panicking on a nil service.
func (m *Module) AttachRBAC(svc *rbac.Service) { m.roles.attach(svc) }

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module: "authn", and only "authn".
//
// Every other runtime read Register performs -- m.orgModule.Members(),
// m.complianceModule.AuditQuery(), m.notificationModule.Deliveries() -- is
// safe regardless of registration order, because each of those three
// modules builds the returned value inside its own NewModule constructor,
// before Bootstrap ever runs. authn is the one exception:
// authn.Module.Service() is documented nil until authn's OWN Register has
// built it (see WithAuthn's doc comment), so admin's Register must run
// strictly after authn's -- this is exactly what DependsOn exists to
// express, and Kernel.Bootstrap's own dependency sort (sortModulesByDependency)
// honors it regardless of the order modules were passed to Bootstrap in.
func (m *Module) DependsOn() []string { return []string{authnModuleName} }

// authnModuleName is authn's pkgcore.Module.Name() -- "authn" -- spelled
// as its own constant rather than a bare string literal at the DependsOn
// call site, so a reader (and a future refactor) sees it is a coordination
// point with another module's own identity, not an arbitrary label.
const authnModuleName = "authn"

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: admin's own error-code descriptions
// and the impersonation-started notification's bilingual templates, in
// both supported languages with identical id sets.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module: admin's own OpenAPI fragment, the
// sixth after notes, org, authn, storage and notification.
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements pkgcore.Module. Per the interface's contract it only
// declares and wires -- no database call, no outbound call, nothing that
// touches m.db.
//
// It refuses to proceed (before declaring anything) when a mandatory
// With* option was never applied -- ErrAuthnServiceRequired,
// ErrOrgModuleRequired, ErrComplianceModuleRequired,
// ErrNotificationModuleRequired -- the same "fail Bootstrap with a named
// missing seam" shape org's ErrEmailIndexerRequired and config's
// ErrCipherRequired already use.
//
// It deliberately declares only the round-1 subset of
// docs/internal/23-admin.md's design: D3 (record-only, no D4 enforcement
// seam), D5 in full, D6, and D7's read side only (no export leg, no D8
// role-management permissions or audit actions -- admin.role.assigned/
// revoked are round 2's, alongside rbac.AssignRole/RevokeRole's own
// already-published domain events, which this module does not yet call).
func (m *Module) Register(reg *pkgcore.Registry) error {
	if m.authnModule == nil {
		return ErrAuthnServiceRequired
	}
	authnSvc := m.authnModule.Service()
	if authnSvc == nil {
		// DependsOn() declares "authn", so Bootstrap's dependency sort
		// should make this unreachable in practice -- but Register must
		// still fail closed rather than hand SearchService a nil
		// *authn.Service if that guarantee is ever violated (a host
		// calling Register directly, bypassing Bootstrap, say).
		return ErrAuthnServiceRequired
	}
	if m.orgModule == nil {
		return ErrOrgModuleRequired
	}
	if m.complianceModule == nil {
		return ErrComplianceModuleRequired
	}
	if m.notificationModule == nil {
		return ErrNotificationModuleRequired
	}
	if m.queue == nil {
		return ErrQueueRequired
	}

	if err := reg.Permissions.Add(
		PermissionAccess,
		PermissionTenantsManage,
		PermissionSearchUsers,
		PermissionImpersonate,
		PermissionAuditRead,
		PermissionAuditExport,
		PermissionRolesManage,
		PermissionUsageRead,
		PermissionNotificationsRead,
	); err != nil {
		return err
	}
	if err := reg.AuditActions.Add(
		AuditActionTenantStatusChanged,
		AuditActionImpersonationStarted,
		AuditActionImpersonationEnded,
	); err != nil {
		return err
	}
	if err := reg.Notifications.Add(pkgcore.NotificationType{
		Key:             NotificationTypeImpersonationStarted,
		Group:           notificationGroupSecurity,
		DefaultChannels: []string{notification.ChannelInApp, notification.ChannelEmail},
		// Unsubscribable: true is the mechanism D5's "mandatory,
		// non-unsubscribable security notification" requirement realizes -- verified against
		// go/notification/integration_test/clinic/module.go's identical
		// Add call, the pattern this registration mirrors exactly.
		Unsubscribable: true,
	}); err != nil {
		return err
	}

	pkgcore.RegisterSystemPurpose(SystemPurposeAdminCrossTenant)

	bus := reg.EventBus()
	m.tenants.attachAudit(bus, reg.AuditActions)
	m.impersonation.attach(bus, reg.AuditActions, m.notificationModule.Deliveries())

	m.search = NewSearchService(authnSvc, m.orgModule.Members(), m.tenants)
	m.search.attach(bus)

	m.auditSvc = NewAuditService(m.complianceModule.AuditQuery(), m.tenants)
	m.auditSvc.attach(bus)

	m.exportSvc = NewExportService(m.complianceModule.Export(), m.queue)
	if err := reg.Jobs.Handle(jobTypeAuditExport, m.exportSvc); err != nil {
		return err
	}

	m.usage = NewUsageService(m.meteringModule, m.billingModule, m.tenants)
	m.usage.attach(bus)

	sendRecords := NewSendRecordSearchService(m.notificationModule.Deliveries(), m.tenants)
	sendRecords.attach(bus)

	reg.Events.Subscribe(org.EventNodeCreated, m.tenants.handleOrgNodeCreated)

	m.handler = NewHandler(m.tenants, m.impersonation, m.search, m.auditSvc, m.exportSvc, m.roles, m.usage, sendRecords)
	reg.Routes.Mount(apiPath, m.handler)
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
