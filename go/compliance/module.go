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
// sweep hard-deletes it (docs/internal/04-data-and-tenancy.md's delete-
// semantics section, §2). It is declared, tenant-overridable
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
		Description: "How long a data-export download link (minted through go/sharing) stays valid. Deliberately much shorter than a general-purpose share's own default expiry, since an export bundles a subject's complete personal data.",
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
// and claimed from. Without it, Register returns ErrQueueRequired --
// mirroring go/storage's identical WithQueue/ErrQueueRequired pattern,
// for the identical reason: a registered task handler with no queue to
// drain it can never run.
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

// NewModule returns a Module reading and writing audit events through
// auditRepo -- the same *audit.Repository instance the host's dbkit/audit
// wiring already constructs over its own database connection, sharing it
// rather than opening a second one (the same sharing shape
// cmd/server/server.go's own audit.New(db) wiring already establishes for
// the reference app's notes module). Constructing a Module performs no
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
// every business module in docs/internal/01-architecture.md's graph (just
// below admin), and every one of its dependencies on a *business module's*
// participation -- notes, storage's objects, or any other future
// pkgcore.RetentionParticipant -- arrives through the host-populated
// pkgcore.Registry.Retention registrar at call time, never through a
// construction-time requirement DependsOn would express -- mirroring
// go/storage's and go/metering's identical answer for the identical
// reason. go/sharing is different: it is a lower-level *platform* module
// (docs/internal/01-architecture.md places it below compliance, alongside
// billing/ai-gateway/integration), imported directly for its Go API the
// same sanctioned way go/billing imports go/metering (SharingCreator's own
// doc comment) -- but that is a compile-time package import, not a
// pkgcore.Module the bootstrap set must contain in a particular order, so
// it still does not belong in DependsOn (which is reserved for "this
// module's Register call requires another module to have registered
// first" -- compliance's own Register never reads anything go/sharing's
// Register declares).
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module. compliance owns no table of its
// own this round: the retention sweep, right-to-erasure and export
// gathering are pure orchestration over each participant's own table
// (already dual-dialect migrated by that participant's own module) plus
// dbkit/audit's existing audit_events table (already migrated by
// dbkit/audit's own Module) -- see AGENTS.md's "Why no table" section for
// the reasoning spelled out in full, including why an erasure-request log
// was considered and rejected for this round. Returning the zero embed.FS
// is not an error: dbkit.MigrationRegistry.Register documents "a module
// with no subdirectory at all for that dialect is treated as declaring
// zero migrations for it, not as an error."
func (m *Module) Migrations() embed.FS { return embed.FS{} }

// Locales implements pkgcore.Module: the descriptions of compliance's
// error codes, in both supported languages with identical id sets.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module. compliance has no HTTP surface
// this round -- see AGENTS.md's Known limitations -- so this returns nil,
// the same "no fragment yet" answer go/config's, go/pki's and go/metering's
// Module give.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's own contract it
// only declares and wires -- no database call, no outbound call, nothing
// that touches m.auditQuery's underlying connection.
//
// It declares compliance's configuration schema, its permissions and its
// audit vocabulary, registers the periodic retention-sweep task's
// handler on reg.Jobs, registers this module's two audited system
// purposes, and attaches the registry's EventBus, AuditActions and
// Retention registrar onto all three orchestration services plus the
// registry's resolved ObjectStore onto ExportService. It refuses to
// proceed without a queue (ErrQueueRequired) -- see WithQueue's doc
// comment. ExportService's SharingCreator is not part of this: it is not
// a pkgcore.Registry seam, so WithSharing wires it directly at Module
// construction time (NewModule's own Option application), and its absence
// is never a reason to refuse Register -- see WithSharing's own doc
// comment for why that check is Export's own, call-time responsibility.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if m.queue == nil {
		return ErrQueueRequired
	}
	if err := reg.Config.Add(configItemDecls...); err != nil {
		return err
	}
	if err := reg.Permissions.Add(
		PermissionAuditRead,
		PermissionRetentionManage,
		PermissionErasureExecute,
		PermissionExportExecute,
	); err != nil {
		return err
	}
	if err := reg.AuditActions.Add(
		AuditActionRetentionSweep,
		AuditActionErasureRequest,
		AuditActionExportRequest,
	); err != nil {
		return err
	}

	pkgcore.RegisterSystemPurpose(SystemPurposeRetentionSweep)
	pkgcore.RegisterSystemPurpose(SystemPurposeRightToErasure)

	// Subscribe to config's own EventConfigItemChanged so every successful
	// config.Set finally gets the dedicated audit record
	// docs/internal/11-cross-cutting.md's "变更审计" bullet promised for
	// this module's own round -- see onConfigItemChanged's doc comment
	// (config_audit.go). Valid to install regardless of whether config's
	// Register has run yet, mirroring go/dbkit/audit's own Module.Register
	// doc comment on the identical point.
	reg.Events.Subscribe(config.EventConfigItemChanged, m.onConfigItemChanged)

	bus := reg.EventBus()
	m.retention.retention = reg.Retention
	m.retention.bus = bus
	m.retention.actions = reg.AuditActions
	m.erasure.retention = reg.Retention
	m.erasure.bus = bus
	m.erasure.actions = reg.AuditActions
	m.export.retention = reg.Retention
	m.export.bus = bus
	m.export.actions = reg.AuditActions
	m.export.store = reg.ObjectStore()

	// Claim the retention-sweep task handler so a host that drains
	// reg.Jobs.Handlers() onto its jobs.Queue after Bootstrap gets a
	// worker that runs it -- a plain catalog insertion, no I/O.
	return reg.Jobs.Handle(taskTypeRetentionSweep, retentionSweepHandler{svc: m.retention})
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)

// compile-time check that *sharing.Service satisfies SharingCreator
// structurally, so a host constructing a Module with WithSharing can pass
// a real sharing.Module's Service() straight through with no adapter to
// write -- the identical proof go/billing/module.go gives for
// *metering.Aggregator satisfying its own UsageReader.
var _ SharingCreator = (*sharing.Service)(nil)
