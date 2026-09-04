package integration

import (
	"encoding/json"
	"time"

	"gorm.io/datatypes"

	"github.com/vislake/speed/go/dbkit"
)

// tableWebhookSubscriptions and tableWebhookDeliveries are round 2's two new
// table names, following round 1's tableAPIKeys convention.
const (
	tableWebhookSubscriptions = "integration_webhook_subscriptions"
	tableWebhookDeliveries    = "integration_webhook_deliveries"
)

// WebhookSecretSerializerName is the GORM serializer name
// WebhookSubscription.Secret is encrypted under.
//
// Following go/org's EmailSerializerName and go/notification's
// ContactAddressSerializerName precedent exactly: a host must register it
// once during bootstrap, before opening the *gorm.DB this module's tables
// live in --
//
//	dbkit.RegisterEncryptedSerializer(integration.WebhookSecretSerializerName, cipher)
//
// -- with a cipher key that is a different secret from any blind-index key
// this module might grow later (dbkit.NewCipher and dbkit.NewBlindIndexer's
// own doc comments both explain why an encryption key and an HMAC index key
// must never be the same bytes). Unlike an encrypted address, the secret
// this column stores is never queried by value -- there is no lookup "find
// the subscription with this secret" -- so no blind index accompanies it.
//
// Round 1's APIKey.Hash deliberately does NOT use this mechanism (see that
// field's own doc comment): a raw API key is full-entropy randomness, so its
// SHA-256 needs no reversible encryption. A webhook secret is different in
// kind -- it must be read back in plaintext on every delivery attempt to
// compute that attempt's HMAC signature (see webhook_signature.go), so it is
// encrypted at rest rather than hashed, exactly like org's invitation email
// and notification's contact address.
const WebhookSecretSerializerName = "integration_webhook_secret_enc"

// WebhookSubscription is a tenant's standing configuration for one outbound
// webhook: which public event types it wants delivered, and to which URL,
// per docs/internal/07-platform-services.md's outbound-webhook section.
//
// # Data domain
//
// Tenant data (docs/internal/04-data-and-tenancy.md), isolation proven by
// tenancytest.AssertIsolated (webhook_repository_test.go) -- a subscription
// belongs to exactly one tenant and must never be visible from, or
// deliverable to, another, following round 1's APIKey precedent: the
// primary key is (id) alone (an application-generated UUID, globally
// unique on its own), tenant_id riding along as a plain column promoted by
// the embedded TenantModel.
//
// # EventTypes names PUBLIC event types, never internal ones
//
// EventTypes is the set of PUBLIC event type strings
// (docs/internal/07-platform-services.md's "event.type", for example
// "org.member.joined") the tenant wants delivered -- never an internal
// pkgcore.Event.Type. The two vocabularies are related but distinct: a
// business module's Register call may map several internal event types onto
// one public type-and-version (or vice versa), and a subscription is
// written entirely in terms of the public contract a receiving endpoint was
// actually built against. See eventmapping.go's EventMapping for the
// mapping itself.
//
// Stored as a JSON array (scopesJSON/parseScopes's identical convention,
// renamed eventTypesJSON/parseEventTypes below): no native arrays
// (PostgreSQL-only), plain TEXT on both dialects, NOT NULL, empty selection
// is "[]" never NULL.
type WebhookSubscription struct {
	// ID is an application-generated UUID (uuid.NewString, in
	// Service.CreateWebhookSubscription), mirroring APIKey.ID's own doc
	// comment.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes tenant_id and dbkit.TenantScoped.
	dbkit.TenantModel

	// URL is the receiving endpoint every matching event is POSTed to.
	// Validated at creation time (and, through the delivery transport's own
	// dial-time recheck, again at every delivery attempt) by
	// ValidateWebhookURL -- see ssrf.go's file comment for why both checks
	// exist.
	URL string `gorm:"column:url;size:2048;not null"`

	// EventTypes is the JSON array of public event types this subscription
	// wants delivered. See the type's own doc comment ("EventTypes names
	// PUBLIC event types").
	EventTypes datatypes.JSON `gorm:"column:event_types;not null"`

	// Secret is the per-subscription HMAC key every delivered payload is
	// signed with (webhook_signature.go), encrypted at rest under
	// WebhookSecretSerializerName. Shown to the caller in plaintext exactly
	// once, at Service.CreateWebhookSubscription's return -- see that
	// method's own doc comment -- mirroring API key material's "shown once"
	// rule (model.go's own file comment), even though (unlike an API key
	// hash) this column can, by construction, be decrypted again: nothing
	// in this module ever serves it back out after creation.
	Secret string `gorm:"column:secret;size:512;serializer:integration_webhook_secret_enc;not null"`

	// Active gates delivery: an event matching a subscription whose Active
	// is false is never fanned out to it (webhook_delivery.go's
	// matchingSubscriptions). Service.UpdateWebhookSubscription is the only
	// way to flip it; there is no separate "pause" operation this round.
	Active bool `gorm:"column:active;not null"`

	// CreatedBy is the authn user id of whoever configured this
	// subscription -- an id reference only, per the root CLAUDE.md's
	// module-boundary rule, following APIKey.CreatedBy's identical
	// reasoning.
	CreatedBy string `gorm:"column:created_by;size:64;not null"`

	// CreatedAt and UpdatedAt are written by gorm's autoCreateTime /
	// autoUpdateTime, never by application code or a database default.
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the integration_webhook_subscriptions table.
func (WebhookSubscription) TableName() string { return tableWebhookSubscriptions }

// compile-time check that WebhookSubscription satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = WebhookSubscription{}

// eventTypesJSON marshals an event-type selection into the form the
// event_types column stores, the identical contract scopesJSON documents
// (model.go): a nil slice becomes the stored empty array "[]", never JSON
// null, since the column is NOT NULL.
func eventTypesJSON(types []string) datatypes.JSON {
	if types == nil {
		types = []string{}
	}
	raw, _ := json.Marshal(types)
	return raw
}

// parseEventTypes decodes a stored event_types column back into a selection.
// A stored value that is not a JSON array of strings is a corrupt row -- the
// column is written only by eventTypesJSON -- and callers wrap the error as
// ErrInternal.
func parseEventTypes(stored datatypes.JSON) ([]string, error) {
	var types []string
	if err := json.Unmarshal(stored, &types); err != nil {
		return nil, err
	}
	return types, nil
}

// The WebhookDelivery.Status vocabulary -- a closed set kept in Go rather
// than a database enum, per the backend coding standard's dual-dialect rule
// (PostgreSQL has enum types, SQLite does not).
//
//	pending ---(attempt fails, retries remain)---> failed ---(retry)---> pending/failed
//	pending ---(attempt succeeds)----------------> delivered   [terminal]
//	failed  ---(retries exhausted)----------------> dead_letter [terminal]
//
// pending is a row's state before its first attempt, and its state again
// between a failed attempt and the retry that follows it -- Attempts is what
// actually distinguishes "never tried" from "tried once and about to retry"
// (failed is set for the interval the row is not currently pending-for-retry
// -- see webhook_delivery.go's Handle for exactly when each transition
// happens). Manual redelivery (docs/internal/07's own name for it) is
// explicitly this round's boundary; see AGENTS.md's "Deliberately not in
// scope" table --
// the state machine above is what a later round's redelivery feature will
// act on, nothing more.
const (
	DeliveryStatusPending    = "pending"
	DeliveryStatusFailed     = "failed"
	DeliveryStatusDelivered  = "delivered"
	DeliveryStatusDeadLetter = "dead_letter"
)

// WebhookDelivery is one attempted (or about-to-be-attempted) delivery of
// one public event to one subscription -- the delivery log
// docs/internal/07-platform-services.md requires: which subscription, which
// event, its outcome, and enough detail for a later round's manual
// redelivery to act on.
//
// # Data domain
//
// Tenant data, isolation proven by tenancytest.AssertIsolated -- a delivery
// belongs to the same tenant as the subscription it was fanned out from, and
// nothing about the underlying business event (whose OWN tenant is what
// selected the subscription in the first place, see
// webhook_delivery.go's handleDomainEvent) is visible across a tenant
// boundary through this table either.
//
// # One row per (subscription, event), not one row per HTTP attempt
//
// A delivery that fails and retries updates the SAME row (Attempts,
// LastStatusCode, LastError, Status) rather than appending a new one: the
// row's identity is the (subscription, event) pair, not the individual HTTP
// call, so "list recent deliveries for this subscription" (the query
// idxWebhookDeliveriesSubscriptionCreatedAt exists for) shows one line per
// logical delivery with its current outcome, not a flood of retry rows for
// what is, from the tenant's point of view, one thing that did or did not
// eventually arrive.
type WebhookDelivery struct {
	// ID is an application-generated UUID.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes tenant_id and dbkit.TenantScoped.
	dbkit.TenantModel

	// SubscriptionID names the WebhookSubscription this delivery was fanned
	// out from. Deliberately an id reference, never a GORM association or
	// foreign key -- the root CLAUDE.md's "no cross-module foreign keys"
	// rule applies to this module's own two tables exactly as it does
	// across module boundaries, since dbkit.Repository[T] offers no
	// cascading-delete machinery either way; a subscription's own delete
	// leaves its past delivery rows in place as history (see
	// Service.DeleteWebhookSubscription's doc comment).
	SubscriptionID string `gorm:"column:subscription_id;size:36;not null"`

	// EventType and EventVersion are the public event's own identity
	// (docs/internal/07's "event.type" + "event.version"), copied from the
	// EventMapping that produced this delivery -- never the internal
	// pkgcore.Event.Type, matching WebhookSubscription.EventTypes' own
	// public-only vocabulary.
	EventType    string `gorm:"column:event_type;size:128;not null"`
	EventVersion string `gorm:"column:event_version;size:16;not null"`

	// IdempotencyKey is the derived delivery key (deriveWebhookDeliveryKey
	// in webhook_delivery.go) that makes event-to-subscription fan-out
	// idempotent under the at-least-once delivery an EventBus subscriber
	// must tolerate: a redelivered domain event derives the identical key
	// for the identical subscription, and handleDomainEvent probes it
	// before creating a new row (see that function's doc comment) --
	// exactly the role notification.SendRecord.IdempotencyKey plays for its
	// own replay convergence, scoped by subscription here instead of by
	// recipient and channel.
	IdempotencyKey string `gorm:"column:idempotency_key;size:64;not null"`

	// Payload is the exact JSON body a delivery attempt sends (or will
	// send): the versioned public envelope {event: {type, version}, data:
	// ...} webhook_delivery.go's buildEnvelope produces, computed once when
	// the row is created and never recomputed on retry -- an internal
	// event's payload could, in principle, be re-read and re-mapped
	// differently across retries if a later release changed the mapping
	// mid-flight, and storing the payload up front is what makes every
	// attempt of one delivery byte-for-byte identical, which HMAC signing
	// requires in the first place (a signature covers exactly these bytes).
	// It is also what a later round's manual redelivery needs: the
	// original body, on hand, with no dependency on the source event still
	// being reconstructable.
	Payload datatypes.JSON `gorm:"column:payload;not null"`

	// Status is one of the DeliveryStatus* values (see that block's own
	// doc comment for the state machine).
	Status string `gorm:"column:status;size:16;not null"`

	// Attempts counts every HTTP attempt made so far, starting at 0 for a
	// freshly created row.
	Attempts int `gorm:"column:attempts;not null"`

	// LastStatusCode is the HTTP status the most recent attempt received,
	// nil when no attempt has reached the point of receiving one (a DNS
	// failure, a connection refusal, or no attempt yet).
	LastStatusCode *int `gorm:"column:last_status_code"`

	// LastError is the most recent attempt's failure text, truncated at
	// webhookDeliveryErrorBudget -- the empty-string sentinel while Status
	// is DeliveryStatusDelivered or before any attempt has failed. Treated
	// as untrusted diagnostic text, matching SendRecord.Error's own
	// convention: it may echo a receiving server's own response body
	// fragment.
	LastError string `gorm:"column:last_error;size:4000;not null"`

	// LastAttemptAt is when the most recent HTTP attempt was made, nil
	// before the first one.
	LastAttemptAt *time.Time `gorm:"column:last_attempt_at"`

	// DeliveredAt is when this delivery reached DeliveryStatusDelivered,
	// nil until then and permanent once set.
	DeliveredAt *time.Time `gorm:"column:delivered_at"`

	// CreatedAt and UpdatedAt are written by gorm's autoCreateTime /
	// autoUpdateTime.
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the integration_webhook_deliveries table.
func (WebhookDelivery) TableName() string { return tableWebhookDeliveries }

// compile-time check that WebhookDelivery satisfies dbkit.TenantScoped.
var _ dbkit.TenantScoped = WebhookDelivery{}

// webhookDeliveryErrorBudget mirrors notification's identical
// sendRecordErrorBudget: the column's own 4000-char width, enforced at the
// write site so no transport response body can overflow the schema.
const webhookDeliveryErrorBudget = 4000
