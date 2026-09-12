package notes

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/vislake/speed/examples/reference-app/internal/notes/api"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/httpapi"
)

// ErrTextRequired is returned when a create-note request's text is empty
// or all whitespace. Its localized text lives in this module's Locales()
// resources (locales/{zh-CN,en-US}.toml, key "notes.text_required"), never
// hardcoded here: a handler returns the structured code alone, and a
// client resolves the human-readable message through its own i18n
// catalog keyed on that code.
var ErrTextRequired = apperr.Invalid("notes.text_required")

// maxTextLength is the maximum number of characters -- Unicode code
// points, not bytes -- a note's text may contain. NotesCreateNote
// enforces it below with utf8.RuneCountInString, deliberately not
// len(text): len counts UTF-8 bytes, which would wrongly reject
// in-bounds multi-byte text (see handler_test.go's multi-byte-text
// cases) and drift from what this constant is actually meant to bound.
//
// This value must match three other independent representations of the
// same limit that cannot themselves reference a Go constant: this
// module's api/openapi.yaml (NotesCreateNoteRequest.text's "maxLength:
// 4000" -- JSON Schema's maxLength keyword is itself defined in Unicode
// code points, so utf8.RuneCountInString is what actually matches the
// spec, not a byte count), model.go's Text gorm "size:4000" tag, and the
// VARCHAR(4000) column in both migrations/{postgres,sqlite}/
// 0001_create_notes.sql (PostgreSQL's VARCHAR(n) is itself a character
// count, not a byte count, so this check matches the distributed
// deployment mode too).
//
// This check exists because SQLite -- the standalone deployment mode's
// only backend -- does not enforce a VARCHAR length limit at all under
// its type-affinity system: without an application-level check here, a
// request exceeding this limit is silently accepted and stored in full
// in the standalone deployment mode, where the documented spec and a
// real PostgreSQL column under the distributed deployment mode would
// both reject it.
const maxTextLength = 4000

// maxRequestBodyBytes bounds the size of a create-note request body BEFORE
// it is decoded. The bound is deliberately the same 64 KiB
// go/authn/handler.go's maxRequestBodyBytes uses for its own
// unauthenticated register and login endpoints: this surface is
// authenticated, but the body still feeds an unbounded json.Decoder before
// any validation has had a chance to refuse it, so an arbitrarily large
// body would otherwise be buffered in full (go/authn/handler.go's own doc
// comment gives the reasoning in full). A body any legitimate note can
// produce stays far below the bound -- maxTextLength is 4000 Unicode code
// points, and even an all-HTML-escaped text (the JSON encoder's widest
// legal rendering, six bytes per character) tops out near 24 KiB of body,
// leaving an order of magnitude of headroom.
const maxRequestBodyBytes = 1 << 16

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

// ErrSubjectUnresolved is returned when a create-note request cannot be
// attributed to a user: no SubjectResolver is wired, or the wired resolver
// could not name the request's creator. NotesCreateNote needs the creator's
// user id to carry in the note-created event it publishes (see
// NoteCreatedPayload.CreatorUserID), so an unattributable request is
// refused with a 401 before any note is created -- never served an empty
// or invented creator -- mirroring the rule org's own SubjectResolver module
// documents ("a resolver that returns ok=false -- or is not wired at all
// -- fails every endpoint closed"; see the SubjectResolver declaration at
// the bottom of this file). Its localized text lives in this module's
// Locales() resources (locales/{zh-CN,en-US}.toml, key
// "notes.subject_unresolved"), never hardcoded here -- see ErrTextRequired's
// doc comment for the same rule.
var ErrSubjectUnresolved = apperr.Unauthorized("notes.subject_unresolved")

// ErrNoteNotFound is returned when a delete or restore request names no
// note of the caller's tenant in the state the operation needs it in: the
// id is unknown, belongs to another tenant, or (for delete) the note is
// already deleted / (for restore) the note is not deleted. The three cases
// collapse into one uniform 404, mirroring the collapse dbkit's own
// ErrRecordNotFound performs for exactly these cases (and the "the refusal
// is uniform" property this module's own fragment documents) -- while
// naming the answer with a module-owned code, never dbkit's, so a client's
// i18n catalog resolves notes.note_not_found from this module's own
// Locales() resources (see noteMutationError for the translation). Its
// localized text lives in this module's Locales() resources
// (locales/{zh-CN,en-US}.toml, key "notes.note_not_found"), never
// hardcoded here -- see ErrTextRequired's doc comment for the same rule.
var ErrNoteNotFound = apperr.NotFound("notes.note_not_found")

// Handler serves notes' HTTP endpoints by implementing the spec-generated
// api.ServerInterface (see api/notes-server.gen.go, regenerated from this
// module's api/openapi.yaml by task api:gen:app -- the compile-time assertion
// at the bottom of this file is what makes "spec changed, handler not" a
// compile failure instead of a runtime surprise). It must run downstream
// of tenancy.Middleware on a non-allowlisted path: every method reads the
// tenant tenancy.Middleware already resolved into the request context --
// via pkgcore.MustTenantFromContext, both directly here and, redundantly,
// again inside dbkit.Repository[Note]'s own methods -- and never from a
// request parameter, header or body.
type Handler struct {
	repo         *Repository
	bus          pkgcore.EventBus
	auditActions pkgcore.AuditActionRegistrar
	// subject answers "who created this note" for NotesCreateNote -- the
	// creator's user id travels in the note-created event (see
	// NoteCreatedPayload.CreatorUserID) so a subscriber such as the
	// notification module can route to the right recipient. Nil means
	// unwired: NotesCreateNote then refuses every request with
	// ErrSubjectUnresolved (see resolveSubject).
	subject SubjectResolver
	mux     *http.ServeMux
}

// NewHandler returns a Handler serving repo's notes, publishing
// EventNoteCreated on bus and recording an AuditEvent through
// audit.Emit (validated against auditActions) whenever a note is created.
// subject resolves the creating user for each create request (see
// SubjectResolver at the bottom of this file); nil fails every
// NotesCreateNote closed with ErrSubjectUnresolved -- never an invented
// creator in the published event.
// bus may be nil, in which case creating the note still succeeds but
// nothing is published -- see the NotesCreateNote method. auditActions
// must not be nil when bus is non-nil: NotesCreateNote calls audit.Emit
// unconditionally once a note is created, and Emit needs a real
// AuditActionRegistrar to validate AuditActionNoteCreate against (see
// module.go's Register, the one real caller, for why the two are always
// handed to NewHandler together, both sourced from the same *pkgcore.
// Registry).
//
// The returned Handler's routing is registered by the generated
// api.HandlerFromMux helper: it derives this module's method+path
// patterns ("POST /api/v1/notes", "GET /api/v1/notes") from the "paths:"
// keys of api/openapi.yaml itself (see api/notes-server.gen.go's
// HandlerWithOptions) -- one less copy of path+method
// truth to keep in step with the spec by hand. net/http's own ServeMux
// gives every other method on apiPath an automatic 405 Method Not
// Allowed (with a correctly populated Allow header) for free. The spec's
// path and module.go's apiPath (the mount point, and
// the path tests request) must keep agreeing -- see apiPath's doc
// comment in module.go.
func NewHandler(repo *Repository, bus pkgcore.EventBus, auditActions pkgcore.AuditActionRegistrar, subject SubjectResolver) *Handler {
	h := &Handler{repo: repo, bus: bus, auditActions: auditActions, subject: subject}
	h.mux = http.NewServeMux()
	api.HandlerFromMux(h, h.mux)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// NotesCreateNote implements api.ServerInterface: it handles POST
// /api/v1/notes, creating a note under the caller's tenant and publishing
// EventNoteCreated. The request body is decoded into the spec-generated
// api.NotesCreateNoteRequest -- whose only field is text, exactly as
// api/openapi.yaml declares: there is deliberately no tenant_id field
// anywhere on the request, because the tenant is always the one
// tenancy.Middleware already resolved and injected into the request
// context, never a value the client supplies (encoding/json's default
// decoder silently ignores any unknown field a client does send, such as
// a forged tenant_id -- pinned by flowtests/server_test.go's tenant-hint test).
func (h *Handler) NotesCreateNote(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		// Unreachable in normal operation: apiPath is never allowlisted
		// (see internal/app/server.go's BuildServer), so tenancy.Middleware
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

	// The creator is resolved up front, before the body is even read: the
	// creator's user id is a required ingredient of the event NotesCreateNote
	// publishes below (see NoteCreatedPayload.CreatorUserID), so a request
	// no resolver can attribute is refused with 401 here -- before any
	// validation, before any side effect -- never half-processed. Like the
	// tenant, the creator never comes from the request body: it is resolved
	// exclusively through the host's SubjectResolver module (see its
	// declaration at the bottom of this file), which in a real deployment
	// reads the caller's identity from whatever the server itself verified.
	creatorUserID, ok := h.resolveSubject(w, r)
	if !ok {
		return
	}

	var req api.NotesCreateNoteRequest
	if !httpapi.DecodeJSON(w, r, maxRequestBodyBytes, &req, apperr.Invalid("notes.invalid_request_body")) {
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

	note := &Note{ID: uuid.NewString(), Text: text, CreatorUserID: creatorUserID}
	if err := h.repo.Create(ctx, note); err != nil {
		writeError(w, err)
		return
	}

	h.recordNoteCreatedAudit(ctx, note, creatorUserID)
	h.publishNoteCreated(ctx, tenant, note, creatorUserID)

	// obs.FromContext(ctx) attaches tenant_id (and trace_id/span_id, once
	// this request's span carries an active one) automatically -- see
	// backend-coding-standards.md §11's "logger from context, not a fresh
	// one" rule -- so it is not repeated as an explicit key-value pair
	// here the way it would have to be with a bare slog.Default() call.
	obs.FromContext(ctx).Info("note created", "note_id", note.ID)

	httpapi.WriteJSON(w, http.StatusCreated, toNoteResponse(note))
}

// resolveSubject resolves the HTTP caller's user id through the host's
// SubjectResolver module (declared at the bottom of this file), refusing
// with ErrSubjectUnresolved -- a 401 -- when no resolver is wired, when
// the resolver cannot attribute the request, or when it returns an empty
// user id: an empty id is treated exactly like no id, so a resolver bug can
// never smuggle an empty creator into the published event. It is the
// handler's one and only source of the creator: neither the request body,
// a header read here, nor the context ever names the creator directly
// (only the module interface -- which the host may of course back with
// whatever it verifies -- may).
func (h *Handler) resolveSubject(w http.ResponseWriter, r *http.Request) (string, bool) {
	if h.subject == nil {
		writeError(w, ErrSubjectUnresolved)
		return "", false
	}
	userID, ok := h.subject.Subject(r)
	if !ok || userID == "" {
		writeError(w, ErrSubjectUnresolved)
		return "", false
	}
	return userID, true
}

// recordNoteCreatedAudit records an AuditEvent for note's creation through
// audit.Emit -- the declarative collection mechanism go/dbkit/audit
// documents, used here instead of dbkit's automatic AuditBus-driven write
// capture, which this app's shared connection deliberately scopes off
// Note: internal/app's dbkit.Open call wires the bus but leaves Note off its
// Options.AuditModels list, because this module records its own note trail
// declaratively (see model.go's AuditResourceType doc comment for the
// reason in full). Calling this AFTER h.repo.Create has already returned
// keeps audit.Emit's own write out of Create's transaction: by this point
// Create's own WithTenantSession transaction has committed, so Emit's
// write against the same database opens a fresh, uncontended session
// rather than nesting inside an open one.
//
// The recorded event is attributed to the creating user -- the SAME
// creatorUserID NotesCreateNote resolved through the SubjectResolver module
// and stamped on the note and the note-created event -- by layering
// pkgcore.WithActor onto the ctx audit.Emit reads (audit.Emit copies the
// Actor from ctx at emit time; see go/dbkit/audit/emit.go), the exact
// shape go/authn/handler.go's own recordAudit uses for its events. The
// layer is applied HERE, at the narrowest point that needs it, rather than
// by a middleware that would change every surface, because no layer in the
// composed chain populates pkgcore.Actor today (authn's recordAudit doc
// comment says so explicitly), and this handler is the one place that
// knows the creator: the value comes from the SubjectResolver module, never
// from an ambient context value. An empty creatorUserID -- a resolver bug, or
// a create that somehow reached the audit call with no resolved creator
// -- leaves ctx exactly as given, so Emit falls back to its own "no actor
// set" zero value exactly as authn's recordAudit does for an unknown
// actor, never an invented one.
//
// A failure is logged at Error level, not returned: the note itself was
// already committed by the time this runs, so a failure to record its
// audit trail must not turn an otherwise successful create into a 500 for
// the caller -- matching publishNoteCreated's identical reasoning below,
// and the rule that an
// audit-write failure "must alert, must not be silently dropped": an
// Error-level structured log line is what "alert" means at this
// scope, there being no dedicated alerting pipe yet. This and
// publishNoteCreated are deliberately the only two places in this handler
// that both log and do not also return the same error -- logging and
// returning the same error for the SAME failure is what the discipline
// forbids, and here nothing else
// ever surfaces either one.
func (h *Handler) recordNoteCreatedAudit(ctx context.Context, note *Note, creatorUserID string) {
	if h.bus == nil {
		return
	}
	if creatorUserID != "" {
		// Actor.DisplayName is deliberately left empty
		// here because this handler genuinely has no name to record. Its
		// whole knowledge of the creator is the user id its host's
		// SubjectResolver module answered -- the module interface's
		// contract is id-only by documented design (see SubjectResolver
		// at the bottom of this file), and in this app the creator is
		// frequently not an account at all: the demo flows attribute
		// creates to X-Demo-User-Id header values like DemoNotesCreatorUserID,
		// which have no user row behind them, and even a verified-Principal
		// creator (authn.Principal) carries no display name in the token
		// (go/authn/token.go). Filling the label would require the module
		// interface to carry a display name (and the host to resolve one
		// from whatever it verifies), a contract change for this single
		// audit label -- recorded here rather than silently guessed. The
		// row stays fully attributable by id.
		ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: creatorUserID})
	}
	// Resource.DisplayName is deliberately left empty rather than set to
	// note.Text: a note's whole content is arbitrary, caller-supplied free
	// text (up to maxTextLength characters -- see handler.go's own
	// constant), never a short human-readable label the way doc 10's
	// Resource.DisplayName is meant to be, and the audit trail is not the
	// place to duplicate a resource's full body just because a short label
	// is unavailable for this placeholder resource.
	err := audit.Emit(ctx, h.bus, h.auditActions, audit.Input{
		Action:   AuditActionNoteCreate,
		Resource: audit.Resource{Type: "note", ID: note.ID},
		Result:   audit.Result{Success: true},
	})
	if err != nil {
		// See this method's own doc comment above for why tenant_id is not
		// repeated here as an explicit key-value pair: obs.FromContext(ctx)
		// already attaches it.
		obs.FromContext(ctx).Error("notes.note.create audit event emit failed",
			"note_id", note.ID, "error", err)
	}
}

// publishNoteCreated publishes EventNoteCreated for note, using the
// EventBus the Module obtained from the Registry at wiring time (see
// module.go's Register) -- this is the "actual event publish happens
// later, inside the handler" half of that split. creatorUserID is the
// creating user's id resolveSubject resolved from the request (see
// NotesCreateNote), and it is exactly the value subscribers need to route
// the fact to the right recipient -- notification's UserAddressResolver
// consults the same user id at dispatch time. A publish failure is
// logged, not returned: the note itself was already committed by the time
// this runs, so a subscriber's failure must not turn an otherwise
// successful create into a 500 for the caller. See recordNoteCreatedAudit
// above for the identical reasoning applied to this handler's other
// log-not-return call.
func (h *Handler) publishNoteCreated(ctx context.Context, tenant pkgcore.TenantID, note *Note, creatorUserID string) {
	if h.bus == nil {
		return
	}
	evt := pkgcore.Event{
		Type:     EventNoteCreated,
		TenantID: tenant,
		Payload: NoteCreatedPayload{
			NoteID:        note.ID,
			TenantID:      string(tenant),
			CreatorUserID: creatorUserID,
		},
	}
	if err := h.bus.Publish(ctx, evt); err != nil {
		// See NotesCreateNote's comment above on why tenant_id is not
		// repeated here as an explicit key-value pair:
		// obs.FromContext(ctx) already attaches it.
		obs.FromContext(ctx).Error("notes.note.created event publish failed",
			"note_id", note.ID, "error", err)
	}
}

// toNoteResponse converts note to its spec-generated JSON response type,
// api.NotesNote. Every field of NotesNote is optional in api/openapi.yaml,
// hence pointer-typed in Go (see api/oapi-codegen.yaml's comment on the
// pointer default); this handler always sets all three, so every field is
// always present on the wire.
//
// CreatedAt is truncated to the whole second before it is handed to the
// encoder, preserving the wire format of the hand-written noteResponse
// this function replaced (which formatted with time.RFC3339): the
// generated type is a time.Time, which encoding/json renders with
// RFC3339Nano -- byte-identical to RFC3339 whenever the fractional
// second is zero, so truncation is what keeps a created_at like
// "2026-09-03T04:20:00Z" instead of "2026-09-03T04:20:00.123456789Z".
// See handler_test.go's TestHandler_Create_ResponseCreatedAt_IsWholeSeconds,
// which pins that wire shape.
func toNoteResponse(note *Note) api.NotesNote {
	createdAt := note.CreatedAt.Truncate(time.Second)
	return api.NotesNote{
		CreatedAt: &createdAt,
		ID:        &note.ID,
		Text:      &note.Text,
	}
}

// NotesListNotes implements api.ServerInterface: it handles GET
// /api/v1/notes, returning every note belonging to the caller's tenant.
func (h *Handler) NotesListNotes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// This handler runs downstream of tenancy.Middleware exactly like
	// NotesCreateNote above, so ctx already carries the same resolved
	// tenant by the time this runs -- see NotesCreateNote's identical
	// call above for why AnnotateTenant is what actually attaches
	// tenant_id to this request's span from here, rather than at
	// obs.Middleware's own layer. It is a no-op when ctx carries no
	// tenant (see AnnotateTenant's own doc comment), which is exactly the
	// case handler_test.go's
	// TestHandler_List_NoTenantInContext_ReturnsInternalError case
	// exercises: h.repo.List still fails closed on that same missing
	// tenant one line down, unaffected by this call.
	obs.AnnotateTenant(ctx)

	notes, err := h.repo.List(ctx)
	if err != nil {
		writeError(w, err)
		return
	}

	// The slice is always allocated, never left nil, so the pointer is
	// non-nil even for an empty list and "notes" is always present on the
	// wire as [] -- preserving the empty-list bytes of the hand-written
	// listNotesResponse this replaced, and pinned by handler_test.go's
	// TestHandler_List_EmptyTenant_ReturnsNotesEmptyArrayOnWire.
	items := make([]api.NotesNote, 0, len(notes))
	for i := range notes {
		items = append(items, toNoteResponse(&notes[i]))
	}
	resp := api.NotesListNotesResponse{Notes: &items}

	// obs.FromContext(ctx) attaches tenant_id (and trace_id/span_id, once
	// this request's span carries an active one) automatically -- see
	// NotesCreateNote's identical comment above -- so a GET request now
	// leaves behind the same kind of tenant_id-bearing log line a POST
	// already did, instead of none at all.
	obs.FromContext(ctx).Info("notes listed", "note_count", len(items))

	httpapi.WriteJSON(w, http.StatusOK, resp)
}

// NotesDeleteNote implements api.ServerInterface: it handles DELETE
// /api/v1/notes/{noteId}, mark-deleting the caller's tenant's note --
// dbkit.Repository[Note].Delete's soft-delete branch (Note implements
// dbkit.SoftDeletable), which sets deleted_at/deleted_by and hides the row
// from ordinary queries, leaving it restorable through NotesRestoreNote
// until a retention sweep physically reaps it (see
// retention_participant.go). The tenant is read from the request context,
// never from the request -- the same rule NotesCreateNote documents. The
// caller is resolved through the SubjectResolver module and installed as the
// row's deleted_by actor, mirroring how self-service provisioning
// attributes its own user-scoped writes (internal/app/self_service.go): a
// deleted_by attribution that names the real actor is the point of the
// column, and an unattributable request is refused with 401 exactly like
// an unattributable create -- never deleted with an empty attribution.
func (h *Handler) NotesDeleteNote(w http.ResponseWriter, r *http.Request, noteID string) {
	ctx := r.Context()

	// The tenant gate, mirroring NotesCreateNote's identical opening: the
	// repository's own Delete would fail with the same unwrapped
	// pkgcore.ErrNoTenant one layer down, and refusing here keeps the log
	// line specific to this handler.
	if _, err := pkgcore.MustTenantFromContext(ctx); err != nil {
		writeError(w, errInternal.WithCause(err))
		return
	}
	obs.AnnotateTenant(ctx)

	userID, ok := h.resolveSubject(w, r)
	if !ok {
		return
	}
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: userID})

	if err := h.repo.Delete(ctx, noteID); err != nil {
		writeError(w, noteMutationError(err, noteID))
		return
	}

	obs.FromContext(ctx).Info("note deleted", "note_id", noteID, "deleted_by", userID)

	w.WriteHeader(http.StatusNoContent)
}

// NotesRestoreNote implements api.ServerInterface: it handles POST
// /api/v1/notes/{noteId}/restore, undoing NotesDeleteNote's mark -- the
// row's deleted_at/deleted_by are cleared, so the note is visible to the
// list operation again with its text and creator untouched. Restoring a
// note that is not currently deleted under the caller's tenant (live,
// unknown, or another tenant's) answers the same uniform 404 the delete
// operation's own refusal answers (notes.note_not_found); the caller
// resolves through the SubjectResolver module exactly as on the delete path,
// kept for symmetry of attribution even though restore clears rather than
// writes an actor.
func (h *Handler) NotesRestoreNote(w http.ResponseWriter, r *http.Request, noteID string) {
	ctx := r.Context()

	if _, err := pkgcore.MustTenantFromContext(ctx); err != nil {
		writeError(w, errInternal.WithCause(err))
		return
	}
	obs.AnnotateTenant(ctx)

	userID, ok := h.resolveSubject(w, r)
	if !ok {
		return
	}
	ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: userID})

	if err := h.repo.Restore(ctx, noteID); err != nil {
		writeError(w, noteMutationError(err, noteID))
		return
	}

	obs.FromContext(ctx).Info("note restored", "note_id", noteID, "restored_by", userID)

	w.WriteHeader(http.StatusNoContent)
}

// noteMutationError translates the one repository error the two mutation
// operations above surface into the module's own vocabulary before it
// reaches the wire: dbkit's ErrRecordNotFound (a "row not found under
// ctx's tenant in the state the write needs" answer, returned by both
// Delete's soft-delete branch and Restore for exactly the collapsed cases
// their fragment entries document) becomes ErrNoteNotFound, carrying the
// note id the client itself named. Any other error passes through
// unchanged -- an *apperr.Error already speaks a module code (or fails to
// decode as one and is folded into notes.internal_error by writeError).
func noteMutationError(err error, noteID string) error {
	if dbkit.IsRecordNotFound(err) {
		return ErrNoteNotFound.WithParam("note_id", noteID)
	}
	return err
}

// writeError writes err to w as this module's coded error envelope (see
// pkgcore/httpapi): an *apperr.Error keeps its own code and status,
// anything else -- something below this handler did not classify it, such
// as pkgcore.ErrNoTenant -- is folded into errInternal so a caller never
// sees raw Go error text either way.
func writeError(w http.ResponseWriter, err error) {
	httpapi.WriteError(w, err, errInternal)
}

// SubjectResolver is the module interface that answers "who created this
// note" for every create request NotesCreateNote serves. It is declared
// here -- in the file whose only consumer (resolveSubject) reads it --
// the way org declares its own module interface in org's handler package,
// and it is structurally identical to org's SubjectResolver (and
// notification's, which declares the same single method over stdlib types
// only): any type a host wired for org's SubjectResolver satisfies notes'
// too, and none of the modules imports another.
//
// The implementation is the host's to supply: in the reference app, the
// demo identity layer reads the demo acting user's id from the request
// (internal/app/demo/demo_subject.go); in a host running the authn module,
// whatever connects a verified principal to the request answers here. The
// module itself never reads the creator's identity from a header, the
// context or the request body, and it never imports an authenticating
// module's types -- the module interface is the whole of its knowledge of
// who its callers are. NotesCreateNote is the only operation that
// resolves: notes list without one, because listing needs no creator.
//
// A resolver that returns ok=false -- or is not wired at all -- fails
// every create request closed with ErrSubjectUnresolved (see
// resolveSubject): an unattributable request is refused, never served a
// default user or an empty creator in the published event.
type SubjectResolver interface {
	// Subject returns the creating user's id for the request, and whether
	// the request could be attributed to a user at all. A nil resolver, a
	// failing resolver and a resolver that cannot attribute the request
	// all answer ok=false; the handler refuses with ErrSubjectUnresolved
	// in every such case, and treats an empty user id the same as no user.
	Subject(r *http.Request) (userID string, ok bool)
}

// compile-time check that *Handler implements the api.ServerInterface
// generated from this module's api/openapi.yaml -- the enforcement half of
// the spec-first flow: add an operation
// to the fragment, regenerate, and this assertion stops compiling until
// Handler implements it.
var _ api.ServerInterface = (*Handler)(nil)
