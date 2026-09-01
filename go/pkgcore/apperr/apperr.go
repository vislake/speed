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
	Params map[string]any

	// Status is the suggested HTTP status code. Constructors pre-fill it and
	// callers may override it when a specific endpoint needs a different one.
	Status int

	// cause is the optional underlying error, exposed through Unwrap so that
	// errors.Is and errors.As keep working across the boundary.
	cause error
}

// Error implements the error interface. It renders the code alone, or
// "code: cause" when a cause is set. Params are deliberately left out: they are
// structured data for API responses, not for the Go error string.
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
func (e *Error) WithParam(key string, value any) *Error {
	derived := e.clone()
	if derived.Params == nil {
		derived.Params = make(map[string]any, 1)
	}
	derived.Params[key] = value
	return derived
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
		Code:   e.Code,
		Params: maps.Clone(e.Params),
		Status: e.Status,
		cause:  e.cause,
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

// newError builds an *Error with the given code and suggested HTTP status.
func newError(code string, status int) *Error {
	return &Error{Code: code, Status: status}
}

// compile-time check that *Error satisfies the standard error interface.
var _ error = (*Error)(nil)
