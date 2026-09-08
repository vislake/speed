package rbac

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// permissionSeparator is the single character that divides a permission
// string's two halves, "<resource>:<action>" -- the naming convention
// Permission composes.
const permissionSeparator = ":"

// authzErrorContentType is the Content-Type the middleware sets on every
// response it writes itself, matching go/tenancy's Middleware so a client
// parses one error shape across the whole fixed chain.
const authzErrorContentType = "application/json; charset=utf-8"

// MiddlewareOption configures RequirePermission and RequirePermissionFunc.
type MiddlewareOption func(*middlewareConfig)

// middlewareConfig accumulates the options passed to the two constructors.
type middlewareConfig struct {
	// subjectFrom produces the Subject a request is decided for. It
	// defaults to reading the one the authenticating side installed with
	// WithSubject; WithSubjectResolver replaces it.
	subjectFrom func(*http.Request) (Subject, bool)
}

// subjectFromRequestContext is the default subjectFrom: the Subject the
// authenticating middleware installed on the request context.
func subjectFromRequestContext(r *http.Request) (Subject, bool) {
	return SubjectFromContext(r.Context())
}

// WithSubjectResolver replaces how the middleware obtains the Subject for
// a request.
//
// This is the module's second no-import seam, and it exists for the same
// reason SubtreeResolver does. By default the middleware reads the Subject
// the authenticating side installed with WithSubject, which already
// requires no import in either direction. A host whose authenticating
// layer carries identity some other way -- its own context key, a value
// its framework threads through, a demo header in an example app -- wires
// an adapter here instead of teaching rbac what that layer looks like or
// teaching that layer about rbac's context key.
//
// fn must fail closed: returning ok == false denies the request. It must
// never derive the tenant from anything the caller controls (a header, a
// query parameter, a body field) -- the tenant belongs to the verified
// access token's claims. A nil fn is ignored, so a caller that passes one
// by accident keeps the context-reading default rather than a middleware
// that denies everything.
func WithSubjectResolver(fn func(*http.Request) (Subject, bool)) MiddlewareOption {
	return func(c *middlewareConfig) {
		if fn != nil {
			c.subjectFrom = fn
		}
	}
}

// RequirePermission returns net/http middleware that lets a request
// through only when its Subject holds permission, given as the composed
// "<resource>:<action>" string Permission produces.
//
// It is rbac's whole contribution to the HTTP layer -- the permission
// gate the fixed middleware chain names after authentication -- and
// deliberately not a route: this module mounts no endpoints of its own
// (see Module.OpenAPISpec).
//
// Everything about it fails closed. A request with no usable Subject, a
// permission string that does not parse, and a subject that simply lacks
// the permission all end the same way: 403 Forbidden with the
// rbac.permission_denied code. The three are NOT distinguished in the
// response on purpose -- an unauthenticated caller learning that a
// permission exists and that it is merely not signed in yet is free
// reconnaissance, and rbac has no authentication opinion to express
// anyway (it never learns how identity was established, which is the
// point of Subject).
//
// A failure to reach a DECISION is different from a denial and is
// reported differently: an Authorizer error means the decision genuinely
// could not be performed -- storage unreachable, an engine failure -- so
// the response is 500 with rbac.storage_error. The request still does not
// proceed -- an undecidable check is never allowed through -- but a client
// sees a retryable server failure rather than a permanent "you may not",
// which is the honest answer and the one that will not send a user chasing
// a permission they already hold. An incomplete subject is NOT such a
// failure: the middleware refuses it as a denial before any decision is
// attempted (see the subject validation in RequirePermissionFunc), so an
// invalid subject can never be misreported as a retryable server error.
//
// The gate is COARSE. It answers Can, which ignores organization-tree
// scope, so passing it means the request may proceed, not that every row
// is visible. A handler returning tenant data must also call
// Authorizer.DataScope and filter with it; see the DataScope type.
func RequirePermission(az Authorizer, permission string, opts ...MiddlewareOption) func(http.Handler) http.Handler {
	return RequirePermissionFunc(az, func(*http.Request) string { return permission }, opts...)
}

// RequirePermissionFunc is RequirePermission with the required permission
// chosen per request, for a handler mounted at one path that needs
// different authority per method (a read on GET, a write on POST) or per
// sub-resource.
//
// permissionFor is called on every request and must be a pure function of
// the request's ROUTE -- its method and path -- never of a header, a query
// parameter or a body field. A permission chosen by something the caller
// controls is a permission the caller can choose to be one they hold.
// Returning "" (or any string that is not "<resource>:<action>") denies
// the request, so a method the table forgot fails closed instead of
// falling through unguarded, which is why there is no "no permission
// required" return value.
//
// Everything else -- the fail-closed rules, the response shapes, the
// coarseness of the gate -- is exactly as RequirePermission documents.
func RequirePermissionFunc(az Authorizer, permissionFor func(*http.Request) string, opts ...MiddlewareOption) func(http.Handler) http.Handler {
	cfg := &middlewareConfig{subjectFrom: subjectFromRequestContext}
	for _, opt := range opts {
		opt(cfg)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A nil Authorizer or a nil permission function is a host
			// wiring bug, not a request-level condition, and it is the one
			// case that must not be reported as a denial: silently 403ing
			// every request would look exactly like a correctly-configured
			// gate refusing an unauthorized user, and nothing would ever
			// point at the wiring. ErrServiceNotAttached names it.
			if az == nil || permissionFor == nil {
				writeAuthzError(w, ErrServiceNotAttached)
				return
			}

			permission := permissionFor(r)
			resource, action, ok := splitPermission(permission)
			if !ok {
				writeAuthzError(w, ErrPermissionDenied.WithParam("permission", permission))
				return
			}

			sub, ok := cfg.subjectFrom(r)
			if !ok || !sub.Valid() {
				// A subject that fails Valid is no subject, exactly like an
				// absent one. The default subjectFrom already folds that
				// into ok=false (SubjectFromContext rejects an incomplete
				// subject), but a host's WithSubjectResolver hands the
				// middleware a raw Subject nothing else has checked, so the
				// gate re-checks Valid here. An invalid subject must never
				// reach Can: the Service's grantsFor answers
				// ErrSubjectRequired for it, which would surface here as a
				// 500 rbac.storage_error -- an authorization failure
				// expressed as a retryable server error, and a 403-shaped
				// refusal misread by every client that treats 5xx as
				// transient. Refusing it as the same denial an absent
				// subject gets also keeps the response unable to
				// distinguish "malformed identity" from "no identity",
				// which is precisely how the fail-closed doc comment above
				// wants every unusable-subject case to look.
				writeAuthzError(w, ErrPermissionDenied.WithParam("permission", permission))
				return
			}

			allowed, err := az.Can(r.Context(), sub, action, resource)
			if err != nil {
				// The cause is deliberately dropped rather than written to
				// the body: it may carry SQL fragments or internal
				// identifiers, and internal detail must never reach an API
				// response. rbac does not log it either: the 5xx this
				// writes is recorded by the host's own observability
				// middleware.
				writeAuthzError(w, ErrStorage)
				return
			}
			if !allowed {
				writeAuthzError(w, ErrPermissionDenied.WithParam("permission", permission))
				return
			}

			// The Subject travels on to the handler, so a handler that
			// needs DataScope for row filtering reads back the very
			// identity this gate decided on rather than resolving it a
			// second time and possibly differently. The context is only
			// rebuilt when it does not already carry this exact Subject,
			// which is the common case when the authenticating side
			// installed it (Subject is comparable: two strings).
			ctx := r.Context()
			if current, present := SubjectFromContext(ctx); !present || current != sub {
				r = r.WithContext(WithSubject(ctx, sub))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// splitPermission is the inverse of Permission: it divides
// "<resource>:<action>" at its FIRST separator and reports whether the
// result is well-formed, meaning both halves are non-empty and neither
// contains a further separator.
//
// It is strict because it guards a gate. "notes:" , ":read", "notes" and
// "a:b:c" all name a permission no module could have declared through
// Permission, so treating any of them as a near-miss and guessing would
// turn a typo in a route table into an unguarded route. They are refused,
// and the caller denies.
func splitPermission(permission string) (resource, action string, ok bool) {
	resource, action, found := strings.Cut(permission, permissionSeparator)
	if !found || resource == "" || action == "" || strings.Contains(action, permissionSeparator) {
		return "", "", false
	}
	return resource, action, true
}

// authzErrorBody is the JSON shape the middleware writes, the {code,
// params} structured-error convention and the same body go/tenancy's
// Middleware produces.
type authzErrorBody struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// writeAuthzError writes appErr to w as that JSON body, at the status its
// apperr constructor pre-filled. The localized prose is never written:
// the API returns a code plus parameters and the client resolves it
// against locales/{zh-CN,en-US}.toml.
func writeAuthzError(w http.ResponseWriter, appErr *apperr.Error) {
	w.Header().Set("Content-Type", authzErrorContentType)
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(authzErrorBody{
		Code:   appErr.Code,
		Params: appErr.Params,
	})
}
