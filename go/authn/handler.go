package authn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/vislake/speed/go/authn/api"
	"github.com/vislake/speed/go/dbkit/audit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/httpapi"
)

// jsonContentType is the Content-Type every response below writes, the
// same JSON type the coded refusals carry (see pkgcore/httpapi).
const jsonContentType = "application/json; charset=utf-8"

// preAuthCookieName names the cookie a browser carries across an
// authorization round trip -- registration is never required to have one,
// only the social flow. Its value is opaque and never itself sent to a
// provider; SocialAuthorizeURL and SocialCallback only ever see its SHA-256
// digest, computed by BindingFromCookie (provider.go), which is what
// StateBinding.SessionBinding actually compares.
const preAuthCookieName = "authn_preauth"

// preAuthCookieBytes is the entropy of a freshly minted pre-auth cookie
// value, in bytes.
const preAuthCookieBytes = 32

// Handler serves authn's HTTP endpoints by implementing the spec-generated
// api.ServerInterface (see api/authn-server.gen.go, regenerated from this
// module's api/openapi.yaml by task api:gen -- the compile-time assertion at
// the bottom of this file is what makes "spec changed, handler not" a
// compile failure instead of a runtime surprise).
//
// Unlike notes' Handler, this one does NOT run downstream of
// tenancy.Middleware: most of its operations happen before any tenant is
// known at all (registration, sign-in, token refresh), and the ones that do
// act inside a tenant read it from the Principal's own TenantID claim, never
// from ctx via pkgcore.TenantFromContext. Every operation that needs an
// authenticated caller reads the Principal go/authn/middleware.go's
// Middleware already put in the request context -- see requirePrincipal --
// rather than checking tenancy at all. The ONE deliberate exception to
// "never from ctx": a caller may layer its decided tenant onto the ctx it
// hands to a downstream emit -- recordAudit for audit rows, principalCtx for
// the domain events a protected operation publishes -- and the emit reads
// it back. Business decisions never come from a ctx tenant; only the
// enrichment of an already-committed fact does (see Service.publish's own
// doc comment).
type Handler struct {
	svc          *Service
	bus          pkgcore.EventBus
	auditActions pkgcore.AuditActionRegistrar
	mux          *http.ServeMux

	// nilBusWarned makes recordAudit's nil-bus signal (below) one-shot: a
	// Handler built without a bus leaves every declared audit action
	// unrecorded for its whole life, and that permanent inoperative state
	// is announced once -- at the first audited operation, not per
	// operation -- because the NewHandler contract sanctions a deliberate
	// bus-less construction, and per-operation Error lines would drown the
	// very operator who chose it.
	nilBusWarned sync.Once
}

// NewHandler returns a Handler serving svc's operations. Its routing is
// registered by the generated api.HandlerFromMux helper, deriving this
// module's method+path patterns from api/openapi.yaml's own "paths:" keys --
// see notes' identical NewHandler doc comment for the mechanism.
//
// bus and auditActions back this Handler's own audit.Emit calls (see
// recordAudit) for the audit actions module.go's Register declares --
// notes.NewHandler's identical two parameters are the established
// convention this mirrors. bus may be nil, in which case every operation
// still succeeds but records no audit event, exactly as notes.Handler's
// own nil-bus case behaves; auditActions must not be nil when bus is
// non-nil, for the same reason notes.NewHandler's own doc comment gives
// (Emit needs a real AuditActionRegistrar to validate each action
// against). module.go's Register is the one real caller, sourcing both
// from the same *pkgcore.ComponentRegistry.
func NewHandler(svc *Service, bus pkgcore.EventBus, auditActions pkgcore.AuditActionRegistrar) *Handler {
	h := &Handler{svc: svc, bus: bus, auditActions: auditActions}
	h.mux = http.NewServeMux()
	api.HandlerFromMux(h, h.mux)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// requirePrincipal reads the authenticated Principal Middleware put in ctx,
// writing authn.authentication_required and reporting false when there is
// none. Every protected operation below calls this first; it is this
// module's per-route enforcement -- authn.Middleware authenticates
// optionally, and RequireAuthenticated is per-route, not global (see
// middleware.go) -- applied at the operation level because this one Handler
// serves both public and protected paths.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeAppError(w, ErrAuthenticationRequired)
		return Principal{}, false
	}
	return principal, true
}

// maxRequestBodyBytes bounds every body-reading operation's request body
// BEFORE it is decoded (see pkgcore/httpapi's DecodeJSON, which each
// operation calls with this bound): a body that exceeds the bound fails
// with ErrInvalidRequestBody (errors.go) exactly like malformed JSON, as
// soon as the read passes the limit rather than after the whole body has
// been buffered. Every operation's legitimate body is a handful of short
// fields whose longest single value this module's own rules bound at 128
// runes (a password or a display name; the worst-case JSON escaping of one
// rune is six bytes), so 64 KiB leaves an order of magnitude of headroom
// while making sure an unauthenticated register or login endpoint never
// buffers an attacker's arbitrarily large body before any validation has
// had a chance to refuse it.
const maxRequestBodyBytes = 1 << 16

// recordAudit emits an AuditEvent for one of the audit actions module.go's
// Register declares, through audit.Emit -- the declarative collection
// mechanism go/dbkit/audit documents, exactly as notes.Handler's own
// recordNoteCreatedAudit uses it (see that method's doc comment for the
// mechanism itself). h.bus nil means nothing is recorded and the operation
// that already succeeded (or failed, for a login-failure record) is
// unaffected either way, exactly as notes' handler treats it -- but unlike
// notes' handler, the inoperative state itself is announced: the first
// audited operation logs it once at Error (see nilBusWarned), so a Handler
// a host wired without a bus is never silently deaf for the rest of its
// life.
//
// tenantID is the tenant the recorded action happened in: the acting
// Principal's own TenantID claim wherever one exists, empty for a
// platform-level or pre-auth action. audit.Emit reads the tenant from ctx
// via pkgcore.TenantFromContext, but this Handler's routes are deliberately
// NOT downstream of tenancy.Middleware (see the Handler doc comment), so
// there is no ambient tenant to read -- every call site below names one
// itself. The argument is authoritative: it is layered onto ctx
// unconditionally, so an empty tenantID stamps a row with NO tenant
// (pkgcore.WithTenant stores an empty id, which the readers never report)
// rather than inheriting whatever a non-standard composition might have
// left in ctx. Audit rows are platform data whose tenant_id column is not
// enforced, but the tenant-scoped read paths
// (audit.Repository.ListByTenant, compliance.AuditQuery) filter on it, so
// a row must carry exactly the tenant its call site decided on, never one
// that leaked in by accident.
//
// Two shapes of call site pass "": a PRE-AUTH action whose caller was
// anonymous (a registration nobody authenticated, a failed sign-in), where
// the tenant_id a request merely asserts is not an attestation -- an
// unauthenticated caller must not be able to stamp rows into a tenant's
// ledger by naming it -- and the social-callback BIND, which is recorded at
// an unauthenticated callback (the flow authenticates by the single-use
// state the signed-in authorize step minted, and no session is started), so
// no tenant is attested at recording time. Registration records more when
// more was attested: an authenticated register caller's own Principal claim
// is the one tenant the request actually attested, and AuthnRegister's call
// site layers it onto the row (the account itself is created in no tenant,
// and the row's tenant answers "a member of which tenant initiated this
// creation", never "which tenant the account was created in"). Every
// protected operation below -- logout,
// identity unbind, MFA changes, tenant switch, session revoke -- carries
// its principal's tenant, and every sign-in success this Handler records
// (password, SMS, social) carries the tenant the new session resolved.
// Enterprise SSO sign-ins have no recordAudit site: the SSO service has no
// mounted HTTP surface, and AuditActionSSOConfigure's own emission --
// oidc.go's emitConfigSavedAudit -- records configuration writes from the
// service layer, not sign-ins. AuthnSwitchTenant
// records the PRINCIPAL'S tenant (the tenant the session acted in before
// the switch): the row answers "a member of which tenant performed this
// action", and the switch's own destination is the request's business,
// visible in the returned pair and in EventTenantSwitched's
// FromTenantID/ToTenantID payload.
//
// Unlike notes' own call site, this sets the acting Actor explicitly on
// ctx before calling Emit (see pkgcore.WithActor) rather than relying on
// one already present: no middleware in this chain populates
// pkgcore.Actor today, and for login/register in particular there could
// be nothing upstream TO populate it from -- identity is only established
// by the very call this method is recording the outcome of. actorID empty
// means "no identity is known for this record" (an unmatched login
// attempt, for instance), in which case ctx is left exactly as given and
// Emit falls back to its own "no actor set" zero value.
//
// When an actor IS known, its display name is resolved from the users
// table at record time (h.svc.Users().FindByID) and carried on the Actor,
// so the audit record stays readable after the account is renamed or
// deleted -- the purpose pkgcore.Actor.DisplayName documents for itself.
// The resolution is deliberately inside this one
// funnel rather than at its call sites: every site's actor is an authn
// user id it knows only as an id (a Principal carries no display name,
// see token.go), and the users table is the single honest source for the
// label. A lookup failure is Warn-logged and the record proceeds
// id-only -- the id remains the authoritative attribution, and a
// best-effort label failure must not turn an already-committed
// operation's audit record into a lost one, exactly like an Emit failure
// below.
//
// A failure is logged, not returned, for the identical reason
// recordNoteCreatedAudit's own doc comment gives: the underlying
// operation has already been committed and answered to the caller, so an
// audit-write failure must not turn that answer into something else.
func (h *Handler) recordAudit(ctx context.Context, tenantID pkgcore.TenantID, actorID, action string, resource audit.Resource, result audit.Result) {
	if h.bus == nil {
		// The PERMANENT no-bus failure must not be quieter than the
		// transient one below (an Emit failure logs at Error): a Handler
		// built without a bus never records any of the declared actions,
		// for its whole life, and that state is announced once at Error --
		// see nilBusWarned's own doc comment for why once rather than per
		// operation.
		h.nilBusWarned.Do(func() {
			obs.FromContext(ctx).Error("authn audit recording is inoperative: Handler was built with no event bus, so no audit action this module declares will ever be recorded by it")
		})
		return
	}
	if actorID != "" {
		actor := pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: actorID}
		if user, err := h.svc.Users().FindByID(ctx, actorID); err != nil {
			obs.FromContext(ctx).Warn("authn could not resolve the audit actor's display name; recording id-only",
				"user_id", actorID, "error", err)
		} else {
			actor.DisplayName = user.DisplayName
		}
		ctx = pkgcore.WithActor(ctx, actor)
	}
	ctx = pkgcore.WithTenant(ctx, tenantID)
	if err := audit.Emit(ctx, h.bus, h.auditActions, audit.Input{
		Action:   action,
		Resource: resource,
		Result:   result,
	}); err != nil {
		obs.FromContext(ctx).Error("authn audit event emit failed",
			"action", action, "resource_type", resource.Type, "resource_id", resource.ID, "error", err)
	}
}

// principalCtx returns r's context carrying the tenant the acting principal
// is in, layered unconditionally -- an empty principal TenantID stores a
// value the readers never report, so a principal without a tenant attests
// none. Service.publish reads the layered tenant back so the domain events a
// protected operation announces (identity unbound, MFA enrolled, recovery
// codes regenerated) carry the same tenant as the operation's own audit row,
// which the service layer cannot know: its methods receive the acting
// userID, and the user's tenants are not the tenant the request acted in.
// The call sites are exactly the protected operations whose Service methods
// publish one of those events.
//
// A pre-authentication path deliberately does NOT call this: those
// operations' events must never carry a tenant at all, and that is enforced
// at the publish sites themselves (Service.publishTenantless) rather than
// trusted to the absence of this layering -- a host composition may have
// resolved the caller's bearer even on an allowlisted pre-auth route, so
// the raw request context is not guaranteed tenant-free (see
// Service.publishTenantless's own doc comment).
func principalCtx(ctx context.Context, principal Principal) context.Context {
	return pkgcore.WithTenant(ctx, principal.TenantID)
}

// auditFailureReason extracts a short failure reason for an audit
// Result.FailureReason from err -- its apperr.Error.Code, which every
// error a Service method returns here already is. A non-*apperr.Error
// (should not happen; writeAppError itself falls back the same way for
// exactly this case) reports a generic fallback rather than leaving the
// field empty or embedding err's own free-text message, which could carry
// something this module must not put in an audit trail unredacted.
func auditFailureReason(err error) string {
	if appErr, ok := apperr.As(err); ok {
		return appErr.Code
	}
	return "unknown"
}

// headerXForwardedFor is the forwarding header this module reads for a
// request whose direct peer is a declared trusted proxy (see clientIP and
// WithTrustedProxies): the chain of proxies a request passed through that
// generic reverse proxies append to. It is read under the trusted-peer
// gate ALONE, with no per-header host declaration, because the chain
// carries its own protection: a proxy appends the peer it saw, so the
// right-to-left walk strips entries the declared proxies themselves
// appended and never trusts anything a client wrote -- see
// xForwardedForClientIP.
//
// headerFlyClientIP is the internal alias of the VendorClientIPHeader
// constant of the same name (module.go): a single-hop vendor header, read
// ONLY when the host opted into it with WithVendorClientIPHeaders -- the
// reason it cannot share X-Forwarded-For's gate-only treatment is that
// option's own doc comment.
const (
	headerFlyClientIP   = string(VendorClientIPHeaderFlyClientIP)
	headerXForwardedFor = "X-Forwarded-For"
)

// clientIP extracts the requesting client's address from r, for the rate
// limiter and the login/session records.
//
// The address a deployment behind a reverse proxy wants recorded is the
// client's, not the proxy's: the platform-injected forwarding headers
// (headerXForwardedFor generally, headerFlyClientIP on Fly.io) carry it,
// but a header is only what it claims to be when the proxy -- not an
// arbitrary client -- wrote it. So the derivation is gated on the
// host-declared trusted-proxy list Service carries (WithTrustedProxies):
// the forwarding headers are read ONLY when the request's direct
// connection address (RemoteAddr) is one of the declared proxies, and
// every other request records its direct connection address. A direct
// client can never mint a recorded address by setting a header: it is not
// a declared proxy, so its headers are never read.
//
// Two different header kinds are read under that gate, on two different
// footings:
//
//   - headerXForwardedFor needs no per-header declaration, because the
//     chain protects itself. A proxy appends the peer it saw to
//     X-Forwarded-For, so the header may carry entries the client itself
//     supplied in front of the proxy's own; the chain is therefore walked
//     from the right -- the end the trusted proxies appended -- stripping
//     the entries that name declared proxies until the first entry that
//     names no declared proxy, the address the leftmost trusted proxy
//     actually saw, is found. An entry that does not parse as an IP
//     address (an empty slot, a hostname) ends the walk with no answer:
//     the chain is not what this deployment's proxies write, and
//     recording the peer is the honest result rather than an address
//     guessed past a malformed entry.
//
//   - a single-hop vendor header (VendorClientIPHeader, headerFlyClientIP
//     among them) is read ONLY when the host opted into that specific
//     header with WithVendorClientIPHeaders, and even then only when the
//     chain walk above produced no answer. The peer gate alone can never
//     authorize such a header: "the request came from a declared proxy"
//     cannot distinguish Fly's proxy -- which overwrites Fly-Client-IP on
//     every request -- from a generic reverse proxy that forwards a
//     client-chosen Fly-Client-IP verbatim, because both look identical
//     to this process; only the host knows which topology it runs, and
//     the opt-in is its declaration. Ordering the protected chain walk
//     first guarantees the unprotected single-hop read can never
//     short-circuit it.
func (h *Handler) clientIP(r *http.Request) string {
	peer := remoteAddrHost(r.RemoteAddr)
	if len(h.svc.trustedProxies) == 0 || !peerWithinAny(peer, h.svc.trustedProxies) {
		return peer
	}
	if client, ok := xForwardedForClientIP(r.Header.Get(headerXForwardedFor), h.svc.trustedProxies); ok {
		return client
	}
	for _, hdr := range h.svc.vendorClientIPHeaders {
		if client, ok := singleHopClientIP(r.Header.Get(string(hdr))); ok {
			return client
		}
	}
	return peer
}

// remoteAddrHost extracts the host part of a RemoteAddr -- "host:port" or
// a bracketed "[::1]:port". net.SplitHostPort does the splitting, and its
// refusal of a BARE address -- one carrying no port at all -- is exactly
// why it is used: a manual last-colon split of a bare IPv6 like "::1"
// would take everything after its first colon as the port and come back
// with the garbage ":".
func remoteAddrHost(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		// SplitHostPort returns the host unbracketed: "[::1]:443" gives
		// "::1" directly.
		return host
	}
	// Not "host:port": the address is bare. Strip the brackets a bare
	// address could still carry ("[::1]"), and return the rest as-is.
	return strings.TrimSuffix(strings.TrimPrefix(remoteAddr, "["), "]")
}

// peerWithinAny reports whether peer is an IP address contained in one of
// nets -- the gate that decides whether a request's forwarding headers may
// be read at all. An unparseable peer (a hostname, an empty RemoteAddr) is
// never within a proxy range, so its request stays on the connection-
// address path.
func peerWithinAny(peer string, nets []netip.Prefix) bool {
	addr, err := netip.ParseAddr(peer)
	if err != nil {
		return false
	}
	return peerWithinAddr(addr.Unmap(), nets)
}

// singleHopClientIP validates and canonicalizes a vendor client-address
// header value -- the wire value of a VendorClientIPHeader the host opted
// into (WithVendorClientIPHeaders), headerFlyClientIP among them. Only
// the first comma-separated entry is read: the vendors these headers
// belong to set a single value, overwriting whatever the client sent,
// so a chain would mean the header is not one that vendor's proxy wrote.
// An entry that does not parse as an IP address reports not-ok, and the
// caller falls through to the connection address rather than recording a
// value no proxy wrote.
func singleHopClientIP(value string) (string, bool) {
	entry := value
	if idx := strings.IndexByte(entry, ','); idx != -1 {
		entry = entry[:idx]
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(entry))
	if err != nil {
		return "", false
	}
	return addr.Unmap().String(), true
}

// xForwardedForClientIP recovers the client address from an
// headerXForwardedFor chain, walking it from the right and stripping the
// entries that name declared proxies -- see clientIP's doc comment for
// the full reasoning and fail-closed rule.
func xForwardedForClientIP(value string, trusted []netip.Prefix) (string, bool) {
	entries := strings.Split(value, ",")
	for i := len(entries) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(entries[i]))
		if err != nil {
			// A proxy appends only bare peer addresses, so an entry
			// that does not parse means the chain is not one this
			// deployment's proxies wrote -- report no answer rather
			// than guessing past it.
			return "", false
		}
		addr = addr.Unmap()
		if peerWithinAddr(addr, trusted) {
			continue
		}
		return addr.String(), true
	}
	// Every entry names a declared proxy: the client is itself on the
	// trusted side of the chain, invisible to this walk. The caller
	// records the direct connection address, the only untrusted fact it
	// holds.
	return "", false
}

// peerWithinAddr is peerWithinAny's parsed-address half, shared with
// xForwardedForClientIP's chain walk.
func peerWithinAddr(addr netip.Addr, nets []netip.Prefix) bool {
	for _, net := range nets {
		if net.Contains(addr) {
			return true
		}
	}
	return false
}

// deref returns *p, or "" for a nil p -- every optional string field the
// generated request types carry.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// str returns a pointer to s, or nil for an empty s -- the inverse of deref,
// used when building a response so an empty field is genuinely absent on the
// wire (see AuthnTokenPair.RefreshToken's doc comment in openapi.yaml) rather
// than present as an empty string.
func str(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ensurePreAuthCookie returns the value of the pre-authentication cookie a
// browser carries across a social authorization round trip, minting and
// setting a fresh one when none is present yet.
//
// The cookie's own value never leaves this server: SocialAuthorizeURL and
// SocialCallback only ever see BindingFromCookie(value), its SHA-256 digest,
// which is what a forged or replayed callback cannot reproduce without
// having read this exact cookie from this exact browser.
//
// The Secure attribute is set when the request arrived over TLS (r.TLS) OR
// the host assembled the Service WithSecureCookies(true): r.TLS is nil for
// every request in the most common production topology -- TLS terminated at
// a reverse proxy, with this process only ever seeing plaintext HTTP on its
// own listener -- so the host, who knows its own topology, forces it through
// that option rather than this handler trusting any client-supplied signal
// (the same reasoning clientIP's doc comment gives for reading forwarding
// headers only from a request whose peer is a declared trusted proxy, never
// from a client that can set its own). A host not actually serving over
// HTTPS anywhere must never pass it.
func (h *Handler) ensurePreAuthCookie(w http.ResponseWriter, r *http.Request) (string, error) {
	if cookie, err := r.Cookie(preAuthCookieName); err == nil && cookie.Value != "" {
		return cookie.Value, nil
	}

	raw := make([]byte, preAuthCookieBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", apperr.Internal("authn.internal_error").WithCause(err)
	}
	value := hex.EncodeToString(raw)
	http.SetCookie(w, &http.Cookie{
		Name:  preAuthCookieName,
		Value: value,
		Path:  "/api/v1/authn/social",
		// MaxAge tracks the state store's TTL -- the lifetime the state
		// record minted in this very request was issued with (Service's
		// configured cfg.oauthStateTTL, DefaultOAuthStateTTL when no
		// option overrode it) -- never the package default in a vacuum: a
		// host that configured a longer TTL for slow identity providers
		// must not get a cookie that dies before its state, stranding the
		// callback without its binding. Rounded UP to a whole second --
		// the finest a Max-Age can express -- so the cookie is never
		// shorter-lived than the state it accompanies.
		MaxAge:   int((h.svc.states.ttl + time.Second - 1) / time.Second),
		HttpOnly: true,
		Secure:   h.svc.secureCookies || r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	return value, nil
}

// readPreAuthCookie returns the pre-authentication cookie's value, or "" when
// none is present -- the callback side of ensurePreAuthCookie's round trip,
// which never mints a fresh cookie of its own: a callback with no cookie at
// all cannot possibly have originated from this server's own authorize
// step, and is refused with ErrOAuthStateInvalid exactly like a callback
// whose state cannot be found in the state store.
func readPreAuthCookie(r *http.Request) string {
	cookie, err := r.Cookie(preAuthCookieName)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// AuthnRegister implements api.ServerInterface.
func (h *Handler) AuthnRegister(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req api.AuthnRegisterRequest
	if !httpapi.DecodeJSON(w, r, maxRequestBodyBytes, &req, ErrInvalidRequestBody) {
		return
	}

	user, err := h.svc.Register(ctx, RegisterInput{
		Email:       deref(req.Email),
		Phone:       deref(req.Phone),
		Password:    req.Password,
		DisplayName: deref(req.DisplayName),
		Locale:      deref(req.Locale),
		// The request's own language, resolved by the frontend's chain and
		// transported in this header, is the locale chain's tier behind the
		// body's declared value; the timezone is the browser's report from
		// the registration form. Both are the chain's first/second tiers
		// only -- Service.Register validates leniently and stores what
		// survives the chain (see registrationLocale/registrationTimeZone).
		AcceptLanguage: r.Header.Get("Accept-Language"),
		Timezone:       deref(req.Timezone),
		IP:             h.clientIP(r),
	})
	if err != nil {
		writeAppError(w, err)
		return
	}

	// Registration is pre-tenant by construction: the account is created in
	// no tenant, and the authn.user.created event announcing it is
	// published through Service.publishTenantless so it never carries one
	// whatever this request's context holds (see that method's doc comment
	// for why an inherited tenant would seat the account in the caller's
	// tenant instead of leaving the host's tenant-less provisioning to
	// create the registrant's own workspace). An authenticated caller is
	// neither refused nor acted for: the account is an independent one (the
	// multi-account-per-person shape), so the tenant their bearer attested
	// is ignored by everything registration does.
	//
	// The audit row is the one ledger surface that records what the request
	// itself attested: the acting Principal's TenantID claim when the
	// caller held one -- the row answers "a member of which tenant
	// initiated this account's creation", never "which tenant the account
	// was created in" (none was). An anonymous caller attests nothing and
	// the row stays tenant-less, recordAudit's pre-auth case.
	attestedTenant := pkgcore.TenantID("")
	if principal, ok := PrincipalFromContext(ctx); ok {
		attestedTenant = principal.TenantID
	}
	h.recordAudit(ctx, attestedTenant, user.ID, AuditActionUserRegister,
		audit.Resource{Type: "user", ID: user.ID},
		audit.Result{Success: true})
	obs.FromContext(ctx).Info("account registered", "user_id", user.ID)
	writeJSON(w, http.StatusCreated, toUserResponse(user))
}

// AuthnLoginWithPassword implements api.ServerInterface.
func (h *Handler) AuthnLoginWithPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req api.AuthnLoginWithPasswordRequest
	if !httpapi.DecodeJSON(w, r, maxRequestBodyBytes, &req, ErrInvalidRequestBody) {
		return
	}

	pair, err := h.svc.Login(ctx, LoginInput{
		Identifier: req.Identifier,
		Password:   req.Password,
		TenantID:   pkgcore.TenantID(deref(req.TenantID)),
		Device:     deref(req.Device),
		UserAgent:  r.UserAgent(),
		IP:         h.clientIP(r),
	})
	if err != nil {
		// A failed sign-in is a pre-auth event: no tenant is attested,
		// and the tenant_id this request's own body may name is a client
		// assertion, not one -- recordAudit's pre-auth case.
		h.recordAudit(ctx, "", "", AuditActionUserLogin,
			audit.Resource{Type: "user"},
			audit.Result{Success: false, FailureReason: auditFailureReason(err)})
		writeAppError(w, err)
		return
	}

	h.recordAudit(ctx, pair.Principal.TenantID, pair.Principal.UserID, AuditActionUserLogin,
		audit.Resource{Type: "user", ID: pair.Principal.UserID},
		audit.Result{Success: true})
	obs.FromContext(ctx).Info("password sign-in succeeded", "user_id", pair.Principal.UserID, "session_id", pair.Principal.SessionID)
	writeJSON(w, http.StatusOK, toTokenPairResponse(pair))
}

// AuthnRequestSMSCode implements api.ServerInterface.
func (h *Handler) AuthnRequestSMSCode(w http.ResponseWriter, r *http.Request) {
	var req api.AuthnRequestSMSCodeRequest
	if !httpapi.DecodeJSON(w, r, maxRequestBodyBytes, &req, ErrInvalidRequestBody) {
		return
	}

	if err := h.svc.RequestSMSCode(r.Context(), RequestSMSCodeInput{
		Phone:          req.Phone,
		AcceptLanguage: r.Header.Get("Accept-Language"),
		IP:             h.clientIP(r),
	}); err != nil {
		writeAppError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// AuthnLoginWithSMSCode implements api.ServerInterface.
func (h *Handler) AuthnLoginWithSMSCode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req api.AuthnLoginWithSMSCodeRequest
	if !httpapi.DecodeJSON(w, r, maxRequestBodyBytes, &req, ErrInvalidRequestBody) {
		return
	}

	pair, err := h.svc.LoginWithSMSCode(ctx, SMSLoginInput{
		Phone:     req.Phone,
		Code:      req.Code,
		TenantID:  pkgcore.TenantID(deref(req.TenantID)),
		Device:    deref(req.Device),
		UserAgent: r.UserAgent(),
		IP:        h.clientIP(r),
	})
	if err != nil {
		// Pre-auth event, exactly as the password leg above: no tenant
		// is attested at a failed sign-in.
		h.recordAudit(ctx, "", "", AuditActionUserLogin,
			audit.Resource{Type: "user"},
			audit.Result{Success: false, FailureReason: auditFailureReason(err)})
		writeAppError(w, err)
		return
	}

	h.recordAudit(ctx, pair.Principal.TenantID, pair.Principal.UserID, AuditActionUserLogin,
		audit.Resource{Type: "user", ID: pair.Principal.UserID},
		audit.Result{Success: true})
	obs.FromContext(ctx).Info("sms sign-in succeeded", "user_id", pair.Principal.UserID, "session_id", pair.Principal.SessionID)
	writeJSON(w, http.StatusOK, toTokenPairResponse(pair))
}

// AuthnRefreshToken implements api.ServerInterface.
func (h *Handler) AuthnRefreshToken(w http.ResponseWriter, r *http.Request) {
	var req api.AuthnRefreshTokenRequest
	if !httpapi.DecodeJSON(w, r, maxRequestBodyBytes, &req, ErrInvalidRequestBody) {
		return
	}

	pair, err := h.svc.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTokenPairResponse(pair))
}

// AuthnLogout implements api.ServerInterface.
func (h *Handler) AuthnLogout(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := h.svc.Logout(ctx, principal.SessionID); err != nil {
		writeAppError(w, err)
		return
	}
	h.recordAudit(ctx, principal.TenantID, principal.UserID, AuditActionSessionRevoke,
		audit.Resource{Type: "session", ID: principal.SessionID},
		audit.Result{Success: true})
	w.WriteHeader(http.StatusNoContent)
}

// AuthnGetMe implements api.ServerInterface.
func (h *Handler) AuthnGetMe(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toPrincipalResponse(principal))
}

// AuthnGetPreferences implements api.ServerInterface: the caller's own
// stored locale and timezone, the pair the settings surface reads before
// rendering its controls. Reading is per-principal by construction -- the
// account is the Principal's own user id, never a request field.
func (h *Handler) AuthnGetPreferences(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	prefs, err := h.svc.Preferences(r.Context(), principal.UserID)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPreferencesResponse(prefs))
}

// AuthnUpdatePreferences implements api.ServerInterface: the partial
// preference update (absent field = unchanged, empty string = cleared --
// see PreferencesPatch). Validation is the strict kind, so an unstorable
// value answers the coded 400 with nothing written, and the response
// echoes the pair as stored AFTER the update, which is what lets the
// settings surface confirm a clearing (the field comes back empty) rather
// than having to infer it.
func (h *Handler) AuthnUpdatePreferences(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req api.AuthnUpdatePreferencesRequest
	if !httpapi.DecodeJSON(w, r, maxRequestBodyBytes, &req, ErrInvalidRequestBody) {
		return
	}
	prefs, err := h.svc.UpdatePreferences(r.Context(), principal.UserID, PreferencesPatch{
		Locale:   req.Locale,
		Timezone: req.Timezone,
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toPreferencesResponse(prefs))
}

// AuthnSocialAuthorize implements api.ServerInterface.
//
// A caller with an authenticated Principal is binding a new identity to
// their own account rather than signing in -- see SocialAuthorizeInput's own
// doc comment. Calling it unauthenticated (no Authorization header at all)
// is the ordinary sign-in flow's first step.
func (h *Handler) AuthnSocialAuthorize(w http.ResponseWriter, r *http.Request, provider string, params api.AuthnSocialAuthorizeParams) {
	binding, err := h.ensurePreAuthCookie(w, r)
	if err != nil {
		writeAppError(w, err)
		return
	}

	linkUserID := ""
	if principal, ok := PrincipalFromContext(r.Context()); ok {
		linkUserID = principal.UserID
	}

	url, err := h.svc.SocialAuthorizeURL(r.Context(), SocialAuthorizeInput{
		Provider:       provider,
		RedirectURI:    params.RedirectURI,
		SessionBinding: BindingFromCookie(binding),
		LinkUserID:     linkUserID,
	})
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.AuthnSocialAuthorizeResponse{AuthorizeURL: &url})
}

// AuthnSocialCallback implements api.ServerInterface.
func (h *Handler) AuthnSocialCallback(w http.ResponseWriter, r *http.Request, provider string) {
	ctx := r.Context()

	var req api.AuthnSocialCallbackRequest
	if !httpapi.DecodeJSON(w, r, maxRequestBodyBytes, &req, ErrInvalidRequestBody) {
		return
	}

	cookie := readPreAuthCookie(r)
	if cookie == "" {
		// No pre-auth cookie means this callback did not originate from
		// this server's own AuthnSocialAuthorize step -- the same refusal
		// SocialCallback itself gives an unrecognized state, so this
		// early return does not weaken the error's meaning.
		writeAppError(w, ErrOAuthStateInvalid)
		return
	}

	result, err := h.svc.SocialCallback(ctx, SocialCallbackInput{
		Provider:       provider,
		Code:           req.Code,
		State:          req.State,
		SessionBinding: BindingFromCookie(cookie),
		TenantID:       pkgcore.TenantID(deref(req.TenantID)),
		UserAgent:      r.UserAgent(),
		IP:             h.clientIP(r),
	})
	if err != nil {
		writeAppError(w, err)
		return
	}

	// Bound and Tokens are mutually exclusive (SocialLoginResult's own doc
	// comment): Bound means an already-signed-in caller attached a new
	// identity, and audits as a bind; otherwise this call started a real
	// session (whether onto an existing or a newly provisioned account)
	// and audits as a login, matching AuthnLoginWithPassword/
	// AuthnLoginWithSMSCode's identical action above.
	if result.Bound {
		// A bind is recorded with no tenant: it completes at an
		// unauthenticated callback (the flow authenticates by the
		// single-use state the signed-in authorize step minted, and no
		// session is started), so the caller's tenant is not attested at
		// recording time -- recordAudit's account-level case.
		h.recordAudit(ctx, "", result.User.ID, AuditActionIdentityBind,
			audit.Resource{Type: "identity", ID: result.Identity.ID},
			audit.Result{Success: true})
	} else {
		// A social sign-in starts a real session, so the row carries the
		// tenant that session resolved -- exactly like the password and
		// SMS legs above.
		tenantID := pkgcore.TenantID("")
		if result.Tokens != nil {
			// SocialLoginResult's contract makes !Bound imply non-nil
			// Tokens; the guard keeps a future contract break from
			// panicking this already-committed sign-in.
			tenantID = result.Tokens.Principal.TenantID
		}
		h.recordAudit(ctx, tenantID, result.User.ID, AuditActionUserLogin,
			audit.Resource{Type: "user", ID: result.User.ID},
			audit.Result{Success: true})
	}
	obs.FromContext(ctx).Info("social callback completed", "provider", provider, "bound", result.Bound, "created", result.Created)
	writeJSON(w, http.StatusOK, toSocialLoginResponse(result))
}

// AuthnListIdentities implements api.ServerInterface.
func (h *Handler) AuthnListIdentities(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	identities, err := h.svc.ListIdentities(r.Context(), principal.UserID)
	if err != nil {
		writeAppError(w, err)
		return
	}
	items := make([]api.AuthnIdentity, 0, len(identities))
	for i := range identities {
		items = append(items, toIdentityResponse(&identities[i]))
	}
	writeJSON(w, http.StatusOK, api.AuthnListIdentitiesResponse{Identities: &items})
}

// AuthnUnbindIdentity implements api.ServerInterface.
func (h *Handler) AuthnUnbindIdentity(w http.ResponseWriter, r *http.Request, identityID string) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// principalCtx layers the acting tenant so the domain event this
	// protected operation announces carries the same tenant as its audit
	// row; see principalCtx's own doc comment.
	ctx := principalCtx(r.Context(), principal)
	if err := h.svc.UnbindIdentity(ctx, principal.UserID, identityID); err != nil {
		writeAppError(w, err)
		return
	}
	h.recordAudit(ctx, principal.TenantID, principal.UserID, AuditActionIdentityUnbind,
		audit.Resource{Type: "identity", ID: identityID},
		audit.Result{Success: true})
	w.WriteHeader(http.StatusNoContent)
}

// AuthnEnrollTOTP implements api.ServerInterface.
func (h *Handler) AuthnEnrollTOTP(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	// Service.EnrollTOTP itself decides whether principal.AMR needs to
	// carry a step-up: that decision depends on whether an ACTIVE factor
	// already exists to replace, which only the service can see without
	// an extra round trip. See its doc comment.
	result, err := h.svc.EnrollTOTP(r.Context(), principal)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.AuthnEnrollTOTPResponse{
		Secret:          &result.Secret,
		ProvisioningURI: &result.ProvisioningURI,
	})
}

// AuthnConfirmTOTP implements api.ServerInterface.
func (h *Handler) AuthnConfirmTOTP(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req api.AuthnConfirmTOTPRequest
	if !httpapi.DecodeJSON(w, r, maxRequestBodyBytes, &req, ErrInvalidRequestBody) {
		return
	}
	// principalCtx: see AuthnUnbindIdentity's own call site comment.
	ctx := principalCtx(r.Context(), principal)
	codes, err := h.svc.ConfirmTOTP(ctx, principal.UserID, req.Code)
	if err != nil {
		writeAppError(w, err)
		return
	}
	h.recordAudit(ctx, principal.TenantID, principal.UserID, AuditActionMFAEnroll,
		audit.Resource{Type: "mfa_factor", ID: principal.UserID},
		audit.Result{Success: true})
	writeJSON(w, http.StatusOK, api.AuthnRecoveryCodesResponse{RecoveryCodes: &codes})
}

// AuthnRegenerateRecoveryCodes implements api.ServerInterface.
//
// Unlike AuthnEnrollTOTP, this operation only ever acts on an ALREADY
// ACTIVE factor (RegenerateRecoveryCodes' own precondition), so there is no
// first-time-setup case to carve out: every call changes existing MFA
// settings, and RequireStepUp's unconditional gate applies directly,
// exactly as wired.
func (h *Handler) AuthnRegenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	RequireStepUp(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.requirePrincipal(w, r)
		if !ok {
			return
		}
		// principalCtx: see AuthnUnbindIdentity's own call site comment.
		ctx := principalCtx(r.Context(), principal)
		codes, err := h.svc.RegenerateRecoveryCodes(ctx, principal.UserID)
		if err != nil {
			writeAppError(w, err)
			return
		}
		h.recordAudit(ctx, principal.TenantID, principal.UserID, AuditActionMFARecoveryCodesRegenerate,
			audit.Resource{Type: "mfa_recovery_codes", ID: principal.UserID},
			audit.Result{Success: true})
		writeJSON(w, http.StatusOK, api.AuthnRecoveryCodesResponse{RecoveryCodes: &codes})
	})).ServeHTTP(w, r)
}

// AuthnVerifyStepUp implements api.ServerInterface.
func (h *Handler) AuthnVerifyStepUp(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req api.AuthnVerifyStepUpRequest
	if !httpapi.DecodeJSON(w, r, maxRequestBodyBytes, &req, ErrInvalidRequestBody) {
		return
	}
	pair, err := h.svc.VerifyStepUp(r.Context(), principal, req.Code, h.clientIP(r))
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toTokenPairResponse(pair))
}

// AuthnSwitchTenant implements api.ServerInterface.
func (h *Handler) AuthnSwitchTenant(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var req api.AuthnSwitchTenantRequest
	if !httpapi.DecodeJSON(w, r, maxRequestBodyBytes, &req, ErrInvalidRequestBody) {
		return
	}
	ctx := r.Context()
	pair, err := h.svc.SwitchTenant(ctx, principal, pkgcore.TenantID(req.TenantID))
	if err != nil {
		writeAppError(w, err)
		return
	}
	// The row is stamped with the principal's tenant -- the tenant the
	// session acted in before the switch -- per recordAudit's own doc
	// comment on this site; the destination tenant is the pair's business.
	h.recordAudit(ctx, principal.TenantID, principal.UserID, AuditActionTenantSwitch,
		audit.Resource{Type: "session", ID: principal.SessionID},
		audit.Result{Success: true})
	writeJSON(w, http.StatusOK, toTokenPairResponse(pair))
}

// AuthnListSessions implements api.ServerInterface.
func (h *Handler) AuthnListSessions(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	sessions, err := h.svc.ListSessions(r.Context(), principal.UserID)
	if err != nil {
		writeAppError(w, err)
		return
	}
	items := make([]api.AuthnSession, 0, len(sessions))
	for i := range sessions {
		items = append(items, toSessionResponse(&sessions[i], principal.SessionID))
	}
	writeJSON(w, http.StatusOK, api.AuthnListSessionsResponse{Sessions: &items})
}

// AuthnRevokeSession implements api.ServerInterface.
func (h *Handler) AuthnRevokeSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if err := h.svc.RevokeSession(ctx, principal.UserID, sessionID); err != nil {
		writeAppError(w, err)
		return
	}
	h.recordAudit(ctx, principal.TenantID, principal.UserID, AuditActionSessionRevoke,
		audit.Resource{Type: "session", ID: sessionID},
		audit.Result{Success: true})
	w.WriteHeader(http.StatusNoContent)
}

// AuthnRevokeOtherSessions implements api.ServerInterface.
func (h *Handler) AuthnRevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	revoked, err := h.svc.RevokeOtherSessions(ctx, principal.UserID, principal.SessionID)
	if err != nil {
		writeAppError(w, err)
		return
	}
	// One aggregate audit record for the whole bulk revoke, rather than
	// one per session: RevokeOtherSessions' own return value is already a
	// count, not the individual session ids, and Changes.After is exactly
	// where a value that does not fit Resource's {Type, ID, DisplayName}
	// shape belongs.
	h.recordAudit(ctx, principal.TenantID, principal.UserID, AuditActionSessionRevoke,
		audit.Resource{Type: "session", ID: principal.SessionID, DisplayName: "other sessions"},
		audit.Result{Success: true})
	writeJSON(w, http.StatusOK, api.AuthnRevokeOtherSessionsResponse{RevokedCount: &revoked})
}

// AuthnListLoginHistory implements api.ServerInterface.
func (h *Handler) AuthnListLoginHistory(w http.ResponseWriter, r *http.Request, params api.AuthnListLoginHistoryParams) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	limit := 0
	if params.Limit != nil {
		limit = *params.Limit
	}
	attempts, err := h.svc.ListLoginHistory(r.Context(), principal.UserID, limit)
	if err != nil {
		writeAppError(w, err)
		return
	}
	items := make([]api.AuthnLoginAttempt, 0, len(attempts))
	for i := range attempts {
		items = append(items, toLoginAttemptResponse(&attempts[i]))
	}
	writeJSON(w, http.StatusOK, api.AuthnListLoginHistoryResponse{Attempts: &items})
}

// toUserResponse converts user to its spec-generated JSON response type.
func toUserResponse(user *User) api.AuthnUser {
	createdAt := user.CreatedAt
	return api.AuthnUser{
		ID:            &user.ID,
		Email:         str(user.Email),
		Phone:         str(user.Phone),
		DisplayName:   &user.DisplayName,
		Locale:        str(user.Locale),
		Timezone:      str(user.Timezone),
		EmailVerified: &user.EmailVerified,
		PhoneVerified: &user.PhoneVerified,
		CreatedAt:     &createdAt,
	}
}

// toPreferencesResponse converts prefs to its spec-generated JSON response
// type, mapping the empty (not-chosen) value to an absent wire field
// through str, the same conversion every other response in this module
// uses: a client reads a missing field as "not chosen yet", which is
// exactly what the empty column means, and a cleared preference therefore
// comes back as the field's absence rather than as a distinguishable
// second spelling of the same state.
func toPreferencesResponse(prefs PreferencesInput) api.AuthnPreferences {
	return api.AuthnPreferences{
		Locale:   str(prefs.Locale),
		Timezone: str(prefs.Timezone),
	}
}

// toIdentityResponse converts identity to its spec-generated JSON response
// type. ExternalID is deliberately not exposed: it is a lookup key, not
// display data (see identity.go's UserIdentity doc comment), and the
// settings page this endpoint serves has no use for a provider's raw
// subject identifier.
func toIdentityResponse(identity *UserIdentity) api.AuthnIdentity {
	createdAt := identity.CreatedAt
	return api.AuthnIdentity{
		ID:          &identity.ID,
		Provider:    &identity.Provider,
		Email:       str(identity.Email),
		DisplayName: str(identity.DisplayName),
		AvatarURL:   str(identity.AvatarURL),
		CreatedAt:   &createdAt,
		LastLoginAt: identity.LastLoginAt,
	}
}

// toPrincipalResponse converts p to its spec-generated JSON response type.
func toPrincipalResponse(p Principal) api.AuthnPrincipal {
	tenantID := string(p.TenantID)
	amr := append([]string(nil), p.AMR...)
	return api.AuthnPrincipal{
		UserID:    &p.UserID,
		TenantID:  &tenantID,
		SessionID: &p.SessionID,
		Email:     str(p.Email),
		Amr:       &amr,
	}
}

// toTokenPairResponse converts pair to its spec-generated JSON response
// type. RefreshToken and RefreshExpiresAt stay nil (and thus absent on the
// wire) exactly when pair carries no new refresh token -- a tenant switch or
// a step-up, both of which reuse the caller's existing one; see
// AuthnTokenPair.RefreshToken's doc comment in openapi.yaml.
func toTokenPairResponse(pair *TokenPair) api.AuthnTokenPair {
	accessExpiresAt := pair.AccessExpiresAt
	resp := api.AuthnTokenPair{
		AccessToken:     &pair.AccessToken,
		AccessExpiresAt: &accessExpiresAt,
		RefreshToken:    str(pair.RefreshToken),
		Principal:       principalPtr(toPrincipalResponse(pair.Principal)),
	}
	if pair.RefreshToken != "" {
		refreshExpiresAt := pair.RefreshExpiresAt
		resp.RefreshExpiresAt = &refreshExpiresAt
	}
	return resp
}

// principalPtr returns a pointer to p, for the one field of AuthnTokenPair
// that is always present.
func principalPtr(p api.AuthnPrincipal) *api.AuthnPrincipal { return &p }

// toSocialLoginResponse converts result to its spec-generated JSON response
// type. Tokens stays nil (and thus absent on the wire) exactly when result
// itself carries none -- a binding flow by an already-signed-in caller,
// which starts no session; see SocialLoginResult.Tokens's own doc comment.
func toSocialLoginResponse(result *SocialLoginResult) api.AuthnSocialLoginResponse {
	user := toUserResponse(result.User)
	identity := toIdentityResponse(result.Identity)
	resp := api.AuthnSocialLoginResponse{
		User:       &user,
		Identity:   &identity,
		Created:    &result.Created,
		Bound:      &result.Bound,
		AutoLinked: &result.AutoLinked,
	}
	if result.Tokens != nil {
		tokens := toTokenPairResponse(result.Tokens)
		resp.Tokens = &tokens
	}
	return resp
}

// toSessionResponse converts session to its spec-generated JSON response
// type. IsCurrent is true exactly when session is the one currentSessionID
// (the calling Principal's own session id) names.
//
// RevokeReason is the projection exportRevokeReason produces from the
// stored reason, and only a revoked row carries it: an active session (or
// any row whose status is not revoked) has no reason to report, whatever
// its column holds. The fold happens HERE, at the API boundary -- the
// stored Session row keeps the real reason, which is what in-process
// readers (and the audit trail) see.
func toSessionResponse(session *Session, currentSessionID string) api.AuthnSession {
	createdAt := session.CreatedAt
	lastSeenAt := session.LastSeenAt
	expiresAt := session.ExpiresAt
	isCurrent := session.ID == currentSessionID
	amr := session.AMRList()
	var revokeReason *api.AuthnSessionRevokeReason
	if session.Status == SessionStatusRevoked {
		projected := api.AuthnSessionRevokeReason(exportRevokeReason(session.RevokeReason))
		revokeReason = &projected
	}
	return api.AuthnSession{
		ID:           &session.ID,
		Status:       &session.Status,
		Device:       str(session.Device),
		UserAgent:    str(session.UserAgent),
		IP:           str(session.IP),
		Amr:          &amr,
		CreatedAt:    &createdAt,
		LastSeenAt:   &lastSeenAt,
		ExpiresAt:    &expiresAt,
		RevokeReason: revokeReason,
		IsCurrent:    &isCurrent,
	}
}

// toLoginAttemptResponse converts attempt to its spec-generated JSON
// response type. FailureReason is included: unlike the API-response error
// this module returns for a failed sign-in itself (deliberately generic,
// per ErrInvalidCredentials's doc comment), this endpoint is the caller's
// OWN authenticated history of their own account, where the specific reason
// is exactly what a security-conscious owner wants to see.
func toLoginAttemptResponse(attempt *LoginAttempt) api.AuthnLoginAttempt {
	createdAt := attempt.CreatedAt
	return api.AuthnLoginAttempt{
		ID:            &attempt.ID,
		Method:        &attempt.Method,
		Result:        &attempt.Result,
		FailureReason: str(attempt.FailureReason),
		IP:            str(attempt.IP),
		UserAgent:     str(attempt.UserAgent),
		CreatedAt:     &createdAt,
	}
}

// writeJSON writes v as a JSON body with status.
//
// A structured error goes through writeAppError instead (middleware.go),
// which this Handler shares with Middleware and RequireAuthenticated so
// every authn error response -- from token verification, from
// RequireAuthenticated, and from every operation below -- has exactly one
// shape and exactly one place that decides what a Retry-After header is
// worth. Its {code, params} wire shape matches api.AuthnError's JSON tags
// exactly, even though pkgcore/httpapi.WriteError -- which writeAppError
// delegates the body to -- builds it from that package's own envelope
// type rather than the generated one.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// compile-time check that *Handler implements the api.ServerInterface
// generated from this module's api/openapi.yaml: add an operation to the
// fragment, regenerate, and this assertion stops compiling until Handler
// implements it.
var _ api.ServerInterface = (*Handler)(nil)
