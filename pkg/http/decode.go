package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
)

// Validator is implemented by a request body type that checks itself. This
// package provides the mechanism and no rules at all: Decode guarantees a type
// that implements Validator has been validated, and what counts as valid is
// the type's own business.
//
// Rules declared through struct tags would need a third-party validation
// library, and this package's surface carries no third-party type.
type Validator interface {
	// Validate reports what is wrong with the value, or nil when nothing
	// is. Decode wraps the returned error, so a caller can reach its own
	// type back out with errors.As.
	Validate() error
}

// Decode reads r's body as JSON into dst, then calls dst.Validate when dst
// implements Validator.
//
// An unknown field is always rejected, and there is no switch that would let
// one through. A lenient decode turns a misspelled field name into a field
// that was never sent while the request still looks successful, which is the
// failure shape this project avoids everywhere. A handler that genuinely wants
// a lenient parse uses encoding/json directly.
//
// Decode does not limit the size of the body. The endpoint's configured limit
// is imposed by the outermost layer of the chain, so it covers every handler
// on the endpoint rather than only the ones that call Decode; a body cut off
// by that limit reaches here as an error wrapping ErrBodyTooLarge.
//
// The returned errors wrap ErrMalformedBody, ErrBodyTooLarge or ErrValidation.
// An empty body is one of the malformed ones and has no class of its own.
// Decode writes nothing to the response: the shape of an error response
// belongs to the module that provides the API, so a caller turns the error
// into a status code with StatusFor and writes the body it wants.
func Decode(r *nethttp.Request, dst any) error {
	dec := json.NewDecoder(requestBody(r))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeFailure(err)
	}
	var trailing json.RawMessage
	err := dec.Decode(&trailing)
	switch {
	case err == nil:
		return fmt.Errorf("%w: the body carries a second JSON value after the first. "+
			"Send exactly one JSON value", ErrMalformedBody)
	case !errors.Is(err, io.EOF):
		return decodeFailure(err)
	}
	if v, ok := dst.(Validator); ok {
		if err := v.Validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrValidation, err)
		}
	}
	return nil
}

// requestBody returns the reader to decode from. A request without a body is
// handled as an empty body rather than by panicking: a handler reaches Decode
// with whatever the client sent, and a client that sent nothing is data, not a
// defect in the calling code.
func requestBody(r *nethttp.Request) io.Reader {
	if r == nil || r.Body == nil {
		return nethttp.NoBody
	}
	return r.Body
}

// decodeFailure classifies a failure from the JSON decoder.
func decodeFailure(err error) error {
	var tooLarge *nethttp.MaxBytesError
	if errors.As(err, &tooLarge) {
		return fmt.Errorf("%w: the endpoint's limit is %d bytes: %w", ErrBodyTooLarge, tooLarge.Limit, err)
	}
	if errors.Is(err, io.EOF) {
		// An empty body is one more malformed body, not a class of its
		// own: calling Decode is how a handler declares it needs a
		// body, and what the caller does about an absent one is what it
		// does about a misshapen one. A handler that does not need a
		// body does not call Decode.
		return fmt.Errorf("%w: the body is empty, and an empty body is not a JSON value. "+
			"Send the JSON document the endpoint expects", ErrMalformedBody)
	}
	return fmt.Errorf("%w: %w", ErrMalformedBody, err)
}
