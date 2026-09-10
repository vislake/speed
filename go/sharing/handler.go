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
	"github.com/vislake/speed/go/pkgcore/httpapi"

	"github.com/vislake/speed/go/sharing/api"
)

// jsonContentType is the Content-Type every response below writes, the
// same JSON type the coded refusals carry (see pkgcore/httpapi).
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

// Handler serves every one of sharing's HTTP operations -- the public
// access route (PathAccess) and the five owner-facing operations
// (PathShares) -- by implementing the spec-generated api.ServerInterface
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
// Cache-Control: no-store, ever runs. EVERY response the access route can
// produce must carry that header -- revocation and a spent view must reach
// the viewer's next fetch, never a cached copy -- so NewHandler instead
// calls api.HandlerWithOptions
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
// resolver and the view recording to Service's reserve/confirm/refund
// trio -- because every rule that actually matters (outward-identical
// refusals, the constant-time password check, revocation with no caching,
// and the rule that a view is consumed only when the content was actually
// delivered) already lives in Service and must not be duplicated here.
//
// # The serve flow: authorize, reserve, deliver, then confirm
//
// A MaxViews-limited share's views are the budget this route exists to
// spend, and a view is spent ONLY once the share's content has actually
// reached the viewer -- the reserve/confirm/refund shape go/billing's
// credits ledger and go/storage's transfer lifecycle establish in this
// codebase. The two directions it must hold in both ways: bytes not
// delivered never spend the view, and bytes delivered are never given
// away unspent (service.go's viewReservationTimeout doc comment carries
// the full reasoning):
//
//  1. authorizePublicAccess runs the prelude's per-IP rate-limit check
//     and token-to-tenant lookup, then every one of Access's own refusal
//     checks (the tenant-scoped token lookup, the constant-time password
//     check, current liveness), and records NOTHING on success -- no
//     view, no reservation, no granted log row. Every refusal of a token
//     that resolved to a share is settled as one denied row and one
//     denied event, exactly as Service.Access settles them; a refusal
//     answered before any share is on hand -- an over-budget caller's
//     prelude 429 or an unrecognized token's ErrNotAccessible -- settles
//     nothing, no row existing to attribute an entry to.
//  2. A MaxViews-limited share's view is then RESERVED (reserveAccessView)
//     before any delivery can begin: the share's ceiling already accounts
//     for the serve in flight, so a concurrent second fetch is refused up
//     front -- the identical outward answer a share that had simply
//     exhausted its views answers with -- instead of being delivered and
//     only then losing a settlement race. A lost reservation (the share
//     was revoked, expired, exhausted or taken by a concurrent serve in
//     the interim) is settled as denied and answered; nothing has been
//     delivered either way.
//  3. The content is resolved (resolver) and streamed to the response. An
//     unwired resolver, a resolver failure, or a stream that dies partway
//     through REFUNDS the reservation (refundAccessView -- no view
//     consumed, so the share keeps every view it had) and settles the
//     attempt as DENIED (settleAccessDenied): the visitor never got the
//     content, and the log says refused.
//  4. Only after the body has been fully copied is the reservation
//     resolved into a spent view: a limited share's serve is CONFIRMED
//     (confirmAccessView -- the reserved view becomes a counted view and
//     the granted log row commits in one guarded transaction), an
//     unlimited share's serve is settled granted exactly as it always was
//     (settleAccessGranted, no reservation ever standing for it). A
//     delivered serve is never refunded: if its confirm write fails, the
//     reservation keeps the view held in use -- spent -- rather than
//     returning it to the share (confirmAccessView's own doc comment has
//     the two-directions reasoning in full).
//
// Cache-Control: no-store is set FIRST, before any other work, so every
// response this method can possibly produce -- including one that panics
// partway through resolving the resource, which recovers to nothing more
// specific than a connection reset -- carries it. This is the route-level
// half of the module's no-caching rule: revocation and a spent view must
// reach the viewer's very next fetch, never a cached page (model.go's
// RevokedAt field comment).
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
	// context would cancel the very log write the module's mandatory
	// every-access-is-logged rule exists to guarantee, so the settles run on
	// a context that survives the request.
	settleCtx := context.WithoutCancel(ctx)

	// Step 2: reserve the view of a MaxViews-limited share BEFORE any
	// delivery can begin -- see the doc comment above for why the ceiling
	// must account for the serve in flight before its bytes leave the
	// server.
	limited := share.MaxViews != nil
	if limited {
		share, err = h.svc.reserveAccessView(settleCtx, share, p)
		if err != nil {
			// Either the in-use refusal (ErrNotAccessible, already settled
			// as denied) or a store failure on the reservation itself (its
			// denied settle attempted, the internal error surfaced) --
			// nothing has been delivered either way.
			writeError(w, err)
			return
		}
	}

	// refundAndSettleDenied releases a limited serve's reservation and
	// settles the failed serve as denied -- the no-resolver answer, the
	// resolver's failure, and the interrupted stream all land here. A
	// refund failure is logged (a reservation a refund cannot land is left
	// for the convergence owners viewReservationTimeout's own doc comment
	// names); the denied settle's failure is the caller's to answer or log,
	// exactly as the pre-reservation route treated the settle alone.
	refundAndSettleDenied := func() error {
		if limited {
			if refundErr := h.svc.refundAccessView(settleCtx, share); refundErr != nil {
				observability.FromContext(ctx).Warn("share reservation could not be refunded after a failed serve",
					"share_id", share.ID, "error", refundErr)
			}
		}
		return h.svc.settleAccessDenied(settleCtx, share, p)
	}

	if h.resolver == nil {
		// Authorized, but nothing can serve this share's content: settle the
		// attempt as denied -- the share keeps every view it had -- and
		// answer the distinct 502. A settle failure means the refusal left
		// no trail: answer the internal error instead, the same way
		// Service.Access refuses rather than answering when its own denied
		// row cannot be written.
		if settleErr := refundAndSettleDenied(); settleErr != nil {
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
		if settleErr := refundAndSettleDenied(); settleErr != nil {
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
		// made it out. Settle the interrupted delivery honestly: refund the
		// reservation (no view consumed, so a flaky first attempt never
		// spends a limited share's budget), then one denied row and one
		// denied event. A settle failure here can only be logged: there is
		// no response left to answer with.
		observability.FromContext(ctx).Warn("share resource stream failed",
			"share_id", share.ID, "error", copyErr)
		if settleErr := refundAndSettleDenied(); settleErr != nil {
			observability.FromContext(ctx).Error("share stream interruption could not be logged",
				"share_id", share.ID, "error", settleErr)
		}
		return
	}

	// Step 4: the full content reached the viewer -- only NOW is the view
	// consumed and the access logged granted (confirmAccessView on a
	// MaxViews-limited share, settleAccessGranted on an unlimited one --
	// both commit the view record and the granted log row in one
	// transaction). The response is already committed, so a settle failure
	// can only be logged.
	var settleErr error
	if limited {
		settleErr = h.svc.confirmAccessView(settleCtx, share, p)
	} else {
		settleErr = h.svc.settleAccessGranted(settleCtx, share, p)
	}
	if settleErr != nil {
		observability.FromContext(ctx).Error("share access delivery could not be settled",
			"share_id", share.ID, "error", settleErr)
	}
}

// decodeJSON decodes r's body into dst, writing ErrInvalidRequest and
// reporting false on any decode failure (see pkgcore/httpapi's DecodeJSON;
// the body carries no size bound). Only sharing_createShare
// (SharingCreateShare, the one owner-facing operation with a request body)
// calls it.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	return httpapi.DecodeJSON(w, r, 0, dst, ErrInvalidRequest)
}

// The sharing_listShares page-size bound, the same 1-200 window
// go/storage's own listing serves -- see ErrInvalidLimit.
const (
	minListLimit = 1
	maxListLimit = 200
)

// SharingListShares implements api.ServerInterface: GET
// /api/v1/sharing/shares. A thin translation of Service.List: up to limit
// shares (default 50) of the tenant tenancy.Middleware already resolved
// into the request context, newest first, or the page of older shares
// after the row named by beforeId when the cursor is present. An explicit
// limit outside the 1-200 bound answers sharing.invalid_limit before
// Service is reached, exactly as storage_listObjects' own handler checks
// its limit; a beforeId naming no share of the tenant answers
// sharing.share_not_found (Service.List's mapping), indistinguishable from
// a cursor that never existed. See PathShares' own doc comment for this
// route's gating contract -- Handler performs no authorization decision
// here at all; a host's own permission gate is what gets a request this
// far.
func (h *Handler) SharingListShares(w http.ResponseWriter, r *http.Request, params api.SharingListSharesParams) {
	ctx := r.Context()
	limit := defaultListPageSize
	if params.Limit != nil {
		limit = *params.Limit
		if limit < minListLimit || limit > maxListLimit {
			writeError(w, ErrInvalidLimit.
				WithParam("limit", limit).
				WithParam("min", minListLimit).
				WithParam("max", maxListLimit))
			return
		}
	}
	beforeID := ""
	if params.BeforeID != nil {
		beforeID = *params.BeforeID
	}
	shares, err := h.svc.List(ctx, limit, beforeID)
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
// (ratelimit.go's checkAccessIPLimit) -- trusting a spoofable header for
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

// writeError writes err to w as the coded error envelope (see
// pkgcore/httpapi): an *apperr.Error keeps its own code and status,
// anything else -- something below this handler did not classify it -- is
// folded into ErrInternal so a caller never sees raw Go error text either
// way.
func writeError(w http.ResponseWriter, err error) {
	httpapi.WriteError(w, err, ErrInternal)
}

// compile-time check that *Handler implements the api.ServerInterface
// generated from this module's api/openapi.yaml -- the enforcement half of
// the spec-first flow: add an operation to the fragment, regenerate, and
// this assertion stops compiling until Handler implements it.
var _ api.ServerInterface = (*Handler)(nil)
