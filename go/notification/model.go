package notification

import (
	"time"

	"gorm.io/datatypes"

	"github.com/vislake/speed/go/dbkit"
)

// tableInAppMessages is the in_app_messages table name, shared by the
// model's TableName and by the migrations' own header comments.
const tableInAppMessages = "in_app_messages"

// InboxMessage is one delivered message in a tenant's in-app inbox: the
// row a recipient sees when they open the product's notification center.
//
// # Data domain
//
// Tenant data (docs/internal/04-data-and-tenancy.md). A message is
// meaningful only inside the tenant that produced it, so InboxMessage
// implements dbkit.TenantScoped, is reached only through Repository (which
// embeds dbkit.Repository[InboxMessage]), and its isolation is proven by
// tenancytest.AssertIsolated.
//
// It embeds dbkit.TenantModel for the tenant_id column and the promoted
// GetTenantID method, the pattern dbkit's AGENTS.md documents for a
// tenant-scoped model that does not need tenant_id inside its primary key:
// ID is an application-generated UUID, already globally unique on its own,
// so a plain tenant_id column backed by a composite listing index is
// enough. Do NOT redeclare a same-named TenantID field here to add a
// primaryKey tag -- dbkit's tenant_scope.go doc comment documents exactly
// how that silently breaks GetTenantID and, with it, FindByID for the
// row's own legitimate owner.
//
// # Recipient model
//
// RecipientUserID names a user of the tenant, by the same opaque-id rule
// every cross-module reference in this codebase follows: the column
// deliberately stores no foreign key (cross-module foreign keys are
// forbidden, docs/internal/04-data-and-tenancy.md rule 4), because authn's
// users table and notification's inbox are independently released
// migrations. The width mirrors the user_id column org's memberships table
// uses for the identical reference. A person who belongs to several
// tenants simply has one row set per tenant -- the tenant_id column, not
// the recipient, is what scopes a row.
//
// # Content model
//
// The row stores what the recipient sees, not the template that produced
// it: the delivery subscriber renders type_key's template in the
// recipient's locale at send time and stores the finished Title and Body,
// alongside Params -- the template parameters that produced them, kept as
// JSON so a later re-render (a locale change, say) needs no re-parse of
// the source event. Params is plain TEXT in the database on both dialects
// (see the migration), never JSONB: the JSON stays portable, and no query
// ever filters on a parameter.
//
// Group is the row's classification -- the value of the notification type's
// group (see the design doc's vocabulary), denormalized onto the row so the
// inbox's group filter needs no join. The column name "group" is taken
// verbatim from the design's table shape; GROUP is a reserved word on
// PostgreSQL, so the migration quotes the column name and any hand-written
// SQL in later blocks must quote it too ("group" is unambiguous on SQLite
// as well). GORM-generated SQL always quotes identifiers, so code that
// goes through the Repository never needs to think about it.
//
// Link is the optional deep link the message points at; empty means the
// message has no destination. ExpiryAt, when set, is the moment the
// message stops being listed as unread; ReadAt is when the recipient
// opened it -- nil means unread. Neither is written by the delivery
// subscriber; both belong to the read path.
//
// DedupeKey is the delivery idempotency key this row was produced under --
// the same derivation the delivery subscriber's queue job carries (type,
// tenant, payload content, recipient, channel), see the migration's doc
// comment on uq_in_app_messages_dedupe_key for the full semantics. It is
// NULL for the rare row no delivery produced. Application code never
// invents one.
type InboxMessage struct {
	// ID is an application-generated UUID, never a database-generated one:
	// the backend coding standard forbids gen_random_uuid(), which SQLite
	// has no equivalent for.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and the GetTenantID method
	// that satisfies dbkit.TenantScoped -- see the doc comment above for
	// why InboxMessage embeds it instead of declaring TenantID directly.
	dbkit.TenantModel

	// RecipientUserID is the inbox owner's user id (see the doc comment
	// above: an opaque id, no foreign key, no authn import).
	RecipientUserID string `gorm:"column:recipient_user_id;size:64;not null"`

	// TypeKey names the notification type this message was rendered from,
	// following the <module>.<entity>.<action> convention the design doc
	// pins for type keys (for example "notes.note.shared").
	TypeKey string `gorm:"column:type_key;size:128;not null"`

	// Group is the message's classification inside the inbox, taken from
	// the notification type at delivery time. See the doc comment above
	// for why the column name is quoted everywhere it appears in SQL.
	Group string `gorm:"column:group;size:64;not null;default:''"`

	// Title is the rendered headline of the message, in the recipient's
	// locale at delivery time.
	Title string `gorm:"column:title;size:255;not null"`

	// Body is the rendered body of the message, in the recipient's locale
	// at delivery time. The VARCHAR(4000) width matches the longest text
	// column in the codebase (notes' text); a body that would not fit is a
	// template bug, not a storage concern.
	Body string `gorm:"column:body;size:4000;not null"`

	// Params is the JSON of the template parameters that produced Title
	// and Body -- see the doc comment above for what it is for and why it
	// is never queried into.
	Params datatypes.JSON `gorm:"column:params"`

	// Link is the optional deep link the message points at; empty when the
	// message has none (see the doc comment above).
	Link string `gorm:"column:link;size:2000;not null;default:''"`

	// DedupeKey is the delivery idempotency key this row was produced
	// under (see the doc comment above).
	DedupeKey *string `gorm:"column:dedupe_key;size:128"`

	// ExpiryAt is when the message stops counting as unread; nil means it
	// never expires.
	ExpiryAt *time.Time `gorm:"column:expiry_at"`

	// ReadAt is when the recipient opened the message; nil means unread.
	ReadAt *time.Time `gorm:"column:read_at"`

	// CreatedAt and UpdatedAt are populated by gorm's autoCreateTime /
	// autoUpdateTime, never written by application code and never NOW() in
	// a migration (SQLite has no NOW()).
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the in_app_messages table.
func (InboxMessage) TableName() string { return tableInAppMessages }

// compile-time check that InboxMessage satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = InboxMessage{}
