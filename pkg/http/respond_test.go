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

// startupSentinels are the classes that can only arise while the assembly is
// being built. They never travel to a client, so a handler that hands one to
// StatusFor is reporting a defect of its own.
var startupSentinels = []string{
	"ErrUnknownEndpoint", "ErrRouteConflict", "ErrMiddlewareCycle",
	"ErrInvalidSpec", "ErrListen", "ErrDrainTimeout",
}

// requestSentinels are the classes Decode produces, which the design says
// StatusFor maps to a status code of their own.
var requestSentinels = []string{"ErrMalformedBody", "ErrBodyTooLarge", "ErrValidation"}

// TestStatusForMapsEachSentinel checks every sentinel has a determinate
// mapping, and that the mapping is read with errors.Is rather than by
// comparison, so a wrapped sentinel maps the same way a bare one does.
//
// The exact code for ErrValidation and for a nil error is not written here:
// the design rules on neither (see the plan's F12), and pinning a provisional
// value would turn the ruling into a test failure. What is pinned is the part
// the design does settle.
func TestStatusForMapsEachSentinel(t *testing.T) {
	for name, sentinel := range sentinels {
		bare := StatusFor(sentinel)
		wrapped := StatusFor(fmt.Errorf("handling POST /things: %w", sentinel))
		if bare != wrapped {
			t.Errorf("%s maps to %d bare and %d wrapped; the mapping has to be read with errors.Is, "+
				"so that a handler may wrap the error with its own context", name, bare, wrapped)
		}
		if bare < 100 || bare > 599 {
			t.Errorf("%s maps to %d, which is not an HTTP status code", name, bare)
		}
	}
	for _, name := range startupSentinels {
		if got := StatusFor(sentinels[name]); got != nethttp.StatusInternalServerError {
			t.Errorf("%s maps to %d, want 500: it cannot be caused by the request", name, got)
		}
	}
	for _, name := range requestSentinels {
		got := StatusFor(sentinels[name])
		if got < 400 || got >= 500 {
			t.Errorf("%s maps to %d, want a 4xx: the request is what is wrong, and a 5xx tells "+
				"the client to retry an identical request", name, got)
		}
		t.Logf("%s currently maps to %d", name, got)
	}
	if got := StatusFor(ErrMalformedBody); got != nethttp.StatusBadRequest {
		t.Errorf("ErrMalformedBody maps to %d, want 400", got)
	}
	if got := StatusFor(ErrBodyTooLarge); got != nethttp.StatusRequestEntityTooLarge {
		t.Errorf("ErrBodyTooLarge maps to %d, want 413, the code that names the limit that was hit", got)
	}
}

// TestStatusForMapsAStrangerTo500 pins the design's catch-all: an error this
// package did not produce says nothing about the request.
func TestStatusForMapsAStrangerTo500(t *testing.T) {
	if got := StatusFor(errors.New("the billing ledger is unreachable")); got != nethttp.StatusInternalServerError {
		t.Errorf("an error from elsewhere maps to %d, want 500", got)
	}
}

// TestStatusForOnNoError records what a nil error maps to. Which code that
// should be is not ruled on (the plan's F12); what is asserted is only that a
// caller who reached here without a failure is not told the server broke.
func TestStatusForOnNoError(t *testing.T) {
	got := StatusFor(nil)
	if got >= 400 {
		t.Errorf("a nil error maps to %d, which reports a failure where there was none", got)
	}
	t.Logf("a nil error currently maps to %d", got)
}
