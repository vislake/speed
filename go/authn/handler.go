package authn

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/vislake/speed/go/authn/api"
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
	svc *Service
	mux *http.ServeMux
}

// NewHandler returns a Handler serving svc's operations. Its routing is
// registered by the generated api.HandlerFromMux helper, deriving this
// module's method+path patterns from api/openapi.yaml's own "paths:" keys --
// see notes' identical NewHandler doc comment for the mechanism.
func NewHandler(svc *Service) *Handler {
	h := &Handler{svc: svc}
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

// decodeJSON decodes r's body into v, translating a decode failure into the
// structured invalid-request-body error every operation below reports it as.
func decodeJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return apperr.Invalid("authn.invalid_request_body").WithCause(err)
	}
	return nil
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
func ensurePreAuthCookie(w http.ResponseWriter, r *http.Request) (string, error) {
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
		Secure:   r.TLS != nil,
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
		writeAppError(w, err)
		return
	}

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
		writeAppError(w, err)
		return
	}

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
	if err := h.svc.Logout(r.Context(), principal.SessionID); err != nil {
		writeAppError(w, err)
		return
	}
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
	binding, err := ensurePreAuthCookie(w, r)
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
	if err := h.svc.UnbindIdentity(r.Context(), principal.UserID, identityID); err != nil {
		writeAppError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AuthnEnrollTOTP implements api.ServerInterface.
func (h *Handler) AuthnEnrollTOTP(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	result, err := h.svc.EnrollTOTP(r.Context(), principal.UserID)
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
	codes, err := h.svc.ConfirmTOTP(r.Context(), principal.UserID, req.Code)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.AuthnRecoveryCodesResponse{RecoveryCodes: &codes})
}

// AuthnRegenerateRecoveryCodes implements api.ServerInterface.
func (h *Handler) AuthnRegenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	codes, err := h.svc.RegenerateRecoveryCodes(r.Context(), principal.UserID)
	if err != nil {
		writeAppError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, api.AuthnRecoveryCodesResponse{RecoveryCodes: &codes})
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
	pair, err := h.svc.SwitchTenant(r.Context(), principal, pkgcore.TenantID(req.TenantID))
	if err != nil {
		writeAppError(w, err)
		return
	}
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
	if err := h.svc.RevokeSession(r.Context(), principal.UserID, sessionID); err != nil {
		writeAppError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AuthnRevokeOtherSessions implements api.ServerInterface.
func (h *Handler) AuthnRevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	revoked, err := h.svc.RevokeOtherSessions(r.Context(), principal.UserID, principal.SessionID)
	if err != nil {
		writeAppError(w, err)
		return
	}
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
