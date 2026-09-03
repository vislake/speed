package storage

import (
	"embed"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/storage/locales"
	"github.com/vislake/speed/go/storage/migrations"
)

// The permissions storage contributes to the platform's permission catalog.
// Enforcement belongs to rbac, which decides which role holds which of
// these; storage only declares that they exist and what they are called.
const (
	// PermissionRead covers reading object metadata and their derivatives.
	PermissionRead = "storage:read"
	// PermissionWrite covers declaring, completing and deleting objects.
	PermissionWrite = "storage:write"
)

// The audit actions storage contributes to the audit vocabulary. The object
// lifecycle is audited at its three irreversible transitions: an upload is
// declared, an upload's bytes are finalized, and an object is permanently
// deleted. Pure reads and in-flight progress need no audit row.
const (
	AuditActionObjectCreate   = "storage.object.create"
	AuditActionObjectComplete = "storage.object.complete"
	AuditActionObjectDelete   = "storage.object.delete"
)

// The domain events storage publishes about its objects. Names follow
// pkgcore.EventDecl's <module>.<entity>.<action> convention.
const (
	// EventObjectCompleted announces that an object's bytes finished
	// revalidation and the object is permanently readable. Subscribers
	// include whatever must react to a media asset finally being there.
	EventObjectCompleted = "storage.object.completed"
	// EventObjectDeleted announces that an object, its bytes and its
	// derivatives are gone for good.
	EventObjectDeleted = "storage.object.deleted"
)

// objectEventDecls is the catalog entry for each of the two object events.
//
// They are declared here, in the module's single Register call, because
// pkgcore.Registry is where the platform's event catalog is assembled --
// observability, compliance and integration enumerate the declarations
// without subscribing to any of them. Publishing arrived with the rounds
// that own each transition: ObjectService.Complete publishes
// EventObjectCompleted, and the deletion round's service publishes
// EventObjectDeleted. Register declares both up front either way, so the
// catalog is already complete for the subscribers that will listen for
// them.
var objectEventDecls = []pkgcore.EventDecl{
	{
		Type:        EventObjectCompleted,
		PayloadType: "storage.ObjectCompleted",
		Description: "An object's bytes passed revalidation and the object is finalized.",
	},
	{
		Type:        EventObjectDeleted,
		PayloadType: "storage.ObjectDeleted",
		Description: "An object, its bytes and its derivatives were permanently removed.",
	},
}

// Module implements pkgcore.Module for go/storage: object metadata and its
// derivatives -- the rows that describe media bytes living in the host's
// ObjectStore under keys this module builds and never exposes.
//
// Three services drive the object lifecycle, all built by NewModule from
// the same repositories and queue, all inert until Register attaches the
// registry: ObjectService (m.svc) drives uploads through their lifecycle
// states -- Create declares an upload, Upload streams its bytes into the
// host's ObjectStore, Complete runs the revalidation pipeline over them
// and finalizes the row, and Get, OpenContent and List serve completed
// objects; DeriveService (m.derive) turns a completed image object's
// bytes into its thumbnail derivative, claimed from the queue by the
// handler Register registers; and LifecycleService (m.life) ends object
// life -- Delete removes an object and everything it names, Sweep runs
// one tenant's periodic cleanup, EnqueueExpirySweep schedules it; and the
// Handler (m.handler) serves the HTTP surface in front of all three -- the
// seven operations of the api/ fragment -- which Register mounts at the
// apiPath prefix on the host's router.
type Module struct {
	// db is the connection storage's tables live in. It is opened and
	// migrated by the host before Register is ever called; the module
	// performs no I/O of its own during registration.
	db *gorm.DB

	// objects and derivatives are the module's metadata repositories. They
	// are built by NewModule -- constructing them opens nothing.
	objects     *ObjectRepository
	derivatives *DerivativeRepository

	// queue is the jobs.Queue the module's asynchronous work -- the
	// thumbnail-derive task the completion pipeline enqueues and the
	// expiry-sweep task hosts schedule through LifecycleService -- runs on
	// (WithQueue). Register refuses to proceed without one.
	queue jobs.Queue

	// svc is the module's ObjectService, the runtime that drives the
	// lifecycle. NewModule builds it from the policy fields below, after
	// the With* options have been applied, and Register hands it the
	// registry its host seams come from (attach). Hosts drive uploads
	// through it via ObjectService().
	svc *ObjectService

	// derive is the module's DeriveService, the runtime that turns a
	// completed image object's bytes into its thumbnail derivative. It is
	// the backing service of the thumbnail-derive task's handler, which
	// Register claims on reg.Jobs so a host that drains the registry's
	// handlers onto its queue gets a worker that produces thumbnails.
	derive *DeriveService

	// life is the module's LifecycleService, the deletion and expiry
	// runtime: hosts delete objects through it, schedule a tenant's
	// periodic sweep through EnqueueExpirySweep, and Register claims the
	// expiry-sweep task's handler on reg.Jobs the same way it claims the
	// derive one.
	life *LifecycleService

	// handler serves the module's HTTP surface: the seven operations the
	// api/ fragment defines, implemented in handler.go behind the generated
	// api.ServerInterface, mounted by Register at apiPath on the host's
	// router. Built by Register, not NewModule -- after every With* option
	// has run -- for the reason Register's own doc comment records.
	handler *Handler

	// The policy ObjectService enforces. They are defaults a host
	// overrides through the With* options below, resolved into the
	// service's config at construction: maxUploadBytes caps declared
	// uploads, uploadTTL bounds the upload window, maxImagePixels caps
	// image dimensions, maxObjectLifetime caps requested retentions, and
	// allowedTypes (nil resolving to the module default) gates declared
	// and probed media types alike.
	maxUploadBytes    int64
	maxImagePixels    int64
	derivativeMaxEdge int
	uploadTTL         time.Duration
	maxObjectLifetime time.Duration
	allowedTypes      []string
}

// Option configures a Module at construction time.
type Option func(*Module)

// WithQueue wires the jobs.Queue the module's asynchronous work runs on:
// the thumbnail-derive task ObjectService.Complete enqueues for every
// finalized image object, and the expiry-sweep task EnqueueExpirySweep
// schedules for a tenant's periodic cleanup. Upload finalization itself is
// synchronous -- Complete runs the revalidation pipeline in the caller's
// request before it returns -- so the queue carries only the work that may
// follow a finalize, and the pipeline warns when an enqueue fails rather
// than failing a finalize that stands on its own.
//
// It is REQUIRED: Register returns ErrQueueRequired without one. A storage
// module that accepts uploads it can never finish processing is worse than
// a boot failure, and Register is the single place the platform checks
// wiring completeness before anything runs.
func WithQueue(queue jobs.Queue) Option {
	return func(m *Module) { m.queue = queue }
}

// WithMaxUploadBytes caps how large a single uploaded object may be,
// declared size included. Values below 1 are ignored.
func WithMaxUploadBytes(max int64) Option {
	return func(m *Module) {
		if max > 0 {
			m.maxUploadBytes = max
		}
	}
}

// WithMaxImagePixels caps the pixel count of images the module processes
// for derivatives. Values below 1 are ignored.
func WithMaxImagePixels(max int64) Option {
	return func(m *Module) {
		if max > 0 {
			m.maxImagePixels = max
		}
	}
}

// WithDerivativeMaxEdge caps the longer edge of generated derivatives, in
// pixels. Values below 1 are ignored.
func WithDerivativeMaxEdge(max int) Option {
	return func(m *Module) {
		if max > 0 {
			m.derivativeMaxEdge = max
		}
	}
}

// WithUploadTTL sets how long a declared upload may stay unfinished before
// the bytes are never coming and the declaration is reclaimed. Non-positive
// values are ignored.
func WithUploadTTL(ttl time.Duration) Option {
	return func(m *Module) {
		if ttl > 0 {
			m.uploadTTL = ttl
		}
	}
}

// WithMaxObjectLifetime sets the longest an object may be retained before
// it expires. Non-positive values are ignored.
func WithMaxObjectLifetime(lifetime time.Duration) Option {
	return func(m *Module) {
		if lifetime > 0 {
			m.maxObjectLifetime = lifetime
		}
	}
}

// WithAllowedTypes restricts which media types storage accepts. Types are
// exact, lowercase media types such as "image/png" or "application/pdf".
// ObjectService enforces the restriction twice: at create time against the
// declared type, and at complete time against the media type probed from
// the stored bytes -- a type that never passes either gate cannot become a
// completed object.
//
// The default -- nil -- resolves to the module's own default allowlist of
// image/jpeg and image/png (defaultAllowedTypes), so a host that
// configures nothing gets a real restriction, never an open door. A host
// that needs more types calls WithAllowedTypes with the full set it wants;
// each call replaces the whole set.
func WithAllowedTypes(types ...string) Option {
	return func(m *Module) {
		m.allowedTypes = append([]string(nil), types...)
	}
}

// NewModule returns a Module whose metadata tables live in db. Constructing
// a Module performs no I/O: opening and migrating db is the host's
// responsibility, done once at startup before Bootstrap ever calls
// Register.
func NewModule(db *gorm.DB, opts ...Option) *Module {
	m := &Module{
		db:                db,
		objects:           NewObjectRepository(db),
		derivatives:       NewDerivativeRepository(db),
		maxUploadBytes:    defaultMaxUploadBytes,
		maxImagePixels:    defaultMaxImagePixels,
		derivativeMaxEdge: defaultDerivativeMaxEdge,
		uploadTTL:         defaultUploadTTL,
		maxObjectLifetime: defaultMaxObjectLifetime,
	}
	for _, opt := range opts {
		opt(m)
	}
	m.svc = newObjectService(m.objects, m.queue, serviceConfig{
		maxUploadBytes:    m.maxUploadBytes,
		maxImagePixels:    m.maxImagePixels,
		uploadTTL:         m.uploadTTL,
		maxObjectLifetime: m.maxObjectLifetime,
		allowedTypes:      m.allowedTypes,
	})
	m.derive = newDeriveService(m.objects, m.derivatives, m.derivativeMaxEdge, m.maxImagePixels)
	m.life = newLifecycleService(m.objects, m.derivatives, m.queue)
	return m
}

// The package defaults the With* options override. Each is a named
// constant, never a magic number scattered across call sites, and each
// value is the one the reference-app class of deployments starts from.
const (
	// defaultMaxUploadBytes is the default single-object ceiling: 100 MiB.
	defaultMaxUploadBytes = 100 << 20
	// defaultMaxImagePixels is the default image-processing ceiling, in
	// pixels: 40 megapixels, comfortably above any camera the reference
	// app's dental imagery produces.
	defaultMaxImagePixels = 40_000_000
	// defaultDerivativeMaxEdge is the default longer-edge cap for generated
	// derivatives, in pixels.
	defaultDerivativeMaxEdge = 320
	// defaultUploadTTL is how long a declared upload may stay unfinished.
	defaultUploadTTL = 30 * time.Minute
	// defaultMaxObjectLifetime is the default retention ceiling.
	defaultMaxObjectLifetime = 90 * 24 * time.Hour
)

// defaultAllowedTypes is the module's default media-type allowlist,
// applied when a host configures none (see WithAllowedTypes). JPEG and PNG
// cover the reference-app class of uploads -- dental imagery -- and
// nothing else; a host that needs wider types configures the set
// explicitly.
var defaultAllowedTypes = []string{"image/jpeg", "image/png"}

// Objects returns the module's object-metadata repository, the sanctioned
// way to read and write object rows directly -- for hosts that manage rows
// outside the service lifecycle (seeding, migration tooling, inspection).
// Lifecycle transitions and their policies belong to ObjectService, which
// is what hosts drive uploads through.
func (m *Module) Objects() *ObjectRepository { return m.objects }

// ObjectService returns the module's upload-lifecycle runtime: Create,
// Upload, Complete, Get, OpenContent and List. It is safe to call after
// NewModule; the service resolves its policy at construction and its host
// seams (object store, event bus) when Register attaches the registry --
// before Register, its methods that need the store fail closed with
// storage.store_unavailable.
func (m *Module) ObjectService() *ObjectService { return m.svc }

// DeriveService returns the module's thumbnail-derivation runtime: the
// service Register's registered thumbnail-derive handler is backed by, and
// the synchronous entry point for a host that wants one object's thumbnail
// derived in-call instead of through the queue. Like ObjectService it is
// safe to call after NewModule and inert until Register attaches the
// registry -- before then its store-needing methods fail closed.
func (m *Module) DeriveService() *DeriveService { return m.derive }

// LifecycleService returns the module's deletion and expiry runtime: the
// service hosts delete objects through, Register's registered expiry-sweep
// handler is backed by, and whose EnqueueExpirySweep a host with workers
// calls on its own schedule -- the module runs no timer of its own. Like
// the other two services it is safe to call after NewModule and inert
// until Register attaches the registry.
func (m *Module) LifecycleService() *LifecycleService { return m.life }

// Derivatives returns the module's derivative-metadata repository.
func (m *Module) Derivatives() *DerivativeRepository { return m.derivatives }

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module: nothing.
//
// This is a real answer, not a stub. storage sits above jobs in
// docs/internal/01-architecture.md's graph, but its dependence on a queue
// is a seam the host wires (WithQueue), not a requirement that the jobs
// module itself be in the bootstrap set -- DependsOn enumerates only
// modules in that set, exactly as org's identical answer explains.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: the descriptions of storage's error
// codes, in both supported languages with identical id sets.
func (m *Module) Locales() embed.FS { return locales.FS }

// apiPath is the common prefix storage's HTTP routes are mounted at (see
// Register below). It must agree with the "paths:" keys of this module's
// api/openapi.yaml fragment: api.HandlerFromMux registers the fragment's
// full method+path patterns on Handler's inner mux, and mounting at this
// prefix here only tells the host's outer mux which requests to hand to
// Handler at all -- exactly as notes' and org's identical constants do for
// their own fragments.
const apiPath = "/api/v1/storage"

// openAPISpecYAML is storage's OpenAPI fragment, embedded from api/ so the
// spec -- and the generated ServerInterface and types derived from it --
// travels inside the module binary.
//
//go:embed api/openapi.yaml
var openAPISpecYAML []byte

// OpenAPISpec implements pkgcore.Module: storage's own OpenAPI fragment,
// embedded from api/openapi.yaml. The fragment is the single source of this
// module's HTTP surface -- the api package's generated types and
// ServerInterface (api/storage-server.gen.go, regenerated by task api:gen)
// derive from it, and Handler implements that interface (see handler.go) --
// per docs/internal/21-api-contract.md's spec-first decision. storage is
// the third module to ship a fragment, after notes and org.
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements pkgcore.Module. Per the interface's contract it only
// declares and wires -- no database call, no outbound call, nothing that
// touches m.db, nothing that enqueues on m.queue.
//
// It contributes storage's permissions, its audit vocabulary and its event
// catalog, registers the handlers of the two task types the module's
// services schedule -- the thumbnail-derive task ObjectService.Complete
// enqueues and the expiry-sweep task EnqueueExpirySweep schedules -- and
// validates the one wiring the module cannot live without: a queue,
// without which Register returns ErrQueueRequired (see WithQueue's doc
// comment for the reasoning).
//
// The module's HTTP surface is registered too: Handler is built here and
// mounted on the host's router at apiPath. Routes.Mount is a plain
// registration, no I/O, so Register's no-I/O contract stands.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if m.queue == nil {
		return ErrQueueRequired
	}
	if err := reg.Permissions.Add(PermissionRead, PermissionWrite); err != nil {
		return err
	}
	if err := reg.AuditActions.Add(
		AuditActionObjectCreate,
		AuditActionObjectComplete,
		AuditActionObjectDelete,
	); err != nil {
		return err
	}
	if err := reg.Events.Publishes(objectEventDecls...); err != nil {
		return err
	}
	// Hand the registry to the three services: its ObjectStore and
	// EventBus are the seams Create, Upload, Complete, OpenContent, Delete
	// and the expiry sweep read at call time. Plain assignments -- no I/O
	// -- so Register's no-I/O contract stands.
	m.svc.attach(reg)
	m.derive.attach(reg)
	m.life.attach(reg)
	// Claim the handlers of the tasks the services enqueue and schedule,
	// so a host that drains reg.Jobs.Handlers() onto its jobs.Queue after
	// Bootstrap gets a worker that produces thumbnails and sweeps expiry.
	// Jobs.Handle is a plain catalog insertion, no I/O.
	if err := reg.Jobs.Handle(taskTypeDeriveThumbnail, deriveHandler{svc: m.derive}); err != nil {
		return err
	}
	if err := reg.Jobs.Handle(taskTypeExpirySweep, expirySweepHandler{svc: m.life}); err != nil {
		return err
	}
	// Build and mount the module's HTTP surface. Handler is built here, not
	// in NewModule, deliberately: every Option a caller passed to NewModule
	// has already run by the time Register is called (Bootstrap calls
	// Register only after NewModule has returned), so the handler serves the
	// service and repository instances the host actually configured -- the
	// same m.svc, m.life, m.objects and m.derivatives the job handlers above
	// are bound to -- never versions captured before the options ran.
	// Routes.Mount is a plain registration: no I/O, exactly as the
	// no-I/O contract this doc comment promises requires.
	m.handler = NewHandler(m.svc, m.life, m.objects, m.derivatives)
	reg.Routes.Mount(apiPath, m.handler)
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
