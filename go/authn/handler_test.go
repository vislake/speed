package authn

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn/api"
	"github.com/vislake/speed/go/authn/internal/totp"
	"github.com/vislake/speed/go/pkgcore"
)

// newTestHandler builds a Handler over a fresh serviceFixture, so a test can
// assert on both the HTTP response and the underlying Service state (events
// published, rows written) exactly as notes' handler_test.go does for its
// own Handler.
func newTestHandler(t *testing.T, extra ...Option) (*Handler, *serviceFixture) {
	t.Helper()
	f := newServiceFixture(t, extra...)
	return NewHandler(f.svc), f
}

// doHandlerJSON issues method against path on h, JSON-encoding body when it
// is non-nil, and injecting principal into the request context exactly the
// way authn.Middleware would (WithPrincipal) when principal is non-nil --
// this exercises Handler downstream of where Middleware normally runs, the
// same isolation notes' handler_test.go's doRequest documents for its own
// tests. It is named distinctly from provider.go's own doJSON (the outbound
// HTTP helper social providers use), which this file would otherwise shadow.
func doHandlerJSON(t *testing.T, h *Handler, method, path string, body any, principal *Principal) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if principal != nil {
		req = req.WithContext(WithPrincipal(req.Context(), *principal))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// decodeBody decodes rec's JSON body into T, failing the test on a decode
// error.
func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, rec.Body.String())
	}
	return v
}

// decodeAuthnError decodes rec's body as the structured {code, params}
// envelope every failed operation below writes.
func decodeAuthnError(t *testing.T, rec *httptest.ResponseRecorder) api.AuthnError {
	t.Helper()
	return decodeBody[api.AuthnError](t, rec)
}

// principalFor mints a Principal shaped like the one Middleware would have
// put in the request context for pair, for a test that needs to call a
// protected operation as the user pair.Principal names.
func principalFor(pair *TokenPair) *Principal {
	p := pair.Principal
	return &p
}

func TestHandler_Register_ValidBody_ReturnsCreatedUser(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t)

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/register", api.AuthnRegisterRequest{
		Email: strPtr("new@example.com"), Password: testPassword, DisplayName: strPtr("New Person"),
	}, nil)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	resp := decodeBody[api.AuthnUser](t, rec)
	if resp.ID == nil || *resp.ID == "" {
		t.Fatal("response ID is missing")
	}
	if resp.Email == nil || *resp.Email != "new@example.com" {
		t.Errorf("response Email = %v, want %q", resp.Email, "new@example.com")
	}
}

func TestHandler_Register_WeakPassword_Returns400(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t)

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/register", api.AuthnRegisterRequest{
		Email: strPtr("weak@example.com"), Password: "short",
	}, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	errBody := decodeAuthnError(t, rec)
	if errBody.Code == nil || *errBody.Code != ErrPasswordTooShort.Code {
		t.Errorf("error code = %v, want %s", errBody.Code, ErrPasswordTooShort.Code)
	}
}

func TestHandler_Register_DuplicateEmail_Returns409(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "dup@example.com", testTenantA)

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/register", api.AuthnRegisterRequest{
		Email: strPtr("dup@example.com"), Password: testPassword,
	}, nil)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestHandler_LoginWithPassword_ValidCredentials_ReturnsTokenPair(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "login@example.com", testTenantA)

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/login/password", api.AuthnLoginWithPasswordRequest{
		Identifier: "login@example.com", Password: testPassword,
	}, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	pair := decodeBody[api.AuthnTokenPair](t, rec)
	if pair.AccessToken == nil || *pair.AccessToken == "" {
		t.Error("response carries no access token")
	}
	if pair.RefreshToken == nil || *pair.RefreshToken == "" {
		t.Error("response carries no refresh token")
	}
	if pair.Principal == nil || pair.Principal.UserID == nil {
		t.Fatal("response carries no principal")
	}
}

func TestHandler_LoginWithPassword_WrongPassword_Returns401(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "login2@example.com", testTenantA)

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/login/password", api.AuthnLoginWithPasswordRequest{
		Identifier: "login2@example.com", Password: "the wrong password",
	}, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
	errBody := decodeAuthnError(t, rec)
	if errBody.Code == nil || *errBody.Code != ErrInvalidCredentials.Code {
		t.Errorf("error code = %v, want %s", errBody.Code, ErrInvalidCredentials.Code)
	}
}

// TestHandler_LoginWithPassword_Locked_Returns429WithRetryAfter is the
// round's HTTP-translation proof: ratelimit.go's progressive lockout is
// business logic with no HTTP opinion of its own, and this handler is what
// turns ErrAccountLocked into a 429 carrying a Retry-After header.
func TestHandler_LoginWithPassword_Locked_Returns429WithRetryAfter(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "locked@example.com", testTenantA)

	// One wrong password already opens the progressive lockout window
	// (ratelimit.go's loginLockoutBase applies after the first recorded
	// failure), so the very next attempt -- even with the RIGHT password
	// -- is refused as locked rather than re-verified.
	doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/login/password", api.AuthnLoginWithPasswordRequest{
		Identifier: "locked@example.com", Password: "wrong once",
	}, nil)

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/login/password", api.AuthnLoginWithPasswordRequest{
		Identifier: "locked@example.com", Password: testPassword,
	}, nil)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got == "" || got == "0" {
		t.Errorf("Retry-After header = %q, want a positive value", got)
	}
	errBody := decodeAuthnError(t, rec)
	if errBody.Code == nil || *errBody.Code != ErrAccountLocked.Code {
		t.Errorf("error code = %v, want %s", errBody.Code, ErrAccountLocked.Code)
	}
}

func TestHandler_RequestSMSCode_KnownAndUnknownPhone_BothReturn202(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	h, f := newTestHandler(t, WithSMSSender(NewConsoleSMSSender(&out)))
	f.registerUser(t, "smsuser@example.com", testTenantA)
	if err := f.svc.Users().Save(t.Context(), mustSetPhone(t, f, "smsuser@example.com", "+15550000001")); err != nil {
		t.Fatalf("save the registered phone: %v", err)
	}

	known := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/login/sms/request", api.AuthnRequestSMSCodeRequest{Phone: "+15550000001"}, nil)
	if known.Code != http.StatusAccepted {
		t.Fatalf("known-phone status = %d, want %d; body = %s", known.Code, http.StatusAccepted, known.Body.String())
	}
	if out.Len() == 0 {
		t.Error("no SMS was sent for a known phone number")
	}

	out.Reset()
	unknown := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/login/sms/request", api.AuthnRequestSMSCodeRequest{Phone: "+15559999999"}, nil)
	if unknown.Code != http.StatusAccepted {
		t.Fatalf("unknown-phone status = %d, want %d (a request for a number nobody registered must not disclose that)", unknown.Code, http.StatusAccepted)
	}
	if out.Len() != 0 {
		t.Error("an SMS was sent for an UNREGISTERED phone number, which discloses registration status")
	}
}

// mustSetPhone attaches phone to the account registered under email and
// returns the updated row, for the one test above that needs a phone
// number on an otherwise email-registered fixture user.
func mustSetPhone(t *testing.T, f *serviceFixture, email, phone string) *User {
	t.Helper()
	user, err := f.svc.Users().FindByEmail(t.Context(), email)
	if err != nil {
		t.Fatalf("FindByEmail(%s) error = %v", email, err)
	}
	idx, err := f.svc.Users().PhoneIndexOf(phone)
	if err != nil {
		t.Fatalf("PhoneIndexOf(%s) error = %v", phone, err)
	}
	user.Phone = phone
	user.PhoneIndex = &idx
	return user
}

func TestHandler_LoginWithSMSCode_ValidCode_ReturnsTokenPair(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	h, f := newTestHandler(t, WithSMSSender(NewConsoleSMSSender(&out)))
	f.registerUser(t, "smslogin@example.com", testTenantA)
	if err := f.svc.Users().Save(t.Context(), mustSetPhone(t, f, "smslogin@example.com", "+15550000002")); err != nil {
		t.Fatalf("save the registered phone: %v", err)
	}

	req := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/login/sms/request", api.AuthnRequestSMSCodeRequest{Phone: "+15550000002"}, nil)
	if req.Code != http.StatusAccepted {
		t.Fatalf("request status = %d, want %d", req.Code, http.StatusAccepted)
	}
	code := extractSMSCode(t, out.String())

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/login/sms", api.AuthnLoginWithSMSCodeRequest{
		Phone: "+15550000002", Code: code,
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	pair := decodeBody[api.AuthnTokenPair](t, rec)
	if pair.AccessToken == nil || *pair.AccessToken == "" {
		t.Error("response carries no access token")
	}
}

// smsCodeRunPattern matches a maximal run of ASCII digits, so
// extractSMSCode can find the one run that is EXACTLY smsCodeDigits long --
// the code itself -- rather than the first digit it sees, which
// consoleSMSSender's "SMS to <phone>: <text>" framing (sms.go) puts a
// PHONE NUMBER'S digits before the code.
var smsCodeRunPattern = regexp.MustCompile(`\d+`)

// extractSMSCode pulls the numeric code out of a rendered SMS body -- a
// small, deliberately narrow parser rather than a shared production helper,
// since production code never needs to read a code back out of a message
// it just composed.
func extractSMSCode(t *testing.T, message string) string {
	t.Helper()
	for _, run := range smsCodeRunPattern.FindAllString(message, -1) {
		if len(run) == smsCodeDigits {
			return run
		}
	}
	t.Fatalf("no %d-digit run found in SMS body %q", smsCodeDigits, message)
	return ""
}

func TestHandler_RefreshToken_ValidToken_ReturnsNewPair(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "refresh@example.com", testTenantA)
	login := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/login/password", api.AuthnLoginWithPasswordRequest{
		Identifier: "refresh@example.com", Password: testPassword,
	}, nil)
	original := decodeBody[api.AuthnTokenPair](t, login)

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/token/refresh", api.AuthnRefreshTokenRequest{
		RefreshToken: *original.RefreshToken,
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	rotated := decodeBody[api.AuthnTokenPair](t, rec)
	if *rotated.RefreshToken == *original.RefreshToken {
		t.Error("refresh returned the SAME refresh token, want a rotated one")
	}

	// The refresh-rotation replay rule reaches all the way through this
	// handler: presenting the already-consumed original token again must
	// now fail, not merely rotate a second time.
	replay := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/token/refresh", api.AuthnRefreshTokenRequest{
		RefreshToken: *original.RefreshToken,
	}, nil)
	if replay.Code != http.StatusUnauthorized {
		t.Errorf("replay status = %d, want %d", replay.Code, http.StatusUnauthorized)
	}
}

func TestHandler_Logout_NoPrincipal_Returns401(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t)
	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/logout", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandler_Logout_ValidPrincipal_RevokesSessionAndReturns204(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	user := f.registerUser(t, "logout@example.com", testTenantA)
	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "logout@example.com", Password: testPassword, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/logout", nil, principalFor(pair))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}

	sessions, err := f.svc.ListSessions(t.Context(), user.ID)
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	if len(sessions) != 1 || sessions[0].Status != SessionStatusRevoked {
		t.Errorf("sessions = %+v, want exactly one revoked session", sessions)
	}
}

func TestHandler_GetMe_NoPrincipal_Returns401(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t)
	rec := doHandlerJSON(t, h, http.MethodGet, "/api/v1/authn/me", nil, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandler_GetMe_ValidPrincipal_ReturnsIt(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "me@example.com", testTenantA)
	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "me@example.com", Password: testPassword, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	rec := doHandlerJSON(t, h, http.MethodGet, "/api/v1/authn/me", nil, principalFor(pair))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decodeBody[api.AuthnPrincipal](t, rec)
	if resp.UserID == nil || *resp.UserID != pair.Principal.UserID {
		t.Errorf("UserID = %v, want %q", resp.UserID, pair.Principal.UserID)
	}
	if resp.TenantID == nil || *resp.TenantID != string(testTenantA) {
		t.Errorf("TenantID = %v, want %q", resp.TenantID, testTenantA)
	}
}

func TestHandler_SocialAuthorize_UnknownProvider_Returns400(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t)
	rec := doHandlerJSON(t, h, http.MethodGet, "/api/v1/authn/social/nosuchprovider/authorize?redirect_uri="+testRedirectURI, nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestHandler_SocialSignIn_FullRoundTrip drives authorize then callback
// exactly as a browser would: the pre-auth cookie the first response sets
// is carried onto the second request. This is the round's proof that the
// cookie-derived SessionBinding actually gates the callback end to end,
// through the HTTP surface rather than by calling SocialAuthorizeURL and
// SocialCallback directly the way identity_test.go's socialSignIn does.
func TestHandler_SocialSignIn_FullRoundTrip(t *testing.T) {
	t.Parallel()

	// The callback auto-links to a PRE-REGISTERED account (verified email,
	// trusted provider) rather than letting the flow JIT-provision a brand
	// new one: a freshly provisioned account has no tenant membership yet
	// (org's own concern, deferred per AGENTS.md's Known Limitations) and
	// would fail to mint a session for exactly that reason, which is not
	// what this test is proving.
	provider := &stubProvider{name: ProviderGoogle, identity: &ExternalIdentity{
		ExternalID: "ext-1", Email: "social@example.com", EmailVerified: true, Name: "Social Person",
	}}
	allowlist, err := NewRedirectAllowlist(testRedirectURI)
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	h, f := newTestHandler(t, WithSocialProviders(provider), WithRedirectAllowlist(allowlist), WithTrustedProviders(ProviderGoogle))
	f.registerUser(t, "social@example.com", testTenantA)

	authorizeReq := httptest.NewRequest(http.MethodGet, "/api/v1/authn/social/google/authorize?redirect_uri="+testRedirectURI, nil)
	authorizeRec := httptest.NewRecorder()
	h.ServeHTTP(authorizeRec, authorizeReq)
	if authorizeRec.Code != http.StatusOK {
		t.Fatalf("authorize status = %d, want %d; body = %s", authorizeRec.Code, http.StatusOK, authorizeRec.Body.String())
	}
	authorizeResp := decodeBody[api.AuthnSocialAuthorizeResponse](t, authorizeRec)
	if authorizeResp.AuthorizeURL == nil || *authorizeResp.AuthorizeURL == "" {
		t.Fatal("authorize response carries no URL")
	}
	state := parseQuery(t, *authorizeResp.AuthorizeURL).Get("state")
	if state == "" {
		t.Fatal("authorize URL carries no state parameter")
	}
	cookies := authorizeRec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("authorize response set %d cookies, want exactly 1 (the pre-auth cookie)", len(cookies))
	}

	body, err := json.Marshal(api.AuthnSocialCallbackRequest{Code: "test-code", State: state, TenantID: strPtr(string(testTenantA))})
	if err != nil {
		t.Fatalf("marshal callback request: %v", err)
	}
	callbackReq := httptest.NewRequest(http.MethodPost, "/api/v1/authn/social/google/callback", bytes.NewReader(body))
	callbackReq.AddCookie(cookies[0])
	callbackRec := httptest.NewRecorder()
	h.ServeHTTP(callbackRec, callbackReq)
	if callbackRec.Code != http.StatusOK {
		t.Fatalf("callback status = %d, want %d; body = %s", callbackRec.Code, http.StatusOK, callbackRec.Body.String())
	}
	callbackResp := decodeBody[api.AuthnSocialLoginResponse](t, callbackRec)
	if callbackResp.AutoLinked == nil || !*callbackResp.AutoLinked {
		t.Error("AutoLinked = false, want true for a verified email on a trusted channel")
	}
	if callbackResp.Created != nil && *callbackResp.Created {
		t.Error("Created = true, want the pre-registered account to be reused")
	}
	if callbackResp.Tokens == nil || callbackResp.Tokens.AccessToken == nil {
		t.Fatal("callback response carries no session for a sign-in flow")
	}
}

// TestHandler_SocialCallback_NoCookie_Returns401 proves the cookie is load
// bearing: a callback that never carried the pre-auth cookie at all cannot
// possibly have originated from this server's own authorize step.
func TestHandler_SocialCallback_NoCookie_Returns401(t *testing.T) {
	t.Parallel()
	provider := &stubProvider{name: ProviderGoogle, identity: &ExternalIdentity{ExternalID: "x", Email: "x@example.com", EmailVerified: true}}
	allowlist, err := NewRedirectAllowlist(testRedirectURI)
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	h, _ := newTestHandler(t, WithSocialProviders(provider), WithRedirectAllowlist(allowlist))

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/social/google/callback", api.AuthnSocialCallbackRequest{
		Code: "test-code", State: "whatever-state",
	}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
}

func TestHandler_ListIdentities_ReturnsOnlyCallersOwn(t *testing.T) {
	t.Parallel()
	provider := &stubProvider{name: ProviderGoogle, identity: &ExternalIdentity{ExternalID: "ext-2", Email: "identities@example.com", EmailVerified: true}}
	allowlist, err := NewRedirectAllowlist(testRedirectURI)
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	h, f := newTestHandler(t, WithSocialProviders(provider), WithRedirectAllowlist(allowlist), WithTrustedProviders(ProviderGoogle))
	user := f.registerUser(t, "identities@example.com", testTenantA)

	result, err := socialSignIn(t, f, provider, testTenantA)
	if err != nil {
		t.Fatalf("socialSignIn() error = %v", err)
	}
	if !result.AutoLinked {
		t.Fatal("AutoLinked = false, want true (Google is on the fixture's trusted-provider list and the email is verified)")
	}

	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "identities@example.com", Password: testPassword, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if pair.Principal.UserID != user.ID {
		t.Fatalf("Login() principal user = %s, want %s", pair.Principal.UserID, user.ID)
	}

	rec := doHandlerJSON(t, h, http.MethodGet, "/api/v1/authn/identities", nil, principalFor(pair))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decodeBody[api.AuthnListIdentitiesResponse](t, rec)
	if resp.Identities == nil || len(*resp.Identities) != 1 {
		t.Fatalf("Identities = %v, want exactly 1", resp.Identities)
	}
	if (*resp.Identities)[0].Provider == nil || *(*resp.Identities)[0].Provider != ProviderGoogle {
		t.Errorf("Identities[0].Provider = %v, want %q", (*resp.Identities)[0].Provider, ProviderGoogle)
	}
}

func TestHandler_UnbindIdentity_NotOwnedBySelf_Returns404(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "unbind@example.com", testTenantA)
	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "unbind@example.com", Password: testPassword, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	rec := doHandlerJSON(t, h, http.MethodDelete, "/api/v1/authn/identities/no-such-identity", nil, principalFor(pair))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestHandler_MFAEnrollConfirmStepUp_FullRoundTrip(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "mfa@example.com", testTenantA)
	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "mfa@example.com", Password: testPassword, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	principal := principalFor(pair)

	enroll := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/totp/enroll", nil, principal)
	if enroll.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, want %d; body = %s", enroll.Code, http.StatusOK, enroll.Body.String())
	}
	enrollResp := decodeBody[api.AuthnEnrollTOTPResponse](t, enroll)
	if enrollResp.Secret == nil || *enrollResp.Secret == "" {
		t.Fatal("enroll response carries no secret")
	}

	code, err := totp.Code(*enrollResp.Secret, time.Now())
	if err != nil {
		t.Fatalf("compute a TOTP code: %v", err)
	}
	confirm := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/totp/confirm", api.AuthnConfirmTOTPRequest{Code: code}, principal)
	if confirm.Code != http.StatusOK {
		t.Fatalf("confirm status = %d, want %d; body = %s", confirm.Code, http.StatusOK, confirm.Body.String())
	}
	confirmResp := decodeBody[api.AuthnRecoveryCodesResponse](t, confirm)
	if confirmResp.RecoveryCodes == nil || len(*confirmResp.RecoveryCodes) != recoveryCodeCount {
		t.Fatalf("RecoveryCodes = %v, want %d codes", confirmResp.RecoveryCodes, recoveryCodeCount)
	}

	// A DIFFERENT time step than the one ConfirmTOTP just consumed:
	// verifyTOTPFactor refuses a step at or before factor.LastUsedStep,
	// exactly like mfa_test.go's own step-up cases use
	// time.Now().Add(totp.Period) for the same reason.
	stepUpCode, err := totp.Code(*enrollResp.Secret, time.Now().Add(totp.Period))
	if err != nil {
		t.Fatalf("compute a step-up TOTP code: %v", err)
	}
	stepUp := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/step-up", api.AuthnVerifyStepUpRequest{Code: stepUpCode}, principal)
	if stepUp.Code != http.StatusOK {
		t.Fatalf("step-up status = %d, want %d; body = %s", stepUp.Code, http.StatusOK, stepUp.Body.String())
	}
	stepUpResp := decodeBody[api.AuthnTokenPair](t, stepUp)
	if stepUpResp.Principal == nil || stepUpResp.Principal.Amr == nil {
		t.Fatal("step-up response carries no amr")
	}
	foundSecondFactor := false
	for _, m := range *stepUpResp.Principal.Amr {
		if m == "mfa:totp" {
			foundSecondFactor = true
		}
	}
	if !foundSecondFactor {
		t.Errorf("step-up amr = %v, want it to include mfa:totp", *stepUpResp.Principal.Amr)
	}
	// The step-up response reuses the caller's existing refresh token
	// (mintPairWithAMR's contract) rather than minting a new one.
	if stepUpResp.RefreshToken != nil {
		t.Error("step-up response carries a refresh token, want it absent (reuses the existing one)")
	}

	regen := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/recovery-codes/regenerate", nil, principal)
	if regen.Code != http.StatusOK {
		t.Fatalf("regenerate status = %d, want %d; body = %s", regen.Code, http.StatusOK, regen.Body.String())
	}
	regenResp := decodeBody[api.AuthnRecoveryCodesResponse](t, regen)
	if regenResp.RecoveryCodes == nil || len(*regenResp.RecoveryCodes) != recoveryCodeCount {
		t.Fatalf("regenerated RecoveryCodes = %v, want %d codes", regenResp.RecoveryCodes, recoveryCodeCount)
	}
}

func TestHandler_SwitchTenant_ActiveMember_ReissuesAccessToken(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "switch@example.com", testTenantA, testTenantB)
	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "switch@example.com", Password: testPassword, TenantID: testTenantA, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/tenant/switch", api.AuthnSwitchTenantRequest{TenantID: string(testTenantB)}, principalFor(pair))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decodeBody[api.AuthnTokenPair](t, rec)
	if resp.Principal == nil || resp.Principal.TenantID == nil || *resp.Principal.TenantID != string(testTenantB) {
		t.Errorf("switched TenantID = %v, want %q", resp.Principal, testTenantB)
	}
	if resp.RefreshToken != nil {
		t.Error("switch response carries a refresh token, want it absent (reuses the existing session)")
	}
}

func TestHandler_SwitchTenant_NotAMember_Returns403(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "notmember@example.com", testTenantA)
	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "notmember@example.com", Password: testPassword, TenantID: testTenantA, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/tenant/switch", api.AuthnSwitchTenantRequest{TenantID: string(testTenantB)}, principalFor(pair))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

func TestHandler_ListSessions_MarksCurrentDevice(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "sessions@example.com", testTenantA)
	if _, err := f.svc.Login(t.Context(), LoginInput{Identifier: "sessions@example.com", Password: testPassword, Device: "laptop", IP: "203.0.113.9"}); err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	f.clock.Advance(time.Minute)
	second, err := f.svc.Login(t.Context(), LoginInput{Identifier: "sessions@example.com", Password: testPassword, Device: "phone", IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	rec := doHandlerJSON(t, h, http.MethodGet, "/api/v1/authn/sessions", nil, principalFor(second))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decodeBody[api.AuthnListSessionsResponse](t, rec)
	if resp.Sessions == nil || len(*resp.Sessions) != 2 {
		t.Fatalf("Sessions = %v, want 2", resp.Sessions)
	}
	current := 0
	for _, s := range *resp.Sessions {
		if s.IsCurrent != nil && *s.IsCurrent {
			current++
			if s.ID == nil || *s.ID != second.Principal.SessionID {
				t.Errorf("the current session = %v, want the calling principal's own session %s", s.ID, second.Principal.SessionID)
			}
		}
	}
	if current != 1 {
		t.Errorf("%d sessions were marked current, want exactly 1", current)
	}
}

func TestHandler_RevokeSession_AnotherUsers_Returns404(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "victim2@example.com", testTenantA)
	f.registerUser(t, "attacker2@example.com", testTenantA)
	victim, err := f.svc.Login(t.Context(), LoginInput{Identifier: "victim2@example.com", Password: testPassword, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login(victim) error = %v", err)
	}
	attacker, err := f.svc.Login(t.Context(), LoginInput{Identifier: "attacker2@example.com", Password: testPassword, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login(attacker) error = %v", err)
	}

	rec := doHandlerJSON(t, h, http.MethodDelete, "/api/v1/authn/sessions/"+victim.Principal.SessionID, nil, principalFor(attacker))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNotFound, rec.Body.String())
	}
	errBody := decodeAuthnError(t, rec)
	if errBody.Code == nil || *errBody.Code != ErrSessionNotFound.Code {
		t.Errorf("error code = %v, want %s", errBody.Code, ErrSessionNotFound.Code)
	}
}

func TestHandler_RevokeOtherSessions_KeepsCurrent(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "bulk2@example.com", testTenantA)
	current, err := f.svc.Login(t.Context(), LoginInput{Identifier: "bulk2@example.com", Password: testPassword, Device: "laptop", IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	if _, err := f.svc.Login(t.Context(), LoginInput{Identifier: "bulk2@example.com", Password: testPassword, Device: "phone", IP: "203.0.113.9"}); err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/sessions/revoke-others", nil, principalFor(current))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decodeBody[api.AuthnRevokeOtherSessionsResponse](t, rec)
	if resp.RevokedCount == nil || *resp.RevokedCount != 1 {
		t.Fatalf("RevokedCount = %v, want 1", resp.RevokedCount)
	}

	if _, err := f.svc.Refresh(t.Context(), current.RefreshToken); err != nil {
		t.Errorf("the current session's refresh failed after revoke-others: %v", err)
	}
}

func TestHandler_ListLoginHistory_ScopedToCallingUser(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "history1@example.com", testTenantA)
	f.registerUser(t, "history2@example.com", testTenantA)
	pair1, err := f.svc.Login(t.Context(), LoginInput{Identifier: "history1@example.com", Password: testPassword, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login(1) error = %v", err)
	}
	if _, err := f.svc.Login(t.Context(), LoginInput{Identifier: "history2@example.com", Password: testPassword, IP: "203.0.113.9"}); err != nil {
		t.Fatalf("Login(2) error = %v", err)
	}

	rec := doHandlerJSON(t, h, http.MethodGet, "/api/v1/authn/login-history", nil, principalFor(pair1))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decodeBody[api.AuthnListLoginHistoryResponse](t, rec)
	if resp.Attempts == nil || len(*resp.Attempts) != 1 {
		t.Fatalf("Attempts = %v, want exactly 1 (never another account's)", resp.Attempts)
	}
}

// TestHandler_DeploymentModeConsistency_SMSFlow is the coding standard's
// mandatory deployment-mode consistency suite: the SAME sequence of HTTP
// calls, exercised against two handlers wired with the two different
// concrete implementations of the one seam this module actually varies by
// deployment mode -- SMSSender (sms.go's NewConsoleSMSSender for the
// standalone deployment mode, NewHTTPSMSSender for the distributed one,
// see module.go's WithDeploymentMode and ErrMissingDistributedSMSSender)
// -- must produce identical outcomes: the same status codes and the same
// error codes at every step. Token values themselves are never compared --
// they are randomly generated on both wirings by design -- only the
// observable behaviour a caller sees.
func TestHandler_DeploymentModeConsistency_SMSFlow(t *testing.T) {
	t.Parallel()

	var consoleOut bytes.Buffer
	standalone, standaloneFixture := newTestHandler(t, WithSMSSender(NewConsoleSMSSender(&consoleOut)))

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(gateway.Close)
	distributed, distributedFixture := newTestHandler(t,
		// The default client is SSRF-guarded (internal/safehttp), which
		// correctly refuses this httptest server's loopback address; a
		// real deployment's gateway is a public endpoint, so this
		// override -- like WithFederationHTTPClient's identical test-only
		// use elsewhere in this package -- is test-only.
		WithSMSSender(NewHTTPSMSSender(gateway.URL, WithHTTPSMSSenderClient(gateway.Client()))),
		WithDeploymentMode(pkgcore.DeploymentModeDistributed),
	)

	type wiring struct {
		name    string
		handler *Handler
		fixture *serviceFixture
	}
	for _, w := range []wiring{
		{"standalone", standalone, standaloneFixture},
		{"distributed", distributed, distributedFixture},
	} {
		t.Run(w.name, func(t *testing.T) {
			w.fixture.registerUser(t, "consistency@example.com", testTenantA)
			user := mustSetPhone(t, w.fixture, "consistency@example.com", "+15550000099")
			if err := w.fixture.svc.Users().Save(t.Context(), user); err != nil {
				t.Fatalf("save phone: %v", err)
			}

			requestRec := doHandlerJSON(t, w.handler, http.MethodPost, "/api/v1/authn/login/sms/request", api.AuthnRequestSMSCodeRequest{Phone: "+15550000099"}, nil)
			if requestRec.Code != http.StatusAccepted {
				t.Fatalf("request status = %d, want %d", requestRec.Code, http.StatusAccepted)
			}

			wrongCodeRec := doHandlerJSON(t, w.handler, http.MethodPost, "/api/v1/authn/login/sms", api.AuthnLoginWithSMSCodeRequest{
				Phone: "+15550000099", Code: "000000",
			}, nil)
			if wrongCodeRec.Code != http.StatusUnauthorized {
				t.Fatalf("wrong-code status = %d, want %d", wrongCodeRec.Code, http.StatusUnauthorized)
			}
			errBody := decodeAuthnError(t, wrongCodeRec)
			if errBody.Code == nil || *errBody.Code != ErrVerificationCodeInvalid.Code {
				t.Errorf("wrong-code error = %v, want %s", errBody.Code, ErrVerificationCodeInvalid.Code)
			}

			unknownRec := doHandlerJSON(t, w.handler, http.MethodPost, "/api/v1/authn/login/sms/request", api.AuthnRequestSMSCodeRequest{Phone: "+15559990001"}, nil)
			if unknownRec.Code != http.StatusAccepted {
				t.Fatalf("unknown-phone request status = %d, want %d", unknownRec.Code, http.StatusAccepted)
			}

			registerRec := doHandlerJSON(t, w.handler, http.MethodPost, "/api/v1/authn/register", api.AuthnRegisterRequest{
				Email: strPtr("dm-" + w.name + "@example.com"), Password: testPassword,
			}, nil)
			if registerRec.Code != http.StatusCreated {
				t.Fatalf("register status = %d, want %d", registerRec.Code, http.StatusCreated)
			}
		})
	}
}

// strPtr returns a pointer to s, for building spec-generated request types
// whose optional fields are pointers.
func strPtr(s string) *string { return &s }
