// The reference app's demo glue for go/notification: the host-side seams a
// real deployment of the module needs and this app has no real source for,
// plus the hand-written routes and subscriptions that demonstrate the module
// end to end. notification_flow_test.go drives everything here through the
// composed HTTP stack.
//
// The glue is deliberately thin and deliberately demo-shaped:
//
//   - demoUserAddressResolver stands in for the user-address store a real
//     host reads from (authn's users table, or a profile service): the demo
//     users of this app exist only as header values (demo_subject.go), with
//     no address store behind them, so the resolver answers from a fixed
//     table. A real resolver would read the address on file for the user id
//     the delivery job asks about; this one returns the demo table's entry.
//
//   - wireDemoNotification's note-created subscription is the reference
//     app's instance of the round's canonical flow: a business module
//     (notes) publishes a domain event as a fact; the notification module
//     consumes it as a dispatch trigger. notes publishes notes.note.created
//     with the creating user's id; the subscription dispatches the same
//     type's notification to that creator (RecipientClassUser), whose
//     channels the notification module resolves against the creator's
//     preference matrix at send time. Demo users carry no profile, so the
//     locale is the fixed zh-CN default authn itself uses; a real host
//     would negotiate the recipient's locale from its own profile store.
//
//   - the demo patient-message route is the module's external-recipient
//     leg, a hand-written POST /api/v1/demo/patient-message route that
//     dispatches the demo module's demo.patient_reminder type (see
//     internal/demo) to a verified external contact of the caller's
//     tenant. It exists outside the OpenAPI machinery on purpose: it is
//     host application code, not a module surface, and its trigger is a
//     scheduling decision (an appointment approaching) that belongs to the
//     host, never to the notification module.
//
// None of the glue makes the module depend on the host or the host's other
// modules: every seam below is a structurally-typed implementation of an
// interface go/notification declares, in the same no-import direction org's
// own host seams observe -- the host implements, the module consumes.
package main

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/vislake/speed/examples/reference-app/internal/demo"
	"github.com/vislake/speed/examples/reference-app/internal/notes"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/i18n"
)

// demoUserAddresses maps the demo user ids this app's flows act as (see
// demo_subject.go's demo user constants and demoNotesCreatorUserID) to the
// outbound addresses a real host would hold in its own address store. The
// map is this app's stand-in for that store -- and nothing more: demo users
// exist only as header values, so there is no address table to read.
//
// Only demoNotesCreatorUserID carries an email: it is the only demo user a
// note-created event can name (notes' create handler resolves the creator
// from the X-Demo-User-Id header, and every helper in the flow tests sends
// demoNotesCreatorUserID), and the email is what lets a note-created
// delivery reach the email channel. It carries no phone, which the flow
// tests use deliberately: a user delivery whose SMS channel finds no phone
// address is skipped with a recorded send record, never failed (see
// UserAddresses' own doc comment). Every other demo user resolves to no
// addresses -- an ordinary state, not an error.
var demoUserAddresses = map[string]notification.UserAddresses{
	demoNotesCreatorUserID: {Email: "user-creator-1@demo.example"},
}

// demoUserAddressResolver is the reference app's implementation of
// notification.UserAddressResolver: the host-side seam through which a user
// delivery learns the recipient's outbound addresses.
//
// The resolver is read at SEND time by the delivery job, never at enqueue
// time (see UserAddressResolver's doc comment), which is exactly why a
// static table works for this app: nothing here needs an address to exist
// before a dispatch is enqueued. A real deployment implements the same
// interface over its own address store -- authn's users table or a profile
// service -- and passes it through the same notification.WithUserAddressResolver
// option; the seam is all the module knows about users, and the host's
// implementation is all the module ever calls.
type demoUserAddressResolver struct{}

// Resolve implements notification.UserAddressResolver.
func (demoUserAddressResolver) Resolve(_ context.Context, userID string) (notification.UserAddresses, error) {
	return demoUserAddresses[userID], nil
}

// compile-time check that demoUserAddressResolver satisfies the seam.
var _ notification.UserAddressResolver = demoUserAddressResolver{}

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
// by noteCreatedFieldsFromPayload.
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

// noteCreatedFieldsFromPayload extracts the note id and the creating user's
// id from a notes.note.created payload of any shape, by round-tripping it
// through JSON into a map and probing the accepted key spellings. It returns
// ok=false rather than an error for every unusable shape, because the
// subscription's contract is to log and drop the event, never to fail the
// publisher (see wireDemoNotification).
func noteCreatedFieldsFromPayload(payload any) (noteID, creatorUserID string, ok bool) {
	if payload == nil {
		return "", "", false
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", "", false
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return "", "", false
	}
	probe := func(spellings []string) (string, bool) {
		for _, key := range spellings {
			if value, present := fields[key]; present {
				if id, isString := value.(string); isString && id != "" {
					return id, true
				}
			}
		}
		return "", false
	}
	noteID, hasNote := probe(noteCreatedFieldKeys.noteID)
	creator, hasCreator := probe(noteCreatedFieldKeys.creator)
	if !hasNote || !hasCreator {
		return "", "", false
	}
	return noteID, creator, true
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
func wireDemoNotification(mux *http.ServeMux, bus pkgcore.EventBus, module *notification.Module) {
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
	// The tenant comes from the event envelope itself (evt.TenantID), and
	// is rebuilt into the dispatch context explicitly: a subscription
	// handler must never assume the publisher's context survives to its
	// own queue enqueue (and in a distributed composition the two sides
	// share no context at all). pkgcore.WithTenant makes the enqueued job
	// -- and every record it writes -- belong to the note's tenant.
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
		noteID, creatorUserID, ok := noteCreatedFieldsFromPayload(evt.Payload)
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
		dispatchCtx := pkgcore.WithTenant(ctx, evt.TenantID)
		// The creator's locale is fixed to the zh-CN default for the same
		// reason the address resolver is a static table: demo users have no
		// profile to negotiate from, and authn's own default locale is
		// zh-CN. A real host dispatches the locale its profile store
		// negotiated for the recipient -- and the notification module
		// renders in exactly that locale, never a fallback.
		_, err := module.Deliveries().Dispatch(dispatchCtx, notification.Dispatch{
			TypeKey: notes.EventNoteCreated,
			Recipient: notification.DispatchRecipient{
				Class:  notification.RecipientClassUser,
				UserID: creatorUserID,
			},
			Locale: i18n.LocaleZHCN,
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
	// reminder, and the module's own send-time gates -- the contact must
	// be verified, the tenant's delivery channels must not be blacklisted
	// -- are what actually protect the recipient. A real deployment would
	// gate its own trigger route however its staff model requires; the
	// dispatch call itself is all the notification module asks of it.
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
		if _, err := module.Deliveries().Dispatch(r.Context(), notification.Dispatch{
			TypeKey: demo.TypeKeyPatientReminder,
			Recipient: notification.DispatchRecipient{
				Class:     notification.RecipientClassExternal,
				ContactID: body.ContactID,
			},
			// The demo reminder type's templates take no parameters (see
			// internal/demo/locales); the copy is a fixed appointment
			// reminder. An external contact's Locale is ignored by the
			// module by contract -- its copy renders in the platform
			// default locale -- so the field stays empty here, exactly as
			// Dispatch.validate requires for the external class.
			Params: map[string]any{},
		}); err != nil {
			http.Error(w, "demo patient-message: dispatch refused: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
}
