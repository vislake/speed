package flowtests

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/apptest"
	"github.com/vislake/speed/examples/reference-app/internal/testutil"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/examples/reference-app/internal/app/demo"

	"github.com/vislake/speed/go/storage"
)

// This file drives the case surface's two photo operations
// (internal/app/cases_photos.go) through the real composed HTTP stack:
// POST /api/v1/cases/photos/upload runs go/storage's three-step protocol
// in-process, and GET /api/v1/cases/{caseId}/photos/{photoObjectID}/content
// serves one attached photo's stored bytes. The photos are real -- a
// genuine JPEG, EXIF-bearing like a phone photo -- and every read and
// refusal goes through the generated routes a browser would call.

// uploadPhotoAs POSTs one photo's base64 to the upload route, asserting
// the 201 and returning the decoded answer.
func uploadPhotoAs(t *testing.T, srv *httptest.Server, token string, contentBase64 string) struct {
	ObjectID string `json:"object_id"`
} {
	t.Helper()
	body, err := json.Marshal(map[string]string{"content_base64": contentBase64})
	if err != nil {
		t.Fatalf("marshal upload body: %v", err)
	}
	resp := testutil.CasesRequestAs(t, srv, http.MethodPost, casesPhotosUploadPath, token, "", bytes.NewReader(body))
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST %s status = %d, want 201; body = %s", casesPhotosUploadPath, resp.StatusCode, raw)
	}
	var out struct {
		ObjectID string `json:"object_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode upload answer %s: %v", raw, err)
	}
	if out.ObjectID == "" {
		t.Fatalf("upload answer carried no object_id: %s", raw)
	}
	return out
}

// photoContentAs GETs one attached photo's content as token, asserting
// the 200 and returning the decoded answer.
func photoContentAs(t *testing.T, srv *httptest.Server, token, caseID, photoObjectID string) struct {
	MediaType     string `json:"media_type"`
	ContentBase64 string `json:"content_base64"`
} {
	t.Helper()
	path := casePhotoContentPath(caseID, photoObjectID)
	resp := testutil.CasesRequestAs(t, srv, http.MethodGet, path, token, "", nil)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200; body = %s", path, resp.StatusCode, raw)
	}
	var out struct {
		MediaType     string `json:"media_type"`
		ContentBase64 string `json:"content_base64"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode content answer %s: %v", raw, err)
	}
	return out
}

// TestCasesPhotos_UploadThenContentOnCase_Journey is the photo half of
// the case-opening journey through the real composed stack: a
// clinic user uploads a real EXIF-bearing JPEG through the new upload
// op, creates a case naming the resulting object, and reads the photo
// back through the content op -- with the served bytes byte-identical to
// what the storage module's own content surface serves for the same
// object (both are the sanitized, EXIF-stripped form the completion
// pipeline finalized, which is also what the browser's blob URL would
// render).
func TestCasesPhotos_UploadThenContentOnCase_Journey(t *testing.T) {
	srv, cfg, _ := apptest.BuildServer(t)
	acmeToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-photos")

	jpegBytes := jpegWithExif(t)
	if !bytes.Contains(jpegBytes, exifSignature) {
		t.Fatalf("test jpeg carries no EXIF profile to strip")
	}
	uploaded := uploadPhotoAs(t, srv, acmeToken, base64.StdEncoding.EncodeToString(jpegBytes))

	created := testutil.CreateCaseAs(t, srv, acmeToken, "", testutil.CaseCreateBody{
		PatientName:    "Photo Journey Patient",
		PhotoObjectIDs: []string{uploaded.ObjectID},
	})
	if len(created.Photos) != 1 || created.Photos[0].ObjectID != uploaded.ObjectID {
		t.Fatalf("created case photos = %+v, want exactly the uploaded object", created.Photos)
	}

	content := photoContentAs(t, srv, acmeToken, created.ID, uploaded.ObjectID)
	if content.MediaType != "image/jpeg" {
		t.Fatalf("content media_type = %q, want image/jpeg (the probe's answer)", content.MediaType)
	}
	decoded, err := base64.StdEncoding.DecodeString(content.ContentBase64)
	if err != nil {
		t.Fatalf("content base64 does not decode: %v", err)
	}
	if len(decoded) == 0 {
		t.Fatal("content decoded to no bytes")
	}

	// The served bytes are the completed object's sanitized form -- the
	// EXIF (a GPS latitude) is gone -- byte-identical to what the storage
	// module's own content surface serves for the same object.
	if bytes.Contains(decoded, exifSignature) {
		t.Fatal("served photo content still carries the EXIF profile the completion pipeline must strip")
	}
	storageContent := storageRequest(t, srv, http.MethodGet, "/api/v1/storage/objects/"+uploaded.ObjectID+"/content",
		acmeToken, demo.DemoOwnerUserID, "", nil)
	defer storageContent.Body.Close()
	rawStorage, err := io.ReadAll(storageContent.Body)
	if err != nil {
		t.Fatalf("read storage content body: %v", err)
	}
	if storageContent.StatusCode != http.StatusOK {
		t.Fatalf("storage content status = %d, want 200", storageContent.StatusCode)
	}
	if !bytes.Equal(decoded, rawStorage) {
		t.Fatalf("cases content bytes differ from the storage surface's own content bytes (%d vs %d)", len(decoded), len(rawStorage))
	}
}

// TestCasesPhotos_UploadRefusals pins the upload route's coded refusals:
// an absent payload, an undecodable one, and bytes the storage probe
// refuses (not an image) each answer their own code, and none of them
// leaves anything behind (the refusing tenant can still upload and
// attach afterwards).
func TestCasesPhotos_UploadRefusals(t *testing.T) {
	srv, cfg, _ := apptest.BuildServer(t)
	acmeToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-photos-refusals")

	// An absent/blank content_base64 is a malformed request.
	empty := testutil.CasesRequestAs(t, srv, http.MethodPost, casesPhotosUploadPath, acmeToken, "",
		bytes.NewReader([]byte(`{"content_base64":"   "}`)))
	assertCasesError(t, empty, http.StatusBadRequest, "cases.photo_content_required", "upload with blank content")

	// Not base64 at all.
	junk := testutil.CasesRequestAs(t, srv, http.MethodPost, casesPhotosUploadPath, acmeToken, "",
		bytes.NewReader([]byte(`{"content_base64":"!!!not-base64!!!"}`)))
	assertCasesError(t, junk, http.StatusBadRequest, "cases.photo_content_invalid", "upload with undecodable content")

	// Perfectly good base64 of bytes that are not an image: the storage
	// probe refuses them, and the refusal surfaces as the surface's own
	// coded rejection (never a storage.* code leaking through a cases
	// route).
	notAnImage := testutil.CasesRequestAs(t, srv, http.MethodPost, casesPhotosUploadPath, acmeToken, "",
		bytes.NewReader([]byte(`{"content_base64":"`+base64.StdEncoding.EncodeToString([]byte("hello, not an image"))+`"}`)))
	assertCasesError(t, notAnImage, http.StatusBadRequest, "cases.photo_rejected", "upload content the probe refuses")

	// The refused attempts left nothing blocking: the same tenant can
	// still upload a real photo and attach it to a case.
	uploaded := uploadPhotoAs(t, srv, acmeToken, base64.StdEncoding.EncodeToString(jpegWithExif(t)))
	_ = testutil.CreateCaseAs(t, srv, acmeToken, "", testutil.CaseCreateBody{
		PatientName:    "After the refusals",
		PhotoObjectIDs: []string{uploaded.ObjectID},
	})
}

// TestCasesPhotos_ContentReadRefusals pins the content route's coded
// refusals: an unknown case, a photo object id the case does not carry,
// and another tenant's view of the same case each answer their own
// refusal -- and the photo-not-found answer is the same whether the
// object id never existed or was never attached here.
func TestCasesPhotos_ContentReadRefusals(t *testing.T) {
	srv, cfg, _ := apptest.BuildServer(t)
	acmeToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-photos-content-refusals")
	globexToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-globex", "cases-photos-content-foreign")

	uploaded := uploadPhotoAs(t, srv, acmeToken, base64.StdEncoding.EncodeToString(jpegWithExif(t)))
	created := testutil.CreateCaseAs(t, srv, acmeToken, "", testutil.CaseCreateBody{
		PatientName:    "Content Refusal Patient",
		PhotoObjectIDs: []string{uploaded.ObjectID},
	})

	// An unknown case id answers the case's own not-found.
	unknownCase := testutil.CasesRequestAs(t, srv, http.MethodGet, casePhotoContentPath("no-such-case", uploaded.ObjectID), acmeToken, "", nil)
	assertCasesError(t, unknownCase, http.StatusNotFound, "cases.not_found", "content of an unknown case")

	// A real storage object the case does not carry answers photo-not-
	// found, indistinguishable from an object id that never existed.
	stray := uploadPhotoAs(t, srv, acmeToken, base64.StdEncoding.EncodeToString(jpegWithExif(t)))
	notAttached := testutil.CasesRequestAs(t, srv, http.MethodGet, casePhotoContentPath(created.ID, stray.ObjectID), acmeToken, "", nil)
	assertCasesError(t, notAttached, http.StatusNotFound, "cases.photo_not_found", "content of an un-attached photo")

	// Another tenant's view of the same case answers the case's own
	// not-found before any storage read: no cross-tenant probe of whether
	// an object exists behind a case.
	foreign := testutil.CasesRequestAs(t, srv, http.MethodGet, casePhotoContentPath(created.ID, uploaded.ObjectID), globexToken, "", nil)
	assertCasesError(t, foreign, http.StatusNotFound, "cases.not_found", "content of another tenant's case")
}

// TestCasesPhotos_UploadOversize_Refused pins the upload route's byte
// bound: a decodable payload whose decoded length passes MaxPhotoBytes
// is refused with the coded too-large answer before the storage protocol
// runs -- the route never stores a photo the serve path could not answer.
func TestCasesPhotos_UploadOversize_Refused(t *testing.T) {
	srv, cfg, _ := apptest.BuildServer(t)
	acmeToken := apptest.RegisterAndAuthenticate(t, srv, cfg, "tenant-acme", "cases-photos-oversize")

	oversize := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'x'}, app.MaxPhotoBytes+1))
	resp := testutil.CasesRequestAs(t, srv, http.MethodPost, casesPhotosUploadPath, acmeToken, "",
		strings.NewReader(`{"content_base64":"`+oversize+`"}`))
	assertCasesError(t, resp, http.StatusBadRequest, "cases.photo_content_too_large", "upload beyond the photo byte bound")
}

// TestCasesPhotoReadError_MapsTheObjectNotFoundCodeToThePhotoAnswer pins
// CasesPhotoReadError's classification directly: a content read whose
// object no longer exists -- a deleted or reclaimed object, which the
// storage module answers with the decorated storage.object_not_found
// code (matched by Code, never identity, exactly as apperr.HasCode
// documents) -- maps onto the same cases.photo_not_found a case that
// never referenced the object answers, and any other failure maps onto
// the internal fallback carrying the original cause.
func TestCasesPhotoReadError_MapsTheObjectNotFoundCodeToThePhotoAnswer(t *testing.T) {
	missing := storage.ErrObjectNotFound.WithParam("id", "reclaimed-object")
	got := app.CasesPhotoReadError(missing)
	if got.Code != app.ErrPhotoNotFound.Code {
		t.Fatalf("CasesPhotoReadError(object-not-found) code = %q, want %q", got.Code, app.ErrPhotoNotFound.Code)
	}

	other := app.CasesPhotoReadError(context.DeadlineExceeded)
	if other.Code != "cases.internal_error" {
		t.Fatalf("CasesPhotoReadError(raw failure) code = %q, want the internal fallback", other.Code)
	}
	if !errors.Is(other, context.DeadlineExceeded) {
		t.Error("CasesPhotoReadError(raw failure) dropped the original cause, want it preserved")
	}
}
