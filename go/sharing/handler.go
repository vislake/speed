package sharing

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"

	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/sharing/api"
)

// jsonContentType is the Content-Type every structured-error response below
// writes, matching notes', config's, org's and storage's own handler
// constant of the same name.
const jsonContentType = "application/json; charset=utf-8"

// octetStreamContentType is the fallback Content-Type a granted access
// writes when the resolved ResourceContent carries no MIME at all.
const octetStreamContentType = "application/octet-stream"

// HeaderSharePassword is the header a caller presents a share's access
// password through -- see api/openapi.yaml's own parameter description for
// why this is a header, never a second query parameter. Exported so a
// caller of this module's public route (a browser-facing password page, a
// test, an SDK) has one source of truth for the header's literal name.
//
// #nosec G101 -- this is a header NAME, not a credential value: gosec's
// hardcoded-credential heuristic matches on the substring "password"
// alone, the same false positive authn's identical header/field-name
// constants are already excepted from elsewhere in this codebase.
const HeaderSharePassword = "X-Sharing-Password"

// Handler serves sharing's one HTTP operation by implementing the
// spec-generated api.ServerInterface (api/sharing-server.gen.go,
// regenerated from this module's api/openapi.yaml by task api:gen -- the
// compile-time assertion at the bottom of this file is what makes "spec
// changed, handler not" a compile failure instead of a runtime surprise).
//
// Unlike every other module's Handler in this codebase, this one is NOT
// meant to run downstream of tenancy.Middleware's ordinary tenant
// resolution: the request it serves carries no tenant claim at all, by
// design (a genuinely unauthenticated visitor holds no access token), and
// Service.AccessPublic (service.go) is what resolves the tenant instead,
// from the token alone. A host MUST allowlist this route's exact
// (GET, PathAccess) pair with tenancy.WithAllowlist -- module.go's own
// Register doc comment repeats this obligation at the point a host would
// actually wire it.
//
// Handler performs no data access of its own beyond the two calls its
// contract requires: Service.AccessPublic (svc) for the share-access
// decision, and ResourceResolver.OpenResource (resolver, optionally nil)
// for the resource's bytes once access is granted.
type Handler struct {
	svc      *Service
	resolver ResourceResolver
	mux      *http.ServeMux
}

// NewHandler returns a Handler serving the module's public access route
// through svc, resolving a granted share's ResourceRef through resolver.
// resolver may be nil: a share whose access is granted then answers
// ErrResourceUnavailable rather than serving bytes it has no way to reach
// -- a host that mounts this route without wiring a resolver gets a route
// that always fails past the access-decision stage, never one that panics.
//
// Unlike every other module's handler in this codebase, this one cannot
// wire the mux with the bare api.HandlerFromMux: that helper installs
// oapi-codegen's default ErrorHandlerFunc (plain http.Error) for a request
// the spec-generated parameter binder itself rejects -- a missing or
// malformed token query parameter, a duplicated X-Sharing-Password header
// -- and that path returns before SharingAccessShare, the method that sets
// Cache-Control: no-store, ever runs. AGENTS.md's "Revocation and caching"
// section is explicit that EVERY response this route can produce must
// carry that header, so NewHandler instead calls api.HandlerWithOptions
// with a custom ErrorHandlerFunc (bindingErrorHandler below) that sets the
// header and writes the module's own SharingError envelope itself.
func NewHandler(svc *Service, resolver ResourceResolver) *Handler {
	h := &Handler{svc: svc, resolver: resolver}
	h.mux = http.NewServeMux()
	api.HandlerWithOptions(h, api.StdHTTPServerOptions{
		BaseRouter:       h.mux,
		ErrorHandlerFunc: bindingErrorHandler,
	})
	return h
}

// bindingErrorHandler is NewHandler's ErrorHandlerFunc: it runs in place of
// api.HandlerFromMux's default (a bare http.Error) whenever the
// spec-generated parameter binder rejects a request before Handler's own
// method is ever called. It sets Cache-Control: no-store first -- matching
// SharingAccessShare's own ordering discipline -- then answers the same
// SharingError JSON envelope every other refusal on this route produces,
// via ErrInvalidRequest wrapping the binder's own error as its cause.
func bindingErrorHandler(w http.ResponseWriter, _ *http.Request, err error) {
	w.Header().Set("Cache-Control", "no-store")
	writeError(w, ErrInvalidRequest.WithCause(err))
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// SharingAccessShare implements api.ServerInterface: GET
// /api/v1/sharing/access. See api/openapi.yaml's own operation description
// for the full outward contract; this method is deliberately thin --
// extract the request's two inputs, delegate the access decision to
// Service.AccessPublic in full, then delegate the resource read to
// resolver -- because every rule that actually matters (outward-identical
// refusals, the constant-time password check, revocation with no caching)
// already lives in Service and must not be duplicated here.
//
// Cache-Control: no-store is set FIRST, before any other work, so every
// response this method can possibly produce -- including one that panics
// partway through resolving the resource, which recovers to nothing more
// specific than a connection reset -- carries it. This is the one
// behavior AGENTS.md's "Revocation and caching" section names as a binding
// obligation on whichever round adds this route.
func (h *Handler) SharingAccessShare(w http.ResponseWriter, r *http.Request, params api.SharingAccessShareParams) {
	w.Header().Set("Cache-Control", "no-store")
	ctx := r.Context()

	share, err := h.svc.AccessPublic(ctx, params.Token, AccessParams{
		Password:  params.XSharingPassword,
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
		Referrer:  r.Referer(),
	})
	if err != nil {
		writeError(w, err)
		return
	}

	if h.resolver == nil {
		writeError(w, ErrResourceUnavailable)
		return
	}
	// The tenant Service.AccessPublic resolved (from the token alone, per
	// its own doc comment) lives only inside that call -- it never mutated
	// r.Context(). Rebuild it here from the granted share's own row
	// (share.GetTenantID(), the tenant_id column the tenant-scope plugin
	// populated when the row was created) so ResourceResolver
	// implementations that themselves require ctx to carry a tenant (like
	// go/storage's ObjectService.OpenContent) see the correct one, exactly
	// as they would for an authenticated caller.
	resourceCtx := pkgcore.WithTenant(ctx, share.GetTenantID())
	content, err := h.resolver.OpenResource(resourceCtx, share.ResourceRef)
	if err != nil {
		writeError(w, ErrResourceUnavailable.WithCause(err))
		return
	}
	defer func() {
		if closeErr := content.Body.Close(); closeErr != nil {
			observability.FromContext(ctx).Warn("share resource stream close failed",
				"share_id", share.ID, "error", closeErr)
		}
	}()

	mime := content.MIME
	if mime == "" {
		mime = octetStreamContentType
	}
	w.Header().Set("Content-Type", mime)
	if content.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(content.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, copyErr := io.Copy(w, content.Body); copyErr != nil {
		observability.FromContext(ctx).Warn("share resource stream failed",
			"share_id", share.ID, "error", copyErr)
	}
}

// clientIP reports the direct connection's address from r.RemoteAddr, with
// the port stripped. This module deliberately does NOT honor
// X-Forwarded-For or any other caller-supplied header here: an
// unauthenticated route is exactly the shape a caller can freely spoof
// such a header against, and this IP feeds both the access log
// (AccessLogEntry.IP) and this module's own rate-limit key
// (ratelimit.go's checkAccessRateLimit) -- trusting a spoofable header for
// either would let an attacker rotate around their own rate limit for
// free. A host that terminates TLS behind a trusted reverse proxy and
// wants the proxy's forwarded address instead is expected to normalize
// r.RemoteAddr itself, ahead of this handler, the same way any other
// trusted-proxy concern in this codebase is a host-side decision.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// writeJSON writes v to w as JSON with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes err to w as a JSON {code, params} body -- the
// spec-generated api.SharingError, the same structured-error envelope
// notes', config's, org's and storage's own writeError produce. An err
// that is not an *apperr.Error -- meaning something below this handler did
// not classify it -- is folded into ErrInternal so a caller never sees raw
// Go error text.
func writeError(w http.ResponseWriter, err error) {
	appErr, ok := apperr.As(err)
	if !ok {
		appErr = ErrInternal
	}
	envelope := api.SharingError{Code: appErr.Code}
	if appErr.Params != nil {
		envelope.Params = &appErr.Params
	}
	writeJSON(w, appErr.Status, envelope)
}

// compile-time check that *Handler implements the api.ServerInterface
// generated from this module's api/openapi.yaml -- the enforcement half of
// the spec-first flow (docs/internal/21-api-contract.md): add an operation
// to the fragment, regenerate, and this assertion stops compiling until
// Handler implements it.
var _ api.ServerInterface = (*Handler)(nil)
