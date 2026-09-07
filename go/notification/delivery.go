package notification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
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

// InstrumentationName identifies this package's own tracer/meter, mirroring
// go/jobs/standalone_queue.go's identical use of its own package path for the
// same purpose (see that file's own doc comment on InstrumentationName).
const InstrumentationName = "github.com/vislake/speed/go/notification"

// Metric instrument names registerDeliveryMetrics wires under
// InstrumentationName -- the per-channel delivery success rate, latency and
// bounce rate row
// docs/internal/09-observability.md's must-instrument table requires for the
// notification domain. Bounce rate is derivable from
// deliveryCountMetricName's own status attribute: a transport's permanent
// failure settles as SendRecordStatusFailed through failAndStop (see
// ContactService's bounce marking), so no separate bounce instrument exists.
const (
	deliveryCountMetricName    = "notification.delivery.count"
	deliveryDurationMetricName = "notification.delivery.duration"
)

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
	// placeholders spell out (render.go's renderContent). Params is also
	// part of the delivery key derivation, so two dispatches that differ
	// only in parameters are two deliveries.
	//
	// Params is the RECIPIENT-VISIBLE channel: a parameter the delivered
	// copy renders persists into the recipient's inbox row -- the in-app
	// delivery stores the parameters the row's own copy was rendered from,
	// which the inbox API serves back (normalized to an empty object,
	// never absent) -- while a parameter no delivered copy renders travels
	// nowhere at all. Two boundaries govern what may ride here, each
	// enforced by delivery itself rather than left to a writer's memory:
	//
	//   - the type's DECLARATION (pkgcore.NotificationType.
	//     RecipientVisibleParams) decides which parameters may reach the
	//     recipient at all. DeliveryService.Dispatch refuses a parameter
	//     outside it with ErrDispatchParamsNotAllowed, and the delivery
	//     path narrows a payload that nevertheless carries one (a job
	//     enqueued before the declaration restricted its params) down to
	//     the declared list before anything renders or persists
	//     (recipientVisibleOnly below). Delivery-internal context -- an
	//     operator's free-text justification, an actor's user id -- never
	//     belongs here.
	//   - the type's own COPY decides which parameters it actually uses.
	//     A parameter no template of the type references would render
	//     nowhere and yet still ride verbatim through the inbox row, so
	//     Dispatch refuses one with ErrDispatchParamsUnreferenced, and the
	//     delivery path drops it from a payload that nevertheless carries
	//     one (a job enqueued before this rule) before anything renders,
	//     derives into the delivery key or persists (copyParamsForChannel
	//     below). A caller that wants to distinguish two otherwise
	//     identical deliveries names a fresh OccurrenceID -- the
	//     first-class per-occurrence marker, which never renders and never
	//     persists -- never a copy-inert parameter.
	Params map[string]any `json:"params"`

	// OccurrenceID names the delivery OCCURRENCE this Dispatch is -- the
	// deliberate-resend marker. It is optional, never renders, and never
	// appears in an inbox row's params: it exists so a caller can
	// intentionally deliver the same type, recipient and parameters more
	// than once -- a reminder re-sent because the patient asked, a demo
	// re-triggered -- by dispatching the new occurrence under a fresh id.
	//
	// The derived delivery key (deriveDeliveryKey) hashes it, so two
	// dispatches that differ only in OccurrenceID are two deliveries: each
	// settles its own send record and, on the in-app channel, its own
	// inbox row, and neither's replay probe swallows the other. Two
	// dispatches that share an occurrence id -- or that both leave it
	// empty -- are ONE delivery: the queue's retry of a job re-runs the
	// same payload, and that retry must keep converging on the first
	// attempt's record, which is exactly why the marker is a first-class
	// field rather than another template parameter to stuff into Params
	// (the reference app's smilesim glue carries its per-occurrence job id
	// in Params today; a parameter renders into the copy and round-trips
	// through the inbox row's API shape, neither of which a delivery
	// marker should do). Empty is the ordinary value: most notifications
	// occur once per (type, recipient, parameters).
	OccurrenceID string `json:"occurrence_id,omitempty"`
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
// delivery. A dispatch to a user with no address on file is not a retryable
// event -- the channels skip (see Resolve) and the attempt settles into
// send_records as a deliberate non-send whose skip reason names the gap, so
// nothing retries an address that is simply not there. What the send-time
// consultation buys instead is freshness: an address that arrives after the
// job ran is picked up by the NEXT dispatch, and one removed before the job
// runs is never sent to. Only a resolver failure is retried (see Resolve).
type UserAddressResolver interface {
	// Resolve returns the outbound addresses on file for userID. A user
	// with no addresses returns an empty UserAddresses and nil -- absence
	// of addresses is not an error, it is the ordinary state that makes
	// the email and SMS channels skip. A store failure is an error, and
	// the delivery job retries it.
	//
	// The addresses returned must be the host's own VERIFIED addresses
	// for that user: this module performs no consent or verification
	// check on the user path, so the resolver is the entire "may we send
	// to this address" gate for user deliveries. That is the deliberate
	// asymmetry with the module's VerifiedContact path, whose full
	// consent ledger (double opt-in, terminal unsubscribed and bounced
	// states, blind-indexed addresses) lives in this module's own tables
	// because an external contact has no host-side identity store to
	// hold verification -- user addresses are identity data, owned and
	// verified by the host's authn half. A resolver returning an address
	// the host never verified for that user bypasses the
	// never-send-to-an-unverified-address rule on the user side, and
	// nothing in this module can detect it; the obligation is written
	// into the contract because the contract is the only place this
	// module can hold it.
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
//     success and stops, which makes replay convergence best-effort
//     at-most-once per (tenant, key) despite the queue's at-least-once
//     delivery. Best-effort, not absolute: a crash between the transport's
//     accept and the record's settle, or two attempts probing before
//     either settles, can still double-send (deliverUserChannel's doc
//     below spells the windows out);
//   - replay convergence never rewrites history: settle's
//     never-downgrade-succeeded guard (its own doc below) refuses to
//     overwrite a record that already says succeeded with any later
//     skipped or failed outcome under the same key, whatever path --
//     refusal, preference, error -- settled it;
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

	// deliveryCount and deliveryDuration back the
	// "notification.delivery.count"/"notification.delivery.duration"
	// instruments registerDeliveryMetrics wires from newDeliveryService.
	// settle (the single write funnel every delivery path -- success,
	// failure and skip alike -- runs through) records both, guarded against
	// their nil zero value the same way jobs' own
	// recordJobMetrics/recordDeadLetter guard registerJobMetrics's fields --
	// a metrics-registration failure must never turn into a nil-pointer
	// panic on the delivery path itself.
	deliveryCount    metric.Int64Counter
	deliveryDuration metric.Float64Histogram
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
	count, duration := registerDeliveryMetrics()
	return &DeliveryService{
		prefs:            prefs,
		contacts:         contacts,
		inbox:            inbox,
		sendRecs:         NewSendRecordRepository(inbox.db),
		deliveryCount:    count,
		deliveryDuration: duration,
	}
}

// registerDeliveryMetrics wires the "notification.delivery.count" Counter
// and "notification.delivery.duration" Histogram this file's own
// deliveryCountMetricName/deliveryDurationMetricName doc comment names.
// Mirrors go/observability/middleware.go's Middleware, which registers its
// two HTTP instruments once at construction and ignores the (in practice
// unreachable, since the global otel API returns a working no-op instrument
// alongside any error) registration error the same way -- unlike
// go/jobs/standalone_queue.go's registerJobMetrics, which has a live ctx/
// logger available at its own call site (Start) to warn on failure and
// deliberately does not have one here (newDeliveryService is a plain
// constructor, called once per Module before any request context exists).
func registerDeliveryMetrics() (metric.Int64Counter, metric.Float64Histogram) {
	meter := otel.Meter(InstrumentationName)
	count, _ := meter.Int64Counter(
		deliveryCountMetricName,
		metric.WithDescription("Number of notification delivery attempts settled, by notification type, channel and resulting status (succeeded, failed or skipped). Failure rate and bounce rate are both derivable from this by status."),
		metric.WithUnit("{delivery}"),
	)
	duration, _ := meter.Float64Histogram(
		deliveryDurationMetricName,
		metric.WithDescription("Duration of one delivery attempt's transport call, in seconds, by notification type, channel and resulting status. Zero for a channel with no transport call (in-app)."),
		metric.WithUnit("s"),
	)
	return count, duration
}

// attachHost binds the host registry to the service. Module.Register calls
// it in its third phase, after the module's own declarations succeeded, so
// the catalog is read from the registry at call time -- never captured here,
// when reg.Locales() is still nil.
func (s *DeliveryService) attachHost(reg *pkgcore.Registry) {
	s.host = reg
}

// SendRecords returns the module's SendRecordRepository -- the same
// instance every delivery attempt settles into (newDeliveryService's own
// doc comment: one data path, not a second wrapper over one connection).
// This is what a caller outside this package (go/admin's D10,
// docs/internal/23-admin.md) reaches to search send records by tenant,
// time range, channel and status through SendRecordRepository.ListByFilter,
// rather than this package growing its own HTTP surface for it.
func (s *DeliveryService) SendRecords() *SendRecordRepository { return s.sendRecs }

// Dispatch validates d and enqueues one delivery job for it, returning the
// job's id. Delivery is asynchronous: nothing is sent, rendered or checked
// here beyond validation -- the queue worker's Handle runs the send-time
// pipeline, re-checking preferences, consent and addresses when it runs.
//
// The tenant comes from ctx (pkgcore.TenantFromContext); a dispatch without
// one is refused with pkgcore.ErrNoTenant, because the job and every record
// it writes belong to a tenant. A Dispatch whose payload cannot be marshaled
// -- a Params map holding a channel or function, say -- is refused with
// ErrDispatchInvalid naming the "params" field. Two further refusals guard
// the Params channel itself, in order: a parameter the named type's
// declaration does not mark recipient-visible is refused with
// ErrDispatchParamsNotAllowed (a type with static copy declares an empty
// list, so delivery-internal context -- an operator's justification, an
// actor's user id -- can never ride Params into the recipient's row or
// API), and a parameter no copy template of the type references is refused
// with ErrDispatchParamsUnreferenced (such a parameter would render
// nowhere yet still round-trip through the inbox row; distinguishing two
// otherwise identical deliveries is OccurrenceID's job, never a
// copy-inert parameter's). Both refusals answer before anything is
// enqueued, while the caller can still act on them.
func (s *DeliveryService) Dispatch(ctx context.Context, d Dispatch) (jobs.JobID, error) {
	if err := d.validate(); err != nil {
		return "", err
	}
	if err := s.checkParamsRecipientVisible(d); err != nil {
		return "", err
	}
	if err := s.checkParamsReferenced(d); err != nil {
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

// recipientVisibleParams returns the parameter names the type named typeKey
// declares recipient-visible, and whether the type declares any restriction
// at all. An undeclared type answers unrestricted: its key is resolved --
// and refused with ErrTypeNotFound when absent -- at delivery time, exactly
// as before. A nil RecipientVisibleParams, the pre-annotation legacy value
// (pkgcore.NotificationType's own field doc), also answers unrestricted, so
// no pre-existing type changes behaviour: the restriction engages only when
// a type's declaration states its recipient-visible list explicitly, the
// empty list included.
func (s *DeliveryService) recipientVisibleParams(typeKey string) ([]string, bool) {
	typ, err := s.prefs.lookupType(typeKey)
	if err != nil {
		return nil, false
	}
	if typ.RecipientVisibleParams == nil {
		return nil, false
	}
	return typ.RecipientVisibleParams, true
}

// paramsOutside returns the sorted names of every key of params that is not
// in allowed -- the offending set a refusal names. Sorting keeps the set
// deterministic across replicas, so the same refusal carries the same
// "params" answer everywhere.
func paramsOutside(params map[string]any, allowed []string) []string {
	set := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		set[name] = struct{}{}
	}
	var offending []string
	for name := range params {
		if _, ok := set[name]; !ok {
			offending = append(offending, name)
		}
	}
	slices.Sort(offending)
	return offending
}

// checkParamsRecipientVisible refuses a Dispatch whose Params carry a
// parameter name the type's declaration does not mark recipient-visible
// (ErrDispatchParamsNotAllowed, naming the type and the offending keys). A
// type with static copy declares an empty list, so a dispatch carrying
// anything at all for it is refused here, at the enqueue boundary, where
// the caller can still do something about it.
func (s *DeliveryService) checkParamsRecipientVisible(d Dispatch) error {
	allowed, restricted := s.recipientVisibleParams(d.TypeKey)
	if !restricted {
		return nil
	}
	if offending := paramsOutside(d.Params, allowed); len(offending) > 0 {
		return ErrDispatchParamsNotAllowed.
			WithParam("type_key", d.TypeKey).
			WithParam("params", strings.Join(offending, ","))
	}
	return nil
}

// checkParamsReferenced refuses a Dispatch whose Params carry a parameter
// name no copy template of the named type references in the locale the
// copy will render in (ErrDispatchParamsUnreferenced, naming the type and
// the offending keys). Such a parameter renders into no copy and yet would
// still ride verbatim into the inbox row and API -- the exact route by
// which a value nobody's copy uses reaches the recipient -- and the only
// legitimate use a copy-inert parameter ever had, distinguishing two
// otherwise identical deliveries, is OccurrenceID's first-class job. It is
// the structural half of Dispatch.Params' own doc comment, and it applies
// to every type whether or not the type's declaration restricts its
// recipient-visible list: declaration governs exposure, copy governs use,
// and a parameter must satisfy both.
//
// The copy is probed through the merged catalog at the enqueue boundary --
// rendering is a pure function, so probing costs nothing a dispatch does
// not already pay at delivery time -- and only a channel whose copy fully
// renders counts toward the referenced union: a channel with a missing or
// unrenderable template contributes nothing, and a dispatch whose type has
// no renderable copy at all is not judged here (its delivery would fail
// with its own render refusal). A nil catalog (a service exercised before
// Register attached the host registry) skips the gate entirely: nothing
// could render from it either. A type the registry does not declare is
// skipped too: such a dispatch can never deliver (the delivery path's own
// undeclared-type refusal -- a recorded terminal stop on the contact
// path -- is the authority there), so copy governance for it would only
// pre-empt the refusal the module already answers with.
func (s *DeliveryService) checkParamsReferenced(d Dispatch) error {
	if len(d.Params) == 0 {
		return nil
	}
	if _, err := s.prefs.lookupType(d.TypeKey); err != nil {
		return nil
	}
	catalog := s.catalog()
	if catalog == nil {
		return nil
	}
	locale := deliveryLocale(d)
	referenced := make(map[string]struct{})
	verified := false
	for channel := range channelRenderParts {
		kept, ok := copyParamsForChannel(catalog, locale, d.TypeKey, channel, d.Params)
		if !ok {
			continue
		}
		verified = true
		for name := range kept {
			referenced[name] = struct{}{}
		}
	}
	if !verified {
		return nil
	}
	var offending []string
	for name := range d.Params {
		if _, ok := referenced[name]; !ok {
			offending = append(offending, name)
		}
	}
	if len(offending) == 0 {
		return nil
	}
	slices.Sort(offending)
	return ErrDispatchParamsUnreferenced.
		WithParam("type_key", d.TypeKey).
		WithParam("params", strings.Join(offending, ","))
}

// recipientVisibleOnly narrows d to the parameters its type's declaration
// marks recipient-visible, returning d unchanged when the type declares no
// restriction or the payload already carries none outside it. runDelivery
// applies it to every payload that reaches the delivery path -- including
// jobs enqueued before the type's declaration restricted its params, which
// Dispatch's own refusal never saw: such a payload still delivers, but
// nothing it carries beyond the declaration renders into copy, derives into
// the delivery key, or persists into the inbox row. The narrowing is a pure
// function of the declaration, so every replica and every retry narrows the
// same payload to the same result.
func (s *DeliveryService) recipientVisibleOnly(d Dispatch) Dispatch {
	allowed, restricted := s.recipientVisibleParams(d.TypeKey)
	if !restricted || len(d.Params) == 0 {
		return d
	}
	if len(paramsOutside(d.Params, allowed)) == 0 {
		return d
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	subset := make(map[string]any, len(allowed))
	for name, value := range d.Params {
		if _, ok := allowedSet[name]; ok {
			subset[name] = value
		}
	}
	if len(subset) == 0 {
		subset = nil
	}
	d.Params = subset
	return d
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
	// Narrow the payload to the type's recipient-visible parameters before
	// any render, key derivation or row write (recipientVisibleOnly's own
	// doc comment).
	d = s.recipientVisibleOnly(d)
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
// The probe is the replay-convergence backstop: every attempt settles a
// send record under the same derived key, so an attempt that follows a
// succeeded record -- a retry arriving after the previous attempt's settle,
// a duplicate enqueue, a concurrent replica -- stops before the transport.
// It is best-effort, not a transport-side dedupe, and two windows can
// double-send despite it: a crash between the transport's accept and the
// record's settle leaves no succeeded record for the retry to find, and two
// attempts probing before either settles both pass it (the UNIQUE
// (tenant_id, idempotency_key) index and settle's race handling converge
// the RECORDS of racing attempts onto one row; they cannot unsend a second
// transport call). A probe failure is returned without sending: the
// record's state is unknown, and sending on unknown state is exactly the
// double-delivery this probe exists to prevent.
func (s *DeliveryService) deliverUserChannel(ctx context.Context, tenantID string, d Dispatch, group, channel string) error {
	rec := s.sendRecordFor(tenantID, d, channel)

	// Narrow the payload to the parameters THIS channel's own copy
	// templates actually reference before the delivery key derives and
	// anything renders or persists (copyParamsForChannel's doc comment):
	// the in-app row must persist exactly the parameters its own copy was
	// rendered from, and a parameter no copy on this channel renders must
	// not distinguish the delivery either. The narrowing is a pure
	// function of the copy, and it never errors -- a copy that cannot
	// render keeps the payload untouched, and the channel's own render
	// refusal below stays the honest referee.
	if len(d.Params) > 0 {
		if kept, ok := copyParamsForChannel(s.catalog(), d.Locale, d.TypeKey, channel, d.Params); ok {
			if len(kept) == 0 {
				d.Params = nil
			} else {
				d.Params = kept
			}
		}
	}

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
		return s.failAndStop(ctx, tenantID, rec, classify(failureReasonUnknownChannel, fmt.Errorf("notification: deliver on unknown channel %q", channel)))
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
			return s.failAndStop(ctx, tenantID, rec, classify(failureReasonRenderFailed, err))
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
				return s.failAndRetry(ctx, tenantID, rec, classify(failureReasonInboxWriteFailed, err))
			}
			row = winner
		}
	}

	if err := s.announceInbox(ctx, tenantID, d, row); err != nil {
		// The row is durable; only its announcement failed. Record the
		// failed attempt and retry -- the retry's probe finds the row and
		// re-announces instead of re-writing.
		return s.failAndRetry(ctx, tenantID, rec, classify(failureReasonInboxAnnounceFailed, err))
	}
	rec.Status = SendRecordStatusSucceeded
	return s.settle(ctx, tenantID, rec)
}

// buildInboxRow renders one type's in-app copy into the row the delivery
// will write. The row's Params column carries the JSON of the template
// parameters that produced the copy, so a later re-render (a locale change,
// say) needs no re-parse of the source dispatch; a dispatch with no
// parameters stores the NULL column, never the JSON "null". Only parameters
// the type's declaration marks recipient-visible AND the in-app copy
// itself renders can reach this column: runDelivery narrows the payload
// before this method runs (recipientVisibleOnly), and deliverUserChannel
// narrows it further to what this channel's own copy references
// (copyParamsForChannel), so the column holds exactly the parameters the
// row's title and body were rendered from -- which is what makes the row
// safe to serve back through the inbox API as it stands.
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
		return s.failAndStop(ctx, tenantID, rec, classify(failureReasonResolverMissing, ErrUserAddressResolverRequired))
	}
	addrs, err := s.resolver.Resolve(ctx, d.Recipient.UserID)
	if err != nil {
		// Address resolution failed -- a store hiccup in the host's
		// identity half. Record the failed attempt and retry.
		return s.failAndRetry(ctx, tenantID, rec, classify(failureReasonResolutionFailed, fmt.Errorf("notification: resolve addresses for user %s: %w", d.Recipient.UserID, err)))
	}
	if addrs.Email == "" {
		// No email on file is a legitimate, possibly temporary state -- a
		// dispatch racing a profile setup -- and the queue must not retry
		// it. The skipped record keeps the outcome observable.
		return s.skipAndStop(ctx, tenantID, rec, skipReasonNoEmail)
	}

	parts, err := renderContent(s.catalog(), d.Locale, d.TypeKey, ChannelEmail, d.Params)
	if err != nil {
		return s.failAndStop(ctx, tenantID, rec, classify(failureReasonRenderFailed, err))
	}

	start := time.Now()
	err = s.sendMail(ctx, parts, []string{addrs.Email})
	rec.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		// The record stores no transport text: a transport error may echo the
		// recipient's address in whatever form the transport chose, so the
		// bounded classification is what the record carries, while the raw
		// cause stays reachable through Unwrap for errors.Is/As.
		cause := classifyTransportCause(err)
		if errors.Is(err, ErrTransportPermanent) {
			// A user's address is the host's data, not a verified_contacts
			// row, so there is no contact to mark bounced -- the refusal
			// is terminal, recorded, and the job stops.
			return s.failAndStop(ctx, tenantID, rec, cause)
		}
		return s.failAndRetry(ctx, tenantID, rec, cause)
	}
	rec.Status = SendRecordStatusSucceeded
	return s.settle(ctx, tenantID, rec)
}

// deliverUserSMS is the SMS channel's user delivery path: the twin of
// deliverUserEmail over the module's SMS sender, rendering the type's SMS
// copy (a single text).
func (s *DeliveryService) deliverUserSMS(ctx context.Context, tenantID string, d Dispatch, rec *SendRecord) error {
	if s.resolver == nil {
		return s.failAndStop(ctx, tenantID, rec, classify(failureReasonResolverMissing, ErrUserAddressResolverRequired))
	}
	addrs, err := s.resolver.Resolve(ctx, d.Recipient.UserID)
	if err != nil {
		return s.failAndRetry(ctx, tenantID, rec, classify(failureReasonResolutionFailed, fmt.Errorf("notification: resolve addresses for user %s: %w", d.Recipient.UserID, err)))
	}
	if addrs.Phone == "" {
		return s.skipAndStop(ctx, tenantID, rec, skipReasonNoPhone)
	}

	parts, err := renderContent(s.catalog(), d.Locale, d.TypeKey, ChannelSMS, d.Params)
	if err != nil {
		return s.failAndStop(ctx, tenantID, rec, classify(failureReasonRenderFailed, err))
	}

	start := time.Now()
	err = s.sendSMS(ctx, parts, addrs.Phone)
	rec.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		cause := classifyTransportCause(err)
		if errors.Is(err, ErrTransportPermanent) {
			return s.failAndStop(ctx, tenantID, rec, cause)
		}
		return s.failAndRetry(ctx, tenantID, rec, cause)
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
// channel. Before any channel's transport runs, the type registry is
// consulted and a type nobody declared is terminal-refused and recorded
// (see the gate below) -- the contact path's half of the undeclared-type
// refusal the user path makes through ResolveForDelivery, which no channel
// exists to record under there. A verified contact then proceeds to its
// channel's transport, and a permanent transport refusal marks the
// tenant's own contact bounced (MarkBounced) before the attempt is
// recorded -- the delivery job's hard-failure leg; writing the platform
// blacklist is a later round's work (blacklist.go's doc comment records
// the boundary).
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

	// The type registry is consulted before anything renders: an undeclared
	// type carries no declared channels, no preference-matrix entry and no
	// unsubscribe decision -- and its copy would render anyway whenever a
	// locale bundle happens to carry the <type_key>.<channel>.<part> ids,
	// which is exactly the harmful half of a module that ships its
	// templates but forgets reg.Notifications.Add. A message that went out
	// past every preference and opt-out decision the declaration owns is
	// the delivery this refusal exists to prevent (deliverToUser refuses
	// the same type through ResolveForDelivery before any channel exists).
	// The refusal is terminal and recorded under the contact's own channel,
	// and the job stops -- the same recorded-stop shape the corrupt
	// unknown-channel row below takes, since no retry can declare a type
	// nobody declared.
	if _, err := s.prefs.lookupType(d.TypeKey); err != nil {
		return s.failAndStop(ctx, tenantID, rec, classify(failureReasonTypeUndeclared, err))
	}

	switch contact.Channel {
	case ChannelEmail:
		return s.deliverContactEmail(ctx, tenantID, d, contact, rec)
	case ChannelSMS:
		return s.deliverContactSMS(ctx, tenantID, d, contact, rec)
	default:
		// A verified contact whose channel no transport can serve -- a row
		// only a writer around CreateContact's own channel validation could
		// have produced -- is a terminal state, not a retryable one: the
		// attempt is recorded as a failed send under the corrupt channel
		// and the job stops, so an operator reads the refusal in the send
		// records and nothing walks the retry-and-dead-letter horizon over
		// a row that will never heal.
		return s.failAndStop(ctx, tenantID, rec, classify(failureReasonUnknownChannel, fmt.Errorf("notification: contact %s is verified on unknown channel %q", contact.ID, contact.Channel)))
	}
}

// settleContactRefusal records one terminal consent refusal -- a contact
// that unsubscribed or bounced -- as a skipped send. The channel comes from
// the refusal error's own "channel" parameter (EnsureDeliverable attaches
// the contact's channel to these two refusals; the other refusals carry no
// channel and are not recorded, because a send record without a channel
// could never be probed by a retry). A refusal that lands on a key whose
// record already says succeeded is dropped by settle's
// never-downgrade-succeeded guard: the earlier delivery's outcome is the
// durable fact, whatever the recipient's consent says now.
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
		return s.failAndStop(ctx, tenantID, rec, classify(failureReasonRenderFailed, err))
	}

	start := time.Now()
	err = s.sendMail(ctx, parts, []string{contact.Address})
	rec.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		cause := classifyTransportCause(err)
		if errors.Is(err, ErrTransportPermanent) {
			// The address rejects mail. Mark the tenant's own contact
			// bounced -- its future deliveries are refused by the ledger
			// before any transport -- record the failed attempt, and stop.
			bounceErr := s.contacts.MarkBounced(ctx, contact.ID)
			stopErr := s.failAndStop(ctx, tenantID, rec, cause)
			if bounceErr != nil {
				// The bounce did not land; the record did. Retrying
				// re-runs the whole path and converges the bounce.
				return errors.Join(stopErr, bounceErr)
			}
			return stopErr
		}
		return s.failAndRetry(ctx, tenantID, rec, cause)
	}
	rec.Status = SendRecordStatusSucceeded
	return s.settle(ctx, tenantID, rec)
}

// deliverContactSMS is the SMS channel's contact delivery path, the twin of
// deliverContactEmail over the module's SMS sender.
func (s *DeliveryService) deliverContactSMS(ctx context.Context, tenantID string, d Dispatch, contact *VerifiedContact, rec *SendRecord) error {
	parts, err := renderContent(s.catalog(), platformDefaultLocale, d.TypeKey, ChannelSMS, d.Params)
	if err != nil {
		return s.failAndStop(ctx, tenantID, rec, classify(failureReasonRenderFailed, err))
	}

	start := time.Now()
	err = s.sendSMS(ctx, parts, contact.Address)
	rec.DurationMs = time.Since(start).Milliseconds()
	if err != nil {
		cause := classifyTransportCause(err)
		if errors.Is(err, ErrTransportPermanent) {
			bounceErr := s.contacts.MarkBounced(ctx, contact.ID)
			stopErr := s.failAndStop(ctx, tenantID, rec, cause)
			if bounceErr != nil {
				return errors.Join(stopErr, bounceErr)
			}
			return stopErr
		}
		return s.failAndRetry(ctx, tenantID, rec, cause)
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

// The bounded outcome text a send record's Error column carries. The column
// stores no failure text of any origin: a transport failure routinely echoes
// the recipient's address in whatever form the transport chose -- not
// necessarily the normalized form the module handed it -- and send_records
// is a platform table with no deletion path, so the module refuses to store
// any raw cause text at all. A failed record instead carries the
// classification of its failure, decided at the settle site, where the code
// knows which class the failure is; a skipped record carries the short
// reason of the deliberate non-send; a succeeded record carries the
// empty-string sentinel. Both vocabularies are closed and module-authored,
// so the column -- and every read of it, the D10 operator search first
// among them -- can never carry plaintext PII.
//
// The skip reasons: a skip is a deliberate non-send -- no address on file,
// consent withdrawn, the address bounced -- and the reason is the
// operator's whole answer on why; the empty-string sentinel covers the
// records that never skipped.
const (
	skipReasonNoEmail      = "no email address on file"
	skipReasonNoPhone      = "no phone number on file"
	skipReasonUnsubscribed = "contact unsubscribed"
	skipReasonBounced      = "contact bounced"
)

// The failure classifications of a failed send record, grouped by the
// settle site that decides them. failureReasonTransportRefused and
// failureReasonTransportFailed classify a transport error by its
// permanent/transient signal (ErrTransportPermanent): refused is terminal
// and stops the channel (and, on the contact path, marks the contact
// bounced), failed is retried. failureReasonResolutionFailed and
// failureReasonResolverMissing cover the host's user-address resolver
// seam; failureReasonRenderFailed covers every copy-render refusal (a
// missing template or catalog is terminal and cannot heal on retry);
// failureReasonTypeUndeclared covers the contact path's registry gate;
// failureReasonUnknownChannel the corrupt-row refusal on both recipient
// paths; and failureReasonInboxWriteFailed / failureReasonInboxAnnounceFailed
// the in-app channel's two retried infrastructure failures.
const (
	failureReasonTransportRefused    = "transport refused"
	failureReasonTransportFailed     = "transport failed"
	failureReasonResolutionFailed    = "user address resolution failed"
	failureReasonResolverMissing     = "user address resolver not wired"
	failureReasonRenderFailed        = "render failed"
	failureReasonTypeUndeclared      = "notification type not declared"
	failureReasonUnknownChannel      = "unknown delivery channel"
	failureReasonInboxWriteFailed    = "inbox row write failed"
	failureReasonInboxAnnounceFailed = "inbox announcement failed"
)

// classifiedError is the error every failing settle site records and
// returns: Error() renders the bounded classification -- one of the
// failureReason* values above -- while Unwrap exposes the original cause,
// so the delivery job's retry signals -- and a host's OnFailure
// classification through errors.Is or apperr.As -- still see the
// transport's own wrapped error and the module's coded errors, even though
// no raw text travels in the record or in the returned error's own
// message.
type classifiedError struct {
	reason string
	cause  error
}

func (e *classifiedError) Error() string { return e.reason }

func (e *classifiedError) Unwrap() error { return e.cause }

// classify returns cause under the bounded classification reason: the
// returned error reads as reason and unwraps to cause. Every failing
// settle site builds its record's outcome this way.
func classify(reason string, cause error) error {
	return &classifiedError{reason: reason, cause: cause}
}

// classifyTransportCause classifies a transport failure by its permanent
// signal: a cause wrapping ErrTransportPermanent is a refusal (terminal),
// any other transport failure is retried -- the two failureReasonTransport*
// values. The delivery paths' four send sites and the verification-code
// send path (contact.go's sendCode) share the classification.
func classifyTransportCause(cause error) error {
	if errors.Is(cause, ErrTransportPermanent) {
		return classify(failureReasonTransportRefused, cause)
	}
	return classify(failureReasonTransportFailed, cause)
}

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
// type, channel, locale, occurrence, parameters) send is recorded under:
// the SHA-256 of the canonical JSON of the seed below, hex-encoded.
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
// collapse into one key. The locale participates for the same reason on
// the language axis: a delivery rendered in another locale is a different
// copy, and a resend after a locale change must deliver in the new locale,
// never be swallowed by the old delivery's record. The recipient's id and
// the channel do likewise: one dispatch fans out to one key per channel,
// so each channel's delivery is independently replay-safe. The caller's
// per-occurrence marker (Dispatch.OccurrenceID) participates last: a
// deliberate resend of identical content is a new occurrence, and the
// caller names it by dispatching under a fresh marker -- without one, two
// dispatches of identical content stay the one delivery the replay probe
// dedupes, which is what keeps a queue retry from double-sending. By the
// time the key is derived the payload's parameters are already narrowed to
// the type's recipient-visible set (runDelivery applies recipientVisibleOnly
// first), and a user delivery's channel leg narrows them further to the
// parameters that channel's own copy references (deliverUserChannel), so
// the key never depends on delivery-internal context and never on a
// parameter no rendered copy uses -- each narrowing is a pure function,
// giving every replica and every retry the same key for the same payload.
func deriveDeliveryKey(tenantID string, d Dispatch, channel string) (string, error) {
	seed := deliveryKeySeed{
		TenantID:       tenantID,
		TypeKey:        d.TypeKey,
		RecipientClass: d.Recipient.Class,
		UserID:         d.Recipient.UserID,
		ContactID:      d.Recipient.ContactID,
		Channel:        channel,
		Locale:         deliveryLocale(d),
		OccurrenceID:   d.OccurrenceID,
		Params:         d.Params,
	}
	raw, err := json.Marshal(seed)
	if err != nil {
		return "", fmt.Errorf("notification: derive delivery key: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// deliveryLocale returns the locale the copy of d is actually rendered in
// -- the value the delivery key must key on, since a different rendered
// copy is a different delivery. A user recipient renders in the dispatch's
// own Locale; an external contact's copy renders in the platform default
// locale whatever the dispatch's Locale field says (that field is
// contractually ignored for the external class -- see Dispatch.Locale), so
// the key uses the rendered default rather than an ignored field whose
// variation between two otherwise identical dispatches would invent a
// delivery that renders the same copy twice.
func deliveryLocale(d Dispatch) string {
	if d.Recipient.Class == RecipientClassExternal {
		return platformDefaultLocale
	}
	return d.Locale
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
	Locale         string         `json:"locale"`
	OccurrenceID   string         `json:"occurrence_id,omitempty"`
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
// write every delivery path funnels through. It probes the record already
// under the attempt's key, adopts its id (a retry overwrites its earlier
// attempts' row in place, so one delivery keeps one record for life) or
// invents one for the first write, and returns nil when the record landed.
// The outcome text is bounded by construction -- the failure classes and
// skip reasons above never approach the column's width -- so settle does no
// write-site truncation. A record that
// fails to land is returned as an error: the attempt's outcome must be
// visible even when the record write itself failed, which is what makes a
// lost succeeded record a retried delivery rather than a silent gap in the
// log.
//
// # Never-downgrade-succeeded
//
// A row that already says succeeded refuses to be overwritten with a
// different terminal status -- skipped or failed -- and the settle that
// would write it returns nil without writing: the succeeded row is the
// historical fact that this key already delivered, and a later settle under
// the same key (a refusal replay landing after the recipient unsubscribed
// or bounced, a retry failing inside the acknowledged double-send window,
// any future rejection path) must never erase it. The write is dropped, not
// rewritten -- updated_at stays unmoved -- and the caller sees the same nil
// convergence its own settle would have returned; the job's next retry then
// converges on the alreadyDelivered probe.
//
// The refusal is decided by the write itself, never by a probe: every write
// to an existing row runs through the repository's SaveGuarded, a
// compare-and-set whose single UPDATE statement carries the guard in its
// WHERE -- the row's status is checked and the row written in the one
// statement the database serializes, so a succeeded row committed by a
// concurrent settle between this settle's probe and its write still refuses
// this write. SQLite has no SELECT FOR UPDATE, which is why the guard
// cannot be a probe-then-write pair; a two-statement guard has exactly the
// losing interleaving of the acknowledged double-send window (the guard
// checked, the winner's succeeded settle committed, the loser's write
// erasing it), and the statement-level compare-and-set is the answer
// (contact.go's verified-contact flips use the identical shape). The
// probe-time check above the write is only a fast path that skips a write
// the guard would refuse all the same; it is not the authority.
//
// Two attempts that race for a key no record exists under yet both write
// their own fresh id, and the loser converges on the UNIQUE (tenant_id,
// idempotency_key) index: its insert fails, settle re-probes, adopts the
// winner's row and writes it guarded -- the upgrade a succeeded attempt
// makes over a failed row the guard allows, and the drop when the winner
// already says succeeded. settle is entered with an empty ID by every
// delivery path (records are built through sendRecordFor, so the adopt
// branch is where an existing row's id is ever picked up); only the
// guarded writes below can meet an already-succeeded row.
func (s *DeliveryService) settle(ctx context.Context, tenantID string, rec *SendRecord) error {
	adopted := rec.ID != ""
	if !adopted {
		existing, err := s.sendRecs.ByTenantAndKey(ctx, tenantID, rec.IdempotencyKey)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.Status == SendRecordStatusSucceeded && rec.Status != SendRecordStatusSucceeded {
				// Never-downgrade-succeeded fast path: the key already
				// delivered; SaveGuarded below would refuse this settle all
				// the same, so skip the write (see the doc above).
				return nil
			}
			rec.ID = existing.ID
			adopted = true
		} else {
			// First write for the key: invent the record's id for life.
			rec.ID = uuid.NewString()
		}
	}

	if !adopted {
		// A fresh record inserts with plain Save -- it cannot erase a
		// succeeded row that does not exist yet. When the insert races
		// another writer that committed the same (tenant, key) first, it
		// fails on the UNIQUE index and falls through to adopt the winner's
		// row below.
		err := s.sendRecs.Save(ctx, rec)
		if err == nil {
			s.recordDeliveryMetrics(ctx, rec)
			return nil
		}
		existing, probeErr := s.sendRecs.ByTenantAndKey(ctx, tenantID, rec.IdempotencyKey)
		if probeErr != nil {
			return errors.Join(err, probeErr)
		}
		if existing == nil {
			// The insert failed for its own reason, not a key race -- no
			// winner row exists to adopt. The outcome must stay visible:
			// return the failure for the job to retry.
			return err
		}
		rec.ID = existing.ID
	}

	// An adopted row is written by one guarded statement: SaveGuarded
	// checks the never-downgrade-succeeded guard in the same UPDATE that
	// writes, so the refusal reflects the row's state at write time -- a
	// succeeded row a concurrent settle committed after this settle's probe
	// still refuses this write. A refused write drops this attempt's
	// outcome (nil, no metrics): the succeeded row is the durable fact that
	// the key delivered.
	landed, err := s.sendRecs.SaveGuarded(ctx, rec)
	if err != nil {
		return err
	}
	if landed {
		s.recordDeliveryMetrics(ctx, rec)
	}
	return nil
}

// recordDeliveryMetrics records one delivery attempt's outcome onto the
// "notification.delivery.count" Counter and "notification.delivery.duration"
// Histogram, labeled by notification type, channel and resulting status
// only -- deliberately never tenant_id, for the identical cardinality reason
// go/jobs/standalone_queue.go's registerJobMetrics doc comment gives (all
// three label values are bounded, declared vocabularies: a type key from the
// host's type registry, one of the three channel constants, one of the
// three SendRecordStatus* values). Called from settle after each write it
// persists lands -- the single write funnel every delivery path (success,
// failure and skip alike) runs through -- never for a write the
// never-downgrade-succeeded guard dropped, so the recorded outcome always
// matches the record actually persisted, even across settle's own id-race
// retry (only the winning row's settle counts once). rec.DurationMs is 0 for a channel
// with no transport call (deliverInbox never sets it), which the histogram
// simply records as a zero-duration observation rather than skipping.
//
// Guarded against s.deliveryCount/s.deliveryDuration being nil -- the same
// fail-open contract StandaloneQueue's own metric fields document -- so a
// registerDeliveryMetrics failure (in practice unreachable; see that
// function's own doc comment) never turns a metrics gap into a panic on the
// delivery path itself.
func (s *DeliveryService) recordDeliveryMetrics(ctx context.Context, rec *SendRecord) {
	attrs := metric.WithAttributes(
		attribute.String("type_key", rec.TypeKey),
		attribute.String("channel", rec.Channel),
		attribute.String("status", rec.Status),
	)
	if s.deliveryCount != nil {
		s.deliveryCount.Add(ctx, 1, attrs)
	}
	if s.deliveryDuration != nil {
		s.deliveryDuration.Record(ctx, float64(rec.DurationMs)/1000, attrs)
	}
}

// failAndRetry records the attempt as a failed send record carrying cause's
// bounded classification (every failing settle site passes a classifiedError,
// so the record stores the class, never raw cause text), then returns cause
// (joined with a record-write failure, should one land):
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

// failAndStop records the attempt as a failed send record carrying cause's
// bounded classification (the same classifiedError contract as failAndRetry) and
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
