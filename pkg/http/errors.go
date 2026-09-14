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
	//
	// The text names both patterns and the endpoint they are on, and not
	// the registrants: the registration surface does not know who called
	// it, the same reason a layer has to declare its own Provides. A
	// pattern is a literal in the source, so naming it locates the call.
	ErrRouteConflict = errors.New("http: two route patterns on one endpoint conflict")
	// ErrMiddlewareCycle reports that the After and Before constraints of
	// an endpoint's middleware form a cycle, so no order satisfies them.
	// The text names every member of the cycle along with its own
	// declarations.
	ErrMiddlewareCycle = errors.New("http: middleware constraints form a cycle")
	// ErrChainAssembly reports that an endpoint's middleware chain could
	// not be built out of the layers registered on it. A Wrap that hands
	// back a nil handler is that class: the layer would leave a hole in the
	// chain, and the request would die on a nil handler with nothing naming
	// the layer that produced it. The text names the layer and the endpoint.
	//
	// It is a startup failure and not a panic, although the mistake is at
	// the call site: assembly runs in Serve, where a panic would skip the
	// registry's rollback and leave the sockets and goroutines of the
	// endpoints already bound with nobody to release them.
	ErrChainAssembly = errors.New("http: endpoint's middleware chain could not be assembled")
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
