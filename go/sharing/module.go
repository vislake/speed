package sharing

import (
	"context"
	"embed"
	"errors"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/sharing/locales"
	"github.com/vislake/speed/go/sharing/migrations"
)

// moduleName is sharing's pkgcore.Module.Name(), and the key
// dbkit.MigrationRegistry.Register builds its dependency graph on.
const moduleName = "sharing"

// The permissions sharing contributes to the platform's permission catalog.
// Enforcement belongs to rbac, which decides which role holds which of
// these; sharing only declares that they exist and what they are called.
const (
	// PermissionRead covers reading a share's own metadata and its access
	// log -- Service.Get, Service.ListAccessLog.
	PermissionRead = "sharing:read"
	// PermissionCreate covers minting a new share link -- Service.Create.
	PermissionCreate = "sharing:create"
	// PermissionRevoke covers withdrawing a share -- Service.Revoke.
	PermissionRevoke = "sharing:revoke"
)

// AuditActionSensitiveShareCreate is the one audit action sharing
// contributes this round: rule 4 (docs/internal/07-platform-services.md's
// "sensitive resource sharing needs confirmation" rule)
// requires that creating a share for a resource carrying sensitive personal
// information is itself an audit event. Fired by Service.Create's
// emitSensitiveAudit, only when CreateParams.Sensitive is true -- never
// unconditionally, per this codebase's own "an undeclared-but-unused
// action is dead catalog weight" discipline turned around: a declared
// action every Create call fired regardless of Sensitive would be noise,
// not signal.
const AuditActionSensitiveShareCreate = "sharing.share.create_sensitive"

// The domain events sharing publishes. Names follow pkgcore.EventDecl's
// <module>.<entity>.<action> convention.
const (
	// EventShareCreated announces a new share link.
	EventShareCreated = "sharing.share.created"
	// EventShareAccessed announces one access attempt, granted or denied
	// alike (ShareAccessedPayload.Granted) -- the event
	// docs/internal/07-platform-services.md's "relationship to other
	// modules" section names as flowing into compliance's own audit trail
	// once that module exists. This module only publishes; see events.go's
	// own doc comment.
	EventShareAccessed = "sharing.share.accessed"
	// EventShareRevoked announces a share's revocation, owner-initiated or
	// sweep-initiated alike.
	EventShareRevoked = "sharing.share.revoked"
)

// shareEventDecls is the catalog entry for each of the three events, all
// declared up front in Register even though only Create/Access/Revoke's own
// rounds each publish -- there is exactly one round here, so all three ship
// together.
var shareEventDecls = []pkgcore.EventDecl{
	{
		Type:        EventShareCreated,
		PayloadType: "sharing.ShareCreatedPayload",
		Description: "A new public share link was created.",
	},
	{
		Type:        EventShareAccessed,
		PayloadType: "sharing.ShareAccessedPayload",
		Description: "A share link was accessed, granted or denied.",
	},
	{
		Type:        EventShareRevoked,
		PayloadType: "sharing.ShareRevokedPayload",
		Description: "A share link was revoked, by its owner or by the expiry sweep.",
	},
}

// ConfigDefaultExpiry is the tenant-overridable configuration key backing
// rule 2's default-expiry duration (docs/internal/07-platform-services.md's
// "default expiry" rule): "30 days if the tenant has not configured one". Declared
// on the registry by Register so the value is visible and, eventually,
// editable through go/config's own admin-console machinery; see
// TenantConfigReader's own doc comment for the honest statement that no
// host wires a live reader against it yet in this round -- Service.Create
// falls back to defaultShareExpiry, which is exactly this item's own
// Default value, whenever no TenantConfigReader is wired at all.
const ConfigDefaultExpiry = "sharing.default_expiry"

// configItemDecls is the catalog entry for sharing's one config item,
// declared in Register.
var configItemDecls = []pkgcore.ConfigItem{
	{
		Key:         ConfigDefaultExpiry,
		Type:        "duration",
		Default:     defaultShareExpiry,
		Description: "Default share-link expiry applied when a caller does not specify one.",
		Group:       "sharing",
	},
}

// ErrQueueRequiredForSweep is returned by Module.EnqueueExpirySweep when the
// module was built without WithQueue. Unlike go/storage's identical-shaped
// ErrQueueRequired, this is not enforced at Register time: sweeping is
// optional row hygiene (cleanup.go's own doc comment), never a reason to
// refuse boot, so a host that runs no workers and never calls
// EnqueueExpirySweep is never told about this at all.
var ErrQueueRequiredForSweep = errors.New("sharing: no queue wired; construct the module with WithQueue to schedule the expiry sweep")

// Module implements pkgcore.Module for go/sharing.
//
// # Wiring
//
// A host constructs one with NewModule and hands it to Kernel.Bootstrap.
// Constructing a Module performs no I/O: db is opened and migrated by the
// host before Register is ever called, exactly like every other module in
// this codebase.
//
// # No HTTP surface this round
//
// OpenAPISpec returns nil -- this round ships the Service/Access API only;
// see AGENTS.md's Known limitations for the unauthenticated public endpoint
// a later round mounts.
type Module struct {
	db *gorm.DB

	cfg   TenantConfigReader
	queue jobs.Queue

	svc *Service
}

// Option configures a Module at construction time.
type Option func(*Module)

// WithTenantConfigReader wires the live reader Service.Create resolves a
// tenant's configured default expiry through. Without it (the default),
// Create always falls back to defaultShareExpiry -- still correct, per
// rule 2's own "30 days if the tenant has not configured one" fallback,
// just never per-tenant-tunable until a host wires this. See
// TenantConfigReader's own doc comment.
func WithTenantConfigReader(cfg TenantConfigReader) Option {
	return func(m *Module) { m.cfg = cfg }
}

// WithQueue wires the jobs.Queue Module.EnqueueExpirySweep enqueues the
// expiry-sweep task on. Optional: without it, Register still registers the
// sweep's jobs.Handler (a host may run workers without ever scheduling this
// module's own sweep, or may schedule it once a queue becomes available
// later), and EnqueueExpirySweep itself fails with
// ErrQueueRequiredForSweep only if a caller actually tries to use it.
func WithQueue(queue jobs.Queue) Option {
	return func(m *Module) { m.queue = queue }
}

// NewModule returns a Module whose tables live in db. Constructing a Module
// performs no I/O: opening and migrating db is the host's responsibility,
// done once at startup before Bootstrap ever calls Register.
func NewModule(db *gorm.DB, opts ...Option) *Module {
	m := &Module{db: db}
	for _, opt := range opts {
		opt(m)
	}
	m.svc = NewService(db, m.cfg)
	return m
}

// Service returns the module's Service: Create, Access, Revoke, Get,
// ListAccessLog.
func (m *Module) Service() *Service { return m.svc }

// EnqueueExpirySweep enqueues the expiry-sweep task for the tenant ctx
// carries (see cleanup.go's Service.Sweep for what the task does). ctx must
// carry a tenant; a caller with none gets ErrInternal, since a tenant-less
// sweep is a wiring error. Fails with ErrQueueRequiredForSweep when the
// module was built without WithQueue -- see that Option's own doc comment
// for why this is a call-time refusal rather than a Register-time one.
func (m *Module) EnqueueExpirySweep(ctx context.Context) error {
	if m.queue == nil {
		return ErrQueueRequiredForSweep
	}
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return ErrInternal.WithCause(err)
	}
	_, err = m.queue.Enqueue(ctx, jobs.Task{
		Type:           taskTypeExpirySweep,
		TenantID:       tenant,
		IdempotencyKey: expirySweepIdempotencyKey(tenant),
	})
	return err
}

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module: nothing. sharing sits above jobs in
// docs/internal/01-architecture.md's graph, but its dependence on a queue
// is a seam the host wires (WithQueue), not a requirement that the jobs
// module itself be in the bootstrap set -- the identical reasoning
// go/storage's own DependsOn doc comment gives.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: the descriptions of sharing's error
// codes, in both supported languages with identical id sets.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module. sharing has no HTTP surface this
// round -- see AGENTS.md's Known limitations -- so this returns nil, the
// same "no fragment yet" answer go/config's and go/pki's Module give.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's own contract it
// only declares and wires -- no database call, no outbound call, nothing
// that touches m.db, nothing that enqueues on m.queue.
//
// It declares sharing's permissions, its one audit action, its event
// catalog and its one configuration item; registers the expiry-sweep
// task's jobs.Handler so a host that drains reg.Jobs.Handlers() onto its
// own jobs.Queue gets a worker that reaps expired shares; and attaches the
// registry to Service so Create's event publish and sensitive-resource
// audit emission, and Access's own event publish, read the host's actual
// bus and audit-action registrar at call time. It mounts no HTTP route --
// no OpenAPI fragment exists yet.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if err := reg.Permissions.Add(PermissionRead, PermissionCreate, PermissionRevoke); err != nil {
		return err
	}
	if err := reg.AuditActions.Add(AuditActionSensitiveShareCreate); err != nil {
		return err
	}
	if err := reg.Events.Publishes(shareEventDecls...); err != nil {
		return err
	}
	if err := reg.Config.Add(configItemDecls...); err != nil {
		return err
	}
	if err := reg.Jobs.Handle(taskTypeExpirySweep, expirySweepHandler{svc: m.svc}); err != nil {
		return err
	}
	m.svc.attach(reg)
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
