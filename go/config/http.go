package config

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/vislake/speed/go/config/api"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/httpapi"
)

const (
	// PathPublic is where this module's unauthenticated public
	// configuration endpoint is mounted: /api/v1/config/public, under the
	// versioned, module-scoped prefix every other module's routes follow
	// and the one this module's OpenAPI fragment (api/openapi.yaml, which
	// declares the operation as config_getPublicConfig) merges under. It
	// answers GET and HEAD with the effective values of every Public item
	// plus the enabled feature flags, resolved for the tenant the
	// request's host maps to (see ConfigGetPublicConfig). Hosts mount
	// config routes behind their own tenant middleware; the path constant
	// is exported so a host can name the route in an allowlist without
	// stringly duplicating it -- the reference app does exactly that
	// (internal/app/server.go's middleware allowlist).
	//
	// Both this path and PathSystemFeatures are a breaking change for a
	// host that addressed the endpoints by their earlier, unversioned
	// paths by hand; a host that named these constants follows the move
	// without an edit, which is the point of exporting them.
	PathPublic = "/api/v1/config/public"

	// PathSystemFeatures is where this module's feature-flag endpoint is
	// mounted: /api/v1/config/features -- the enablement half of the same
	// host-resolved display answer, declared by the same fragment as
	// config_getSystemFeatures. Both endpoints are GET/HEAD-only and
	// pre-auth; both are rate limited per caller address (see ratelimit.go
	// and checkPreAuthIPLimit) and both declare their caching policy in
	// the response headers (see publicCacheMaxAge). The generated hooks
	// their fragment produces are the primary frontend call surface for
	// them, with @speed/api-client's config-fetcher and its hooks
	// remaining the mapping layer over the endpoints' dynamic config keys.
	PathSystemFeatures = "/api/v1/config/features"
)

// publicCacheMaxAge is the freshness lifetime a successful pre-auth answer
// declares: one minute. The answer is display-only and per-tenant -- brand
// items and the enabled flag list -- and changes only when an operator
// writes a configuration row, so letting every visitor of one host share
// one copy for a bounded minute is exactly what the endpoint's caching
// contract wants. That is the deliberate difference from an access-gated
// resource (go/sharing's no-store), where a cached copy outliving a
// revocation is the failure mode; nothing here can be revoked.
//
// The bound exists because no HTTP cache can be told that a value changed.
// The module's own config.item.changed subscription invalidates its
// process-local cache the moment a write lands, so an origin replica never
// answers from a superseded row; a browser or intermediary holding a copy
// is out of that event's reach, and only the freshness lifetime it was
// handed can bound how long it may keep serving it. Hence a small constant
// rather than a long one: it is the maximum staleness a brand or flag
// change can exhibit to a client that already cached the answer.
const publicCacheMaxAge = 60 * time.Second

// publicCacheMaxAgeSeconds is publicCacheMaxAge in the whole seconds the
// Cache-Control max-age directive speaks, computed from the duration so the
// header and the constant cannot drift apart.
const publicCacheMaxAgeSeconds = int(publicCacheMaxAge / time.Second)

// setPublicCacheHeaders marks a successful answer cacheable for
// publicCacheMaxAge and names the request dimension it varies on. The
// answer is resolved per tenant from the request's Host (see
// requestContext), so Vary: Host states that dependency explicitly --
// though a host is part of a URL's authority and a conforming cache keys
// on it already, an intermediary that normalizes the authority (as some
// forward proxies do) would otherwise be free to collapse two tenants'
// answers into one entry.
//
// Refusals never get these headers: they carry no-store instead (see
// writeError and handleMethodNotAllowed).
func setPublicCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(publicCacheMaxAgeSeconds))
	w.Header().Set("Vary", "Host")
}

// jsonContentType is the Content-Type every response below writes. It is
// the same constant notes' handler uses, kept locally because the module
// cannot import the reference app.
const jsonContentType = "application/json; charset=utf-8"

// writeError writes err to w as the coded error envelope the fragment
// declares for every refusal on both operations (see pkgcore/httpapi). An
// err that is not an *apperr.Error -- something below this handler failed
// to classify -- is folded into ErrStorage, the module's internal error, so
// a caller never sees raw Go error text (nor, on a storage failure, the
// underlying cause chain, which can name a driver, a host or a ciphertext
// detail).
//
// The response is marked Cache-Control: no-store before anything else is
// written: a refusal is never a cacheable answer. A cached 429 would keep
// refusing requests past the window its retry_after_seconds announced, and
// a cached 500 would hide the recovery of whatever failed -- neither is a
// display decision the endpoint's publicCacheMaxAge applies to.
func writeError(w http.ResponseWriter, err error) {
	w.Header().Set("Cache-Control", "no-store")
	httpapi.WriteError(w, err, ErrStorage)
}

// methodNotAllowedCode is the stable code handleMethodNotAllowed answers
// with. It is a plain constant rather than an *apperr.Error: the
// endpoints' method contract is an HTTP convention, not an
// apperr-classified operation, and the fragment's 405 response documents
// exactly this code.
const methodNotAllowedCode = "config.method_not_allowed"

// handleMethodNotAllowed answers the 405 every non-GET/HEAD request to
// either endpoint gets, with the Allow header a well-behaved client
// follows. preAuthHandler registers it as a methodless pattern on the
// fragment's mux, which is where it earns its keep: net/http itself
// answers a method mismatch on a registered "GET <path>" pattern, as a
// plain-text 405 with no envelope and no caching header, and this handler
// is what keeps the endpoint's own structured refusal in front of that
// built-in. Like every other refusal it carries Cache-Control: no-store.
func handleMethodNotAllowed(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Allow", "GET, HEAD")
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(http.StatusMethodNotAllowed)
	_ = json.NewEncoder(w).Encode(api.ConfigError{Code: methodNotAllowedCode})
}

// preAuthHandler returns the http.Handler both pre-auth paths are mounted
// as: the spec fragment's generated wrapper (api.HandlerWithOptions over
// the mux below), which routes each operation by the request's own literal
// path, so what a request reaches is the fragment's operation -- never a
// hand-written dispatch that could drift from the spec.
//
// The two methodless patterns are the other half of that assembly: a
// methodless pattern is less specific than the generated "GET <path>" one,
// so a GET (or a HEAD, which net/http matches against a GET pattern too)
// still reaches its operation, while every other method lands on
// handleMethodNotAllowed -- this module's structured 405, answered from
// local state alone, cheaper than the limiter's own store round trip, so
// gating it behind the limiter would only make a method-mismatch flood
// more expensive to answer than to send.
//
// api.HandlerWithOptions' default ErrorHandlerFunc -- the plain-text 400
// its parameter binder calls when binding rejects a request -- is
// unreachable here: neither operation declares a parameter, so the
// generated wrapper has nothing to bind and no request can be refused
// before its operation method runs.
func (m *Module) preAuthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(PathPublic, handleMethodNotAllowed)
	mux.HandleFunc(PathSystemFeatures, handleMethodNotAllowed)
	return api.HandlerWithOptions(m, api.StdHTTPServerOptions{BaseRouter: mux})
}

// ConfigGetPublicConfig implements api.ServerInterface: GET
// /api/v1/config/public. The generated wrapper routes GET -- and HEAD,
// which net/http matches against the wrapper's GET pattern -- here; every
// other method is answered by preAuthHandler's methodless fallback, before
// this method is ever reached.
//
// It returns the public configuration snapshot -- every Public item's
// effective value plus every enabled feature flag -- for the tenant the
// request resolves to, as the fragment's ConfigPublicSnapshot: durations
// render as their canonical "1m30s" text, booleans and ints as their JSON
// natives, and no Sensitive key can appear at all (pkgcore's declaration
// validation makes Sensitive and Public mutually exclusive). A Public item
// with no value anywhere -- no row at any reachable scope and no declared
// Default -- is omitted from the snapshot rather than failing the response
// (see PublicSnapshot).
//
// Tenant resolution order: custom domain first, platform subdomain second,
// platform defaults last -- and never an error. The module's own resolver
// (WithResolver) is consulted per request; when it reports no tenant, or
// when no resolver is wired, the request reads platform defaults.
//
// The endpoint is deliberately pre-auth, serving only display decisions,
// never data: public configuration is a login-page dependency, so the
// answer must never depend on a sign-in that has not happened yet.
//
// Being pre-auth is also what makes the per-address budget below load
// bearing: any caller can ask, so the only identity the request carries is
// its own network address. A successful answer carries the endpoints'
// caching contract (setPublicCacheHeaders).
func (m *Module) ConfigGetPublicConfig(w http.ResponseWriter, r *http.Request) {
	if m.service == nil {
		// The window between Register and Attach; a host wiring bug (see
		// (*Module).Attach's doc comment).
		writeError(w, ErrServiceNotAttached)
		return
	}
	// The abuse gate runs before any resolution or store work.
	if err := m.service.checkPreAuthIPLimit(r.Context(), clientIP(r)); err != nil {
		writeError(w, err)
		return
	}
	values, features, err := m.service.PublicSnapshot(m.requestContext(r))
	if err != nil {
		writeError(w, err)
		return
	}
	setPublicCacheHeaders(w)
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(api.ConfigPublicSnapshot{
		Config:   values,
		Features: features,
	})
}

// ConfigGetSystemFeatures implements api.ServerInterface: GET
// /api/v1/config/features, registered exactly like ConfigGetPublicConfig.
// It returns the feature-flag half of the snapshot alone, as the
// fragment's ConfigSystemFeatures: a client that only needs to know what
// is on need not fetch the public configuration too. Everything
// ConfigGetPublicConfig says about tenant resolution, pre-auth status,
// method gating and the caching contract applies unchanged -- including
// the rate-limit budget, which both endpoints consume from the same
// per-address key, because the two serve one page-load worth of the same
// display answer.
func (m *Module) ConfigGetSystemFeatures(w http.ResponseWriter, r *http.Request) {
	if m.service == nil {
		writeError(w, ErrServiceNotAttached)
		return
	}
	if err := m.service.checkPreAuthIPLimit(r.Context(), clientIP(r)); err != nil {
		writeError(w, err)
		return
	}
	features, err := m.service.EnabledFlags(m.requestContext(r))
	if err != nil {
		writeError(w, err)
		return
	}
	setPublicCacheHeaders(w)
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(api.ConfigSystemFeatures{Features: features})
}

// requestContext returns the request's context carrying the tenant the
// configured resolver maps the request to -- when it maps to one. An
// unmatched host, a resolver error, or no resolver at all leaves the
// context untagged, which is exactly the platform-defaults tier: every
// read below resolves from the system row down, never failing over a host
// the resolver does not know (the unauthenticated display rule).
func (m *Module) requestContext(r *http.Request) context.Context {
	ctx := r.Context()
	if m.resolver == nil {
		return ctx
	}
	tenant, err := m.resolver.Resolve(r)
	if err != nil || tenant == "" {
		return ctx
	}
	return pkgcore.WithTenant(ctx, tenant)
}

// clientIP reports the direct connection's address from r.RemoteAddr, with
// the port stripped. It is the key of the per-address rate-limit dimension
// (checkPreAuthIPLimit), and this module deliberately does NOT honor
// X-Forwarded-For or any other caller-supplied header here: these two
// endpoints are unauthenticated, exactly the shape a caller can freely
// spoof such a header against, and a spoofable rate-limit key would let one
// attacker mint itself unlimited budgets by rotating a header value on
// every request. A host that terminates TLS behind a trusted reverse proxy
// and wants the proxy's forwarded address instead is expected to normalize
// r.RemoteAddr itself, ahead of these handlers -- the trusted-proxy decision
// is host-side, the same way go/sharing's access route and go/authn's
// proxy-aware clientIP treat it.
//
// An empty RemoteAddr (a caller that supplied none -- a request built in
// process rather than read off a socket) resolves to the empty string,
// which shares one counter with every other empty-address caller: a
// consequence of supplying no better identifier, not a case this function
// special-cases into a free pass.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// compile-time check that *Module implements the api.ServerInterface
// generated from this module's api/openapi.yaml -- the enforcement half of
// the spec-first flow: add an operation to the fragment, regenerate, and
// this assertion stops compiling until Module implements it.
var _ api.ServerInterface = (*Module)(nil)
