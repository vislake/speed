// Package httpapi carries the two HTTP conventions every module's request
// handlers share: the coded error envelope a refusal is written as, and the
// bounded decode of a JSON request body.
//
// The envelope is how a structured application error crosses the HTTP
// boundary. APIs never return localized text: a refusal is a stable code
// plus structured parameters, serialized as {"code": ..., "params": ...},
// and the client resolves the code through its own i18n catalog (see
// go/pkgcore/apperr for the error value itself). WriteError is the one
// place that shape is built, so every module's surface -- and every
// generated fragment -- answers refusals identically.
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

// jsonContentType is the shared JSON content type of the envelope and of the
// responses around it.
const jsonContentType = "application/json; charset=utf-8"

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
		w.Header().Set("Content-Type", jsonContentType)
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
