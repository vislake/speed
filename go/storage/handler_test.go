package storage

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/storage/api"
	"github.com/vislake/speed/go/storage/internal/testutil"
)

// This file tests Handler, the module's HTTP surface: the seven operations
// the api/ fragment defines, served behind the spec-generated
// api.ServerInterface (api/storage-server.gen.go) -- the enforcement half of
// the spec-first flow. Every test drives whole requests through Handler's own
// mux -- the same routing Module.Register mounts at apiPath -- over the real
// services and repositories sharing one migrated database (so each test is
// also a proof the migration files apply), with the host seams on the
// file-local fakes object_test.go defines (fakeStore, recordingQueue,
// recordingBus). The tenant travels in the request context exactly as
// tenancy.Middleware would have resolved it; there is no tenant anywhere on
// the wire.
//
// The behavioural core underneath -- revalidation, sanitization, the delete
// protocol -- is pinned in object_test.go, validate_test.go, sanitize_test.go
// and cleanup_test.go. This file pins the surface: which status and which
// {code, params} envelope each refusal writes, which fields each response
// carries (and which it never does -- no derivatives member on create and
// list bodies), and which headers the byte endpoints set. White-box seeding
// through the repositories is deliberate where no service method can produce
// a state the surface must answer for (deleting rows, foreign rows).

// newHandlerHarness returns a Handler over the module's real composition --
// one migrated database, one ObjectRepository and one DerivativeRepository
// shared by ObjectService, LifecycleService and Handler -- with the host
// seams attached to fresh fakes. Tests assert on the returned fakes (bytes
// under which key, which task was enqueued) and reach the shared repositories
// through the handler's own fields (h.objects, h.life) to seed rows no
// service lifecycle produces.
func newHandlerHarness(t *testing.T) (*Handler, *fakeStore, *recordingQueue) {
	t.Helper()
	store := newFakeStore()
	queue := &recordingQueue{}
	host := &fakeHost{store: store, bus: newRecordingBus()}
	db := newTestDB(t)
	objects := NewObjectRepository(db)
	derivatives := NewDerivativeRepository(db)
	svc := newObjectService(objects, queue, testServiceConfig())
	svc.host = host
	life := newLifecycleService(objects, derivatives, queue)
	life.host = host
	return NewHandler(svc, life, objects, derivatives), store, queue
}

// request drives one request against h with the tenant in the request context
// -- the exact position tenancy.Middleware leaves it for a non-allowlisted
// route -- and returns the recorder. A nil body sends no bytes.
func request(t *testing.T, h *Handler, tenant pkgcore.TenantID, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	req = req.WithContext(serviceCtx(tenant))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// bodyFor turns a test's declared body value into the raw bytes a request
// sends. A nil value sends no body at all; an []byte IS the payload and
// passes through verbatim (marshaling one would base64 it into a JSON
// string); anything else is marshaled as JSON.
func bodyFor(t *testing.T, v any) []byte {
	t.Helper()
	if raw, ok := v.([]byte); ok {
		return raw
	}
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal request body %v: %v", v, err)
	}
	return raw
}

// envelope is the wire shape of the api.StorageError response body, decoded
// loosely (a plain struct instead of the generated pointer-typed model) so a
// test can read params without nil dances.
type envelope struct {
	Code   *string        `json:"code"`
	Params map[string]any `json:"params"`
}

// assertEnvelope fails t unless the response carries wantStatus and a JSON
// envelope with exactly wantCode, returning the decoded envelope for param
// assertions.
func assertEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode string) envelope {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, wantStatus, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != jsonContentType {
		t.Errorf("Content-Type = %q, want %q", ct, jsonContentType)
	}
	var e envelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("response is not the {code, params} envelope: %v (%s)", err, rec.Body.String())
	}
	if e.Code == nil {
		t.Fatalf("response carries no code: %s", rec.Body.String())
	}
	if *e.Code != wantCode {
		t.Fatalf("code = %q, want %q (body %s)", *e.Code, wantCode, rec.Body.String())
	}
	return e
}

// assertObject decodes raw as an api.StorageObject, failing on any error.
func assertObject(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int) api.StorageObject {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, wantStatus, rec.Body.String())
	}
	var obj api.StorageObject
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatalf("response is not an object: %v (%s)", err, rec.Body.String())
	}
	return obj
}

// lifecycle creates, uploads and completes one object of tenant whose content
// is body, returning the finalized metadata the complete response carried.
// The name recalls the service-level createAndUpload helper: this is its
// handler-level form.
func lifecycle(t *testing.T, h *Handler, tenant pkgcore.TenantID, body []byte, declaredType string) api.StorageObject {
	t.Helper()
	created := assertObject(t, request(t, h, tenant, http.MethodPost, apiPath+"/objects", bodyFor(t, map[string]any{
		"declaredSize": int64(len(body)),
		"declaredType": declaredType,
	})), http.StatusCreated)
	if got := request(t, h, tenant, http.MethodPut, apiPath+"/objects/"+*created.ID+"/content", body); got.Code != http.StatusNoContent {
		t.Fatalf("upload status = %d, want 204 (body %s)", got.Code, got.Body.String())
	}
	return assertObject(t, request(t, h, tenant, http.MethodPost, apiPath+"/objects/"+*created.ID+"/complete", nil), http.StatusOK)
}

func TestHandler_StorageCreateObject_DeclaresAnUpload(t *testing.T) {
	h, store, queue := newHandlerHarness(t)
	rec := request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects", bodyFor(t, map[string]any{
		"declaredSize": 4096,
		"declaredType": "Image/Jpeg;foo=bar", // a free-form header: canonicalized server-side
	}))
	obj := assertObject(t, rec, http.StatusCreated)
	if rec.Header().Get("Content-Type") != jsonContentType {
		t.Errorf("Content-Type = %q, want %q", rec.Header().Get("Content-Type"), jsonContentType)
	}

	if obj.ID == nil || len(*obj.ID) != objectIDLen {
		t.Fatalf("id = %v, want a %d-character UUID", obj.ID, objectIDLen)
	}
	if obj.State == nil || *obj.State != ObjectStateUploading {
		t.Errorf("state = %v, want %q", obj.State, ObjectStateUploading)
	}
	if obj.DeclaredSize == nil || *obj.DeclaredSize != 4096 {
		t.Errorf("declaredSize = %v, want 4096", obj.DeclaredSize)
	}
	if obj.DeclaredType == nil || *obj.DeclaredType != "image/jpeg" {
		t.Errorf("declaredType = %v, want the canonical image/jpeg", obj.DeclaredType)
	}
	if obj.UploadExpiresAt == nil || !obj.UploadExpiresAt.After(time.Now()) {
		t.Errorf("uploadExpiresAt = %v, want a future window", obj.UploadExpiresAt)
	}
	if obj.ExpiresAt != nil {
		t.Errorf("expiresAt = %v, want absent on an object that never expires", obj.ExpiresAt)
	}
	// The finalized half stays absent on the wire, never empty strings.
	if obj.Size != nil || obj.MimeType != nil || obj.ChecksumSha256 != nil || obj.Width != nil || obj.Height != nil {
		t.Errorf("finalized fields present on an uploading row: %+v", obj)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("derivatives")) {
		t.Error("create response carries a derivatives member; only getObject carries the array")
	}

	// The declaration is a metadata row only: no bytes reached the store, and
	// nothing was enqueued.
	if len(store.objects) != 0 {
		t.Errorf("store holds %d keys after a bare declaration", len(store.objects))
	}
	if len(queue.tasks) != 0 {
		t.Errorf("queue holds %d tasks after a bare declaration", len(queue.tasks))
	}
	row, err := h.objects.FindByID(serviceCtx("tenant-a"), *obj.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	if row.State != ObjectStateUploading {
		t.Errorf("row state = %q, want %q", row.State, ObjectStateUploading)
	}
}

// TestHandler_StorageCreateObject_RefusesInvalidDeclarations pins the 400
// answers of the declare step: every refusal is a {code, params} envelope,
// and none of them leaves any trace behind -- no row, no bytes.
func TestHandler_StorageCreateObject_RefusesInvalidDeclarations(t *testing.T) {
	cases := []struct {
		name       string
		body       any
		wantCode   string
		wantParams map[string]any
	}{
		{
			name:     "malformed body",
			body:     []byte("{not json"),
			wantCode: ErrInvalidRequestBody.Code,
		},
		{
			name:     "missing body",
			body:     nil,
			wantCode: ErrInvalidRequestBody.Code,
		},
		{
			name:     "zero declared size",
			body:     map[string]any{"declaredSize": 0, "declaredType": "image/jpeg"},
			wantCode: ErrInvalidSize.Code,
		},
		{
			name:     "declared size over the ceiling",
			body:     map[string]any{"declaredSize": 1<<20 + 1, "declaredType": "image/jpeg"},
			wantCode: ErrObjectTooLarge.Code,
			wantParams: map[string]any{
				"max_bytes": float64(1 << 20), // JSON numbers decode as float64
			},
		},
		{
			name:     "declared type outside the allowlist",
			body:     map[string]any{"declaredSize": 10, "declaredType": "image/gif"},
			wantCode: ErrTypeNotAllowed.Code,
			wantParams: map[string]any{
				"allowed": "image/jpeg,image/png",
			},
		},
		{
			name:     "malformed declared checksum",
			body:     map[string]any{"declaredSize": 10, "declaredType": "image/jpeg", "declaredChecksum": "nope"},
			wantCode: ErrInvalidChecksum.Code,
		},
		{
			name: "retention deadline already past",
			body: map[string]any{
				"declaredSize": 10,
				"declaredType": "image/jpeg",
				"expiresAt":    time.Now().Add(-time.Hour),
			},
			wantCode: ErrInvalidExpiry.Code,
			wantParams: map[string]any{
				"max_lifetime_days": float64(90),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, store, _ := newHandlerHarness(t)
			e := assertEnvelope(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects", bodyFor(t, tc.body)), http.StatusBadRequest, tc.wantCode)
			for key, want := range tc.wantParams {
				if got, ok := e.Params[key]; !ok || got != want {
					t.Errorf("param %q = %v (present %v), want %v", key, got, ok, want)
				}
			}
			if len(store.objects) != 0 {
				t.Errorf("refused declaration left %d keys in the store", len(store.objects))
			}
			rows, err := h.objects.listPageState(serviceCtx("tenant-a"), "", 200, "")
			if err != nil {
				t.Fatalf("listing after the refusal: %v", err)
			}
			if len(rows) != 0 {
				t.Errorf("refused declaration left %d rows behind", len(rows))
			}
		})
	}
}

func TestHandler_StorageUploadObjectContent_StreamsBytes(t *testing.T) {
	h, store, _ := newHandlerHarness(t)
	body := bytes.Repeat([]byte("01234567"), 512) // exactly 4096 bytes
	created := assertObject(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects", bodyFor(t, map[string]any{
		"declaredSize": 4096,
		"declaredType": "image/jpeg",
	})), http.StatusCreated)

	rec := request(t, h, "tenant-a", http.MethodPut, apiPath+"/objects/"+*created.ID+"/content", body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("upload status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	row, err := h.objects.FindByID(serviceCtx("tenant-a"), *created.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	stored, ok := store.bytes(row.Key)
	if !ok {
		t.Fatal("no bytes stored under the object's key")
	}
	if !bytes.Equal(stored, body) {
		t.Fatalf("stored %d bytes, want the %d uploaded", len(stored), len(body))
	}
	assertStillUploading(t, h.svc, serviceCtx("tenant-a"), *created.ID)
}

func TestHandler_StorageUploadObjectContent_ContentLengthSemantics(t *testing.T) {
	t.Run("a differing content length is a 400 before any byte is read", func(t *testing.T) {
		h, store, _ := newHandlerHarness(t)
		body := bytes.Repeat([]byte("x"), 4096)
		created := assertObject(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects", bodyFor(t, map[string]any{
			"declaredSize": 4096,
		})), http.StatusCreated)

		req := httptest.NewRequest(http.MethodPut, apiPath+"/objects/"+*created.ID+"/content", bytes.NewReader(body))
		req.ContentLength = 4095 // the transport announces fewer bytes than the declaration
		req = req.WithContext(serviceCtx("tenant-a"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		e := assertEnvelope(t, rec, http.StatusBadRequest, ErrContentLengthMismatch.Code)
		if e.Params["declared"] != float64(4096) {
			t.Errorf("declared param = %v, want 4096", e.Params["declared"])
		}
		if len(store.objects) != 0 {
			t.Error("refused transport length left bytes behind")
		}
	})

	t.Run("a chunked upload (no content length) is accepted", func(t *testing.T) {
		h, store, _ := newHandlerHarness(t)
		body := bytes.Repeat([]byte("y"), 2048)
		created := assertObject(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects", bodyFor(t, map[string]any{
			"declaredSize": 2048,
		})), http.StatusCreated)

		req := httptest.NewRequest(http.MethodPut, apiPath+"/objects/"+*created.ID+"/content", bytes.NewReader(body))
		req.ContentLength = -1 // chunked transfer: the transport announces no length
		req = req.WithContext(serviceCtx("tenant-a"))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("upload status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
		}
		if len(store.objects) != 1 {
			t.Fatalf("store holds %d keys, want the one object's bytes", len(store.objects))
		}
	})

	t.Run("an unknown object is a 404", func(t *testing.T) {
		h, _, _ := newHandlerHarness(t)
		const unknown = "00000000-0000-4000-8000-000000000000"
		e := assertEnvelope(t, request(t, h, "tenant-a", http.MethodPut, apiPath+"/objects/"+unknown+"/content", []byte("late bytes")), http.StatusNotFound, ErrObjectNotFound.Code)
		if e.Params["id"] != unknown {
			t.Errorf("id param = %v, want %q", e.Params["id"], unknown)
		}
	})
}

// TestHandler_StorageCompleteObject_StripsExifAndFinalizes is the surface's
// spine: the GPS-bearing JPEG declared and uploaded over HTTP finalizes with
// the metadata of the bytes actually stored -- sanitized, EXIF-free, shorter
// than the upload -- and the thumbnail-derive task is enqueued for the queue
// workers.
func TestHandler_StorageCompleteObject_StripsExifAndFinalizes(t *testing.T) {
	h, store, queue := newHandlerHarness(t)
	withExif := jpegWithExif(t)
	created := assertObject(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects", bodyFor(t, map[string]any{
		"declaredSize": int64(len(withExif)),
		"declaredType": "image/jpeg",
	})), http.StatusCreated)
	if got := request(t, h, "tenant-a", http.MethodPut, apiPath+"/objects/"+*created.ID+"/content", withExif); got.Code != http.StatusNoContent {
		t.Fatalf("upload status = %d", got.Code)
	}
	rec := request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects/"+*created.ID+"/complete", nil)
	completed := assertObject(t, rec, http.StatusOK)

	if completed.State == nil || *completed.State != ObjectStateCompleted {
		t.Errorf("state = %v, want %q", completed.State, ObjectStateCompleted)
	}
	if completed.MimeType == nil || *completed.MimeType != "image/jpeg" {
		t.Errorf("mimeType = %v, want image/jpeg", completed.MimeType)
	}
	if completed.Width == nil || *completed.Width != 48 || completed.Height == nil || *completed.Height != 32 {
		t.Errorf("dimensions = %vx%v, want 48x32", completed.Width, completed.Height)
	}
	if completed.DeclaredType == nil || *completed.DeclaredType != "image/jpeg" {
		t.Errorf("declaredType = %v, want the declaration preserved", completed.DeclaredType)
	}
	if completed.UploadExpiresAt == nil {
		t.Error("uploadExpiresAt absent on a completed row -- the window record stays")
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("derivatives")) {
		t.Error("complete response carries a derivatives member; derivation surfaces through getObject")
	}

	row, err := h.objects.FindByID(serviceCtx("tenant-a"), *completed.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	stored, ok := store.bytes(row.Key)
	if !ok {
		t.Fatal("stored bytes vanished at Complete")
	}
	if bytes.Contains(stored, exifSignature) {
		t.Fatal("exif signature survives the pipeline")
	}
	if completed.Size == nil || *completed.Size != int64(len(stored)) {
		t.Errorf("size = %v, want %d -- the sanitized length", completed.Size, len(stored))
	}
	if completed.ChecksumSha256 == nil || *completed.ChecksumSha256 != sha256HexDigest(stored) {
		t.Errorf("checksumSha256 = %v, want the digest of the stored bytes", completed.ChecksumSha256)
	}
	if len(queue.tasks) != 1 || queue.tasks[0].Type != taskTypeDeriveThumbnail {
		t.Errorf("tasks = %v, want one %s task", queue.tasks, taskTypeDeriveThumbnail)
	}
}

func TestHandler_StorageCompleteObject_RejectsMismatchedFinalizations(t *testing.T) {
	t.Run("content never arrived", func(t *testing.T) {
		h, _, _ := newHandlerHarness(t)
		created := assertObject(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects", bodyFor(t, map[string]any{
			"declaredSize": 4096,
			"declaredType": "image/jpeg",
		})), http.StatusCreated)
		e := assertEnvelope(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects/"+*created.ID+"/complete", nil), http.StatusConflict, ErrContentMissing.Code)
		if e.Params["id"] != *created.ID {
			t.Errorf("id param = %v, want %q", e.Params["id"], *created.ID)
		}
		assertStillUploading(t, h.svc, serviceCtx("tenant-a"), *created.ID)
	})

	t.Run("stored bytes disagree with the declared checksum", func(t *testing.T) {
		h, _, _ := newHandlerHarness(t)
		body := testutil.JPEG(t, 8, 8)
		created := assertObject(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects", bodyFor(t, map[string]any{
			"declaredSize":     int64(len(body)),
			"declaredType":     "image/jpeg",
			"declaredChecksum": sha256HexDigest([]byte("bytes that never arrive")),
		})), http.StatusCreated)
		if got := request(t, h, "tenant-a", http.MethodPut, apiPath+"/objects/"+*created.ID+"/content", body); got.Code != http.StatusNoContent {
			t.Fatalf("upload status = %d", got.Code)
		}
		assertEnvelope(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects/"+*created.ID+"/complete", nil), http.StatusConflict, ErrChecksumMismatch.Code)
		assertStillUploading(t, h.svc, serviceCtx("tenant-a"), *created.ID)
	})

	t.Run("stored bytes probe as a type the declaration did not name", func(t *testing.T) {
		h, _, _ := newHandlerHarness(t)
		png := testutil.PNG(t, 8, 8)
		created := assertObject(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects", bodyFor(t, map[string]any{
			"declaredSize": int64(len(png)),
			"declaredType": "image/jpeg", // png reality, jpeg claim
		})), http.StatusCreated)
		if got := request(t, h, "tenant-a", http.MethodPut, apiPath+"/objects/"+*created.ID+"/content", png); got.Code != http.StatusNoContent {
			t.Fatalf("upload status = %d", got.Code)
		}
		assertEnvelope(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects/"+*created.ID+"/complete", nil), http.StatusBadRequest, ErrTypeMismatch.Code)
		assertStillUploading(t, h.svc, serviceCtx("tenant-a"), *created.ID)
	})

	t.Run("a completed object cannot be completed again", func(t *testing.T) {
		h, _, _ := newHandlerHarness(t)
		completed := lifecycle(t, h, "tenant-a", testutil.JPEG(t, 8, 8), "image/jpeg")
		assertEnvelope(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects/"+*completed.ID+"/complete", nil), http.StatusConflict, ErrObjectNotUploading.Code)
	})
}

// TestHandler_StorageGetObject_AnswersCompletedRowsWithDerivatives pins the
// read surface: only a completed row of the caller's tenant answers 200;
// uploading rows answer 404 like ids that never existed. The derivatives
// array is the get's own addition -- present from the first read as an empty
// array, never null, and populated once a derivation row exists.
func TestHandler_StorageGetObject_AnswersCompletedRowsWithDerivatives(t *testing.T) {
	h, store, _ := newHandlerHarness(t)
	completed := lifecycle(t, h, "tenant-a", testutil.JPEG(t, 48, 32), "image/jpeg")
	path := apiPath + "/objects/" + *completed.ID

	t.Run("metadata plus an empty derivatives array", func(t *testing.T) {
		rec := request(t, h, "tenant-a", http.MethodGet, path, nil)
		obj := assertObject(t, rec, http.StatusOK)
		if obj.Derivatives == nil {
			t.Fatal("derivatives is null; the schema promises an array")
		}
		if len(*obj.Derivatives) != 0 {
			t.Errorf("derivatives = %d entries, want none before derivation", len(*obj.Derivatives))
		}
		if obj.State == nil || *obj.State != ObjectStateCompleted {
			t.Errorf("state = %v, want completed", obj.State)
		}
	})

	t.Run("derivatives appear once a thumbnail row exists", func(t *testing.T) {
		seedDerivativeBytes(t, h.life, store, *completed.ID, "tenant-a")
		rec := request(t, h, "tenant-a", http.MethodGet, path, nil)
		obj := assertObject(t, rec, http.StatusOK)
		if obj.Derivatives == nil || len(*obj.Derivatives) != 1 {
			t.Fatalf("derivatives = %v, want one entry", obj.Derivatives)
		}
		d := (*obj.Derivatives)[0]
		if d.Kind == nil || *d.Kind != DerivativeKindThumbnail {
			t.Errorf("kind = %v, want %q", d.Kind, DerivativeKindThumbnail)
		}
		if d.MimeType == nil || *d.MimeType != "image/png" || d.Size == nil || *d.Size != 64 {
			t.Errorf("mimeType/size = %v/%v, want image/png/64", d.MimeType, d.Size)
		}
		// The seeded row carries no measured dimensions, and the surface
		// passes that absence through -- width/height never marshal as zeros.
		if d.Width != nil || d.Height != nil {
			t.Errorf("dimensions = %vx%v, want absent on an unmeasured derivative", d.Width, d.Height)
		}
	})

	t.Run("an uploading row reads as not found", func(t *testing.T) {
		created := assertObject(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects", bodyFor(t, map[string]any{
			"declaredSize": 10,
		})), http.StatusCreated)
		assertEnvelope(t, request(t, h, "tenant-a", http.MethodGet, apiPath+"/objects/"+*created.ID, nil), http.StatusNotFound, ErrObjectNotFound.Code)
	})

	t.Run("an id that never existed reads as not found", func(t *testing.T) {
		const unknown = "00000000-0000-4000-8000-000000000000"
		assertEnvelope(t, request(t, h, "tenant-a", http.MethodGet, apiPath+"/objects/"+unknown, nil), http.StatusNotFound, ErrObjectNotFound.Code)
	})
}

func TestHandler_StorageGetObject_AnotherTenantsObjectIsInvisible(t *testing.T) {
	h, _, _ := newHandlerHarness(t)
	completed := lifecycle(t, h, "tenant-a", testutil.JPEG(t, 8, 8), "image/jpeg")

	e := assertEnvelope(t, request(t, h, "tenant-b", http.MethodGet, apiPath+"/objects/"+*completed.ID, nil), http.StatusNotFound, ErrObjectNotFound.Code)
	if e.Params["id"] != *completed.ID {
		t.Errorf("id param = %v, want %q", e.Params["id"], *completed.ID)
	}
}

// TestHandler_StorageListObjects_CompletedOnlyPages pins the listing
// surface: completed objects of the caller's tenant, newest first, with the
// uploading row created most recently of all still invisible, an explicit
// empty page never marshaling as null, and the metadata-only promise -- no
// item carries a derivatives member.
func TestHandler_StorageListObjects_CompletedOnlyPages(t *testing.T) {
	h, _, _ := newHandlerHarness(t)
	base := time.Now().Add(-2 * time.Hour)
	for i := 0; i < 3; i++ {
		seedObject(t, h.objects, tenantCtx("tenant-a"),
			newCompleted(fmt.Sprintf("c-%02d", i), "tenant-a", base.Add(time.Duration(i)*time.Minute)))
	}
	// The newest row of all is an upload in flight -- created through the
	// surface so the row is the real article.
	created := assertObject(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects", bodyFor(t, map[string]any{
		"declaredSize": 10,
	})), http.StatusCreated)

	t.Run("the full page, newest first", func(t *testing.T) {
		rec := request(t, h, "tenant-a", http.MethodGet, apiPath+"/objects", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
		}
		var list api.StorageListObjectsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("response is not a list: %v", err)
		}
		if list.Objects == nil || len(*list.Objects) != 3 {
			t.Fatalf("objects = %v, want the 3 completed rows", list.Objects)
		}
		for i, wantID := range []string{"c-02", "c-01", "c-00"} {
			if id := *(*list.Objects)[i].ID; id != wantID {
				t.Errorf("page[%d] = %s, want %s (newest first)", i, id, wantID)
			}
		}
		if *(*list.Objects)[0].ID == *created.ID {
			t.Error("uploading row listed")
		}
		if bytes.Contains(rec.Body.Bytes(), []byte("derivatives")) {
			t.Error("list response carries a derivatives member -- list is metadata only")
		}
	})

	t.Run("limit and the keyset cursor", func(t *testing.T) {
		rec := request(t, h, "tenant-a", http.MethodGet, apiPath+"/objects?limit=1", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var list api.StorageListObjectsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("response is not a list: %v", err)
		}
		if len(*list.Objects) != 1 || *(*list.Objects)[0].ID != "c-02" {
			t.Fatalf("limit=1 page = %v, want [c-02]", list.Objects)
		}
		rec = request(t, h, "tenant-a", http.MethodGet, apiPath+"/objects?limit=1&beforeId=c-01", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("cursor page status = %d", rec.Code)
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("response is not a list: %v", err)
		}
		if len(*list.Objects) != 1 || *(*list.Objects)[0].ID != "c-00" {
			t.Fatalf("cursor page = %v, want [c-00]", list.Objects)
		}
	})

	t.Run("a limit outside the 1-200 bound", func(t *testing.T) {
		for _, limit := range []int{0, 201} {
			e := assertEnvelope(t, request(t, h, "tenant-a", http.MethodGet, fmt.Sprintf("%s/objects?limit=%d", apiPath, limit), nil), http.StatusBadRequest, ErrInvalidLimit.Code)
			if e.Params["limit"] != float64(limit) || e.Params["min"] != float64(1) || e.Params["max"] != float64(200) {
				t.Errorf("params = %v, want limit=%d min=1 max=200", e.Params, limit)
			}
		}
	})

	t.Run("a cursor naming nothing of this listing", func(t *testing.T) {
		const unknown = "00000000-0000-4000-8000-000000000000"
		assertEnvelope(t, request(t, h, "tenant-a", http.MethodGet, apiPath+"/objects?beforeId="+unknown, nil), http.StatusNotFound, ErrObjectNotFound.Code)
	})

	t.Run("an empty tenant lists an empty array, never null", func(t *testing.T) {
		rec := request(t, h, "tenant-b", http.MethodGet, apiPath+"/objects", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var list api.StorageListObjectsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("response is not a list: %v", err)
		}
		if list.Objects == nil || len(*list.Objects) != 0 {
			t.Errorf("objects = %v, want an empty non-null array", list.Objects)
		}
	})
}

// TestHandler_StorageDeleteObject_Answers pins the delete surface's three
// answers: 204 when the row is completed (and the row is gone afterwards),
// 404 for absent and foreign rows -- so a repeated delete answers 404 after
// the first 204 -- and 409 while the object is still uploading, with a
// deleting row's deletion resumed to completion exactly as the spec promises.
func TestHandler_StorageDeleteObject_Answers(t *testing.T) {
	t.Run("completed row: 204, gone, repeat delete 404", func(t *testing.T) {
		h, store, _ := newHandlerHarness(t)
		completed := lifecycle(t, h, "tenant-a", testutil.JPEG(t, 8, 8), "image/jpeg")
		path := apiPath + "/objects/" + *completed.ID

		if rec := request(t, h, "tenant-a", http.MethodDelete, path, nil); rec.Code != http.StatusNoContent {
			t.Fatalf("delete status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
		}
		if len(store.objects) != 0 {
			t.Errorf("store holds %d keys after the delete", len(store.objects))
		}
		assertEnvelope(t, request(t, h, "tenant-a", http.MethodGet, path, nil), http.StatusNotFound, ErrObjectNotFound.Code)
		assertEnvelope(t, request(t, h, "tenant-a", http.MethodDelete, path, nil), http.StatusNotFound, ErrObjectNotFound.Code)
		if _, err := h.objects.FindByID(serviceCtx("tenant-a"), *completed.ID); err == nil {
			t.Error("row still exists after the delete")
		}
	})

	t.Run("uploading row: 409, nothing deleted", func(t *testing.T) {
		h, _, _ := newHandlerHarness(t)
		created := assertObject(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects", bodyFor(t, map[string]any{
			"declaredSize": 10,
		})), http.StatusCreated)
		e := assertEnvelope(t, request(t, h, "tenant-a", http.MethodDelete, apiPath+"/objects/"+*created.ID, nil), http.StatusConflict, ErrObjectUploading.Code)
		if e.Params["id"] != *created.ID {
			t.Errorf("id param = %v, want %q", e.Params["id"], *created.ID)
		}
		assertStillUploading(t, h.svc, serviceCtx("tenant-a"), *created.ID)
	})

	t.Run("deleting row: the interrupted deletion resumes to 204", func(t *testing.T) {
		h, _, _ := newHandlerHarness(t)
		deleting := newCompleted("obj-interrupted", "tenant-a", time.Now())
		deleting.State = ObjectStateDeleting
		seedObject(t, h.objects, tenantCtx("tenant-a"), deleting)
		if rec := request(t, h, "tenant-a", http.MethodDelete, apiPath+"/objects/obj-interrupted", nil); rec.Code != http.StatusNoContent {
			t.Fatalf("delete status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
		}
		if _, err := h.objects.FindByID(serviceCtx("tenant-a"), "obj-interrupted"); err == nil {
			t.Error("resumed deletion left the row behind")
		}
	})

	t.Run("another tenant's object is invisible to the delete", func(t *testing.T) {
		h, _, _ := newHandlerHarness(t)
		foreign := lifecycle(t, h, "tenant-a", testutil.JPEG(t, 8, 8), "image/jpeg")
		e := assertEnvelope(t, request(t, h, "tenant-b", http.MethodDelete, apiPath+"/objects/"+*foreign.ID, nil), http.StatusNotFound, ErrObjectNotFound.Code)
		if e.Params["id"] != *foreign.ID {
			t.Errorf("id param = %v, want %q", e.Params["id"], *foreign.ID)
		}
		if _, err := h.objects.FindByID(serviceCtx("tenant-a"), *foreign.ID); err != nil {
			t.Errorf("foreign delete removed the row: %v", err)
		}
	})
}

// TestHandler_StorageGetObjectContent_StreamsBytes pins the download side:
// the bytes stream with the probed media type and the finalized length on the
// headers, and only completed objects answer at all -- uploading and absent
// ids answer 404, never an empty 200.
func TestHandler_StorageGetObjectContent_StreamsBytes(t *testing.T) {
	h, store, _ := newHandlerHarness(t)
	withExif := jpegWithExif(t)
	completed := lifecycle(t, h, "tenant-a", withExif, "image/jpeg")
	path := apiPath + "/objects/" + *completed.ID + "/content"

	rec := request(t, h, "tenant-a", http.MethodGet, path, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q, want the probed image/jpeg", ct)
	}
	row, err := h.objects.FindByID(serviceCtx("tenant-a"), *completed.ID)
	if err != nil {
		t.Fatalf("FindByID: %v", err)
	}
	stored, _ := store.bytes(row.Key)
	if cl := rec.Header().Get("Content-Length"); cl != fmt.Sprintf("%d", len(stored)) {
		t.Errorf("Content-Length = %q, want %d", cl, len(stored))
	}
	body := rec.Body.Bytes()
	if len(body) != len(stored) || !bytes.Equal(body, stored) {
		t.Fatalf("streamed %d bytes that differ from the stored %d", len(body), len(stored))
	}
	if bytes.Contains(body, exifSignature) {
		t.Fatal("exif signature streams out of the content endpoint")
	}
	assertDecodesEqual(t, "handler content jpeg", body, testutil.JPEG(t, 48, 32))
}

func TestHandler_StorageGetObjectContent_InvisibleRowsAnswerNotFound(t *testing.T) {
	h, _, _ := newHandlerHarness(t)

	t.Run("an uploading row", func(t *testing.T) {
		created := assertObject(t, request(t, h, "tenant-a", http.MethodPost, apiPath+"/objects", bodyFor(t, map[string]any{
			"declaredSize": 10,
		})), http.StatusCreated)
		assertEnvelope(t, request(t, h, "tenant-a", http.MethodGet, apiPath+"/objects/"+*created.ID+"/content", nil), http.StatusNotFound, ErrObjectNotFound.Code)
	})

	t.Run("an id that never existed", func(t *testing.T) {
		const unknown = "00000000-0000-4000-8000-000000000000"
		assertEnvelope(t, request(t, h, "tenant-a", http.MethodGet, apiPath+"/objects/"+unknown+"/content", nil), http.StatusNotFound, ErrObjectNotFound.Code)
	})
}

// TestHandler_FailsClosedWithoutAResolvedTenant pins the fail-closed answer
// of the surface's one unguarded assumption: a request that reaches Handler
// with no tenant in its context -- which a host can only produce by routing
// storage's path around tenancy.Middleware -- is a 500 storage.internal_error,
// never a half-answered request and never an invented tenant.
func TestHandler_FailsClosedWithoutAResolvedTenant(t *testing.T) {
	h, _, _ := newHandlerHarness(t)
	req := httptest.NewRequest(http.MethodPost, apiPath+"/objects", bytes.NewReader(bodyFor(t, map[string]any{
		"declaredSize": 10,
	})))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assertEnvelope(t, rec, http.StatusInternalServerError, ErrInternal.Code)
}
