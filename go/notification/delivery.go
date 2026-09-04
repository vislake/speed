package notification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"

	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/i18n"
)

// jobTypeDeliver is the Task.Type of every delivery job this module enqueues
// and handles. The jobs package requires the type to be stable for the
// lifetime of the Handler and unique within one Queue; the module registers
// exactly one handler (Module.Register's reg.Jobs.Handle call), and the type
// string is deliberately not the name of a notification type -- one handler
// delivers every declared type, deciding what to do per payload.
const jobTypeDeliver = "notification.deliver"

// Dispatch is the module's public delivery request: "deliver this
// notification type's message copy to this recipient, rendered in this
// locale, with these template parameters." It is also the job payload --
// DeliveryService.Dispatch marshals it into the queue and the Handler
// unmarshals it back out -- so its shape is part of the queue contract: a
// change to these fields is a change to every in-flight job of a rolling
// release, and the json tags below are therefore as load-bearing as the
// fields themselves.
//
// A Dispatch is async by construction: Dispatch() only validates and
// enqueues, and every decision that can change between enqueue and delivery
// -- the recipient's channel preferences, an external contact's consent, the
// addresses on file -- is re-checked at send time by the job, never frozen
// into the payload. The payload carries the minimum the job needs to render
// (type key, recipient, locale, parameters); everything that can go stale
// stays out.
type Dispatch struct {
	// TypeKey names the notification type whose copy is delivered. It must
	// be declared on the host's registry (the preference service resolves
	// it); an undeclared key is refused at delivery time with
	// ErrTypeNotFound.
	TypeKey string `json:"type_key"`

	// Recipient names who the message goes to: RecipientClassUser or
	// RecipientClassExternal, with the matching id field set.
	Recipient DispatchRecipient `json:"recipient"`

	// Locale is the language the message copy is rendered in -- the
	// recipient's negotiated locale, which the caller knows and the module
	// never guesses. It is REQUIRED for a user recipient (a delivery in a
	// wrong language is worse than a failed one, and the module's copy
	// rule forbids silent fallback), and ignored for an external contact,
	// whose copy renders in the platform default locale (contact.go's
	// renderContactCode documents the same deferral: a contact row carries
	// no locale).
	Locale string `json:"locale"`

	// Params supplies the interpolation values the type's templates
	// reference, keyed by the names the templates' own {{.name}}
	// placeholders spell out (render.go's renderContent). It is also part
	// of the delivery key derivation, so two dispatches that differ only
	// in parameters are two deliveries.
	Params map[string]any `json:"params"`
}

// DispatchRecipient is the recipient half of a Dispatch: which class of
// recipient, and which id within it. Exactly one of UserID and ContactID is
// set, depending on Class.
type DispatchRecipient struct {
	// Class is RecipientClassUser or RecipientClassExternal -- the closed
	// vocabulary of the send_records.recipient_class column, travelled
	// unmodified through the payload.
	Class string `json:"class"`

	// UserID names the recipient when Class is RecipientClassUser: a user
	// of the host platform, whose addresses the host's UserAddressResolver
	// resolves and whose preferences decide the channels.
	UserID string `json:"user_id,omitempty"`

	// ContactID names the recipient when Class is RecipientClassExternal:
	// a verified contact of the tenant (verified_contacts), whose address
	// and consent the module itself holds.
	ContactID string `json:"contact_id,omitempty"`
}

// validate refuses a Dispatch the delivery pipeline cannot honour. Each
// refusal carries the offending field in ErrDispatchInvalid's "field"
// parameter; Dispatch() and the job Handler both run it, so a malformed
// payload is refused at the API boundary and, if one still reaches the
// queue, dead-letters instead of half-delivering.
func (d Dispatch) validate() error {
	invalid := func(field string) error {
		return ErrDispatchInvalid.WithParam("field", field)
	}
	if d.TypeKey == "" {
		return invalid("type_key")
	}
	switch d.Recipient.Class {
	case RecipientClassUser:
		if d.Recipient.UserID == "" {
			return invalid("recipient.user_id")
		}
		if d.Locale == "" {
			return invalid("locale")
		}
	case RecipientClassExternal:
		if d.Recipient.ContactID == "" {
			return invalid("recipient.contact_id")
		}
	case "":
		return invalid("recipient.class")
	default:
		return invalid("recipient.class")
	}
	return nil
}

// ErrTransportPermanent is the sentinel a transport returns -- wrapped, per
// Go convention -- when a delivery failure is permanent: the address rejects
// the message (an SMTP 5xx, a provider "invalid number" response), and no
// retry will ever succeed. It is the job's signal to stop retrying a
// transport attempt, and, for an external contact, to mark the tenant's own
// contact bounced (MarkBounced) so future deliveries to it are refused
// before they reach any transport.
//
// The module's own transports never return it (the console mailer and SMS
// sender have no permanent failures); a host's real transports wrap it, and
// a test double returns it to pin the bounce path. The sentinel deliberately
// lives here rather than in errors.go because it is not an apperr: it is a
// control signal between the transport and the delivery job, matched with
// errors.Is, never surfaced to a caller.
var ErrTransportPermanent = errors.New("notification: permanent transport failure")

// UserAddresses is what a user delivery's address resolution returns: the
// outbound addresses the host has on file for one user. Either field may be
// empty; the delivery job skips the matching channel (recording a skipped
// send record) when the address the channel needs is absent.
type UserAddresses struct {
	// Email is the user's outbound email address, empty when none is on
	// file. The host holds it -- it is identity data, out of this module's
	// tables -- in whatever canonical form its own address store uses; the
	// module hands it to the mailer verbatim.
	Email string

	// Phone is the user's phone number in E.164 form, the canonical shape
	// dbkit.NormalizePhoneE164 produces -- the same shape every number in
	// verified_contacts holds. Empty when none is on file.
	Phone string
}

// UserAddressResolver is the structurally-typed seam through which a user
// delivery learns the recipient's addresses. The module cannot hold users
// (identity data belongs to the host's authn half, and this module never
// imports it -- the same no-import rule org's seams observe), so the host
// supplies a resolver -- authn's user-address store, or any layer over it --
// at wiring time through WithUserAddressResolver.
//
// The resolver is consulted at SEND time, never at enqueue time: an address
// added or removed between Dispatch and the job's run is honoured by the
// delivery, which is what makes a dispatch to a user with no address yet a
// retryable event rather than a permanently failed one.
type UserAddressResolver interface {
	// Resolve returns the outbound addresses on file for userID. A user
	// with no addresses returns an empty UserAddresses and nil -- absence
	// of addresses is not an error, it is the ordinary state that makes
	// the email and SMS channels skip. A store failure is an error, and
	// the delivery job retries it.
	Resolve(ctx context.Context, userID string) (UserAddresses, error)
}

// deliveryHost is the slice of the host's *pkgcore.Registry the delivery
// service reads at run time: the bus it announces inbox deliveries on, the
// mailer email goes out through, and the merged message catalog it renders
// from. Each is read at call time -- never captured at Register, when
// reg.Locales() is still nil -- so a host satisfies the interface
// structurally (the compile-time assertion below pins *pkgcore.Registry)
// and Register hands the real registry over in its third phase.
type deliveryHost interface {
	EventBus() pkgcore.EventBus
	Mailer() pkgcore.Mailer
	Locales() *i18n.Catalog
}

var _ deliveryHost = (*pkgcore.Registry)(nil)

// DeliveryService is the module's outbound-delivery pipeline: the decision
// layer that turns a Dispatch into per-channel sends, and the jobs.Handler
// that executes them on the queue.
//
// It is deliberately a service of its own rather than a method set on Module
// or on the preference or contact services: delivery is the first consumer
// that needs all three repositories (preferences, contacts, inbox) plus the
// send-records log at once, and its queue/Host/address seams arrive through
// Module's options and Register phases like every other seam in this module.
//
// # The pipeline
//
// DeliveryService.Dispatch validates a Dispatch, marshals it, and enqueues
// one notification.deliver job on the module's queue; the job's Handle
// (also this type) runs the send-time pipeline:
//
//   - a user recipient's channels come from PreferenceService
//     (ResolveForDelivery: the stored selection folded over the type's
//     declared defaults) and each channel delivers independently -- an
//     inbox write, an email, an SMS -- so one channel's failure never
//     starves the others;
//   - an external contact's deliverability is re-checked through
//     ContactService.EnsureDeliverable (the send-time consent recheck;
//     see AGENTS.md's "Every consent and address decision is re-checked
//     at send time"); the verification-code exception that created the
//     contact is long
//     past, so every delivery to it stands behind verified consent;
//   - every send attempt is recorded in send_records under a derived
//     delivery key (deriveDeliveryKey), and the record's succeeded state is
//     probed before any attempt -- a retried job finds its own earlier
//     success and stops, which is what makes the whole pipeline
//     at-most-once per (tenant, key) despite the queue's at-least-once
//     delivery;
//   - an inbox delivery additionally publishes EventInboxCreated after the
//     row is committed, announcing it to every replica's Hub.
//
// # Failure semantics
//
// Handle returns nil only when the attempt is terminal: the delivery
// succeeded, was skipped for a reason that will not change (no address on
// file, consent withdrawn, address bounced), or failed permanently (a
// template missing, a transport refusing the address). A non-nil return is
// the signal to retry, and every retried failure is first recorded in
// send_records as a failed attempt, so an operator sees each attempt's
// outcome even while the job is still converging.
type DeliveryService struct {
	// prefs resolves a user delivery's channels; contacts gates and
	// marks an external contact's deliverability; inbox is the in-app
	// destination; sendRecs the outbound-delivery log every attempt
	// settles into.
	prefs    *PreferenceService
	contacts *ContactService
	inbox    *Repository
	sendRecs *SendRecordRepository

	// queue is where Dispatch enqueues and the worker runs Handle. It is
	// filled from Module's WithDeliveryQueue option; a service without one
	// refuses Dispatch with ErrDeliveryQueueRequired.
	queue jobs.Queue

	// resolver supplies a user recipient's addresses (see
	// UserAddressResolver); sms and mailFrom are the module's outbound
	// transports for the SMS and email channels; host is the registry
	// slice attached during Register (see deliveryHost).
	resolver UserAddressResolver
	sms      SMSSender
	mailFrom string
	host     deliveryHost
}

// newDeliveryService returns a DeliveryService over the module's inbox
// repository and its two decision services. The inbox repository is
// constructed by Module -- once, at NewModule -- and the SAME instance
// reaches both the pipeline and the HTTP surface, so delivery writes and
// handler reads are one data path, not two wrappers over one connection.
// The queue, address resolver and host seams are filled in later -- the
// queue and resolver by Module's options, the host by Register -- exactly
// as ContactService's seams arrive; before then, Dispatch refuses on the
// missing queue and the job cannot be registered.
func newDeliveryService(inbox *Repository, prefs *PreferenceService, contacts *ContactService) *DeliveryService {
	return &DeliveryService{
		prefs:    prefs,
		contacts: contacts,
		inbox:    inbox,
		sendRecs: NewSendRecordRepository(inbox.db),
	}
}

// attachHost binds the host registry to the service. Module.Register calls
// it in its third phase, after the module's own declarations succeeded, so
// the catalog is read from the registry at call time -- never captured here,
// when reg.Locales() is still nil.
func (s *DeliveryService) attachHost(reg *pkgcore.Registry) {
	s.host = reg
}

// Dispatch validates d and enqueues one delivery job for it, returning the
// job's id. Delivery is asynchronous: nothing is sent, rendered or checked
// here beyond validation -- the queue worker's Handle runs the send-time
// pipeline, re-checking preferences, consent and addresses when it runs.
//
// The tenant comes from ctx (pkgcore.TenantFromContext); a dispatch without
// one is refused with pkgcore.ErrNoTenant, because the job and every record
// it writes belong to a tenant. A Dispatch whose payload cannot be marshaled
// -- a Params map holding a channel or function, say -- is refused with
// ErrDispatchInvalid naming the "params" field.
func (s *DeliveryService) Dispatch(ctx context.Context, d Dispatch) (jobs.JobID, error) {
	if err := d.validate(); err != nil {
		return "", err
	}
	if s.queue == nil {
		return "", ErrDeliveryQueueRequired
	}
	tenantID, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return "", pkgcore.ErrNoTenant
	}
	payload, err := json.Marshal(d)
	if err != nil {
		return "", ErrDispatchInvalid.WithParam("field", "params").WithCause(err)
	}
	return s.queue.Enqueue(ctx, jobs.Task{
		Type:     jobTypeDeliver,
		TenantID: tenantID,
		Payload:  payload,
	})
}

// Type implements jobs.Handler.
func (s *DeliveryService) Type() string { return jobTypeDeliver }

// Handle implements jobs.Handler: one attempt at one delivery job.
//
// The job's context already carries the job's tenant -- jobs rebuilds it
// from the job record before Handle runs (see jobs.Handler's doc comment) --
// so every repository call below resolves the tenant it writes under, and
// the worker never inherits an ambient tenant from its enqueuer. A payload
// that does not decode or validate is returned as an error -- the queue
// retries and then dead-letters it, the honest outcome for a payload that
// slipped past Dispatch's own validation.
func (s *DeliveryService) Handle(ctx context.Context, job *jobs.Job, _ jobs.ProgressFn) (jobs.Result, error) {
	var d Dispatch
	if err := json.Unmarshal(job.Payload, &d); err != nil {
		return jobs.Result{}, fmt.Errorf("notification: decode delivery job payload: %w", err)
	}
	if err := d.validate(); err != nil {
		return jobs.Result{}, err
	}
	if err := s.runDelivery(ctx, d); err != nil {
		return jobs.Result{}, err
	}
	return jobs.Result{}, nil
}

// runDelivery dispatches one validated Dispatch to its recipient class's
// delivery path. The tenant is read from ctx (jobs rebuilt it into the
// worker's context); its absence is refused with pkgcore.ErrNoTenant rather
// than guessed.
func (s *DeliveryService) runDelivery(ctx context.Context, d Dispatch) error {
	tenantID, ok := pkgcore.TenantFromContext(ctx)
	if !ok {
		return pkgcore.ErrNoTenant
	}
	switch d.Recipient.Class {
	case RecipientClassUser:
		return s.deliverToUser(ctx, string(tenantID), d)
	case RecipientClassExternal:
		return s.deliverToContact(ctx, string(tenantID), d)
	default:
		// Unreachable: Dispatch.validate already refused the class. Kept
		// because the worker can run a payload the validating caller
		// never saw.
		return ErrDispatchInvalid.WithParam("field", "recipient.class")
	}
}

// deliverToUser is the user-recipient delivery path: resolve the channels
// the user's preferences (folded over the type's defaults) select for this
// type, and deliver on each. Channels deliver independently, and their
// outcomes are joined: one channel's terminal failure neither starves the
// others nor is hidden by their success -- the job returns non-nil (and
// retries) when any channel asks for it.
func (s *DeliveryService) deliverToUser(ctx context.Context, tenantID string, d Dispatch) error {
	group, channels, err := s.prefs.ResolveForDelivery(ctx, d.Recipient.UserID, d.TypeKey)
	if err != nil {
		// An undeclared type has no channels and no copy to render;
		// nothing can be recorded without a channel, so the refusal
		// surfaces for the queue to dead-letter.
		return err
	}
	var firstErr error
	for _, channel := range channels {
		if err := s.deliverUserChannel(ctx, tenantID, d, group, channel); err != nil {
			firstErr = errors.Join(firstErr, err)
		}
	}
	return firstErr
}

// deliverUserChannel delivers one Dispatch over one resolved channel: the
// per-channel replay probe, then the channel's own delivery path.
//
// The probe is the at-most-once backstop: every attempt settles a send
// record under the same derived key, so an attempt that follows a succeeded
// record -- a retry after a crash that landed between transport and record,
// a duplicate enqueue, a concurrent replica racing the same dispatch -- has
// nothing left to do. A probe failure is returned without sending: the
// record's state is unknown, and sending on unknown state is exactly the
// double-delivery this probe exists to prevent.
func (s *DeliveryService) deliverUserChannel(ctx context.Context, tenantID string, d Dispatch, group, channel string) error {
	rec := s.sendRecordFor(tenantID, d, channel)
	key, err := deriveDeliveryKey(tenantID, d, channel)
	if err != nil {
		return err
	}
	rec.IdempotencyKey = key

	done, err := s.alreadyDelivered(ctx, tenantID, key)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	switch channel {
	case ChannelInApp:
		return s.deliverInbox(ctx, tenantID, d, group, rec)
	case ChannelEmail:
		return s.deliverUserEmail(ctx, tenantID, d, rec)
	case ChannelSMS:
		return s.deliverUserSMS(ctx, tenantID, d, rec)
	default:
		// Unreachable through any path that resolves channels: the
		// preference matrix validates every stored selection against the
		// types.go vocabulary. A corrupt stored row surfacing here is
		// recorded and stopped, not looped on.
		return s.failAndStop(ctx, tenantID, rec,
			fmt.Errorf("notification: deliver on unknown channel %q", channel))
	}
}

// deliverInbox is the in-app channel's delivery path: render the type's
// inbox copy (a title and a body) in the recipient's locale, write the
// in_app_messages row, and announce it.
//
// The row write is idempotent by construction: the row carries the derived
// delivery key in DedupeKey, under the migration's global unique index, and
// the write is preceded by a probe (FindByDedupeKey) and followed by one on
// a refused insert -- so a row a concurrent or earlier attempt wrote is
// found, not duplicated, whatever the error the insert reported. The
// announce then goes out once per job run that finds the row un-announced:
// a crash between the row commit and the announce leaves a record-less row
// whose retry announces it, and an announce the bus refused is recorded as
// a failed attempt whose retry re-announces the row the probe finds.
func (s *DeliveryService) deliverInbox(ctx context.Context, tenantID string, d Dispatch, group string, rec *SendRecord) error {
	key := rec.IdempotencyKey

	row, err := s.inbox.FindByDedupeKey(ctx, key)
	if err != nil {
		return err
	}
	if row == nil {
		row, err = s.buildInboxRow(ctx, d, group, key)
		if err != nil {
			// A render failure is terminal -- the template or catalog
			// will not heal on retry -- and is recorded as such.
			return s.failAndStop(ctx, tenantID, rec, err)
		}
		if err := s.inbox.Create(ctx, row); err != nil {
			// The unique index on dedupe_key refuses a duplicate insert,
			// but a refused insert and a store failure are
			// indistinguishable by error type, so the probe decides: a
			// row under the key means a concurrent attempt committed it
			// and it is the one to announce; no row means the create
			// genuinely failed and the whole delivery retries.
			winner, perr := s.inbox.FindByDedupeKey(ctx, key)
			if perr != nil {
				return perr
			}
			if winner == nil {
				return s.failAndRetry(ctx, tenantID, rec, err)
			}
			row = winner
		}
	}

	if err := s.announceInbox(ctx, tenantID, d, row); err != nil {
		// The row is durable; only its announcement failed. Record the
		// failed attempt and retry -- the retry's probe finds the row and
		// re-announces instead of re-writing.
		return s.failAndRetry(ctx, tenantID, rec, err)
	}
	rec.Status = SendRecordStatusSucceeded
	return s.settle(ctx, tenantID, rec)
}

// buildInboxRow renders one type's in-app copy into the row the delivery
// will write. The row's Params column carries the JSON of the template
// parameters that produced the copy, so a later re-render (a locale change,
// say) needs no re-parse of the source dispatch; a dispatch with no
// parameters stores the NULL column, never the JSON "null".
func (s *DeliveryService) buildInboxRow(_ context.Context, d Dispatch, group, key string) (*InboxMessage, error) {
	parts, err := renderContent(s.catalog(), d.Locale, d.TypeKey, ChannelInApp, d.Params)
	if err != nil {
		return nil, err
	}
	row := &InboxMessage{
		ID:              uuid.NewString(),
		RecipientUserID: d.Recipient.UserID,
		TypeKey:         d.TypeKey,
		Group:           group,
		Title:           parts["title"],
		Body:            parts["body"],
		DedupeKey:       &key,
	}
	if len(d.Params) > 0 {
		raw, err := json.Marshal(d.Params)
		if err != nil {
			// Dispatch marshalled the same parameters successfully, so a
			// failure here means the map changed between the two calls.
			return nil, fmt.Errorf("notification: marshal delivery params into the inbox row: %w", err)
		}
		row.Params = datatypes.JSON(raw)
	}
	return row, nil
}

// announceInbox publishes EventInboxCreated for one committed inbox row --
// the announcement every replica's Hub fans out to its connections. The row
// is already durable when the event goes out, which is what makes a lost
// announcement recoverable and a duplicate one harmless.
func (s *DeliveryService) announceInbox(ctx context.Context, tenantID string, d Dispatch, row *InboxMessage) error {
	if s.bus() == nil {
		return errors.New("notification: no event bus to announce the inbox delivery on")
	}
	return s.bus().Publish(ctx, pkgcore.Event{
		Type:     EventInboxCreated,
		TenantID: pkgcore.TenantID(tenantID),
		Payload: InboxCreatedPayload{
			MessageID:       row.ID,
			TenantID:        tenantID,
			RecipientUserID: row.RecipientUserID,
			TypeKey:         row.TypeKey,
		},
	})
}

// deliverUserEmail is the email channel's user delivery path: resolve the
// user's addresses at send time, render the type's email copy (a subject
// and a plain-text body) in the recipient's locale, and send through the
// host's mailer.
func (s *DeliveryService) deliverUserEmail(ctx context.Context, tenantID string, d Dispatch, rec *SendRecord) error {
	if s.resolver == nil {
		return s.failAndStop(ctx, tenantID, rec, ErrUserAddressResolverRequired)
	}
	addrs, err := s.resolver.Resolve(ctx, d.Recipient.UserID)
	if err != nil {
		// Address resolution failed -- a store hiccup in the host's
		// identity half. Record the failed attempt and retry.
		return s.failAndRetry(ctx, tenantID, rec,
			fmt.Errorf("notification: resolve addresses for user %s: %w", d.Recipient.UserID, err))
	}
	if addrs.Email == "" {
		// No email on file is a legitimate, possibly temporary state -- a
		// dispatch racing a profile setup -- and the queue must not retry
		// it. The skipped record keeps the outcome observable.
		return s.skipAndStop(ctx, tenantID, rec, skipReasonNoEmail)
	}

	parts, err := renderContent(s.catalog(), d.Locale, d.TypeKey, ChannelEmail, d.Params)
	if err != nil {
		return s.failAndStop(ctx, tenantID, rec, err)
	}

	start := time.Now()
	err = s.sendMail(ctx, parts, []string{addrs.Email})
	rec.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		if errors.Is(err, ErrTransportPermanent) {
			// A user's address is the host's data, not a verified_contacts
			// row, so there is no contact to mark bounced -- the refusal
			// is terminal, recorded, and the job stops.
			return s.failAndStop(ctx, tenantID, rec, err)
		}
		return s.failAndRetry(ctx, tenantID, rec, err)
	}
	rec.Status = SendRecordStatusSucceeded
	return s.settle(ctx, tenantID, rec)
}

// deliverUserSMS is the SMS channel's user delivery path: the twin of
// deliverUserEmail over the module's SMS sender, rendering the type's SMS
// copy (a single text).
func (s *DeliveryService) deliverUserSMS(ctx context.Context, tenantID string, d Dispatch, rec *SendRecord) error {
	if s.resolver == nil {
		return s.failAndStop(ctx, tenantID, rec, ErrUserAddressResolverRequired)
	}
	addrs, err := s.resolver.Resolve(ctx, d.Recipient.UserID)
	if err != nil {
		return s.failAndRetry(ctx, tenantID, rec,
			fmt.Errorf("notification: resolve addresses for user %s: %w", d.Recipient.UserID, err))
	}
	if addrs.Phone == "" {
		return s.skipAndStop(ctx, tenantID, rec, skipReasonNoPhone)
	}

	parts, err := renderContent(s.catalog(), d.Locale, d.TypeKey, ChannelSMS, d.Params)
	if err != nil {
		return s.failAndStop(ctx, tenantID, rec, err)
	}

	start := time.Now()
	err = s.sendSMS(ctx, parts, addrs.Phone)
	rec.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		if errors.Is(err, ErrTransportPermanent) {
			return s.failAndStop(ctx, tenantID, rec, err)
		}
		return s.failAndRetry(ctx, tenantID, rec, err)
	}
	rec.Status = SendRecordStatusSucceeded
	return s.settle(ctx, tenantID, rec)
}

// deliverToContact is the external-contact delivery path, standing behind
// the module's own consent ledger: ContactService.EnsureDeliverable is the
// send-time recheck that refuses a delivery whose consent lapsed between
// enqueue and delivery (AGENTS.md's "Every consent and address decision
// is re-checked at send time" adjudication -- the module never sends to
// an unverified address, the verification message itself being the only
// exception, and delivery is not it).
//
// The refusal mapping follows the ledger's statuses, split on whether a
// retry can change the answer: a pending contact (consent never proved) and
// a contact that no longer exists return the gate's refusal, and the
// queue's bounded retry-and-dead-letter horizon answers it -- a
// verification landing inside the horizon lets the job deliver itself, and
// a refusal the horizon outlives converges to an operator-visible
// dead-lettered job, never a silent success. Neither deferred refusal
// settles a send record: the gate refused before the contact's channel
// resolved, and a record without a channel could never be probed by a retry
// (see settleContactRefusal) -- the attempt carries no record precisely so
// the retry that follows a verification probes fresh and delivers. An
// unsubscribed or bounced contact is terminal -- no retry changes the
// answer -- and is recorded as a skipped send under the contact's own
// channel. A verified contact proceeds to its channel's transport, and a
// permanent transport refusal marks the tenant's own contact bounced
// (MarkBounced) before the attempt is recorded -- the delivery job's
// hard-failure leg; writing the platform blacklist is a later round's work
// (blacklist.go's doc comment records the boundary).
func (s *DeliveryService) deliverToContact(ctx context.Context, tenantID string, d Dispatch) error {
	contact, err := s.contacts.EnsureDeliverable(ctx, d.Recipient.ContactID)
	if err != nil {
		if perr, ok := apperr.As(err); ok {
			switch perr.Code {
			case ErrContactNotFound.Code, ErrContactNotVerified.Code:
				// Neither refusal is terminal: a pending contact may be
				// verified before the retry horizon ends, and a not-found
				// id is a dispatch an operator should see dead-lettered,
				// not a message lost to a silent success. The refusal
				// returns for the queue's bounded horizon (see the doc
				// comment), with nothing recorded.
				return err
			case ErrContactUnsubscribed.Code, ErrContactBounced.Code:
				return s.settleContactRefusal(ctx, tenantID, d, perr)
			}
		}
		// A store failure behind the gate is not a consent answer; the
		// job retries it.
		return err
	}

	if !isContactChannel(contact.Channel) {
		return fmt.Errorf("notification: contact %s is verified on channel %q", contact.ID, contact.Channel)
	}

	rec := s.sendRecordFor(tenantID, d, contact.Channel)
	rec.ContactID = contact.ID
	key, err := deriveDeliveryKey(tenantID, d, contact.Channel)
	if err != nil {
		return err
	}
	rec.IdempotencyKey = key

	done, err := s.alreadyDelivered(ctx, tenantID, key)
	if err != nil {
		return err
	}
	if done {
		return nil
	}

	switch contact.Channel {
	case ChannelEmail:
		return s.deliverContactEmail(ctx, tenantID, d, contact, rec)
	case ChannelSMS:
		return s.deliverContactSMS(ctx, tenantID, d, contact, rec)
	default:
		return s.failAndStop(ctx, tenantID, rec,
			fmt.Errorf("notification: deliver to contact %s on channel %q", contact.ID, contact.Channel))
	}
}

// settleContactRefusal records one terminal consent refusal -- a contact
// that unsubscribed or bounced -- as a skipped send. The channel comes from
// the refusal error's own "channel" parameter (EnsureDeliverable attaches
// the contact's channel to these two refusals; the other refusals carry no
// channel and are not recorded, because a send record without a channel
// could never be probed by a retry).
func (s *DeliveryService) settleContactRefusal(ctx context.Context, tenantID string, d Dispatch, perr *apperr.Error) error {
	channel, _ := perr.Params["channel"].(string)
	if channel == "" {
		// Defensive: every unsubscribed or bounced contact has a channel.
		// Without one there is no record to write and no retry to
		// converge, so the refusal is simply absorbed.
		return nil
	}
	rec := s.sendRecordFor(tenantID, d, channel)
	key, err := deriveDeliveryKey(tenantID, d, channel)
	if err != nil {
		return err
	}
	rec.IdempotencyKey = key

	reason := skipReasonUnsubscribed
	if perr.Code == ErrContactBounced.Code {
		reason = skipReasonBounced
	}
	return s.skipAndStop(ctx, tenantID, rec, reason)
}

// deliverContactEmail is the email channel's contact delivery path: render
// the type's copy in the platform default locale (a contact row carries no
// locale -- contact.go's renderContactCode documents the same deferral) and
// send to the contact's own address.
func (s *DeliveryService) deliverContactEmail(ctx context.Context, tenantID string, d Dispatch, contact *VerifiedContact, rec *SendRecord) error {
	parts, err := renderContent(s.catalog(), platformDefaultLocale, d.TypeKey, ChannelEmail, d.Params)
	if err != nil {
		return s.failAndStop(ctx, tenantID, rec, err)
	}

	start := time.Now()
	err = s.sendMail(ctx, parts, []string{contact.Address})
	rec.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		if errors.Is(err, ErrTransportPermanent) {
			// The address rejects mail. Mark the tenant's own contact
			// bounced -- its future deliveries are refused by the ledger
			// before any transport -- record the failed attempt, and stop.
			bounceErr := s.contacts.MarkBounced(ctx, contact.ID)
			stopErr := s.failAndStop(ctx, tenantID, rec, err)
			if bounceErr != nil {
				// The bounce did not land; the record did. Retrying
				// re-runs the whole path and converges the bounce.
				return errors.Join(stopErr, bounceErr)
			}
			return stopErr
		}
		return s.failAndRetry(ctx, tenantID, rec, err)
	}
	rec.Status = SendRecordStatusSucceeded
	return s.settle(ctx, tenantID, rec)
}

// deliverContactSMS is the SMS channel's contact delivery path, the twin of
// deliverContactEmail over the module's SMS sender.
func (s *DeliveryService) deliverContactSMS(ctx context.Context, tenantID string, d Dispatch, contact *VerifiedContact, rec *SendRecord) error {
	parts, err := renderContent(s.catalog(), platformDefaultLocale, d.TypeKey, ChannelSMS, d.Params)
	if err != nil {
		return s.failAndStop(ctx, tenantID, rec, err)
	}

	start := time.Now()
	err = s.sendSMS(ctx, parts, contact.Address)
	rec.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		if errors.Is(err, ErrTransportPermanent) {
			bounceErr := s.contacts.MarkBounced(ctx, contact.ID)
			stopErr := s.failAndStop(ctx, tenantID, rec, err)
			if bounceErr != nil {
				return errors.Join(stopErr, bounceErr)
			}
			return stopErr
		}
		return s.failAndRetry(ctx, tenantID, rec, err)
	}
	rec.Status = SendRecordStatusSucceeded
	return s.settle(ctx, tenantID, rec)
}

// sendMail sends one rendered email through the host's mailer. The rendered
// parts come from renderContent, so parts["subject"] and parts["body_text"]
// are always present when render succeeded. The From address is the
// module's own, fixed at wiring time (WithMailFrom), never a recipient's.
func (s *DeliveryService) sendMail(ctx context.Context, parts map[string]string, to []string) error {
	if s.host == nil || s.host.Mailer() == nil {
		return errors.New("notification: delivery has no mailer")
	}
	return s.host.Mailer().Send(ctx, pkgcore.Mail{
		From:    s.mailFrom,
		To:      to,
		Subject: parts["subject"],
		Text:    parts["body_text"],
	})
}

// sendSMS sends one rendered text message through the module's SMS sender.
func (s *DeliveryService) sendSMS(ctx context.Context, parts map[string]string, to string) error {
	if s.sms == nil {
		return errors.New("notification: delivery has no SMS sender")
	}
	return s.sms.Send(ctx, SMS{To: to, Text: parts["text"]})
}

// The short reasons a skipped send record carries in its Error column. A
// skip is a deliberate non-send -- no address on file, consent withdrawn,
// the address bounced -- and the reason is the operator's whole answer on
// why; the empty-string sentinel covers the records that never skipped.
const (
	skipReasonNoEmail      = "no email address on file"
	skipReasonNoPhone      = "no phone number on file"
	skipReasonUnsubscribed = "contact unsubscribed"
	skipReasonBounced      = "contact bounced"
)

// sendRecordErrorBudget is the longest message the Error column of a send
// record can hold -- the column's own 4000-char width (send_record.go).
// Truncation happens here, at the write site, so no transport message and no
// skip reason can overflow the schema.
const sendRecordErrorBudget = 4000

// sendRecordFor returns the send record a delivery attempt over channel will
// settle, carrying every field known before the attempt runs: the tenant,
// the type, the channel and the recipient class and id. The record's ID is
// filled at settle time (the first write for its key invents one; every
// later write adopts the existing row's), and its IdempotencyKey is set by
// the caller from deriveDeliveryKey -- the pair that makes the record
// probeable.
func (s *DeliveryService) sendRecordFor(tenantID string, d Dispatch, channel string) *SendRecord {
	rec := &SendRecord{
		TenantID:       tenantID,
		TypeKey:        d.TypeKey,
		Channel:        channel,
		RecipientClass: d.Recipient.Class,
	}
	if d.Recipient.Class == RecipientClassUser {
		rec.RecipientUserID = d.Recipient.UserID
	} else {
		rec.ContactID = d.Recipient.ContactID
	}
	return rec
}

// deriveDeliveryKey derives the delivery key one (tenant, recipient,
// type, channel, parameters) send is recorded under: the SHA-256 of the
// canonical JSON of the seed below, hex-encoded.
//
// The key is what makes the whole pipeline replay-safe. The delivery job
// recomputes it on every attempt and probes send_records with it, so a
// retried job finds its own earlier success; the inbox row carries it in
// DedupeKey under a global unique index, so a duplicate row write is
// refused; and the (tenant_id, idempotency_key) pair of send_records is
// unique, so two concurrent attempts of one delivery converge on one record.
// Canonicality comes from encoding/json's sorted map keys: the same
// dispatch derives the same key on every attempt and on every replica.
//
// The parameters participate in the derivation because two dispatches that
// differ only in what the copy says are two different deliveries -- a
// reminder for the same appointment and a cancellation of it must not
// collapse into one key. The recipient's id and the channel do likewise:
// one dispatch fans out to one key per channel, so each channel's delivery
// is independently replay-safe.
func deriveDeliveryKey(tenantID string, d Dispatch, channel string) (string, error) {
	seed := deliveryKeySeed{
		TenantID:       tenantID,
		TypeKey:        d.TypeKey,
		RecipientClass: d.Recipient.Class,
		UserID:         d.Recipient.UserID,
		ContactID:      d.Recipient.ContactID,
		Channel:        channel,
		Params:         d.Params,
	}
	raw, err := json.Marshal(seed)
	if err != nil {
		return "", fmt.Errorf("notification: derive delivery key: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// deliveryKeySeed is the canonical shape deriveDeliveryKey hashes. It is
// deliberately its own struct rather than the Dispatch itself: the key must
// name one CHANNEL of a delivery (Dispatch carries all channels at once),
// and the seed's field set is the key's public contract -- a field added
// here changes every derived key, which is safe only across a coordinated
// release (keys are derived, never stored by callers, so nothing stale
// lingers).
type deliveryKeySeed struct {
	TenantID       string         `json:"tenant_id"`
	TypeKey        string         `json:"type_key"`
	RecipientClass string         `json:"recipient_class"`
	UserID         string         `json:"user_id,omitempty"`
	ContactID      string         `json:"contact_id,omitempty"`
	Channel        string         `json:"channel"`
	Params         map[string]any `json:"params"`
}

// alreadyDelivered reports whether a send record under (tenant, key) already
// records a succeeded send -- the replay answer that lets a retried delivery
// converge without a second transport call. Only the succeeded status stops
// a retry: a failed record is a previous attempt's outcome, and the queue's
// retry of the job IS the response to it, so skipping on it would make every
// retry a no-op.
func (s *DeliveryService) alreadyDelivered(ctx context.Context, tenantID, key string) (bool, error) {
	existing, err := s.sendRecs.ByTenantAndKey(ctx, tenantID, key)
	if err != nil {
		return false, err
	}
	return existing != nil && existing.Status == SendRecordStatusSucceeded, nil
}

// settle persists one attempt's outcome as its send record -- the single
// write every delivery path funnels through. It adopts the id of the record
// already under the attempt's key (a retry overwrites its earlier attempts'
// row in place, so one delivery keeps one record for life), invents one for
// the first write, truncates the recorded message to the column's budget,
// and returns nil when the record landed. A record that fails to land is
// returned as an error: the attempt's outcome must be visible even when the
// record write itself failed, which is what makes a lost succeeded record a
// retried delivery rather than a silent gap in the log.
func (s *DeliveryService) settle(ctx context.Context, tenantID string, rec *SendRecord) error {
	if len(rec.Error) > sendRecordErrorBudget {
		rec.Error = rec.Error[:sendRecordErrorBudget]
	}

	if rec.ID == "" {
		existing, err := s.sendRecs.ByTenantAndKey(ctx, tenantID, rec.IdempotencyKey)
		if err != nil {
			return err
		}
		if existing != nil {
			rec.ID = existing.ID
		} else {
			rec.ID = uuid.NewString()
		}
	}

	if err := s.sendRecs.Save(ctx, rec); err != nil {
		// The Save raced another writer that committed the same
		// (tenant, key): adopt the winner's id and save once more. A
		// second failure is returned for the job to retry.
		existing, probeErr := s.sendRecs.ByTenantAndKey(ctx, tenantID, rec.IdempotencyKey)
		if probeErr != nil {
			return errors.Join(err, probeErr)
		}
		if existing == nil {
			return err
		}
		rec.ID = existing.ID
		return s.sendRecs.Save(ctx, rec)
	}
	return nil
}

// failAndRetry records the attempt as a failed send record carrying cause,
// then returns cause (joined with a record-write failure, should one land):
// the job's retry is the response to a failure that may resolve, and the
// record keeps every attempt observable while the job converges.
func (s *DeliveryService) failAndRetry(ctx context.Context, tenantID string, rec *SendRecord, cause error) error {
	rec.Status = SendRecordStatusFailed
	rec.Error = cause.Error()
	if err := s.settle(ctx, tenantID, rec); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

// failAndStop records the attempt as a failed send record carrying cause and
// returns nil: the failure is terminal -- the template is missing, the
// transport refuses the address, the wiring is broken -- and retrying would
// repeat it, not resolve it. Only a record-write failure surfaces, because a
// terminal failure whose record did not land must still be retried until it
// does.
func (s *DeliveryService) failAndStop(ctx context.Context, tenantID string, rec *SendRecord, cause error) error {
	rec.Status = SendRecordStatusFailed
	rec.Error = cause.Error()
	return s.settle(ctx, tenantID, rec)
}

// skipAndStop records the attempt as a skipped send record carrying a short
// reason (see the skipReason* constants) and returns nil: a skip is a
// deliberate non-send on a state that will not change by retrying.
func (s *DeliveryService) skipAndStop(ctx context.Context, tenantID string, rec *SendRecord, reason string) error {
	rec.Status = SendRecordStatusSkipped
	rec.Error = reason
	return s.settle(ctx, tenantID, rec)
}

// isContactChannel reports whether channel is one of the two transport
// channels a verified contact can be reached on. In-app is not among them:
// the in-app channel belongs to users, and the consent ledger's own
// vocabulary refuses a contact on it (ErrContactInvalidChannel).
func isContactChannel(channel string) bool {
	return channel == ChannelEmail || channel == ChannelSMS
}

// The host-seam accessors below read the registry slice attached during
// Register, with nil defenses for a service exercised before Register ran:
// a nil host or a nil seam resolves to the zero answer of the accessor
// (nil catalog, nil bus), which the delivery paths above turn into recorded
// terminal failures rather than panics.

// catalog returns the host's merged message catalog, or nil before Register
// attached one.
func (s *DeliveryService) catalog() *i18n.Catalog {
	if s.host == nil {
		return nil
	}
	return s.host.Locales()
}

// bus returns the host's event bus, or nil before Register attached one.
func (s *DeliveryService) bus() pkgcore.EventBus {
	if s.host == nil {
		return nil
	}
	return s.host.EventBus()
}

// compile-time checks: *DeliveryService is a jobs.Handler, so Module.Register
// can hand it to the registry's job registrar with the guarantee intact.
var _ jobs.Handler = (*DeliveryService)(nil)
