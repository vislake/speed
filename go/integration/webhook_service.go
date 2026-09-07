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

	result := &CreatedWebhookSubscription{
		ID:         row.ID,
		URL:        row.URL,
		EventTypes: in.EventTypes,
		Secret:     secret,
		Active:     true,
		CreatedBy:  in.CreatedBy,
		CreatedAt:  row.CreatedAt,
	}

	// An audit-recording failure AFTER the row committed must not lose the
	// signing secret: Secret is shown exactly once and nothing this module
	// returns after this call ever reproduces it (see that field's own doc
	// comment) -- a caller sent away empty-handed could never learn the
	// secret of a subscription that genuinely exists and is already
	// delivering events signed with it. The result is therefore returned
	// alongside the error, the identical (value, err) partial-failure
	// contract Service.Create's own doc comment documents for the API-key
	// create surface (service.go). See handler.go's
	// integration_createWebhookSubscription for the HTTP translation.
	if err := s.emitWebhookAudit(ctx, AuditActionWebhookSubscriptionCreate, row); err != nil {
		return result, err
	}

	return result, nil
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
//
// # The write is a targeted column update, and the deletion always wins
//
// The changed columns -- and only the changed columns -- are written through
// WebhookSubscriptionRepository.updateFields, never a full-row save of the
// row read above: an Update that raced a concurrent
// DeleteWebhookSubscription with a stale full-row save would write that
// stale copy's nil DeletedAt back over the mark-delete, resurrecting a
// subscription the tenant already deleted -- Active value included (see
// updateFields' own doc comment for the full argument). A subscription
// deleted between this method's FindByID read and its write matches
// updateFields' live-rows-only WHERE clause with nothing, and this method
// then answers ErrWebhookSubscriptionNotFound, exactly as if the deletion
// had landed before the read. An input with no field set at all (every
// field nil) writes nothing and still records the update audit event,
// matching the call's pre-existing "an update was requested" accounting.
func (s *Service) UpdateWebhookSubscription(ctx context.Context, in UpdateWebhookSubscriptionInput) (*WebhookSubscriptionSummary, error) {
	row, err := s.webhookRepo.FindByID(ctx, in.ID)
	if err != nil {
		return nil, translateWebhookRepoErr(err)
	}

	var changes webhookSubscriptionChanges
	if in.URL != nil {
		if *in.URL == "" {
			return nil, ErrWebhookURLRequired
		}
		if urlErr := s.validateWebhookURL(ctx, *in.URL); urlErr != nil {
			return nil, urlErr
		}
		row.URL = *in.URL
		changes.URL = in.URL
	}
	if in.EventTypes != nil {
		if len(in.EventTypes) == 0 {
			return nil, ErrEventTypesRequired
		}
		if typesErr := s.validateEventTypes(in.EventTypes); typesErr != nil {
			return nil, typesErr
		}
		row.EventTypes = eventTypesJSON(in.EventTypes)
		changes.EventTypes = in.EventTypes
	}
	if in.Active != nil {
		row.Active = *in.Active
		changes.Active = in.Active
	}

	if changes.URL != nil || changes.EventTypes != nil || changes.Active != nil {
		matched, updateErr := s.webhookRepo.updateFields(ctx, in.ID, changes)
		if updateErr != nil {
			return nil, ErrInternal.WithCause(updateErr)
		}
		if !matched {
			// Deleted between the FindByID read above and this write --
			// deletion wins, indistinguishable from "already gone".
			return nil, ErrWebhookSubscriptionNotFound
		}
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

// DeleteWebhookSubscription mark-deletes one subscription of the caller's
// tenant -- WebhookSubscription's dbkit.SoftDeletable adoption
// (webhook_model.go's own doc comment) is what makes the promoted
// webhookRepo.Delete(ctx, id) call below an UPDATE setting deleted_at/
// deleted_by rather than a physical DELETE. Its past WebhookDelivery rows
// are left in place as history either way -- see that field's own doc
// comment in webhook_model.go for why this module never cascades the
// delete (no cross-module-style foreign keys, even within this module's
// own two tables) -- and a delivery already enqueued before this call
// settles exactly as it always did: handleDeliveryJob's own
// webhookRepo.FindByID lookup on the subscription is hidden from a
// mark-deleted row by dbkit's soft-delete auto-scope plugin exactly as it
// was made impossible by a physical DELETE before this round, so an
// in-flight delivery still resolves ErrRecordNotFound and settles terminal
// with "webhook subscription was deleted" (webhook_delivery.go's
// handleDeliveryJob) -- and ONLY genuine not-found settles that way: any
// other FindByID failure (a transient store error, a secret that no longer
// decrypts) is returned so the job retries, never dead-lettered as if the
// subscription were the problem (see handleDeliveryJob's own doc comment).
//
// See RestoreWebhookSubscription for undoing this, and
// go/integration/AGENTS.md's "Soft deletion" section for the round's full
// design record, including why a terminal delivery marked "webhook
// subscription was deleted" does not mean the row can never reappear the
// way it could not before a Restore existed -- a restored subscription gets
// FRESH deliveries off the next matching domain event, never a replay of
// the already-settled one (see handleDeliveryJob's own doc comment).
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

// RestoreWebhookSubscription undoes the mark-delete DeleteWebhookSubscription
// made to the subscription named by id, in the caller's tenant -- symmetric
// with DeleteWebhookSubscription's own (ctx, id) shape, since a
// subscription's natural key genuinely is its own opaque id (unlike, say,
// go/rbac's RoleBinding, whose RestoreRole instead takes the tuple that
// identifies a grant, because a caller revoking a grant is not expected to
// have kept the binding's own row id around -- see that method's doc
// comment). It reports ErrWebhookSubscriptionNotFound both for an id with
// nothing to restore and for an id that exists but is not currently
// mark-deleted -- the identical collapsed not-found signal
// dbkit.Repository[T].Restore's own doc comment describes, matching go/org's
// MemberService.Restore and go/rbac's RestoreRole precedent, so a caller
// cannot learn which case it hit from the error shape alone.
//
// # Restore always lands the subscription PAUSED (Active = false),
// regardless of what Active held at the moment it was deleted
//
// This is a deliberate divergence from go/org's and go/rbac's own Restore
// methods, both of which change nothing about the restored row but its two
// soft-delete columns (see go/org/AGENTS.md's and go/rbac/AGENTS.md's "Soft
// deletion" sections, and rbac's RestoreRole doc comment, for the reasoning
// behind that choice in THEIR domains). The reason this round does not
// follow that precedent unchanged is that org's and rbac's Restore calls
// resume a purely INTERNAL fact -- an organization membership, an
// authorization grant -- evaluated fresh against the rest of the system on
// every read. Restoring a WebhookSubscription with Active still true is
// different in kind: webhook_delivery.go's handleDomainEvent fans out to
// every ACTIVE subscription automatically, on every matching domain event,
// entirely without further human action -- so an unconditional restore
// would silently resume POSTing the tenant's live event data to an
// external, third-party URL nobody has looked at again since the
// subscription was deleted, however long ago that was and however stale
// its URL or receiving system might now be. That is a real-world side
// effect leaving this process, not an internal state change this codebase
// can fully reason about the safety of the way it can for an org membership
// or an rbac grant. Forcing Active = false here costs the caller exactly
// one extra explicit step -- an UpdateWebhookSubscription call setting
// Active back to true -- to resume delivery, which is the same "no implicit
// side effect on a structural edit" discipline this codebase already
// applies elsewhere (see go/org/AGENTS.md's identical framing for why
// TreeService.Delete does not re-parent orphans and does not cascade
// Restore). A caller wanting the subscription resumed in one round trip
// simply follows Restore with such a call; nothing here prevents that.
//
// # The restore and the forced pause land in ONE guarded write
//
// The unmark (clearing deleted_at/deleted_by) and the forced pause
// (Active = false) are performed by a single UPDATE --
// WebhookSubscriptionRepository.restorePaused -- never by the
// restore-then-pause pair of separate writes this method first shipped
// with. Two writes created a real window on both sides of the pause: the
// row was LIVE with Active still true between the unmark's commit and the
// pause's, which is the exact state handleDomainEvent's fan-out matches
// (webhook_delivery.go's matchingSubscriptions, over ListActiveByTenant) --
// so a matching domain event observed in that window was silently fanned
// out to the external URL this pause exists to stop POSTing to -- and a
// pause write that failed left the row permanently restored-ACTIVE while
// this method reported ErrInternal, a half-restored state whose repair a
// retry could never reach (the row was no longer mark-deleted, so the
// retry answered the collapsed not-found). One statement closes both
// hazards: no interleaving can observe the row live-and-active between two
// writes that are one write, and a failure of the single write leaves the
// row exactly as it was -- still mark-deleted, never restored at all --
// failing closed toward "nothing resumes delivering" rather than toward an
// unnoticed resumption.
//
// The write's own guard preserves every interleaving property this
// method's contract promises: its WHERE requires deleted_at IS NOT NULL, so
// only a row this call exists to restore matches -- the collapsed
// not-found above, a row that is live (never deleted, or already restored,
// or another tenant's, the tenant filter coming from the same tenant-scope
// plugin the update path relies on) all match nothing and all answer the
// one ErrWebhookSubscriptionNotFound -- and a concurrent
// DeleteWebhookSubscription whose mark-delete commits first wins the race
// (matched == false, the row stays dead) rather than being silently undone
// by the restore, exactly as on the update path.
//
// Restore does not re-validate the restored row against
// CreateWebhookSubscription's own preconditions beyond the collapsed
// not-found check above (its URL is not re-run through ValidateWebhookURL,
// for instance): it changes nothing about the row but its two soft-delete
// columns and, per the paragraph above, Active. A caller wanting every
// modern invariant re-checked calls UpdateWebhookSubscription afterward.
func (s *Service) RestoreWebhookSubscription(ctx context.Context, id string) error {
	matched, err := s.webhookRepo.restorePaused(ctx, id)
	if err != nil {
		return ErrInternal.WithCause(err)
	}
	if !matched {
		// Nothing was mark-deleted under id in this tenant: the id never
		// existed, the subscription is live (never deleted, or already
		// restored), it belongs to another tenant, or a concurrent
		// re-delete won the race. One collapsed signal, indistinguishable
		// by design -- see the doc comment above.
		return ErrWebhookSubscriptionNotFound
	}

	row, err := s.webhookRepo.FindByID(ctx, id)
	if err != nil {
		// A re-delete that committed between the restore's write and this
		// read hides the row again: the deletion wins, indistinguishable
		// from a restore that never happened.
		return translateWebhookRepoErr(err)
	}

	return s.emitWebhookAudit(ctx, AuditActionWebhookSubscriptionRestore, row)
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
// falls back to defaultRecentDeliveriesLimit, and a limit above
// maxRecentDeliveriesLimit is clamped to it (the fragment's own limit
// parameter declares the identical 100 ceiling -- this clamp is its
// enforcement, since the generated parameter binding performs no range
// validation of its own). See
// WebhookDeliveryRepository.ListRecentBySubscription.
//
// subscriptionID is not itself verified to exist (unlike Update/Delete):
// an id belonging to another tenant, or no subscription at all, simply
// returns an empty slice, exactly as a genuine subscription with zero
// deliveries would -- there is no observable difference between the two,
// which is deliberate for the identical cross-tenant-enumeration reason
// ErrKeyNotFound's own doc comment gives.
func (s *Service) ListRecentWebhookDeliveries(ctx context.Context, subscriptionID string, limit int) ([]WebhookDeliverySummary, error) {
	if limit > maxRecentDeliveriesLimit {
		limit = maxRecentDeliveriesLimit
	}
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

// validateWebhookURL runs ValidateWebhookURL unless a test or demo host
// explicitly opted into an override through WithWebhookURLValidator -- see
// that Option's own doc comment in module.go for why this seam exists at
// all and why a production host must never call it.
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
//
// row.URL -- a VARCHAR(2048) column (see the migration files) -- travels
// verbatim into audit.Resource.DisplayName, which audit_events stores in
// resource_display_name (VARCHAR(255)): a source wider than its target,
// by deliberate design. The fitting happens at the audit write path, not
// here: go/dbkit/audit's Repository.Insert cuts caller-supplied content
// to its columns (an over-long URL is legal content and is stored cut at
// 255 runes, with the cut recorded in a structured warning -- see
// go/dbkit/audit/AGENTS.md's "Column bounds" section), so a legal long
// URL must never fail this audit event after the subscription row has
// already committed.
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

// isWebhookRecordNotFound reports whether err is dbkit's not-found error
// (by Code, never by identity -- see translateRepoErr's own doc comment for
// why). It is the classifier webhook_delivery.go's handleDeliveryJob uses to
// tell the one FindByID failure that means "the subscription is gone" from
// every other FindByID failure a delivery attempt can hit (a transient store
// error, a secret that no longer decrypts) -- only the former settles the
// delivery terminal; the latter must retry (see handleDeliveryJob's own doc
// comment).
func isWebhookRecordNotFound(err error) bool {
	found, ok := apperr.As(err)
	return ok && found.Code == dbkit.ErrRecordNotFound.Code
}

// translateWebhookRepoErr is webhook_service.go's counterpart of
// service.go's translateRepoErr, mapping a dbkit not-found onto
// ErrWebhookSubscriptionNotFound instead of ErrKeyNotFound. See
// translateRepoErr's own doc comment for why matching is by Code, never by
// identity.
func translateWebhookRepoErr(err error) error {
	if isWebhookRecordNotFound(err) {
		return ErrWebhookSubscriptionNotFound
	}
	return ErrInternal.WithCause(err)
}
