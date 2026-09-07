package sharing

import (
	"context"
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

// Handler serves every one of sharing's HTTP operations -- the one public
// access route (round 2) and the five owner-facing operations (round 3, PathShares)
// -- by implementing the spec-generated api.ServerInterface
// (api/sharing-server.gen.go, regenerated from this module's
// api/openapi.yaml by task api:gen -- the compile-time assertion at the
// bottom of this file is what makes "spec changed, handler not" a compile
// failure instead of a runtime surprise). module.go's Register mounts this
// SAME Handler instance at both PathAccess and PathShares: the two families
// are gated in opposite ways by the host (see PathShares' own doc comment),
// but they are one Go type because oapi-codegen generates one
// ServerInterface per spec file, and a request's own literal path -- never
// which host-level mount matched it there first -- is what the internal mux
// (below) actually dispatches on.
//
// SharingAccessShare is NOT meant to run downstream of tenancy.Middleware's
// ordinary tenant resolution: the request it serves carries no tenant claim
// at all, by design (a genuinely unauthenticated visitor holds no access
// token), and Service.AccessPublic (service.go) is what resolves the tenant
// instead, from the token alone. A host MUST allowlist this route's exact
// (GET, PathAccess) pair with tenancy.WithAllowlist -- module.go's own
// Register doc comment repeats this obligation at the point a host would
// actually wire it. The five PathShares operations are the opposite shape:
// ordinary tenant-scoped reads and writes, expected to run downstream of
// tenancy.Middleware like every other module's fragment, with the tenant
// read from request context by the Service methods they call -- Handler
// performs no authorization decision for any of them, and no tenant
// resolution of its own either.
//
// Handler performs no data access of its own beyond the calls each
// operation's own contract requires -- Service's access-route trio for the
// share-access decision and its settlement (authorizePublicAccess,
// settleAccessDenied and settleAccessGranted, the authorize-deliver-consume
// flow SharingAccessShare's own doc comment describes) plus
// ResourceResolver.OpenResource (resolver, optionally nil) for the
// resource's bytes once access is authorized, and, for the five PathShares
// operations, a direct one-to-one call into
// Service.Create/Revoke/Get/ListAccessLog/List -- never a new business
// rule of its own.
type Handler struct {
	svc      *Service
	resolver ResourceResolver
	mux      *http.ServeMux
}

// NewHandler returns a Handler serving every operation this module's
// api/openapi.yaml declares through svc, resolving an authorized share's
// ResourceRef through resolver. resolver may be nil: a share whose access
// is authorized then answers ErrResourceUnavailable rather than serving
// bytes it has no way to reach -- a host that mounts the access route
// without wiring a resolver gets a route that always fails past the
// access-decision stage, never one that panics. Such a refused serve is
// settled as denied and consumes none of the share's views (SharingAccessShare's
// own doc comment). The five PathShares operations never touch
// resolver at all.
//
// Unlike every other module's handler in this codebase, this one cannot
// wire the mux with the bare api.HandlerFromMux: that helper installs
// oapi-codegen's default ErrorHandlerFunc (plain http.Error) for a request
// the spec-generated parameter binder itself rejects -- a missing or
// malformed token query parameter, a duplicated X-Sharing-Password header
// -- and that path returns before SharingAccessShare, the method that sets
// Cache-Control: no-store, ever runs. AGENTS.md's "Revocation and caching"
// section is explicit that EVERY response the access route can produce must
// carry that header, so NewHandler instead calls api.HandlerWithOptions
// with a custom ErrorHandlerFunc (bindingErrorHandler below) that sets the
// header and writes the module's own SharingError envelope itself. This
// same ErrorHandlerFunc also runs for a PathShares binding failure (a
// malformed shareId path segment, say) -- a harmless, if unnecessary,
// no-store header on an ordinary tenant-scoped response, not a correctness
// concern for that family.
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
// extract the request's inputs, delegate the access decision to
// Service.authorizePublicAccess, then delegate the resource read to
// resolver and the view recording to Service's settle methods -- because
// every rule that actually matters (outward-identical refusals, the
// constant-time password check, revocation with no caching, and the rule
// that a view is consumed only when the content was actually delivered)
// already lives in Service and must not be duplicated here.
//
// # The serve flow: authorize, deliver, then consume
//
// A limited share's view is the budget this route exists to spend, and it
// is spent ONLY once the share's content has actually reached the viewer:
//
//  1. authorizePublicAccess runs every refusal check (rate limit, token
//     lookup, the constant-time password check, current liveness) and
//     records NOTHING on success -- no view, no granted log row. Refusals
//     are settled as one denied row and one denied event, exactly as
//     Service.Access settles them.
//  2. The content is resolved (resolver) and streamed to the response. An
//     unwired resolver, a resolver failure, or a stream that dies partway
//     through settles the attempt as DENIED (settleAccessDenied): the
//     visitor never got the content, so the share keeps its views and the
//     log says refused -- the pre-fix flow recorded the view inside
//     Service.AccessPublic at the start of this method instead, so any of
//     those failure shapes permanently spent a MaxViews=1 share whose
//     content nobody ever saw (Service.authorizeAttempt's doc comment has
//     the full reasoning).
//  3. Only after the body has been fully copied does settleAccessGranted
//     consume the view and commit the granted log row, in one guarded
//     transaction.
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
	p := AccessParams{
		Password:  params.XSharingPassword,
		IP:        clientIP(r),
		UserAgent: r.UserAgent(),
		Referrer:  r.Referer(),
	}

	// Step 1: authorize WITHOUT consuming -- see the doc comment above for
	// why the view must not be recorded before the content exists.
	share, err := h.svc.authorizePublicAccess(ctx, params.Token, p)
	if err != nil {
		writeError(w, err)
		return
	}

	// The settles below can run after the response has been committed -- and
	// an interrupted stream is exactly the case where the client is already
	// gone, which is when net/http cancels the request context. A canceled
	// context would cancel the very log write rule 4 exists to guarantee, so
	// the settles run on a context that survives the request.
	settleCtx := context.WithoutCancel(ctx)

	if h.resolver == nil {
		// Authorized, but nothing can serve this share's content: settle the
		// attempt as denied -- the share keeps every view it had -- and
		// answer the distinct 502. A settle failure means the refusal left
		// no trail: answer the internal error rule 4 demands instead, the
		// same way Service.Access refuses rather than answering when its own
		// denied row cannot be written.
		if settleErr := h.svc.settleAccessDenied(settleCtx, share, p); settleErr != nil {
			writeError(w, settleErr)
			return
		}
		writeError(w, ErrResourceUnavailable)
		return
	}
	// The tenant authorizePublicAccess resolved (from the token alone, per
	// its own doc comment) lives only inside that call -- it never mutated
	// r.Context(). Rebuild it here from the share's own row
	// (share.GetTenantID(), the tenant_id column the tenant-scope plugin
	// populated when the row was created) so ResourceResolver
	// implementations that themselves require ctx to carry a tenant (like
	// go/storage's ObjectService.OpenContent) see the correct one, exactly
	// as they would for an authenticated caller.
	resourceCtx := pkgcore.WithTenant(ctx, share.GetTenantID())
	content, err := h.resolver.OpenResource(resourceCtx, share.ResourceRef)
	if err != nil {
		if settleErr := h.svc.settleAccessDenied(settleCtx, share, p); settleErr != nil {
			writeError(w, settleErr)
			return
		}
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
		// The response is already committed -- 200 and however many bytes
		// made it out. Settle the interrupted delivery honestly: one denied
		// row and one denied event, no view consumed, so a flaky first
		// attempt never spends a limited share's budget. A settle failure
		// here can only be logged: there is no response left to answer
		// with.
		observability.FromContext(ctx).Warn("share resource stream failed",
			"share_id", share.ID, "error", copyErr)
		if settleErr := h.svc.settleAccessDenied(settleCtx, share, p); settleErr != nil {
			observability.FromContext(ctx).Error("share stream interruption could not be logged",
				"share_id", share.ID, "error", settleErr)
		}
		return
	}

	// Step 3: the full content reached the viewer -- only NOW is the view
	// consumed and the access logged granted (settleAccessGranted commits
	// the guarded view record and the granted log row in one transaction).
	// The response is already committed, so a settle failure can only be
	// logged.
	if settleErr := h.svc.settleAccessGranted(settleCtx, share, p); settleErr != nil {
		observability.FromContext(ctx).Error("share access delivery could not be settled",
			"share_id", share.ID, "error", settleErr)
	}
}

// decodeJSON decodes r's body into dst, writing ErrInvalidRequest and
// reporting false on any decode failure. Only sharing_createShare
// (SharingCreateShare, the one owner-facing operation with a request body)
// calls it -- matching go/storage's identical decodeJSON helper.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, ErrInvalidRequest.WithCause(err))
		return false
	}
	return true
}

// SharingListShares implements api.ServerInterface: GET
// /api/v1/sharing/shares. A thin translation of Service.List: every share
// of the tenant tenancy.Middleware already resolved into the request
// context, newest first. See PathShares' own doc comment for this route's
// gating contract -- Handler performs no authorization decision here at
// all; a host's own permission gate is what gets a request this far.
func (h *Handler) SharingListShares(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	shares, err := h.svc.List(ctx)
	if err != nil {
		writeError(w, err)
		return
	}
	// An explicit make, so an empty tenant's list marshals as [] -- never
	// null -- matching go/storage's identical StorageListObjects discipline.
	items := make([]api.SharingShare, 0, len(shares))
	for i := range shares {
		items = append(items, toShareResponse(&shares[i]))
	}
	writeJSON(w, http.StatusOK, api.SharingListSharesResponse{Shares: &items})
}

// SharingCreateShare implements api.ServerInterface: POST
// /api/v1/sharing/shares. A thin translation of Service.Create -- every
// validation rule (resourceRef required, forever always refused, maxViews
// positive, the rate limit) is Service's own, never reimplemented here. The
// response carries the bearer token exactly once, per CreateResult's own
// doc comment; no other operation on this surface ever returns it again.
func (h *Handler) SharingCreateShare(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req api.SharingCreateShareRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	forever := req.Forever != nil && *req.Forever
	sensitive := req.Sensitive != nil && *req.Sensitive
	created, err := h.svc.Create(ctx, CreateParams{
		ResourceRef: req.ResourceRef,
		ExpiresAt:   req.ExpiresAt,
		Forever:     forever,
		MaxViews:    req.MaxViews,
		Password:    req.Password,
		Sensitive:   sensitive,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, api.SharingCreateShareResponse{
		Share: toShareResponse(created.Share),
		Token: created.Token,
	})
}

// SharingGetShare implements api.ServerInterface: GET
// /api/v1/sharing/shares/{shareId}. A thin translation of Service.Get.
func (h *Handler) SharingGetShare(w http.ResponseWriter, r *http.Request, shareID api.ShareID) {
	share, err := h.svc.Get(r.Context(), shareID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toShareResponse(share))
}

// SharingRevokeShare implements api.ServerInterface: POST
// /api/v1/sharing/shares/{shareId}/revoke. A thin translation of
// Service.Revoke, followed by a Service.Get of the same row: Revoke itself
// returns no value, and the spec promises the caller the share's own
// now-revoked state back (mirroring go/pki's pki_revokeSigningKey/
// pki_revokeCertificate response shape) -- composing two existing Service
// methods at the HTTP layer, never a new business rule. Idempotent exactly
// as Service.Revoke itself is: revoking an already-revoked share still
// answers 200 with its current, unchanged state.
func (h *Handler) SharingRevokeShare(w http.ResponseWriter, r *http.Request, shareID api.ShareID) {
	ctx := r.Context()
	if err := h.svc.Revoke(ctx, shareID); err != nil {
		writeError(w, err)
		return
	}
	share, err := h.svc.Get(ctx, shareID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toShareResponse(share))
}

// SharingListShareAccessLog implements api.ServerInterface: GET
// /api/v1/sharing/shares/{shareId}/access-log. A thin translation of
// Service.ListAccessLog -- which itself confirms the share exists in the
// caller's tenant before listing, so an unknown or foreign shareId answers
// sharing.share_not_found here exactly as it does for SharingGetShare, never
// an empty list.
func (h *Handler) SharingListShareAccessLog(w http.ResponseWriter, r *http.Request, shareID api.ShareID) {
	entries, err := h.svc.ListAccessLog(r.Context(), shareID)
	if err != nil {
		writeError(w, err)
		return
	}
	items := make([]api.SharingAccessLogEntry, 0, len(entries))
	for i := range entries {
		items = append(items, toAccessLogEntryResponse(&entries[i]))
	}
	writeJSON(w, http.StatusOK, api.SharingListAccessLogResponse{Entries: &items})
}

// toShareResponse converts s to its spec-generated JSON response type.
// Deliberately never carries TokenHash or PasswordHash -- only whether a
// password is set (PasswordProtected) -- since neither is meant to leave
// this module in any form; see SharingShare's own spec description.
func toShareResponse(s *Share) api.SharingShare {
	passwordProtected := s.PasswordHash != nil
	viewCount := s.ViewCount
	return api.SharingShare{
		ID:                &s.ID,
		ResourceRef:       &s.ResourceRef,
		ExpiresAt:         s.ExpiresAt,
		MaxViews:          s.MaxViews,
		ViewCount:         &viewCount,
		PasswordProtected: &passwordProtected,
		Sensitive:         &s.Sensitive,
		RevokedAt:         s.RevokedAt,
		CreatedAt:         &s.CreatedAt,
	}
}

// toAccessLogEntryResponse converts e to its spec-generated JSON response
// type.
func toAccessLogEntryResponse(e *AccessLogEntry) api.SharingAccessLogEntry {
	return api.SharingAccessLogEntry{
		ID:         &e.ID,
		OccurredAt: &e.OccurredAt,
		IP:         &e.IP,
		UserAgent:  &e.UserAgent,
		Referrer:   &e.Referrer,
		Outcome:    &e.Outcome,
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
