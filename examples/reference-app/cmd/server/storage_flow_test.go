package main

// storage_flow_test.go drives the reference app's composed HTTP stack
// through go/storage's whole upload lifecycle -- declare, upload,
// complete, derive, download, delete -- and through the gates around it.
// It follows org_flow_test.go's wire-shape discipline: responses are
// decoded into structs that mirror the JSON on the wire, never into the
// spec-generated types of a module it must not import, so the assertions
// bind to the actual response contract rather than to the generator's Go
// shapes.
//
// The two module fragments deliberately never meet in this file:
// go/storage is exercised only through HTTP, and the demo identity
// machinery (demo_subject.go) is the only thing that knows storage's
// permission names. That mirrors the module boundary the whole repository
// is built on -- the reference app is the host that composes them.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// exifSignature is the ASCII marker that opens every APP1 EXIF payload,
// right after the APP1 segment's own header. Its presence in bytes is the
// tell the sanitization assertions look for.
var exifSignature = []byte("Exif\x00\x00")

// testStorageObject is the wire shape of the spec's StorageObject -- which
// fields are populated depends on the object's state, so presence is
// asserted through the zero values (an uploading row never carries Size or
// ChecksumSha256; a completed row always does). Derivatives ride only on
// the single-object get, so the pointer distinguishes "carried and empty"
// from "not carried at all".
type testStorageObject struct {
	ID               string                   `json:"id"`
	State            string                   `json:"state"`
	DeclaredSize     int64                    `json:"declaredSize"`
	DeclaredType     string                   `json:"declaredType"`
	DeclaredChecksum string                   `json:"declaredChecksum"`
	Size             int64                    `json:"size"`
	MimeType         string                   `json:"mimeType"`
	ChecksumSha256   string                   `json:"checksumSha256"`
	Width            int                      `json:"width"`
	Height           int                      `json:"height"`
	ExpiresAt        string                   `json:"expiresAt"`
	UploadExpiresAt  string                   `json:"uploadExpiresAt"`
	CreatedAt        string                   `json:"createdAt"`
	Derivatives      *[]testStorageDerivative `json:"derivatives"`
}

type testStorageDerivative struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	MimeType  string `json:"mimeType"`
	Size      int64  `json:"size"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	CreatedAt string `json:"createdAt"`
}

type testStorageListResponse struct {
	Objects []testStorageObject `json:"objects"`
}

// storageRequest issues method against path on srv with the given Host
// and acting user. body may be any reader (JSON for the metadata
// endpoints, raw bytes for the content endpoint); when it is non-nil,
// contentType names the body's media type. An empty user sends no demo
// user header at all, which is how an unauthenticated request is
// expressed. The caller owns the returned response's body.
func storageRequest(t *testing.T, srv *httptest.Server, method, path, host, user, contentType string, body io.Reader) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Host = host
	if user != "" {
		req.Header.Set(demoUserHeader, user)
	}
	if body != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s (Host=%s, user=%q): %v", method, path, host, user, err)
	}
	return resp
}

// decodeStorageObject reads resp, requires its status to be wantStatus,
// and decodes its body as a StorageObject wire shape.
func decodeStorageObject(t *testing.T, resp *http.Response, wantStatus int, what string) testStorageObject {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, wantStatus, body)
	}
	var out testStorageObject
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	return out
}

// assertStorageError reads resp and requires it to be the storage
// surface's structured error: status wantStatus and the envelope's code
// exactly wantCode -- not merely "some 4xx".
func assertStorageError(t *testing.T, resp *http.Response, wantStatus int, wantCode, what string) {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, wantStatus, body)
	}
	var decoded struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	if decoded.Code != wantCode {
		t.Fatalf("%s: error code = %q, want %q; body = %s", what, decoded.Code, wantCode, body)
	}
}

// jpegWithExif returns a decodable 48x32 JPEG carrying a genuine EXIF
// APP1 segment: a little-endian TIFF whose IFD0 points at a GPS IFD
// holding GPSLatitudeRef = "N". That is the canonical carrier of location
// metadata the module's completion pipeline must strip before any bytes
// are finalized, and the exact shape the reference app's own sanitization
// proof needs -- a plausible GPS tag, not a placeholder string.
func jpegWithExif(t *testing.T) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 48, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 48; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 5), G: uint8(y * 8), B: 120, A: 255})
		}
	}
	var base bytes.Buffer
	if err := jpeg.Encode(&base, img, nil); err != nil {
		t.Fatalf("encode base jpeg: %v", err)
	}
	raw := base.Bytes()
	if len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xD8 {
		t.Fatalf("encoded jpeg lacks its SOI prefix")
	}

	// The EXIF APP1 payload: "Exif\0\0", then a real TIFF header. IFD0
	// holds one entry -- GPSInfo (0x8825), a LONG living at offset 26 --
	// and the GPS IFD at that offset holds GPSLatitudeRef (0x0001), an
	// ASCII "N". Every offset is absolute from the TIFF header, which
	// begins right after the Exif signature.
	var payload []byte
	payload = append(payload, exifSignature...)
	payload = append(payload, 'I', 'I', '*', 0)                          // TIFF header, little-endian magic
	payload = append(payload, 8, 0, 0, 0)                                // IFD0 lives at file offset 8
	payload = append(payload, 1, 0)                                      // IFD0 holds one entry
	payload = append(payload, 0x25, 0x88, 4, 0, 1, 0, 0, 0, 26, 0, 0, 0) // GPSInfo (0x8825), LONG, offset 26
	payload = append(payload, 0, 0, 0, 0)                                // IFD0 has no next IFD
	payload = append(payload, 1, 0)                                      // GPS IFD holds one entry
	payload = append(payload, 1, 0, 2, 0, 2, 0, 0, 0, 'N', 0, 0, 0)      // GPSLatitudeRef (0x0001), ASCII "N"
	payload = append(payload, 0, 0, 0, 0)                                // GPS IFD has no next IFD

	// Splice the APP1 segment (0xFF 0xE1, big-endian length, payload)
	// directly after the SOI, where a camera's firmware would have
	// written it.
	seg := []byte{0xFF, 0xE1}
	seg = binary.BigEndian.AppendUint16(seg, uint16(len(payload)+2))
	seg = append(seg, payload...)

	out := make([]byte, 0, len(raw)+len(seg))
	out = append(out, raw[:2]...)
	out = append(out, seg...)
	out = append(out, raw[2:]...)
	return out
}

// sha256Hex returns the lowercase hex SHA-256 of b, the form the
// declaration's declaredChecksum travels in.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// waitForDerivative polls the single-object get until the object's
// derivatives array is non-empty -- the asynchronous thumbnail derivation
// running on the app's real standalone queue -- and returns the final
// object. 15 seconds of 20ms polls is far beyond what a local derive
// needs, so a timeout means the derivation genuinely never landed, not
// that the poll was impatient.
func waitForDerivative(t *testing.T, srv *httptest.Server, host, objectID string) testStorageObject {
	t.Helper()

	const deadline = 15 * time.Second
	path := "/api/v1/storage/objects/" + objectID
	start := time.Now()
	for {
		resp := storageRequest(t, srv, http.MethodGet, path, host, demoOwnerUserID, "", nil)
		obj := decodeStorageObject(t, resp, http.StatusOK, "poll GET "+path)
		if obj.Derivatives != nil && len(*obj.Derivatives) > 0 {
			return obj
		}
		if time.Since(start) > deadline {
			t.Fatalf("GET %s: no derivative appeared within %v; object = %+v", path, deadline, obj)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// declareUpload POSTs a create declaration and returns the uploading
// descriptor (the 201 response).
func declareUpload(t *testing.T, srv *httptest.Server, host, user string, declaredSize int64, declaredType, declaredChecksum string) testStorageObject {
	t.Helper()

	body := map[string]any{
		"declaredSize": declaredSize,
		"declaredType": declaredType,
	}
	if declaredChecksum != "" {
		body["declaredChecksum"] = declaredChecksum
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal declaration: %v", err)
	}
	resp := storageRequest(t, srv, http.MethodPost, "/api/v1/storage/objects",
		host, user, "application/json", bytes.NewReader(payload))
	return decodeStorageObject(t, resp, http.StatusCreated, "POST /api/v1/storage/objects")
}

// uploadBytes PUTs content onto the declared object, requiring the 204
// the spec promises for a byte pipe that accepted the stream.
func uploadBytes(t *testing.T, srv *httptest.Server, host, user, objectID string, content []byte) {
	t.Helper()

	resp := storageRequest(t, srv, http.MethodPut,
		"/api/v1/storage/objects/"+objectID+"/content", host, user, "application/octet-stream",
		bytes.NewReader(content))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT content of %s: status = %d, want %d; body = %s",
			objectID, resp.StatusCode, http.StatusNoContent, body)
	}
}

// completeObject POSTs the completion of objectID, returning the finalized
// metadata (or, when wantConflict is set, the raw response whose 409 the
// caller asserts on).
func completeObject(t *testing.T, srv *httptest.Server, host, user, objectID string) testStorageObject {
	t.Helper()

	resp := storageRequest(t, srv, http.MethodPost,
		"/api/v1/storage/objects/"+objectID+"/complete", host, user, "", nil)
	return decodeStorageObject(t, resp, http.StatusOK, "POST complete of "+objectID)
}

// uploadAndComplete walks the whole three-step lifecycle for content
// declared with declaredChecksum (empty to declare none) and returns the
// completed object's metadata.
func uploadAndComplete(t *testing.T, srv *httptest.Server, host string, content []byte, declaredChecksum string) testStorageObject {
	t.Helper()

	declared := declareUpload(t, srv, host, demoOwnerUserID, int64(len(content)), "image/jpeg", declaredChecksum)
	if declared.State != "uploading" {
		t.Fatalf("create response state = %q, want %q", declared.State, "uploading")
	}
	if declared.Size != 0 || declared.ChecksumSha256 != "" {
		t.Fatalf("uploading descriptor carries finalized fields: %+v", declared)
	}
	if declared.UploadExpiresAt == "" {
		t.Fatalf("uploading descriptor carries no uploadExpiresAt: %+v", declared)
	}

	// An uploading row is invisible on the read surface: its descriptor
	// was the create response.
	resp := storageRequest(t, srv, http.MethodGet, "/api/v1/storage/objects/"+declared.ID,
		host, demoOwnerUserID, "", nil)
	assertStorageError(t, resp, http.StatusNotFound, "storage.object_not_found",
		"GET an uploading object before completion")

	uploadBytes(t, srv, host, demoOwnerUserID, declared.ID, content)
	return completeObject(t, srv, host, demoOwnerUserID, declared.ID)
}

// TestBuildServer_StorageFlow_UploadSanitizeDeriveDownloadDelete_EndToEnd
// is the reference app discharging its duty as go/storage's first
// consumer through the real composed stack: tenancy resolution from the
// Host, rbac's gate, the module's real handler, a real SQLite database,
// the app's real standalone queue, and the module's real local object
// store. The whole three-step lifecycle runs over one JPEG carrying a
// genuine GPS-bearing EXIF profile:
//
//  1. declare the upload (with a size and checksum over the real bytes);
//  2. PUT the bytes (EXIF and all) onto the declaration;
//  3. complete, which must strip the EXIF before finalizing -- so the
//     finalized size and checksum describe the SANITIZED bytes, and the
//     content endpoint serves those, never the GPS the uploader sent;
//  4. wait for the thumbnail derivation the completion pipeline enqueued
//     on the real queue, and see it surface through the get response;
//  5. list (metadata only -- no fan-out), and delete, after which the id
//     answers 404 like one that never existed.
func TestBuildServer_StorageFlow_UploadSanitizeDeriveDownloadDelete_EndToEnd(t *testing.T) {
	srv := buildTestServer(t)
	const acmeHost = "acme.demo.localhost"

	jpegBytes := jpegWithExif(t)
	if !bytes.Contains(jpegBytes, exifSignature) {
		t.Fatalf("test jpeg carries no EXIF profile to sanitize")
	}

	// Step 1 + 2 + 3: the lifecycle, completed.
	completed := uploadAndComplete(t, srv, acmeHost, jpegBytes, sha256Hex(jpegBytes))
	if completed.State != "completed" {
		t.Fatalf("complete response state = %q, want %q", completed.State, "completed")
	}
	if completed.Size <= 0 || completed.MimeType != "image/jpeg" || completed.ChecksumSha256 == "" {
		t.Fatalf("completed object lacks its finalized half: %+v", completed)
	}
	if completed.Width != 48 || completed.Height != 32 {
		t.Fatalf("completed dimensions = %dx%d, want 48x32", completed.Width, completed.Height)
	}
	if completed.UploadExpiresAt == "" || completed.CreatedAt == "" {
		t.Fatalf("completed object lost its lifecycle timestamps: %+v", completed)
	}

	// The declared checksum covered the bytes as uploaded (EXIF still
	// present); the finalized checksum covers the sanitized bytes and so
	// differs -- the strongest wire-level sign the pipeline really
	// rewrote the content rather than finalizing it untouched.
	if completed.ChecksumSha256 == sha256Hex(jpegBytes) {
		t.Fatalf("completed checksum still matches the EXIF-carrying upload; nothing was sanitized")
	}

	// Step 4: the derived thumbnail lands asynchronously, through the
	// app's real queue -- the complete response carries no derivatives
	// (its handler never fans out), and polling the single-object get
	// must eventually show one.
	final := waitForDerivative(t, srv, acmeHost, completed.ID)
	derivatives := *final.Derivatives
	if len(derivatives) != 1 {
		t.Fatalf("derivatives = %d, want exactly 1 (%+v)", len(derivatives), derivatives)
	}
	thumb := derivatives[0]
	if thumb.Kind != "thumbnail" {
		t.Fatalf("derivative kind = %q, want %q", thumb.Kind, "thumbnail")
	}
	// The derivation contract never upscales: a source under the host's
	// thumbnail edge is re-encoded at its own size (a module behavior
	// go/storage pins in its own derive tests), so the wire-level
	// property this example can assert without knowing the host's edge
	// is "no larger than the original, and decodable".
	if thumb.Width <= 0 || thumb.Height <= 0 {
		t.Fatalf("thumbnail has no dimensions: %+v", thumb)
	}
	if thumb.Width > completed.Width || thumb.Height > completed.Height {
		t.Fatalf("thumbnail %dx%d upscales the %dx%d original", thumb.Width, thumb.Height, completed.Width, completed.Height)
	}

	// Step 5a: download the sanitized original. It must be a decodable
	// 48x32 JPEG whose EXIF (and its GPS) is gone.
	contentResp := storageRequest(t, srv, http.MethodGet,
		"/api/v1/storage/objects/"+completed.ID+"/content", acmeHost, demoOwnerUserID, "", nil)
	content, err := io.ReadAll(contentResp.Body)
	contentResp.Body.Close()
	if err != nil {
		t.Fatalf("read content response: %v", err)
	}
	if contentResp.StatusCode != http.StatusOK {
		t.Fatalf("GET content: status = %d, want %d; body = %s", contentResp.StatusCode, http.StatusOK, content)
	}
	if bytes.Contains(content, exifSignature) {
		t.Fatalf("content still carries the EXIF profile the completion pipeline must strip")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil {
		t.Fatalf("sanitized content is not a decodable image: %v", err)
	}
	if cfg.Width != 48 || cfg.Height != 32 {
		t.Fatalf("sanitized content dimensions = %dx%d, want 48x32", cfg.Width, cfg.Height)
	}
	if sha256Hex(content) != completed.ChecksumSha256 {
		t.Fatalf("content bytes do not match the finalized checksum %q", completed.ChecksumSha256)
	}

	// Step 5b: the list is metadata only -- the completed object appears,
	// and no item carries a derivatives array (a list must never fan out
	// one query per row).
	listResp := storageRequest(t, srv, http.MethodGet, "/api/v1/storage/objects?limit=10",
		acmeHost, demoOwnerUserID, "", nil)
	listBody, err := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if err != nil {
		t.Fatalf("read list response: %v", err)
	}
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("GET list: status = %d, want %d; body = %s", listResp.StatusCode, http.StatusOK, listBody)
	}
	var listed testStorageListResponse
	if err := json.Unmarshal(listBody, &listed); err != nil {
		t.Fatalf("decoding list %s: %v", listBody, err)
	}
	if len(listed.Objects) != 1 || listed.Objects[0].ID != completed.ID {
		t.Fatalf("list = %+v, want exactly the completed object %s", listed.Objects, completed.ID)
	}
	if listed.Objects[0].Derivatives != nil {
		t.Fatalf("a list item carries derivatives; list must be metadata only: %+v", listed.Objects[0].Derivatives)
	}

	// Step 6: delete. The id then answers 404 -- once, and on every
	// repeat: a second delete is indistinguishable from one that never
	// existed.
	for attempt, want := range []int{http.StatusNoContent, http.StatusNotFound} {
		resp := storageRequest(t, srv, http.MethodDelete, "/api/v1/storage/objects/"+completed.ID,
			acmeHost, demoOwnerUserID, "", nil)
		if attempt == 0 {
			resp.Body.Close()
			if resp.StatusCode != want {
				t.Fatalf("DELETE: status = %d, want %d", resp.StatusCode, want)
			}
		} else {
			assertStorageError(t, resp, want, "storage.object_not_found",
				"DELETE of the already-deleted object")
		}
	}

	// The deleted object's content is gone too -- the bytes went with it.
	resp := storageRequest(t, srv, http.MethodGet,
		"/api/v1/storage/objects/"+completed.ID+"/content", acmeHost, demoOwnerUserID, "", nil)
	assertStorageError(t, resp, http.StatusNotFound, "storage.object_not_found",
		"GET content of the deleted object")
}

// TestBuildServer_StoragePermissionGate_EnforcesTheStoragePermissions is
// the storage mirror of server_test.go's notes gate test: the read-only
// demo user holds notes:read and nothing else -- deliberately no storage
// permission at all -- so on storage's routes it must be refused both
// ways, while the owner passes. The gate answers with rbac's structured
// code, asserted on the code rather than on the status alone.
func TestBuildServer_StoragePermissionGate_EnforcesTheStoragePermissions(t *testing.T) {
	srv := buildTestServer(t)
	const acmeHost = "acme.demo.localhost"

	// The reader may neither declare an upload nor list objects: both
	// directions of the storage:write / storage:read gate are closed.
	resp := storageRequest(t, srv, http.MethodPost, "/api/v1/storage/objects",
		acmeHost, demoReaderUserID, "application/json",
		bytes.NewReader([]byte(`{"declaredSize":10,"declaredType":"text/plain"}`)))
	assertPermissionDenied(t, resp, "POST /api/v1/storage/objects as the read-only demo user")

	resp = storageRequest(t, srv, http.MethodGet, "/api/v1/storage/objects",
		acmeHost, demoReaderUserID, "", nil)
	assertPermissionDenied(t, resp, "GET /api/v1/storage/objects as the read-only demo user")

	// A user with no grant at all is refused too.
	resp = storageRequest(t, srv, http.MethodPost, "/api/v1/storage/objects",
		acmeHost, "nobody", "application/json",
		bytes.NewReader([]byte(`{"declaredSize":10,"declaredType":"text/plain"}`)))
	assertPermissionDenied(t, resp, "POST as an ungranted user")

	// A request with a resolvable tenant and no identity is refused as
	// well. (This example's gate cannot tell anonymous from
	// authenticated-but-grantless, so both answer rbac.permission_denied
	// -- a known demo-identity limitation recorded in the storage round's
	// final report, not a claim about what authn will one day return.)
	resp = storageRequest(t, srv, http.MethodPost, "/api/v1/storage/objects",
		acmeHost, "", "application/json",
		bytes.NewReader([]byte(`{"declaredSize":10,"declaredType":"text/plain"}`)))
	assertPermissionDenied(t, resp, "POST with no demo user header")

	// The owner, by contrast, walks straight through the gate: the 201
	// proves the refusal above was the permission decision, not the
	// storage surface itself failing.
	jpegBytes := jpegWithExif(t)
	declared := declareUpload(t, srv, acmeHost, demoOwnerUserID, int64(len(jpegBytes)), "image/jpeg", "")
	if declared.ID == "" || declared.State != "uploading" {
		t.Fatalf("owner's declaration = %+v, want an uploading object", declared)
	}
}

// TestBuildServer_StorageIsolation_CrossTenantObjectInvisible is the
// storage surface's half of the multi-tenant promise: an object created in
// tenant-acme must be invisible from tenant-globex -- a 404 naming no
// completed object of the caller's tenant, indistinguishable on purpose
// from an id that never existed. The demo-owner user holds the owner role
// in BOTH tenants, so the refusal cannot be the authorization layer's:
// the same user, through the gate, is told the object does not exist.
func TestBuildServer_StorageIsolation_CrossTenantObjectInvisible(t *testing.T) {
	srv := buildTestServer(t)
	const acmeHost = "acme.demo.localhost"
	const globexHost = "globex.demo.localhost"

	jpegBytes := jpegWithExif(t)
	acmeObject := uploadAndComplete(t, srv, acmeHost, jpegBytes, "")

	// tenant-globex, acting as the same owner-role user, cannot see the
	// object...
	resp := storageRequest(t, srv, http.MethodGet, "/api/v1/storage/objects/"+acmeObject.ID,
		globexHost, demoOwnerUserID, "", nil)
	assertStorageError(t, resp, http.StatusNotFound, "storage.object_not_found",
		"GET tenant-acme's object from tenant-globex")

	// ...nor its bytes...
	resp = storageRequest(t, srv, http.MethodGet, "/api/v1/storage/objects/"+acmeObject.ID+"/content",
		globexHost, demoOwnerUserID, "", nil)
	assertStorageError(t, resp, http.StatusNotFound, "storage.object_not_found",
		"GET tenant-acme's content from tenant-globex")

	// ...nor delete it...
	resp = storageRequest(t, srv, http.MethodDelete, "/api/v1/storage/objects/"+acmeObject.ID,
		globexHost, demoOwnerUserID, "", nil)
	assertStorageError(t, resp, http.StatusNotFound, "storage.object_not_found",
		"DELETE tenant-acme's object from tenant-globex")

	// ...and its own list is empty: the other tenant's completed object
	// leaked nowhere. The object in tenant-acme is untouched and still
	// serves its own tenant.
	listResp := storageRequest(t, srv, http.MethodGet, "/api/v1/storage/objects?limit=10",
		globexHost, demoOwnerUserID, "", nil)
	listed := decodeList(t, listResp, "GET list as tenant-globex")
	if len(listed.Objects) != 0 {
		t.Fatalf("tenant-globex's list = %+v, want empty", listed.Objects)
	}

	stillThere := waitForDerivative(t, srv, acmeHost, acmeObject.ID)
	if len(*stillThere.Derivatives) != 1 {
		t.Fatalf("tenant-acme's object lost its derivative after the cross-tenant probes: %+v", stillThere)
	}
}

// decodeList reads a list response body and decodes it, requiring 200.
func decodeList(t *testing.T, resp *http.Response, what string) testStorageListResponse {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s: read body: %v", what, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: status = %d, want %d; body = %s", what, resp.StatusCode, http.StatusOK, body)
	}
	var out testStorageListResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: decoding %s: %v", what, body, err)
	}
	return out
}

// TestBuildServer_StorageComplete_ChecksumMismatchConflicts pins the
// declaration's teeth through the wire: an upload whose bytes do not hash
// to the declared checksum is refused at complete time with storage's
// conflict code -- and the refusal advances nothing, so the object stays
// uploading (and stays invisible on the read surface) afterwards.
func TestBuildServer_StorageComplete_ChecksumMismatchConflicts(t *testing.T) {
	srv := buildTestServer(t)
	const acmeHost = "acme.demo.localhost"

	jpegBytes := jpegWithExif(t)
	// Declare the checksum of different bytes -- same length, so nothing
	// but the hash can catch the lie.
	fake := bytes.Repeat([]byte{0xAB}, len(jpegBytes))
	declared := declareUpload(t, srv, acmeHost, demoOwnerUserID, int64(len(jpegBytes)), "image/jpeg", sha256Hex(fake))
	uploadBytes(t, srv, acmeHost, demoOwnerUserID, declared.ID, jpegBytes)

	resp := storageRequest(t, srv, http.MethodPost,
		"/api/v1/storage/objects/"+declared.ID+"/complete", acmeHost, demoOwnerUserID, "", nil)
	assertStorageError(t, resp, http.StatusConflict, "storage.checksum_mismatch",
		"POST complete with mismatching checksum")

	// The conflict advanced nothing: the row is still uploading, and an
	// uploading row stays invisible -- its 404 proves no finalization
	// slipped through on the conflict path.
	resp = storageRequest(t, srv, http.MethodGet, "/api/v1/storage/objects/"+declared.ID,
		acmeHost, demoOwnerUserID, "", nil)
	assertStorageError(t, resp, http.StatusNotFound, "storage.object_not_found",
		"GET after the refused completion")
}
