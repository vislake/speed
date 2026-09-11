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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/datatypes"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/jobs"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// This file is go/integration's event-driven outbound-delivery pipeline:
// the subscriber side that turns a matching internal domain event into one
// signed HTTP POST per matching subscription, per the outbound-webhook
// design: a tenant subscribes to event types and configures a receiving
// address; the event source is the domain event bus, and business modules
// need write no extra code for it.
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

// webhookMaxRetries is the bounded retry horizon the design's
// exponential-backoff-retry requirement asks for: beyond the first
// attempt, jobs.StandaloneQueue
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

	// occurrenceAt is this fan-out's occurrence marker: one reading of the
	// service clock taken at the subscription boundary for THIS arrival of
	// the event, shared by every subscription the event fans out to. It is
	// what keeps two genuinely distinct occurrences of one event type -- a
	// member removed and later re-added, say, whose public bodies are
	// byte-identical -- from collapsing into one delivery; see
	// deriveWebhookDeliveryKey's own doc comment for the full argument and
	// for what the marker cannot distinguish.
	occurrenceAt := s.clock()

	for _, sub := range subs {
		if err := s.enqueueDelivery(ctx, sub, mapping, body, occurrenceAt); err != nil {
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
// WebhookDelivery row for (sub, the event's own delivery key), and enqueues
// its delivery job.
//
// # Idempotent fan-out -- within one observed occurrence
//
// deriveWebhookDeliveryKey is recomputed from the SAME inputs on every call
// -- subscription, public type/version, the rendered body, and the
// occurrence marker occurrenceAt that handleDomainEvent stamped at the
// subscription boundary for this arrival of the event. Two calls that
// present the identical combination therefore derive the identical key and
// find the row ByIdempotencyKey already created, rather than creating a
// second one and sending the receiver two copies; the
// uq_integration_webhook_deliveries_tenant_subscription_key index is the
// backstop for the race two concurrent handlers of one redelivery could
// otherwise hit: Create's own unique-constraint failure is treated exactly
// like "found by the probe", never surfaced as an error.
//
// The marker is what bounds how much of an at-least-once redelivery this
// dedupe can merge: pkgcore.Event carries no occurrence identity of its own
// (no id, no timestamp -- see pkgcore's Event type), so the subscription
// boundary's own reading of the clock is the only signal this module has
// for "same event, again". A redelivery observed at the same clock reading
// as the original arrival merges into it; one observed later does not.
// Bounding the merge that way is a deliberate trade: an unbounded
// content-hash dedupe would silently drop a genuinely NEW occurrence whose
// public body is byte-identical to an older one's (a member removed and
// later re-added, when the mapping's payload names only the member), which
// is a lost delivery a receiver can never recover -- whereas a duplicate
// delivery of one occurrence is at-least-once behavior receivers already
// dedupe against through HeaderWebhookID (see webhook_signature.go's
// header docs).
func (s *Service) enqueueDelivery(ctx context.Context, sub WebhookSubscription, mapping EventMapping, body []byte, occurrenceAt time.Time) error {
	tenantID, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return pkgcore.ErrNoTenant
	}

	key := deriveWebhookDeliveryKey(sub.ID, mapping.PublicType, mapping.PublicVersion, body, occurrenceAt)

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

// deriveWebhookDeliveryKey derives the key that makes one
// (subscription, observed occurrence) fan-out idempotent, mirroring
// notification's deriveDeliveryKey in spirit (canonical inputs, hashed)
// though simpler in shape: the rendered body already IS the canonical form
// of "what this event means", since buildEnvelope produced it
// deterministically from the event and the mapping.
//
// # The occurrence marker -- why the body alone is not the key
//
// The final input, occurrenceAt (the subscription-boundary clock reading
// handleDomainEvent stamped for this arrival of the event), is what stops
// the key from conflating two genuinely distinct occurrences of one event
// type whose public bodies are byte-identical -- the same member removed
// and later re-added, when the mapping's payload names only the member.
// Keyed on body alone, the second occurrence would probe the first's
// already-settled delivery row and be silently dropped: a webhook delivery
// the receiver never receives and this module never retries. Keyed with
// the marker, each occurrence derives its own key and fans out on its own
// -- and, because buildEnvelope is deterministic, an at-least-once
// redelivery of the SAME occurrence observed at the SAME clock reading
// still derives the identical key and merges (see enqueueDelivery's own
// doc comment for what the marker cannot merge, and why that is the right
// trade). pkgcore.Event itself carries no id or timestamp the envelope
// could carry instead -- see that type's fields -- so the subscription
// boundary's own clock is the only occurrence signal this module has.
func deriveWebhookDeliveryKey(subscriptionID, publicType, publicVersion string, body []byte, occurrenceAt time.Time) string {
	h := sha256.New()
	h.Write([]byte(subscriptionID))
	h.Write([]byte{0})
	h.Write([]byte(publicType))
	h.Write([]byte{0})
	h.Write([]byte(publicVersion))
	h.Write([]byte{0})
	h.Write(body)
	h.Write([]byte{0})
	h.Write([]byte(strconv.FormatInt(occurrenceAt.UTC().UnixNano(), 10)))
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
		// Only a genuine record-not-found is the subscription-gone refusal
		// that is terminal: the subscription was mark-deleted after this
		// delivery was enqueued (webhook_service.go's
		// DeleteWebhookSubscription deliberately leaves past delivery rows
		// in place, and dbkit's soft-delete auto-scope plugin hides the
		// mark-deleted row from this very FindByID call). There is no URL
		// and no secret to deliver
		// with any more, and none will reappear within this job's own
		// bounded retry horizon (webhookMaxRetries) by retrying, so this is
		// terminal -- a caller wanting delivery to resume calls
		// RestoreWebhookSubscription (plus, per its own doc comment, an
		// explicit UpdateWebhookSubscription to reactivate it), which
		// produces a FRESH delivery off the next matching domain event
		// rather than reviving this already-settled row.
		//
		// Any OTHER FindByID failure -- a transient store error, a secret
		// whose stored ciphertext no longer decrypts -- is not that
		// refusal, and settling it terminal here would both dead-letter a
		// delivery whose only problem was a moment of bad luck and record a
		// LastError that blames the subscription for it. Those errors are
		// returned so jobs retries them, exactly like a failed HTTP attempt.
		if !dbkit.IsRecordNotFound(err) {
			return jobs.Result{}, fmt.Errorf("integration: load webhook subscription %s: %w", delivery.SubscriptionID, err)
		}
		return jobs.Result{}, s.settleTerminal(ctx, delivery, "webhook subscription was deleted")
	}
	if !sub.Active {
		// Paused after this delivery was enqueued. A paused delivery is
		// not requeued when the subscription is reactivated -- the attempt
		// is recorded terminal rather than retried to exhaustion against a
		// receiver the tenant asked to stop hearing from.
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
			// sends are).
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
		// The module-level default, built once at package init -- see
		// webhook_guard.go's defaultWebhookHTTPClient doc comment for why
		// every delivery attempt shares one transport instead of each
		// building its own. Its own file comment also explains why this
		// transport re-validates the destination at dial time on every
		// attempt, not only once at subscription-creation time.
		client = defaultWebhookHTTPClient
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

// truncateWebhookErrorText cuts a failure text to webhookDeliveryErrorBudget
// runes and guarantees valid UTF-8 -- the two properties the last_error
// column (VARCHAR(4000) on both dialects, webhook_model.go) requires on
// PostgreSQL: VARCHAR(n) counts CHARACTERS, so a cut by rune (not byte) is
// what actually fits, and a UTF-8-encoded database refuses a value carrying
// invalid byte sequences outright (SQL error 22021), which would make every
// failure-path update of the delivery row fail -- permanently wedging the
// record, since the retry horizon's dead-letter write fails the same way.
// The failure text is exactly the kind of value that can carry either
// hazard: attemptDelivery echoes a receiver's raw response-body bytes into
// it, and a receiver may answer its body in any encoding, so both a
// multi-byte text crossing byte 4000 mid-rune and invalid bytes within the
// budget are ordinary inputs, never exotic ones. Invalid bytes are
// therefore rendered as the Unicode replacement character -- never silently
// dropped, since dropping them could concatenate two arbitrary byte runs
// into a different valid value -- and the cut happens after that
// sanitization, on the resulting runes, mirroring go/sharing's
// truncateAccessLogValue (go/sharing/service.go, which handles the
// identical hazard for a caller-controlled User-Agent or Referer) and
// go/authn's truncateClientField (go/authn/model.go). Matches
// notification's identical truncate-at-the-write-site convention.
func truncateWebhookErrorText(text string) string {
	if len(text) <= webhookDeliveryErrorBudget && utf8.ValidString(text) {
		return text
	}
	runes := []rune(strings.ToValidUTF8(text, "\uFFFD"))
	if len(runes) > webhookDeliveryErrorBudget {
		runes = runes[:webhookDeliveryErrorBudget]
	}
	return string(runes)
}

// onWebhookDeliveryDeadLetter is the jobs.FailureHook business logic,
// wrapped by module.go's webhookDeliveryHandler, called at most once per
// Job strictly after jobs has already persisted it as StatusDeadLetter
// (jobs.FailureHook's own doc comment). It is this module's ONLY
// compensation for an exhausted retry horizon, per the rule that business
// compensation belongs to the business module, never the queue layer --
// here, that compensation is simply recording the terminal state a
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

// RedeliverWebhookDelivery re-enqueues one dead-lettered delivery of the
// caller's tenant for a fresh attempt cycle: the delivery's stored Payload
// is sent again through the ordinary handleDeliveryJob path, against the
// subscription it was originally fanned out from, with the delivery row
// updated in place -- the row's identity is the (subscription, occurrence)
// pair (see WebhookDelivery's own doc comment), so a redelivery is more
// attempts on the SAME row, never a second row for one occurrence.
//
// # Who may be redelivered, and when the call refuses
//
// Exactly the terminal-failure state may be redelivered, and every other
// state is refused with a code naming the reason:
//
//   - An id that does not exist in the caller's tenant answers
//     ErrWebhookDeliveryNotFound (collapsed across tenants like every
//     other not-found in this module).
//   - A delivery whose Status is not DeliveryStatusDeadLetter answers
//     ErrWebhookDeliveryNotDeadLetter: a pending or failed delivery already
//     has a live job of its own under the queue (a second job would
//     double-send), and a delivered one already reached the receiver.
//     Dead-letter is the one state with no live job left -- its retry
//     horizon is exhausted -- which is exactly the state this method
//     exists to pull back from.
//   - A dead-lettered delivery whose subscription has since been
//     mark-deleted answers ErrWebhookSubscriptionNotFound, and one whose
//     subscription is paused (Active = false) answers
//     ErrWebhookSubscriptionInactive: handleDeliveryJob would refuse either
//     before a single HTTP attempt (settling the row terminal again with
//     "webhook subscription was deleted"/"is inactive"), so scheduling a
//     job that can only re-fail is refused up front instead -- the
//     subscription must exist and be active for a redelivery to mean
//     anything, exactly as it must for the original fan-out
//     (matchingSubscriptions only ever matches active subscriptions).
//
// # A fresh attempt cycle on a fresh job
//
// The re-enqueued job is a new jobs.Job under a NEW idempotency key
// (redeliveryIdempotencyKey), never the original delivery job's key: jobs'
// idempotency is unconditional on StandaloneQueue -- a resolved key is held
// forever, dead-lettered outcome included -- so re-enqueueing under the
// original key would keep returning the original dead job's id and never
// run. The new key names one attempt cycle of the row (delivery id plus the
// row's current Attempts count), so a duplicate enqueue within one cycle --
// a double-click, a caller retry after a timeout -- dedupes onto the one
// job, while a later cycle, whose count has moved on, gets its own job and
// runs again: each manual redelivery is a deliberate operator action, and
// each must be able to happen, forever, however many times a delivery
// dead-letters. With no queue wired (nil -- the Module was built without
// WithWebhookQueue), this method fails with a plain error: unlike the
// event-driven fan-out's record-and-warn posture (enqueueDelivery), a
// caller that EXPLICITLY asked for a delivery must learn that none can
// run, never silently no-op.
//
// On a successful enqueue the row is flipped back to DeliveryStatusPending
// -- the state a delivery awaiting its next attempt holds -- with Attempts,
// LastError and the rest left as the failed cycle left them, to be
// overwritten by the new cycle's own attempts. The flip is a guarded,
// single-column write (WebhookDeliveryRepository.markPending) ordered
// AFTER the enqueue, for two reasons. Ordered after: an enqueue failure
// leaves the row exactly as it was (dead-lettered, still honestly
// describing its last outcome) and a caller retries. Guarded: the worker
// can pick the job up the moment Enqueue returns, and a flip that matched
// nothing means that job (or a concurrent redelivery) already moved the
// row past dead-letter -- the delivered outcome must never be overwritten
// by a stale pending, and there is nothing to restore anyway; the call
// still answers nil, since the job it enqueued is what settles the row
// from here on. If the flip's own WRITE fails, the enqueued job still
// settles the row on its first attempt, and a caller retrying this method
// converges on the existing job (same cycle, same idempotency key) rather
// than double-enqueueing.
func (s *Service) RedeliverWebhookDelivery(ctx context.Context, deliveryID string) error {
	delivery, err := s.deliveryRepo.FindByID(ctx, deliveryID)
	if err != nil {
		return translateDeliveryRepoErr(err)
	}
	if delivery.Status != DeliveryStatusDeadLetter {
		return ErrWebhookDeliveryNotDeadLetter
	}

	sub, err := s.webhookRepo.FindByID(ctx, delivery.SubscriptionID)
	if err != nil {
		// The subscription is mark-deleted (or belongs to another tenant,
		// which the delivery's own tenant scope already rules out -- this
		// lookup is for the row's own subscription id inside the same
		// tenant). Nothing to deliver to and nothing that will reappear:
		// the collapsed not-found is the honest answer, matching
		// handleDeliveryJob's own terminal settlement for this case.
		return translateWebhookRepoErr(err)
	}
	if !sub.Active {
		return ErrWebhookSubscriptionInactive
	}

	if s.queue == nil {
		return errors.New("integration: no queue wired (WithWebhookQueue)")
	}
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return ErrInternal.WithCause(err)
	}

	payload, err := json.Marshal(webhookDeliveryJobPayload{SubscriptionID: delivery.SubscriptionID, DeliveryID: delivery.ID})
	if err != nil {
		return fmt.Errorf("integration: encode webhook delivery job payload: %w", err)
	}
	if _, err := s.queue.Enqueue(ctx, jobs.Task{
		Type:           jobTypeWebhookDeliver,
		TenantID:       tenant,
		Payload:        payload,
		IdempotencyKey: redeliveryIdempotencyKey(delivery.ID, delivery.Attempts),
	}, jobs.WithMaxRetries(webhookMaxRetries)); err != nil {
		return err
	}

	if _, err := s.deliveryRepo.markPending(ctx, delivery.ID); err != nil {
		return ErrInternal.WithCause(err)
	}
	return nil
}

// redeliveryIdempotencyKey derives the jobs idempotency key one manual
// redelivery of a delivery row is enqueued under: the delivery id (the
// opaque business identity of the operation) plus the row's Attempts count
// at enqueue time, which is what makes the key name ONE attempt cycle
// rather than "some redelivery or other" -- see RedeliverWebhookDelivery's
// own doc comment for why a cycle-scoped key is the right shape here (a
// same-cycle duplicate enqueue merges; a later cycle, whose count has
// moved on, gets its own job forever). Attempts is read from the row the
// redelivery call already loaded, so the deterministic-replay property an
// idempotency key exists for holds within a cycle: a caller retrying the
// same enqueue reproduces the same key.
func redeliveryIdempotencyKey(deliveryID string, attempts int) string {
	return "integration.redeliver:" + deliveryID + ":" + strconv.Itoa(attempts)
}

// translateDeliveryRepoErr maps a dbkit.Repository[WebhookDelivery]
// not-found error onto this module's own ErrWebhookDeliveryNotFound,
// mirroring translateRepoErr's and translateWebhookRepoErr's identical
// shape -- matching by Code, never by identity (see translateRepoErr's own
// doc comment for why).
func translateDeliveryRepoErr(err error) error {
	if dbkit.IsRecordNotFound(err) {
		return ErrWebhookDeliveryNotFound
	}
	return ErrInternal.WithCause(err)
}
