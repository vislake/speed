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

// APIPath is the common prefix admin's HTTP routes are mounted at. It must
// agree with the "paths:" keys of api/openapi.yaml.
//
// It is exported because a host composing the platform middleware chain
// splits admin's subtree out by this prefix: admin's surface must not sit
// behind ordinary tenancy resolution or identity substitution, so the
// router that partitions the mounted routes needs the module's own
// mount-point constant rather than a copy that can drift from it.
const APIPath = "/api/v1/admin"

// The resource:action permissions admin contributes to the platform's
// permission catalog, the vocabulary admin's own routes are evaluated
// against in rbac.SystemDomain -- "who may enter the admin console" is a
// permission like any other, so enforcement is rbac's job (the host's own
// reg.Routes gate, mirroring how it gates notes and storage) and never
// this module's.
const (
	// PermissionAccess gates reading the tenant ledger -- the coarse
	// "may this person use the admin console at all" permission every
	// other admin operation is layered above.
	PermissionAccess = "admin:access"

	// PermissionTenantsManage gates PATCH /api/v1/admin/tenants/{id} --
	// renaming, suspending or resuming a ledger row.
	PermissionTenantsManage = "admin:tenants_manage"

	// PermissionSearchUsers gates the cross-tenant user search and
	// membership composition.
	PermissionSearchUsers = "admin:search_users"

	// PermissionImpersonate gates the whole impersonation pipeline:
	// start, end and list.
	PermissionImpersonate = "admin:impersonate"

	// PermissionAuditRead gates the audit-query HTTP surface.
	PermissionAuditRead = "admin:audit_read"

	// PermissionAuditExport gates the export leg (POST
	// /api/v1/admin/audit-events/export) -- kept distinct from
	// PermissionAuditRead since exporting a tenant's complete audit
	// trail as a downloadable package is a materially stronger action
	// than merely reading it through the paginated query surface.
	PermissionAuditExport = "admin:audit_export"

	// PermissionRolesManage gates the whole role-management surface:
	// listing the declared-permission catalog, defining a role and
	// binding it to a user.
	PermissionRolesManage = "admin:roles_manage"

	// PermissionUsageRead gates the cross-tenant usage/billing dashboard.
	PermissionUsageRead = "admin:usage_read"

	// PermissionNotificationsRead gates the cross-tenant notification
	// send-record search.
	PermissionNotificationsRead = "admin:notifications_read"
)

// The audit actions admin contributes to the audit vocabulary.
// admin.role.assigned/revoked are deliberately NOT declared here: role
// management wraps rbac.Service, whose own AssignRole/RevokeRole publish
// rbac.role_binding.assigned/revoked (this file's own Register doc
// comment).
const (
	AuditActionTenantStatusChanged  = "admin.tenant.status_changed"
	AuditActionImpersonationStarted = "admin.impersonation.started"
	AuditActionImpersonationEnded   = "admin.impersonation.ended"

	// AuditActionAuditExport is emitted by ExportService once a tenant's
	// audit-event export actually completes: exporting a tenant's full
	// audit history is itself a security-relevant action, and without this
	// the export would leave no trace of "who exported which tenant, when"
	// anywhere in the audit trail.
	AuditActionAuditExport = "admin.audit_export"
)

// SystemPurposeAdminCrossTenant is the pkgcore.SystemPurpose admin
// registers for every cross-tenant operation it performs under the
// audited system-context wrapper: the user search and its membership
// composition (both halves take the audited wrapper -- see search.go),
// the cross-tenant audit query, the cross-tenant notification dispatch to
// an impersonation target, the per-tenant usage dashboard and the
// cross-tenant send-record search. One purpose covers all of them, since
// they are all instances of the same underlying operation -- "admin
// acting across the tenant boundary it does not itself belong to".
const SystemPurposeAdminCrossTenant pkgcore.SystemPurpose = "admin.cross_tenant"

// NotificationTypeImpersonationStarted is the notification type the
// impersonation pipeline registers: the mandatory, non-unsubscribable
// security notification sent to the target user the moment an
// impersonation grant is started -- see Register's own
// reg.Notifications.Add call below for the registration and
// locales/{zh-CN,en-US}.toml for its bilingual templates (there is no
// separate notifications.go file; both live here and in the locale
// bundles).
const NotificationTypeImpersonationStarted = "admin.impersonation_started"

// notificationGroupSecurity is the preference-matrix group
// NotificationTypeImpersonationStarted is filed under. It carries no
// meaning beyond grouping related types together in a UI; the group never
// affects whether recipients may opt out -- that is the type's own
// Unsubscribable field, false here (see Register's Add call).
const notificationGroupSecurity = "security"

// Module implements pkgcore.Module for go/admin, the operations-console
// backend: the tenant ledger, the impersonation pipeline, cross-tenant
// user search, the audit-query HTTP surface with its asynchronous export
// leg, role management, the usage/billing dashboard and notification
// send-record search.
//
// admin sits at the top of the module dependency graph, so unlike most
// business modules it is explicitly permitted to import the concrete
// packages of every module below it directly, rather than through a
// structurally-typed, no-import seam -- see this file's own Register doc
// comment for exactly which imports that covers.
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
	meteringModule     *metering.Module // optional -- see WithMetering
	billingModule      *billing.Module  // optional -- see WithBilling
	queue              jobs.Queue       // mandatory -- see WithQueue

	handler *Handler
}

// Option configures a Module at construction time.
type Option func(*Module)

// WithAuthn wires the *authn.Module the cross-tenant user search reads
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
// the reference app's config-handle gate adapters both apply for the
// identical reason.
func WithAuthn(authnModule *authn.Module) Option {
	return func(m *Module) { m.authnModule = authnModule }
}

// WithOrg wires the *org.Module the search path's membership composition
// (the per-tenant loop over org.MemberService.Get) reads through. Without
// it, Register returns ErrOrgModuleRequired.
func WithOrg(orgModule *org.Module) Option {
	return func(m *Module) { m.orgModule = orgModule }
}

// WithCompliance wires the *compliance.Module the audit-query HTTP
// surface reads through (compliance.Module.AuditQuery()). Without it,
// Register returns ErrComplianceModuleRequired.
func WithCompliance(complianceModule *compliance.Module) Option {
	return func(m *Module) { m.complianceModule = complianceModule }
}

// WithNotification wires the *notification.Module the mandatory
// impersonation-started security notification dispatches through. Without
// it, Register returns ErrNotificationModuleRequired.
func WithNotification(notificationModule *notification.Module) Option {
	return func(m *Module) { m.notificationModule = notificationModule }
}

// WithQueue wires the jobs.Queue the audit-export leg (POST
// /api/v1/admin/audit-events/export) enqueues onto -- mandatory, like the
// options above: without it, Register returns ErrQueueRequired.
// compliance.ExportService.Export gathers, stores and delivers a tenant's
// complete audit export in one call, which does not belong inside an HTTP
// request's own timeout budget (long-running work never runs synchronously
// inside a request), so this module needs a queue exactly as go/storage's
// and go/notification's own WithQueue/WithDeliveryQueue options do.
func WithQueue(queue jobs.Queue) Option {
	return func(m *Module) { m.queue = queue }
}

// WithMetering wires the *metering.Module the usage dashboard reads
// go/metering's per-tenant UsageSummary rows through
// (metering.Module.Summaries().List). OPTIONAL, unlike the options above:
// a host that never calls this simply gets no metering dimension in the
// dashboard's response rows (nil MeteringSummaries on every row) rather
// than failing Bootstrap -- see UsageService's own doc comment for why
// go/metering and go/billing are each independently optional rather than
// both mandatory the way authn/org/compliance/notification are.
func WithMetering(meteringModule *metering.Module) Option {
	return func(m *Module) { m.meteringModule = meteringModule }
}

// WithBilling wires the *billing.Module the usage dashboard reads
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
		impersonation: newImpersonationService(grantRepo),
		roles:         NewRoleService(),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Tenants returns the module's TenantService. It also implements
// tenancy.TenantStatusResolver -- a host wires
// tenancy.WithTenantStatusResolver(adminModule.Tenants()) into its own
// tenancy.Middleware call to give tenant suspension real teeth.
func (m *Module) Tenants() *TenantService { return m.tenants }

// Impersonation returns the module's ImpersonationService.
func (m *Module) Impersonation() *ImpersonationService { return m.impersonation }

// Search returns the module's SearchService. Nil until Register has run.
func (m *Module) Search() *SearchService { return m.search }

// Roles returns the module's RoleService. Every method on it fails closed
// with ErrRBACServiceRequired until the host calls AttachRBAC -- see that
// method's own doc comment for when to call it.
func (m *Module) Roles() *RoleService { return m.roles }

// Usage returns the module's UsageService. Nil until Register has run.
func (m *Module) Usage() *UsageService { return m.usage }

// Export returns the module's ExportService (the audit-export leg). Nil
// until Register has run.
func (m *Module) Export() *ExportService { return m.exportSvc }

// AttachRBAC gives the module's RoleService the *rbac.Service every one
// of its methods delegates to. The host calls this exactly once,
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
//
// It also gives the impersonation pipeline the same *rbac.Service:
// ImpersonationService.attachRBAC lets a live grant be automatically
// ended when the administrator's own admin:impersonate permission is
// later revoked (see impersonation_service.go's endIfNoLongerPermitted
// for the mechanism) -- the identical post-Bootstrap-only timing
// constraint applies, since the check calls rbac.Service.Can. And because
// that automatic end is the only thing standing between a revoked
// administrator and a still-live grant, ImpersonationService.Start
// refuses with ErrRBACServiceRequired until this call has run
// (impersonation_service.go's own rbacSvc doc comment) -- the same
// fail-closed gate every RoleService method above applies, on the very
// same seam. Calling this before Bootstrap, or not at all, thus leaves
// the whole role-management surface AND impersonation grant-starting
// failing closed rather than a nil-service panic or a grant the module
// could never automatically end.
func (m *Module) AttachRBAC(svc *rbac.Service) {
	m.roles.attach(svc)
	m.impersonation.attachRBAC(svc)
}

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

// OpenAPISpec implements pkgcore.Module: admin's own OpenAPI fragment
// (api/openapi.yaml).
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements pkgcore.Module. Per the interface's contract it only
// declares and wires -- no database call, no outbound call, nothing that
// touches m.db.
//
// It refuses to proceed (before declaring anything) when a mandatory
// With* option was never applied -- ErrAuthnServiceRequired,
// ErrOrgModuleRequired, ErrComplianceModuleRequired,
// ErrNotificationModuleRequired or ErrQueueRequired, each naming the
// option the host forgot.
//
// It declares the module's full surface: the permission catalog
// (PermissionAccess through PermissionNotificationsRead), the audit
// actions, the jobTypeAuditExport job handler on reg.Jobs.Handle, the
// usage dashboard and the send-record search wiring.
// admin.role.assigned/admin.role.revoked are the one deliberate
// exception: rbac's own AssignRole/RevokeRole already publish
// rbac.role_binding.assigned/revoked, and RoleService's wrapper calls
// them exactly as any other caller would, so admin declares no audit
// action of its own for either.
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
		AuditActionAuditExport,
	); err != nil {
		return err
	}
	if err := reg.Notifications.Add(pkgcore.NotificationType{
		Key:             NotificationTypeImpersonationStarted,
		Group:           notificationGroupSecurity,
		DefaultChannels: []string{notification.ChannelInApp, notification.ChannelEmail},
		// RecipientVisibleParams: []string{} (the empty list is a real
		// declaration, never "unset") marks the boundary the impersonation
		// notice must hold: its copy is static, so nothing about a start is
		// parameterized and NOTHING may ride the dispatch's params channel
		// to the recipient. An impersonation reason's mandatory semantics
		// are exactly "why am I looking at this account", and the target is
		// precisely the party an investigation must not brief, so the
		// operator's free-text justification and either party's identity
		// data must never land in the impersonated user's own inbox row or
		// inbox API. notification enforces the declaration
		// (ErrDispatchParamsNotAllowed at Dispatch, narrowing at delivery),
		// so any per-start copy has to extend this list consciously.
		RecipientVisibleParams: []string{},
		// Unsubscribable: false is what makes the "mandatory,
		// non-unsubscribable security notification" requirement REAL:
		// pkgcore.NotificationType.Unsubscribable reports whether recipients
		// may opt out (pkgcore/registry.go's own field doc), so a mandatory
		// type must declare false -- the preference matrix then refuses an
		// empty selection with notification.ErrPreferenceOptoutNotAllowed
		// (the recipient may narrow channels but never switch the
		// notification off entirely; go/notification/preference_service.go's
		// Set, case 4). Reading the field's plain-English sense instead of
		// its actual semantics would let the target opt out of the one
		// notification whose whole purpose is telling them an administrator
		// is inside their account.
		Unsubscribable: false,
	}); err != nil {
		return err
	}

	pkgcore.RegisterSystemPurpose(SystemPurposeAdminCrossTenant)

	bus := reg.EventBus()
	m.tenants.attachAudit(bus, reg.AuditActions, authnSvc)
	m.impersonation.attach(bus, reg.AuditActions, m.notificationModule.Deliveries(), authnSvc, m.orgModule.Members())

	m.search = NewSearchService(authnSvc, m.orgModule.Members(), m.tenants)
	m.search.attach(bus)

	m.auditSvc = NewAuditService(m.complianceModule.AuditQuery(), m.tenants)
	m.auditSvc.attach(bus)

	m.exportSvc = NewExportService(m.complianceModule.Export(), m.queue)
	m.exportSvc.attachAudit(bus, reg.AuditActions, authnSvc)
	if err := reg.Jobs.Handle(jobTypeAuditExport, m.exportSvc); err != nil {
		return err
	}

	m.usage = NewUsageService(m.meteringModule, m.billingModule, m.tenants)
	m.usage.attach(bus)

	sendRecords := NewSendRecordSearchService(m.notificationModule.Deliveries(), m.tenants)
	sendRecords.attach(bus)

	reg.Events.Subscribe(org.EventNodeCreated, m.tenants.handleOrgNodeCreated)
	// A live impersonation grant must not outlive its administrator's own
	// admin:impersonate permission -- see impersonation_service.go's
	// onRoleBindingRevoked/onRoleChanged for the mechanism, which only
	// takes effect once Module.AttachRBAC has given it a real *rbac.Service
	// to re-check against.
	reg.Events.Subscribe(rbac.EventRoleBindingRevoked, m.impersonation.onRoleBindingRevoked)
	reg.Events.Subscribe(rbac.EventRoleChanged, m.impersonation.onRoleChanged)

	m.handler = NewHandler(m.tenants, m.impersonation, m.search, m.auditSvc, m.exportSvc, m.roles, m.usage, sendRecords)
	// The handler's optional catalog slice: hand it the registry so its
	// impersonation-start locale negotiation can read the merged catalog
	// at request time -- never captured earlier, because reg.Locales() is
	// nil inside Register by design (every consumer reads it at call
	// time). A handler built directly (no Register) keeps a nil host and
	// skips the tier.
	m.handler.host = reg
	reg.Routes.Mount(APIPath, m.handler)
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
