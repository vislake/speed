// The reference app's demo glue for go/notification: the host-side seams a
// real deployment of the module needs and this app has no real source for,
// plus the hand-written routes and subscriptions that demonstrate the module
// end to end. flowtests/notification_flow_test.go drives everything here through the
// composed HTTP stack.
//
// The glue is deliberately thin and deliberately demo-shaped:
//
//   - DemoUserAddresses stands in for the user-address store a real host
//     reads from (authn's users table, or a profile service): the demo
//     users of this app exist only as header values (demo_subject.go), with
//     no address store behind them, so the notification module is wired
//     with go/notification/staticaddr over this fixed table (see
//     server.go's assembly site). A real resolver would read the address on
//     file for the user id the delivery job asks about; staticaddr returns
//     the table's entry, and its package doc carries the seam's obligation
//     -- the table is the operator's declaration of verified addresses.
//
//   - wireDemoNotification's note-created subscription is the reference
//     app's instance of the canonical event-driven flow: a business module
//     (notes) publishes a domain event as a fact; the notification module
//     consumes it as a dispatch trigger. notes publishes notes.note.created
//     with the creating user's id; the subscription dispatches the same
//     type's notification to that creator (RecipientClassUser), whose
//     channels the notification module resolves against the creator's
//     preference matrix at send time. The copy's language is the
//     recipient's stored locale through the AuthnUserLocales adapter
//     below (the header-only demo identities have no profile row, so for
//     them the chain lands on the platform default); a real host resolves
//     the same value from its own profile store.
//
//   - the demo patient-message route is the module's external-recipient
//     leg, a hand-written POST /api/v1/demo/patient-message route that
//     dispatches the demo module's demo.patient_reminder type (see
//     internal/demo) to a verified external contact of the caller's
//     tenant. It exists outside the OpenAPI machinery on purpose: it is
//     host application code, not a module surface, and its trigger is a
//     scheduling decision (an appointment approaching) that belongs to the
//     host, never to the notification module. The route demonstrates the
//     external-recipient language rule: the requesting staff member's own
//     language (the request's Accept-Language, negotiated against the
//     merged catalog) is captured here and dispatched as the copy's
//     language, because the contact row itself carries none.
//
// None of the glue makes the module depend on the host or the host's other
// modules: every seam below is a structurally-typed implementation of an
// interface go/notification declares, in the same no-import direction org's
// own host seams observe -- the host implements, the module consumes.

package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/vislake/speed/examples/reference-app/internal/demo"
	"github.com/vislake/speed/examples/reference-app/internal/notes"
	"github.com/vislake/speed/examples/reference-app/internal/smilesim"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/i18n"
)

// AuthnUserLocales adapts authn's user store to notification's
// UserLocaleResolver seam: the type directory's fallback tier asks it for
// the caller's stored locale when the request's Accept-Language matched
// nothing, and the demo glue's note-created subscription asks it for the
// note creator's stored language. It is the same two-modules-touching
// adapter shape the app wires everywhere else -- the host implements, the
// modules consume, neither module imports the other -- and it holds the
// authn MODULE rather than a service because this wiring runs before
// Bootstrap: Service() is nil until authn's Register has run, and a
// request can only reach the seam after that.
type AuthnUserLocales struct {
	authn *authn.Module
}

// UserLocale implements notification.UserLocaleResolver. A user the store
// cannot resolve (the header-only demo identities among them) and a user
// who never chose a language both answer ok=false: "nothing to confirm",
// not an error -- the same contract the seam's own doc comment states.
func (a AuthnUserLocales) UserLocale(ctx context.Context, userID string) (string, bool, error) {
	if a.authn == nil {
		return "", false, nil
	}
	svc := a.authn.Service()
	if svc == nil {
		return "", false, nil
	}
	user, err := svc.Users().FindByID(ctx, userID)
	if err != nil {
		if errors.Is(err, authn.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	if user.Locale == "" {
		return "", false, nil
	}
	return user.Locale, true, nil
}

var _ notification.UserLocaleResolver = AuthnUserLocales{}

// DemoUserAddresses maps the demo user ids this app's flows act as (see
// demo_subject.go's demo user constants and DemoNotesCreatorUserID) to the
// outbound addresses a real host would hold in its own address store. The
// map is this app's stand-in for that store -- and nothing more: the users
// this app's own flows act as exist only as header values, so there is no
// address table to read.
//
// Only DemoNotesCreatorUserID carries an email: it is the only id a
// note-created event this app's flow helpers publish can name (every
// helper sends it as the X-Demo-User-Id header notes' create handler
// attributes through), and the email is what lets a note-created delivery
// reach the email channel. A note-created event can instead name a seeded
// account's real user id, when the account acts through its access token
// with no demo header -- DemoNotesSubjectResolver's Principal fallback,
// the shape of cmd/server/demo_users_test.go's own requests; that id has no entry
// here, so its delivery resolves to no addresses, the same ordinary skip
// as any other user with no addresses. DemoNotesCreatorUserID carries no
// phone,
// which the flow tests use deliberately: a user delivery whose SMS channel
// finds no phone address is skipped with a recorded send record, never
// failed (see UserAddresses' own doc comment). Every other demo user
// resolves to no addresses -- an ordinary state, not an error.
//
// DemoSmileSimRecipientUserID is the smile-simulation completion
// notification's demo recipient fixture: a phone-only entry (the smilesim
// completion notification's DefaultChannels is sms-only -- see
// internal/demo/module.go's TypeKeySimulationReady -- so a phone is the
// one address that matters here), named as a POST /simulate request's
// recipient_user_id by the flow test that drives the delivery. That test
// also grants the fixture an ACTIVE MEMBERSHIP in the tenant its caller
// operates in -- the demo shape of a real patient account in the clinic's
// own tenant -- because the simulate surface refuses any recipient that is
// not an active member of the caller's tenant (smilesim.go's
// validateSimulateRecipient): a recipient id that belongs to another
// tenant must never receive this tenant's notification.

// DemoSmileSimRecipientUserID is the user id of the fixture patient
// account whose outbound address the demo notification flows deliver to.
const DemoSmileSimRecipientUserID = "user-smilesim-recipient-1"

// DemoUserAddresses is the demo stand-in for a real host's address store,
// mapping the demo user ids (see the narrative above) to their outbound
// addresses.
var DemoUserAddresses = map[string]notification.UserAddresses{
	DemoNotesCreatorUserID:      {Email: "user-creator-1@demo.example"},
	DemoSmileSimRecipientUserID: {Phone: "+8613800138099"},
}

// demoPatientMessagePath is the demo patient-message route: POST it with a
// JSON body naming a verified external contact of the caller's tenant
// ({"contact_id": "..."}), and the demo module's patient-reminder type is
// dispatched to that contact. The route exists to give the notification
// module's external-recipient leg a real trigger in this app (see the
// package comment); a real deployment would trigger the same Dispatch from
// its own scheduling service.
const demoPatientMessagePath = "/api/v1/demo/patient-message"

// noteCreatedFieldKeys are the field spellings accepted for the note id and
// the creator user id inside a notes.note.created payload, probed in order
// by NoteCreatedFieldsFromPayload.
//
// Several spellings are accepted because the payload reaches the
// subscription as data, not as a type: a same-process publish delivers
// notes' struct (whose fields this package could name only by importing
// notes' type), while the Redis bus delivers a map built from that struct's
// JSON encoding -- and even within one bus, a struct's fields marshal under
// their own names, so the probe names both the struct spellings and the
// map's. The pattern is go/org's own (see go/org/events.go's
// userCreatedUserIDKeys, which probes the identical situation for
// authn.user.created); notes' payload type deliberately carries no JSON tags
// (the reference app's events are facts, not API contracts), so the struct
// spellings are the plain field names.
var noteCreatedFieldKeys = struct {
	noteID  []string
	creator []string
}{
	noteID:  []string{"note_id", "NoteID", "noteId"},
	creator: []string{"creator_user_id", "CreatorUserID", "creatorUserID"},
}

// NoteCreatedFieldsFromPayload extracts the note id and the creating user's
// id from a notes.note.created payload of any shape, probing the accepted
// key spellings through pkgcore.EventPayloadString. It returns ok=false
// rather than an error for every unusable shape, because the subscription's
// contract is to log and drop the event, never to fail the publisher (see
// wireDemoNotification).
func NoteCreatedFieldsFromPayload(payload any) (noteID, creatorUserID string, ok bool) {
	noteID, hasNote := pkgcore.EventPayloadString(payload, noteCreatedFieldKeys.noteID...)
	creatorUserID, hasCreator := pkgcore.EventPayloadString(payload, noteCreatedFieldKeys.creator...)
	if !hasNote || !hasCreator {
		return "", "", false
	}
	return noteID, creatorUserID, true
}

// simulationCompletedFieldKeys are the field spellings accepted for the
// image job id, the recipient user id and the success flag inside a
// smilesim.EventSimulationCompleted payload, probed in order by
// SimulationCompletedFieldsFromPayload.
//
// Both sides of this subscription are reference-app code, but that does NOT
// make the payload's wire shape unambiguous: pkgcore's in-memory EventBus
// delivers the publisher's exact Go value, so a same-process subscriber on
// that bus sees the real smilesim.SimulationCompletedPayload struct, but
// pkgcore/eventbus/redis's EventBus always round-trips a payload through
// JSON before a subscriber on a different bus instance sees it -- including
// a subscriber in the SAME process, if that subscriber's own bus instance is
// not the one that published (a genuinely distributed deployment's normal
// shape, since every replica subscribes on its own instance and the bus
// fans a publish out to all of them). This subscription and smilesim's own
// publisher happen to share one bus instance in this app's current wiring,
// but nothing about the pkgcore.EventBus contract guarantees that stays
// true, and the note-created subscription's own NoteCreatedFieldsFromPayload
// probe exists for the identical reason. smilesim.SimulationCompletedPayload
// carries no JSON tags (like notes' own payload, its fields are facts, not
// an API contract), so the map spellings below are the plain struct field
// names.
var simulationCompletedFieldKeys = struct {
	imageJobID      []string
	recipientUserID []string
	succeeded       []string
}{
	imageJobID:      []string{"image_job_id", "ImageJobID", "imageJobID"},
	recipientUserID: []string{"recipient_user_id", "RecipientUserID", "recipientUserID"},
	succeeded:       []string{"succeeded", "Succeeded"},
}

// SimulationCompletedFieldsFromPayload extracts the image job id, the
// recipient user id and the success flag from a
// smilesim.EventSimulationCompleted payload of any shape, probing the
// accepted key spellings through pkgcore.EventPayloadString and
// EventPayloadBool -- mirroring NoteCreatedFieldsFromPayload exactly, down
// to returning ok=false rather than an error for every unusable shape (the
// subscription's contract is to log and drop the event, never to fail the
// publisher; see wireDemoNotification). imageJobID and recipientUserID must each be a
// non-empty string to count as present: recipientUserID matching
// SimulationCompletedPayload.RecipientUserID's own "never empty" invariant,
// and imageJobID because the dispatch below carries it in the delivery's
// Params -- the per-occurrence marker that keeps two completed simulations
// for the same recipient from collapsing into one delivery -- so a payload
// that cannot name the job it happened for has nothing this subscription
// can truthfully dispatch (a publisher that omits the job id is not this
// app's own NotifyOnCompletion, whose payload always carries it). succeeded
// is read as whatever bool value is actually present -- false is a
// meaningful, valid answer (a failed or cancelled simulation), so its
// presence is tracked separately from its value.
func SimulationCompletedFieldsFromPayload(payload any) (recipientUserID, imageJobID string, succeeded, ok bool) {
	imageJobID, hasJob := pkgcore.EventPayloadString(payload, simulationCompletedFieldKeys.imageJobID...)
	recipientUserID, hasRecipient := pkgcore.EventPayloadString(payload, simulationCompletedFieldKeys.recipientUserID...)
	succeeded, hasSucceeded := pkgcore.EventPayloadBool(payload, simulationCompletedFieldKeys.succeeded...)
	if !hasJob || !hasRecipient || !hasSucceeded {
		return "", "", false, false
	}
	return recipientUserID, imageJobID, succeeded, true
}

// wireDemoNotification mounts the reference app's demo glue for the
// notification module on mux: the subscription that turns notes'
// note-created event into a notification dispatch for the note's creator,
// and the demo patient-message route. bus is reg.EventBus() -- the same bus
// Kernel.Bootstrap gave every module, so the subscription hears exactly
// what notes' handler publishes -- and module is the app's notification
// module, whose Deliveries() accessor the two glue pieces drive.
//
// The call cannot fail: subscribing to a bus returns no error, and mounting
// a route on a *http.ServeMux cannot fail for a well-formed pattern.
func wireDemoNotification(mux *http.ServeMux, bus pkgcore.EventBus, module *notification.Module, reg *pkgcore.Registry, userLocales AuthnUserLocales) {
	// The note-created subscription: notes publishes notes.note.created as
	// a fact (see internal/notes/handler.go's publishNoteCreated) whenever
	// a note is created; this subscription dispatches the type of the same
	// name to the note's creator. The dispatch carries only what the job
	// needs to render (the type key, the recipient, the locale, the
	// template's note_id parameter); everything that can go stale -- the
	// creator's preferences, the creator's addresses -- is re-read by the
	// delivery job at send time, never frozen into the payload (see
	// Dispatch's own doc comment).
	//
	// The tenant comes from the event envelope itself (evt.TenantID),
	// rebuilt into a cancel-free dispatch context inside the handler below
	// -- in a distributed composition the publisher and this subscriber
	// share no context at all, and in any composition the publisher's
	// cancellation must not be able to drop the dispatch once the note it
	// announced is committed (see the derivation's own inline comment).
	//
	// An event whose payload cannot be read is dropped with a logged
	// warning, never an error returned to the publisher: the subscription
	// is a consumer of facts, and a malformed fact (a payload shape this
	// app's own notes module never publishes) must not fail the note
	// creation that published it. The same log-and-continue rule covers a
	// dispatch the queue refuses -- with one nuance: Dispatch returns the
	// enqueued job's id, and an enqueue failure here is a wiring failure
	// (a stopped queue), which the log makes visible exactly once per
	// event rather than silently.
	bus.Subscribe(notes.EventNoteCreated, func(ctx context.Context, evt pkgcore.Event) error {
		logger := observability.FromContext(ctx)
		noteID, creatorUserID, ok := NoteCreatedFieldsFromPayload(evt.Payload)
		if !ok {
			// The log message is a constant string and the variable
			// values ride as key-value attributes (backend coding
			// standard's structured-logging rule); tenant_id is not
			// repeated as an explicit attribute because the logger read
			// from the publisher's context already carries it.
			logger.Warn("demo notification glue dropped a notes.note.created event with an unreadable payload",
				"event_type", evt.Type)
			return nil
		}
		// dispatchCtx is the publisher's context stripped of its
		// cancellation, with the event's tenant rebuilt into it. The
		// cancel-free derivation is the same boundary
		// internal/smilesim's NotifyOnCompletion draws around its own
		// post-terminal writes: by the time this subscription runs, the
		// note row is already committed, and a publisher-side disconnect
		// (the note-create request's client closing its tab the moment the
		// create landed) must not be able to drop the notification the
		// committed note was supposed to trigger -- this handler logs and
		// swallows a refused dispatch, so nothing else would ever retry
		// it. The tenant comes from the event envelope itself (evt.TenantID),
		// never assumed to survive from the publisher's context, and
		// pkgcore.WithTenant makes the enqueued job -- and every record it
		// writes -- belong to the note's tenant.
		dispatchCtx := pkgcore.WithTenant(context.WithoutCancel(ctx), evt.TenantID)
		// The creator's language comes from the same authn adapter the type
		// directory consults: the recipient's stored locale when the profile
		// store confirms one, the platform default when it does not (the
		// header-only demo identities have no profile, exactly as they have
		// no address outside the static table). This is the recipient-tier
		// half of the delivery chain -- the recipient is known and is not
		// the requester, so nothing about the publishing request's own
		// language participates.
		locale, ok, localeErr := userLocales.UserLocale(dispatchCtx, creatorUserID)
		if localeErr != nil {
			logger.Warn("demo notification glue could not read the note creator's locale; dispatching in the platform default",
				"user_id", creatorUserID, "error", localeErr)
		}
		if !ok {
			locale = i18n.LocaleENUS
		}
		_, err := module.Deliveries().Dispatch(dispatchCtx, notification.Dispatch{
			TypeKey: notes.EventNoteCreated,
			Recipient: notification.DispatchRecipient{
				Class:  notification.RecipientClassUser,
				UserID: creatorUserID,
			},
			Locale: locale,
			Params: map[string]any{"note_id": noteID},
		})
		if err != nil {
			logger.Warn("demo notification glue could not dispatch the note-created delivery",
				"event_type", evt.Type, "user_id", creatorUserID, "error", err)
		}
		return nil
	})

	// The demo patient-message route: dispatch the demo module's
	// patient-reminder type to a verified external contact of the
	// caller's tenant. The caller's tenant context is the middleware
	// chain's doing (the same tenancy.Middleware that gates every other
	// mounted route), and the dispatch context below carries it into the
	// enqueued job untouched.
	//
	// The route takes no subject and checks no permission of its own: in
	// this app every authenticated member of a tenant may trigger a demo
	// reminder, and the module's own send-time gates -- re-checked by the
	// delivery job after enqueue -- are what actually protect the
	// recipient: the named contact is resolved inside the caller's tenant
	// and must be verified (an unknown or still-pending contact is refused,
	// and one that has since unsubscribed or bounced is settled as a
	// skipped send before any transport is touched), and the send travels
	// only on the contact's own verified channel. The platform blacklist
	// is not among those gates: nothing in the delivery
	// pipeline consults it, and no writer or bounce-remediation path
	// exists to populate it. A real deployment would gate its
	// own trigger route however its staff model requires; the dispatch
	// call itself is all the notification module asks of it.
	mux.HandleFunc(http.MethodPost+" "+demoPatientMessagePath, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ContactID string `json:"contact_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "demo patient-message: malformed JSON body", http.StatusBadRequest)
			return
		}
		if body.ContactID == "" {
			http.Error(w, "demo patient-message: contact_id is required", http.StatusBadRequest)
			return
		}
		// The requester's own language, captured from this request and
		// dispatched as the copy's language: an external contact carries no
		// locale of its own, and the requesting staff member's language is
		// the contact's first available signal (the producer-captured
		// requester tier of the dispatch chain). A request naming no
		// language this deployment's catalog ships dispatches an empty
		// Locale, and the module renders the platform default.
		locale := ""
		if catalog := reg.Locales(); catalog != nil {
			locale, _ = i18n.Negotiate(r.Header.Get("Accept-Language"), catalog.Locales())
		}
		if _, err := module.Deliveries().Dispatch(r.Context(), notification.Dispatch{
			TypeKey: demo.TypeKeyPatientReminder,
			Recipient: notification.DispatchRecipient{
				Class:     notification.RecipientClassExternal,
				ContactID: body.ContactID,
			},
			Locale: locale,
			// The demo reminder type's templates take no parameters (see
			// internal/demo/locales); the copy is a fixed appointment
			// reminder.
			Params: map[string]any{},
		}); err != nil {
			http.Error(w, "demo patient-message: dispatch refused: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})

	// The smilesim completion subscription: internal/smilesim's Service
	// publishes smilesim.EventSimulationCompleted (see its own package
	// comment's "Completion notification" section) once a requested smile
	// simulation reaches a terminal status for a caller who named a
	// recipient; this subscription dispatches the demo module's own
	// demo.TypeKeySimulationReady type (a DIFFERENT string from the event
	// type -- see that constant's own doc comment for why) to that
	// recipient -- the identical "business fact published as a
	// pkgcore.Event, notification consumes it as a dispatch trigger" shape
	// notes.EventNoteCreated's subscription above uses, and for the
	// identical reason it too reads the payload through a probe
	// (SimulationCompletedFieldsFromPayload) rather than a naked type
	// assertion: both sides of this subscription are reference-app code,
	// but that does not make the payload's wire shape unambiguous -- see
	// the probe's own doc comment for why a same-process subscriber is not
	// automatically safe from the Redis EventBus's JSON round-trip.
	//
	// A failed simulation (succeeded: false) is deliberately never
	// dispatched: "your simulation is ready to view" would be actively
	// wrong for a job that dead-lettered or was cancelled, and this app
	// has no distinct "your simulation failed" copy to send instead.
	//
	// The dispatch below carries the completing job's own id in its
	// Params -- the per-occurrence marker mirroring this function's own
	// note-created dispatch carrying note_id -- so every completed
	// simulation delivers as the distinct occurrence it is (see the
	// dispatch call's own comment for why the parameter is load-bearing
	// and not just copy).
	bus.Subscribe(smilesim.EventSimulationCompleted, func(ctx context.Context, evt pkgcore.Event) error {
		logger := observability.FromContext(ctx)
		recipientUserID, imageJobID, succeeded, ok := SimulationCompletedFieldsFromPayload(evt.Payload)
		if !ok {
			logger.Warn("demo notification glue dropped a smilesim.simulation_completed event with an unreadable payload",
				"event_type", evt.Type)
			return nil
		}
		if !succeeded {
			return nil
		}
		// dispatchCtx carries the event's tenant, on a cancel-free copy of
		// the publisher's context -- the identical boundary
		// internal/smilesim's NotifyOnCompletion draws before it publishes
		// this very event (see its own persistCtx comment): by the time
		// this subscription runs, the simulation job is terminal and
		// settled, and a publisher-side disconnect must not be able to
		// drop the delivery the terminal job was supposed to trigger --
		// this handler logs and swallows a refused dispatch, so nothing
		// else would ever retry it.
		dispatchCtx := pkgcore.WithTenant(context.WithoutCancel(ctx), evt.TenantID)
		// Params carry the completing job's own id as
		// demo.simulation_ready.sms.text's {{.simulation_job_id}}
		// placeholder and -- the load-bearing half -- as part of the
		// derived delivery key go/notification hashes from the dispatch
		// (tenant, type, recipient, channel, params): two simulations
		// completed for the SAME recipient are two distinct occurrences,
		// and each must deliver on its own, never collapse into one
		// delivery because the second dispatch derived an identical key
		// and was settled as a duplicate of the first. The note-created
		// subscription above makes the identical choice for its note_id
		// parameter.
		// The recipient's language, resolved from the authn adapter exactly
		// as the note-created subscription above resolves its creator's:
		// the recipient's stored locale, else the platform default.
		locale, ok, localeErr := userLocales.UserLocale(dispatchCtx, recipientUserID)
		if localeErr != nil {
			logger.Warn("demo notification glue could not read the simulation recipient's locale; dispatching in the platform default",
				"user_id", recipientUserID, "error", localeErr)
		}
		if !ok {
			locale = i18n.LocaleENUS
		}
		if _, err := module.Deliveries().Dispatch(dispatchCtx, notification.Dispatch{
			TypeKey: demo.TypeKeySimulationReady,
			Recipient: notification.DispatchRecipient{
				Class:  notification.RecipientClassUser,
				UserID: recipientUserID,
			},
			Locale: locale,
			Params: map[string]any{"simulation_job_id": imageJobID},
		}); err != nil {
			logger.Warn("demo notification glue could not dispatch the simulation-completed delivery",
				"event_type", evt.Type, "user_id", recipientUserID, "error", err)
		}
		return nil
	})
}
