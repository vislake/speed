package httpapi_test

// Runnable documentation for the httpapi public API. Every example here is
// compiled and executed by `go test`.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"

	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/httpapi"
)

// ExampleWriteError shows a refusal crossing the HTTP boundary: the coded
// error's own suggested status, the shared JSON content type, and the
// {code, params} envelope the client resolves through its own i18n catalog.
func ExampleWriteError() {
	rec := httptest.NewRecorder()

	httpapi.WriteError(rec,
		apperr.NotFound("org.node_not_found").WithParam("id", "node_42"),
		apperr.Internal("org.internal_error"))

	fmt.Println(rec.Code)
	fmt.Println(rec.Result().Header.Get("Content-Type"))
	fmt.Print(rec.Body.String())

	// Output:
	// 404
	// application/json; charset=utf-8
	// {"code":"org.node_not_found","params":{"id":"node_42"}}
}

// ExampleWriteJSON shows the shared JSON success response: the caller's
// status, the shared content type, and the encoded body the encoder
// terminates with a newline.
func ExampleWriteJSON() {
	rec := httptest.NewRecorder()

	httpapi.WriteJSON(rec, http.StatusCreated, map[string]string{"id": "note_7"})

	fmt.Println(rec.Code)
	fmt.Println(rec.Result().Header.Get("Content-Type"))
	fmt.Print(rec.Body.String())

	// Output:
	// 201
	// application/json; charset=utf-8
	// {"id":"note_7"}
}

// ExampleDecodeJSON shows both halves of the bounded request decode: a
// legitimate body decodes into the caller's request type, and a body past
// the bound answers the module's own coded invalid-request-body refusal --
// the same refusal malformed JSON gets.
func ExampleDecodeJSON() {
	// The module's request type and its invalid-request-body sentinel.
	type createNoteRequest struct {
		Text string `json:"text"`
	}
	errInvalidBody := apperr.Invalid("notes.invalid_request_body")

	var req createNoteRequest
	valid := httptest.NewRequest(http.MethodPost, "/api/v1/notes", strings.NewReader(`{"text":"hello"}`))
	if !httpapi.DecodeJSON(httptest.NewRecorder(), valid, 1<<16, &req, errInvalidBody) {
		return
	}
	fmt.Println(req.Text)

	oversized := httptest.NewRequest(http.MethodPost, "/api/v1/notes",
		strings.NewReader(`{"text":"`+strings.Repeat("a", 70<<10)+`"}`))
	rec := httptest.NewRecorder()
	httpapi.DecodeJSON(rec, oversized, 1<<16, &req, errInvalidBody)
	fmt.Println(rec.Code, rec.Body.String())

	// Output:
	// hello
	// 400 {"code":"notes.invalid_request_body"}
}
