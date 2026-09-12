package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// errFallback stands in for a module's internal-error sentinel: what every
// handler writes when something below it did not classify the failure.
var errFallback = apperr.Internal("test.internal_error")

// errInvalidBody stands in for a module's invalid-request-body sentinel, the
// coded refusal DecodeJSON maps every decode failure onto.
var errInvalidBody = apperr.Invalid("test.invalid_request_body")

// TestWriteError_EnvelopeBytes pins the serialized envelope exactly: the
// {code, params} shape, the params key omitted entirely for a
// parameter-less error (never "params": null), and the shared JSON
// content type. These bytes are the wire contract every module's surface
// answers refusals with.
func TestWriteError_EnvelopeBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantBody    string
		wantContent string
	}{
		{
			name:        "coded error with params",
			err:         apperr.Forbidden("rbac.permission_denied").WithParam("permission", "billing:write").WithParam("action", "read"),
			wantStatus:  http.StatusForbidden,
			wantBody:    `{"code":"rbac.permission_denied","params":{"action":"read","permission":"billing:write"}}` + "\n",
			wantContent: JSONContentType,
		},
		{
			name:        "coded error without params omits the key",
			err:         apperr.NotFound("org.node_not_found"),
			wantStatus:  http.StatusNotFound,
			wantBody:    `{"code":"org.node_not_found"}` + "\n",
			wantContent: JSONContentType,
		},
		{
			name:        "uncoded error takes the fallback envelope",
			err:         errors.New("dial tcp 10.0.0.7:5432: connection refused"),
			wantStatus:  http.StatusInternalServerError,
			wantBody:    `{"code":"test.internal_error"}` + "\n",
			wantContent: JSONContentType,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()

			WriteError(rec, tc.err, errFallback)

			if rec.Code != tc.wantStatus {
				t.Errorf("WriteError status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Body.String(); got != tc.wantBody {
				t.Errorf("WriteError body = %q, want %q", got, tc.wantBody)
			}
			if got := rec.Result().Header.Get("Content-Type"); got != tc.wantContent {
				t.Errorf("WriteError Content-Type = %q, want %q", got, tc.wantContent)
			}
		})
	}
}

// TestWriteError_UncodedError_NeverLeaksRawText pins that the fallback path
// writes only the fallback's own code: the raw error's message -- which can
// name a driver, a host or a ciphertext failure -- must not appear in the
// body.
func TestWriteError_UncodedError_NeverLeaksRawText(t *testing.T) {
	t.Parallel()

	const raw = "s3://internal-bucket/object: access denied"
	rec := httptest.NewRecorder()

	WriteError(rec, errors.New(raw), errFallback)

	if strings.Contains(rec.Body.String(), raw) {
		t.Errorf("WriteError body %q carries the raw error text", rec.Body.String())
	}
}

// TestWriteError_WrappedCodedError_KeepsTheCodedEnvelope pins that an
// *apperr.Error wrapped in a plain error is still classified as coded, at
// its own status: the wrap adds server-side context (its cause chain), not
// wire-visible content.
func TestWriteError_WrappedCodedError_KeepsTheCodedEnvelope(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("charging invoice inv_7: %w",
		apperr.Conflict("billing.invoice_conflict").WithParam("id", "inv_7"))
	rec := httptest.NewRecorder()

	WriteError(rec, wrapped, errFallback)

	if rec.Code != http.StatusConflict {
		t.Errorf("WriteError status = %d, want %d", rec.Code, http.StatusConflict)
	}
	want := `{"code":"billing.invoice_conflict","params":{"id":"inv_7"}}` + "\n"
	if got := rec.Body.String(); got != want {
		t.Errorf("WriteError body = %q, want %q", got, want)
	}
}

// TestWriteError_CallerContentType_IsPreserved pins the override seam: an
// endpoint whose contract pins a different content type (set before the
// call) keeps it, while a caller that sets none gets the shared JSON type.
func TestWriteError_CallerContentType_IsPreserved(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/problem+json")

	WriteError(rec, apperr.NotFound("org.node_not_found"), errFallback)

	if got := rec.Result().Header.Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("WriteError Content-Type = %q, want the caller's own", got)
	}
}

// TestWriteJSON_ResponseBytes pins the bytes every shared JSON success
// response carries: the status the caller gave, the shared JSON content
// type, and the encoded body including the encoder's trailing newline.
func TestWriteJSON_ResponseBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		status   int
		value    any
		wantBody string
	}{
		{
			name:   "object body",
			status: http.StatusCreated,
			value: struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}{ID: "note_7", Name: "first"},
			wantBody: `{"id":"note_7","name":"first"}` + "\n",
		},
		{
			name:     "list body",
			status:   http.StatusOK,
			value:    []string{"a", "b"},
			wantBody: `["a","b"]` + "\n",
		},
		{
			name:     "empty list body stays []",
			status:   http.StatusOK,
			value:    []string{},
			wantBody: `[]` + "\n",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()

			WriteJSON(rec, tc.status, tc.value)

			if rec.Code != tc.status {
				t.Errorf("WriteJSON status = %d, want %d", rec.Code, tc.status)
			}
			if got := rec.Result().Header.Get("Content-Type"); got != JSONContentType {
				t.Errorf("WriteJSON Content-Type = %q, want %q", got, JSONContentType)
			}
			if got := rec.Body.String(); got != tc.wantBody {
				t.Errorf("WriteJSON body = %q, want %q", got, tc.wantBody)
			}
		})
	}
}

// TestWriteJSON_SetsTheContentTypeUnconditionally pins the write contract
// every call site relied on: the shared type replaces whatever a caller had
// set, so a pre-set header cannot silently change the media type of a
// shared JSON response.
func TestWriteJSON_SetsTheContentTypeUnconditionally(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "application/problem+json")

	WriteJSON(rec, http.StatusOK, map[string]string{"ok": "yes"})

	if got := rec.Result().Header.Get("Content-Type"); got != JSONContentType {
		t.Errorf("WriteJSON Content-Type = %q, want the shared %q", got, JSONContentType)
	}
}

// TestWriteJSON_UnencodableValue_KeepsTheStatusLine pins the dropped-error
// contract: a value the encoder cannot marshal leaves the committed status
// line and the content type alone -- no panic, no second write.
func TestWriteJSON_UnencodableValue_KeepsTheStatusLine(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()

	WriteJSON(rec, http.StatusOK, make(chan int))

	if rec.Code != http.StatusOK {
		t.Errorf("WriteJSON status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Result().Header.Get("Content-Type"); got != JSONContentType {
		t.Errorf("WriteJSON Content-Type = %q, want %q", got, JSONContentType)
	}
}

// decodeTarget is the shape DecodeJSON's tests decode into.
type decodeTarget struct {
	Name string `json:"name"`
}

// TestDecodeJSON_ValidBody_DecodesAndWritesNothing pins the success path: dst
// is populated and no response is written.
func TestDecodeJSON_ValidBody_DecodesAndWritesNothing(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	var dst decodeTarget

	if ok := DecodeJSON(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"name":"acme"}`)), 0, &dst, errInvalidBody); !ok {
		t.Fatal("DecodeJSON refused a valid body")
	}
	if dst.Name != "acme" {
		t.Errorf("dst.Name = %q, want %q", dst.Name, "acme")
	}
	if rec.Body.Len() != 0 || rec.Code != http.StatusOK {
		t.Errorf("DecodeJSON wrote a response on success: status %d body %q", rec.Code, rec.Body.String())
	}
}

// TestDecodeJSON_MalformedAndOversizedBodies_WriteTheSameRefusal pins the
// body-limit error mapping: malformed JSON and a body past maxBytes answer
// with the identical coded refusal (status and envelope), so a client sees
// one invalid-request-body error either way.
func TestDecodeJSON_MalformedAndOversizedBodies_WriteTheSameRefusal(t *testing.T) {
	t.Parallel()

	const maxBytes = 32
	oversized := `{"name":"` + strings.Repeat("a", maxBytes) + `"}`

	refusals := make([]string, 0, 2)
	for _, body := range []string{`{"name":`, oversized} {
		rec := httptest.NewRecorder()
		var dst decodeTarget

		if ok := DecodeJSON(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), maxBytes, &dst, errInvalidBody); ok {
			t.Fatalf("DecodeJSON accepted a body it should refuse: %q", body)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("DecodeJSON status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
		want := `{"code":"test.invalid_request_body"}` + "\n"
		if got := rec.Body.String(); got != want {
			t.Errorf("DecodeJSON body = %q, want %q", got, want)
		}
		refusals = append(refusals, rec.Body.String())
	}

	if refusals[0] != refusals[1] {
		t.Errorf("malformed and oversized refusals differ: %q vs %q", refusals[0], refusals[1])
	}
}

// TestDecodeJSON_PositiveMaxBytes_AcceptsUpToTheBound pins that the bound
// refuses only bodies past the limit: a body exactly at it still decodes.
func TestDecodeJSON_PositiveMaxBytes_AcceptsUpToTheBound(t *testing.T) {
	t.Parallel()

	body := `{"name":"` + strings.Repeat("a", 21) + `"}` // 32 bytes, exactly at the bound
	if len(body) != 32 {
		t.Fatalf("test body is %d bytes, want exactly 32", len(body))
	}
	rec := httptest.NewRecorder()
	var dst decodeTarget

	if ok := DecodeJSON(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), 32, &dst, errInvalidBody); !ok {
		t.Fatalf("DecodeJSON refused a body at the bound: %q", rec.Body.String())
	}
	if len(dst.Name) != 21 {
		t.Errorf("dst.Name has %d bytes, want 21", len(dst.Name))
	}
}

// TestDecodeJSON_NonPositiveMaxBytes_DecodesUnbounded pins the
// no-limit mode: a body far past the 64 KiB bound every bounded caller uses
// still decodes when maxBytes is non-positive.
func TestDecodeJSON_NonPositiveMaxBytes_DecodesUnbounded(t *testing.T) {
	t.Parallel()

	body := `{"name":"` + strings.Repeat("a", 70<<10) + `"}`
	rec := httptest.NewRecorder()
	var dst decodeTarget

	if ok := DecodeJSON(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)), 0, &dst, errInvalidBody); !ok {
		t.Fatalf("DecodeJSON refused an unbounded body: %q", rec.Body.String())
	}
	if len(dst.Name) != 70<<10 {
		t.Errorf("dst.Name has %d bytes, want %d", len(dst.Name), 70<<10)
	}
}

// TestDecodeJSON_WrongShape_FailsClosed pins that a well-formed JSON value of
// the wrong shape is a refusal, not a silent partial decode.
func TestDecodeJSON_WrongShape_FailsClosed(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	var dst decodeTarget

	if ok := DecodeJSON(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`"not an object"`)), 0, &dst, errInvalidBody); ok {
		t.Fatal("DecodeJSON accepted a JSON value of the wrong shape")
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("DecodeJSON status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("refusal body is not JSON: %v", err)
	}
	if envelope.Code != errInvalidBody.Code {
		t.Errorf("refusal code = %q, want %q", envelope.Code, errInvalidBody.Code)
	}
}
