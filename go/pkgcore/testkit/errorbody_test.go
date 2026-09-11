package testkit

import (
	"strings"
	"testing"
)

func TestDecodeErrorBody_CodeAndParams_Decoded(t *testing.T) {
	body := DecodeErrorBody(t, strings.NewReader(`{"code":"test.refused","params":{"limit":3}}`))
	if body.Code != "test.refused" {
		t.Fatalf("Code = %q, want %q", body.Code, "test.refused")
	}
	if got := body.Params["limit"]; got != float64(3) {
		t.Fatalf("Params[limit] = %v, want 3", got)
	}
}

func TestDecodeErrorBody_CodeOnly_LeavesParamsEmpty(t *testing.T) {
	body := DecodeErrorBody(t, strings.NewReader(`{"code":"test.refused"}`))
	if body.Code != "test.refused" {
		t.Fatalf("Code = %q, want %q", body.Code, "test.refused")
	}
	if len(body.Params) != 0 {
		t.Fatalf("Params = %v, want none", body.Params)
	}
}

func TestDecodeErrorBody_BodyWithTrailingContent_Rejected(t *testing.T) {
	if _, err := decodeErrorBody([]byte(`{"code":"test.refused"}{"code":"test.other"}`)); err == nil {
		t.Fatal("decodeErrorBody accepted two concatenated JSON values, want a rejection")
	}
}

func TestDecodeErrorBody_MalformedBody_Rejected(t *testing.T) {
	if _, err := decodeErrorBody([]byte(`not json`)); err == nil {
		t.Fatal("decodeErrorBody accepted a malformed body, want a rejection")
	}
}
