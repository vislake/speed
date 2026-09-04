package main

// notification_flow_test.go drives go/notification end to end through the
// composed HTTP stack: the authn+tenancy middleware chain, the module's
// real handler on its mounted route (demo_subject.go's
// demoRouteGuards names it routePublic -- the module resolves and requires
// its own caller identity per operation), a real temp-file SQLite database
// and the real standalone queue. Three legs cover the round's acceptance
// shape:
//
//   - the user-delivery leg: a note created by demoNotesCreatorUserID
//     publishes notes.note.created; demo_notification.go's subscription
//     dispatches it back to the creator; the module resolves the
//     creator's channels and addresses at send time (the email lands on
//     demoUserAddresses' address), the inbox row lands on the creator's
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
// posture server_test.go's testNote and org_flow_test.go's orgNode take.
// Captured messages are the only assertions on what went out: mails are
// read back from org_flow_test.go's capturingMailer and SMS from the
// locked buffer injected through cfg.SMSOutput (server.go defaults the
// writer to os.Stdout, so a test that wants to observe SMS must override
// it, exactly as cfg.Mailer overrides the mailer seam).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// notifCodePattern finds a 6-digit verification code inside a rendered
// code message. The template prints the code as a bare six digits (see
// go/notification/locales' notification.contact.verify_code copy), and a
// digit-run of another length -- an E.164 phone number, a uuid's hex --
// cannot match: the word boundaries require a run of exactly six digits
// between non-digits.
var notifCodePattern = regexp.MustCompile(`\b\d{6}\b`)

// buildNotifTestServer is notification_flow_test.go's server builder: the
// same composed handler buildTestServer wires (server_test.go), with the
// two transports this suite must observe pointed at test doubles --
// cfg.Mailer at a capturingMailer (the org flow test's double, defined in
// org_flow_test.go) and cfg.SMSOutput at a locked buffer (this file's
// double, below) -- so every message the module sends lands somewhere the
// test can read back instead of the console. cfg is returned alongside so
// the caller can reach cfg.Memberships the way registerAndAuthenticate
// expects.
func buildNotifTestServer(t *testing.T) (*httptest.Server, serverConfig, *capturingMailer, *lockedBuffer) {
	t.Helper()

	cfg := testConfig(t)
	mailer := &capturingMailer{}
	cfg.Mailer = mailer
	sms := &lockedBuffer{}
	cfg.SMSOutput = sms

	handler, cleanup, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
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
// console SMS sender's output (go/notification/sms.go writes one
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
	notifErrorBody struct {
		Code *string `json:"code"`
	}
)

// notifRequest issues method against srv.URL+path with a bearer token, the
// acting subject (the X-Demo-User-Id header demoOrgSubjectResolver reads;
// empty omits it) and an optional JSON body, and requires the response to
// carry wantStatus, decoding it into out (nil to skip decoding, for empty
// responses like the 204s and the demo route's 202). The envelope of every
// refusal decodes into a notifErrorBody through the same out slot.
func notifRequest(t *testing.T, srv *httptest.Server, method, path, token, subjectUserID string, body any, wantStatus int, out any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, srv.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if subjectUserID != "" {
		req.Header.Set(demoOrgUserHeader, subjectUserID)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != wantStatus {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s status = %d, want %d; body = %s", method, path, resp.StatusCode, wantStatus, respBody)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode %s %s response: %v", method, path, err)
		}
	}
}

// notifError issues a request whose response must carry the given error
// status, and returns the decoded envelope so the caller can pin the code.
func notifError(t *testing.T, srv *httptest.Server, method, path, token, subjectUserID string, body any, wantStatus int) notifErrorBody {
	t.Helper()
	var env notifErrorBody
	notifRequest(t, srv, method, path, token, subjectUserID, body, wantStatus, &env)
	if env.Code == nil {
		t.Fatalf("%s %s: error response carried no code", method, path)
	}
	return env
}

// eventually polls cond until it reports true or timeout passes, failing
// the test on timeout with what describing the waited-for condition. It
// is the determinism tool for worker-goroutine work: a dispatch's queue
// job runs in the background, so a test polls until the job's effect is
// visible instead of sleeping a guess.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

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
func noteIDByText(t *testing.T, notes []testNote, text string) string {
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

// TestNotificationFlow_NoteCreatedUserDelivery_EndToEnd drives the round's
// canonical user-recipient flow through the composed HTTP stack: creating
// a note publishes notes.note.created, demo_notification.go's
// subscription dispatches the same type back to the note's creator
// (demoNotesCreatorUserID), and the notification module delivers over the
// creator's resolved channels -- an inbox row on the creator's own message
// list and an email to the address demoUserAddresses holds for the creator
// -- with the delivery re-read at send time, never frozen into the event.
// The second half of the test drives the preference surface: each channel
// the creator switches off stops arriving (the email stops while the inbox
// keeps coming), and a full opt-out stops the type's deliveries entirely,
// because notes.note.created is Unsubscribable (the demo type's refusal
// leg lives in the external-contact test below).
func TestNotificationFlow_NoteCreatedUserDelivery_EndToEnd(t *testing.T) {
	srv, cfg, mailer, _ := buildNotifTestServer(t)
	token := registerAndAuthenticate(t, srv, cfg, demoSingleTenantID, "notif-owner")
	subject := demoNotesCreatorUserID
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
	eventually(t, 12*time.Second, "the note-created inbox message", func() bool {
		var out notifMessages
		notifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages", token, subject, nil, http.StatusOK, &out)
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
	eventually(t, 12*time.Second, "the note-created email", func() bool {
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
	notifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages/unread-count", token, subject, nil, http.StatusOK, &unread)
	if unread.Count != 1 {
		t.Fatalf("unread count after note 1 = %d, want 1", unread.Count)
	}
	notifRequest(t, srv, http.MethodPost, "/api/v1/notifications/messages/"+msg1.ID+"/read", token, subject, nil, http.StatusNoContent, nil)
	notifRequest(t, srv, http.MethodPost, "/api/v1/notifications/messages/"+msg1.ID+"/read", token, subject, nil, http.StatusNoContent, nil)
	notifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages/unread-count", token, subject, nil, http.StatusOK, &unread)
	if unread.Count != 0 {
		t.Fatalf("unread count after marking read = %d, want 0", unread.Count)
	}
	var out notifMessages
	notifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages", token, subject, nil, http.StatusOK, &out)
	msg1, _ = messageByNoteID(t, out.Items, note1ID)
	if msg1.ReadAt == nil {
		t.Errorf("read message read_at = nil, want the read timestamp")
	}

	// Switch the email channel off for the notes type. The preference
	// answer reports the reduced effective set immediately.
	var pref notifPreference
	notifRequest(t, srv, http.MethodPut, "/api/v1/notifications/preferences/"+noteTypeKey+"/email", token, subject,
		map[string]bool{"enabled": false}, http.StatusOK, &pref)
	if !equalStrings(pref.Channels, []string{"in_app", "sms"}) {
		t.Fatalf("channels after email opt-out = %v, want [in_app sms]", pref.Channels)
	}

	// A second note still reaches the inbox (the in-app channel is on) but
	// its email never goes out.
	const note2Text = "second note of the notification flow test"
	createNoteAs(t, srv, token, note2Text)
	note2ID := noteIDByText(t, listNotesAs(t, srv, token), note2Text)
	eventually(t, 12*time.Second, "the second note's inbox message", func() bool {
		var listed notifMessages
		notifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages", token, subject, nil, http.StatusOK, &listed)
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
	notifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages/unread-count", token, subject, nil, http.StatusOK, &unread)
	if unread.Count != 1 {
		t.Fatalf("unread count after note 2 = %d, want 1", unread.Count)
	}
	var readAll notifReadAll
	notifRequest(t, srv, http.MethodPost, "/api/v1/notifications/messages/read-all", token, subject, nil, http.StatusOK, &readAll)
	if readAll.ReadCount != 1 {
		t.Fatalf("read-all read_count = %d, want 1", readAll.ReadCount)
	}
	notifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages/unread-count", token, subject, nil, http.StatusOK, &unread)
	if unread.Count != 0 {
		t.Fatalf("unread count after read-all = %d, want 0", unread.Count)
	}

	// Opt out of the remaining channels too. notes.note.created is
	// Unsubscribable, so the empty set is a legal answer -- and a delivery
	// over no channels writes nothing and sends nothing: the third note
	// leaves the inbox at two rows and the mailer at one message.
	notifRequest(t, srv, http.MethodPut, "/api/v1/notifications/preferences/"+noteTypeKey+"/in_app", token, subject,
		map[string]bool{"enabled": false}, http.StatusOK, &pref)
	if !equalStrings(pref.Channels, []string{"sms"}) {
		t.Fatalf("channels after in_app opt-out = %v, want [sms]", pref.Channels)
	}
	notifRequest(t, srv, http.MethodPut, "/api/v1/notifications/preferences/"+noteTypeKey+"/sms", token, subject,
		map[string]bool{"enabled": false}, http.StatusOK, &pref)
	if len(pref.Channels) != 0 {
		t.Fatalf("channels after full opt-out = %v, want none", pref.Channels)
	}
	var prefs notifListPreferences
	notifRequest(t, srv, http.MethodGet, "/api/v1/notifications/preferences", token, subject, nil, http.StatusOK, &prefs)
	for _, row := range prefs.Items {
		if row.TypeKey == noteTypeKey && len(row.Channels) != 0 {
			t.Fatalf("stored preference for %s = %v, want the empty opt-out", noteTypeKey, row.Channels)
		}
	}

	const note3Text = "third note of the notification flow test"
	createNoteAs(t, srv, token, note3Text)
	never(t, 4*time.Second, "a delivery after the full opt-out", func() bool {
		var listed notifMessages
		notifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages", token, subject, nil, http.StatusOK, &listed)
		if len(listed.Items) != 2 {
			return true
		}
		return len(mailsTo(mailer, "user-creator-1@demo.example")) != 1
	})

	// The type directory answers with both declared types and their
	// unsubscribable flags -- the copy of the very distinction this test
	// leaned on (notes' type may be switched off entirely; the demo type
	// below may not, and its refusal leg lives in the next test).
	var types notifListTypes
	notifRequest(t, srv, http.MethodGet, "/api/v1/notifications/types", token, subject, nil, http.StatusOK, &types)
	byKey := make(map[string]notifType, len(types.Items))
	for _, item := range types.Items {
		byKey[item.TypeKey] = item
	}
	if len(types.Items) != 2 {
		t.Fatalf("type directory carries %d types, want the two this app declares", len(types.Items))
	}
	notesType, ok := byKey[noteTypeKey]
	if !ok || !notesType.Unsubscribable || !equalStrings(notesType.DefaultChannels, []string{"in_app", "email", "sms"}) {
		t.Errorf("notes type directory row = %+v, want unsubscribable with default_channels [in_app email sms]", notesType)
	}
	demoType, ok := byKey["demo.patient_reminder"]
	if !ok || demoType.Unsubscribable || !equalStrings(demoType.DefaultChannels, []string{"email", "sms"}) {
		t.Errorf("demo type directory row = %+v, want non-unsubscribable with default_channels [email sms]", demoType)
	}

	// The module's own identity gate: an authenticated caller without an
	// acting subject (no X-Demo-User-Id) is refused 401 by the module's
	// per-operation check -- the reason demoRouteGuards names this path
	// routePublic rather than gating it at the router.
	env := notifError(t, srv, http.MethodGet, "/api/v1/notifications/messages", token, "", nil, http.StatusUnauthorized)
	if *env.Code != "notification.subject_unresolved" {
		t.Errorf("subject-less message list code = %q, want notification.subject_unresolved", *env.Code)
	}
}

// TestNotificationFlow_ExternalContactDoubleOptIn_EndToEnd drives the
// external-recipient leg through the demo patient-message route
// (demo_notification.go): a contact joins the tenant's roster through
// double opt-in, and only a VERIFIED contact receives anything -- a
// dispatch to the still-pending contact is refused by the module's
// send-time gate and the queue dead-letters it after the bounded retry
// horizon with no message ever going out. Verification itself is
// synchronous (the code message is the module's one exception to
// event-driven delivery), which is why it can arrive before the response
// and is only re-read for its code here.
func TestNotificationFlow_ExternalContactDoubleOptIn_EndToEnd(t *testing.T) {
	srv, cfg, mailer, sms := buildNotifTestServer(t)
	token := registerAndAuthenticate(t, srv, cfg, demoSingleTenantID, "notif-clinic")
	subject := demoNotesCreatorUserID
	const contactEmail = "flow-patient@example.com"

	// Double opt-in, half one: the contact is created pending, and the
	// verification code arrives over its channel before the create even
	// answers (the module's synchronous exception).
	var contact notifContact
	notifRequest(t, srv, http.MethodPost, "/api/v1/notifications/contacts", token, subject,
		map[string]string{"channel": "email", "address": contactEmail}, http.StatusCreated, &contact)
	if contact.Status != "pending" || contact.Channel != "email" || contact.ID == "" {
		t.Fatalf("created contact = %+v, want a pending email contact with an id", contact)
	}
	var code string
	eventually(t, 5*time.Second, "the verification-code email", func() bool {
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
	notifRequest(t, srv, http.MethodPost, "/api/v1/demo/patient-message", token, subject,
		map[string]string{"contact_id": contact.ID}, http.StatusAccepted, nil)
	never(t, 15*time.Second, "a patient reminder to an unverified contact", func() bool {
		return len(mailsTo(mailer, contactEmail)) != 1
	})

	// Double opt-in, half two: the code verifies the contact.
	notifRequest(t, srv, http.MethodPost, "/api/v1/notifications/contacts/"+contact.ID+"/verify", token, subject,
		map[string]string{"code": code}, http.StatusOK, &contact)
	if contact.Status != "verified" {
		t.Fatalf("verified contact status = %q, want verified", contact.Status)
	}

	// The same dispatch now delivers: the reminder arrives over the
	// contact's channel, rendered from the demo type's copy -- which
	// carries no code-shaped digits, the wire-level difference between
	// the reminder and the verification message that preceded it.
	notifRequest(t, srv, http.MethodPost, "/api/v1/demo/patient-message", token, subject,
		map[string]string{"contact_id": contact.ID}, http.StatusAccepted, nil)
	eventually(t, 10*time.Second, "the patient reminder email", func() bool {
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
	notifRequest(t, srv, http.MethodPut, "/api/v1/notifications/preferences/demo.patient_reminder/email", token, subject,
		map[string]bool{"enabled": false}, http.StatusOK, nil)
	env := notifError(t, srv, http.MethodPut, "/api/v1/notifications/preferences/demo.patient_reminder/sms", token, subject,
		map[string]bool{"enabled": false}, http.StatusBadRequest)
	if *env.Code != "notification.preference_optout_not_allowed" {
		t.Errorf("closing the demo type's last channel code = %q, want notification.preference_optout_not_allowed", *env.Code)
	}

	// The SMS half of the external leg: a phone contact verifies over the
	// console sender's output and receives the reminder's sms copy the
	// same way the email contact received its mail.
	const contactPhone = "+8613800138000"
	notifRequest(t, srv, http.MethodPost, "/api/v1/notifications/contacts", token, subject,
		map[string]string{"channel": "sms", "address": contactPhone}, http.StatusCreated, &contact)
	if contact.Status != "pending" || contact.Channel != "sms" {
		t.Fatalf("created sms contact = %+v, want a pending sms contact", contact)
	}
	var smsCode string
	eventually(t, 5*time.Second, "the verification-code SMS", func() bool {
		lines := smsLinesTo(sms, contactPhone)
		if len(lines) != 1 {
			return false
		}
		smsCode = notifCodePattern.FindString(lines[0])
		return smsCode != ""
	})
	notifRequest(t, srv, http.MethodPost, "/api/v1/notifications/contacts/"+contact.ID+"/verify", token, subject,
		map[string]string{"code": smsCode}, http.StatusOK, &contact)
	if contact.Status != "verified" {
		t.Fatalf("verified sms contact status = %q, want verified", contact.Status)
	}
	notifRequest(t, srv, http.MethodPost, "/api/v1/demo/patient-message", token, subject,
		map[string]string{"contact_id": contact.ID}, http.StatusAccepted, nil)
	eventually(t, 10*time.Second, "the patient reminder SMS", func() bool {
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
	token := registerAndAuthenticate(t, srv, cfg, demoSingleTenantID, "notif-ratelimit")
	subject := demoNotesCreatorUserID
	const contactEmail = "rate-patient@example.com"

	var contact notifContact
	notifRequest(t, srv, http.MethodPost, "/api/v1/notifications/contacts", token, subject,
		map[string]string{"channel": "email", "address": contactEmail}, http.StatusCreated, &contact)

	var code string
	eventually(t, 5*time.Second, "the verification-code email", func() bool {
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
		env := notifError(t, srv, http.MethodPost, "/api/v1/notifications/contacts/"+contact.ID+"/verify", token, subject,
			map[string]string{"code": wrong}, http.StatusBadRequest)
		if *env.Code != "notification.contact_code_invalid" {
			t.Fatalf("wrong-code attempt %d code = %q, want notification.contact_code_invalid", i+1, *env.Code)
		}
	}

	// The eleventh attempt is the correct code -- and the budget check
	// refuses it before the code is ever compared: brute force fails
	// closed.
	env := notifError(t, srv, http.MethodPost, "/api/v1/notifications/contacts/"+contact.ID+"/verify", token, subject,
		map[string]string{"code": code}, http.StatusTooManyRequests)
	if *env.Code != "notification.contact_rate_limited" {
		t.Fatalf("eleventh-attempt code = %q, want notification.contact_rate_limited", *env.Code)
	}

	// The budget is the address's, not the code instance's: a resend
	// issues a fresh code, and verifying with it is still refused, because
	// the address has no guesses left within the code-lifetime window.
	notifRequest(t, srv, http.MethodPost, "/api/v1/notifications/contacts/"+contact.ID+"/resend", token, subject, nil, http.StatusNoContent, nil)
	eventually(t, 5*time.Second, "the resent verification-code email", func() bool {
		mails := mailsTo(mailer, contactEmail)
		if len(mails) != 2 {
			return false
		}
		return notifCodePattern.FindString(mails[1].Text) != ""
	})
	env = notifError(t, srv, http.MethodPost, "/api/v1/notifications/contacts/"+contact.ID+"/verify", token, subject,
		map[string]string{"code": notifCodePattern.FindString(mailsTo(mailer, contactEmail)[1].Text)}, http.StatusTooManyRequests)
	if *env.Code != "notification.contact_rate_limited" {
		t.Fatalf("fresh-code verify after exhaustion code = %q, want notification.contact_rate_limited", *env.Code)
	}
}
