package integration

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// This file holds Service's webhook-subscription-management surface --
// round 2's counterpart of service.go's API key Create/List/Rotate/Revoke.
// The actual event-driven delivery pipeline (subscribing to the bus,
// mapping events, enqueueing and running delivery jobs) lives in
// webhook_delivery.go; this file is configuration only.

// CreateWebhookSubscriptionInput is what a caller passes to
// Service.CreateWebhookSubscription.
type CreateWebhookSubscriptionInput struct {
	// URL is the receiving endpoint. Mandatory (ErrWebhookURLRequired when
	// empty) and validated by ValidateWebhookURL (ErrWebhookURLInvalid /
	// ErrWebhookURLUnresolvable / ErrWebhookURLBlocked) before anything is
	// persisted.
	URL string

	// EventTypes is the non-empty set of public event types this
	// subscription wants delivered (ErrEventTypesRequired when empty). Every
	// entry must be some registered EventMapping's PublicType
	// (ErrWebhookEventTypeUnknown naming the first one that is not).
	EventTypes []string

	// CreatedBy is the authn user id configuring this subscription.
	// Mandatory (ErrCreatedByRequired when empty), mirroring
	// CreateInput.CreatedBy.
	CreatedBy string
}

// CreatedWebhookSubscription is Service.CreateWebhookSubscription's result:
// the one and only place the raw signing secret is ever available, mirroring
// CreatedAPIKey's identical "shown once" contract (see WebhookSubscription's
// own doc comment in webhook_model.go).
type CreatedWebhookSubscription struct {
	ID         string
	URL        string
	EventTypes []string
	// Secret is the raw HMAC signing secret the caller must record now.
	// Nothing this module returns after this call ever reproduces it (List
	// and Update never echo it back), even though -- unlike an API key hash
	// -- the stored column CAN technically be decrypted again: no code path
	// in this module does.
	Secret    string
	Active    bool
	CreatedBy string
	CreatedAt time.Time
}

// WebhookSubscriptionSummary is what Service.ListWebhookSubscriptions and
// Service.UpdateWebhookSubscription expose for one subscription: every
// field of WebhookSubscription except Secret.
type WebhookSubscriptionSummary struct {
	ID         string
	URL        string
	EventTypes []string
	Active     bool
	CreatedBy  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// CreateWebhookSubscription validates in, generates a fresh signing secret,
// and persists a new, active WebhookSubscription.
func (s *Service) CreateWebhookSubscription(ctx context.Context, in CreateWebhookSubscriptionInput) (*CreatedWebhookSubscription, error) {
	if in.CreatedBy == "" {
		return nil, ErrCreatedByRequired
	}
	if in.URL == "" {
		return nil, ErrWebhookURLRequired
	}
	if len(in.EventTypes) == 0 {
		return nil, ErrEventTypesRequired
	}
	if err := s.validateEventTypes(in.EventTypes); err != nil {
		return nil, err
	}
	if err := s.validateWebhookURL(ctx, in.URL); err != nil {
		return nil, err
	}

	secret, err := newWebhookSecret()
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}

	row := &WebhookSubscription{
		ID:         uuid.NewString(),
		URL:        in.URL,
		EventTypes: eventTypesJSON(in.EventTypes),
		Secret:     secret,
		Active:     true,
		CreatedBy:  in.CreatedBy,
	}
	if err := s.webhookRepo.Create(ctx, row); err != nil {
		return nil, ErrInternal.WithCause(err)
	}

	if err := s.emitWebhookAudit(ctx, AuditActionWebhookSubscriptionCreate, row); err != nil {
		return nil, err
	}

	return &CreatedWebhookSubscription{
		ID:         row.ID,
		URL:        row.URL,
		EventTypes: in.EventTypes,
		Secret:     secret,
		Active:     true,
		CreatedBy:  in.CreatedBy,
		CreatedAt:  row.CreatedAt,
	}, nil
}

// ListWebhookSubscriptions returns every webhook subscription of the
// caller's tenant, secret-free, in no particular order -- mirroring
// Service.List's identical "no ordering guarantee" contract.
func (s *Service) ListWebhookSubscriptions(ctx context.Context) ([]WebhookSubscriptionSummary, error) {
	rows, err := s.webhookRepo.List(ctx)
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	out := make([]WebhookSubscriptionSummary, 0, len(rows))
	for _, row := range rows {
		summary, err := s.toSummary(row)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	return out, nil
}

// UpdateWebhookSubscriptionInput is what a caller passes to
// Service.UpdateWebhookSubscription. Every pointer/nil-slice field left
// unset leaves the corresponding column unchanged -- the same "nil means no
// change" convention config's own item-write API uses.
type UpdateWebhookSubscriptionInput struct {
	// ID is the subscription to update. Mandatory.
	ID string
	// URL, when non-nil, replaces the stored URL after the identical
	// validation CreateWebhookSubscription applies.
	URL *string
	// EventTypes, when non-nil, replaces the stored selection after the
	// identical non-empty/known-type validation CreateWebhookSubscription
	// applies. A non-nil EMPTY slice is refused with
	// ErrEventTypesRequired, exactly like Create -- there is no supported
	// way to leave a subscription with zero event types short of Delete.
	EventTypes []string
	// Active, when non-nil, flips the subscription's delivery gate (see
	// WebhookSubscription.Active's own doc comment). This is the only
	// supported way to pause a subscription without deleting it.
	Active *bool
}

// UpdateWebhookSubscription applies a partial update to one subscription of
// the caller's tenant.
func (s *Service) UpdateWebhookSubscription(ctx context.Context, in UpdateWebhookSubscriptionInput) (*WebhookSubscriptionSummary, error) {
	row, err := s.webhookRepo.FindByID(ctx, in.ID)
	if err != nil {
		return nil, translateWebhookRepoErr(err)
	}

	if in.URL != nil {
		if *in.URL == "" {
			return nil, ErrWebhookURLRequired
		}
		if urlErr := s.validateWebhookURL(ctx, *in.URL); urlErr != nil {
			return nil, urlErr
		}
		row.URL = *in.URL
	}
	if in.EventTypes != nil {
		if len(in.EventTypes) == 0 {
			return nil, ErrEventTypesRequired
		}
		if typesErr := s.validateEventTypes(in.EventTypes); typesErr != nil {
			return nil, typesErr
		}
		row.EventTypes = eventTypesJSON(in.EventTypes)
	}
	if in.Active != nil {
		row.Active = *in.Active
	}

	if updateErr := s.webhookRepo.Update(ctx, row); updateErr != nil {
		return nil, ErrInternal.WithCause(updateErr)
	}
	if auditErr := s.emitWebhookAudit(ctx, AuditActionWebhookSubscriptionUpdate, row); auditErr != nil {
		return nil, auditErr
	}

	summary, err := s.toSummary(*row)
	if err != nil {
		return nil, err
	}
	return &summary, nil
}

// DeleteWebhookSubscription permanently removes one subscription of the
// caller's tenant. Its past WebhookDelivery rows are left in place as
// history -- see that field's own doc comment in webhook_model.go for why
// this module never cascades the delete (no cross-module-style foreign
// keys, even within this module's own two tables).
func (s *Service) DeleteWebhookSubscription(ctx context.Context, id string) error {
	row, err := s.webhookRepo.FindByID(ctx, id)
	if err != nil {
		return translateWebhookRepoErr(err)
	}
	if err := s.webhookRepo.Delete(ctx, id); err != nil {
		return ErrInternal.WithCause(err)
	}
	return s.emitWebhookAudit(ctx, AuditActionWebhookSubscriptionDelete, row)
}

// WebhookDeliverySummary is what Service.ListRecentWebhookDeliveries exposes
// for one delivery -- every field of WebhookDelivery except the raw Payload
// bytes, which are an implementation detail of the send pipeline rather
// than something a subscription-management caller needs to see; a later
// round's manual-redelivery feature reads Payload directly off the
// repository row instead (see AGENTS.md's "Deliberately not in scope"
// table).
type WebhookDeliverySummary struct {
	ID             string
	SubscriptionID string
	EventType      string
	EventVersion   string
	Status         string
	Attempts       int
	LastStatusCode *int
	LastError      string
	LastAttemptAt  *time.Time
	DeliveredAt    *time.Time
	CreatedAt      time.Time
}

// ListRecentWebhookDeliveries returns up to limit of subscriptionID's most
// recent deliveries, newest first -- the delivery log read path
// docs/internal/07-platform-services.md asks for. A non-positive limit
// falls back to defaultRecentDeliveriesLimit (see
// WebhookDeliveryRepository.ListRecentBySubscription).
//
// subscriptionID is not itself verified to exist (unlike Update/Delete):
// an id belonging to another tenant, or no subscription at all, simply
// returns an empty slice, exactly as a genuine subscription with zero
// deliveries would -- there is no observable difference between the two,
// which is deliberate for the identical cross-tenant-enumeration reason
// ErrKeyNotFound's own doc comment gives.
func (s *Service) ListRecentWebhookDeliveries(ctx context.Context, subscriptionID string, limit int) ([]WebhookDeliverySummary, error) {
	rows, err := s.deliveryRepo.ListRecentBySubscription(ctx, subscriptionID, limit)
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	out := make([]WebhookDeliverySummary, 0, len(rows))
	for _, row := range rows {
		out = append(out, WebhookDeliverySummary{
			ID:             row.ID,
			SubscriptionID: row.SubscriptionID,
			EventType:      row.EventType,
			EventVersion:   row.EventVersion,
			Status:         row.Status,
			Attempts:       row.Attempts,
			LastStatusCode: row.LastStatusCode,
			LastError:      row.LastError,
			LastAttemptAt:  row.LastAttemptAt,
			DeliveredAt:    row.DeliveredAt,
			CreatedAt:      row.CreatedAt,
		})
	}
	return out, nil
}

// validateEventTypes refuses any requested type that is not some
// EventMapping's PublicType (ErrWebhookEventTypeUnknown naming the first
// offender), mirroring Service.validateScopes' identical
// "fail on the first unknown entry" shape.
func (s *Service) validateEventTypes(types []string) error {
	for _, t := range types {
		if !s.mappings.publicTypes[t] {
			return ErrWebhookEventTypeUnknown.WithParam("event_type", t)
		}
	}
	return nil
}

// validateWebhookURL runs ValidateWebhookURL unless a test has overridden
// it through withWebhookURLValidator -- see that unexported Option's own
// doc comment in module.go for why a test-only override exists at all
// (httptest servers listen on loopback, which production validation must
// always refuse).
func (s *Service) validateWebhookURL(ctx context.Context, url string) error {
	validate := s.urlValidator
	if validate == nil {
		validate = ValidateWebhookURL
	}
	return validate(ctx, url)
}

// toSummary converts a stored row into its API-facing summary, decoding
// EventTypes and wrapping a decode failure as ErrInternal (a corrupt row --
// the column is written only by eventTypesJSON).
func (s *Service) toSummary(row WebhookSubscription) (WebhookSubscriptionSummary, error) {
	types, err := parseEventTypes(row.EventTypes)
	if err != nil {
		return WebhookSubscriptionSummary{}, ErrInternal.WithCause(err)
	}
	return WebhookSubscriptionSummary{
		ID:         row.ID,
		URL:        row.URL,
		EventTypes: types,
		Active:     row.Active,
		CreatedBy:  row.CreatedBy,
		CreatedAt:  row.CreatedAt,
		UpdatedAt:  row.UpdatedAt,
	}, nil
}

// emitWebhookAudit records one audit event for a WebhookSubscription
// mutation, mirroring Service.emit's identical shape for API keys.
func (s *Service) emitWebhookAudit(ctx context.Context, action string, row *WebhookSubscription) error {
	if err := audit.Emit(ctx, s.bus, s.auditActions, audit.Input{
		Action:   action,
		Resource: audit.Resource{Type: "integration.webhook_subscription", ID: row.ID, DisplayName: row.URL},
		Result:   audit.Result{Success: true},
	}); err != nil {
		return ErrInternal.WithCause(err)
	}
	return nil
}

// translateWebhookRepoErr is webhook_service.go's counterpart of
// service.go's translateRepoErr, mapping a dbkit not-found onto
// ErrWebhookSubscriptionNotFound instead of ErrKeyNotFound. See
// translateRepoErr's own doc comment for why matching is by Code, never by
// identity.
func translateWebhookRepoErr(err error) error {
	if found, ok := apperr.As(err); ok && found.Code == dbkit.ErrRecordNotFound.Code {
		return ErrWebhookSubscriptionNotFound
	}
	return ErrInternal.WithCause(err)
}
