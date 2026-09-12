// Package httpapi carries the three HTTP conventions every module's request
// handlers share: how a success body is written as JSON (WriteJSON), the
// coded error envelope a refusal is written as (WriteError), and the bounded
// decode of a JSON request body (DecodeJSON).
//
// A JSON response is the one shape every module's surface answers with:
// WriteJSON sets the shared content type, writes the status and encodes the
// value in one place, so success bodies -- like the refusals below -- answer
// identically, byte for byte, across every module and every generated
// fragment.
//
// The envelope is how a structured application error crosses the HTTP
// boundary. APIs never return localized text: a refusal is a stable code
// plus structured parameters, serialized as {"code": ..., "params": ...},
// and the client resolves the code through its own i18n catalog (see
// go/pkgcore/apperr for the error value itself). WriteError is the one
// place that shape is built for a refusal the module classifies as an
// application error, so every module's surface -- and every generated
// fragment -- answers such refusals identically. A protocol-level refusal
// with no apperr-classified operation behind it -- a module's answer to a
// method its endpoints do not allow -- is the one deliberate exception:
// it writes the same shape from its fragment's generated type instead
// (go/config/http.go's handleMethodNotAllowed).
//
// DecodeJSON is the request-side half: it bounds the body before decoding
// and maps every decode failure, an oversized body included, onto the
// module's own coded invalid-request-body refusal.
//
// The package depends on the standard library and go/pkgcore/apperr only,
// so any module can use it.
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// JSONContentType is the media type of every JSON response the shared
// surface writes: the success bodies of WriteJSON and the coded error
// envelope of WriteError both carry it.
const JSONContentType = "application/json; charset=utf-8"

// WriteJSON writes v to w as the shared JSON response: the shared JSON
// content type, the status line, and the JSON encoding of v -- which the
// encoder terminates with a newline, so a JSON body always ends the same
// way regardless of what wrote it.
//
// The content type is set unconditionally: every JSON response on the
// shared surface carries JSONContentType, and a response whose contract
// pins a different media type writes that body itself rather than going
// through here (the PEM and octet-stream paths among the modules' own
// handlers).
//
// An encode failure after the status line went out is a broken client
// connection, not a response defect -- the status is already committed, so
// there is nothing left to answer with and the error is dropped.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", JSONContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// errorEnvelope is the wire shape WriteError emits. The two fields marshal
// in declaration order ("code" before "params"), which is also the order a
// map-shaped envelope produces, so the serialized bytes are stable.
type errorEnvelope struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// WriteError writes err to w as the coded error envelope: the JSON body
// {"code": ..., "params": ...} under the HTTP status the error's apperr
// constructor suggested (400 for Invalid, 404 for NotFound, and so on).
//
// An err that is not, and does not wrap, an *apperr.Error -- something below
// the handler did not classify it -- is written as fallback instead, so raw
// Go error text never reaches a caller either way. The fallback carries its
// own code, status and parameters; a caller whose err is statically an
// *apperr.Error (a package sentinel) may pass nil, since the fallback branch
// is then unreachable.
//
// Only the code and its parameters are serialized. An *apperr.Error's cause
// -- the WithCause detail, which may name a driver, a host or a ciphertext
// failure -- stays server-side, and a parameter-less error omits the params
// key entirely rather than emitting "params": null.
//
// The response's Content-Type is the shared JSON type unless the caller has
// already set one, so an endpoint whose contract pins a different content
// type sets its own header before calling.
func WriteError(w http.ResponseWriter, err error, fallback *apperr.Error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = fallback
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", JSONContentType)
	}
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{Code: appErr.Code, Params: appErr.Params})
}

// DecodeJSON decodes r's JSON body into dst and reports whether it did.
//
// A positive maxBytes bounds the body BEFORE decoding: a body that exceeds
// the bound fails as soon as the read passes the limit rather than after the
// whole body has been buffered, and passing w lets net/http ask the server
// to close the connection after the oversized request. A non-positive
// maxBytes decodes without a bound, for the endpoints that set no body
// limit.
//
// On any decode failure -- malformed JSON, a value of the wrong shape, or a
// body past the bound -- DecodeJSON writes invalid, carrying the decode
// error as its cause, through WriteError and reports false; the oversize
// answer is thus the same coded refusal as malformed JSON. The cause stays
// server-side, which is what makes the failure diagnosable from a log or
// trace without putting decoder detail on the wire. invalid is the calling
// module's own invalid-request-body error, so its code is the one that
// module's catalog documents.
func DecodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any, invalid *apperr.Error) bool {
	if maxBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		WriteError(w, invalid.WithCause(err), invalid)
		return false
	}
	return true
}
