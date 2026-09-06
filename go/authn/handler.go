package authn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/vislake/speed/go/authn/api"
	"github.com/vislake/speed/go/dbkit/audit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// jsonContentType is the Content-Type every response below writes, matching
// go/tenancy/middleware.go's own tenantErrorContentType constant and this
// module's own writeAppError in middleware.go.
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
// rather than checking tenancy at all.
type Handler struct {
	svc          *Service
	bus          pkgcore.EventBus
	auditActions pkgcore.AuditActionRegistrar
	mux          *http.ServeMux
}

// NewHandler returns a Handler serving svc's operations. Its routing is
// registered by the generated api.HandlerFromMux helper, deriving this
// module's method+path patterns from api/openapi.yaml's own "paths:" keys --
// see notes' identical NewHandler doc comment for the mechanism.
//
// bus and auditActions back this Handler's own audit.Emit calls (see
// recordAudit) for the 9 audit actions module.go's Register declares --
// notes.NewHandler's identical two parameters are the established
// convention this mirrors. bus may be nil, in which case every operation
// still succeeds but records no audit event, exactly as notes.Handler's
// own nil-bus case behaves; auditActions must not be nil when bus is
// non-nil, for the same reason notes.NewHandler's own doc comment gives
// (Emit needs a real AuditActionRegistrar to validate each action
// against). module.go's Register is the one real caller, sourcing both
// from the same *pkgcore.Registry.
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
// module's per-route enforcement (root CLAUDE.md's "authn.Middleware is
// optional-auth; RequireAuthenticated is per-route, not global" -- see
// middleware.go), applied at the operation level because this one Handler
// serves both public and protected paths.
func (h *Handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeAppError(w, ErrAuthenticationRequired)
		return Principal{}, false
	}
	return principal, true
}

// decodeJSON decodes r's body into v, translating a decode failure into
// ErrInvalidRequestBody (errors.go) -- the structured invalid-request-body
// error every operation below reports it as, now catalogued and
// bilingually rendered like every other error this module returns (see
// ErrInvalidRequestBody's own doc comment).
func decodeJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return ErrInvalidRequestBody.WithCause(err)
	}
	return nil
}

// recordAudit emits an AuditEvent for one of the 9 audit actions module.go's
// Register declares, through audit.Emit -- the declarative collection
// mechanism go/dbkit/audit documents, exactly as notes.Handler's own
// recordNoteCreatedAudit uses it (see that method's doc comment for the
// mechanism itself). h.bus nil is treated exactly like notes' handler
// treats it: nothing is recorded, and the operation that already
// succeeded (or failed, for a login-failure record) is unaffected either
// way.
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
// Two shapes of call site pass "": a PRE-AUTH event (registration, a failed
// sign-in), where the caller is not yet authenticated and the tenant_id a
// request merely asserts is not an attestation -- an unauthenticated caller
// must not be able to stamp rows into a tenant's ledger by naming it -- and
// the social-callback BIND, which is recorded at an unauthenticated
// callback (the flow authenticates by the single-use state the signed-in
// authorize step minted, and no session is started), so no tenant is
// attested at recording time. Every protected operation below -- logout,
// identity unbind, MFA changes, tenant switch, session revoke -- carries
// its principal's tenant, and every sign-in success this Handler records
// (password, SMS, social) carries the tenant the new session resolved
// (enterprise SSO sign-ins have no recordAudit site: the SSO service has no
// mounted HTTP surface, the gap AGENTS.md's known-limitation table records
// for AuditActionSSOConfigure). AuthnSwitchTenant
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
// A failure is logged, not returned, for the identical reason
// recordNoteCreatedAudit's own doc comment gives: the underlying
// operation has already been committed and answered to the caller, so an
// audit-write failure must not turn that answer into something else.
func (h *Handler) recordAudit(ctx context.Context, tenantID pkgcore.TenantID, actorID, action string, resource audit.Resource, result audit.Result) {
	if h.bus == nil {
		return
	}
	if actorID != "" {
		ctx = pkgcore.WithActor(ctx, pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: actorID})
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

// clientIP extracts the requesting client's address from r, for the rate
// limiter and the login/session records. It reads RemoteAddr's host part
// only -- never X-Forwarded-For, which an untrusted client can set to
// whatever it likes; a deployment behind a real proxy terminates that header
// into RemoteAddr before this handler ever sees the request.
func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if idx := strings.LastIndex(host, ":"); idx != -1 && !strings.Contains(host[idx:], "]") {
		host = host[:idx]
	}
	return strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
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
// (the same reasoning clientIP's doc comment gives for never reading
// X-Forwarded-For). A host not actually serving over HTTPS anywhere must
// never pass it.
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
		Name:     preAuthCookieName,
		Value:    value,
		Path:     "/api/v1/authn/social",
		MaxAge:   int(DefaultOAuthStateTTL.Seconds()),
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
	if err := decodeJSON(r, &req); err != nil {
		writeAppError(w, err)
		return
	}

	user, err := h.svc.Register(ctx, RegisterInput{
		Email:       deref(req.Email),
		Phone:       deref(req.Phone),
		Password:    req.Password,
		DisplayName: deref(req.DisplayName),
		Locale:      deref(req.Locale),
		IP:          clientIP(r),
	})
	if err != nil {
		writeAppError(w, err)
		return
	}

	// Registration is pre-tenant: the caller is unauthenticated and the
	// account has no membership yet (org's own machinery grants one on
	// authn.user.created), so no tenant is attested -- recordAudit's
	// pre-auth case, documented on the method itself.
	h.recordAudit(ctx, "", user.ID, AuditActionUserRegister,
		audit.Resource{Type: "user", ID: user.ID},
		audit.Result{Success: true})
	obs.FromContext(ctx).Info("account registered", "user_id", user.ID)
	writeJSON(w, http.StatusCreated, toUserResponse(user))
}

// AuthnLoginWithPassword implements api.ServerInterface.
func (h *Handler) AuthnLoginWithPassword(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req api.AuthnLoginWithPasswordRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAppError(w, err)
		return
	}

	pair, err := h.svc.Login(ctx, LoginInput{
		Identifier: req.Identifier,
		Password:   req.Password,
		TenantID:   pkgcore.TenantID(deref(req.TenantID)),
		Device:     deref(req.Device),
		UserAgent:  r.UserAgent(),
		IP:         clientIP(r),
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
	if err := decodeJSON(r, &req); err != nil {
		writeAppError(w, err)
		return
	}

	if err := h.svc.RequestSMSCode(r.Context(), RequestSMSCodeInput{Phone: req.Phone, IP: clientIP(r)}); err != nil {
		writeAppError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// AuthnLoginWithSMSCode implements api.ServerInterface.
func (h *Handler) AuthnLoginWithSMSCode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req api.AuthnLoginWithSMSCodeRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAppError(w, err)
		return
	}

	pair, err := h.svc.LoginWithSMSCode(ctx, SMSLoginInput{
		Phone:     req.Phone,
		Code:      req.Code,
		TenantID:  pkgcore.TenantID(deref(req.TenantID)),
		Device:    deref(req.Device),
		UserAgent: r.UserAgent(),
		IP:        clientIP(r),
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
	if err := decodeJSON(r, &req); err != nil {
		writeAppError(w, err)
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
	if err := decodeJSON(r, &req); err != nil {
		writeAppError(w, err)
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
		IP:             clientIP(r),
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
	ctx := r.Context()
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
	if err := decodeJSON(r, &req); err != nil {
		writeAppError(w, err)
		return
	}
	ctx := r.Context()
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
// first-time-setup case to carve out: every call is "changing MFA
// settings" (docs/internal/05 line 127) and RequireStepUp's unconditional
// gate applies directly, exactly as wired.
func (h *Handler) AuthnRegenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	RequireStepUp(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := h.requirePrincipal(w, r)
		if !ok {
			return
		}
		ctx := r.Context()
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
	if err := decodeJSON(r, &req); err != nil {
		writeAppError(w, err)
		return
	}
	pair, err := h.svc.VerifyStepUp(r.Context(), principal, req.Code, clientIP(r))
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
	if err := decodeJSON(r, &req); err != nil {
		writeAppError(w, err)
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
		EmailVerified: &user.EmailVerified,
		PhoneVerified: &user.PhoneVerified,
		CreatedAt:     &createdAt,
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
func toSessionResponse(session *Session, currentSessionID string) api.AuthnSession {
	createdAt := session.CreatedAt
	lastSeenAt := session.LastSeenAt
	isCurrent := session.ID == currentSessionID
	amr := session.AMRList()
	return api.AuthnSession{
		ID:         &session.ID,
		Status:     &session.Status,
		Device:     str(session.Device),
		UserAgent:  str(session.UserAgent),
		IP:         str(session.IP),
		Amr:        &amr,
		CreatedAt:  &createdAt,
		LastSeenAt: &lastSeenAt,
		IsCurrent:  &isCurrent,
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
// exactly, even though it is written from the package-private errorBody
// type rather than the generated one.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", jsonContentType)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// compile-time check that *Handler implements the api.ServerInterface
// generated from this module's api/openapi.yaml -- the enforcement half of
// the spec-first flow (docs/internal/21-api-contract.md): add an operation
// to the fragment, regenerate, and this assertion stops compiling until
// Handler implements it.
var _ api.ServerInterface = (*Handler)(nil)
