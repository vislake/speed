package http

import "errors"

// The sentinel errors this module produces. A caller tells the classes apart
// with errors.Is, and the error text carries what is needed to locate the
// cause and states the fixing action. Wrapping is always with %w, so the
// underlying net error chain of a bind failure stays intact.
//
// The table is closed over the failures this module judges for itself: a newly
// identified class either joins an existing sentinel or adds one here.
// Failures that merely travel through this module are not in it — a handler
// and a middleware define their own, and this module invents no names for
// them.
var (
	// ErrUnknownEndpoint reports a lookup for an endpoint name that
	// configuration never declared. The text lists the names that do exist,
	// because that is the fixing action; there is no fallback to a default
	// endpoint.
	ErrUnknownEndpoint = errors.New("http: endpoint name was never declared")
	// ErrRouteConflict reports two route patterns on one endpoint that the
	// routing engine judged to be in conflict. It surfaces while the chain
	// is assembled in Serve, not at registration: a registration records a
	// declaration, and conflict is a property of the assembled set.
	ErrRouteConflict = errors.New("http: two route patterns on one endpoint conflict")
	// ErrMiddlewareCycle reports that the After and Before constraints of
	// an endpoint's middleware form a cycle, so no order satisfies them.
	// The text names every member of the cycle along with its own
	// declarations.
	ErrMiddlewareCycle = errors.New("http: middleware constraints form a cycle")
	// ErrInvalidSpec reports a declared OpenAPI fragment whose shape is not
	// usable: empty, not UTF-8, not JSON, not an object at the top level, or
	// an object without an openapi field. The content beyond that shape is
	// not checked here.
	ErrInvalidSpec = errors.New("http: OpenAPI fragment has an unusable shape")
	// ErrListen reports that an endpoint could not bind its address. It
	// wraps the underlying net error, so a caller can still reach for
	// syscall.EADDRINUSE through it.
	ErrListen = errors.New("http: endpoint could not bind its address")
	// ErrDrainTimeout reports that an endpoint's in-flight requests had not
	// finished when its configured drain timeout expired. The connections
	// still open at that moment are cut.
	ErrDrainTimeout = errors.New("http: endpoint did not drain within its timeout")
	// ErrMalformedBody reports a request body Decode could not read as the
	// target type: not JSON, an unknown field, trailing data.
	ErrMalformedBody = errors.New("http: request body is not usable as the target type")
	// ErrBodyTooLarge reports a request body that exceeded the endpoint's
	// configured limit. The limit is imposed by the outermost layer of the
	// chain, so it applies to every handler on the endpoint, including the
	// ones that never call Decode.
	ErrBodyTooLarge = errors.New("http: request body exceeds the endpoint's limit")
	// ErrValidation reports that the decoded value's own Validate rejected
	// it. The error Validate returned is wrapped inside, so the caller can
	// reach its own type back out with errors.As.
	ErrValidation = errors.New("http: request body failed its own validation")
)
