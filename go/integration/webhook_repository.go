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
// WebhookDelivery row, which is what makes fanning an at-least-once
// redelivered domain event out to the same subscription idempotent: see
// uq_integration_webhook_deliveries_tenant_subscription_key in the
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
