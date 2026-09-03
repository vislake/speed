package storage

import (
	"time"

	"github.com/vislake/speed/go/dbkit"
)

// tableObjects and tableObjectDerivatives name the module's tables, shared
// by the models' TableName methods and by the migrations' own header
// comments.
const (
	tableObjects           = "objects"
	tableObjectDerivatives = "object_derivatives"
	// objectIDLen is the length of an application-generated UUID string
	// (uuid.NewString), the fixed width every id column in this module's
	// tables declares.
	objectIDLen = 36
)

// The lifecycle states of an Object, stored in its state column and
// advanced only by ObjectService (B2/B3): an upload transaction opens a row
// in ObjectStateUploading, Complete's revalidation pipeline moves it to
// ObjectStateCompleted, and a delete protocol marks it ObjectStateDeleting
// before its bytes are removed from the object store.
const (
	// ObjectStateUploading means the object's metadata row exists but its
	// content has not been validated and finalized yet. Rows in this state
	// are invisible to readers and are swept when their upload window
	// (upload_expires_at) passes.
	ObjectStateUploading = "uploading"
	// ObjectStateCompleted means the object's bytes passed the full
	// revalidation pipeline and the row carries the finalized size, mime,
	// digest and dimensions. This is the only state readers see.
	ObjectStateCompleted = "completed"
	// ObjectStateDeleting means a delete protocol is in flight: the row is
	// marked before its bytes are removed from the object store, so a crash
	// between the two steps leaves a resumable, not a lost, deletion.
	ObjectStateDeleting = "deleting"
)

// DerivativeKindThumbnail is the kind of a downscaled image rendition of an
// original object. It is the only derivative kind this round ships; the
// kind column is open-ended so later rounds can add more without a schema
// change.
const DerivativeKindThumbnail = "thumbnail"

// Object is the metadata half of one stored object: the object's bytes live
// in the host's ObjectStore under an internal key, and Object is the row
// that knows about them -- its declared intent (size, type, checksum) as
// the uploader stated it, its finalized reality (size, mime, digest,
// dimensions) once the revalidation pipeline has confirmed them, and its
// lifecycle state.
//
// # Data domain
//
// Tenant data (docs/internal/04-data-and-tenancy.md). An object is
// meaningless outside its tenant and must never be visible across one, so
// it implements dbkit.TenantScoped (through the embedded TenantModel), is
// reached only through ObjectRepository (which embeds
// dbkit.Repository[Object]), and its isolation is proven by
// tenancytest.AssertIsolated.
//
// # Primary key
//
// The primary key is (id) alone, and id is an application-generated UUID:
// globally unique on its own, so tenant_id needs no part in the key. It
// rides along as a plain, non-key column with its own index, promoted by the
// embedded TenantModel -- the pattern dbkit's AGENTS.md documents for a
// tenant-scoped model that does not need tenant_id inside its primary key.
// The id-alone key also forbids cross-tenant id reuse: the same id can never
// name two rows, even in different tenants. Do NOT redeclare a same-named
// TenantID field here to add a primaryKey tag -- dbkit's tenant_scope.go doc
// comment documents exactly how that silently breaks GetTenantID and, with
// it, FindByID for the row's own legitimate owner.
//
// # The key is internal
//
// Key is the object-store key the bytes were (or are being) written to,
// built by the deterministic grammar in key.go. It is an implementation
// detail of where bytes physically live and is deliberately never exposed
// through any API surface: consumers address objects by id, and the store
// key could change without a schema change. See key.go's own doc comment.
//
// # Declared versus finalized
//
// The Declared* columns are the uploader's stated intent, taken at create
// time before any bytes exist; the bare Size, MIME, ChecksumSHA256, Width
// and Height columns are the finalized reality, written exactly once by
// Complete's revalidation pipeline and NULL until then. A reader must never
// trust a Declared* value as describing the stored bytes -- the two halves
// disagree on purpose until the pipeline has reconciled them.
type Object struct {
	// ID is an application-generated UUID (uuid.NewString), never a
	// database-generated one: the backend coding standard forbids
	// gen_random_uuid(), which SQLite has no equivalent for. Its global
	// uniqueness is what lets the primary key be (id) alone -- see the
	// type doc comment.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and the GetTenantID method
	// that satisfies dbkit.TenantScoped.
	dbkit.TenantModel

	// Key is the internal object-store key of the object's bytes,
	// "<tenantID>/<objectID>/original" per key.go. Never exposed through
	// the API.
	Key string `gorm:"column:key;size:512;not null"`

	// State is one of the ObjectState* constants above.
	State string `gorm:"column:state;size:16;not null"`

	// DeclaredSize is the Content-Length the uploader stated at create
	// time, in bytes. The column is BIGINT on both dialects so a declared
	// size of many gigabytes needs no narrowing anywhere.
	DeclaredSize int64 `gorm:"column:declared_size;not null"`

	// DeclaredType is the media type the uploader stated at create time,
	// free-form (a browser-derived value, say). It is a claim, never a
	// verdict: the finalized MIME column is the pipeline's own probe
	// result.
	DeclaredType string `gorm:"column:declared_type;size:255;not null;default:''"`

	// DeclaredChecksum is the client-supplied SHA-256 of the bytes the
	// uploader intends to send, lowercase hex. "" means the uploader did
	// not supply one, in which case no checksum comparison is performed.
	// The column is NOT NULL (no default): every row carries a value, and
	// "" IS the "no checksum declared" value.
	DeclaredChecksum string `gorm:"column:declared_checksum;size:64;not null"`

	// Size is the finalized byte count, written by Complete after the
	// bytes are in. NULL until the object is completed.
	Size *int64 `gorm:"column:size"`

	// MIME is the finalized media type, probed from the stored bytes by
	// the revalidation pipeline. NULL until the object is completed.
	MIME *string `gorm:"column:mime;size:255"`

	// ChecksumSHA256 is the SHA-256 digest of the finalized stored bytes,
	// lowercase hex. NULL until the object is completed.
	ChecksumSHA256 *string `gorm:"column:checksum_sha256;size:64"`

	// Width and Height are the finalized pixel dimensions, present only
	// when the completed object is a raster image the pipeline could
	// decode. NULL until the object is completed.
	Width  *int `gorm:"column:width"`
	Height *int `gorm:"column:height"`

	// UploadExpiresAt is the deadline by which the upload must be
	// completed; a row still in ObjectStateUploading past it is swept.
	UploadExpiresAt time.Time `gorm:"column:upload_expires_at;not null"`

	// ExpiresAt is the tenant-retention deadline of the completed object,
	// when the tenant asked for one. NULL means the object does not expire.
	ExpiresAt *time.Time `gorm:"column:expires_at"`

	// CreatedAt and UpdatedAt are the standard lifecycle timestamps,
	// maintained by gorm's autoCreateTime/autoUpdateTime and declared
	// explicitly so both dialects' migrations can name them.
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime;not null"`
}

// TableName returns the objects table name.
func (Object) TableName() string { return tableObjects }

// ObjectDerivative is one rendition of a completed Object -- in this round,
// exactly the thumbnail the derive pipeline produces -- stored as its own
// object-store object under a derivative key, with the row above the bytes.
//
// # Data domain
//
// Tenant data, like Object. The object_id column is an id reference, never
// a foreign key: cross-table FKs make independently released migrations and
// cascading deletes unmanageable (root CLAUDE.md's own rule), so the
// referential integrity between an Object and its derivatives is maintained
// by LifecycleService, which deletes the derivatives of an object it is
// deleting.
//
// # Primary key
//
// The primary key is (id) alone, an application-generated UUID with the
// same rationale as Object's own id (see that type's doc comment): globally
// unique, so tenant_id stays a plain indexed column. The uniqueness that
// actually governs derivatives is the UNIQUE index of the next section, not
// the key.
//
// # Uniqueness
//
// A tenant can hold at most one derivative of a given kind per object: the
// UNIQUE(tenant_id, object_id, kind) index the migrations declare backs the
// derive pipeline's idempotent skip -- re-deriving an existing thumbnail is
// a no-op, never a duplicate row.
type ObjectDerivative struct {
	// ID is an application-generated UUID (uuid.NewString), never a
	// database-generated one: the backend coding standard forbids
	// gen_random_uuid(), which SQLite has no equivalent for. Its global
	// uniqueness is what lets the primary key be (id) alone -- see the
	// type doc comment.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and the GetTenantID method
	// that satisfies dbkit.TenantScoped.
	dbkit.TenantModel

	// ObjectID is the id of the Object this derivative was derived from.
	// Plain id reference, no foreign key -- see the type doc comment.
	ObjectID string `gorm:"column:object_id;size:36;not null"`

	// Kind is the derivative kind, DerivativeKindThumbnail today, an
	// open-ended vocabulary for later rounds.
	Kind string `gorm:"column:kind;size:32;not null"`

	// Key is the internal object-store key of the derivative's bytes,
	// "<tenantID>/<objectID>/derivatives/<kind>" per key.go. Never exposed
	// through the API.
	Key string `gorm:"column:key;size:512;not null"`

	// MIME is the media type of the derivative's encoded bytes.
	MIME string `gorm:"column:mime;size:255;not null;default:''"`

	// Size is the byte count of the derivative's encoded bytes.
	Size int64 `gorm:"column:size;not null"`

	// Width and Height are the pixel dimensions of the derivative. NULL
	// for a future non-image derivative kind.
	Width  *int `gorm:"column:width"`
	Height *int `gorm:"column:height"`

	// CreatedAt is the derivative's creation timestamp.
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime;not null"`
}

// TableName returns the object_derivatives table name.
func (ObjectDerivative) TableName() string { return tableObjectDerivatives }
