package compliance

import (
	"embed"
	"time"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/sharing"

	"github.com/vislake/speed/go/compliance/locales"
)

// moduleName is compliance's pkgcore.Module.Name().
const moduleName = "compliance"

// ConfigDefaultRetentionWindow is the dotted configuration key for the
// per-tenant retention-window override RetentionService.RetentionWindow
// resolves: how long a soft-deleted row survives before the periodic
// sweep hard-deletes it. It is declared, tenant-overridable
// (pkgcore.ConfigItem carries no scope of its own -- go/config's Service
// resolves tenant-then-system-then-default per key, per its own Get doc
// comment), through Module.Register; RetentionWindow itself falls back to
// defaultRetentionWindow rather than failing when no *config.Service was
// wired at all.
const ConfigDefaultRetentionWindow = "compliance.default_retention_window"

// The resource:action permissions compliance declares. Erasure execution
// deliberately gets its own tightly-scoped permission rather than being
// folded into a general "manage" one: it is the one operation this module
// performs that is irreversible, so a grant of "retention:manage" (view
// and tune the sweep schedule) must not, by itself, also authorize
// triggering a right-to-erasure request.
const (
	// PermissionAuditRead gates reading the audit trail through AuditQuery.
	PermissionAuditRead = "compliance:audit:read"
	// PermissionRetentionManage gates viewing and tuning retention-sweep
	// configuration and triggering an ad hoc sweep.
	PermissionRetentionManage = "compliance:retention:manage"
	// PermissionErasureExecute gates triggering a right-to-erasure
	// request -- the one irreversible operation this module performs, so
	// it is never folded into PermissionRetentionManage.
	PermissionErasureExecute = "compliance:erasure:execute"
	// PermissionExportExecute gates triggering a data-export gathering run.
	PermissionExportExecute = "compliance:export:execute"
)

// configItemDecls is the catalog entry for compliance's configuration
// items, declared in Register.
var configItemDecls = []pkgcore.ConfigItem{
	{
		Key:         ConfigDefaultRetentionWindow,
		Type:        "duration",
		Default:     defaultRetentionWindow,
		Description: "How long a soft-deleted (mark-deleted) row survives before the periodic retention sweep hard-deletes it.",
		Group:       "compliance",
		Min:         time.Hour,
	},
	{
		Key:         ConfigExportDeliveryExpiry,
		Type:        "duration",
		Default:     defaultExportDeliveryExpiry,
		Description: "How long a data-export download link (minted through go/sharing) stays valid. Deliberately much shorter than a general-purpose share's own default expiry, since an export bundles a whole tenant's data.",
		Group:       "compliance",
		Min:         time.Hour,
		Max:         72 * time.Hour,
	},
}

// Module implements pkgcore.Module for go/compliance: the governance
// layer over retention-window sweeping, right-to-erasure orchestration,
// data-export gathering and read-only audit querying, all built on top of
// primitives that already ship lower in the module graph -- see doc.go's
// own package comment for the full framing.
//
// A host constructs one with NewModule and hands it to Kernel.Bootstrap.
// Constructing a Module performs no I/O: db (audit's own connection) is
// opened and migrated by the host before Register is ever called, exactly
// like every other module in this codebase.
type Module struct {
	retention  *RetentionService
	erasure    *ErasureService
	export     *ExportService
	auditQuery *AuditQuery

	// auditRepo is the same *audit.Repository instance auditQuery reads
	// through, kept as its own field so onConfigItemChanged (config_audit.go)
	// can write to it without reaching into auditQuery's unexported field.
	auditRepo *audit.Repository

	queue jobs.Queue
}

// Option configures a Module at construction time.
type Option func(*Module)

// WithQueue wires the jobs.Queue the retention-sweep task is enqueued on
// and claimed from. Without it, Register returns ErrQueueRequired: a
// registered task handler with no queue to drain it can never run.
func WithQueue(queue jobs.Queue) Option {
	return func(m *Module) {
		m.queue = queue
		m.retention.queue = queue
	}
}

// WithConfigService wires the *config.Service RetentionService.
// RetentionWindow reads ConfigDefaultRetentionWindow's tenant-resolved
// value through. Without it, every sweep uses defaultRetentionWindow for
// every tenant regardless of any per-tenant override an operator may have
// set -- see ErrConfigServiceRequired's doc comment.
func WithConfigService(cfg *config.Service) Option {
	return func(m *Module) { m.retention.cfg = cfg }
}

// WithTenantLister wires the TenantLister RetentionService.SweepAllTenants
// uses to discover which tenants to sweep from one scheduled task.
// Without it, SweepAllTenants returns ErrTenantListerRequired;
// EnqueueRetentionSweep and SweepTenant need no lister at all.
func WithTenantLister(lister TenantLister) Option {
	return func(m *Module) { m.retention.lister = lister }
}

// WithSharing wires the SharingCreator ExportService.Export mints a
// delivery share through -- typically a host's real *sharing.Service,
// which satisfies SharingCreator structurally (module.go's compile-time
// assertion). Without it, RetentionService and ErasureService are
// unaffected, but Export refuses every call with ErrSharingRequired: see
// that error's own doc comment for why this is a call-time refusal rather
// than one Register enforces the way WithQueue's ErrQueueRequired does.
func WithSharing(s SharingCreator) Option {
	return func(m *Module) { m.export.sharing = s }
}

// WithExportConfigReader wires the live reader ExportService.Export
// resolves a tenant's configured export-delivery-link expiry through --
// NewConfigReader over the config module's Handle is the sanctioned one.
// Without it (the default), Export always falls back to
// defaultExportDeliveryExpiry -- still correct, per this item's own
// declared Default, just never per-tenant-tunable until a host wires
// this. See ExportDeliveryExpiryReader's own doc comment (export.go) for
// why this is a construction-time Option accepting a small interface
// rather than a *config.Service field the way WithConfigService gives
// RetentionService: config.Module.Attach's *config.Service is only
// produced strictly after Kernel.Bootstrap returns, by which point
// NewModule has already run.
func WithExportConfigReader(r ExportDeliveryExpiryReader) Option {
	return func(m *Module) { m.export.cfg = r }
}

// NewModule returns a Module reading and writing audit events through
// auditRepo -- the same *audit.Repository instance the host's dbkit/audit
// wiring already constructs over its own database connection, shared
// rather than opening a second one. Constructing a Module performs no
// I/O.
func NewModule(auditRepo *audit.Repository, opts ...Option) *Module {
	m := &Module{
		retention:  newRetentionService(),
		erasure:    newErasureService(),
		export:     newExportService(),
		auditQuery: NewAuditQuery(auditRepo),
		auditRepo:  auditRepo,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Retention returns the module's RetentionService.
func (m *Module) Retention() *RetentionService { return m.retention }

// Erasure returns the module's ErasureService.
func (m *Module) Erasure() *ErasureService { return m.erasure }

// Export returns the module's ExportService.
func (m *Module) Export() *ExportService { return m.export }

// AuditQuery returns the module's read-only AuditQuery.
func (m *Module) AuditQuery() *AuditQuery { return m.auditQuery }

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module: nothing. compliance sits above
// every business module in the module dependency graph (just below
// admin), and every one of its dependencies on a *business module's*
// participation -- notes, storage's objects, or any other
// pkgcore.RetentionParticipant -- arrives through the host-populated
// pkgcore.Registry.Retention registrar at call time, never through a
// construction-time requirement DependsOn would express. go/sharing is
// different: it is a lower-level platform module (below compliance in the
// graph), imported directly for its Go API the same sanctioned way
// go/billing imports go/metering (SharingCreator's own doc comment) --
// but that is a compile-time package import, not a pkgcore.Module the
// bootstrap set must contain in a particular order, so it still does not
// belong in DependsOn (which is reserved for "this module's Register call
// requires another module to have registered first" -- compliance's own
// Register never reads anything go/sharing's Register declares).
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module. compliance owns no table of its
// own: the retention sweep, right-to-erasure and export gathering are
// pure orchestration over each participant's own table (already
// dual-dialect migrated by that participant's own module) plus
// dbkit/audit's existing audit_events table (already migrated by
// dbkit/audit's own Module). Returning the zero embed.FS
// is not an error: dbkit.MigrationRegistry.Register documents "a module
// with no subdirectory at all for that dialect is treated as declaring
// zero migrations for it, not as an error."
func (m *Module) Migrations() embed.FS { return embed.FS{} }

// Locales implements pkgcore.Module: the descriptions of compliance's
// error codes, in both supported languages with identical id sets.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module. compliance has no HTTP surface,
// so this returns nil -- the same answer go/metering's and go/rbac's
// Module give.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's own contract it
// only declares and wires -- no database call, no outbound call, nothing
// that touches m.auditQuery's underlying connection.
//
// It declares compliance's configuration schema, its permissions and its
// audit vocabulary, registers the periodic retention-sweep task's
// handler on the Jobs seat, attaches the registry's EventBus, AuditActions
// and Retention registrar onto all three orchestration services plus the
// registry's resolved ObjectStore onto ExportService, and registers the
// module's own export-manifests cleanup participant onto the Retention seat
// so the retention sweep also reaps expired export manifests
// (export_cleanup.go). The module's two audited system purposes are
// descriptor data, not declarations made here: the component descriptor
// (component.go) carries them as SystemPurposes, which the assembly
// registers when it closes its Init stage and the transition bridge
// registers inside the module's own registration turn.
// It refuses to proceed without a queue (ErrQueueRequired) -- see
// WithQueue's doc comment. ExportService's SharingCreator is not part of this: it is not
// a pkgcore.Registry seam, so WithSharing wires it directly at Module
// construction time (NewModule's own Option application), and its absence
// is never a reason to refuse Register -- see WithSharing's own doc
// comment for why that check is Export's own, call-time responsibility.
func (m *Module) Register(reg pkgcore.Registrar) error {
	if m.queue == nil {
		return ErrQueueRequired
	}
	if err := reg.ConfigSeat().Add(configItemDecls...); err != nil {
		return err
	}
	if err := reg.PermissionsSeat().Add(
		PermissionAuditRead,
		PermissionRetentionManage,
		PermissionErasureExecute,
		PermissionExportExecute,
	); err != nil {
		return err
	}
	if err := reg.AuditActionsSeat().Add(
		AuditActionRetentionSweep,
		AuditActionErasureRequest,
		AuditActionExportRequest,
	); err != nil {
		return err
	}

	// Subscribe to config's own EventConfigItemChanged so every successful
	// config.Set gets the dedicated audit record onConfigItemChanged
	// writes (config_audit.go). Valid to install regardless of whether
	// config's Register has run yet.
	reg.EventsSeat().Subscribe(config.EventConfigItemChanged, m.onConfigItemChanged)

	bus := reg.EventBus()
	m.retention.retention = reg.RetentionSeat()
	m.retention.bus = bus
	m.retention.actions = reg.AuditActionsSeat()
	m.erasure.retention = reg.RetentionSeat()
	m.erasure.bus = bus
	m.erasure.actions = reg.AuditActionsSeat()
	m.export.retention = reg.RetentionSeat()
	m.export.bus = bus
	m.export.actions = reg.AuditActionsSeat()
	m.export.store = reg.ObjectStore()

	// Register the module's own export-manifest cleanup participant so
	// every retention sweep this module orchestrates also reaps stored
	// export manifests whose delivery share has expired (export_cleanup.go's
	// header comment has the full mechanism). Register runs before any
	// host-populated post-Bootstrap Add, so the reserved
	// compliance.export_manifests name can never collide with a host's
	// participant.
	if err := reg.RetentionSeat().Add(exportManifestsParticipant(m.auditRepo, reg.ObjectStore())); err != nil {
		return err
	}

	// Claim the retention-sweep task handler so a host that drains
	// reg.Jobs.Handlers() onto its jobs.Queue after Bootstrap gets a
	// worker that runs it -- a plain catalog insertion, no I/O.
	if err := reg.JobsSeat().Handle(taskTypeRetentionSweep, retentionSweepHandler{svc: m.retention}); err != nil {
		return err
	}
	// Declare the sweep's periodic schedule alongside its handler:
	// declaring means scheduled, so a host that runs a jobs.Scheduler over
	// the registry's declarations sweeps every tenant at the module's own
	// window cadence -- the schedule point this module does not run
	// itself.
	return reg.SchedulesSeat().Add(retentionSweepSchedule)
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)

// compile-time check that *sharing.Service satisfies SharingCreator
// structurally, so a host constructing a Module with WithSharing can pass
// a real sharing.Module's Service() straight through with no adapter to
// write.
var _ SharingCreator = (*sharing.Service)(nil)
