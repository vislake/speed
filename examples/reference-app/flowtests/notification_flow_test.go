package flowtests

// notification_flow_test.go drives go/notification end to end through the
// composed HTTP stack: the authn+tenancy middleware chain, the module's
// real handler on its mounted route (internal/app/demo/demo_subject.go's
// DemoRouteRules declares it public -- the module resolves and requires
// its own caller identity per operation), a real temp-file SQLite database
// and the real standalone queue. Three legs cover the module's acceptance
// shape:
//
//   - the user-delivery leg: a note created by DemoNotesCreatorUserID
//     publishes notes.note.created; internal/app/demo/demo_notification.go's subscription
//     dispatches it back to the creator; the module resolves the
//     creator's channels and addresses at send time (the email lands on
//     DemoUserAddresses' address), the inbox row lands on the creator's
//     own message list, and preference opt-outs steer later deliveries --
//     down to a full opt-out that stops the type's deliveries entirely.
//
//   - the external-recipient leg: a contact joins the tenant's roster
//     through double opt-in (create -> code message -> verify), an
//     unverified contact's dispatch is refused and dead-letters after the
//     bounded retry horizon without a single message going out, and the
//     verified contact receives the demo type's reminder over its own
//     channel.
//
//   - the rate-limit leg: verify attempts pay a per-address budget before
//     the code is even checked, so the tenth wrong guess is a 400 and the
//     eleventh -- with the correct code -- is a 429: brute force fails
//     closed.
//
// The assertions are deliberately wire-shaped (decode by JSON field name,
// never by importing go/notification/api's generated types), the same
// posture server_test.go's testutil.TestNote and org_flow_test.go's orgNode take.
// Captured messages are the only assertions on what went out: mails are
// read back from org_flow_test.go's capturingMailer and SMS from the
// locked buffer injected through cfg.SMSOutput (internal/app/server.go defaults the
// writer to os.Stdout, so a test that wants to observe SMS must override
// it, exactly as cfg.Mailer overrides the mailer seam).

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/examples/reference-app/internal/apptest"
	"github.com/vislake/speed/examples/reference-app/internal/testutil"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/examples/reference-app/internal/app/demo"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/testkit"
)

// notifCodePattern finds a 6-digit verification code inside a rendered
// code message. The template prints the code as a bare six digits (see
// go/notification/locales' notification.contact.verify_code copy), and a
// digit-run of another length -- an E.164 phone number, a uuid's hex --
// cannot match: the word boundaries require a run of exactly six digits
// between non-digits.
var notifCodePattern = regexp.MustCompile(`\b\d{6}\b`)

// buildNotifTestServer is notification_flow_test.go's server builder: the
// same composed handler apptest.BuildServer wires (server_test.go), with the
// two transports this suite must observe pointed at test doubles --
// cfg.Mailer at a capturingMailer (the org flow test's double, defined in
// org_flow_test.go) and cfg.SMSOutput at a locked buffer (this file's
// double, below) -- so every message the module sends lands somewhere the
// test can read back instead of the console. cfg is returned alongside so
// the caller can reach cfg.Memberships the way apptest.RegisterAndAuthenticate
// expects.
func buildNotifTestServer(t *testing.T) (*httptest.Server, app.ServerConfig, *capturingMailer, *lockedBuffer) {
	t.Helper()

	cfg := apptest.ServerConfig(t)
	mailer := &capturingMailer{}
	cfg.Mailer = mailer
	sms := &lockedBuffer{}
	cfg.SMSOutput = sms

	handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, cfg, mailer, sms
}

// snapshot returns a copy of every message captured so far. A test must
// read through this rather than touching m.sent directly: deliveries run
// on the standalone queue's worker goroutine, and an unlocked read of
// m.sent from the test goroutine would race the worker's locked append
// under -race.
func (m *capturingMailer) snapshot() []pkgcore.Mail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]pkgcore.Mail(nil), m.sent...)
}

// lockedBuffer is an io.Writer that keeps every write, for observing the
// console SMS sender's output (go/pkgcore/sms_console.go writes one
// "SMS to <address>: <text>" line per message). The lock exists for the
// same reason snapshot() does: verification-code SMS are sent
// synchronously from a handler goroutine, delivery SMS from a queue
// worker, and both write here.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write implements io.Writer.
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// text returns everything written so far.
func (b *lockedBuffer) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// smsLinesTo returns the recorded "SMS to <address>" lines for address.
func smsLinesTo(b *lockedBuffer, address string) []string {
	var lines []string
	for _, line := range strings.Split(b.text(), "\n") {
		if strings.HasPrefix(line, "SMS to "+address+":") {
			lines = append(lines, line)
		}
	}
	return lines
}

// The wire shapes this suite decodes, field-named after the JSON the
// module's generated handler actually serves.
type (
	notifMessage struct {
		ID      string         `json:"id"`
		TypeKey string         `json:"type_key"`
		Group   string         `json:"group"`
		Title   string         `json:"title"`
		Body    string         `json:"body"`
		Params  map[string]any `json:"params"`
		ReadAt  *string        `json:"read_at"`
	}
	notifMessages struct {
		Items []notifMessage `json:"items"`
	}
	notifUnreadCount struct {
		Count int `json:"count"`
	}
	notifReadAll struct {
		ReadCount int `json:"read_count"`
	}
	notifContact struct {
		ID      string `json:"id"`
		Channel string `json:"channel"`
		Status  string `json:"status"`
	}
	notifPreference struct {
		TypeKey  string   `json:"type_key"`
		Channels []string `json:"channels"`
	}
	notifListPreferences struct {
		Items []notifPreference `json:"items"`
	}
	notifType struct {
		TypeKey         string   `json:"type_key"`
		Group           string   `json:"group"`
		DefaultChannels []string `json:"default_channels"`
		Unsubscribable  bool     `json:"unsubscribable"`
	}
	notifListTypes struct {
		Items []notifType `json:"items"`
	}
)

// never asserts that cond stays false for the whole window, polling on the
// same tick eventually uses. It is the negative-window tool: the thing
// that must NOT happen (a message that must not go out) is asserted absent
// across the window its delivery could plausibly appear in.
func never(t *testing.T, window time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if cond() {
			t.Fatalf("%s within %s", what, window)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// mailsTo returns the captured mails addressed to address.
func mailsTo(mailer *capturingMailer, address string) []pkgcore.Mail {
	var out []pkgcore.Mail
	for _, mail := range mailer.snapshot() {
		for _, to := range mail.To {
			if to == address {
				out = append(out, mail)
			}
		}
	}
	return out
}

// noteIDByText finds the note whose text is text in a listNotesAs answer.
func noteIDByText(t *testing.T, notes []testutil.TestNote, text string) string {
	t.Helper()
	for _, note := range notes {
		if note.Text == text {
			return note.ID
		}
	}
	t.Fatalf("no note with text %q in the listing", text)
	return ""
}

// messageByNoteID finds the inbox message whose params name noteID.
func messageByNoteID(t *testing.T, items []notifMessage, noteID string) (notifMessage, bool) {
	t.Helper()
	for _, item := range items {
		if item.Params["note_id"] == noteID {
			return item, true
		}
	}
	return notifMessage{}, false
}

// equalStrings is the equality assertion the channel-list checks use.
func equalStrings(got, want []string) bool {
	return reflect.DeepEqual(got, want)
}

// TestNotificationFlow_NoteCreatedUserDelivery_EndToEnd drives the
// canonical user-recipient flow through the composed HTTP stack: creating
// a note publishes notes.note.created, internal/app/demo/demo_notification.go's
// subscription dispatches the same type back to the note's creator
// (DemoNotesCreatorUserID), and the notification module delivers over the
// creator's resolved channels -- an inbox row on the creator's own message
// list and an email to the address DemoUserAddresses holds for the creator
// -- with the delivery re-read at send time, never frozen into the event.
// The second half of the test drives the preference surface: each channel
// the creator switches off stops arriving (the email stops while the inbox
// keeps coming), and a full opt-out stops the type's deliveries entirely,
// because notes.note.created is Unsubscribable (the demo type's refusal
// leg lives in the external-contact test below).
func TestNotificationFlow_NoteCreatedUserDelivery_EndToEnd(t *testing.T) {
	srv, cfg, mailer, _ := buildNotifTestServer(t)
	token := apptest.RegisterAndAuthenticate(t, srv, cfg, demo.DemoSingleTenantID, "notif-owner")
	subject := demo.DemoNotesCreatorUserID
	const noteTypeKey = "notes.note.created"

	// The first note: its creation publishes the event whose dispatch this
	// leg is really about.
	const note1Text = "first note of the notification flow test"
	createNoteAs(t, srv, token, note1Text)
	note1ID := noteIDByText(t, listNotesAs(t, srv, token), note1Text)

	// The dispatch lands in the creator's inbox. Poll rather than sleep:
	// the subscription enqueues a delivery job the standalone queue runs
	// in the background.
	var msg1 notifMessage
	testkit.EventuallyWithin(t, 12*time.Second, "the note-created inbox message", func() bool {
		var out notifMessages
		testutil.NotifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages", token, subject, nil, http.StatusOK, &out)
		if len(out.Items) != 1 {
			return false
		}
		msg, ok := messageByNoteID(t, out.Items, note1ID)
		msg1 = msg
		return ok && msg.TypeKey == noteTypeKey && msg.Title != "" && msg.Body != ""
	})
	if msg1.Group != "collaboration" {
		t.Errorf("message group = %q, want the notes type's group", msg1.Group)
	}
	if msg1.ReadAt != nil {
		t.Errorf("fresh message read_at = %v, want unread", *msg1.ReadAt)
	}

	// The same dispatch delivers the email channel to the address the
	// demo resolver holds for the creator, with the note id interpolated
	// into the rendered copy.
	testkit.EventuallyWithin(t, 12*time.Second, "the note-created email", func() bool {
		for _, mail := range mailsTo(mailer, "user-creator-1@demo.example") {
			if strings.Contains(mail.Text, note1ID) {
				return true
			}
		}
		return false
	})

	// The read surface: one unread message, markable read individually
	// (twice -- the second call is the idempotent replay), after which the
	// unread count is zero and the row carries its read time.
	var unread notifUnreadCount
	testutil.NotifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages/unread-count", token, subject, nil, http.StatusOK, &unread)
	if unread.Count != 1 {
		t.Fatalf("unread count after note 1 = %d, want 1", unread.Count)
	}
	testutil.NotifRequest(t, srv, http.MethodPost, "/api/v1/notifications/messages/"+msg1.ID+"/read", token, subject, nil, http.StatusNoContent, nil)
	testutil.NotifRequest(t, srv, http.MethodPost, "/api/v1/notifications/messages/"+msg1.ID+"/read", token, subject, nil, http.StatusNoContent, nil)
	testutil.NotifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages/unread-count", token, subject, nil, http.StatusOK, &unread)
	if unread.Count != 0 {
		t.Fatalf("unread count after marking read = %d, want 0", unread.Count)
	}
	var out notifMessages
	testutil.NotifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages", token, subject, nil, http.StatusOK, &out)
	msg1, _ = messageByNoteID(t, out.Items, note1ID)
	if msg1.ReadAt == nil {
		t.Errorf("read message read_at = nil, want the read timestamp")
	}

	// Switch the email channel off for the notes type. The preference
	// answer reports the reduced effective set immediately.
	var pref notifPreference
	testutil.NotifRequest(t, srv, http.MethodPut, "/api/v1/notifications/preferences/"+noteTypeKey+"/email", token, subject,
		map[string]bool{"enabled": false}, http.StatusOK, &pref)
	if !equalStrings(pref.Channels, []string{"in_app", "sms"}) {
		t.Fatalf("channels after email opt-out = %v, want [in_app sms]", pref.Channels)
	}

	// A second note still reaches the inbox (the in-app channel is on) but
	// its email never goes out.
	const note2Text = "second note of the notification flow test"
	createNoteAs(t, srv, token, note2Text)
	note2ID := noteIDByText(t, listNotesAs(t, srv, token), note2Text)
	testkit.EventuallyWithin(t, 12*time.Second, "the second note's inbox message", func() bool {
		var listed notifMessages
		testutil.NotifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages", token, subject, nil, http.StatusOK, &listed)
		if len(listed.Items) != 2 {
			return false
		}
		_, ok := messageByNoteID(t, listed.Items, note2ID)
		return ok
	})
	never(t, 3*time.Second, "an email for the opted-out channel", func() bool {
		return len(mailsTo(mailer, "user-creator-1@demo.example")) != 1
	})

	// The second note's row is the one unread message; read-all clears it.
	testutil.NotifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages/unread-count", token, subject, nil, http.StatusOK, &unread)
	if unread.Count != 1 {
		t.Fatalf("unread count after note 2 = %d, want 1", unread.Count)
	}
	var readAll notifReadAll
	testutil.NotifRequest(t, srv, http.MethodPost, "/api/v1/notifications/messages/read-all", token, subject, nil, http.StatusOK, &readAll)
	if readAll.ReadCount != 1 {
		t.Fatalf("read-all read_count = %d, want 1", readAll.ReadCount)
	}
	testutil.NotifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages/unread-count", token, subject, nil, http.StatusOK, &unread)
	if unread.Count != 0 {
		t.Fatalf("unread count after read-all = %d, want 0", unread.Count)
	}

	// Opt out of the remaining channels too. notes.note.created is
	// Unsubscribable, so the empty set is a legal answer -- and a delivery
	// over no channels writes nothing and sends nothing: the third note
	// leaves the inbox at two rows and the mailer at one message.
	testutil.NotifRequest(t, srv, http.MethodPut, "/api/v1/notifications/preferences/"+noteTypeKey+"/in_app", token, subject,
		map[string]bool{"enabled": false}, http.StatusOK, &pref)
	if !equalStrings(pref.Channels, []string{"sms"}) {
		t.Fatalf("channels after in_app opt-out = %v, want [sms]", pref.Channels)
	}
	testutil.NotifRequest(t, srv, http.MethodPut, "/api/v1/notifications/preferences/"+noteTypeKey+"/sms", token, subject,
		map[string]bool{"enabled": false}, http.StatusOK, &pref)
	if len(pref.Channels) != 0 {
		t.Fatalf("channels after full opt-out = %v, want none", pref.Channels)
	}
	var prefs notifListPreferences
	testutil.NotifRequest(t, srv, http.MethodGet, "/api/v1/notifications/preferences", token, subject, nil, http.StatusOK, &prefs)
	for _, row := range prefs.Items {
		if row.TypeKey == noteTypeKey && len(row.Channels) != 0 {
			t.Fatalf("stored preference for %s = %v, want the empty opt-out", noteTypeKey, row.Channels)
		}
	}

	const note3Text = "third note of the notification flow test"
	createNoteAs(t, srv, token, note3Text)
	never(t, 4*time.Second, "a delivery after the full opt-out", func() bool {
		var listed notifMessages
		testutil.NotifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages", token, subject, nil, http.StatusOK, &listed)
		if len(listed.Items) != 2 {
			return true
		}
		return len(mailsTo(mailer, "user-creator-1@demo.example")) != 1
	})

	// The type directory answers with every declared type and its
	// unsubscribable flags -- the copy of the very distinction this test
	// leaned on (notes' type may be switched off entirely; the
	// demo.patient_reminder type below may not, and its refusal leg lives
	// in the next test). Four types answer: go/admin registers its
	// own admin.impersonation_started security notification alongside
	// notes' and demo's two (see internal/app/demo/demo_admin.go's wiring in internal/app/server.go, and
	// demo's own module.go for demo.simulation_ready, the smilesim
	// completion notification internal/app/demo/demo_notification.go's own
	// EventSimulationCompleted subscription dispatches).
	var types notifListTypes
	testutil.NotifRequest(t, srv, http.MethodGet, "/api/v1/notifications/types", token, subject, nil, http.StatusOK, &types)
	byKey := make(map[string]notifType, len(types.Items))
	for _, item := range types.Items {
		byKey[item.TypeKey] = item
	}
	if len(types.Items) != 4 {
		t.Fatalf("type directory carries %d types, want the four this app declares", len(types.Items))
	}
	notesType, ok := byKey[noteTypeKey]
	if !ok || !notesType.Unsubscribable || !equalStrings(notesType.DefaultChannels, []string{"in_app", "email", "sms"}) {
		t.Errorf("notes type directory row = %+v, want unsubscribable with default_channels [in_app email sms]", notesType)
	}
	demoType, ok := byKey["demo.patient_reminder"]
	if !ok || demoType.Unsubscribable || !equalStrings(demoType.DefaultChannels, []string{"email", "sms"}) {
		t.Errorf("demo type directory row = %+v, want non-unsubscribable with default_channels [email sms]", demoType)
	}
	adminType, ok := byKey["admin.impersonation_started"]
	if !ok || adminType.Unsubscribable || !equalStrings(adminType.DefaultChannels, []string{"in_app", "email"}) {
		t.Errorf("admin impersonation-started type directory row = %+v, want non-unsubscribable with default_channels [in_app email]", adminType)
	}
	simulationReadyType, ok := byKey["demo.simulation_ready"]
	if !ok || !simulationReadyType.Unsubscribable || !equalStrings(simulationReadyType.DefaultChannels, []string{"sms"}) {
		t.Errorf("simulation-ready type directory row = %+v, want unsubscribable with default_channels [sms]", simulationReadyType)
	}

	// The module's own identity gate: an authenticated caller without an
	// acting subject (no X-Demo-User-Id) is refused 401 by the module's
	// per-operation check -- the reason DemoRouteRules declares this path
	// public rather than gating it at the router.
	env := testutil.NotifError(t, srv, http.MethodGet, "/api/v1/notifications/messages", token, "", nil, http.StatusUnauthorized)
	if *env.Code != "notification.subject_unresolved" {
		t.Errorf("subject-less message list code = %q, want notification.subject_unresolved", *env.Code)
	}
}

// TestNotificationFlow_ExternalContactDoubleOptIn_EndToEnd drives the
// external-recipient leg through the demo patient-message route
// (internal/app/demo/demo_notification.go): a contact joins the tenant's roster through
// double opt-in, and only a VERIFIED contact receives anything -- a
// dispatch to the still-pending contact is refused by the module's
// send-time gate and the queue dead-letters it after the bounded retry
// horizon with no message ever going out. Verification itself is
// synchronous (the code message is the module's one exception to
// event-driven delivery), which is why it can arrive before the response
// and is only re-read for its code here.
func TestNotificationFlow_ExternalContactDoubleOptIn_EndToEnd(t *testing.T) {
	srv, cfg, mailer, sms := buildNotifTestServer(t)
	token := apptest.RegisterAndAuthenticate(t, srv, cfg, demo.DemoSingleTenantID, "notif-clinic")
	subject := demo.DemoNotesCreatorUserID
	const contactEmail = "flow-patient@example.com"

	// Double opt-in, half one: the contact is created pending, and the
	// verification code arrives over its channel before the create even
	// answers (the module's synchronous exception).
	var contact notifContact
	testutil.NotifRequest(t, srv, http.MethodPost, "/api/v1/notifications/contacts", token, subject,
		map[string]string{"channel": "email", "address": contactEmail}, http.StatusCreated, &contact)
	if contact.Status != "pending" || contact.Channel != "email" || contact.ID == "" {
		t.Fatalf("created contact = %+v, want a pending email contact with an id", contact)
	}
	var code string
	testkit.EventuallyWithin(t, 5*time.Second, "the verification-code email", func() bool {
		mails := mailsTo(mailer, contactEmail)
		if len(mails) != 1 {
			return false
		}
		code = notifCodePattern.FindString(mails[0].Text)
		return code != ""
	})

	// A dispatch to the still-pending contact is enqueued (202) and then
	// refused at send time by the module's own gate: the queue retries
	// with bounded backoff and dead-letters, and across the whole horizon
	// no message goes out. The window covers the full retry schedule (the
	// fourth attempt lands ~7s in) with margin for a slow worker.
	testutil.NotifRequest(t, srv, http.MethodPost, "/api/v1/demo/patient-message", token, subject,
		map[string]string{"contact_id": contact.ID}, http.StatusAccepted, nil)
	never(t, 15*time.Second, "a patient reminder to an unverified contact", func() bool {
		return len(mailsTo(mailer, contactEmail)) != 1
	})

	// Double opt-in, half two: the code verifies the contact.
	testutil.NotifRequest(t, srv, http.MethodPost, "/api/v1/notifications/contacts/"+contact.ID+"/verify", token, subject,
		map[string]string{"code": code}, http.StatusOK, &contact)
	if contact.Status != "verified" {
		t.Fatalf("verified contact status = %q, want verified", contact.Status)
	}

	// The same dispatch now delivers: the reminder arrives over the
	// contact's channel, rendered from the demo type's copy -- which
	// carries no code-shaped digits, the wire-level difference between
	// the reminder and the verification message that preceded it.
	testutil.NotifRequest(t, srv, http.MethodPost, "/api/v1/demo/patient-message", token, subject,
		map[string]string{"contact_id": contact.ID}, http.StatusAccepted, nil)
	testkit.EventuallyWithin(t, 10*time.Second, "the patient reminder email", func() bool {
		mails := mailsTo(mailer, contactEmail)
		if len(mails) != 2 {
			return false
		}
		return notifCodePattern.FindString(mails[1].Text) == ""
	})
	never(t, 3*time.Second, "a duplicated reminder delivery", func() bool {
		return len(mailsTo(mailer, contactEmail)) != 2
	})

	// The demo type is not Unsubscribable: closing its second channel is
	// refused, where the notes type's full opt-out above was accepted --
	// the contract difference this app exists to demonstrate end to end.
	testutil.NotifRequest(t, srv, http.MethodPut, "/api/v1/notifications/preferences/demo.patient_reminder/email", token, subject,
		map[string]bool{"enabled": false}, http.StatusOK, nil)
	env := testutil.NotifError(t, srv, http.MethodPut, "/api/v1/notifications/preferences/demo.patient_reminder/sms", token, subject,
		map[string]bool{"enabled": false}, http.StatusBadRequest)
	if *env.Code != "notification.preference_optout_not_allowed" {
		t.Errorf("closing the demo type's last channel code = %q, want notification.preference_optout_not_allowed", *env.Code)
	}

	// The SMS half of the external leg: a phone contact verifies over the
	// console sender's output and receives the reminder's sms copy the
	// same way the email contact received its mail.
	const contactPhone = "+8613800138000"
	testutil.NotifRequest(t, srv, http.MethodPost, "/api/v1/notifications/contacts", token, subject,
		map[string]string{"channel": "sms", "address": contactPhone}, http.StatusCreated, &contact)
	if contact.Status != "pending" || contact.Channel != "sms" {
		t.Fatalf("created sms contact = %+v, want a pending sms contact", contact)
	}
	var smsCode string
	testkit.EventuallyWithin(t, 5*time.Second, "the verification-code SMS", func() bool {
		lines := smsLinesTo(sms, contactPhone)
		if len(lines) != 1 {
			return false
		}
		smsCode = notifCodePattern.FindString(lines[0])
		return smsCode != ""
	})
	testutil.NotifRequest(t, srv, http.MethodPost, "/api/v1/notifications/contacts/"+contact.ID+"/verify", token, subject,
		map[string]string{"code": smsCode}, http.StatusOK, &contact)
	if contact.Status != "verified" {
		t.Fatalf("verified sms contact status = %q, want verified", contact.Status)
	}
	testutil.NotifRequest(t, srv, http.MethodPost, "/api/v1/demo/patient-message", token, subject,
		map[string]string{"contact_id": contact.ID}, http.StatusAccepted, nil)
	testkit.EventuallyWithin(t, 10*time.Second, "the patient reminder SMS", func() bool {
		lines := smsLinesTo(sms, contactPhone)
		if len(lines) != 2 {
			return false
		}
		return notifCodePattern.FindString(lines[1]) == ""
	})
}

// TestNotificationFlow_VerifyCodeRateLimit_FailsClosed pins the brute-force
// bound of a 6-digit code at the HTTP surface: verify attempts pay the
// per-address budget BEFORE the code is checked, so ten wrong guesses are
// ten 400s that each burn budget, and the eleventh attempt -- carrying the
// correct code -- is refused 429 rather than honored. The refusal is not
// per-code-instance either: a resend issues a fresh code and the budget is
// still exhausted, because the budget is the address's guess allowance,
// not the code's.
func TestNotificationFlow_VerifyCodeRateLimit_FailsClosed(t *testing.T) {
	srv, cfg, mailer, _ := buildNotifTestServer(t)
	token := apptest.RegisterAndAuthenticate(t, srv, cfg, demo.DemoSingleTenantID, "notif-ratelimit")
	subject := demo.DemoNotesCreatorUserID
	const contactEmail = "rate-patient@example.com"

	var contact notifContact
	testutil.NotifRequest(t, srv, http.MethodPost, "/api/v1/notifications/contacts", token, subject,
		map[string]string{"channel": "email", "address": contactEmail}, http.StatusCreated, &contact)

	var code string
	testkit.EventuallyWithin(t, 5*time.Second, "the verification-code email", func() bool {
		mails := mailsTo(mailer, contactEmail)
		if len(mails) != 1 {
			return false
		}
		code = notifCodePattern.FindString(mails[0].Text)
		return code != ""
	})

	// Ten wrong guesses: every attempt is a 400 -- the code is wrong --
	// and every attempt spends one of the address's ten guesses. Mangle
	// the last digit so the wrong code can never coincide with the real
	// one.
	last := code[len(code)-1]
	wrong := code[:len(code)-1] + string(byte('0'+(last-'0'+1)%10))
	for i := 0; i < 10; i++ {
		env := testutil.NotifError(t, srv, http.MethodPost, "/api/v1/notifications/contacts/"+contact.ID+"/verify", token, subject,
			map[string]string{"code": wrong}, http.StatusBadRequest)
		if *env.Code != "notification.contact_code_invalid" {
			t.Fatalf("wrong-code attempt %d code = %q, want notification.contact_code_invalid", i+1, *env.Code)
		}
	}

	// The eleventh attempt is the correct code -- and the budget check
	// refuses it before the code is ever compared: brute force fails
	// closed.
	env := testutil.NotifError(t, srv, http.MethodPost, "/api/v1/notifications/contacts/"+contact.ID+"/verify", token, subject,
		map[string]string{"code": code}, http.StatusTooManyRequests)
	if *env.Code != "notification.contact_rate_limited" {
		t.Fatalf("eleventh-attempt code = %q, want notification.contact_rate_limited", *env.Code)
	}

	// The budget is the address's, not the code instance's: a resend
	// issues a fresh code, and verifying with it is still refused, because
	// the address has no guesses left within the code-lifetime window.
	testutil.NotifRequest(t, srv, http.MethodPost, "/api/v1/notifications/contacts/"+contact.ID+"/resend", token, subject, nil, http.StatusNoContent, nil)
	testkit.EventuallyWithin(t, 5*time.Second, "the resent verification-code email", func() bool {
		mails := mailsTo(mailer, contactEmail)
		if len(mails) != 2 {
			return false
		}
		return notifCodePattern.FindString(mails[1].Text) != ""
	})
	env = testutil.NotifError(t, srv, http.MethodPost, "/api/v1/notifications/contacts/"+contact.ID+"/verify", token, subject,
		map[string]string{"code": notifCodePattern.FindString(mailsTo(mailer, contactEmail)[1].Text)}, http.StatusTooManyRequests)
	if *env.Code != "notification.contact_rate_limited" {
		t.Fatalf("fresh-code verify after exhaustion code = %q, want notification.contact_rate_limited", *env.Code)
	}
}
