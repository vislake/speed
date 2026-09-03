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

// ObjectCompletedPayload is the JSON payload of EventObjectCompleted.
// ObjectID is the object's id, Size its finalized byte size, and MIME the
// media type the bytes were validated to carry.
type ObjectCompletedPayload struct {
	ObjectID string `json:"object_id"`
	Size     int64  `json:"size"`
	MIME     string `json:"mime"`
}

// ObjectDeletedPayload is the JSON payload of EventObjectDeleted. ObjectID
// is the id of the deleted object.
type ObjectDeletedPayload struct {
	ObjectID string `json:"object_id"`
}

// Module implements pkgcore.Module for go/storage: object metadata and its
// derivatives -- the rows that describe media bytes living in the host's
// ObjectStore under keys this module builds and never exposes.
//
// ObjectService (m.svc) drives uploads through their lifecycle states:
// Create declares an upload, Upload streams its bytes into the host's
// ObjectStore, Complete runs the revalidation pipeline over them and
// finalizes the row, and Get, OpenContent and List serve completed
// objects. The HTTP round that mounts the OpenAPI fragment in front of
// the service, and the processing round that claims the thumbnail-derive
// task the service enqueues, both land later.
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
	// thumbnail-derive task the completion pipeline enqueues, deletion's
	// task in a later round -- runs on (WithQueue). Register refuses to
	// proceed without one.
	queue jobs.Queue

	// svc is the module's ObjectService, the runtime that drives the
	// lifecycle. NewModule builds it from the policy fields below, after
	// the With* options have been applied, and Register hands it the
	// registry its host seams come from (attach). Hosts drive uploads
	// through it via ObjectService().
	svc *ObjectService

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
// finalized image object, and deletion's task once the delete round lands.
// Upload finalization itself is synchronous -- Complete runs the
// revalidation pipeline in the caller's request before it returns -- so
// the queue carries only the work that may follow a finalize, and the
// pipeline warns when an enqueue fails rather than failing a finalize that
// stands on its own.
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

// OpenAPISpec implements pkgcore.Module: nil, this round.
//
// The module's HTTP surface -- its OpenAPI fragment under api/, the
// generated handler behind it, and the apiPath prefix its routes mount at
// -- is owned by the HTTP round, which ships the spec first and then the
// implementation behind the interface that spec generates. Until that
// fragment exists, returning nil is the honest answer, and it is what
// every module that does not ship an API (the modules hosting nothing but
// platform plumbing) returns. Register mirrors the absence by mounting no
// routes.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's contract it only
// declares and wires -- no database call, no outbound call, nothing that
// touches m.db, nothing that enqueues on m.queue.
//
// It contributes storage's permissions, its audit vocabulary and its event
// catalog, and validates the one wiring the module cannot live without:
// a queue, without which Register returns ErrQueueRequired (see
// WithQueue's doc comment for the reasoning).
//
// What Register deliberately does NOT do yet: mount the module's routes or
// declare the apiPath prefix (the HTTP surface is the HTTP round's, spec
// first), and register the thumbnail-derive task's handler (the service
// enqueues the type this round, but only the processing round implements
// the worker that claims it). The registration stays honest about the
// surface it actually ships.
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
	// Hand the registry to the service: its ObjectStore and EventBus are
	// the seams Complete, Upload and OpenContent read at call time. A plain
	// assignment -- no I/O -- so Register's no-I/O contract stands.
	m.svc.attach(reg)
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
