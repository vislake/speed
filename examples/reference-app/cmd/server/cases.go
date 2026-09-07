// The reference app's case-domain HTTP surface: the app-side
// implementation of the spec-derived interface generated from
// internal/cases/api/openapi.yaml. Product round P3a promoted the three
// hand-written routes (product round P2b's demo glue for internal/cases)
// to that fragment: this type implements the generated
// casesapi.ServerInterface (compile-time-checked below), and its routing
// is registered by the generated api.HandlerFromMux helper, which
// derives this surface's method+path patterns from the "paths:" keys of
// the spec fragment itself -- replacing what used to be a hand-written
// registration of the same patterns, one less copy of path+method truth
// to keep in step with the spec by hand. The fragment joins the merged
// application document, so the operations ship in the generated
// @speed/api-sdk surface the P3 web UI calls. cases_flow_test.go drives
// them through the composed HTTP stack.
//
// The five operations the P3 web UI needs: upload a patient photo (the
// one-shot go/storage protocol wrapper the block-A round added, in
// cases_photos.go), create a case (a clinic staff member naming a
// patient and the photos already uploaded through that upload op), list
// the caller's tenant's cases, read one case's detail with its photos,
// and read one photo's bytes for the case view to render. A case
// detail's per-photo simulations deliberately stay on the smile-
// simulation surface (see internal/cases's package doc comment's
// "Shape decision" section) -- fetched per photo by the P3 view from
// the P3a fragment that file's sibling cmd/server/smilesim.go
// implements.
//
// None of these operations takes a permission check of its own: in this
// app every authenticated member of a tenant may work cases, and the
// scoping that actually protects another tenant's rows -- dbkit's
// tenant-injecting repository and plugin, exactly as for every other
// tenant-domain table -- plus the per-request creator attribution
// through the SubjectResolver seam below (create only: the case row's
// recorded CreatorUserID), are what actually gate access. Only the
// create route resolves a creator; the list, detail, upload and
// photo-content routes need none. Like the smile-simulation surface,
// these routes are mounted directly on mux rather than through
// reg.Routes/mountModuleRoutes, so none needs (and cannot silently
// skip) an entry in demoRouteGuards' table, the same structural
// argument smilesim.go's own header makes.
package main

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/storage"

	"github.com/vislake/speed/examples/reference-app/internal/cases"
	casesapi "github.com/vislake/speed/examples/reference-app/internal/cases/api"
)

// casesErrInternal folds any error this file's handlers surface that is
// not itself an *apperr.Error into a stable code, the same fallback
// smilesim.go's own writeSmileSimError applies.
var casesErrInternal = apperr.Internal("cases.internal_error")

// casesInvalidRequestBody is the malformed-or-oversized request body
// answer every body-reading cases route writes. It exists as a named
// sentinel (rather than the inline apperr.Invalid(...) creation the
// routes used to carry) so the error-mapping audits and the shell's
// codes-alignment suite can cite a stable declaration site, exactly as
// the notes surface's own handler sentinels are cited.
var casesInvalidRequestBody = apperr.Invalid("cases.invalid_request_body")

// casesMaxRequestBodyBytes bounds a create-case request body BEFORE it is
// decoded, mirroring go/authn/handler.go's maxRequestBodyBytes and notes'
// own handler.go constant of the same value and reasoning: the body feeds
// an unbounded json.Decoder before any validation has had a chance to
// refuse it, so an arbitrarily large body would otherwise be buffered in
// full. A body any legitimate create-case request can produce stays far
// below the bound -- the request's own limits (a 200-rune patient name, a
// 64-rune reference, 50 photo object ids of at most 64 runes) sum to a few
// kilobytes.
const casesMaxRequestBodyBytes = 1 << 16

// casesHandler implements casesapi.ServerInterface -- the app-side
// implementation of the spec fragment's five operations -- backed by svc
// and resolving the acting user through subject, the attribution seam
// whose answer becomes a case row's CreatorUserID (internal/cases's
// SubjectResolver doc comment). objects is the app's go/storage
// ObjectService, which the photo-upload and photo-content operations
// (cases_photos.go) drive and read -- the same instance the storage
// module's own HTTP surface serves, so the app-side handlers and the
// module's surface agree on what an object is.
type casesHandler struct {
	svc     *cases.Service
	subject cases.SubjectResolver
	objects *storage.ObjectService
}

// compile-time check that casesHandler implements every operation the
// spec fragment declares -- a spec change whose generated interface
// outgrew this file stops the app from compiling.
var _ casesapi.ServerInterface = (*casesHandler)(nil)

// wireCasesRoutes mounts the case domain's five routes on mux, backed
// by svc, subject and the storage objects service, through the generated
// api.HandlerFromMux helper: the mount patterns come from
// internal/cases/api/openapi.yaml itself, never a second hand-written
// copy. A create request no resolver can attribute is refused with the
// seam's coded 401 (cases.subject_unresolved) before the body is even
// read, exactly the order notes' create handler follows.
func wireCasesRoutes(mux *http.ServeMux, svc *cases.Service, subject cases.SubjectResolver, objects *storage.ObjectService) {
	casesapi.HandlerFromMux(&casesHandler{svc: svc, subject: subject, objects: objects}, mux)
}

// CasesCreateCase implements casesapi.ServerInterface: it handles POST
// /api/v1/cases, creating a case under the caller's tenant. The request
// body is decoded into the spec-generated casesapi.CasesCreateCaseRequest
// -- the clinic-given patient record (name required, reference optional)
// and the case's initial photos as already-uploaded go/storage object
// ids, in attachment order, both optional lists allowed to be absent or
// empty -- a case can be created for an intake whose photos arrive
// later. There is deliberately no tenant_id field and no creator field
// anywhere on the request: the tenant is the one tenancy.Middleware
// already resolved into the request context, and the creator comes from
// the SubjectResolver seam below (encoding/json's default decoder
// silently ignores any unknown field a client does send, such as a
// forged tenant_id or creator_user_id), the identical documented
// behavior of notes' create handler.
func (h *casesHandler) CasesCreateCase(w http.ResponseWriter, r *http.Request) {
	creatorUserID, ok := resolveCasesSubject(w, h.subject, r)
	if !ok {
		return
	}

	// The body is bounded by casesMaxRequestBodyBytes BEFORE decoding, the
	// identical shape notes' create handler and go/authn/handler.go's
	// decodeJSON apply: a body that exceeds the bound fails with the same
	// invalid-request-body error as malformed JSON, as soon as the read
	// passes the limit rather than after the whole body has been buffered.
	// Passing w lets net/http ask the server to close the connection after
	// the oversized request.
	r.Body = http.MaxBytesReader(w, r.Body, casesMaxRequestBodyBytes)
	var body casesapi.CasesCreateCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeCasesError(w, casesInvalidRequestBody.WithCause(err))
		return
	}

	// PatientRef and PhotoObjectIds are spec-optional, hence
	// pointer-shaped in the generated request type: an absent field maps
	// to the domain layer's documented "gave none"/"no photos yet"
	// sentinels (empty string, empty list), exactly as the hand-written
	// request type's plain-string/plain-slice fields used to.
	patientRef := ""
	if body.PatientRef != nil {
		patientRef = *body.PatientRef
	}
	var photoObjectIDs []string
	if body.PhotoObjectIds != nil {
		photoObjectIDs = *body.PhotoObjectIds
	}

	created, photos, err := h.svc.Create(r.Context(), cases.CreateInput{
		PatientName:    body.PatientName,
		PatientRef:     patientRef,
		CreatorUserID:  creatorUserID,
		PhotoObjectIDs: photoObjectIDs,
	})
	if err != nil {
		writeCasesError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toCasesCase(created, photos))
}

// CasesListCases implements casesapi.ServerInterface: it handles GET
// /api/v1/cases, listing every case of the caller's tenant, newest
// first -- the clinic-wide list the block-A product decision names (see
// the spec fragment's own description). The list needs no creator
// attribution: any authenticated member of the tenant may read every
// case of the tenant, so no subject is resolved here.
func (h *casesHandler) CasesListCases(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.List(r.Context())
	if err != nil {
		writeCasesError(w, err)
		return
	}

	entries := make([]casesapi.CasesCase, 0, len(list))
	for _, c := range list {
		entries = append(entries, toCasesCase(c, nil))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(casesapi.CasesListResponse{Cases: entries})
}

// CasesGetCase implements casesapi.ServerInterface: it handles GET
// /api/v1/cases/{caseId}, reading one case of the caller's tenant with
// its photos in attachment order. An unknown case id, and a case of
// another tenant, both answer cases.not_found -- never a cross-tenant
// confirmation of a case's existence. Like the notes list, this read
// needs no creator, so no subject is resolved.
func (h *casesHandler) CasesGetCase(w http.ResponseWriter, r *http.Request, caseID string) {
	record, photos, err := h.svc.Get(r.Context(), caseID)
	if err != nil {
		writeCasesError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(toCasesCase(record, photos))
}

// resolveCasesSubject resolves the acting user through the host's
// SubjectResolver seam, refusing with the seam's coded 401 -- never an
// invented or empty creator -- when no resolver is wired, when it cannot
// attribute the request, or when it returns an empty user id (an empty
// id is treated exactly like no id, so a seam bug can never smuggle an
// empty creator into a case row). It is this surface's one and only
// source of the creator, and the create route is its only caller -- the
// list, detail, upload and photo-content routes read no subject --
// mirroring notes' handler.resolveSubject.
func resolveCasesSubject(w http.ResponseWriter, subject cases.SubjectResolver, r *http.Request) (string, bool) {
	if subject == nil {
		writeCasesError(w, cases.ErrSubjectUnresolved)
		return "", false
	}
	userID, ok := subject.Subject(r)
	if !ok || userID == "" {
		writeCasesError(w, cases.ErrSubjectUnresolved)
		return "", false
	}
	return userID, true
}

// toCasesCase renders a case (with its photos, when given) as the
// spec-generated wire type both the create answer and the detail answer
// share -- one shape a P3 view can render with one component. CreatedAt
// renders on the wire in the same whole-seconds UTC form the hand-written
// route used: the stored time is UTC (gorm's autoCreateTime round-trips
// it so), and truncating the sub-second part keeps encoding/json's
// RFC3339Nano rendering byte-identical to the old RFC3339 output. An
// empty photos slice renders as [] (never null): the detail of a
// photo-less case and the list entries both stay honest about what
// exists.
func toCasesCase(record cases.Case, photos []cases.Photo) casesapi.CasesCase {
	photoEntries := make([]casesapi.CasesPhoto, 0, len(photos))
	for _, photo := range photos {
		photoEntries = append(photoEntries, casesapi.CasesPhoto{ObjectID: photo.ObjectID})
	}
	return casesapi.CasesCase{
		ID:            record.ID,
		PatientName:   record.PatientName,
		PatientRef:    record.PatientRef,
		CreatorUserID: record.CreatorUserID,
		CreatedAt:     record.CreatedAt.Truncate(time.Second),
		Photos:        photoEntries,
	}
}

// writeCasesError writes err to w as a JSON {code, params} body, the same
// structured-error envelope shape smilesim.go's own writeSmileSimError
// produces, encoded through the spec fragment's CasesError type.
func writeCasesError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = casesErrInternal
	}
	envelope := casesapi.CasesError{Code: &appErr.Code}
	// Params stays nil (and thus omitted, per its omitempty tag) unless
	// the error actually carries parameters: a pointer to a nil map would
	// marshal as "params": null instead of the key being absent, which is
	// not the shape the hand-written envelope produced.
	if appErr.Params != nil {
		envelope.Params = &appErr.Params
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(envelope)
}

// compile-time check that the demo notes resolver also satisfies the cases
// package's identical copy of the attribution seam (one host type, two
// same-layer declarations -- see internal/cases's SubjectResolver doc
// comment).
var _ cases.SubjectResolver = demoNotesSubjectResolver{}
