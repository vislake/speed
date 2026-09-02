package notes

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

	var resp noteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, rec.Body.String())
	}
	if resp.ID == "" {
		t.Fatal("response ID is empty")
	}
	if resp.Text != "buy milk" {
		t.Fatalf("response Text = %q, want %q", resp.Text, "buy milk")
	}
	if resp.CreatedAt == "" {
		t.Fatal("response CreatedAt is empty")
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
	if payload.NoteID != resp.ID {
		t.Fatalf("published event NoteID = %q, want %q", payload.NoteID, resp.ID)
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
		// Handler.create (notes.invalid_request_body, not
		// notes.text_required) for a reason that has nothing to do with
		// what this test is actually about.
		encoded, err := json.Marshal(createNoteRequest{Text: text})
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		body := bytes.NewReader(encoded)
		req := httptest.NewRequest(http.MethodPost, apiPath, body)
		rec := doRequest(h, req, "tenant-acme")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("text=%q: status = %d, want %d", text, rec.Code, http.StatusBadRequest)
		}
		var got errorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		if got.Code != ErrTextRequired.Code {
			t.Fatalf("text=%q: error code = %q, want %q", text, got.Code, ErrTextRequired.Code)
		}
	}
}

// TestHandler_Create_TextExceedsMaxLength_ReturnsTextTooLongError reproduces
// the round-3 kernel bootstrap smoke test finding directly against Handler:
// curling POST /api/v1/notes with a 10,000-character text value returned
// HTTP 201 Created and stored the full string verbatim (confirmed by
// reading it back via GET), even though openapi.yaml declares "maxLength:
// 4000" on NotesCreateNoteRequest.text and both
// migrations/{postgres,sqlite}/0001_create_notes.sql declare the column
// VARCHAR(4000) -- SQLite's type-affinity system does not itself enforce
// that length, and create used to check only for empty/whitespace-only
// text, never a maximum. This test fails against that unfixed handler
// (status would be 201, not 400) and passes once create enforces
// maxTextLength.
func TestHandler_Create_TextExceedsMaxLength_ReturnsTextTooLongError(t *testing.T) {
	h, _ := newTestHandler(t)

	encoded, err := json.Marshal(createNoteRequest{Text: strings.Repeat("a", maxTextLength+1)})
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, apiPath, bytes.NewReader(encoded))
	rec := doRequest(h, req, "tenant-acme")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	var got errorBody
	if err = json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if got.Code != ErrTextTooLong.Code {
		t.Fatalf("error code = %q, want %q", got.Code, ErrTextTooLong.Code)
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
// negative control for the test above: openapi.yaml's "maxLength: 4000"
// makes exactly 4000 characters valid, so a correct fix must accept
// maxTextLength and reject only maxTextLength+1 -- proving the check added
// for the test above is not off-by-one against a request this module must
// keep accepting.
func TestHandler_Create_TextAtExactlyMaxLength_ReturnsCreated(t *testing.T) {
	h, _ := newTestHandler(t)

	encoded, err := json.Marshal(createNoteRequest{Text: strings.Repeat("a", maxTextLength)})
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
// down that create's length check counts Unicode code points
// (utf8.RuneCountInString), not UTF-8 bytes (len(text)): "笔" ("note",
// fittingly) is a 3-byte rune, so maxTextLength copies of it sit exactly at
// the character-count limit -- matching openapi.yaml's JSON-Schema
// "maxLength: 4000" (itself defined in Unicode code points) and
// PostgreSQL's VARCHAR(4000) (itself a character count, not a byte count)
// -- while being 3x over the limit in bytes. A byte-counting
// implementation would wrongly reject this in-bounds request; this test
// catches exactly that regression.
func TestHandler_Create_MultiByteTextAtExactlyMaxLength_ReturnsCreated(t *testing.T) {
	h, _ := newTestHandler(t)

	encoded, err := json.Marshal(createNoteRequest{Text: strings.Repeat("笔", maxTextLength)})
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
	var got errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if got.Code != "notes.invalid_request_body" {
		t.Fatalf("error code = %q, want %q", got.Code, "notes.invalid_request_body")
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

	var resp listNotesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Notes) != 2 {
		t.Fatalf("got %d notes for tenant-acme, want 2 (response: %+v)", len(resp.Notes), resp)
	}
	for _, n := range resp.Notes {
		if n.Text == "globex note 1" {
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
	var got errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if got.Code != errInternal.Code {
		t.Fatalf("error code = %q, want %q", got.Code, errInternal.Code)
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
