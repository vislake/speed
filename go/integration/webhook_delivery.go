package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"

	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is go/integration's event-driven outbound-delivery pipeline:
// the subscriber side that turns a matching internal domain event into one
// signed HTTP POST per matching subscription, per
// docs/internal/07-platform-services.md's outbound-webhook section: a
// tenant subscribes to event types and configures a receiving address; the
// event source is the domain event bus, and business modules need write no
// extra code for it.
//
//   - handleDomainEvent is the pkgcore.EventHandler Module.Register
//     subscribes (through module.go's Module-level forwarding wrapper) for
//     every InternalType a host declared via WithEventMapping. It maps the
//     event to its public schema (eventmapping.go's buildEnvelope) and
//     enqueues one jobs.Task per matching, active subscription.
//   - handleDeliveryJob is the jobs.Handler business logic (wrapped by
//     module.go's webhookDeliveryHandler) that runs one HTTP attempt and
//     settles the WebhookDelivery row.
//   - onWebhookDeliveryDeadLetter is the jobs.FailureHook business logic
//     that marks a delivery dead_letter once jobs has exhausted retries.

// jobTypeWebhookDeliver is the Task.Type of every webhook delivery job this
// module enqueues and handles, following notification's identical
// jobTypeDeliver naming convention.
const jobTypeWebhookDeliver = "integration.webhook.deliver"

// webhookMaxRetries is the bounded retry horizon
// docs/internal/07-platform-services.md's exponential-backoff-retry
// requirement asks for: beyond the first attempt, jobs.StandaloneQueue
// retries up to this
// many times (jobs.WithMaxRetries), each wait growing under its own
// exponential backoff, before the job -- and, through
// onWebhookDeliveryDeadLetter, this module's own delivery row -- moves to
// DeliveryStatusDeadLetter. jobs.DefaultMaxRetries (3) is judged too short
// for a webhook receiver, which is much more likely to be down for a
// transient few minutes than an in-process handler is; six gives a receiver
// roughly the same order-of-magnitude recovery window Stripe's own default
// webhook retry schedule does, without retrying forever.
const webhookMaxRetries = 6

// webhookDeliveryTimeout bounds one HTTP attempt.
const webhookDeliveryTimeout = 10 * time.Second

// webhookDeliveryResponseBudget caps how many bytes of a receiver's
// response body handleDeliveryJob reads before discarding the rest -- just
// enough to let a receiver's error text end up (truncated, like every other
// diagnostic text this module stores) in LastError, without an
// adversarial or malfunctioning receiver being able to make a delivery
// attempt hold an unbounded amount of memory.
const webhookDeliveryResponseBudget = 4096

// webhookDeliveryJobPayload is the job payload handleDomainEvent enqueues
// and handleDeliveryJob decodes: which subscription, which delivery row.
// The row itself -- not the job payload -- carries the actual body to send
// (WebhookDelivery.Payload), so this payload only needs to be an id pair.
type webhookDeliveryJobPayload struct {
	SubscriptionID string `json:"subscription_id"`
	DeliveryID     string `json:"delivery_id"`
}

// handleDomainEvent processes one domain event this module subscribed to
// because some EventMapping declared it as an InternalType. It NEVER
// returns a non-nil error: on the in-memory EventBus, a subscriber's error
// is joined back into the PUBLISHER's own Publish call
// (pkgcore.memoryEventBus.Publish's own doc comment), so a webhook wiring
// problem in this module must never surface as a failure of, say, org's own
// membership-creation call. Every failure below is logged at Warn and
// swallowed, following org.handleUserCreated's identical resilience
// contract for the same reason.
func (s *Service) handleDomainEvent(ctx context.Context, evt pkgcore.Event) error {
	log := obs.FromContext(ctx)

	mapping, ok := s.mappings.byInternal[evt.Type]
	if !ok {
		// Defensive: Module.Register only ever subscribes to a type some
		// mapping declared, so this is unreachable through any path that
		// goes through this module's own wiring. Kept because a handler is
		// reachable by whatever else has a reference to the same EventBus.
		return nil
	}
	if evt.TenantID == "" {
		// A platform-wide event with no tenant has no subscription to fan
		// out to -- every WebhookSubscription belongs to exactly one
		// tenant. Mirrors org.handleUserCreated's identical no-tenant skip.
		log.Debug("integration skipped a domain event with no tenant for webhook fan-out",
			"event_type", evt.Type)
		return nil
	}
	ctx = pkgcore.WithTenant(ctx, evt.TenantID)

	subs, err := s.matchingSubscriptions(ctx, mapping.PublicType)
	if err != nil {
		log.Warn("integration could not list webhook subscriptions for a domain event",
			"event_type", evt.Type, "error", err)
		return nil
	}
	if len(subs) == 0 {
		return nil
	}

	body, err := buildEnvelope(ctx, mapping, evt)
	if err != nil {
		log.Warn("integration could not map a domain event onto its public webhook schema",
			"event_type", evt.Type, "public_type", mapping.PublicType, "error", err)
		return nil
	}

	for _, sub := range subs {
		if err := s.enqueueDelivery(ctx, sub, mapping, body); err != nil {
			log.Warn("integration could not enqueue a webhook delivery",
				"subscription_id", sub.ID, "event_type", evt.Type, "error", err)
		}
	}
	return nil
}

// matchingSubscriptions returns every active subscription of the tenant in
// ctx whose EventTypes includes publicType. A subscription row whose
// EventTypes fails to decode is skipped and logged, rather than failing the
// whole fan-out over one corrupt row -- the column is written only by
// eventTypesJSON, so this should be unreachable, but a fan-out that already
// found several genuinely matching subscribers should still reach them.
func (s *Service) matchingSubscriptions(ctx context.Context, publicType string) ([]WebhookSubscription, error) {
	all, err := s.webhookRepo.ListActiveByTenant(ctx)
	if err != nil {
		return nil, err
	}
	matched := make([]WebhookSubscription, 0, len(all))
	for _, sub := range all {
		types, err := parseEventTypes(sub.EventTypes)
		if err != nil {
			obs.FromContext(ctx).Warn("integration skipped a webhook subscription with an unreadable event_types column",
				"subscription_id", sub.ID, "error", err)
			continue
		}
		if slices.Contains(types, publicType) {
			matched = append(matched, sub)
		}
	}
	return matched, nil
}

// enqueueDelivery creates (or, on a redelivered domain event, finds) the
// WebhookDelivery row for (sub, the event body's own idempotency key), and
// enqueues its delivery job.
//
// # Idempotent fan-out
//
// deriveWebhookDeliveryKey is recomputed from the SAME inputs on every call
// -- subscription, public type/version, and the rendered body -- so an
// at-least-once redelivery of the underlying domain event (which any real
// EventBus implementation, including a distributed one, may produce)
// derives the identical key and finds the row ByIdempotencyKey already
// created, rather than creating a second one and sending the receiver two
// copies. The uq_integration_webhook_deliveries_tenant_subscription_key
// index is the backstop for the race two concurrent handlers of a
// redelivery could otherwise hit: Create's own unique-constraint failure is
// treated exactly like "found by the probe", never surfaced as an error.
func (s *Service) enqueueDelivery(ctx context.Context, sub WebhookSubscription, mapping EventMapping, body []byte) error {
	tenantID, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return pkgcore.ErrNoTenant
	}

	key := deriveWebhookDeliveryKey(sub.ID, mapping.PublicType, mapping.PublicVersion, body)

	existing, err := s.deliveryRepo.ByIdempotencyKey(ctx, sub.ID, key)
	if err != nil {
		return err
	}
	delivery := existing
	if delivery == nil {
		delivery = &WebhookDelivery{
			ID:             uuid.NewString(),
			SubscriptionID: sub.ID,
			EventType:      mapping.PublicType,
			EventVersion:   mapping.PublicVersion,
			IdempotencyKey: key,
			Payload:        datatypes.JSON(body),
			Status:         DeliveryStatusPending,
		}
		if createErr := s.deliveryRepo.Create(ctx, delivery); createErr != nil {
			// A concurrent handler may have won the race and already
			// created the row under the same unique key -- adopt it rather
			// than surfacing a spurious failure for what is, from the
			// tenant's point of view, one successful fan-out.
			winner, probeErr := s.deliveryRepo.ByIdempotencyKey(ctx, sub.ID, key)
			if probeErr != nil {
				return createErr
			}
			if winner == nil {
				return createErr
			}
			delivery = winner
		}
	}

	if s.queue == nil {
		obs.FromContext(ctx).Warn("integration has no jobs.Queue wired (WithWebhookQueue); webhook delivery recorded but not enqueued",
			"subscription_id", sub.ID, "delivery_id", delivery.ID)
		return nil
	}

	payload, err := json.Marshal(webhookDeliveryJobPayload{SubscriptionID: sub.ID, DeliveryID: delivery.ID})
	if err != nil {
		return fmt.Errorf("integration: encode webhook delivery job payload: %w", err)
	}

	// IdempotencyKey is set too (jobs.Task's own field), a second line of
	// defense on top of the WebhookDelivery-level dedupe above: even if two
	// calls both got past the row-level race and reached here, jobs.Queue
	// itself enqueues the underlying Job at most once for this
	// (tenant, key) pair.
	_, err = s.queue.Enqueue(ctx, jobs.Task{
		Type:           jobTypeWebhookDeliver,
		TenantID:       tenantID,
		Payload:        payload,
		IdempotencyKey: key,
	}, jobs.WithMaxRetries(webhookMaxRetries))
	return err
}

// deriveWebhookDeliveryKey derives the key that makes one (subscription,
// event) fan-out idempotent, mirroring notification's
// deriveDeliveryKey in spirit (canonical inputs, hashed) though simpler in
// shape: the rendered body already IS the canonical form of "what this
// event means", since buildEnvelope produced it deterministically from the
// event and the mapping.
func deriveWebhookDeliveryKey(subscriptionID, publicType, publicVersion string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(subscriptionID))
	h.Write([]byte{0})
	h.Write([]byte(publicType))
	h.Write([]byte{0})
	h.Write([]byte(publicVersion))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// handleDeliveryJob is the jobs.Handler business logic for one webhook
// delivery attempt, wrapped as a jobs.Handler by module.go's
// webhookDeliveryHandler. ctx already carries the job's tenant (jobs
// rebuilds it before calling Handle), so every repository call below
// resolves the tenant that owns both the subscription and the delivery row.
//
// Returning a non-nil error tells jobs to retry (up to webhookMaxRetries);
// returning nil marks the attempt terminal one way or another -- delivered,
// or a refusal this module has judged will never resolve by retrying
// (the subscription was deleted or paused after the delivery was
// enqueued).
func (s *Service) handleDeliveryJob(ctx context.Context, job *jobs.Job) (jobs.Result, error) {
	var payload webhookDeliveryJobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return jobs.Result{}, fmt.Errorf("integration: decode webhook delivery job payload: %w", err)
	}

	delivery, err := s.deliveryRepo.FindByID(ctx, payload.DeliveryID)
	if err != nil {
		return jobs.Result{}, fmt.Errorf("integration: load webhook delivery %s: %w", payload.DeliveryID, err)
	}
	if delivery.Status == DeliveryStatusDelivered {
		// A retry that arrived after an earlier attempt's success already
		// settled the row -- see jobs.Task.IdempotencyKey's own doc comment
		// for why a redelivered job for an already-succeeded key can still
		// reach a worker. Nothing more to do.
		return jobs.Result{}, nil
	}

	sub, err := s.webhookRepo.FindByID(ctx, delivery.SubscriptionID)
	if err != nil {
		// The subscription was mark-deleted after this delivery was
		// enqueued (webhook_service.go's DeleteWebhookSubscription
		// deliberately leaves past delivery rows in place, and dbkit's
		// soft-delete auto-scope plugin hides the mark-deleted row from
		// this very FindByID call exactly as a physical DELETE always did
		// before this module adopted dbkit.SoftDeletable). There is no URL
		// and no secret to deliver with any more, and none will reappear
		// within this job's own bounded retry horizon (webhookMaxRetries)
		// by retrying, so this is terminal -- a caller wanting delivery to
		// resume calls RestoreWebhookSubscription (plus, per its own doc
		// comment, an explicit UpdateWebhookSubscription to reactivate it),
		// which produces a FRESH delivery off the next matching domain
		// event rather than reviving this already-settled row.
		return jobs.Result{}, s.settleTerminal(ctx, delivery, "webhook subscription no longer exists")
	}
	if !sub.Active {
		// Paused after this delivery was enqueued. Round 2 does not
		// requeue a paused delivery when the subscription is reactivated
		// (see AGENTS.md's "Deliberately not in scope" table) -- the
		// attempt is recorded terminal rather than retried to exhaustion
		// against a receiver the tenant asked to stop hearing from.
		return jobs.Result{}, s.settleTerminal(ctx, delivery, "webhook subscription is inactive")
	}

	statusCode, sendErr := s.attemptDelivery(ctx, sub, delivery)

	now := s.clock()
	delivery.Attempts++
	delivery.LastAttemptAt = &now
	if statusCode != 0 {
		delivery.LastStatusCode = &statusCode
	}

	if sendErr == nil {
		delivery.Status = DeliveryStatusDelivered
		delivery.DeliveredAt = &now
		delivery.LastError = ""
		if err := s.deliveryRepo.Update(ctx, delivery); err != nil {
			// The send succeeded but the record did not land -- report the
			// write failure so the job retries; a retry that lands on an
			// already-delivered receiver simply sends a second, harmless
			// duplicate (webhooks are not required to be exactly-once on
			// this module's send side any more than notification's own
			// sends are -- see AGENTS.md's Known limitations).
			return jobs.Result{}, err
		}
		return jobs.Result{}, nil
	}

	delivery.Status = DeliveryStatusFailed
	delivery.LastError = truncateWebhookErrorText(sendErr.Error())
	if err := s.deliveryRepo.Update(ctx, delivery); err != nil {
		return jobs.Result{}, errors.Join(sendErr, err)
	}
	return jobs.Result{}, sendErr
}

// settleTerminal marks delivery DeliveryStatusDeadLetter with reason and
// saves it, for the two handleDeliveryJob refusals that are known never to
// resolve by retrying (the subscription is gone, or paused). Reusing
// DeliveryStatusDeadLetter here (rather than inventing a third terminal
// status) keeps the state machine to the two terminal states
// webhook_model.go documents; the LastError text is what tells an operator
// apart a normal retries-exhausted dead-letter from one of these two early
// refusals.
func (s *Service) settleTerminal(ctx context.Context, delivery *WebhookDelivery, reason string) error {
	delivery.Status = DeliveryStatusDeadLetter
	delivery.LastError = reason
	return s.deliveryRepo.Update(ctx, delivery)
}

// attemptDelivery runs exactly one HTTP POST of delivery.Payload to
// sub.URL, signed per webhook_signature.go, and returns the response status
// code (0 if the request never received one) and an error describing why
// the attempt should count as a failure -- a transport error, or any
// non-2xx status.
func (s *Service) attemptDelivery(ctx context.Context, sub *WebhookSubscription, delivery *WebhookDelivery) (int, error) {
	timestamp := s.clock().Unix()
	signature := signWebhookPayload(sub.Secret, timestamp, delivery.Payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.URL, bytes.NewReader(delivery.Payload))
	if err != nil {
		return 0, fmt.Errorf("integration: build webhook delivery request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderWebhookID, delivery.ID)
	req.Header.Set(HeaderWebhookTimestamp, strconv.FormatInt(timestamp, 10))
	req.Header.Set(HeaderWebhookSignature, signature)

	client := s.httpClient
	if client == nil {
		// See ssrf.go's own file comment for why this transport re-validates
		// the destination at dial time on every attempt, not only once at
		// subscription-creation time.
		client = newSafeHTTPClient(webhookDeliveryTimeout)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("integration: webhook delivery request: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, webhookDeliveryResponseBudget))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, nil
	}

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, webhookDeliveryResponseBudget))
	return resp.StatusCode, fmt.Errorf("integration: webhook receiver answered %d: %s", resp.StatusCode, snippet)
}

// truncateWebhookErrorText enforces webhookDeliveryErrorBudget, matching
// notification's identical truncate-at-the-write-site convention.
func truncateWebhookErrorText(text string) string {
	if len(text) > webhookDeliveryErrorBudget {
		return text[:webhookDeliveryErrorBudget]
	}
	return text
}

// onWebhookDeliveryDeadLetter is the jobs.FailureHook business logic,
// wrapped by module.go's webhookDeliveryHandler, called at most once per
// Job strictly after jobs has already persisted it as StatusDeadLetter
// (jobs.FailureHook's own doc comment). It is this module's ONLY
// compensation for an exhausted retry horizon, per the root CLAUDE.md's
// "the queue offers an OnFailure hook; ... belongs to the business module"
// rule -- here, that compensation is simply recording the terminal state a
// tenant's delivery-log view (Service.ListRecentWebhookDeliveries) shows.
func (s *Service) onWebhookDeliveryDeadLetter(ctx context.Context, job *jobs.Job, cause error) {
	log := obs.FromContext(ctx)

	var payload webhookDeliveryJobPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		log.Warn("integration could not decode a dead-lettered webhook delivery job's payload",
			"job_id", string(job.ID), "error", err)
		return
	}

	delivery, err := s.deliveryRepo.FindByID(ctx, payload.DeliveryID)
	if err != nil {
		log.Warn("integration could not load a dead-lettered webhook delivery",
			"delivery_id", payload.DeliveryID, "error", err)
		return
	}
	if delivery.Status == DeliveryStatusDelivered || delivery.Status == DeliveryStatusDeadLetter {
		// Already terminal -- a race between this hook and a concurrent
		// settle, or a hook invoked twice, changes nothing.
		return
	}

	delivery.Status = DeliveryStatusDeadLetter
	delivery.LastError = truncateWebhookErrorText(cause.Error())
	if err := s.deliveryRepo.Update(ctx, delivery); err != nil {
		log.Warn("integration could not record a webhook delivery as dead-lettered",
			"delivery_id", delivery.ID, "error", err)
	}
}
