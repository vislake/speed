package storage

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/storage/api"
)

// jsonContentType is the Content-Type every response below writes, matching
// notes', config's and org's own handler constant of the same name.
const jsonContentType = "application/json; charset=utf-8"

// The page-size bound this module's surface promises. The spec's
// storage_listObjects documents the same 1-200 window with a default of 50;
// the constants below are what the handler actually enforces. They exist in
// code because the spec cannot enforce anything: the generated parameter
// binding accepts any integer, and the bound is this module's promise to its
// consumers -- the same two-source split the declaredType note in
// StorageCreateObject's doc comment records for request bodies.
const (
	minListLimit = 1
	maxListLimit = 200
)

// Handler serves storage's HTTP endpoints by implementing the spec-generated
// api.ServerInterface (api/storage-server.gen.go, regenerated from this
// module's api/openapi.yaml by task api:gen -- the compile-time assertion at
// the bottom of this file is what makes "spec changed, handler not" a
// compile failure instead of a runtime surprise). The seven operations it
// implements are the whole surface the spec defines: the three-step upload
// lifecycle (storage_createObject, storage_uploadObjectContent,
// storage_completeObject), object reads (storage_listObjects,
// storage_getObject, storage_getObjectContent) and object deletion
// (storage_deleteObject).
//
// It must run downstream of tenancy.Middleware on a non-allowlisted path:
// every method reads the tenant tenancy.Middleware already resolved into the
// request context -- via pkgcore.MustTenantFromContext -- and never from a
// request parameter, header or body, per root CLAUDE.md's multi-tenant
// isolation rule. There is no tenant_id anywhere on this surface, exactly as
// the spec's own header records.
//
// Handler performs no data access of its own: it drives the same services
// and repositories Module.Register attached to the host's registry --
// ObjectService (m.svc) for the lifecycle, LifecycleService (m.life) for
// deletion, and the two repositories for the two lookups no service method
// expresses. The delete pre-read exists because the service's own contract
// converges an absent row on success while the HTTP surface promises 404
// (see StorageDeleteObject's doc comment); the get's derivative listing
// exists because no service method serves the array the spec's getObject
// carries (list responses are metadata only, per the spec's own note).
type Handler struct {
	svc         *ObjectService
	life        *LifecycleService
	objects     *ObjectRepository
	derivatives *DerivativeRepository
	mux         *http.ServeMux
}

// NewHandler returns a Handler serving the lifecycle, reads and deletion of
// the module's objects through the given service, lifecycle service and
// repositories -- the instances Module.Register mounts it behind, in the
// same call that attaches it at apiPath. The returned Handler's routing is
// registered by the generated api.HandlerFromMux helper: it derives this
// module's method+path patterns from the "paths:" keys of api/openapi.yaml
// itself, exactly as org's NewHandler does for its own fragment.
func NewHandler(svc *ObjectService, life *LifecycleService, objects *ObjectRepository, derivatives *DerivativeRepository) *Handler {
	h := &Handler{svc: svc, life: life, objects: objects, derivatives: derivatives}
	h.mux = http.NewServeMux()
	api.HandlerFromMux(h, h.mux)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// mustTenant resolves the caller's tenant, annotating the request's span.
// Unreachable in normal operation -- see org's identical comment on its own
// equivalent call -- because a host must never allowlist storage's routes,
// so tenancy.Middleware has already rejected anything that could reach here
// with no resolved tenant. Handled anyway, never assumed away: every
// repository call underneath would otherwise fail closed with this exact
// unwrapped error one layer down, with a less specific log line.
func mustTenant(w http.ResponseWriter, r *http.Request) (pkgcore.TenantID, bool) {
	ctx := r.Context()
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		writeError(w, ErrInternal.WithCause(err))
		return "", false
	}
	observability.AnnotateTenant(ctx)
	return tenant, true
}

// decodeJSON decodes r's body into dst, writing ErrInvalidRequestBody and
// reporting false on any decode failure. Only the one operation with a
// request body -- createObject -- calls it.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, ErrInvalidRequestBody.WithCause(err))
		return false
	}
	return true
}

// StorageListObjects implements api.ServerInterface: GET
// /api/v1/storage/objects. Returns the tenant's completed objects, newest
// first, at most limit rows (default 50) -- or the page of older objects
// after the row named by beforeId when the cursor is present. Responses are
// metadata only: no derivatives array, no bytes, per the spec's own note.
func (h *Handler) StorageListObjects(w http.ResponseWriter, r *http.Request, params api.StorageListObjectsParams) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}

	limit := defaultListPageSize
	if params.Limit != nil {
		limit = *params.Limit
		if limit < minListLimit || limit > maxListLimit {
			writeError(w, ErrInvalidLimit.
				WithParam("limit", limit).
				WithParam("min", minListLimit).
				WithParam("max", maxListLimit))
			return
		}
	}
	beforeID := ""
	if params.BeforeID != nil {
		beforeID = *params.BeforeID
	}
	objects, err := h.svc.List(ctx, limit, beforeID)
	if err != nil {
		writeError(w, err)
		return
	}
	// An explicit make, so an empty page marshals as [] -- never null -- and
	// the response always carries the objects array the schema promises.
	items := make([]api.StorageObject, 0, len(objects))
	for i := range objects {
		items = append(items, toObjectResponse(&objects[i]))
	}
	writeJSON(w, http.StatusOK, api.StorageListObjectsResponse{Objects: &items})
}

// StorageCreateObject implements api.ServerInterface: POST
// /api/v1/storage/objects. Declares an upload and returns its descriptor --
// the object in ObjectStateUploading with its declared metadata, an
// uploadExpiresAt deadline the caller's upload must beat, and nothing else.
//
// The declared type is passed through to ObjectService exactly as decoded:
// the spec marks declaredType required with a minLength of 1, but the
// generated model cannot enforce that -- an absent or empty value decodes
// to the empty string, and the module ships no transport error code for it
// (errors.go's catalog is the whole surface, and no code is a lie if the
// value it guards is legal). The empty string IS legal: ObjectService's own
// contract defines "" as "no type declared", which Create accepts without
// an allowlist check -- and the gap closes at complete time regardless,
// because the stored bytes' probed media type must always pass the
// allowlist and must match the declared type when one exists (object.go's
// Complete doc records the full gate). A completion can therefore never
// smuggle an undeclared type past the module's restrictions, and a caller
// who declared none simply gets probed enforcement instead of declared
// enforcement.
//
// The expiry ceiling is likewise split between the two layers: retention
// beyond the configured maximum is refused by ObjectService (which owns the
// bound), while an ExpiresAt already past -- or exactly now -- is refused
// here, because time.Until of it has no positive lifetime and passing it
// through would silently convert the caller's explicit deadline into "never
// expires". Both halves write the same storage.invalid_expiry with the same
// parameter, so a caller cannot tell the two refusals apart, because it
// does not need to.
func (h *Handler) StorageCreateObject(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	var req api.StorageCreateObjectRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	retention := time.Duration(0)
	if req.ExpiresAt != nil {
		retention = time.Until(*req.ExpiresAt)
		if retention <= 0 {
			writeError(w, ErrInvalidExpiry.WithParam("max_lifetime_days",
				int64(h.svc.cfg.maxObjectLifetime/(24*time.Hour))))
			return
		}
	}
	checksum := ""
	if req.DeclaredChecksum != nil {
		checksum = *req.DeclaredChecksum
	}

	row, err := h.svc.Create(ctx, CreateParams{
		DeclaredSize:     req.DeclaredSize,
		DeclaredType:     req.DeclaredType,
		DeclaredChecksum: checksum,
		Retention:        retention,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	observability.FromContext(ctx).Info("storage object declared", "object_id", row.ID)
	writeJSON(w, http.StatusCreated, toObjectResponse(&row))
}

// StorageGetObject implements api.ServerInterface: GET
// /api/v1/storage/objects/{objectId}. Returns the full metadata of a
// completed object -- the only state the module serves reads for, so an
// id that is uploading, deleting or absent all answer 404, per the spec's
// own description. This endpoint is where the derivatives array appears:
// the listing promise is "no fan-out per row", so list responses stay
// metadata-only and a get carries the array -- empty when no derivation has
// run for the object yet, never null.
func (h *Handler) StorageGetObject(w http.ResponseWriter, r *http.Request, objectID api.ObjectID) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	obj, err := h.svc.Get(ctx, objectID)
	if err != nil {
		writeError(w, err)
		return
	}
	derivatives, err := h.derivatives.listByObject(ctx, objectID)
	if err != nil {
		writeError(w, err)
		return
	}
	items := make([]api.StorageObjectDerivative, 0, len(derivatives))
	for i := range derivatives {
		items = append(items, toDerivativeResponse(&derivatives[i]))
	}
	resp := toObjectResponse(&obj)
	resp.Derivatives = &items
	writeJSON(w, http.StatusOK, resp)
}

// StorageDeleteObject implements api.ServerInterface: DELETE
// /api/v1/storage/objects/{objectId}.
//
// The handler pre-reads the row before delegating to LifecycleService for
// one reason: the HTTP surface's 404 promise. LifecycleService.Delete
// converges an absent row on nil on purpose -- its contract is written for
// the sweep and for racing protocol runs, where "nothing left to delete"
// is success -- but the spec promises the caller that an id naming no
// completed or deleting object answers 404, "so a repeated delete answers
// 404 after the first 204". The pre-read decides between the three answers
// the surface defines before the service protocol runs: an uploading row is
// refused with storage.object_uploading (only the expiry sweep reclaims
// uploads), an absent row -- or one that belongs to another tenant, which
// reads as absent -- is storage.object_not_found, and a completed or
// deleting row is handed to LifecycleService.Delete, whose remaining
// answers all mean 204: a deleting row's deletion is resumed to completion
// exactly as the spec's description promises, and a row that a racing
// deletion removed in the window between the pre-read and the protocol run
// is converged on 204 as well. The pre-read is the reason Handler holds the
// module's object repository at all.
func (h *Handler) StorageDeleteObject(w http.ResponseWriter, r *http.Request, objectID api.ObjectID) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	row, err := findObjectByID(ctx, h.objects, objectID)
	if err != nil {
		writeError(w, err)
		return
	}
	if row.State == ObjectStateUploading {
		writeError(w, ErrObjectUploading.WithParam("id", objectID))
		return
	}
	if err := h.life.Delete(ctx, objectID); err != nil {
		writeError(w, err)
		return
	}
	observability.FromContext(ctx).Info("storage object deleted", "object_id", objectID)
	w.WriteHeader(http.StatusNoContent)
}

// StorageCompleteObject implements api.ServerInterface: POST
// /api/v1/storage/objects/{objectId}/complete. Runs the revalidation
// pipeline over the uploaded bytes -- size, checksum, probed media type
// against the allowlist and against the declared type, image dimensions --
// and finalizes the object when every check passes. The response is the
// finalized metadata; thumbnail derivation is enqueued asynchronously and
// surfaces through getObject's derivatives array, never in this response.
func (h *Handler) StorageCompleteObject(w http.ResponseWriter, r *http.Request, objectID api.ObjectID) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	row, err := h.svc.Complete(ctx, objectID)
	if err != nil {
		writeError(w, err)
		return
	}
	observability.FromContext(ctx).Info("storage object completed", "object_id", row.ID)
	writeJSON(w, http.StatusOK, toObjectResponse(&row))
}

// StorageUploadObjectContent implements api.ServerInterface: PUT
// /api/v1/storage/objects/{objectId}/content. A dumb byte pipe by design --
// the spec says so itself: the media type was declared at create and is
// probed from the stored bytes at complete, and the declared checksum is
// verified there too, so nothing here inspects the body or its Content-Type
// header. The Content-Length is handed to ObjectService when the request
// carries one (a chunked upload carries none), and the service compares it
// against the declared size before a byte of the body is read.
func (h *Handler) StorageUploadObjectContent(w http.ResponseWriter, r *http.Request, objectID api.ObjectID) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	var contentLength *int64
	if r.ContentLength >= 0 {
		contentLength = &r.ContentLength
	}
	if err := h.svc.Upload(ctx, objectID, contentLength, r.Body); err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// StorageGetObjectContent implements api.ServerInterface: GET
// /api/v1/storage/objects/{objectId}/content. Streams the stored bytes of a
// completed object with the media type probed at complete time and the
// finalized byte count. A stream that dies midway cannot report an error --
// the status line has already been sent, and nothing after WriteHeader can
// change the answer -- so it is logged and left to the client's own
// truncated-body detection; likewise a close failure is logged, never
// raised.
func (h *Handler) StorageGetObjectContent(w http.ResponseWriter, r *http.Request, objectID api.ObjectID) {
	ctx := r.Context()
	if _, ok := mustTenant(w, r); !ok {
		return
	}
	obj, rc, err := h.svc.OpenContent(ctx, objectID)
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() {
		if err := rc.Close(); err != nil {
			observability.FromContext(ctx).Warn("object content stream close failed",
				"object_id", objectID, "error", err)
		}
	}()

	mime := "application/octet-stream"
	if obj.MIME != nil {
		mime = *obj.MIME
	}
	w.Header().Set("Content-Type", mime)
	if obj.Size != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(*obj.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		observability.FromContext(ctx).Warn("object content stream failed",
			"object_id", objectID, "error", err)
	}
}

// toObjectResponse converts obj to its spec-generated JSON response type.
// Every field of api.StorageObject is optional (hence pointer-typed) in the
// generated model, and which ones are present on the wire deliberately
// mirrors the object's state -- the "which fields are populated depends on
// the object's state" rule the spec's StorageObject description records.
// The declared half is always present (an uploading row's descriptor is the
// create response, and the finalize never erases what was declared), the
// finalized half only when the pipeline has written it, and the row's
// absent optional values -- no declared checksum, no requested expiry, a
// width a non-image object never gained -- stay absent on the wire rather
// than becoming empty strings or zeros.
//
// Derivatives are deliberately never populated here: only the get handler
// carries them, per the spec's own note that list responses are metadata
// only.
func toObjectResponse(obj *Object) api.StorageObject {
	resp := api.StorageObject{
		ID:              &obj.ID,
		State:           &obj.State,
		DeclaredSize:    &obj.DeclaredSize,
		CreatedAt:       &obj.CreatedAt,
		UploadExpiresAt: &obj.UploadExpiresAt,
		Size:            obj.Size,
		MimeType:        obj.MIME,
		ChecksumSha256:  obj.ChecksumSHA256,
		Width:           obj.Width,
		Height:          obj.Height,
		ExpiresAt:       obj.ExpiresAt,
	}
	if obj.DeclaredType != "" {
		resp.DeclaredType = &obj.DeclaredType
	}
	if obj.DeclaredChecksum != "" {
		resp.DeclaredChecksum = &obj.DeclaredChecksum
	}
	return resp
}

// toDerivativeResponse converts d to its spec-generated JSON response
// type.
func toDerivativeResponse(d *ObjectDerivative) api.StorageObjectDerivative {
	return api.StorageObjectDerivative{
		ID:        &d.ID,
		Kind:      &d.Kind,
		MimeType:  &d.MIME,
		Size:      &d.Size,
		Width:     d.Width,
		Height:    d.Height,
		CreatedAt: &d.CreatedAt,
	}
}

// writeJSON writes v to w as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes err to w as a JSON {code, params} body -- the
// spec-generated api.StorageError, the same structured-error envelope
// notes', config's and org's own writeError produce. An err that is not an
// *apperr.Error -- meaning something below this handler did not classify it
// -- is folded into ErrInternal so a caller never sees raw Go error text
// (which could carry an object-store path, or a cause never meant for the
// wire; the cause stays on the trace).
func writeError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = ErrInternal
	}
	envelope := api.StorageError{Code: &appErr.Code}
	if appErr.Params != nil {
		envelope.Params = &appErr.Params
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(envelope)
}

// compile-time check that *Handler implements the api.ServerInterface
// generated from this module's api/openapi.yaml -- the enforcement half of
// the spec-first flow (docs/internal/21-api-contract.md): add an operation
// to the fragment, regenerate, and this assertion stops compiling until
// Handler implements it.
var _ api.ServerInterface = (*Handler)(nil)
