package notification

// handler_test.go drives Handler (handler.go) over its own mux, the way a
// host's composed HTTP stack reaches it: every request goes through
// ServeHTTP against a fresh fully-seamed handler (the real repository,
// services and hub, a fixture type taxonomy, and a fixed subject seam),
// with the tenant injected the way tenancy.Middleware would inject it.
// What the handler adds on top of the service layer is what these tests
// pin: the route surface and its one shared gate (tenant then subject,
// every one of the twelve routes), the API response shapes and error
// envelopes, the request-language negotiation of the type directory, and
// the hand-mounted stream route's framing, filtering and close behaviour.
//
// The services' own suites (repository_test.go, preference_service_test.go,
// contact_test.go, hub_test.go) pin the layer beneath; this file reaches
// the same code through HTTP and asserts the surface contracts, and does
// not re-test service semantics except where the handler itself reasons
// about them (page validation, the update-preference channel check that
// runs before the body is read, and the stream route's own filtering).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/datatypes"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/notification/api"
	"github.com/vislake/speed/go/pkgcore"
)

// handlerTenant, handlerOtherTenant and handlerUser are the fixed identity
// every request in this file carries: the tenant injected into the request
// context (handlerTenant, or handlerOtherTenant where a test needs a second
// tenant's rows), and the user id the subject seam answers. handlerUser
// deliberately equals repository_test.go's testMessage recipient, so rows
// seeded with testMessage belong to the HTTP caller by construction.
const (
	handlerTenant      = "tenant-acme"
	handlerOtherTenant = "tenant-bella"
	handlerUser        = "user-7"
)

// fixedSubject is a SubjectResolver that answers a fixed identity, the
// stand-in for a host's seam in a composed stack. ok=false or an empty id
// reproduce the seam's failure shapes (nil is reproduced by assigning
// handler.subject = nil directly).
type fixedSubject struct {
	userID string
	ok     bool
}

func (s fixedSubject) Subject(*http.Request) (string, bool) { return s.userID, s.ok }

var _ SubjectResolver = fixedSubject{}

// handlerRoute is one method-plus-path pair the handler serves.
type handlerRoute struct {
	name   string
	method string
	path   string
}

// handlerRoutes enumerates the whole HTTP surface handler.go serves -- the
// fragment's eleven spec operations plus the hand-mounted inbox stream
// route -- with representative path parameters. The two shared gates
// (mustTenant then resolveSubject) run before any body is read or any
// service is called, so the table doubles as the enumeration those gate
// tests sweep.
var handlerRoutes = []handlerRoute{
	{"list contacts", http.MethodGet, apiPath + "/contacts"},
	{"create contact", http.MethodPost, apiPath + "/contacts"},
	{"resend contact code", http.MethodPost, apiPath + "/contacts/contact-1/resend"},
	{"verify contact", http.MethodPost, apiPath + "/contacts/contact-1/verify"},
	{"list messages", http.MethodGet, apiPath + "/messages"},
	{"unread count", http.MethodGet, apiPath + "/messages/unread-count"},
	{"read all", http.MethodPost, apiPath + "/messages/read-all"},
	{"mark message read", http.MethodPost, apiPath + "/messages/message-1/read"},
	{"list preferences", http.MethodGet, apiPath + "/preferences"},
	{"update preference", http.MethodPut, apiPath + "/preferences/clinic.appointment_reminder/email"},
	{"list types", http.MethodGet, apiPath + "/types"},
	{"stream", http.MethodGet, apiPath + "/stream"},
}

// handlerEnv is one fully seam-ful handler: a fresh migrated database
// shared by the repository and the two services (the shape Module's
// NewModule builds -- module.go constructs all three over one db), the
// fixture type taxonomy attached to the preference service, the recording
// host (real notification locale bundle, recording bus and mailer), a
// console SMS sender over smsBuf, a hub, and a Handler whose subject seam
// answers handlerUser. Each test starts empty: no inbox rows, no contact
// ledger rows, no rate-limit budget spent.
type handlerEnv struct {
	host     *testHost
	smsBuf   *bytes.Buffer
	inbox    *Repository
	prefs    *PreferenceService
	contacts *ContactService
	hub      *Hub
	h        *Handler
}

// newHandlerEnv builds a handlerEnv (see the struct comment for the shape).
func newHandlerEnv(t *testing.T) *handlerEnv {
	t.Helper()
	registerContactSerializer()
	env := &handlerEnv{
		host:   newTestHost(t),
		smsBuf: new(bytes.Buffer),
		hub:    NewHub(),
	}
	reg := pkgcore.NewRegistry(env.host.bus, env.host.kv, env.host.mailer)
	if err := reg.AuditActions.Add(contactAuditActionDecls...); err != nil {
		t.Fatalf("register the contact audit actions: %v", err)
	}
	db := newTestDB(t)
	env.inbox = NewRepository(db)
	env.prefs = NewPreferenceService(db)
	env.prefs.attachTypes(fixtureRegistrar{types: fixtureTypes})
	env.contacts = NewContactService(db)
	env.contacts.sms = NewConsoleSMSSender(env.smsBuf)
	env.contacts.mailFrom = testMailFrom
	env.contacts.emailIndexer = testEmailIndexer(t)
	env.contacts.phoneIndexer = testPhoneIndexer(t)
	env.contacts.host = env.host
	env.contacts.audit = reg.AuditActions
	env.h = NewHandler(env.inbox, env.prefs, env.contacts, env.hub, fixedSubject{userID: handlerUser, ok: true})
	env.h.host = env.host
	return env
}

// insertMessage stores one inbox row under tenant through the same
// repository the delivery pipeline writes, so a test's fixture rows exist
// exactly as delivered messages do.
func (e *handlerEnv) insertMessage(t *testing.T, tenant string, msg *InboxMessage) {
	t.Helper()
	if err := e.inbox.Create(tenantCtx(tenant), msg); err != nil {
		t.Fatalf("insert inbox row %s: %v", msg.ID, err)
	}
}

// pinnedRow returns a message for handlerUser with a deterministic CreatedAt
// (base minus minutesAgo minutes), so list ordering assertions never depend
// on wall-clock timing.
func pinnedRow(id string, minutesAgo int, group string) *InboxMessage {
	return messageAt(id, handlerUser, group, time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC).Add(-time.Duration(minutesAgo)*time.Minute))
}

// mustMessageWithRecipient returns testMessage's shape with the recipient
// replaced -- a row that belongs to another recipient of the same tenant,
// the negative fixture the list and mark-read isolation assertions need.
func mustMessageWithRecipient(id, recipient string) *InboxMessage {
	msg := testMessage(id)
	msg.RecipientUserID = recipient
	return msg
}

// do runs one request against the handler's mux with the tenant of
// handlerTenant injected into the context -- the state every request has
// after the host's tenancy middleware ran. body, when non-nil, is sent as
// the JSON request body.
func (e *handlerEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return e.doCtx(t, tenantCtx(handlerTenant), method, path, body, nil)
}

// doCtx is do with the request context, headers and body supplied by the
// caller -- the escape hatch tenant-less and language-negotiating tests
// need.
func (e *handlerEnv) doCtx(t *testing.T, ctx context.Context, method, path string, body any, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body %v: %v", body, err)
		}
		bodyReader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", jsonContentType)
	}
	for key, value := range header {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

// decodeJSONBody decodes rec's body into a fresh T, failing the test on any
// decode error.
func decodeJSONBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q into %T: %v", rec.Body.String(), out, err)
	}
	return out
}

// assertEnvelope fails t unless rec carries wantStatus and a NotificationError
// envelope whose code is wantCode; it returns the envelope's params (empty
// when the envelope carries none) so a caller can assert them.
func assertEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) map[string]interface{} {
	t.Helper()
	if rec.Code != wantStatus {
		t.Errorf("status = %d, want %d (body %s)", rec.Code, wantStatus, rec.Body.String())
	}
	var env api.NotificationError
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope from %q: %v", rec.Body.String(), err)
	}
	if env.Code == nil {
		t.Fatalf("envelope carries no code: %q", rec.Body.String())
	}
	if *env.Code != wantCode {
		t.Errorf("code = %s, want %s", *env.Code, wantCode)
	}
	if env.Params == nil {
		return map[string]interface{}{}
	}
	return *env.Params
}

// assertNoAddressLeak fails t if rec's decoded JSON tree contains an
// "address" or "address_index" key at any depth -- the PII guarantee the
// contact surface makes (the spec serves id, channel, status and
// timestamps only; toContactResponse strips the rest). The structural
// check walks the raw tree so a future spec field that reintroduces an
// address key fails here by construction, whatever shape it takes.
func assertNoAddressLeak(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var root interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &root); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	var walk func(any)
	walk = func(node any) {
		switch node := node.(type) {
		case map[string]interface{}:
			for key, child := range node {
				if key == "address" || key == "address_index" {
					t.Errorf("response leaks the %q key: %s", key, rec.Body.String())
				}
				walk(child)
			}
		case []interface{}:
			for _, child := range node {
				walk(child)
			}
		}
	}
	walk(root)
}

// waitUntil polls cond until it holds or five seconds pass, failing the
// test on timeout -- the synchronisation primitive the stream tests use to
// wait for frames the handler's own goroutine writes.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestHandler_UnwiredSubject_FailsEveryRouteClosed proves the shared gate:
// with the subject seam unwired (nil -- the state before a host wires its
// identity layer, or a host that never does), every one of the twelve
// routes the handler serves answers the same 401
// (notification.subject_unresolved), on a request that is otherwise fully
// valid. The module never invents a caller: no default user, no empty
// identity, no operation served anonymously. The routes table doubles as
// the enumeration of the whole HTTP surface.
func TestHandler_UnwiredSubject_FailsEveryRouteClosed(t *testing.T) {
	env := newHandlerEnv(t)
	env.h.subject = nil
	for _, route := range handlerRoutes {
		t.Run(route.name, func(t *testing.T) {
			rec := env.do(t, route.method, route.path, nil)
			assertEnvelope(t, rec, http.StatusUnauthorized, "notification.subject_unresolved")
		})
	}
}

// TestHandler_TenantlessRequest_FailsEveryRouteClosed proves the other half
// of the gate: a request that reaches the handler without a tenant in its
// context -- the host's tenancy middleware misconfigured or missing -- is a
// host wiring error, and every route refuses it as an internal failure
// (notification.internal_error) rather than inventing a tenant or serving
// the request into one guessed from the request itself.
func TestHandler_TenantlessRequest_FailsEveryRouteClosed(t *testing.T) {
	env := newHandlerEnv(t)
	for _, route := range handlerRoutes {
		t.Run(route.name, func(t *testing.T) {
			rec := env.doCtx(t, context.Background(), route.method, route.path, nil, nil)
			assertEnvelope(t, rec, http.StatusInternalServerError, "notification.internal_error")
		})
	}
}

// TestHandler_SubjectSeam_FailingOrEmpty_AnswersTheSameRefusal pins the
// remaining subject-seam failure shapes: a resolver that answers ok=false
// and one that answers an empty user id are the same anonymous caller as
// an unwired seam, and GET /messages (one representative route) refuses
// both identically -- the seam's whole contract is "ok=true and a non-empty
// id, or the refusal".
func TestHandler_SubjectSeam_FailingOrEmpty_AnswersTheSameRefusal(t *testing.T) {
	env := newHandlerEnv(t)

	env.h.subject = fixedSubject{userID: handlerUser, ok: false}
	rec := env.do(t, http.MethodGet, apiPath+"/messages", nil)
	assertEnvelope(t, rec, http.StatusUnauthorized, "notification.subject_unresolved")

	env.h.subject = fixedSubject{userID: "", ok: true}
	rec = env.do(t, http.MethodGet, apiPath+"/messages", nil)
	assertEnvelope(t, rec, http.StatusUnauthorized, "notification.subject_unresolved")
}

// TestHandler_ListMessages_NewestFirst_OwnRowsOnly drives the plain inbox
// read: with no query at all the handler answers the caller's own rows,
// newest first, with every row field rendered as the spec promises --
// rendered text, the interpolation params as an object (the empty map for a
// row that was delivered without params, never null), timestamps, the
// group, and the type key. Rows of another recipient and rows of another
// tenant never appear: the tenant comes from the request context, the
// recipient from the subject seam, and the repository's own tenant filter
// is the one guard between them and the caller.
func TestHandler_ListMessages_NewestFirst_OwnRowsOnly(t *testing.T) {
	env := newHandlerEnv(t)
	env.insertMessage(t, handlerTenant, pinnedRow("m-new", 1, "appointments"))
	env.insertMessage(t, handlerTenant, pinnedRow("m-mid", 5, "results"))
	env.insertMessage(t, handlerTenant, pinnedRow("m-old", 40, "security"))
	env.insertMessage(t, handlerTenant, mustMessageWithRecipient("m-other-user", "user-8"))
	env.insertMessage(t, handlerOtherTenant, pinnedRow("m-foreign-tenant", 10, "appointments"))

	rec := env.do(t, http.MethodGet, apiPath+"/messages", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	resp := decodeJSONBody[api.NotificationListMessagesResponse](t, rec)
	if len(resp.Items) != 3 {
		t.Fatalf("items = %d, want only the caller's three rows (%s)", len(resp.Items), rec.Body.String())
	}
	wantIDs := []string{"m-new", "m-mid", "m-old"}
	for i, want := range wantIDs {
		if resp.Items[i].ID != want {
			t.Errorf("items[%d].id = %s, want %s (newest first)", i, resp.Items[i].ID, want)
		}
	}
	mid := resp.Items[1]
	if mid.TypeKey != "note.shared" || mid.Group != "results" {
		t.Errorf("mid row type_key/group = %s/%s, want note.shared/results", mid.TypeKey, mid.Group)
	}
	if mid.Title != "Note 42 was shared with you" || mid.Body != "Lin opened Note 42 and shared it with the whole clinic." {
		t.Errorf("mid row renders %q / %q, want the seeded title and body", mid.Title, mid.Body)
	}
	if mid.ReadAt != nil {
		t.Errorf("fresh row read_at = %v, want null", *mid.ReadAt)
	}
	if mid.Params == nil {
		t.Errorf("params = nil, want an object (the empty map when the row has none)")
	}
	if len(mid.Params) != 0 {
		t.Errorf("params = %v, want the empty object", mid.Params)
	}
	if mid.Link != "" || mid.ExpiryAt != nil {
		t.Errorf("row link/expiry_at = %q/%v, want the seeded empty link and nil expiry", mid.Link, mid.ExpiryAt)
	}
}

// TestHandler_ListMessages_SeededParamsSurviveTheTrip pins the params leg in
// the other direction: a row delivered with a real interpolation map comes
// back with exactly that map, values included -- the map a client renders
// its own rich copy from.
func TestHandler_ListMessages_SeededParamsSurviveTheTrip(t *testing.T) {
	env := newHandlerEnv(t)
	msg := testMessage("m-params")
	msg.Params = datatypes.JSON(`{"patient_name":"Wang Fang","clinic_id":"clinic-1"}`)
	env.insertMessage(t, handlerTenant, msg)

	rec := env.do(t, http.MethodGet, apiPath+"/messages", nil)
	resp := decodeJSONBody[api.NotificationListMessagesResponse](t, rec)
	if len(resp.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(resp.Items))
	}
	params := resp.Items[0].Params
	if params["patient_name"] != "Wang Fang" || params["clinic_id"] != "clinic-1" {
		t.Errorf("params = %v, want the seeded interpolation map", params)
	}
}

// TestHandler_ListMessages_PagesNewestFirst_AcrossStablePages proves the
// paging contract: a limit smaller than the row count answers the newest
// rows first, and the next offset answers the rows after them -- the same
// order a full listing would give, page boundaries never reordering rows.
func TestHandler_ListMessages_PagesNewestFirst_AcrossStablePages(t *testing.T) {
	env := newHandlerEnv(t)
	for _, row := range []*InboxMessage{
		pinnedRow("m1", 5, "appointments"),
		pinnedRow("m2", 10, "appointments"),
		pinnedRow("m3", 15, "appointments"),
		pinnedRow("m4", 20, "results"),
	} {
		env.insertMessage(t, handlerTenant, row)
	}

	rec := env.do(t, http.MethodGet, apiPath+"/messages?limit=2", nil)
	page1 := decodeJSONBody[api.NotificationListMessagesResponse](t, rec)
	if len(page1.Items) != 2 || page1.Items[0].ID != "m1" || page1.Items[1].ID != "m2" {
		t.Fatalf("first page = %v, want [m1 m2]", responseIDs(page1.Items))
	}

	rec = env.do(t, http.MethodGet, apiPath+"/messages?limit=2&offset=2", nil)
	page2 := decodeJSONBody[api.NotificationListMessagesResponse](t, rec)
	if len(page2.Items) != 2 || page2.Items[0].ID != "m3" || page2.Items[1].ID != "m4" {
		t.Fatalf("second page = %v, want [m3 m4]", responseIDs(page2.Items))
	}

	rec = env.do(t, http.MethodGet, apiPath+"/messages?limit=2&offset=4", nil)
	empty := decodeJSONBody[api.NotificationListMessagesResponse](t, rec)
	if len(empty.Items) != 0 {
		t.Fatalf("page past the end = %v, want none", responseIDs(empty.Items))
	}
}

// responseIDs is the row-id projection of a list response, the paging
// assertions' comparison form. (repository_test.go owns the same-name
// projection of []InboxMessage; the two slices cannot share one helper.)
func responseIDs(items []api.NotificationMessage) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

// TestHandler_ListMessages_GroupFilter_RestrictsThePage proves the optional
// group parameter: one group's rows only, newest first, the other group's
// rows invisible -- the same predicate the delivery pipeline stamped at
// delivery time (the type's group), applied at read time.
func TestHandler_ListMessages_GroupFilter_RestrictsThePage(t *testing.T) {
	env := newHandlerEnv(t)
	env.insertMessage(t, handlerTenant, pinnedRow("m-appt-1", 5, "appointments"))
	env.insertMessage(t, handlerTenant, pinnedRow("m-appt-2", 15, "appointments"))
	env.insertMessage(t, handlerTenant, pinnedRow("m-sec-1", 10, "security"))

	rec := env.do(t, http.MethodGet, apiPath+"/messages?group=appointments", nil)
	resp := decodeJSONBody[api.NotificationListMessagesResponse](t, rec)
	if len(resp.Items) != 2 || resp.Items[0].ID != "m-appt-1" || resp.Items[1].ID != "m-appt-2" {
		t.Fatalf("appointments page = %v, want [m-appt-1 m-appt-2]", responseIDs(resp.Items))
	}
}

// TestHandler_ListMessages_PageParamsOutOfRange_RefusedWhole pins the
// page-bounding guard: a limit below 1 or above 200 and a negative offset
// are each refused whole with notification.invalid_list_params naming the
// offending parameter -- the spec's "never silently truncated" promise,
// enforced before any repository call.
func TestHandler_ListMessages_PageParamsOutOfRange_RefusedWhole(t *testing.T) {
	env := newHandlerEnv(t)
	for _, tc := range []struct {
		query string
		field string
		value string
	}{
		{"limit=0", "limit", "0"},
		{"limit=201", "limit", "201"},
		{"offset=-1", "offset", "-1"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			rec := env.do(t, http.MethodGet, apiPath+"/messages?"+tc.query, nil)
			params := assertEnvelope(t, rec, http.StatusBadRequest, "notification.invalid_list_params")
			if params["field"] != tc.field || params["value"] != tc.value {
				t.Errorf("params = %v, want field=%s value=%s", params, tc.field, tc.value)
			}
		})
	}
}

// TestHandler_UnreadCount_GovernedByExpiry drives the unread predicate the
// way delivery and the spec intend it: expiry governs what counts as
// unread, never what the inbox lists. An expired row still lists -- the
// recipient may still open it -- but it stops counting; a live unread row
// counts; a read row never counts again.
func TestHandler_UnreadCount_GovernedByExpiry(t *testing.T) {
	env := newHandlerEnv(t)
	now := time.Now()

	live := testMessage("m-live")
	later := now.Add(24 * time.Hour)
	live.ExpiryAt = &later
	env.insertMessage(t, handlerTenant, live)

	expired := testMessage("m-expired")
	earlier := now.Add(-24 * time.Hour)
	expired.ExpiryAt = &earlier
	env.insertMessage(t, handlerTenant, expired)

	read := testMessage("m-read")
	read.ExpiryAt = &later
	env.insertMessage(t, handlerTenant, read)
	env.do(t, http.MethodPost, apiPath+"/messages/"+read.ID+"/read", nil)

	// Both expired and live rows list.
	rec := env.do(t, http.MethodGet, apiPath+"/messages", nil)
	resp := decodeJSONBody[api.NotificationListMessagesResponse](t, rec)
	if len(resp.Items) != 3 {
		t.Fatalf("items = %d, want all three rows listed (expiry never hides a row)", len(resp.Items))
	}

	// Only the live unread row counts.
	rec = env.do(t, http.MethodGet, apiPath+"/messages/unread-count", nil)
	count := decodeJSONBody[api.NotificationUnreadCountResponse](t, rec)
	if count.Count != 1 {
		t.Errorf("unread count = %d, want 1 (the live unread row only)", count.Count)
	}
}

// TestHandler_MarkMessageRead_AnswersAndRepeats pins the mark-read
// contract: a first read of the caller's own message answers 204 and the
// row's next listing carries read_at; a second read answers 204 exactly
// like the first (the spec's idempotence promise -- no 404 for a row that
// is already in the state the call asks for); and a message id outside the
// caller's own inbox -- another recipient's row, another tenant's row, or
// nothing at all -- answers one 404 (notification.message_not_found) with
// the id named, never an existence-disclosing distinction.
func TestHandler_MarkMessageRead_AnswersAndRepeats(t *testing.T) {
	env := newHandlerEnv(t)
	env.insertMessage(t, handlerTenant, testMessage("m-mine"))
	env.insertMessage(t, handlerTenant, mustMessageWithRecipient("m-others", "user-8"))
	env.insertMessage(t, handlerOtherTenant, testMessage("m-theirs"))

	rec := env.do(t, http.MethodPost, apiPath+"/messages/m-mine/read", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("first read status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodPost, apiPath+"/messages/m-mine/read", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("second read status = %d, want 204 (idempotent)", rec.Code)
	}

	listed := env.do(t, http.MethodGet, apiPath+"/messages", nil)
	resp := decodeJSONBody[api.NotificationListMessagesResponse](t, listed)
	if len(resp.Items) != 1 || resp.Items[0].ReadAt == nil {
		t.Fatalf("read row = %+v, want it listed with read_at set", resp.Items)
	}

	for _, id := range []string{"m-others", "m-theirs", "m-ghost"} {
		t.Run(id, func(t *testing.T) {
			rec := env.do(t, http.MethodPost, apiPath+"/messages/"+id+"/read", nil)
			params := assertEnvelope(t, rec, http.StatusNotFound, "notification.message_not_found")
			if params["message_id"] != id {
				t.Errorf("params = %v, want message_id=%s", params, id)
			}
		})
	}
}

// TestHandler_ReadAll_AnswersTheFlippedCountThenZero drives the
// mark-all-read answer: one call flips every unread row -- however old, and
// whatever its expiry -- and answers how many it flipped; the very next
// call answers zero, the flip never counted twice.
func TestHandler_ReadAll_AnswersTheFlippedCountThenZero(t *testing.T) {
	env := newHandlerEnv(t)
	env.insertMessage(t, handlerTenant, testMessage("m1"))
	env.insertMessage(t, handlerTenant, testMessage("m2"))
	env.insertMessage(t, handlerTenant, testMessage("m3"))

	rec := env.do(t, http.MethodPost, apiPath+"/messages/read-all", nil)
	first := decodeJSONBody[api.NotificationReadAllResponse](t, rec)
	if first.ReadCount != 3 {
		t.Fatalf("first read_count = %d, want 3", first.ReadCount)
	}
	rec = env.do(t, http.MethodPost, apiPath+"/messages/read-all", nil)
	second := decodeJSONBody[api.NotificationReadAllResponse](t, rec)
	if second.ReadCount != 0 {
		t.Errorf("second read_count = %d, want 0", second.ReadCount)
	}
}

// TestHandler_ListPreferences_OneRowPerDeclaredType proves the preference
// listing: one row per declared type in declaration order -- never only the
// types the caller has touched -- with the caller's effective channels
// merged against the type's defaults (a type never written stays at its
// defaults, listed in the type's own canonical order).
func TestHandler_ListPreferences_OneRowPerDeclaredType(t *testing.T) {
	env := newHandlerEnv(t)

	rec := env.do(t, http.MethodGet, apiPath+"/preferences", nil)
	resp := decodeJSONBody[api.NotificationListPreferencesResponse](t, rec)
	if len(resp.Items) != 3 {
		t.Fatalf("items = %d, want one row per declared type (%s)", len(resp.Items), rec.Body.String())
	}
	wantKeys := []string{"clinic.appointment_reminder", "clinic.result_ready", "clinic.security_alert"}
	for i, key := range wantKeys {
		if resp.Items[i].TypeKey != key {
			t.Fatalf("items[%d].type_key = %s, want %s (declaration order)", i, resp.Items[i].TypeKey, key)
		}
	}
	appointment := []string{"in_app", "email", "sms"}
	if !slicesEqual(resp.Items[0].Channels, appointment) {
		t.Errorf("appointment channels = %v, want the declared defaults %v", resp.Items[0].Channels, appointment)
	}
	result := []string{"in_app", "email"}
	if !slicesEqual(resp.Items[1].Channels, result) {
		t.Errorf("result channels = %v, want the declared defaults %v", resp.Items[1].Channels, result)
	}
}

// slicesEqual is the order-sensitive string-slice comparison the preference
// assertions need (channels always travel in the canonical channel order).
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHandler_UpdatePreference_ToggleLifecycle drives one type through the
// whole toggle lifecycle over HTTP: an opt-out narrows the effective
// channels and survives the next listing; a no-op opt-out of a channel
// already off answers the same 200 with channels unchanged; switching a
// channel back on restores it; and a fully unsubscribable type may be
// narrowed to silence (channels [] -- the empty array, never null).
func TestHandler_UpdatePreference_ToggleLifecycle(t *testing.T) {
	env := newHandlerEnv(t)
	base := apiPath + "/preferences/clinic.appointment_reminder/"

	// Opt out of email: appointment [in_app email sms] -> [in_app sms].
	rec := env.do(t, http.MethodPut, base+"email", map[string]bool{"enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("opt-out status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	rec = env.do(t, http.MethodGet, apiPath+"/preferences", nil)
	resp := decodeJSONBody[api.NotificationListPreferencesResponse](t, rec)
	if !slicesEqual(resp.Items[0].Channels, []string{"in_app", "sms"}) {
		t.Errorf("after email opt-out channels = %v, want [in_app sms]", resp.Items[0].Channels)
	}

	// No-op: email is already off.
	rec = env.do(t, http.MethodPut, base+"email", map[string]bool{"enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("no-op status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// Turn email back on: [in_app sms] -> [in_app email sms], canonical order.
	rec = env.do(t, http.MethodPut, base+"email", map[string]bool{"enabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("re-enable status = %d, want 200", rec.Code)
	}
	rec = env.do(t, http.MethodGet, apiPath+"/preferences", nil)
	resp = decodeJSONBody[api.NotificationListPreferencesResponse](t, rec)
	if !slicesEqual(resp.Items[0].Channels, []string{"in_app", "email", "sms"}) {
		t.Errorf("after re-enable channels = %v, want [in_app email sms]", resp.Items[0].Channels)
	}

	// Narrow to silence: the type is unsubscribable, so channels [] is legal.
	for _, channel := range []string{"email", "sms", "in_app"} {
		rec = env.do(t, http.MethodPut, base+channel, map[string]bool{"enabled": false})
		if rec.Code != http.StatusOK {
			t.Fatalf("opt-out of %s status = %d, want 200 (body %s)", channel, rec.Code, rec.Body.String())
		}
	}
	rec = env.do(t, http.MethodGet, apiPath+"/preferences", nil)
	resp = decodeJSONBody[api.NotificationListPreferencesResponse](t, rec)
	if resp.Items[0].Channels == nil || len(resp.Items[0].Channels) != 0 {
		t.Errorf("fully opted-out channels = %v, want the empty array", resp.Items[0].Channels)
	}
}

// TestHandler_UpdatePreference_OptOutRefusedOnGuaranteedType pins the
// guarantee the security fixture declares (Unsubscribable false): the type
// may be narrowed to a single channel but never to silence -- the last
// opt-out answers 400 (notification.preference_optout_not_allowed) naming
// the type, whatever came before.
func TestHandler_UpdatePreference_OptOutRefusedOnGuaranteedType(t *testing.T) {
	env := newHandlerEnv(t)
	base := apiPath + "/preferences/clinic.security_alert/"

	for _, channel := range []string{"email", "sms"} {
		rec := env.do(t, http.MethodPut, base+channel, map[string]bool{"enabled": false})
		if rec.Code != http.StatusOK {
			t.Fatalf("opt-out of %s status = %d, want 200 (body %s)", channel, rec.Code, rec.Body.String())
		}
	}
	rec := env.do(t, http.MethodPut, base+"in_app", map[string]bool{"enabled": false})
	params := assertEnvelope(t, rec, http.StatusBadRequest, "notification.preference_optout_not_allowed")
	if params["type_key"] != "clinic.security_alert" {
		t.Errorf("params = %v, want type_key=clinic.security_alert", params)
	}

	// The narrowing before the refusal still holds: email and sms are off,
	// and the refusal protected the one channel that remained.
	rec = env.do(t, http.MethodGet, apiPath+"/preferences", nil)
	resp := decodeJSONBody[api.NotificationListPreferencesResponse](t, rec)
	if !slicesEqual(resp.Items[2].Channels, []string{"in_app"}) {
		t.Errorf("security channels = %v, want [in_app] after the refused opt-out", resp.Items[2].Channels)
	}
}

// TestHandler_UpdatePreference_UnknownType_NotFound pins the stale-client
// refusal: a type key no module declared answers 404
// (notification.type_not_found) with the key named -- never a silent no-op
// that would let a client believe its channel state exists.
func TestHandler_UpdatePreference_UnknownType_NotFound(t *testing.T) {
	env := newHandlerEnv(t)

	rec := env.do(t, http.MethodPut, apiPath+"/preferences/clinic.ghost/email", map[string]bool{"enabled": false})
	params := assertEnvelope(t, rec, http.StatusNotFound, "notification.type_not_found")
	if params["type_key"] != "clinic.ghost" {
		t.Errorf("params = %v, want type_key=clinic.ghost", params)
	}
}

// TestHandler_UpdatePreference_UnknownChannel_RefusedBeforeBodyRead proves
// the channel vocabulary is closed before the request body is even read:
// a channel outside in_app/email/sms answers 400
// (notification.preference_invalid_channels) naming the type and the
// offending channel -- and it does so even on a request whose body would
// not decode, so the check's position is pinned by behaviour, not by
// reading handler code.
func TestHandler_UpdatePreference_UnknownChannel_RefusedBeforeBodyRead(t *testing.T) {
	env := newHandlerEnv(t)

	rec := env.do(t, http.MethodPut, apiPath+"/preferences/clinic.appointment_reminder/carrier_pigeon", map[string]bool{"enabled": true})
	params := assertEnvelope(t, rec, http.StatusBadRequest, "notification.preference_invalid_channels")
	if params["type_key"] != "clinic.appointment_reminder" || params["channels"] != "carrier_pigeon" {
		t.Errorf("params = %v, want type_key and channels=carrier_pigeon", params)
	}

	rec = env.do(t, http.MethodPut, apiPath+"/preferences/clinic.appointment_reminder/carrier_pigeon", "{{{not json")
	params = assertEnvelope(t, rec, http.StatusBadRequest, "notification.preference_invalid_channels")
	if params["channels"] != "carrier_pigeon" {
		t.Errorf("params = %v, want the channel check to precede the body decode", params)
	}
}

// TestHandler_UpdatePreference_MalformedBody_Refused pins the transport
// failure: a known channel with an undecodable body answers 400
// (notification.invalid_request_body) -- the channel vocabulary passed, the
// body did not.
func TestHandler_UpdatePreference_MalformedBody_Refused(t *testing.T) {
	env := newHandlerEnv(t)

	rec := env.do(t, http.MethodPut, apiPath+"/preferences/clinic.appointment_reminder/email", "{{{not json")
	assertEnvelope(t, rec, http.StatusBadRequest, "notification.invalid_request_body")
}

// TestHandler_ListTypes_DirectoryInDeclarationOrder pins the type
// directory's shape: every declared type, in declaration order, with the
// type's own fields -- its group, its declared default channels in
// declaration order, its opt-out permissiveness -- and its description
// rendered from the declaring module's locale bundle (here the fixture
// clinic bundle, standing in for the host's merged catalog) in the
// request's negotiated language, zh-CN when the request names none.
func TestHandler_ListTypes_DirectoryInDeclarationOrder(t *testing.T) {
	env := newHandlerEnv(t)
	env.host.catalog = testClinicCatalog(t)

	rec := env.do(t, http.MethodGet, apiPath+"/types", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	resp := decodeJSONBody[api.NotificationListTypesResponse](t, rec)
	if len(resp.Items) != 3 {
		t.Fatalf("items = %d, want the three declared types (%s)", len(resp.Items), rec.Body.String())
	}
	wantKeys := []string{"clinic.appointment_reminder", "clinic.result_ready", "clinic.security_alert"}
	for i, key := range wantKeys {
		item := resp.Items[i]
		if item.TypeKey != key {
			t.Fatalf("items[%d].type_key = %s, want %s (declaration order)", i, item.TypeKey, key)
		}
		switch i {
		case 0:
			if item.Group != "appointments" || item.Unsubscribable != true ||
				!slicesEqual(item.DefaultChannels, []string{"in_app", "email", "sms"}) {
				t.Errorf("appointment row = %+v, want the appointment fixture's declaration", item)
			}
			if item.Description != "在您的预约时间临近时发送的就诊提醒。" {
				t.Errorf("appointment description = %q, want the zh-CN copy", item.Description)
			}
		case 1:
			if item.Group != "results" || !slicesEqual(item.DefaultChannels, []string{"in_app", "email"}) {
				t.Errorf("result row = %+v, want the result fixture's declaration", item)
			}
			if item.Description != "在检查结果就绪时通知您。" {
				t.Errorf("result description = %q, want the zh-CN copy", item.Description)
			}
		case 2:
			if item.Group != "security" || item.Unsubscribable != false {
				t.Errorf("security row = %+v, want the guaranteed fixture's declaration", item)
			}
			if item.Description != "与您账户安全相关的重要通知。" {
				t.Errorf("security description = %q, want the zh-CN copy", item.Description)
			}
		}
	}
}

// TestHandler_ListTypes_NegotiatesTheDescriptionLanguage drives the
// directory's language negotiation the way a browser would: the request's
// Accept-Language header picks the description language, a language the
// catalog does not ship falls back to the platform default zh-CN (the
// catalog's own no-cross-language rule: the fallback is the declared
// default, never "any other language"), and a quality-0 preference is
// skipped, not honoured.
func TestHandler_ListTypes_NegotiatesTheDescriptionLanguage(t *testing.T) {
	env := newHandlerEnv(t)
	env.host.catalog = testClinicCatalog(t)

	const zh = "在您的预约时间临近时发送的就诊提醒。"
	const en = "An appointment reminder sent as your visit approaches."
	for _, tc := range []struct {
		name   string
		header string
		want   string
	}{
		{"no header", "", zh},
		{"exact en-US", "en-US", en},
		{"bare en prefix", "en", en},
		{"zh-CN explicit", "zh-CN", zh},
		{"unsupported language", "fr-FR", zh},
		{"quality-zero preference skipped", "en;q=0, fr-FR", zh},
		{"preferred list", "fr-FR, en;q=0.8", en},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var header map[string]string
			if tc.header != "" {
				header = map[string]string{"Accept-Language": tc.header}
			}
			rec := env.doCtx(t, tenantCtx(handlerTenant), http.MethodGet, apiPath+"/types", nil, header)
			resp := decodeJSONBody[api.NotificationListTypesResponse](t, rec)
			if len(resp.Items) == 0 || resp.Items[0].Description != tc.want {
				t.Errorf("description = %q, want %q", resp.Items[0].Description, tc.want)
			}
		})
	}
}

// TestHandler_ListTypes_MissingDescription_FailsTheWholeListing pins the
// directory's strict convention: a declared type whose bundle ships no
// "<type_key>.description" id (here a ghost type declared after the clinic
// bundle was built) fails the whole listing with notification.internal_error
// -- never a partial directory with the type's row missing or its
// description empty, which a client would render as a hole it cannot tell
// apart from an empty description.
func TestHandler_ListTypes_MissingDescription_FailsTheWholeListing(t *testing.T) {
	env := newHandlerEnv(t)
	env.host.catalog = testClinicCatalog(t)
	ghost := pkgcore.NotificationType{
		Key:             "clinic.ghost_alert",
		Group:           "security",
		DefaultChannels: []string{ChannelInApp, ChannelEmail},
		Unsubscribable:  false,
	}
	env.prefs.attachTypes(fixtureRegistrar{types: append(slices.Clone(fixtureTypes), ghost)})

	rec := env.do(t, http.MethodGet, apiPath+"/types", nil)
	assertEnvelope(t, rec, http.StatusInternalServerError, "notification.internal_error")
	if strings.Contains(rec.Body.String(), "clinic.appointment_reminder") {
		t.Errorf("the failure leaked a partial directory: %s", rec.Body.String())
	}
}

// TestHandler_ListTypes_UnwiredCatalog_AnswersInternal pins the wiring
// half: a handler whose host installed no catalog (the state before a host
// calls Kernel.Bootstrap with this module attached) has nothing honest to
// render -- every description would be missing -- so the directory fails
// with the module's internal error rather than guessing.
func TestHandler_ListTypes_UnwiredCatalog_AnswersInternal(t *testing.T) {
	env := newHandlerEnv(t)

	rec := env.do(t, http.MethodGet, apiPath+"/types", nil)
	assertEnvelope(t, rec, http.StatusInternalServerError, "notification.internal_error")
}

// auditEvents returns every recorded audit event under action, in order --
// the handler-env mirror of contact_test.go's recordedAudit, which is bound
// to that file's own env type.
func (e *handlerEnv) auditEvents(t *testing.T, action string) []audit.RecordedEvent {
	t.Helper()
	var out []audit.RecordedEvent
	for _, evt := range e.host.bus.events(audit.EventRecorded) {
		rec, ok := evt.Payload.(audit.RecordedEvent)
		if !ok {
			t.Fatalf("audit event payload is %T, want audit.RecordedEvent", evt.Payload)
		}
		if rec.Action == action {
			out = append(out, rec)
		}
	}
	return out
}

// mailCodeAt returns the verification code of the i-th delivered mail (0
// based, in delivery order) -- the handler-env mirror of contact_test.go's
// emailCodeAt, which is bound to that file's own env type.
func (e *handlerEnv) mailCodeAt(t *testing.T, i int) string {
	t.Helper()
	mails := e.host.mailer.messages()
	if i >= len(mails) {
		t.Fatalf("want the %d-th mail, only %d delivered", i, len(mails))
	}
	return lastCode(t, mails[i].Text)
}

// TestHandler_CreateContact_SMS_DoubleOptInFlow drives one SMS contact
// through the whole double opt-in over HTTP: creation answers 201 with a
// pending row (id, channel, status and timestamps -- never the address),
// the verification code arrives over the console SMS sender, a wrong code
// is one refusal, and the right code answers 200 -- after which the roster
// shows the row verified, the state the deliver path requires before it
// ever sends to the address. The verify call's own 200 body is not
// asserted on status (the service returns the pre-transition snapshot by
// contract); the roster after the fact is the authoritative state.
func TestHandler_CreateContact_SMS_DoubleOptInFlow(t *testing.T) {
	env := newHandlerEnv(t)

	rec := env.do(t, http.MethodPost, apiPath+"/contacts", map[string]string{
		"channel": "sms",
		"address": testPhone,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	assertNoAddressLeak(t, rec)
	created := decodeJSONBody[api.NotificationContact](t, rec)
	if created.ID == "" || created.Channel != "sms" || created.Status != "pending" {
		t.Fatalf("created contact = %+v, want an sms row pending", created)
	}
	// A double-opt-in create attests nothing: the attested record belongs to
	// the business-attested leg alone (CreateContact's ConsentRef path),
	// which contact_test.go pins the same way. The flow's audit record is
	// the verified event the code below lands.
	if got := env.auditEvents(t, AuditActionContactAttested); len(got) != 0 {
		t.Errorf("double-opt-in creation recorded %d attested events, want none", len(got))
	}

	code := smsCodeAt(t, env.smsBuf, 0)

	rec = env.do(t, http.MethodPost, apiPath+"/contacts/"+created.ID+"/verify", map[string]string{"code": wrongCode(code)})
	assertEnvelope(t, rec, http.StatusBadRequest, "notification.contact_code_invalid")

	rec = env.do(t, http.MethodPost, apiPath+"/contacts/"+created.ID+"/verify", map[string]string{"code": code})
	if rec.Code != http.StatusOK {
		t.Fatalf("verify status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	rec = env.do(t, http.MethodGet, apiPath+"/contacts", nil)
	list := decodeJSONBody[api.NotificationListContactsResponse](t, rec)
	if len(list.Items) != 1 || list.Items[0].ID != created.ID || list.Items[0].Status != "verified" {
		t.Fatalf("roster = %+v, want the one row verified", list.Items)
	}
	if got := env.auditEvents(t, AuditActionContactVerified); len(got) != 1 {
		t.Errorf("verification recorded %d notification.contact.verified events, want 1", len(got))
	}
}

// TestHandler_CreateContact_Email_CodeByMail drives the email leg of the
// same flow: the code travels in the mail the recording host mailer
// received, and verifying with it proves the address.
func TestHandler_CreateContact_Email_CodeByMail(t *testing.T) {
	env := newHandlerEnv(t)

	rec := env.do(t, http.MethodPost, apiPath+"/contacts", map[string]string{
		"channel": "email",
		"address": "alice@example.com",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	created := decodeJSONBody[api.NotificationContact](t, rec)
	code := env.mailCodeAt(t, 0)

	rec = env.do(t, http.MethodPost, apiPath+"/contacts/"+created.ID+"/verify", map[string]string{"code": code})
	if rec.Code != http.StatusOK {
		t.Fatalf("verify status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestHandler_VerifyContact_UnknownId_NotFound pins the ledger's refusal
// surface over HTTP: a verify naming an id no row of the tenant holds
// answers 404 (notification.contact_not_found) -- nothing discloses
// whether a contact ever existed.
func TestHandler_VerifyContact_UnknownId_NotFound(t *testing.T) {
	env := newHandlerEnv(t)

	rec := env.do(t, http.MethodPost, apiPath+"/contacts/contact-ghost/verify", map[string]string{"code": "000000"})
	assertEnvelope(t, rec, http.StatusNotFound, "notification.contact_not_found")
}

// TestHandler_VerifyContact_CodeBudgetExhausted_Refused pins the fail-closed
// verify limit over HTTP: enough wrong-code attempts against one address
// answer the budget's denial (429 notification.contact_rate_limited) with
// the verify dimension named and a retry-after hint -- the same closed
// behaviour the send limit has, on the guessing path a code probe opens.
func TestHandler_VerifyContact_CodeBudgetExhausted_Refused(t *testing.T) {
	env := newHandlerEnv(t)

	rec := env.do(t, http.MethodPost, apiPath+"/contacts", map[string]string{
		"channel": "sms",
		"address": testPhone,
	})
	created := decodeJSONBody[api.NotificationContact](t, rec)
	code := smsCodeAt(t, env.smsBuf, 0)
	wrong := wrongCode(code)

	for attempt := 0; attempt < 10; attempt++ {
		rec = env.do(t, http.MethodPost, apiPath+"/contacts/"+created.ID+"/verify", map[string]string{"code": wrong})
		assertEnvelope(t, rec, http.StatusBadRequest, "notification.contact_code_invalid")
	}

	rec = env.do(t, http.MethodPost, apiPath+"/contacts/"+created.ID+"/verify", map[string]string{"code": wrong})
	params := assertEnvelope(t, rec, http.StatusTooManyRequests, "notification.contact_rate_limited")
	dimension, _ := params["dimension"].(string)
	if !strings.HasPrefix(dimension, "verify.address.") {
		t.Errorf("dimension = %q, want the verify.address.* budget", dimension)
	}
	if _, has := params["retry_after_seconds"]; !has {
		t.Errorf("params = %v, want a retry_after_seconds hint", params)
	}
}

// TestHandler_ResendCode_IssuesAFreshCode proves the resend surface: a
// pending contact answers 204 and a second code goes out over its channel
// (the sms buffer's second line), and verifying with that code succeeds --
// the resent code is live. A resend naming an id no row holds answers 404
// like verify does.
func TestHandler_ResendCode_IssuesAFreshCode(t *testing.T) {
	env := newHandlerEnv(t)

	rec := env.do(t, http.MethodPost, apiPath+"/contacts", map[string]string{
		"channel": "sms",
		"address": testPhone,
	})
	created := decodeJSONBody[api.NotificationContact](t, rec)
	smsCodeAt(t, env.smsBuf, 0)

	rec = env.do(t, http.MethodPost, apiPath+"/contacts/"+created.ID+"/resend", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("resend status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	fresh := smsCodeAt(t, env.smsBuf, 1)

	rec = env.do(t, http.MethodPost, apiPath+"/contacts/"+created.ID+"/verify", map[string]string{"code": fresh})
	if rec.Code != http.StatusOK {
		t.Fatalf("verify with the resent code status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	rec = env.do(t, http.MethodPost, apiPath+"/contacts/contact-ghost/resend", nil)
	assertEnvelope(t, rec, http.StatusNotFound, "notification.contact_not_found")
}

// TestHandler_CreateContact_InvalidChannelOrAddress_Refused pins the
// create-time validation over HTTP: a channel outside the closed
// vocabulary answers 400 (notification.contact_invalid_channel) and an
// address that cannot normalize to the channel's form answers 400
// (notification.contact_invalid_address) -- nothing is created, no code
// goes out, and neither failure burns rate-limit budget.
func TestHandler_CreateContact_InvalidChannelOrAddress_Refused(t *testing.T) {
	env := newHandlerEnv(t)

	rec := env.do(t, http.MethodPost, apiPath+"/contacts", map[string]string{
		"channel": "carrier_pigeon",
		"address": testPhone,
	})
	assertEnvelope(t, rec, http.StatusBadRequest, "notification.contact_invalid_channel")

	rec = env.do(t, http.MethodPost, apiPath+"/contacts", map[string]string{
		"channel": "sms",
		"address": "not-a-phone-number",
	})
	assertEnvelope(t, rec, http.StatusBadRequest, "notification.contact_invalid_address")

	if got := smsLines(env.smsBuf); len(got) != 0 {
		t.Errorf("%d SMS sent on refused creates, want none", len(got))
	}
	if got := env.auditEvents(t, AuditActionContactAttested); len(got) != 0 {
		t.Errorf("%d attested events on refused creates, want none", len(got))
	}
}

// TestHandler_CreateContact_MalformedBody_Refused pins the transport
// failure of the create surface.
func TestHandler_CreateContact_MalformedBody_Refused(t *testing.T) {
	env := newHandlerEnv(t)

	rec := env.do(t, http.MethodPost, apiPath+"/contacts", "{{{not json")
	assertEnvelope(t, rec, http.StatusBadRequest, "notification.invalid_request_body")
}

// TestHandler_CreateContact_SendBudgetExhausted_Refused pins the send-side
// fail-closed limit over HTTP in the shape the service contract defines: a
// create is the first code message to its address, and the per-address daily
// budget (contactCodeSendDailyPerAddress) covers creates and resends alike --
// the dedupe probe means repeated creates at one address never send again,
// so the budget that remains is spent by resends. Four resends exhaust it;
// a fifth resend -- the sixth message the address would receive that day --
// answers 429 (notification.contact_rate_limited) naming the send dimension,
// before any message goes out, and the sms buffer still holds exactly the
// budget's messages.
func TestHandler_CreateContact_SendBudgetExhausted_Refused(t *testing.T) {
	env := newHandlerEnv(t)

	createdRec := env.do(t, http.MethodPost, apiPath+"/contacts", map[string]string{
		"channel": "sms",
		"address": testPhone,
	})
	if createdRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201 (body %s)", createdRec.Code, createdRec.Body.String())
	}
	created := decodeJSONBody[api.NotificationContact](t, createdRec)

	for i := 0; i < contactCodeSendDailyPerAddress-1; i++ {
		rec := env.do(t, http.MethodPost, apiPath+"/contacts/"+created.ID+"/resend", nil)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("resend %d status = %d, want 204 (body %s)", i+1, rec.Code, rec.Body.String())
		}
	}

	rec := env.do(t, http.MethodPost, apiPath+"/contacts/"+created.ID+"/resend", nil)
	params := assertEnvelope(t, rec, http.StatusTooManyRequests, "notification.contact_rate_limited")
	dimension, _ := params["dimension"].(string)
	if !strings.HasPrefix(dimension, "send.address.") {
		t.Errorf("dimension = %q, want the send.address.* budget", dimension)
	}
	if _, has := params["retry_after_seconds"]; !has {
		t.Errorf("params = %v, want a retry_after_seconds hint", params)
	}
	if got := len(smsLines(env.smsBuf)); got != contactCodeSendDailyPerAddress {
		t.Errorf("sms messages = %d, want the %d that fit inside the budget", got, contactCodeSendDailyPerAddress)
	}
}

// TestHandler_ListContacts_RosterNeverCarriesAddresses pins the PII
// guarantee of the roster read: whatever the rows' channels and states,
// the response tree carries no address and no blind-index key at any
// depth -- the spec's shape (id, channel, status, timestamps) enforced on
// the wire, not only in the generated struct.
func TestHandler_ListContacts_RosterNeverCarriesAddresses(t *testing.T) {
	env := newHandlerEnv(t)
	for _, address := range []string{testPhone, "alice@example.com"} {
		channel := "sms"
		if strings.Contains(address, "@") {
			channel = "email"
		}
		rec := env.do(t, http.MethodPost, apiPath+"/contacts", map[string]string{
			"channel": channel,
			"address": address,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s status = %d, want 201 (body %s)", address, rec.Code, rec.Body.String())
		}
	}

	rec := env.do(t, http.MethodGet, apiPath+"/contacts", nil)
	assertNoAddressLeak(t, rec)
	list := decodeJSONBody[api.NotificationListContactsResponse](t, rec)
	if len(list.Items) != 2 {
		t.Fatalf("items = %d, want the two created rows", len(list.Items))
	}
}

// flushRecorder is the stream tests' response recorder: the mirror of
// httptest.ResponseRecorder that a server-sent-events test needs, recording
// writes and flushes behind a mutex so the test can read what arrived while
// the handler goroutine is still writing. It implements http.ResponseWriter
// and http.Flusher, the two halves handleStream needs.
type flushRecorder struct {
	mu      sync.Mutex
	status  int
	headers http.Header
	body    bytes.Buffer
	flushes int
}

func (r *flushRecorder) Header() http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.headers == nil {
		r.headers = make(http.Header)
	}
	return r.headers
}

func (r *flushRecorder) WriteHeader(status int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		r.status = status
	}
}

func (r *flushRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(p)
}

func (r *flushRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushes++
}

func (r *flushRecorder) statusCode() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func (r *flushRecorder) flushCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushes
}

func (r *flushRecorder) headerValue(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.headers.Get(name)
}

func (r *flushRecorder) bodyText() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

var _ http.Flusher = (*flushRecorder)(nil)

// newFlushRecorder returns an empty flushRecorder.
func newFlushRecorder() *flushRecorder {
	return &flushRecorder{}
}

// announceInbox pushes one inbox-created announcement onto the hub in the
// payload shape the delivery job publishes (events.go's InboxCreatedPayload)
// -- the frame the stream route filters and forwards.
func (e *handlerEnv) announceInbox(t *testing.T, id, recipient, tenant string) {
	t.Helper()
	payload, err := json.Marshal(InboxCreatedPayload{
		MessageID:       id,
		TenantID:        tenant,
		RecipientUserID: recipient,
		TypeKey:         "clinic.appointment_reminder",
	})
	if err != nil {
		t.Fatalf("marshal announcement: %v", err)
	}
	e.hub.Publish(payload)
}

// sseFrames splits a recorded stream body into its frames, each the
// "event: message\ndata: ..." pair the route writes per announcement.
func sseFrames(t *testing.T, body string) []string {
	t.Helper()
	body = strings.TrimSuffix(body, "\n\n")
	if body == "" {
		return nil
	}
	return strings.Split(body, "\n\n")
}

// openStream starts one stream request against the handler on a cancellable
// context and waits until the route's first flush -- the 200 status line
// with the event-stream headers, which happens only after the route
// subscribed to the hub, so an announcement made after openStream returns
// cannot race the subscription.
func (e *handlerEnv) openStream(t *testing.T) (*flushRecorder, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(tenantCtx(handlerTenant))
	rec := newFlushRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, apiPath+"/stream", nil).WithContext(ctx))
	}()
	waitUntil(t, "the stream opened", func() bool { return rec.flushCount() >= 1 })
	return rec, cancel, done
}

// TestHandler_Stream_DeliversTheMatchingAnnouncementAsAnSSEFrame drives the
// stream's happy path: after the route opens (200 with the event-stream
// headers), an inbox-created announcement for the caller's own user and
// tenant arrives as exactly one frame -- the event/data pair the route's
// doc comment pins, carrying the announcement's JSON -- and cancelling the
// request context ends the handler.
func TestHandler_Stream_DeliversTheMatchingAnnouncementAsAnSSEFrame(t *testing.T) {
	env := newHandlerEnv(t)

	rec, cancel, done := env.openStream(t)
	defer cancel()

	env.announceInbox(t, "message-1", handlerUser, handlerTenant)
	waitUntil(t, "the announcement frame was written", func() bool { return rec.flushCount() >= 2 })

	if got := rec.statusCode(); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}
	if got := rec.headerValue("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.headerValue("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	frames := sseFrames(t, rec.bodyText())
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want exactly the one announcement (%q)", len(frames), rec.bodyText())
	}
	lines := strings.Split(frames[0], "\n")
	if len(lines) != 2 || lines[0] != "event: message" || !strings.HasPrefix(lines[1], "data: ") {
		t.Fatalf("frame = %q, want an event/data pair", frames[0])
	}
	var ann map[string]string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &ann); err != nil {
		t.Fatalf("frame data is not JSON: %v (%q)", err, frames[0])
	}
	if ann["message_id"] != "message-1" || ann["recipient_user_id"] != handlerUser || ann["tenant_id"] != handlerTenant {
		t.Errorf("announcement = %v, want the announced message for the caller", ann)
	}

	cancel()
	waitUntil(t, "the handler exited after cancel", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
}

// TestHandler_Stream_SkipsAnnouncementsForOthers pins the route's filtering:
// announcements for another recipient of the caller's tenant, or for the
// caller in another tenant (a user of several), never reach the wire -- the
// flush sequence stays at headers plus the one matching frame, and the
// body carries only that frame.
func TestHandler_Stream_SkipsAnnouncementsForOthers(t *testing.T) {
	env := newHandlerEnv(t)

	rec, cancel, done := env.openStream(t)
	defer cancel()

	env.announceInbox(t, "m-foreign-user", "user-8", handlerTenant)
	env.announceInbox(t, "m-foreign-tenant", handlerUser, handlerOtherTenant)
	env.announceInbox(t, "m-mine", handlerUser, handlerTenant)
	waitUntil(t, "the matching announcement was written", func() bool { return rec.flushCount() >= 2 })

	if got := rec.flushCount(); got != 2 {
		t.Fatalf("flushes = %d, want 2 (headers + the matching frame): %s", got, rec.bodyText())
	}
	frames := sseFrames(t, rec.bodyText())
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want only the matching announcement (%q)", len(frames), rec.bodyText())
	}
	if !strings.Contains(frames[0], "m-mine") {
		t.Errorf("frame = %q, want the matching announcement", frames[0])
	}

	cancel()
	waitUntil(t, "the handler exited after cancel", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
}

// TestHandler_Stream_SkipsUnreadableFrames pins the route's robustness on a
// frame that will not unmarshal into an announcement: it is skipped, never
// forwarded and never allowed to fail the stream -- the flush sequence
// stays headers plus the one good frame.
func TestHandler_Stream_SkipsUnreadableFrames(t *testing.T) {
	env := newHandlerEnv(t)

	rec, cancel, done := env.openStream(t)
	defer cancel()

	env.hub.Publish([]byte("{not json"))
	env.announceInbox(t, "message-1", handlerUser, handlerTenant)
	waitUntil(t, "the matching announcement was written", func() bool { return rec.flushCount() >= 2 })

	if got := rec.flushCount(); got != 2 {
		t.Fatalf("flushes = %d, want 2 (headers + the matching frame): %s", got, rec.bodyText())
	}
	frames := sseFrames(t, rec.bodyText())
	if len(frames) != 1 || !strings.Contains(frames[0], "message-1") {
		t.Errorf("frames = %q, want only the readable announcement", rec.bodyText())
	}

	cancel()
	waitUntil(t, "the handler exited after cancel", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
}

// TestHandler_Stream_EndsWhenTheRequestContextIsCancelled pins the stream's
// termination path: a cancelled request context ends the handler (and the
// deferred Close removes its connection from the hub) rather than leaving
// the connection registered forever.
func TestHandler_Stream_EndsWhenTheRequestContextIsCancelled(t *testing.T) {
	env := newHandlerEnv(t)

	_, cancel, done := env.openStream(t)

	cancel()
	waitUntil(t, "the handler exited", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})

	// The connection is gone: an announcement after the exit reaches no one,
	// and a second stream request still opens fresh (the hub was not left
	// wedged by the closed connection).
	env.announceInbox(t, "message-late", handlerUser, handlerTenant)
	rec2, cancel2, done2 := env.openStream(t)
	defer cancel2()
	env.announceInbox(t, "message-2", handlerUser, handlerTenant)
	waitUntil(t, "the second stream delivered", func() bool { return rec2.flushCount() >= 2 })
	frames := sseFrames(t, rec2.bodyText())
	if len(frames) != 1 || !strings.Contains(frames[0], "message-2") {
		t.Errorf("second stream frames = %q, want the announcement after reopen", rec2.bodyText())
	}
	cancel2()
	waitUntil(t, "the second handler exited", func() bool {
		select {
		case <-done2:
			return true
		default:
			return false
		}
	})
}

// nonFlushingResponseWriter wraps a *httptest.ResponseRecorder behind
// exactly http.ResponseWriter. A bare recorder cannot stand in for a
// non-flushing transport: it implements Flush, so the route's flusher guard
// would pass and the handler would block forever. Embedding one would
// promote that method too -- the wrapper re-declares Header/Write/WriteHeader
// and nothing else, so the guard sees a writer that can never flush.
type nonFlushingResponseWriter struct {
	rec *httptest.ResponseRecorder
}

func (w nonFlushingResponseWriter) Header() http.Header { return w.rec.Header() }

func (w nonFlushingResponseWriter) Write(p []byte) (int, error) { return w.rec.Write(p) }

func (w nonFlushingResponseWriter) WriteHeader(status int) { w.rec.WriteHeader(status) }

// TestHandler_Stream_RefusesAResponseWriterThatCannotFlush pins the route's
// guard on its transport: a response writer without http.Flusher is refused
// with the module's internal error before any stream opens -- a stream that
// could never flush would hold its connection open without ever delivering
// a frame.
func TestHandler_Stream_RefusesAResponseWriterThatCannotFlush(t *testing.T) {
	env := newHandlerEnv(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, apiPath+"/stream", nil).WithContext(tenantCtx(handlerTenant))
	env.h.ServeHTTP(nonFlushingResponseWriter{rec: rec}, req)
	assertEnvelope(t, rec, http.StatusInternalServerError, "notification.internal_error")
}

// unwrappingResponseWriter wraps an http.ResponseWriter WITHOUT directly
// implementing http.Flusher itself, but declaring the standard library's Go
// 1.20+ "Unwrap() http.ResponseWriter" convention instead -- exactly the
// shape go/observability's own statusRecorder uses in this app's real
// composed HTTP chain (see that type's own Unwrap doc comment, which names
// this very stream route as the reason it exists). Embedding the
// http.ResponseWriter INTERFACE, rather than the concrete *flushRecorder,
// is what keeps Flush from being promoted here even though the wrapped
// value underneath genuinely has one: method promotion through an embedded
// interface only promotes that interface's own method set, and Flush is
// not part of http.ResponseWriter.
type unwrappingResponseWriter struct {
	http.ResponseWriter
}

func (w unwrappingResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

var (
	_ http.ResponseWriter                       = unwrappingResponseWriter{}
	_ interface{ Unwrap() http.ResponseWriter } = unwrappingResponseWriter{}
)

// TestHandler_Stream_FindsFlusherThroughAWrappingResponseWriter pins the fix
// for a real bug: handleStream used to look for http.Flusher with a naive
// w.(http.Flusher) type assertion, which fails the instant a middleware
// wraps the response writer in anything that does not ALSO directly
// implement Flush -- exactly the shape every request reaching this handler
// through this app's real composed HTTP chain has, since
// go/observability's Middleware wraps every response writer in its own
// statusRecorder, which exposes Flush only via Unwrap. Before the fix, a
// stream request through that chain always failed with a 500
// notification.internal_error -- discovered by
// examples/reference-app/integration_test/distributed_mode_test.go's
// TestServer_DistributedMode_TwoReplicas_NotificationCrossesRealInfrastructure,
// the first test anywhere in this repository to drive this route through a
// real composed HTTP stack rather than calling the Handler directly. This
// test reproduces the same wrapper shape without needing the whole app: the
// stream must still open (a 200 status and at least one flush) despite the
// response writer never satisfying http.Flusher directly.
func TestHandler_Stream_FindsFlusherThroughAWrappingResponseWriter(t *testing.T) {
	env := newHandlerEnv(t)

	rec := newFlushRecorder()
	wrapped := unwrappingResponseWriter{ResponseWriter: rec}
	ctx, cancel := context.WithCancel(tenantCtx(handlerTenant))
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		env.h.ServeHTTP(wrapped, httptest.NewRequest(http.MethodGet, apiPath+"/stream", nil).WithContext(ctx))
	}()
	waitUntil(t, "the stream opened despite the wrapping response writer", func() bool { return rec.flushCount() >= 1 })
	if got := rec.statusCode(); got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	cancel()
	waitUntil(t, "the handler exited", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
}
