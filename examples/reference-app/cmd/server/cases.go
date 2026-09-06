// The reference app's demo glue for product round P2b's Case domain layer
// (internal/cases): the hand-written routes that serve the three
// operations the P3 web UI needs -- create a case (a clinic staff member
// naming a patient and the photos already uploaded through go/storage's
// own HTTP surface), list the caller's own cases, and read one case's
// detail with its photos. cases_flow_test.go drives them through the
// composed HTTP stack.
//
// Like the smilesim and consult routes, these are mounted by hand, outside
// the OpenAPI machinery: no go/ module ships a spec fragment these routes
// could grow into, and the case package deliberately does not join the
// simulation layer (see internal/cases's package doc comment's "Shape
// decision" section) -- a case detail's per-photo simulations stay on the
// P2a enumeration route, fetched per photo by the P3 view. Also like those
// routes, none of these takes a permission check of its own: in this app
// every authenticated member of a tenant may work cases, and the scoping
// that actually protects another tenant's rows -- dbkit's tenant-injecting
// repository and plugin, exactly as for every other tenant-domain table --
// plus the per-request creator attribution through the SubjectResolver
// seam below, are what actually gate access. They are mounted directly on
// mux rather than through reg.Routes/mountModuleRoutes, so none needs (and
// cannot silently skip) an entry in demoRouteGuards' table, the same
// structural argument smilesim.go's own header makes.
package main

import (
	"encoding/json"
	"net/http"

	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/examples/reference-app/internal/cases"
)

// casesPath is the case domain's list-and-create route. POST creates a
// case; GET lists the caller's own cases (see wireCasesRoutes).
const casesPath = "/api/v1/cases"

// caseDetailPathPrefix is the fixed prefix of the case domain's detail
// route: GET caseDetailPathPrefix+"{id}" reads one case of the caller's
// tenant with its photos in attachment order.
const caseDetailPathPrefix = casesPath + "/"

// casesErrInternal folds any error this file's handlers surface that is
// not itself an *apperr.Error into a stable code, the same fallback
// smilesim.go's own writeSmileSimError applies.
var casesErrInternal = apperr.Internal("cases.internal_error")

// caseCreateRequestBody is the create route's JSON body: the clinic-given
// patient record (name required, reference optional) and the case's
// initial photos as already-uploaded go/storage object ids, in attachment
// order. Both are optional lists allowed to be absent or empty -- a case
// can be created for an intake whose photos arrive later. There is
// deliberately no tenant_id field and no creator field anywhere on the
// request: the tenant is the one tenancy.Middleware already resolved into
// the request context, and the creator comes from the SubjectResolver seam
// below -- encoding/json's default decoder silently ignores any unknown
// field a client does send (such as a forged tenant_id or creator_user_id),
// the identical documented behavior of notes' create handler.
type caseCreateRequestBody struct {
	PatientName    string   `json:"patient_name"`
	PatientRef     string   `json:"patient_ref"`
	PhotoObjectIDs []string `json:"photo_object_ids"`
}

// wireCasesRoutes mounts casesPath and caseDetailPathPrefix+"{id}" on mux,
// backed by svc and resolving the acting user through subject -- the
// attribution seam whose answers become case rows' CreatorUserID and the
// "my cases" list's key (internal/cases's SubjectResolver doc comment).
// A request no resolver can attribute is refused with the seam's coded 401
// (cases.subject_unresolved) before the body is even read, exactly the
// order notes' create handler follows.
func wireCasesRoutes(mux *http.ServeMux, svc *cases.Service, subject cases.SubjectResolver) {
	mux.HandleFunc(http.MethodPost+" "+casesPath, func(w http.ResponseWriter, r *http.Request) {
		creatorUserID, ok := resolveCasesSubject(w, subject, r)
		if !ok {
			return
		}

		var body caseCreateRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeCasesError(w, apperr.Invalid("cases.invalid_request_body").WithCause(err))
			return
		}

		created, photos, err := svc.Create(r.Context(), cases.CreateInput{
			PatientName:    body.PatientName,
			PatientRef:     body.PatientRef,
			CreatorUserID:  creatorUserID,
			PhotoObjectIDs: body.PhotoObjectIDs,
		})
		if err != nil {
			writeCasesError(w, err)
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(caseResponse(created, photos))
	})

	mux.HandleFunc(http.MethodGet+" "+casesPath, func(w http.ResponseWriter, r *http.Request) {
		creatorUserID, ok := resolveCasesSubject(w, subject, r)
		if !ok {
			return
		}

		list, err := svc.ListByCreator(r.Context(), creatorUserID)
		if err != nil {
			writeCasesError(w, err)
			return
		}

		entries := make([]map[string]any, 0, len(list))
		for _, c := range list {
			entries = append(entries, caseResponse(c, nil))
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"cases": entries})
	})

	mux.HandleFunc(http.MethodGet+" "+caseDetailPathPrefix+"{id}", func(w http.ResponseWriter, r *http.Request) {
		record, photos, err := svc.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			writeCasesError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(caseResponse(record, photos))
	})
}

// resolveCasesSubject resolves the acting user through the host's
// SubjectResolver seam, refusing with the seam's coded 401 -- never an
// invented or empty creator -- when no resolver is wired, when it cannot
// attribute the request, or when it returns an empty user id (an empty id
// is treated exactly like no id, so a seam bug can never smuggle an empty
// creator into a case row or a list key). It is this surface's one and
// only source of the creator, mirroring notes' handler.resolveSubject.
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

// caseResponse renders a case (with its photos, when given) as the JSON
// shape both the create answer and the detail answer share -- one shape a
// P3 view can render with one component. created_at is RFC3339 in UTC, the
// same wire format smilesim's routes use. An empty photos slice renders as
// [] (never null): the detail of a photo-less case and the list entries
// both stay honest about what exists.
func caseResponse(record cases.Case, photos []cases.Photo) map[string]any {
	resp := map[string]any{
		"id":              record.ID,
		"patient_name":    record.PatientName,
		"patient_ref":     record.PatientRef,
		"creator_user_id": record.CreatorUserID,
		"created_at":      record.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
	photoEntries := make([]map[string]any, 0, len(photos))
	for _, photo := range photos {
		photoEntries = append(photoEntries, map[string]any{"object_id": photo.ObjectID})
	}
	resp["photos"] = photoEntries
	return resp
}

// writeCasesError writes err to w as a JSON {code, params} body, the same
// structured-error envelope shape smilesim.go's own writeSmileSimError
// produces.
func writeCasesError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = casesErrInternal
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(appErr.Status)
	envelope := map[string]any{"code": appErr.Code}
	if appErr.Params != nil {
		envelope["params"] = appErr.Params
	}
	_ = json.NewEncoder(w).Encode(envelope)
}

// compile-time check that the demo notes resolver also satisfies the cases
// package's identical copy of the attribution seam (one host type, two
// same-layer declarations -- see internal/cases's SubjectResolver doc
// comment).
var _ cases.SubjectResolver = demoNotesSubjectResolver{}
