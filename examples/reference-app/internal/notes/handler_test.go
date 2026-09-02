package notes

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/vislake/speed/examples/reference-app/internal/notes/api"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// newTestHandler returns a Handler backed by a freshly migrated SQLite
// database and a real in-memory pkgcore.EventBus, so tests can assert on
// both the HTTP response and the event actually published -- not a mock of
// either.
func newTestHandler(t *testing.T) (*Handler, pkgcore.EventBus) {
	t.Helper()
	repo := newMigratedRepository(t)
	bus := pkgcore.NewMemoryEventBus()
	return NewHandler(repo, bus), bus
}

// doRequest issues req against h and returns the recorded response. When
// tenant is non-empty, it is injected into the request context exactly the
// way tenancy.Middleware would (pkgcore.WithTenant) -- this test exercises
// Handler in isolation, downstream of where Middleware would normally run;
// cmd/server's own end-to-end test exercises the real Middleware too.
func doRequest(h *Handler, req *http.Request, tenant pkgcore.TenantID) *httptest.ResponseRecorder {
	if tenant != "" {
		req = req.WithContext(pkgcore.WithTenant(req.Context(), tenant))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandler_Create_ValidText_ReturnsCreatedNoteAndPublishesEvent(t *testing.T) {
	h, bus := newTestHandler(t)

	var published []pkgcore.Event
	bus.Subscribe(EventNoteCreated, func(ctx context.Context, evt pkgcore.Event) error {
		published = append(published, evt)
		return nil
	})

	body := strings.NewReader(`{"text":"buy milk"}`)
	req := httptest.NewRequest(http.MethodPost, apiPath, body)
	rec := doRequest(h, req, "tenant-acme")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var resp api.NotesNote
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, rec.Body.String())
	}
	// Generated types make optional spec properties pointers; the response
	// assertions below follow the pointers and require them non-nil --
	// the handler always sets every field of NotesNote, so a nil pointer
	// would be the regression signal.
	if resp.ID == nil {
		t.Fatal("response ID is missing (want a set pointer)")
	}
	if *resp.ID == "" {
		t.Fatal("response ID is empty")
	}
	if resp.Text == nil || *resp.Text != "buy milk" {
		t.Fatalf("response Text = %v, want %q", resp.Text, "buy milk")
	}
	if resp.CreatedAt == nil {
		t.Fatal("response CreatedAt is missing (want a set pointer)")
	}

	if len(published) != 1 {
		t.Fatalf("published %d events, want 1", len(published))
	}
	if published[0].TenantID != "tenant-acme" {
		t.Fatalf("published event TenantID = %q, want %q", published[0].TenantID, "tenant-acme")
	}
	payload, ok := published[0].Payload.(NoteCreatedPayload)
	if !ok {
		t.Fatalf("published event Payload = %#v, want NoteCreatedPayload", published[0].Payload)
	}
	if payload.NoteID != *resp.ID {
		t.Fatalf("published event NoteID = %q, want %q", payload.NoteID, *resp.ID)
	}
	if payload.TenantID != "tenant-acme" {
		t.Fatalf("published event TenantID = %q, want %q", payload.TenantID, "tenant-acme")
	}
}

func TestHandler_Create_EmptyText_ReturnsTextRequiredError(t *testing.T) {
	h, _ := newTestHandler(t)

	for _, text := range []string{"", "   ", "\t\n"} {
		// json.Marshal, not raw string concatenation: text may itself
		// contain characters (here, a literal tab and newline) that are
		// not valid unescaped inside a JSON string, and building the body
		// by hand would send malformed JSON instead of the whitespace-only
		// text this case means to test -- exercising the wrong branch of
		// Handler.NotesCreateNote (notes.invalid_request_body, not
		// notes.text_required) for a reason that has nothing to do with
		// what this test is actually about. The request type is the
		// spec-generated one (api.NotesCreateNoteRequest), so the body
		// this sends is exactly what the module's own contract declares.
		encoded, err := json.Marshal(api.NotesCreateNoteRequest{Text: text})
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		body := bytes.NewReader(encoded)
		req := httptest.NewRequest(http.MethodPost, apiPath, body)
		rec := doRequest(h, req, "tenant-acme")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("text=%q: status = %d, want %d", text, rec.Code, http.StatusBadRequest)
		}
		var got api.NotesError
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		if got.Code == nil || *got.Code != ErrTextRequired.Code {
			t.Fatalf("text=%q: error code = %v, want %q", text, got.Code, ErrTextRequired.Code)
		}
	}
}

// TestHandler_Create_TextExceedsMaxLength_ReturnsTextTooLongError reproduces
// the round-3 kernel bootstrap smoke test finding directly against Handler:
// curling POST /api/v1/notes with a 10,000-character text value returned
// HTTP 201 Created and stored the full string verbatim (confirmed by
// reading it back via GET), even though api/openapi.yaml declares
// "maxLength: 4000" on NotesCreateNoteRequest.text and both
// migrations/{postgres,sqlite}/0001_create_notes.sql declare the column
// VARCHAR(4000) -- SQLite's type-affinity system does not itself enforce
// that length, and NotesCreateNote used to check only for
// empty/whitespace-only text, never a maximum. This test fails against
// that unfixed handler (status would be 201, not 400) and passes once
// NotesCreateNote enforces maxTextLength.
func TestHandler_Create_TextExceedsMaxLength_ReturnsTextTooLongError(t *testing.T) {
	h, _ := newTestHandler(t)

	encoded, err := json.Marshal(api.NotesCreateNoteRequest{Text: strings.Repeat("a", maxTextLength+1)})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, apiPath, bytes.NewReader(encoded))
	rec := doRequest(h, req, "tenant-acme")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	var got api.NotesError
	if err = json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if got.Code == nil || *got.Code != ErrTextTooLong.Code {
		t.Fatalf("error code = %v, want %q", got.Code, ErrTextTooLong.Code)
	}

	// Negative control matching the reported bug precisely: the failure was
	// not merely a wrong status code, it was HTTP 201 plus the oversized
	// text actually landing in storage. Assert directly against the
	// repository -- not just the HTTP response -- so a fix that only
	// patched the returned status while still calling repo.Create
	// underneath would still fail this test.
	notes, err := h.repo.List(pkgcore.WithTenant(context.Background(), "tenant-acme"))
	if err != nil {
		t.Fatalf("list notes: %v", err)
	}
	if len(notes) != 0 {
		t.Fatalf("got %d notes persisted for tenant-acme, want 0 (oversized text must be rejected before Create)", len(notes))
	}
}

// TestHandler_Create_TextAtExactlyMaxLength_ReturnsCreated is the boundary
// negative control for the test above: api/openapi.yaml's "maxLength:
// 4000" makes exactly 4000 characters valid, so a correct fix must accept
// maxTextLength and reject only maxTextLength+1 -- proving the check added
// for the test above is not off-by-one against a request this module must
// keep accepting.
func TestHandler_Create_TextAtExactlyMaxLength_ReturnsCreated(t *testing.T) {
	h, _ := newTestHandler(t)

	encoded, err := json.Marshal(api.NotesCreateNoteRequest{Text: strings.Repeat("a", maxTextLength)})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, apiPath, bytes.NewReader(encoded))
	rec := doRequest(h, req, "tenant-acme")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
}

// TestHandler_Create_MultiByteTextAtExactlyMaxLength_ReturnsCreated pins
// down that NotesCreateNote's length check counts Unicode code points
// (utf8.RuneCountInString), not UTF-8 bytes (len(text)): a CJK ideograph
// (U+7B14, meaning "pen" -- fittingly, the instrument a note is written
// with) is a 3-byte rune, so maxTextLength copies of it sit exactly at the
// character-count limit -- matching api/openapi.yaml's JSON-Schema
// "maxLength: 4000" (itself defined in Unicode code points) and
// PostgreSQL's VARCHAR(4000) (itself a character count, not a byte count)
// -- while being 3x over the limit in bytes. A byte-counting
// implementation would wrongly reject this in-bounds request; this test
// catches exactly that regression.
func TestHandler_Create_MultiByteTextAtExactlyMaxLength_ReturnsCreated(t *testing.T) {
	h, _ := newTestHandler(t)

	encoded, err := json.Marshal(api.NotesCreateNoteRequest{Text: strings.Repeat("笔", maxTextLength)})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, apiPath, bytes.NewReader(encoded))
	rec := doRequest(h, req, "tenant-acme")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
}

func TestHandler_Create_InvalidJSON_ReturnsInvalidRequestBodyError(t *testing.T) {
	h, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, apiPath, strings.NewReader(`{not json`))
	rec := doRequest(h, req, "tenant-acme")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	var got api.NotesError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if got.Code == nil || *got.Code != "notes.invalid_request_body" {
		t.Fatalf("error code = %v, want %q", got.Code, "notes.invalid_request_body")
	}
}

// TestHandler_Create_ResponseCreatedAt_IsWholeSeconds pins the created_at
// wire format to the shape the hand-written noteResponse used to emit.
// The generated NotesNote.CreatedAt is a time.Time (api/openapi.yaml
// declares "format: date-time"), and encoding/json renders a time.Time
// with RFC3339Nano -- byte-identical to the RFC3339 the old code
// formatted by hand whenever the fractional second is zero. Handler's
// toNoteResponse truncates to the whole second before encoding (see its
// doc comment in handler.go); this test fails against a response that
// carries a fractional second (or any non-RFC3339 layout), even though a
// plain JSON decode into time.Time would accept one.
func TestHandler_Create_ResponseCreatedAt_IsWholeSeconds(t *testing.T) {
	h, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, apiPath, strings.NewReader(`{"text":"buy milk"}`))
	rec := doRequest(h, req, "tenant-acme")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	var raw struct {
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, rec.Body.String())
	}
	wholeSeconds := regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(Z|[+-][0-9]{2}:[0-9]{2})$`)
	if !wholeSeconds.MatchString(raw.CreatedAt) {
		t.Fatalf("created_at = %q, want a whole-second RFC3339 timestamp (date + T + HH:MM:SS + Z or numeric offset, no fractional part)", raw.CreatedAt)
	}
	// The string must still name a real instant -- the regex above could
	// in principle match an invented date, and the wire value must be
	// parseable by any RFC3339 client.
	if _, err := time.Parse(time.RFC3339, raw.CreatedAt); err != nil {
		t.Fatalf("created_at = %q does not parse as RFC3339: %v", raw.CreatedAt, err)
	}
}

// TestHandler_List_EmptyTenant_ReturnsNotesEmptyArrayOnWire pins the wire
// shape of an empty list. NotesListNotesResponse.notes is optional in
// api/openapi.yaml, so the generated field is a *[]NotesNote carrying an
// omitempty tag: a nil pointer would drop the "notes" key from the
// response entirely, and a pointer to a nil slice would emit "notes":
// null. Handler's NotesListNotes always allocates the slice (see its doc
// comment in handler.go), so an empty tenant's list must come back as
// {"notes":[]} -- byte-for-byte the same as the hand-written
// listNotesResponse emitted, whose notes field had no omitempty.
func TestHandler_List_EmptyTenant_ReturnsNotesEmptyArrayOnWire(t *testing.T) {
	h, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, apiPath, nil)
	rec := doRequest(h, req, "tenant-acme")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// Encoder.Encode emits the object plus a trailing newline; the exact
	// match pins the empty-array form -- anything else (a missing key,
	// "notes": null, whitespace between tokens) fails here.
	if body := rec.Body.String(); body != "{\"notes\":[]}\n" {
		t.Fatalf("empty list body = %q, want %q", body, "{\"notes\":[]}\n")
	}
}

func TestHandler_List_ReturnsOnlyCallersTenantNotes(t *testing.T) {
	h, _ := newTestHandler(t)

	create := func(tenant pkgcore.TenantID, text string) {
		req := httptest.NewRequest(http.MethodPost, apiPath, strings.NewReader(`{"text":"`+text+`"}`))
		rec := doRequest(h, req, tenant)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create tenant=%s text=%s: status = %d, body = %s", tenant, text, rec.Code, rec.Body.String())
		}
	}
	create("tenant-acme", "acme note 1")
	create("tenant-acme", "acme note 2")
	create("tenant-globex", "globex note 1")

	req := httptest.NewRequest(http.MethodGet, apiPath, nil)
	rec := doRequest(h, req, "tenant-acme")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp api.NotesListNotesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Notes == nil {
		t.Fatal("response Notes is missing (want a set pointer)")
	}
	if len(*resp.Notes) != 2 {
		t.Fatalf("got %d notes for tenant-acme, want 2 (response: %+v)", len(*resp.Notes), resp)
	}
	for _, n := range *resp.Notes {
		if n.Text != nil && *n.Text == "globex note 1" {
			t.Fatalf("tenant-acme's list leaked another tenant's note: %+v", n)
		}
	}
}

func TestHandler_List_NoTenantInContext_ReturnsInternalError(t *testing.T) {
	h, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodGet, apiPath, nil)
	rec := doRequest(h, req, "") // no tenant injected

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	var got api.NotesError
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if got.Code == nil || *got.Code != errInternal.Code {
		t.Fatalf("error code = %v, want %q", got.Code, errInternal.Code)
	}
}

func TestHandler_ServeHTTP_UnsupportedMethod_ReturnsMethodNotAllowed(t *testing.T) {
	h, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodDelete, apiPath, nil)
	rec := doRequest(h, req, "tenant-acme")

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d (net/http ServeMux's automatic response for a method with no registered pattern)",
			rec.Code, http.StatusMethodNotAllowed)
	}
}

// TestHandler_List_AnnotatesSpanWithTenant reproduces the round-4
// reference-app retrofit finding: NotesListNotes runs downstream of the
// identical tenancy.Middleware as NotesCreateNote -- both read an
// equally-resolved tenant off ctx -- yet only NotesCreateNote called
// obs.AnnotateTenant, so a GET /api/v1/notes span carried no tenant_id
// while a POST /api/v1/notes one did. This starts a real span the same
// way go/observability/middleware_test.go's own AnnotateTenant tests do,
// puts a tenant on that same context, calls Handler.NotesListNotes
// through ServeHTTP, and asserts the exported span carries a tenant_id
// attribute. It fails against the unfixed NotesListNotes (no tenant_id
// attribute at all, since AnnotateTenant is never called) and passes
// once NotesListNotes calls obs.AnnotateTenant like NotesCreateNote
// does.
func TestHandler_List_AnnotatesSpanWithTenant(t *testing.T) {
	h, _ := newTestHandler(t)

	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, span := tp.Tracer("notes_test").Start(context.Background(), "GET /api/v1/notes")
	ctx = pkgcore.WithTenant(ctx, "tenant-acme")

	req := httptest.NewRequest(http.MethodGet, apiPath, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	span.End()

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 exported span, got %d", len(spans))
	}
	var tenantAttr string
	var found bool
	for _, a := range spans[0].Attributes {
		if string(a.Key) == obs.TenantIDKey {
			tenantAttr = a.Value.AsString()
			found = true
		}
	}
	if !found {
		t.Fatalf("expected span to carry a %s attribute (matching NotesCreateNote's behavior); attributes: %v",
			obs.TenantIDKey, spans[0].Attributes)
	}
	if tenantAttr != "tenant-acme" {
		t.Errorf("%s attribute = %q, want %q", obs.TenantIDKey, tenantAttr, "tenant-acme")
	}
}

// TestHandler_List_LogsWithTenantID reproduces the round-4 reference-app
// retrofit finding's medium-severity half: NotesListNotes never logged
// anything at all, so a GET /api/v1/notes request left behind zero
// tenant_id-bearing log lines, unlike NotesCreateNote's "note created"
// line (obs.FromContext(ctx).Info in handler.go's NotesCreateNote). This
// injects a text-handler logger via obs.WithLogger -- the same technique
// go/observability/logger_test.go uses to assert on FromContext's
// attached fields -- calls Handler.NotesListNotes through ServeHTTP, and
// asserts a "notes listed" log line was written carrying the caller's
// tenant_id. It fails against the unfixed NotesListNotes (no log line
// captured at all) and passes once NotesListNotes logs through
// obs.FromContext(ctx) like NotesCreateNote does.
func TestHandler_List_LogsWithTenantID(t *testing.T) {
	h, _ := newTestHandler(t)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	ctx := obs.WithLogger(pkgcore.WithTenant(context.Background(), "tenant-acme"), logger)

	req := httptest.NewRequest(http.MethodGet, apiPath, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	out := buf.String()
	if !strings.Contains(out, `msg="notes listed"`) {
		t.Fatalf("expected a %q log line for the GET request; captured log output: %s", "notes listed", out)
	}
	if want := obs.TenantIDKey + "=tenant-acme"; !strings.Contains(out, want) {
		t.Errorf("log line missing %q (matching NotesCreateNote's behavior); got: %s", want, out)
	}
	if want := "note_count=0"; !strings.Contains(out, want) {
		t.Errorf("log line missing %q for an empty tenant; got: %s", want, out)
	}
}
