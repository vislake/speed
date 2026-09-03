package notification

import (
	"encoding/json"
	"time"

	"gorm.io/datatypes"

	"github.com/vislake/speed/go/dbkit"
)

// tableNotificationPreferences is the notification_preferences table name,
// shared by the model's TableName and by the migrations' own header comments.
const tableNotificationPreferences = "notification_preferences"

// NotificationPreference is one recipient's stored channel selection for one
// notification type, inside one tenant.
//
// # Data domain
//
// Tenant data (docs/internal/04-data-and-tenancy.md). A preference is
// meaningful only inside the tenant it was set in -- the same person may want
// billing mail at home and in-app alerts at work -- so NotificationPreference
// implements dbkit.TenantScoped, is reached only through PreferenceRepository
// (which embeds dbkit.Repository[NotificationPreference]), and its isolation
// is proven by tenancytest.AssertIsolated.
//
// It embeds dbkit.TenantModel for the tenant_id column and the promoted
// GetTenantID method, exactly as InboxMessage does and for the same reasons
// (see model.go's doc comment): ID is an application-generated UUID, already
// globally unique on its own, so a plain tenant_id column backed by the
// composite listing index is enough. Do NOT redeclare a same-named TenantID
// field here -- dbkit's tenant_scope.go documents how that silently breaks
// GetTenantID.
//
// # Uniqueness
//
// At most one row exists per (tenant, recipient_user_id, type_key): the
// migration's single unique index uq_notification_preferences_tenant_recipient_type
// is the whole story of what a "preference" is -- the row is the recipient's
// answer for one notification type, and a second answer to the same question
// is an update, not a second row. The unique index's leftmost column also
// serves the per-tenant, per-recipient listing query.
//
// # Absence semantics
//
// There is deliberately no row for "the recipient has not chosen". Absence
// means the type's declared DefaultChannels apply (see types.go), and the
// defaults are never materialized into rows -- a type whose defaults change
// in a later release then changes behavior for every recipient who never
// chose, which is exactly the intent. A row only exists once the recipient
// has actively chosen something, and its channels column then holds the
// choice: a non-empty JSON array of channel names in the platform's canonical
// vocabulary order (types.go's sortedChannels), or the empty array "[]" for
// the one case an empty choice is legal, the deliberate opt-out on a type
// whose declaration permits it (Unsubscribable, see types.go). An empty
// selection on any other type never reaches storage: PreferenceService.Set
// refuses it first. "No row" and "stored empty array" are therefore distinct
// values with distinct meanings -- default, and opted out -- and the
// channels column is NOT NULL so the two can never blur through a NULL.
//
// # Recipient model
//
// RecipientUserID names a user of the tenant, by the same opaque-id rule
// every cross-module reference in this codebase follows (see model.go's
// RecipientUserID doc comment): no foreign key, no authn import, one row set
// per (tenant, person).
type NotificationPreference struct {
	// ID is an application-generated UUID, never a database-generated one:
	// the backend coding standard forbids gen_random_uuid(), which SQLite
	// has no equivalent for.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and the GetTenantID method
	// that satisfies dbkit.TenantScoped -- see the doc comment above for
	// why NotificationPreference embeds it instead of declaring TenantID
	// directly.
	dbkit.TenantModel

	// RecipientUserID is the person the preference belongs to, an opaque id
	// with no foreign key (see the doc comment above).
	RecipientUserID string `gorm:"column:recipient_user_id;size:64;not null"`

	// TypeKey names the notification type this preference answers,
	// following the <module>.<entity>.<action> convention types.go pins.
	// The type's own declaration (channels it supports, whether opting out
	// is legal) is read from the live registrar at call time, never
	// denormalized here.
	TypeKey string `gorm:"column:type_key;size:128;not null"`

	// Channels is the JSON of the recipient's channel selection, stored in
	// the platform's canonical vocabulary order (types.go's sortedChannels)
	// so the column is deterministic regardless of how the caller listed
	// the channels. Plain TEXT in the database on both dialects (see the
	// migration), never JSONB: nothing ever filters on a channel, and the
	// codebase rule forbids PostgreSQL-only types. See the doc comment
	// above for the exact meaning of "[]" and for why the column is NOT
	// NULL. The value is produced and consumed through channelsJSON and
	// parseChannels below; no other code touches the bytes.
	Channels datatypes.JSON `gorm:"column:channels;not null"`

	// CreatedAt and UpdatedAt are populated by gorm's autoCreateTime /
	// autoUpdateTime, never written by application code and never NOW() in
	// a migration (SQLite has no NOW()).
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the notification_preferences table.
func (NotificationPreference) TableName() string { return tableNotificationPreferences }

// channelsJSON marshals a channel selection into the form the channels column
// stores. The caller has already validated the selection (PreferenceService
// does) and ordered it canonically; marshalling a []string cannot fail, which
// is why the function has no error return. An empty selection marshals to
// "[]", never to NULL -- the empty array is the stored form of a legal
// opt-out, and the column is NOT NULL.
func channelsJSON(channels []string) datatypes.JSON {
	// json.Marshal of a nil slice is the JSON null -- and a NULL in the
	// channels column would blur "opted out" into "no row", which the schema
	// already forbids at the NOT NULL level. The stored form of an empty
	// selection is the JSON empty array, never null, so the round trip reads
	// back as an empty (non-nil) selection.
	if channels == nil {
		channels = []string{}
	}
	raw, _ := json.Marshal(channels)
	return raw
}

// parseChannels decodes a stored channels column back into the recipient's
// selection. A stored value that is not a JSON array of strings is a corrupt
// row -- the column is written only by channelsJSON -- and the caller
// (PreferenceService.ResolveChannels) wraps the error as internal.
func parseChannels(stored datatypes.JSON) ([]string, error) {
	var channels []string
	if err := json.Unmarshal(stored, &channels); err != nil {
		return nil, err
	}
	return channels, nil
}

// compile-time check that NotificationPreference satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = NotificationPreference{}
