package http

import (
	"encoding/json"
	"errors"
	"fmt"
	nethttp "net/http"
	"testing"
)

// TestWriteJSONWritesStatusHeaderAndBody is the ordinary path.
func TestWriteJSONWritesStatusHeaderAndBody(t *testing.T) {
	w := newSpyWriter()
	if err := WriteJSON(w, nethttp.StatusCreated, thing{Name: "a", Count: 3}); err != nil {
		t.Fatalf("writing a well-formed value failed: %v", err)
	}
	if w.status != nethttp.StatusCreated {
		t.Errorf("the status written is %d, want %d", w.status, nethttp.StatusCreated)
	}
	if got := w.header.Get("Content-Type"); got != jsonContentType {
		t.Errorf("the Content-Type is %q, want %q", got, jsonContentType)
	}
	if got := string(w.body); got != `{"name":"a","count":3}` {
		t.Errorf("the body is %q, want the value's JSON encoding", got)
	}
}

// TestWriteJSONMarshalFailureWritesNothing pins that encoding happens before
// anything reaches the wire. Encoding straight into the writer would send 200
// and half a document, and the handler would be left with an error it can no
// longer report.
func TestWriteJSONMarshalFailureWritesNothing(t *testing.T) {
	w := newSpyWriter()
	err := WriteJSON(w, nethttp.StatusOK, func() {})
	if err == nil {
		t.Fatal("a value that cannot be encoded was reported as written")
	}
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Errorf("the encoder's own error is not reachable through the chain: %v", err)
	}
	if w.wroteHeader {
		t.Errorf("the status %d was sent before the value was known to encode", w.status)
	}
	if len(w.body) != 0 {
		t.Errorf("%d bytes of body were written for a value that cannot be encoded: %q", len(w.body), w.body)
	}
	if got := w.header.Get("Content-Type"); got != "" {
		t.Errorf("the Content-Type %q was set for a response that was never sent", got)
	}
}

// TestWriteJSONReportsAWriteFailure covers the half where the failure happens
// after the status is on the wire: the caller can no longer change the
// response, but it still has to be able to log what happened.
func TestWriteJSONReportsAWriteFailure(t *testing.T) {
	broken := errors.New("connection reset by peer")
	w := newSpyWriter()
	w.writeErr = broken
	err := WriteJSON(w, nethttp.StatusOK, thing{Name: "a"})
	if err == nil {
		t.Fatal("a failed write was reported as success")
	}
	if !errors.Is(err, broken) {
		t.Errorf("the underlying write error is not reachable through the chain: %v", err)
	}
}

// requestSentinels are the classes Decode produces, each with the status the
// design gives it. Every other sentinel in the table maps to 500: it can only
// arise while the assembly is being built, never travels to a client, and a
// handler that hands one to StatusFor is reporting a defect of its own.
//
// The 500 side is derived rather than listed. A second written-out list would
// stop covering the table the day a sentinel is added to it, and the addition
// would look like nothing happened here.
var requestSentinels = map[string]int{
	"ErrMalformedBody": nethttp.StatusBadRequest,
	"ErrBodyTooLarge":  nethttp.StatusRequestEntityTooLarge,
	"ErrValidation":    nethttp.StatusUnprocessableEntity,
}

// TestStatusForMapsEachSentinel pins the whole table: the three request
// classes each map to the code the design gives them, everything else maps to
// 500, and the mapping is read with errors.Is rather than by comparison, so a
// wrapped sentinel maps the way a bare one does.
func TestStatusForMapsEachSentinel(t *testing.T) {
	for name, sentinel := range sentinels {
		bare := StatusFor(sentinel)
		wrapped := StatusFor(fmt.Errorf("handling POST /things: %w", sentinel))
		if bare != wrapped {
			t.Errorf("%s maps to %d bare and %d wrapped; the mapping has to be read with errors.Is, "+
				"so that a handler may wrap the error with its own context", name, bare, wrapped)
		}
		want, isRequestClass := requestSentinels[name]
		if !isRequestClass {
			want = nethttp.StatusInternalServerError
		}
		if bare != want {
			t.Errorf("%s maps to %d, want %d", name, bare, want)
		}
	}
	for name := range requestSentinels {
		if _, ok := sentinels[name]; !ok {
			t.Errorf("%s is expected here but is not in the package's sentinel table, so the "+
				"status it is given above was never checked against anything", name)
		}
	}
}

// TestStatusForDecodeFailureIsBadRequest and
// TestStatusForValidationIsUnprocessable pin the distinction the design draws
// between the two: 400 says these bytes are not a shape this API can read, 422
// says the shape is right and a value in it is not allowed. Both start from a
// real Decode call rather than a bare sentinel, so the classification Decode
// performs is part of what is pinned. Mapping validation to 400 as well turns
// the second one red.
func TestStatusForDecodeFailureIsBadRequest(t *testing.T) {
	var got thing
	err := Decode(postWithBody(`{"name":`), &got)
	if err == nil {
		t.Fatal("a truncated JSON body was accepted")
	}
	if status := StatusFor(err); status != nethttp.StatusBadRequest {
		t.Errorf("a body that could not be decoded maps to %d, want 400", status)
	}
}

func TestStatusForValidationIsUnprocessable(t *testing.T) {
	var got checkedThing
	err := Decode(postWithBody(`{"name":""}`), &got)
	if err == nil {
		t.Fatal("a body its own Validate rejects was accepted")
	}
	if status := StatusFor(err); status != nethttp.StatusUnprocessableEntity {
		t.Errorf("a body that parsed but failed its own rule maps to %d, want 422: 400 would "+
			"tell the client its JSON is malformed, which it is not", status)
	}
}

// TestStatusForMapsAStrangerTo500 pins the design's catch-all: an error this
// package did not produce says nothing about the request.
func TestStatusForMapsAStrangerTo500(t *testing.T) {
	if got := StatusFor(errors.New("the billing ledger is unreachable")); got != nethttp.StatusInternalServerError {
		t.Errorf("an error from elsewhere maps to %d, want 500", got)
	}
}

// TestStatusForNilIsOK pins the mapping that lets one call site serve both
// outcomes. nil is not a failure, and 200 is what makes
// WriteJSON(w, StatusFor(err), v) stand on the path where nothing went wrong.
func TestStatusForNilIsOK(t *testing.T) {
	if got := StatusFor(nil); got != nethttp.StatusOK {
		t.Errorf("a nil error maps to %d, want 200", got)
	}
}

// TestWriteJSONWithStatusForNilSendsTheValue walks the call the mapping exists
// for, rather than reading StatusFor(nil) on its own: the successful response
// has to arrive at the writer as 200 with the value's encoding. A StatusFor
// that answered 0 or 500 for nil would be visible here as the status on the
// wire.
func TestWriteJSONWithStatusForNilSendsTheValue(t *testing.T) {
	var err error
	w := newSpyWriter()
	value := thing{Name: "a", Count: 3}
	if writeErr := WriteJSON(w, StatusFor(err), value); writeErr != nil {
		t.Fatalf("writing the success response failed: %v", writeErr)
	}
	if !w.wroteHeader {
		t.Fatal("no status was written at all")
	}
	if w.status != nethttp.StatusOK {
		t.Errorf("the success response went out as %d, want 200", w.status)
	}
	if got := string(w.body); got != `{"name":"a","count":3}` {
		t.Errorf("the body is %q, want the value's JSON encoding", got)
	}
}
