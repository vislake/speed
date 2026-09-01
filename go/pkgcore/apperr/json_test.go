package apperr

// Tests for how *Error and its Params behave under encoding/json. Params
// exist so a client can interpolate them into a localized message, so their
// behaviour under JSON is part of the contract in practice; apperr_test.go
// covers construction, WithParam chaining and Error()/Unwrap instead.

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

// jsonCode is the error code the serialization cases build on.
const jsonCode = "billing.quota_exceeded"

// secretDSN is planted inside a wrapped cause. It must never appear in the
// JSON an API would send, because a cause carries internal detail.
const secretDSN = "postgres://user:hunter2@10.0.0.5:5432/billing"

// TestParamsRoundTripThroughJSON marshals the Params map that WithParam
// builds and reads it back, confirming every value survives the trip. JSON
// has no integer type, so a Go int comes back as a float64; the case asserts
// the numeric value rather than the Go type, and pins that behaviour so a
// future change to Params is noticed.
func TestParamsRoundTripThroughJSON(t *testing.T) {
	t.Parallel()

	err := Invalid(jsonCode).
		WithParam("id", "sub_01H8Z").
		WithParam("limit", 100).
		WithParam("used", 137.5).
		WithParam("overLimit", true).
		WithParam("plans", []string{"pro", "team"}).
		WithParam("window", map[string]any{"unit": "month", "size": 1}).
		WithParam("absent", nil)

	blob, mErr := json.Marshal(err.Params)
	if mErr != nil {
		t.Fatalf("json.Marshal(Params) returned an error: %v", mErr)
	}

	var back map[string]any
	if uErr := json.Unmarshal(blob, &back); uErr != nil {
		t.Fatalf("json.Unmarshal returned an error: %v (payload %s)", uErr, blob)
	}

	if len(back) != len(err.Params) {
		t.Errorf("round-tripped %d params, want %d (payload %s)", len(back), len(err.Params), blob)
	}

	t.Run("scalars keep their value", func(t *testing.T) {
		cases := []struct {
			key  string
			want any
		}{
			{"id", "sub_01H8Z"},
			// An int is encoded as a JSON number and decodes into float64.
			{"limit", float64(100)},
			{"used", 137.5},
			{"overLimit", true},
			{"absent", nil},
		}
		for _, c := range cases {
			got, ok := back[c.key]
			if !ok {
				t.Errorf("param %q is missing after the round trip (payload %s)", c.key, blob)
				continue
			}
			if got != c.want {
				t.Errorf("param %q = %#v, want %#v", c.key, got, c.want)
			}
		}
	})

	t.Run("a slice param survives as an array", func(t *testing.T) {
		got, ok := back["plans"].([]any)
		if !ok {
			t.Fatalf("param \"plans\" = %#v, want a JSON array", back["plans"])
		}
		want := []string{"pro", "team"}
		if len(got) != len(want) {
			t.Fatalf("param \"plans\" has %d entries, want %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("plans[%d] = %#v, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("a nested map param survives as an object", func(t *testing.T) {
		got, ok := back["window"].(map[string]any)
		if !ok {
			t.Fatalf("param \"window\" = %#v, want a JSON object", back["window"])
		}
		if got["unit"] != "month" {
			t.Errorf("window.unit = %#v, want %q", got["unit"], "month")
		}
		if got["size"] != float64(1) {
			t.Errorf("window.size = %#v, want %v", got["size"], float64(1))
		}
	})

	t.Run("overwriting a key leaves only the last value", func(t *testing.T) {
		e := Invalid(jsonCode).WithParam("id", "first").WithParam("id", "second")
		b, mErr := json.Marshal(e.Params)
		if mErr != nil {
			t.Fatalf("json.Marshal returned an error: %v", mErr)
		}
		var m map[string]any
		if uErr := json.Unmarshal(b, &m); uErr != nil {
			t.Fatalf("json.Unmarshal returned an error: %v", uErr)
		}
		if m["id"] != "second" {
			t.Errorf("id = %#v, want %q", m["id"], "second")
		}
	})
}

// TestErrorMarshalsWithoutLeakingTheCause pins the documented promise that a
// cause is reachable through Unwrap but is never rendered into an API
// response. A cause routinely carries a DSN, a driver message or a
// stack-shaped string, so leaking it through the transport would be a real
// disclosure bug rather than a cosmetic one.
func TestErrorMarshalsWithoutLeakingTheCause(t *testing.T) {
	t.Parallel()

	cause := errors.New("dial " + secretDSN + ": connection refused")
	err := Internal(jsonCode).WithParam("id", "sub_01H8Z").WithCause(cause)

	blob, mErr := json.Marshal(err)
	if mErr != nil {
		t.Fatalf("json.Marshal(*Error) returned an error: %v", mErr)
	}
	payload := string(blob)

	if strings.Contains(payload, secretDSN) {
		t.Errorf("the wrapped cause leaked into the JSON payload: %s", payload)
	}
	if strings.Contains(payload, "connection refused") {
		t.Errorf("the wrapped cause leaked into the JSON payload: %s", payload)
	}

	// The cause must still be reachable in-process, so logs keep the detail.
	if !errors.Is(err, cause) {
		t.Error("errors.Is no longer finds the cause after marshalling")
	}

	var back map[string]any
	if uErr := json.Unmarshal(blob, &back); uErr != nil {
		t.Fatalf("json.Unmarshal returned an error: %v (payload %s)", uErr, payload)
	}

	// *Error carries no json struct tags, so the exported field names are what
	// a transport actually emits today. Pinning them here means any later
	// change to the wire shape is a deliberate, visible decision.
	code, isString := back["Code"].(string)
	if !isString || code != jsonCode {
		t.Errorf("Code = %#v, want %q (payload %s)", back["Code"], jsonCode, payload)
	}
	status, isNumber := back["Status"].(float64)
	if !isNumber || int(status) != err.Status {
		t.Errorf("Status = %#v, want %d (payload %s)", back["Status"], err.Status, payload)
	}
	params, isObject := back["Params"].(map[string]any)
	if !isObject {
		t.Fatalf("Params = %#v, want a JSON object (payload %s)", back["Params"], payload)
	}
	if params["id"] != "sub_01H8Z" {
		t.Errorf("Params.id = %#v, want %q", params["id"], "sub_01H8Z")
	}
	if _, leaked := back["cause"]; leaked {
		t.Errorf("an unexported cause field surfaced in the payload: %s", payload)
	}
}

// TestParamsWithUnserializableValueIsReported records what happens when a
// caller stores something encoding/json cannot represent. WithParam accepts
// any, so nothing stops it; the failure has to surface at marshal time, and a
// transport must be ready to handle it rather than emit a broken body.
func TestParamsWithUnserializableValueIsReported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value any
	}{
		{"a channel cannot be encoded", make(chan int)},
		{"a function cannot be encoded", func() {}},
		{"NaN is not representable in JSON", math.NaN()},
		{"positive infinity is not representable in JSON", math.Inf(1)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := Internal(jsonCode).WithParam("bad", tt.value)
			blob, mErr := json.Marshal(err.Params)
			if mErr == nil {
				t.Fatalf("json.Marshal succeeded for %T, want an error; payload %s", tt.value, blob)
			}
			// The value itself is unusable, but the error must name the key or
			// the type so an operator can find the offending WithParam call.
			var unsupportedType *json.UnsupportedTypeError
			var unsupportedValue *json.UnsupportedValueError
			if !errors.As(mErr, &unsupportedType) && !errors.As(mErr, &unsupportedValue) {
				t.Errorf("json.Marshal error = %v, want an UnsupportedTypeError or UnsupportedValueError", mErr)
			}
		})
	}
}

// TestNilParamsMarshalsAsNull covers the state every constructor starts in:
// Params is nil until the first WithParam call, and a transport will marshal
// that fresh error as-is.
func TestNilParamsMarshalsAsNull(t *testing.T) {
	t.Parallel()

	err := NotFound(jsonCode)
	if err.Params != nil {
		t.Fatalf("Params = %v, want nil on a fresh error", err.Params)
	}

	blob, mErr := json.Marshal(err.Params)
	if mErr != nil {
		t.Fatalf("json.Marshal(nil Params) returned an error: %v", mErr)
	}
	if got := string(blob); got != "null" {
		t.Errorf("marshalled nil Params = %s, want null", got)
	}

	// The whole error must still marshal, so a handler can serialize an error
	// that carries no params at all.
	whole, mErr := json.Marshal(err)
	if mErr != nil {
		t.Fatalf("json.Marshal(*Error) returned an error: %v", mErr)
	}
	var back map[string]any
	if uErr := json.Unmarshal(whole, &back); uErr != nil {
		t.Fatalf("json.Unmarshal returned an error: %v (payload %s)", uErr, whole)
	}
	if back["Code"] != jsonCode {
		t.Errorf("Code = %#v, want %q", back["Code"], jsonCode)
	}
	if back["Params"] != nil {
		t.Errorf("Params = %#v, want null", back["Params"])
	}
}
