// The photo half of the case-domain surface: the two operations
// cases_uploadPhoto (POST /api/v1/cases/photos/upload) and
// cases_getPhotoContent (GET /api/v1/cases/{caseId}/photos/
// {photoObjectID}/content), both implemented on casesHandler alongside
// the case operations cases.go declares. They are what make the P3
// web UI's photo story real: a browser page can only reach the backend
// through the generated operations over @speed/api-client's JSON-only
// transport, so the photo bytes travel base64-encoded in JSON in both
// directions, and the app-side handler is where go/storage's own
// three-step upload protocol actually runs -- never a second wire
// protocol the browser would have to speak to the storage surface
// directly (which no generated operation covers and no JSON transport
// can carry).
//
// Upload runs the protocol in full: Create with the size and SHA-256
// checksum computed from the decoded bytes (so no declared-vs-arrived
// mismatch is possible), Upload with exactly those bytes, Complete with
// the stored bytes probed and sanitized by go/storage's own pipeline.
// The probe is the authority on what the file really is: a byte
// sequence that is not an allowlisted, decodable, pixel-bounded image
// is refused with cases.photo_rejected (carrying the storage code that
// refused it in params), and the uploading row the refusal leaves
// behind is reclaimed by storage's own expiry sweep like any other
// abandoned upload. A successful upload answers the completed object's
// id -- the value a case create's photo_object_ids list then references
// -- and the object becomes visible under this tenant to the storage
// module's own reads exactly like any object uploaded through its HTTP
// surface.
//
// The content read serves one attached photo's stored bytes for the
// case view to render (a blob URL built from the decoded bytes). The
// photo is addressed by its object id -- the value the case detail's
// photos entries carry -- after the case read itself has established
// that the case is the caller's tenant's: the same cases.not_found
// answer as the detail route for an unknown case, and
// cases.photo_not_found for an object id the case does not carry (or
// whose stored bytes no longer exist), so no read can probe another
// tenant's attachments or a storage object this case never referenced.
//
// Both operations deliberately resolve no creator (see cases.go's
// header): they are tenant-member work like the list and detail reads,
// and the tenant is the one the middleware chain already resolved.

package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/storage"

	casesapi "github.com/vislake/speed/examples/reference-app/internal/cases/api"
)

// The photo surface's own coded errors. Each is a structured code plus
// optional params, never localized text (backend coding standard §6.2) --
// the consuming UI maps code to bilingual text through its own catalog,
// exactly like the case-domain codes internal/cases ships. They live here
// rather than in internal/cases because they are surface orchestration,
// not domain facts: internal/cases records references to go/storage
// objects and knows nothing of bytes; the surface is where the storage
// protocol runs and where its refusals become cases-coded answers.
var (
	// ErrPhotoContentRequired is returned when a photo-upload request's
	// content_base64 is absent or all whitespace -- a malformed request,
	// refused before anything is decoded.
	ErrPhotoContentRequired = apperr.Invalid("cases.photo_content_required")

	// ErrPhotoContentInvalid is returned when a photo-upload request's
	// content_base64 is not valid base64, or decodes to no bytes at all
	// (a zero-length "photo" is not a photo any probe could accept).
	ErrPhotoContentInvalid = apperr.Invalid("cases.photo_content_invalid")

	// ErrPhotoContentTooLarge is returned when a photo's decoded bytes
	// exceed MaxPhotoBytes -- the bound this surface accepts in one
	// request and serves in one answer -- with the limit and the actual
	// length in params. The same code answers the content read when a
	// stored photo exceeds the serve bound.
	ErrPhotoContentTooLarge = apperr.Invalid("cases.photo_content_too_large")

	// ErrPhotoRejected is returned when go/storage's own probe of the
	// stored bytes refuses them: not an allowlisted image type, or not
	// decodable within the module's pixel ceiling. The storage code
	// that refused the bytes rides in params as `reason`, so an
	// operator reading logs can tell type refusal from pixel refusal
	// while the UI's single bilingual text covers both.
	ErrPhotoRejected = apperr.Invalid("cases.photo_rejected")

	// ErrPhotoNotFound is returned when a photo-content request names a
	// photo object id the case does not carry, or whose stored bytes
	// no longer exist -- deliberately the same outward refusal for
	// both, so no request can probe whether a storage object still
	// exists behind a case that does not reference it.
	ErrPhotoNotFound = apperr.NotFound("cases.photo_not_found")
)

// MaxPhotoBytes is the byte ceiling one photo may carry in this
// surface: the decoded length a photo-upload request may submit and the
// decoded length a photo-content answer may serve. It is deliberately
// far below go/storage's own per-object ceiling (the module default is
// 100 MiB): a patient photo is a few megabytes, and the bytes travel
// base64-encoded in JSON both ways, so an unbounded photo would mean an
// unbounded JSON body and an unbounded JSON answer -- the two buffers
// this surface must bound.
const MaxPhotoBytes = 20 << 20

// photoUploadMaxRequestBodyBytes bounds a photo-upload request body
// BEFORE it is decoded, the same MaxBytesReader reasoning as
// casesMaxRequestBodyBytes: base64 inflates bytes by 4/3, so a body any
// in-bounds photo can produce stays under MaxPhotoBytes*4/3 plus JSON
// overhead -- the slack below leaves room for the envelope and
// whitespace while refusing an arbitrarily large payload.
const photoUploadMaxRequestBodyBytes = 32 << 20

// CasesUploadPhoto implements casesapi.ServerInterface: it handles POST
// /api/v1/cases/photos/upload, running go/storage's three-step upload
// protocol over the request's base64-encoded bytes and answering the
// completed object's id. See this file's header for the full contract.
func (h *casesHandler) CasesUploadPhoto(w http.ResponseWriter, r *http.Request) {
	// The body is bounded before decoding, exactly as the create route
	// bounds its own (see casesMaxRequestBodyBytes): the base64 payload
	// feeds a decoder that would otherwise buffer whatever arrives.
	r.Body = http.MaxBytesReader(w, r.Body, photoUploadMaxRequestBodyBytes)
	var body casesapi.CasesUploadPhotoRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeCasesError(w, casesInvalidRequestBody.WithCause(err))
		return
	}

	contentBase64 := strings.TrimSpace(body.ContentBase64)
	if contentBase64 == "" {
		writeCasesError(w, ErrPhotoContentRequired)
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(contentBase64)
	if err != nil {
		writeCasesError(w, ErrPhotoContentInvalid.WithCause(err))
		return
	}
	if len(decoded) == 0 {
		writeCasesError(w, ErrPhotoContentInvalid)
		return
	}
	if int64(len(decoded)) > MaxPhotoBytes {
		writeCasesError(w, ErrPhotoContentTooLarge.
			WithParam("limit", MaxPhotoBytes).
			WithParam("length", len(decoded)))
		return
	}

	// The declaration is computed from the bytes, never claimed by the
	// caller: size and checksum are the decoded content's own facts, so
	// the protocol's declared-vs-arrived checks cannot trip on an honest
	// exchange and the probe at Complete remains the one authority on
	// what the bytes are. No media type is declared -- the probe assigns
	// the real one.
	declaredSize := int64(len(decoded))
	checksum := sha256.Sum256(decoded)
	declaredChecksum := hex.EncodeToString(checksum[:])

	ctx := r.Context()
	obj, err := h.objects.Create(ctx, storage.CreateParams{
		DeclaredSize:     declaredSize,
		DeclaredChecksum: declaredChecksum,
	})
	if err != nil {
		writeCasesError(w, casesPhotoProtocolError(err, "create"))
		return
	}
	if err := h.objects.Upload(ctx, obj.ID, &declaredSize, bytes.NewReader(decoded)); err != nil {
		writeCasesError(w, casesPhotoProtocolError(err, "upload"))
		return
	}
	if _, err := h.objects.Complete(ctx, obj.ID); err != nil {
		writeCasesError(w, casesPhotoProtocolError(err, "complete"))
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(casesapi.CasesUploadPhotoResponse{ObjectID: obj.ID})
}

// CasesGetPhotoContent implements casesapi.ServerInterface: it handles
// GET /api/v1/cases/{caseId}/photos/{photoObjectID}/content, serving
// one photo of one case of the caller's tenant -- the stored bytes,
// base64-encoded with the media type storage's probe assigned at
// complete time -- for the case view to render. See this file's header
// for the full contract.
func (h *casesHandler) CasesGetPhotoContent(w http.ResponseWriter, r *http.Request, caseID string, photoObjectID string) {
	// The case read is the tenant gate: an unknown case id, and a case
	// of another tenant, both answer cases.not_found before any storage
	// read runs -- never a cross-tenant probe of whether an object
	// exists.
	_, photos, err := h.svc.Get(r.Context(), caseID)
	if err != nil {
		writeCasesError(w, err)
		return
	}
	attached := false
	for _, photo := range photos {
		if photo.ObjectID == photoObjectID {
			attached = true
			break
		}
	}
	if !attached {
		writeCasesError(w, ErrPhotoNotFound)
		return
	}

	obj, rc, err := h.objects.OpenContent(r.Context(), photoObjectID)
	if err != nil {
		writeCasesError(w, CasesPhotoReadError(err))
		return
	}
	defer func() { _ = rc.Close() }()

	// The serve bound mirrors the upload bound: a completed object any
	// honest upload produced is within it, and a larger one (created
	// through another surface) is refused rather than buffered in full.
	raw, err := io.ReadAll(io.LimitReader(rc, MaxPhotoBytes+1))
	if err != nil {
		writeCasesError(w, apperr.Internal("cases.internal_error").WithCause(err))
		return
	}
	if int64(len(raw)) > MaxPhotoBytes {
		writeCasesError(w, ErrPhotoContentTooLarge.
			WithParam("limit", MaxPhotoBytes).
			WithParam("length", len(raw)))
		return
	}

	mediaType := "application/octet-stream"
	if obj.MIME != nil && *obj.MIME != "" {
		mediaType = *obj.MIME
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(casesapi.CasesPhotoContent{
		MediaType:     mediaType,
		ContentBase64: base64.StdEncoding.EncodeToString(raw),
	})
}

// casesPhotoProtocolError maps a refusal from the storage protocol the
// upload op runs onto this surface's coded errors -- never a storage.*
// code leaking through a cases route. stage names the protocol step in
// the wrapped internal error.
func casesPhotoProtocolError(err error, stage string) *apperr.Error {
	switch {
	case hasCasesPhotoCode(err, storage.ErrTypeNotAllowed.Code),
		hasCasesPhotoCode(err, storage.ErrPixelLimitExceeded.Code),
		hasCasesPhotoCode(err, storage.ErrImageUnreadable.Code):
		// The probe refused the bytes: not an acceptable image (the
		// declared-type mismatch is deliberately absent -- this op
		// declares no type, so storage's type_mismatch is unreachable
		// here). The refusing code rides in params for operators; the
		// UI's single text covers the refusal class.
		code := storage.ErrTypeNotAllowed.Code
		if appErr, ok := apperr.As(err); ok {
			code = appErr.Code
		}
		return ErrPhotoRejected.WithParam("reason", code)
	default:
		// Every other refusal is an internal failure of the exchange:
		// a store error, a row reclaimed mid-protocol by the expiry
		// sweep, an object not in the state the step expected. None is
		// a client fact the caller can act on differently.
		return apperr.Internal("cases.internal_error").WithCause(err).
			WithParam("stage", stage)
	}
}

// CasesPhotoReadError maps a refusal from the content read onto this
// surface's coded errors: an object that no longer exists (deleted or
// reclaimed) answers the same cases.photo_not_found a case that never
// referenced it answers, and every other failure is internal.
func CasesPhotoReadError(err error) *apperr.Error {
	if hasCasesPhotoCode(err, storage.ErrObjectNotFound.Code) {
		return ErrPhotoNotFound
	}
	return apperr.Internal("cases.internal_error").WithCause(err)
}

// hasCasesPhotoCode reports whether err is, or wraps, an *apperr.Error
// carrying code, matched by Code rather than by identity (apperr.WithParam
// always derives a new *apperr.Error, so pointer identity is not stable
// across decoration).
func hasCasesPhotoCode(err error, code string) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == code
}
