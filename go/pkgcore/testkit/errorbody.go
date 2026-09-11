package testkit

import (
	"encoding/json"
	"io"
	"testing"
)

// ErrorBody is the {code, params} refusal envelope every module's structured
// error is written as on the wire: the machine-readable code is the API's
// contract, the params carry the code's placeholders, and the client
// resolves the text against its own catalog. The decoder below reads a
// response body back into this shape so a suite can assert on the code
// without re-declaring the envelope.
type ErrorBody struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// DecodeErrorBody reads r's body as the {code, params} envelope, failing
// the test when the body is unreadable or not that JSON shape.
func DecodeErrorBody(t *testing.T, r io.Reader) ErrorBody {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading the error body: %v", err)
	}
	body, err := decodeErrorBody(data)
	if err != nil {
		t.Fatalf("decoding the error body %q: %v", data, err)
	}
	return body
}

// decodeErrorBody is DecodeErrorBody's error-free core, so the malformed-
// body rejection is directly testable.
func decodeErrorBody(data []byte) (ErrorBody, error) {
	var body ErrorBody
	if err := json.Unmarshal(data, &body); err != nil {
		return ErrorBody{}, err
	}
	return body, nil
}
