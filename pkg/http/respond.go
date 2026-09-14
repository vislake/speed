package http

import (
	"encoding/json"
	"errors"
	"fmt"
	nethttp "net/http"
)

// jsonContentType is the media type WriteJSON announces. The charset is spelled
// out because a client that guesses gets it wrong on a body this package
// always writes as UTF-8.
const jsonContentType = "application/json; charset=utf-8"

// WriteJSON writes v as the JSON body of a response with the given status.
//
// Encoding happens before anything is written, so a value that cannot be
// encoded leaves the response untouched: no status line, no header, no byte of
// body. Encoding straight into the writer would instead emit 200 and half a
// document, and the handler would have no way left to report the failure.
func WriteJSON(w nethttp.ResponseWriter, status int, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("http: encoding the response body as JSON failed, nothing was written: %w", err)
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("http: writing the response body failed after the status was sent: %w", err)
	}
	return nil
}

// StatusFor maps an error from Decode to a status code, and everything else to
// 500. It returns a code and nothing more: the shape of an error response
// belongs to the module that provides the API, so this package defines no
// error body at all.
//
// A body that could not be decoded maps to 400 and a body its own Validate
// rejected maps to 422. The distinction is worth making to the client: 400
// says these bytes are not a shape this API can read, 422 says the shape is
// right and a value in it is not allowed, and the two are fixed differently.
//
// A nil error maps to 200. nil is not a failure, and that mapping is what lets
// WriteJSON(w, StatusFor(err), v) stand on the path where nothing went wrong,
// instead of forcing the caller to branch on the error twice.
func StatusFor(err error) int {
	switch {
	case err == nil:
		return nethttp.StatusOK
	case errors.Is(err, ErrBodyTooLarge):
		return nethttp.StatusRequestEntityTooLarge
	case errors.Is(err, ErrMalformedBody):
		return nethttp.StatusBadRequest
	case errors.Is(err, ErrValidation):
		return nethttp.StatusUnprocessableEntity
	default:
		return nethttp.StatusInternalServerError
	}
}
