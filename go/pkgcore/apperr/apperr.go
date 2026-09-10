// Package apperr defines the structured application error shared by every
// speed module: a stable, machine-readable error code, structured parameters
// for client-side interpolation, a suggested HTTP status, and an optional
// wrapped cause.
//
// APIs never return localized text. They return the code plus its parameters,
// and the client resolves the code through its own i18n catalog:
//
//	return apperr.NotFound("billing.subscription_not_found").
//		WithParam("id", id)
//
// The builders never write to their receiver, so an *Error may also be declared
// once as a package-level sentinel and decorated per request:
//
//	var ErrSubscriptionNotFound = apperr.NotFound("billing.subscription_not_found")
//
//	return ErrSubscriptionNotFound.WithParam("id", id)
//
// The package depends on the standard library only, so any module can use it
// without pulling in further dependencies.
package apperr

import (
	"errors"
	"maps"
	"net/http"
)

// causeSeparator joins the error code and the wrapped cause produced by
// (*Error).Error.
const causeSeparator = ": "

// Error is a structured application error. It carries a stable dotted code
// rather than a human-readable message, so transports can render it in the
// caller's language and logs can aggregate on it.
//
// The zero value is not useful; build one with NotFound, Invalid, Conflict,
// Unauthorized, Forbidden or Internal.
//
// An *Error is safe to share, including as a package-level sentinel: WithParam
// and WithCause derive a new instance instead of modifying the receiver, so
// concurrent requests never race on Params and never observe one another's
// parameters. Because each builder returns a new pointer, match a decorated
// error on its Code through As rather than by identity. Assigning to the
// exported fields directly still modifies the value in place, so only do that
// on an instance the caller owns.
type Error struct {
	// Code identifies the failure as "<module>.<reason>", for example
	// "billing.subscription_not_found". It is part of the public API contract
	// and must stay stable once released.
	Code string

	// Params carries structured parameters for i18n interpolation on the
	// client side. It is nil until the first WithParam call. Values are held as
	// given, so store data that is not mutated afterwards.
	//
	// Params is the part of the error a transport serializes verbatim into
	// the API response body and hands to an untrusted caller, so what may go
	// in is constrained -- see WithParam's doc comment for the two
	// prohibitions. Values a client must never see do not belong here at all:
	// record them with WithSensitiveParam, which keeps them out of this map.
	Params map[string]any

	// Status is the suggested HTTP status code. Constructors pre-fill it and
	// callers may override it when a specific endpoint needs a different one.
	Status int

	// cause is the optional underlying error, exposed through Unwrap so that
	// errors.Is and errors.As keep working across the boundary.
	cause error

	// sensitiveParams carries the values WithSensitiveParam recorded,
	// deliberately separated from Params so that no transport serializing
	// Params can carry them. See WithSensitiveParam's doc comment.
	sensitiveParams map[string]any
}

// Error implements the error interface. It renders the code alone, or
// "code: cause" when a cause is set. Params are deliberately left out: they are
// structured data for API responses, not for the Go error string. Values
// recorded with WithSensitiveParam are left out for the same reason and a
// stronger one: they exist for server-side diagnostics only, and making them
// printable text would put them where an unstructured log line or a wrapped
// error message could carry them. The only reader that can see them is
// SensitiveParams.
func (e *Error) Error() string {
	if e.cause == nil {
		return e.Code
	}
	return e.Code + causeSeparator + e.cause.Error()
}

// Unwrap returns the wrapped cause, or nil when none was set, so that
// errors.Is and errors.As traverse through this error.
func (e *Error) Unwrap() error {
	return e.cause
}

// WithParam records a parameter for client-side interpolation and returns a
// derived *Error, leaving the receiver untouched so a shared error can be
// decorated per request. Calls can still be chained; repeating a key overwrites
// the value inherited from the receiver.
//
// THE CONTRACT: a parameter recorded here is serialized VERBATIM into the HTTP
// response body and handed to an untrusted caller -- no transport layer
// filters or redacts this map, and no default redaction exists at this layer.
// Two prohibitions follow, and both rest on the caller, because the library
// never inspects a value's content or shape:
//
//   - Content: never record keys, secrets, tokens, personal data, internal
//     hostnames or resolved addresses. A value that is not safe to show the
//     caller who caused the error belongs with WithSensitiveParam instead,
//     which keeps it out of this map entirely.
//   - Shape: WithParam accepts any value and Params is map[string]any, so
//     only caller discipline stops a struct or aggregate from being stored
//     and serialized field by field. Never pass a struct, slice or map: a
//     field that is safe today silently rides into every API response that
//     carries the error tomorrow, and nothing reports it. Keep to known-safe
//     scalars -- ids, counts, booleans, short labels. A value that needs
//     more than a scalar belongs server-side, again through
//     WithSensitiveParam.
//
// A value recorded here must also be JSON-representable: Params is what a
// transport marshals, and a non-serializable value (a channel, a function,
// NaN) fails at marshal time, when the body is already being built.
func (e *Error) WithParam(key string, value any) *Error {
	derived := e.clone()
	if derived.Params == nil {
		derived.Params = make(map[string]any, 1)
	}
	derived.Params[key] = value
	return derived
}

// WithSensitiveParam records a parameter that must never reach a client and
// returns a derived *Error, leaving the receiver untouched exactly like
// WithParam. Calls can be chained; repeating a key overwrites the value
// inherited from the receiver.
//
// The caller declares sensitivity; the library never guesses it. The value is
// stored separately from Params, in a map no transport serializes: the
// envelope-building code that copies Params into an API response cannot
// carry it, and marshalling the *Error does not emit it. The value stays
// server-side, reachable only through SensitiveParams for a deliberate
// diagnostic -- a structured log attribute, never a response field.
//
// The separation protects the client, not the value: nothing here redacts,
// and a server-side log is still a disclosure surface a future reader will
// not think to guard. Record what an operator needs to diagnose the failure,
// nothing more -- never raw credentials or personal data a log line should
// not carry either.
//
// A value recorded here is never rendered into Error()'s string form.
func (e *Error) WithSensitiveParam(key string, value any) *Error {
	derived := e.clone()
	if derived.sensitiveParams == nil {
		derived.sensitiveParams = make(map[string]any, 1)
	}
	derived.sensitiveParams[key] = value
	return derived
}

// SensitiveParams returns the values recorded with WithSensitiveParam, or nil
// when none were. It exists for server-side diagnostics only: code that logs
// what a failure depended on reads it here and turns each entry into a
// structured attribute. A transport must never serialize this map into an
// API response -- keeping it out of the response is the entire point of the
// separation, and the accessor being exported does not make that safe.
func (e *Error) SensitiveParams() map[string]any {
	return e.sensitiveParams
}

// WithCause attaches the underlying error and returns a derived *Error, leaving
// the receiver untouched so a shared error can be decorated per request. The
// cause is reachable through Unwrap but never rendered into an API response.
func (e *Error) WithCause(err error) *Error {
	derived := e.clone()
	derived.cause = err
	return derived
}

// clone returns a shallow copy of e whose Params map is independent of the
// receiver's, so the builders can derive a new error without ever writing to a
// value another goroutine may be holding. maps.Clone keeps a nil map nil.
func (e *Error) clone() *Error {
	return &Error{
		Code:            e.Code,
		Params:          maps.Clone(e.Params),
		Status:          e.Status,
		cause:           e.cause,
		sensitiveParams: maps.Clone(e.sensitiveParams),
	}
}

// NotFound returns an *Error for a missing resource, with the HTTP status set
// to http.StatusNotFound.
func NotFound(code string) *Error {
	return newError(code, http.StatusNotFound)
}

// Invalid returns an *Error for a malformed or rejected request, with the HTTP
// status set to http.StatusBadRequest.
func Invalid(code string) *Error {
	return newError(code, http.StatusBadRequest)
}

// Conflict returns an *Error for a request that clashes with the current state
// of the resource, with the HTTP status set to http.StatusConflict.
func Conflict(code string) *Error {
	return newError(code, http.StatusConflict)
}

// Unauthorized returns an *Error for a missing or invalid credential, with the
// HTTP status set to http.StatusUnauthorized.
func Unauthorized(code string) *Error {
	return newError(code, http.StatusUnauthorized)
}

// Forbidden returns an *Error for an authenticated caller that lacks the
// required permission, with the HTTP status set to http.StatusForbidden.
func Forbidden(code string) *Error {
	return newError(code, http.StatusForbidden)
}

// Internal returns an *Error for an unexpected server-side failure, with the
// HTTP status set to http.StatusInternalServerError.
func Internal(code string) *Error {
	return newError(code, http.StatusInternalServerError)
}

// As reports whether err is, or wraps, an *Error and returns it when it does.
// It is a thin wrapper over errors.As for ergonomic use at transport
// boundaries.
func As(err error) (*Error, bool) {
	var appErr *Error
	if errors.As(err, &appErr) {
		return appErr, true
	}
	return nil, false
}

// HasCode reports whether err is, or wraps, an *Error carrying exactly code.
// It is the predicate form of the matching rule the Error doc comment states:
// decorating an error derives a new instance, so a decorated error is never
// the sentinel it was derived from and classification compares the Code --
// through As, never with == or errors.Is against the declared value.
//
// A nil err and an err that is not an *Error both report false. When the
// chain carries more than one *Error, the outermost one is the one compared,
// exactly as As returns it.
func HasCode(err error, code string) bool {
	appErr, ok := As(err)
	return ok && appErr.Code == code
}

// newError builds an *Error with the given code and suggested HTTP status.
func newError(code string, status int) *Error {
	return &Error{Code: code, Status: status}
}

// compile-time check that *Error satisfies the standard error interface.
var _ error = (*Error)(nil)
