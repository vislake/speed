package notes

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// jsonContentType is the Content-Type every response below writes,
// matching go/tenancy/middleware.go's own tenantErrorContentType constant.
const jsonContentType = "application/json; charset=utf-8"

// ErrTextRequired is returned when a create-note request's text is empty
// or all whitespace. Its localized text lives in this module's Locales()
// resources (locales/{zh-CN,en-US}.toml, key "notes.text_required"), never
// hardcoded here: per the backend coding standard's error-handling rule
// (§6.2), a handler returns the structured code alone, and a client
// resolves the human-readable message through its own i18n catalog keyed
// on that code.
var ErrTextRequired = apperr.Invalid("notes.text_required")

// maxTextLength is the maximum number of characters -- Unicode code
// points, not bytes -- a note's text may contain. create enforces it
// below with utf8.RuneCountInString, deliberately not len(text): len
// counts UTF-8 bytes, which would wrongly reject in-bounds multi-byte
// text (see handler_test.go's multi-byte-text cases) and drift from what
// this constant is actually meant to bound.
//
// This value must match three other independent representations of the
// same limit that cannot themselves reference a Go constant: this
// module's openapi.yaml (NotesCreateNoteRequest.text's "maxLength: 4000"
// -- JSON Schema's maxLength keyword is itself defined in Unicode code
// points, so utf8.RuneCountInString is what actually matches the spec,
// not a byte count), model.go's Text gorm "size:4000" tag, and the
// VARCHAR(4000) column in both migrations/{postgres,sqlite}/
// 0001_create_notes.sql (PostgreSQL's VARCHAR(n) is itself a character
// count, not a byte count, so this check matches production too).
//
// This check exists because SQLite -- the demo profile's only backend --
// does not enforce a VARCHAR length limit at all under its type-affinity
// system: without an application-level check here, a request exceeding
// this limit is silently accepted and stored in full in the demo
// profile, where the documented spec and a real production PostgreSQL
// column would both reject it.
const maxTextLength = 4000

// ErrTextTooLong is returned when a create-note request's text exceeds
// maxTextLength characters (counted as in maxTextLength's own doc
// comment above). Its localized text lives in this module's Locales()
// resources (locales/{zh-CN,en-US}.toml, key "notes.text_too_long"),
// never hardcoded here -- see ErrTextRequired's doc comment above for the
// same rule.
var ErrTextTooLong = apperr.Invalid("notes.text_too_long")

// errInternal is returned when something below this handler fails in a way
// that is not itself an *apperr.Error -- see writeError.
var errInternal = apperr.Internal("notes.internal_error")

// createNoteRequest is the JSON body accepted by Handler's create
// endpoint. It carries no tenant_id field: the tenant is always the one
// tenancy.Middleware already resolved and injected into the request
// context, never a value the client supplies -- see the Handler doc
// comment below and root CLAUDE.md's multi-tenant isolation rule.
type createNoteRequest struct {
	Text string `json:"text"`
}

// noteResponse is the JSON shape of a single note in every response below.
type noteResponse struct {
	ID        string `json:"id"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
}

// listNotesResponse is the JSON body of a successful list response.
type listNotesResponse struct {
	Notes []noteResponse `json:"notes"`
}

// errorBody is the {code, params} structured-error envelope documented in
// docs/internal/11-cross-cutting.md and already used by
// go/tenancy/middleware.go's own (unexported) tenantErrorBody: APIs return
// a stable code plus structured parameters, never localized text.
type errorBody struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// Handler serves notes' HTTP endpoints. It must run downstream of
// tenancy.Middleware on a non-allowlisted path: every method reads the
// tenant tenancy.Middleware already resolved into the request context --
// via pkgcore.MustTenantFromContext, both directly here and, redundantly,
// again inside dbkit.Repository[Note]'s own methods -- and never from a
// request parameter, header or body, per root CLAUDE.md's multi-tenant
// isolation rule and backend coding standard §3.1.
type Handler struct {
	repo *Repository
	bus  pkgcore.EventBus
	mux  *http.ServeMux
}

// NewHandler returns a Handler serving repo's notes and publishing
// EventNoteCreated on bus whenever a note is created. bus may be nil, in
// which case create still succeeds but publishes nothing -- see the create
// method.
//
// The returned Handler's internal mux registers method-specific patterns
// ("POST "+apiPath, "GET "+apiPath) rather than switching on r.Method by
// hand, so that net/http's own ServeMux gives every other method on
// apiPath an automatic 405 Method Not Allowed (with a correctly populated
// Allow header) for free -- exactly the Go 1.22+ ServeMux behavior this
// module's own cmd/server wiring has to reason about carefully for
// mounting (see cmd/server/server.go's mountModuleRoutes doc comment).
func NewHandler(repo *Repository, bus pkgcore.EventBus) *Handler {
	h := &Handler{repo: repo, bus: bus}
	mux := http.NewServeMux()
	mux.HandleFunc(http.MethodPost+" "+apiPath, h.create)
	mux.HandleFunc(http.MethodGet+" "+apiPath, h.list)
	h.mux = mux
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// create handles POST requests: it creates a note under the caller's
// tenant and publishes EventNoteCreated.
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		// Unreachable in normal operation: apiPath is never allowlisted
		// (see cmd/server/server.go's buildServer), so tenancy.Middleware
		// already rejected any request that could reach here without a
		// resolved tenant. Handled anyway, rather than assumed away,
		// because a handler must never rely solely on a lower layer's
		// fail-closed behavior -- dbkit.Repository[Note].Create would
		// otherwise fail with this exact same unwrapped pkgcore.ErrNoTenant,
		// just one layer further down and with a less specific log line.
		writeError(w, errInternal.WithCause(err))
		return
	}
	// The tenant is now known on ctx, and this handler runs downstream of
	// tenancy.Middleware -- and, per main.go's own wiring comment, further
	// downstream of obs.Middleware, which already started this request's
	// span before tenancy.Middleware ever ran. AnnotateTenant is what
	// actually attaches tenant_id to that span from here: see its own doc
	// comment in go/observability/middleware.go for why obs.Middleware
	// itself cannot do this at its own layer.
	obs.AnnotateTenant(ctx)

	var req createNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, apperr.Invalid("notes.invalid_request_body").WithCause(err))
		return
	}

	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeError(w, ErrTextRequired)
		return
	}
	if length := utf8.RuneCountInString(text); length > maxTextLength {
		writeError(w, ErrTextTooLong.WithParam("limit", maxTextLength).WithParam("length", length))
		return
	}

	note := &Note{ID: uuid.NewString(), Text: text}
	if err := h.repo.Create(ctx, note); err != nil {
		writeError(w, err)
		return
	}

	h.publishNoteCreated(ctx, tenant, note)

	// obs.FromContext(ctx) attaches tenant_id (and trace_id/span_id, once
	// this request's span carries an active one) automatically -- see
	// backend-coding-standards.md §11's "logger from context, not a fresh
	// one" rule -- so it is not repeated as an explicit key-value pair
	// here the way it would have to be with a bare slog.Default() call.
	obs.FromContext(ctx).Info("note created", "note_id", note.ID)

	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toNoteResponse(note))
}

// publishNoteCreated publishes EventNoteCreated for note, using the
// EventBus the Module obtained from the Registry at wiring time (see
// module.go's Register) -- this is the "actual event publish happens
// later, inside the handler" half of that split. A publish failure is
// logged, not returned: the note itself was already committed by the time
// this runs, so a subscriber's failure must not turn an otherwise
// successful create into a 500 for the caller. This is deliberately the
// only place in this handler that both logs and does not also return the
// same error -- the backend coding standard's "do not log an error and
// also return it" rule (§11) is about not doing both for the SAME
// failure, and here nothing else ever surfaces this one.
func (h *Handler) publishNoteCreated(ctx context.Context, tenant pkgcore.TenantID, note *Note) {
	if h.bus == nil {
		return
	}
	evt := pkgcore.Event{
		Type:     EventNoteCreated,
		TenantID: tenant,
		Payload: NoteCreatedPayload{
			NoteID:   note.ID,
			TenantID: string(tenant),
		},
	}
	if err := h.bus.Publish(ctx, evt); err != nil {
		// See create's comment above on why tenant_id is not repeated
		// here as an explicit key-value pair: obs.FromContext(ctx)
		// already attaches it.
		obs.FromContext(ctx).Error("notes.note.created event publish failed",
			"note_id", note.ID, "error", err)
	}
}

// toNoteResponse converts note to its JSON response shape.
func toNoteResponse(note *Note) noteResponse {
	return noteResponse{
		ID:        note.ID,
		Text:      note.Text,
		CreatedAt: note.CreatedAt.Format(time.RFC3339),
	}
}

// list handles GET requests: it returns every note belonging to the
// caller's tenant.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	notes, err := h.repo.List(ctx)
	if err != nil {
		writeError(w, err)
		return
	}

	resp := listNotesResponse{Notes: make([]noteResponse, 0, len(notes))}
	for i := range notes {
		resp.Notes = append(resp.Notes, toNoteResponse(&notes[i]))
	}

	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// writeError writes err to w as a JSON {code, params} body, per the
// backend coding standard's API-error rule (§6.2): never a bare Go error
// string, and never the message of an error that might carry internal
// detail (a SQL fragment, a driver error). An err that is not an
// *apperr.Error -- meaning something below this handler did not classify
// it, such as pkgcore.ErrNoTenant -- is folded into errInternal so a
// caller never sees raw Go error text either way.
func writeError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = errInternal
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(errorBody{Code: appErr.Code, Params: appErr.Params})
}
