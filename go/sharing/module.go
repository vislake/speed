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

// PathAccess is where this module's one HTTP route -- the genuinely public,
// unauthenticated share-content route (handler.go's Handler) -- is mounted.
// Exported, mirroring go/config's PathPublic/PathSystemFeatures precedent
// exactly, so a host names this route in a tenancy.WithAllowlist call (and
// in the permission-gate table that decides which mounted routes need no
// permission, e.g. examples/reference-app's DemoRouteRules entry declaring
// it public) without stringly
// duplicating the literal -- see Register's own doc comment for the
// allowlisting obligation this constant exists to keep honest.
const PathAccess = "/api/v1/sharing/access"

// PathShares is where this module's five owner-facing operations (create,
// list, get, revoke, list access log -- api/openapi.yaml's
// sharing_createShare/sharing_listShares/sharing_getShare/
// sharing_revokeShare/sharing_listShareAccessLog) are mounted. Exported for
// the identical reason PathAccess is: a host names this path in its own
// permission-gate table (e.g. examples/reference-app's DemoRouteRules)
// without stringly duplicating the literal.
//
// Unlike PathAccess, this path is an ORDINARY tenant-scoped surface: a host
// must run it downstream of tenancy.Middleware (never allowlisted) and gate
// it on this module's own PermissionRead/PermissionCreate/PermissionRevoke
// permissions through its authorization layer -- Handler performs no
// authorization of its own, exactly like every other module's HTTP surface
// in this codebase. See Register's own doc comment for the full contrast
// with PathAccess's allowlisting obligation.
const PathShares = "/api/v1/sharing/shares"

// moduleName is sharing's module name, and the key
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

// AuditActionSensitiveShareCreate is sharing's one audit action: creating a
// share for a resource carrying sensitive personal information is itself an
// audit event (Share.Sensitive, model.go). Fired by Service.Create's
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
	// alike (ShareAccessedPayload.Granted). This module only publishes;
	// see events.go's own doc comment.
	EventShareAccessed = "sharing.share.accessed"
	// EventShareRevoked announces a share's revocation, owner-initiated or
	// sweep-initiated alike.
	EventShareRevoked = "sharing.share.revoked"
)

// shareEventDecls is the catalog entry for each of the three events, all
// declared up front in Register so a subscriber can declare its interest
// before any publish happens.
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
// the default-expiry fallback: "30 days if the tenant has not configured
// one". Declared on the registry by Register so the value is visible to
// go/config-backed hosts and editable through their own configuration
// machinery. Service.Create resolves it through the TenantConfigReader
// seam only when a host wires one (see TenantConfigReader's own doc
// comment); without a reader it falls back to defaultShareExpiry, which is
// exactly this item's own Default value.
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

// Module implements the module contract for go/sharing.
//
// # Wiring
//
// A host constructs one with NewModule and hands it to the assembly.
// Constructing a Module performs no I/O: db is opened and migrated by the
// host before Register is ever called, exactly like every other module in
// this codebase.
//
// # HTTP surface
//
// OpenAPISpec returns this module's real fragment (api/openapi.yaml): one
// genuinely public, unauthenticated route (handler.go's Handler,
// PathAccess) that resolves a bearer token into the share it names and
// streams the resource behind it, plus five owner-facing operations
// (PathShares) a resource's own owner uses to create, list, get,
// revoke and audit their own shares. See Register's own doc comment for the
// contrasting gating obligation each of the two paths places on a host, and
// WithResourceResolver for the seam that turns a Share's ResourceRef into
// actual bytes.
type Module struct {
	db *gorm.DB

	cfg      TenantConfigReader
	queue    jobs.Queue
	resolver ResourceResolver

	svc     *Service
	handler *Handler
}

// Option configures a Module at construction time.
type Option func(*Module)

// WithTenantConfigReader wires the live reader Service.Create resolves a
// tenant's configured default expiry through. Without it (the default),
// Create always falls back to defaultShareExpiry -- still correct, the
// "30 days if the tenant has not configured one" fallback, just not
// per-tenant-tunable. See TenantConfigReader's own doc comment.
func WithTenantConfigReader(cfg TenantConfigReader) Option {
	return func(m *Module) { m.cfg = cfg }
}

// WithQueue wires the jobs.Queue Module.EnqueueExpirySweep enqueues the
// expiry-sweep task on. Optional: without it, Register still registers the
// sweep's jobs.Handler (a host may run workers without ever scheduling this
// module's own sweep), and EnqueueExpirySweep itself fails with
// ErrQueueRequiredForSweep only if a caller actually tries to use it.
func WithQueue(queue jobs.Queue) Option {
	return func(m *Module) { m.queue = queue }
}

// WithResourceResolver wires the seam Handler's public access route reads a
// granted share's ResourceRef bytes through -- see resolver.go's
// ResourceResolver for the full seam contract. Without it (the default,
// nil), the route's access decision still runs in full (a refused access
// still answers ErrNotAccessible exactly as it would with a resolver
// wired), but a granted one always answers ErrResourceUnavailable, since
// there is nowhere to read the resource's bytes from.
func WithResourceResolver(resolver ResourceResolver) Option {
	return func(m *Module) { m.resolver = resolver }
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

// Service returns the module's Service: Create, Access, AccessPublic,
// Revoke, Get, ListAccessLog.
func (m *Module) Service() *Service { return m.svc }

// Handler returns the module's HTTP Handler, once Register has built it
// (nil before then). Exposed for a host that needs to reach it directly
// rather than through reg.Routes.Routes() -- e.g. to mount it under a
// different outer path, or to wrap it in additional host-specific
// middleware before it reaches Register's own mount.
func (m *Module) Handler() *Handler { return m.handler }

// EnqueueExpirySweep enqueues the expiry-sweep task for the tenant ctx
// carries (see cleanup.go's Service.Sweep for what the task does). The
// sweep's default schedule is the module's own: Register declares it on
// the pkgcore.ComponentRegistry.Schedules seat (expirySweepSchedule, a per-tenant
// task at the sweep's own window), so a host that runs a jobs.Scheduler
// sweeps every tenant without writing a schedule point of its own; this
// method remains the manual entry point. The task's window-scoped
// idempotency key (expirySweepIdempotencyKey) collapses the enqueues of
// one expirySweepWindowSize window -- a scheduler with two replicas ticking
// in the same window, a manual re-run -- into one job, so a tenant is never
// swept by two workers at once. An enqueue whose clock has moved into a
// later window (expirySweepWindowStart) is a new job and runs again: this
// is what makes the sweep periodic on queues whose idempotency is
// unconditional, and what keeps one dead-lettered sweep from poisoning its
// tenant forever -- see expirySweepIdempotencyKey's doc comment for the
// full window semantics. The window is read from the module service's
// clock (Service.now, the same seam Service.Sweep reads when the task
// runs): one clock drives the enqueue-time window and the run-time rows in
// production, and a test pins it once for a deterministic enqueue.
//
// ctx must carry a tenant; a caller with none gets ErrInternal, since a
// tenant-less sweep is a wiring error. Fails with ErrQueueRequiredForSweep
// when the module was built without WithQueue -- see that Option's own doc
// comment for why this is a call-time refusal rather than a Register-time
// one.
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
		IdempotencyKey: expirySweepIdempotencyKey(tenant, expirySweepWindowStart(m.svc.now())),
	})
	return err
}

// Name implements the module contract.
func (m *Module) Name() string { return moduleName }

// DependsOn implements the module contract: nothing. sharing's dependence on a
// queue is a seam the host wires (WithQueue), not a requirement that the
// jobs module itself be in the bootstrap set -- the identical reasoning
// go/storage's own DependsOn doc comment gives.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements the module contract.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements the module contract: the descriptions of sharing's error
// codes, in both supported languages with identical id sets.
func (m *Module) Locales() embed.FS { return locales.FS }

// openAPISpecYAML is sharing's OpenAPI fragment, embedded from api/ so the
// spec -- and the generated ServerInterface and types derived from it --
// travels inside the module binary, exactly as go/storage's identical
// openAPISpecYAML does for its own fragment.
//
//go:embed api/openapi.yaml
var openAPISpecYAML []byte

// OpenAPISpec implements the module contract: sharing's own OpenAPI fragment,
// embedded from api/openapi.yaml. The fragment is the single source of
// this module's HTTP operations -- the api package's generated types and
// ServerInterface (api/sharing-server.gen.go, regenerated by task api:gen)
// derive from it, and Handler implements that interface (see handler.go) --
// so a spec change without a matching handler change cannot compile.
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements the module contract. Per the interface's own contract it
// only declares and wires -- no database call, no outbound call, nothing
// that touches m.db, nothing that enqueues on m.queue.
//
// It declares sharing's permissions, its one audit action, its event
// catalog and its one configuration item; registers the expiry-sweep
// task's jobs.Handler so a host that drains reg.Jobs.Handlers() onto its
// own jobs.Queue gets a worker that reaps expired shares; registers the
// module's access-log retention participant on reg.Retention so a host's
// compliance retention sweep reaps access-log entries past the tenant's
// retention window (retention_participant.go); and attaches the
// registry to Service so Create's event publish and sensitive-resource
// audit emission, and Access's own event publish, read the host's actual
// bus, audit-action registrar and KVStore at call time (the latter is what
// ratelimit.go's rateLimiter reads for Create's and AccessPublic's rate
// limits).
//
// It also builds and mounts this module's HTTP routes: PathAccess (one
// operation) and PathShares (five owner-facing operations), both served
// by the SAME *Handler instance -- Handler
// implements the whole generated api.ServerInterface, and mounting it twice
// under two different registrar paths works correctly because oapi-codegen's
// generated router dispatches on the request's own literal path regardless
// of which host-level mount pattern matched it there first (a request for
// PathAccess never matches a PathShares-rooted pattern, and vice versa, so
// each mount's own host-applied gating -- allowlisted or permission-checked
// -- only ever wraps the requests it is actually meant to). Routes.Mount is
// a plain registration, no I/O, so Register's no-I/O contract stands.
//
// PathAccess is NOT an ordinary tenant-scoped route: a host MUST allowlist
// the exact pair (http.MethodGet, sharing.PathAccess) with
// tenancy.WithAllowlist, the same mechanism go/config's own two pre-auth
// endpoints already use, before this route can ever serve a genuinely
// anonymous visitor -- without that allowlist entry, tenancy.Middleware's
// own fail-closed default refuses every request here with 403 before
// Handler is ever reached, since the request carries no tenant claim by
// design. Whichever gate table a host
// layers on top of Routes.Routes() (examples/reference-app's
// DemoRouteRules, for instance) must declare this same path public, for the
// identical reason: an unauthenticated visitor holds no
// Subject an authorization layer could evaluate a permission against either.
//
// PathShares is the OPPOSITE shape: an ordinary tenant-scoped surface, never
// allowlisted, that a host runs downstream of tenancy.Middleware exactly
// like every other module's fragment and gates on
// PermissionRead/PermissionCreate/PermissionRevoke through its own
// authorization layer (rbac.RequirePermissionFunc, in the reference app) --
// Handler performs no authorization decision of its own for these five
// operations, reading only the tenant tenancy.Middleware already resolved
// into the request context.
func (m *Module) Register(reg *pkgcore.ComponentRegistry) error {
	if err := reg.PermissionsSeat().Add(PermissionRead, PermissionCreate, PermissionRevoke); err != nil {
		return err
	}
	if err := reg.AuditActionsSeat().Add(AuditActionSensitiveShareCreate); err != nil {
		return err
	}
	if err := reg.EventsSeat().Publishes(shareEventDecls...); err != nil {
		return err
	}
	if err := reg.ConfigSeat().Add(configItemDecls...); err != nil {
		return err
	}
	if err := reg.JobsSeat().Handle(taskTypeExpirySweep, expirySweepHandler{svc: m.svc}); err != nil {
		return err
	}
	// Declare the sweep's periodic schedule alongside its handler:
	// declaring means scheduled, so a host that runs a jobs.Scheduler over
	// the registry's declarations sweeps every tenant at the module's own
	// window cadence without writing a schedule point of its own. The
	// declaration follows the handler registration unconditionally, the
	// same way the handler itself does -- no declaration may outlive its
	// executor.
	if err := reg.SchedulesSeat().Add(expirySweepSchedule); err != nil {
		return err
	}
	m.svc.attach(reg)

	// Register the module's own access-log retention participant so every
	// retention sweep a host's compliance layer orchestrates also reaps
	// this tenant's access-log entries whose recorded access time has
	// fallen past the tenant's retention window (retention_participant.go's
	// file comment has the mechanism in full). Register runs before any
	// host-populated post-Bootstrap Add, so the reserved
	// sharing.access_log name can never collide with a host's participant,
	// exactly as compliance's own export-manifests reservation works.
	if err := reg.RetentionSeat().Add(NewAccessLogRetentionParticipant(m.svc.AccessLogs())); err != nil {
		return err
	}

	// Built here, not in NewModule, deliberately: every Option a caller
	// passed to NewModule (WithResourceResolver included) has already run
	// by the time Register is called, so the handler serves the resolver
	// the host actually configured -- the same reasoning go/storage's
	// identical Register-time Handler construction documents.
	m.handler = NewHandler(m.svc, m.resolver)
	reg.RoutesSeat().Mount(PathAccess, m.handler)
	reg.RoutesSeat().Mount(PathShares, m.handler)
	return nil
}
