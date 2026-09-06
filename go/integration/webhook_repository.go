package integration

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// WebhookSubscriptionRepository is this module's tenant-scoped data-access
// type for WebhookSubscription.
//
// It embeds *dbkit.Repository[WebhookSubscription] (Create / FindByID /
// Update / Delete / List promoted unchanged, mirroring
// APIKeyRepository's identical shape) and adds the one query round 1's
// minimal-surface reasoning does not already cover: "every ACTIVE
// subscription of one tenant whose EventTypes includes a given public
// type", the fan-out lookup handleDomainEvent (webhook_delivery.go) needs on
// every matching domain event.
//
// Following go/notification's PreferenceRepository precedent exactly (see
// that type's own doc comment in preference_repository.go): built on the
// SAME *gorm.DB the embedded Repository was built on, against a TenantScoped
// destination, inside dbkit.WithTenantSession so the GORM isolation plugin
// still injects WHERE tenant_id = ? and the PostgreSQL RLS session variable
// is still set for the call even though Repository[T]'s own promoted
// methods are not the ones running it. Nothing in this file hand-writes a
// tenant_id filter, and nothing reaches for db.Table, db.Model or db.Raw.
type WebhookSubscriptionRepository struct {
	*dbkit.Repository[WebhookSubscription]

	// db is the same connection the embedded Repository was built on, kept
	// only so ListActiveByTenant below can be composed on it.
	db *gorm.DB
}

// NewWebhookSubscriptionRepository returns a WebhookSubscriptionRepository
// backed by db.
func NewWebhookSubscriptionRepository(db *gorm.DB) *WebhookSubscriptionRepository {
	return &WebhookSubscriptionRepository{
		Repository: dbkit.NewRepository[WebhookSubscription](db),
		db:         db,
	}
}

// webhookSubscriptionChanges is the change set updateFields applies: each
// non-nil field names one column to write, mirroring the "nil means no
// change" convention UpdateWebhookSubscriptionInput (webhook_service.go)
// already establishes for the Service's own input -- the Service validates
// and assembles a changes value, the repository writes exactly its non-nil
// columns and nothing else. EventTypes is a []string rather than a *[]string
// because the nil slice is the natural "no change" value, the identical
// convention the Service input's EventTypes field follows; the empty slice
// is refused upstream (ErrEventTypesRequired), never written here.
type webhookSubscriptionChanges struct {
	// URL, when non-nil, replaces the stored URL.
	URL *string
	// EventTypes, when non-nil, replaces the stored selection.
	EventTypes []string
	// Active, when non-nil, replaces the stored delivery gate.
	Active *bool
}

// updateFields applies a partial column update -- exactly the columns the
// non-nil fields of changes name, and nothing else -- to the LIVE
// subscription identified by id, in the tenant of ctx, and reports whether
// any row matched. It is the write Service.UpdateWebhookSubscription
// performs after its read-modify validation (webhook_service.go), and two
// of its properties are load-bearing there:
//
//  1. It never carries deleted_at/deleted_by (or any other column outside
//     the caller's own change set) in its SET clause. A full-row
//     Repository[WebhookSubscription].Update of an already-read row would
//     race a concurrent DeleteWebhookSubscription exactly the way
//     recordLastUsed's full-row save raced Revoke (see repository.go's
//     touchLastUsed doc comment): the updating side read the row while it
//     was still live, and re-saving that stale copy would write its nil
//     DeletedAt back over the mark-delete the other call just committed --
//     resurrecting a subscription the tenant already deleted, Active value
//     included.
//  2. Its WHERE clause requires deleted_at IS NULL, mirroring the
//     live-rows-only view FindByID's own query scope already applies: a row
//     deleted between the Service's read and this write matches nothing, so
//     the update reports false and the Service answers
//     ErrWebhookSubscriptionNotFound -- the deletion wins, exactly as if it
//     had landed before the read.
//
// # How the statement is built, and why
//
// The write is expressed as tx.Where(...).Where("deleted_at IS NULL").
// Select(columns).Updates(&m) -- gorm resolves the target table from the
// struct passed to Updates, so nothing here reaches for db.Model / db.Table
// / db.Raw (the three bypass entry points
// tools/semgrep_rules/raw-gorm-bypass.yml flags), the identical
// construction dbkit's own softDelete/Restore, go/ai-gateway's
// markGenerated/markCompleted and go/org's deleteLeaf all use for a guarded
// column-scoped write. Select is load-bearing, not decorative: gorm's
// struct-based Updates silently omits any zero-valued field from the SET
// clause, and Active is a bool whose replacement value may legitimately be
// false (that is how a caller pauses a subscription) -- naming the changed
// columns in Select forces exactly those columns into the SET clause
// regardless of value. The one always-present extra column is UpdatedAt:
// gorm's auto-update-time machinery writes it, exactly as it did under the
// previous statement shape (the map-payload Updates also refreshed it on
// every write), and its presence is what makes RowsAffected a reliable
// "did a live row match" answer on SQLite -- a matched row always changes,
// so it is always counted.
//
// The tenant filter comes from dbkit's tenant-scope plugin (the statement
// runs inside WithTenantSession against the TenantScoped
// WebhookSubscription model, which gorm resolves from the Updates payload
// when no Model is set), so this can never touch another tenant's row.
func (r *WebhookSubscriptionRepository) updateFields(ctx context.Context, id string, changes webhookSubscriptionChanges) (bool, error) {
	m := WebhookSubscription{}
	columns := make([]string, 0, 4)
	if changes.URL != nil {
		m.URL = *changes.URL
		columns = append(columns, "URL")
	}
	if changes.EventTypes != nil {
		m.EventTypes = eventTypesJSON(changes.EventTypes)
		columns = append(columns, "EventTypes")
	}
	if changes.Active != nil {
		m.Active = *changes.Active
		columns = append(columns, "Active")
	}
	// Always part of the SET clause: gorm's auto-update-time machinery fills
	// UpdatedAt with the current time (see the doc comment above).
	columns = append(columns, "UpdatedAt")

	var matched bool
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ?", id).
			Where("deleted_at IS NULL").
			Select(columns).
			Updates(&m)
		matched = res.RowsAffected > 0
		return res.Error
	})
	return matched, err
}

// ListActiveByTenant returns every WebhookSubscription of the tenant in ctx
// whose Active is true, in no particular order -- handleDomainEvent filters
// the result by EventTypes membership itself (webhook_delivery.go's
// matchingSubscriptions), since event_types is a JSON column this module
// deliberately never filters on inside SQL (webhook_model.go's own doc
// comment explains why: no native arrays, no JSONB operator filtering, per
// the backend coding standard's dual-dialect rule). A tenant configures at
// most a handful of webhooks in practice, so reading them all and filtering
// in Go is the right trade for staying dialect-portable.
func (r *WebhookSubscriptionRepository) ListActiveByTenant(ctx context.Context) ([]WebhookSubscription, error) {
	var subs []WebhookSubscription
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("active = ?", true).Find(&subs).Error
	})
	return subs, err
}

// WebhookDeliveryRepository is this module's tenant-scoped data-access type
// for WebhookDelivery, mirroring WebhookSubscriptionRepository's shape:
// the embedded Repository[T] covers Create/FindByID/Update/List, and this
// file adds the two shapes it cannot express -- a lookup by the fan-out's
// own idempotency key, and the "recent deliveries for one subscription"
// listing docs/internal/07-platform-services.md's delivery-log requirement
// asks for.
type WebhookDeliveryRepository struct {
	*dbkit.Repository[WebhookDelivery]
	db *gorm.DB
}

// NewWebhookDeliveryRepository returns a WebhookDeliveryRepository backed by
// db.
func NewWebhookDeliveryRepository(db *gorm.DB) *WebhookDeliveryRepository {
	return &WebhookDeliveryRepository{
		Repository: dbkit.NewRepository[WebhookDelivery](db),
		db:         db,
	}
}

// ByIdempotencyKey returns the delivery already recorded for
// (subscriptionID, key) in the tenant of ctx, or (nil, nil) when none
// exists -- the identical nil-and-nil-is-a-value contract
// PreferenceRepository.ByUserAndType documents, since "no delivery yet
// under this key" is this method's ordinary answer, not a failure.
//
// handleDomainEvent (webhook_delivery.go) probes this before creating a new
// WebhookDelivery row, which is what makes fanning the same OBSERVED
// occurrence of a domain event out to the same subscription idempotent
// (the key carries the occurrence marker -- see deriveWebhookDeliveryKey):
// see uq_integration_webhook_deliveries_tenant_subscription_key in the
// migration.
func (r *WebhookDeliveryRepository) ByIdempotencyKey(ctx context.Context, subscriptionID, key string) (*WebhookDelivery, error) {
	var row WebhookDelivery
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("subscription_id = ? AND idempotency_key = ?", subscriptionID, key).First(&row).Error
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// ListRecentBySubscription returns up to limit deliveries of subscriptionID
// in the tenant of ctx, newest first -- the query
// idx_integration_webhook_deliveries_subscription_created_at exists to
// serve. A non-positive limit falls back to
// defaultRecentDeliveriesLimit, the same defensive-default shape
// go/authn's login-history listing uses, so a caller cannot accidentally
// request every delivery a long-lived subscription has ever attempted.
func (r *WebhookDeliveryRepository) ListRecentBySubscription(ctx context.Context, subscriptionID string, limit int) ([]WebhookDelivery, error) {
	if limit <= 0 {
		limit = defaultRecentDeliveriesLimit
	}
	var rows []WebhookDelivery
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("subscription_id = ?", subscriptionID).
			Order("created_at DESC").
			Limit(limit).
			Find(&rows).Error
	})
	return rows, err
}

// defaultRecentDeliveriesLimit bounds ListRecentBySubscription when the
// caller asks for no explicit limit.
const defaultRecentDeliveriesLimit = 50

// maxRecentDeliveriesLimit is the upper bound Service.
// ListRecentWebhookDeliveries clamps any larger requested limit to -- the
// fragment's own limit query parameter declares the identical ceiling (see
// api/openapi.yaml's integration_listWebhookDeliveries), and this constant
// is its enforcement: the generated parameter binding validates only that
// limit parses as an integer, never that it is within range, so an
// unbounded caller request would otherwise become an unbounded query.
const maxRecentDeliveriesLimit = 100
