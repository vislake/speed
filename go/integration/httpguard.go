package integration

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// errorContentType is the media type every structured error response this
// module writes carries, matching the {code, params} envelope convention
// go/authn and go/tenancy already document (go/authn/middleware.go's
// errorBody).
const errorContentType = "application/json"

// Header names HTTPGuard.Middleware sets on a denied (429) response, on top
// of the standard Retry-After. docs/internal/07-platform-services.md asks
// for quota response headers without naming them, so this module picks the
// conventional X-RateLimit-* trio -- Remaining and Reset mirroring GitHub's
// and Stripe's own widely-copied header names, Layer an addition specific
// to this module's three-layer composition so a caller debugging a 429 can
// tell at a glance which of the three counters actually tripped without
// parsing the JSON body.
const (
	headerRateLimitRemaining = "X-RateLimit-Remaining"
	headerRateLimitReset     = "X-RateLimit-Reset"
	headerRateLimitLayer     = "X-RateLimit-Layer"
)

// Extractor derives the two caller-scoped rate-limit dimensions from an
// inbound request: tenantKey identifies the tenant layer (typically the
// resolved tenant id) and apiKeyID identifies the key layer (typically the
// authenticated API key's id or hash). Either may be the empty string --
// LayeredLimiter.Allow treats an empty key exactly like any other string
// key, so every anonymous or key-less request sharing the empty string
// simply shares one counter, which is a caller-visible consequence of
// supplying no better identifier, not a special case this package handles.
//
// This module takes no position on how a host authenticates a request or
// resolves its tenant -- Extractor is a plain function precisely so a host
// can close over its own authn/tenancy wiring (or, in a unit test, return
// fixed strings) without HTTPGuard importing anything about either.
type Extractor func(r *http.Request) (tenantKey, apiKeyID string)

// HTTPGuard is the HTTP-specific translation layer
// docs/internal/07-platform-services.md's own text assigns to this module:
// go/ratelimit's Decision is protocol-agnostic pure data, and translating a
// denied Decision into a 429 response is integration's own handler-layer
// job. go/ratelimit.Decision itself carries no notion of headers or status
// codes (see that type's own doc comment), so this is deliberately the one
// place in this module that knows what a Decision means as an HTTP
// response.
type HTTPGuard struct {
	limiter   *LayeredLimiter
	globalKey string
	extract   Extractor
}

// NewHTTPGuard returns an HTTPGuard evaluating limiter's three layers on
// every request Middleware wraps, with globalKey as the fixed storage key
// for the global layer (see LayeredLimiter.Allow's own doc comment for why
// that key is caller-supplied rather than hard-coded) and extract supplying
// the per-request tenant and key identifiers.
func NewHTTPGuard(limiter *LayeredLimiter, globalKey string, extract Extractor) *HTTPGuard {
	return &HTTPGuard{limiter: limiter, globalKey: globalKey, extract: extract}
}

// Middleware wraps next with the three-layer rate-limit check: a denied
// LayeredDecision short-circuits to a 429 response (via writeAppError, this
// module's ErrRateLimited decorated with WithRateLimitParams) and next is
// never called; an underlying error from the limiter itself (a KVStore
// failure) short-circuits to a 500 (ErrInternal) for the identical reason --
// a rate limiter that cannot answer must never be treated as "allow", per
// go/ratelimit.Limiter's own doc comment that it decides no fail-open/
// fail-closed policy on the caller's behalf. Only a genuine Allowed
// decision reaches next.
func (g *HTTPGuard) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tenantKey, apiKeyID := g.extract(r)

		decision, err := g.limiter.Allow(r.Context(), g.globalKey, tenantKey, apiKeyID)
		if err != nil {
			writeAppError(w, ErrInternal.WithCause(err))
			return
		}
		if !decision.Allowed {
			writeAppError(w, WithRateLimitParams(decision))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// WithRateLimitParams decorates ErrRateLimited with the two parameters
// errors.go's own doc comment promises -- "layer" and
// "retry_after_seconds" -- from a denied LayeredDecision, and is also what
// writeAppError inspects to set the Retry-After and quota headers. It is
// exported so a caller building its own response path (rather than going
// through Middleware) can still produce the identical decorated error.
func WithRateLimitParams(decision LayeredDecision) *apperr.Error {
	return ErrRateLimited.
		WithParam("layer", decision.Layer).
		WithParam("retry_after_seconds", retryAfterSecondsFromDecision(decision)).
		WithParam("remaining", decision.Decision.Remaining)
}

// retryAfterSecondsFromDecision converts a Decision's ResetAfter to a whole
// number of seconds, rounded up: Retry-After (RFC 9110 §10.2.3) is defined
// in whole seconds, and rounding down would tell a client it may retry
// before the window has actually reset, defeating the header's purpose.
func retryAfterSecondsFromDecision(decision LayeredDecision) int {
	seconds := math.Ceil(decision.Decision.ResetAfter.Seconds())
	if seconds < 0 {
		return 0
	}
	if seconds > math.MaxInt {
		return math.MaxInt
	}
	return int(seconds)
}

// errorBody is the {code, params} envelope every structured error this
// module writes over HTTP uses, matching go/authn/middleware.go's identical
// shape byte for byte -- deliberately not a shared type imported from
// go/authn (that would be a business-module-to-business-module import this
// codebase's module-boundary rule forbids), but the wire shape both modules
// converge on independently is exactly what lets one client-side error
// decoder handle every module's structured error the same way.
type errorBody struct {
	Code   string         `json:"code"`
	Params map[string]any `json:"params,omitempty"`
}

// writeAppError writes err as its structured envelope with its suggested
// HTTP status, setting Retry-After from a "retry_after_seconds" parameter
// when present (ErrRateLimited, via WithRateLimitParams) and the
// X-RateLimit-* quota headers from "layer"/"remaining" when present. Only
// the code and its parameters are written to the body -- an *apperr.Error's
// cause is never serialized, matching go/authn's identical rule.
func writeAppError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = ErrInternal.WithCause(err)
	}

	if seconds, ok := intParam(appErr, "retry_after_seconds"); ok {
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
		w.Header().Set(headerRateLimitReset, strconv.Itoa(seconds))
	}
	if layer, ok := stringParam(appErr, "layer"); ok {
		w.Header().Set(headerRateLimitLayer, layer)
	}
	if remaining, ok := intParam(appErr, "remaining"); ok {
		w.Header().Set(headerRateLimitRemaining, strconv.Itoa(remaining))
	}

	w.Header().Set("Content-Type", errorContentType)
	w.WriteHeader(appErr.Status)
	_ = json.NewEncoder(w).Encode(errorBody{Code: appErr.Code, Params: appErr.Params})
}

// intParam reads an int parameter from appErr.Params, reporting false when
// absent or of a different type.
func intParam(appErr *apperr.Error, key string) (int, bool) {
	if appErr.Params == nil {
		return 0, false
	}
	v, ok := appErr.Params[key].(int)
	return v, ok
}

// stringParam reads a string parameter from appErr.Params, reporting false
// when absent or of a different type.
func stringParam(appErr *apperr.Error, key string) (string, bool) {
	if appErr.Params == nil {
		return "", false
	}
	v, ok := appErr.Params[key].(string)
	return v, ok
}
