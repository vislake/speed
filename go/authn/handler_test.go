package authn

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn/api"
	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/authn/internal/totp"
	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
)

// newTestHandler builds a Handler over a fresh serviceFixture, so a test can
// assert on both the HTTP response and the underlying Service state (events
// published, rows written) exactly as notes' handler_test.go does for its
// own Handler.
func newTestHandler(t *testing.T, extra ...Option) (*Handler, *serviceFixture) {
	t.Helper()
	h, f, _ := newAuditTestHandler(t, extra...)
	return h, f
}

// newAuditTestHandler is newTestHandler's own constructor, plus the
// EventRecorder subscribed to audit.EventRecorded on the same bus the
// returned Handler publishes audit events to -- for the handful of tests
// that actually assert on those events (auditFailureReason and
// recordAudit's real callers, handler.go), rather than every one of
// newTestHandler's many callers that do not.
//
// The bus and pkgcore.Registry built here are deliberately separate from
// serviceFixture's own internal one (newServiceFixtureWithKV's local
// "bus" variable, which Service publishes its OWN business events on):
// nothing about testing Handler.recordAudit requires sharing it, and a
// real *pkgcore.Registry (pkgcore.NewRegistry) is the same construction
// module.go's own Register runs against in production, giving a real
// AuditActionRegistrar rather than a hand-rolled stand-in.
func newAuditTestHandler(t *testing.T, extra ...Option) (*Handler, *serviceFixture, *testutil.EventRecorder) {
	t.Helper()
	f := newServiceFixture(t, extra...)

	bus := pkgcore.NewMemoryEventBus()
	recorder := testutil.NewEventRecorder()
	recorder.Subscribe(bus, audit.EventRecorded)

	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := reg.AuditActions.Add(auditActions...); err != nil {
		t.Fatalf("AuditActions.Add() error = %v", err)
	}
	return NewHandler(f.svc, bus, reg.AuditActions), f, recorder
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

// TestHandler_Register_MalformedBody_ReturnsCatalogedInvalidRequestBodyCode
// is the regression for P2-6: decodeJSON's answer for a malformed request
// body -- returned by every one of this module's operations that reads
// one, register included -- used to have no entry in errorCodes (and so no
// locale text in either language): a real answer no client could
// localize, only render as a raw key. It is now ErrInvalidRequestBody
// (errors.go), cataloged and bilingually rendered like every other coded
// error this module returns.
func TestHandler_Register_MalformedBody_ReturnsCatalogedInvalidRequestBodyCode(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/register", bytes.NewReader([]byte("{not valid json")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	errBody := decodeAuthnError(t, rec)
	if errBody.Code == nil || *errBody.Code != ErrInvalidRequestBody.Code {
		t.Fatalf("error code = %v, want %s", errBody.Code, ErrInvalidRequestBody.Code)
	}

	if !slices.Contains(errorCodes, *errBody.Code) {
		t.Errorf("returned code %q is not in errorCodes; a client has no locale-backed text to render for it", *errBody.Code)
	}
	for _, language := range []string{"zh-CN", "en-US"} {
		if _, ok := loadLocale(t, language)[*errBody.Code]; !ok {
			t.Errorf("%s locale carries no message for %q", language, *errBody.Code)
		}
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

// findAuditEvent scans recorder for an audit.EventRecorded event whose
// Action matches, failing the test when none is found -- the P2-5
// regression tests' shared assertion helper.
func findAuditEvent(t *testing.T, recorder *testutil.EventRecorder, action string) audit.RecordedEvent {
	t.Helper()
	for _, evt := range recorder.Events() {
		if evt.Type != audit.EventRecorded {
			continue
		}
		recorded, ok := evt.Payload.(audit.RecordedEvent)
		if !ok {
			t.Fatalf("audit.EventRecorded payload has type %T, want audit.RecordedEvent", evt.Payload)
		}
		if recorded.Action == action {
			return recorded
		}
	}
	t.Fatalf("no audit.EventRecorded event with Action %q was published; all events = %+v", action, recorder.Events())
	return audit.RecordedEvent{}
}

// TestHandler_LoginWithPassword_ValidCredentials_RecordsLoginAuditEvent is
// one of the P2-5 regression's representative sample (root round prompt's
// own named example, "login success"): 9 audit actions were declared on
// the registry but nothing anywhere ever called audit.Emit for any of
// them, so a real password sign-in used to leave no AuditEvent at all.
func TestHandler_LoginWithPassword_ValidCredentials_RecordsLoginAuditEvent(t *testing.T) {
	t.Parallel()
	h, f, recorder := newAuditTestHandler(t)
	user := f.registerUser(t, "audit-login@example.com", testTenantA)

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/login/password", api.AuthnLoginWithPasswordRequest{
		Identifier: "audit-login@example.com", Password: testPassword,
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	evt := findAuditEvent(t, recorder, AuditActionUserLogin)
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true")
	}
	if evt.Resource.Type != "user" || evt.Resource.ID != user.ID {
		t.Errorf("Resource = %+v, want {Type: user, ID: %s}", evt.Resource, user.ID)
	}
	if evt.Actor.ID != user.ID {
		t.Errorf("Actor.ID = %q, want %q", evt.Actor.ID, user.ID)
	}
}

// TestHandler_LoginWithPassword_WrongPassword_RecordsLoginFailureAuditEvent
// is the P2-5 regression's other named representative ("login failure"):
// a failed sign-in attempt is exactly as security-relevant as a
// successful one, and used to leave the same nothing behind.
func TestHandler_LoginWithPassword_WrongPassword_RecordsLoginFailureAuditEvent(t *testing.T) {
	t.Parallel()
	h, f, recorder := newAuditTestHandler(t)
	f.registerUser(t, "audit-login-fail@example.com", testTenantA)

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/login/password", api.AuthnLoginWithPasswordRequest{
		Identifier: "audit-login-fail@example.com", Password: "the wrong password",
	}, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}

	evt := findAuditEvent(t, recorder, AuditActionUserLogin)
	if evt.Result.Success {
		t.Errorf("Result.Success = true, want false")
	}
	if evt.Result.FailureReason != ErrInvalidCredentials.Code {
		t.Errorf("Result.FailureReason = %q, want %q", evt.Result.FailureReason, ErrInvalidCredentials.Code)
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

// TestHandler_Logout_ValidPrincipal_RecordsSessionRevokeAuditEvent is the
// P2-5 regression's third named representative, "session revocation":
// AuditActionSessionRevoke was declared but never emitted for any of
// this module's three revoke paths (logout, revoke-one, revoke-others).
func TestHandler_Logout_ValidPrincipal_RecordsSessionRevokeAuditEvent(t *testing.T) {
	t.Parallel()
	h, f, recorder := newAuditTestHandler(t)
	user := f.registerUser(t, "audit-logout@example.com", testTenantA)
	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "audit-logout@example.com", Password: testPassword, IP: "203.0.113.10"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/logout", nil, principalFor(pair))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}

	evt := findAuditEvent(t, recorder, AuditActionSessionRevoke)
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true")
	}
	if evt.Resource.Type != "session" || evt.Resource.ID != pair.Principal.SessionID {
		t.Errorf("Resource = %+v, want {Type: session, ID: %s}", evt.Resource, pair.Principal.SessionID)
	}
	if evt.Actor.ID != user.ID {
		t.Errorf("Actor.ID = %q, want %q", evt.Actor.ID, user.ID)
	}
}

// The eight tests below close a code-review gap the P2-5 round's own
// summary left as "a scope decision": only 3 of the 9 declared audit
// actions (AuditActionUserLogin above, twice, plus
// AuditActionSessionRevoke's logout path) were verified against a real
// event landing on the bus. Each of these drives the real HTTP path that
// calls recordAudit and asserts on the recorded event's Action, Resource,
// Actor and Result -- exactly as the three above do -- so a wrong resource
// id, actor or action string at any of these call sites now fails a test
// rather than going unnoticed.

// TestHandler_Register_ValidBody_RecordsUserRegisterAuditEvent covers
// AuditActionUserRegister, wired at AuthnRegister but never previously
// observed landing on the bus (TestHandler_Register_ValidBody_ReturnsCreatedUser
// uses the plain newTestHandler, which discards its EventRecorder).
func TestHandler_Register_ValidBody_RecordsUserRegisterAuditEvent(t *testing.T) {
	t.Parallel()
	h, _, recorder := newAuditTestHandler(t)

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/register", api.AuthnRegisterRequest{
		Email: strPtr("audit-register@example.com"), Password: testPassword, DisplayName: strPtr("Audit Register"),
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	resp := decodeBody[api.AuthnUser](t, rec)
	if resp.ID == nil || *resp.ID == "" {
		t.Fatal("response ID is missing")
	}

	evt := findAuditEvent(t, recorder, AuditActionUserRegister)
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true")
	}
	if evt.Resource.Type != "user" || evt.Resource.ID != *resp.ID {
		t.Errorf("Resource = %+v, want {Type: user, ID: %s}", evt.Resource, *resp.ID)
	}
	if evt.Actor.ID != *resp.ID {
		t.Errorf("Actor.ID = %q, want %q", evt.Actor.ID, *resp.ID)
	}
}

// TestHandler_SwitchTenant_ActiveMember_RecordsTenantSwitchAuditEvent covers
// AuditActionTenantSwitch, wired at AuthnSwitchTenant but never previously
// observed landing on the bus
// (TestHandler_SwitchTenant_ActiveMember_ReissuesAccessToken uses the plain
// newTestHandler).
func TestHandler_SwitchTenant_ActiveMember_RecordsTenantSwitchAuditEvent(t *testing.T) {
	t.Parallel()
	h, f, recorder := newAuditTestHandler(t)
	user := f.registerUser(t, "audit-switch@example.com", testTenantA, testTenantB)
	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "audit-switch@example.com", Password: testPassword, TenantID: testTenantA, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/tenant/switch", api.AuthnSwitchTenantRequest{TenantID: string(testTenantB)}, principalFor(pair))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	evt := findAuditEvent(t, recorder, AuditActionTenantSwitch)
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true")
	}
	if evt.Resource.Type != "session" || evt.Resource.ID != pair.Principal.SessionID {
		t.Errorf("Resource = %+v, want {Type: session, ID: %s}", evt.Resource, pair.Principal.SessionID)
	}
	if evt.Actor.ID != user.ID {
		t.Errorf("Actor.ID = %q, want %q", evt.Actor.ID, user.ID)
	}
}

// TestHandler_SocialCallback_BindToSignedInAccount_RecordsIdentityBindAuditEvent
// covers AuditActionIdentityBind, wired at AuthnSocialCallback's Bound
// branch -- the one branch TestHandler_SocialSignIn_FullRoundTrip never
// exercises, since that test signs a fresh visitor in rather than binding a
// new identity onto an already-authenticated caller. The authorize call
// below carries the signed-in principal in its request context exactly the
// way AuthnSocialAuthorize itself reads it (PrincipalFromContext), which is
// what makes the subsequent callback take the LinkUserID/bind path
// (identity.go's SocialCallback) instead of the ordinary sign-in path.
func TestHandler_SocialCallback_BindToSignedInAccount_RecordsIdentityBindAuditEvent(t *testing.T) {
	t.Parallel()
	provider := &stubProvider{name: ProviderGoogle, identity: &ExternalIdentity{
		ExternalID: "ext-audit-bind-1", Email: "external-bind@example.com", EmailVerified: true, Name: "External Person",
	}}
	allowlist, err := NewRedirectAllowlist(testRedirectURI)
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	h, f, recorder := newAuditTestHandler(t, WithSocialProviders(provider), WithRedirectAllowlist(allowlist))
	f.registerUser(t, "bindme@example.com", testTenantA)
	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "bindme@example.com", Password: testPassword, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	authorizeReq := httptest.NewRequest(http.MethodGet, "/api/v1/authn/social/google/authorize?redirect_uri="+testRedirectURI, nil)
	authorizeReq = authorizeReq.WithContext(WithPrincipal(authorizeReq.Context(), *principalFor(pair)))
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
	if callbackResp.Bound == nil || !*callbackResp.Bound {
		t.Fatalf("Bound = %v, want true for an authenticated caller binding a new identity", callbackResp.Bound)
	}

	evt := findAuditEvent(t, recorder, AuditActionIdentityBind)
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true")
	}
	if evt.Resource.Type != "identity" {
		t.Errorf("Resource.Type = %q, want %q", evt.Resource.Type, "identity")
	}
	if evt.Actor.ID != pair.Principal.UserID {
		t.Errorf("Actor.ID = %q, want %q", evt.Actor.ID, pair.Principal.UserID)
	}
}

// TestHandler_UnbindIdentity_OwnedBySelfWithPasswordRemaining_RecordsIdentityUnbindAuditEvent
// covers AuditActionIdentityUnbind, wired at AuthnUnbindIdentity but never
// previously observed landing on the bus
// (TestHandler_UnbindIdentity_NotOwnedBySelf_Returns404 only exercises the
// 404 path, on an identity id that never existed). The identity is bound
// directly through the Service (SocialAuthorizeURL/SocialCallback), the
// same shape identity_test.go's own Service-level unbind tests use, so this
// test's own HTTP call is the one under review: DELETE
// /api/v1/authn/identities/{id}.
func TestHandler_UnbindIdentity_OwnedBySelfWithPasswordRemaining_RecordsIdentityUnbindAuditEvent(t *testing.T) {
	t.Parallel()
	provider := &stubProvider{name: ProviderGitHub, identity: &ExternalIdentity{
		ExternalID: "gh-audit-unbind-1", Email: "audit-unbind@example.com",
	}}
	allowlist, err := NewRedirectAllowlist(testRedirectURI)
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	h, f, recorder := newAuditTestHandler(t, WithSocialProviders(provider), WithRedirectAllowlist(allowlist))
	user := f.registerUser(t, "audit-unbind@example.com", testTenantA)

	authorizeURL, err := f.svc.SocialAuthorizeURL(t.Context(), SocialAuthorizeInput{
		Provider: provider.Name(), RedirectURI: testRedirectURI, LinkUserID: user.ID,
	})
	if err != nil {
		t.Fatalf("SocialAuthorizeURL() error = %v", err)
	}
	state := parseQuery(t, authorizeURL).Get("state")
	bound, err := f.svc.SocialCallback(t.Context(), SocialCallbackInput{
		Provider: provider.Name(), Code: "code", State: state,
	})
	if err != nil {
		t.Fatalf("SocialCallback() (bind) error = %v", err)
	}

	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "audit-unbind@example.com", Password: testPassword, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	rec := doHandlerJSON(t, h, http.MethodDelete, "/api/v1/authn/identities/"+bound.Identity.ID, nil, principalFor(pair))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}

	evt := findAuditEvent(t, recorder, AuditActionIdentityUnbind)
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true")
	}
	if evt.Resource.Type != "identity" || evt.Resource.ID != bound.Identity.ID {
		t.Errorf("Resource = %+v, want {Type: identity, ID: %s}", evt.Resource, bound.Identity.ID)
	}
	if evt.Actor.ID != user.ID {
		t.Errorf("Actor.ID = %q, want %q", evt.Actor.ID, user.ID)
	}
}

// TestHandler_ConfirmTOTP_ValidCode_RecordsMFAEnrollAuditEvent covers
// AuditActionMFAEnroll, wired at AuthnConfirmTOTP but never previously
// observed landing on the bus (TestHandler_MFAEnrollConfirmStepUp_FullRoundTrip
// uses the plain newTestHandler).
func TestHandler_ConfirmTOTP_ValidCode_RecordsMFAEnrollAuditEvent(t *testing.T) {
	t.Parallel()
	h, f, recorder := newAuditTestHandler(t)
	user := f.registerUser(t, "audit-mfa-enroll@example.com", testTenantA)
	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "audit-mfa-enroll@example.com", Password: testPassword, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	principal := principalFor(pair)

	enroll := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/totp/enroll", nil, principal)
	if enroll.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, want %d; body = %s", enroll.Code, http.StatusOK, enroll.Body.String())
	}
	enrollResp := decodeBody[api.AuthnEnrollTOTPResponse](t, enroll)
	code, err := totp.Code(*enrollResp.Secret, time.Now())
	if err != nil {
		t.Fatalf("compute a TOTP code: %v", err)
	}

	confirm := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/totp/confirm", api.AuthnConfirmTOTPRequest{Code: code}, principal)
	if confirm.Code != http.StatusOK {
		t.Fatalf("confirm status = %d, want %d; body = %s", confirm.Code, http.StatusOK, confirm.Body.String())
	}

	evt := findAuditEvent(t, recorder, AuditActionMFAEnroll)
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true")
	}
	if evt.Resource.Type != "mfa_factor" || evt.Resource.ID != user.ID {
		t.Errorf("Resource = %+v, want {Type: mfa_factor, ID: %s}", evt.Resource, user.ID)
	}
	if evt.Actor.ID != user.ID {
		t.Errorf("Actor.ID = %q, want %q", evt.Actor.ID, user.ID)
	}
}

// TestHandler_RegenerateRecoveryCodes_SteppedUp_RecordsMFARecoveryCodesRegenerateAuditEvent
// covers AuditActionMFARecoveryCodesRegenerate, wired at
// AuthnRegenerateRecoveryCodes but never previously observed landing on the
// bus.
func TestHandler_RegenerateRecoveryCodes_SteppedUp_RecordsMFARecoveryCodesRegenerateAuditEvent(t *testing.T) {
	t.Parallel()
	h, f, recorder := newAuditTestHandler(t)
	user := f.registerUser(t, "audit-mfa-regen@example.com", testTenantA)
	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "audit-mfa-regen@example.com", Password: testPassword, IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	principal := principalFor(pair)

	enroll := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/totp/enroll", nil, principal)
	if enroll.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, want %d; body = %s", enroll.Code, http.StatusOK, enroll.Body.String())
	}
	enrollResp := decodeBody[api.AuthnEnrollTOTPResponse](t, enroll)
	code, err := totp.Code(*enrollResp.Secret, time.Now())
	if err != nil {
		t.Fatalf("compute a TOTP code: %v", err)
	}
	confirm := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/totp/confirm", api.AuthnConfirmTOTPRequest{Code: code}, principal)
	if confirm.Code != http.StatusOK {
		t.Fatalf("confirm status = %d, want %d; body = %s", confirm.Code, http.StatusOK, confirm.Body.String())
	}

	// A different time step than the one ConfirmTOTP just consumed, exactly
	// like TestHandler_MFAEnrollConfirmStepUp_FullRoundTrip's own comment
	// explains: verifyTOTPFactor refuses a step at or before
	// factor.LastUsedStep.
	stepUpCode, err := totp.Code(*enrollResp.Secret, time.Now().Add(totp.Period))
	if err != nil {
		t.Fatalf("compute a step-up TOTP code: %v", err)
	}
	stepUp := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/step-up", api.AuthnVerifyStepUpRequest{Code: stepUpCode}, principal)
	if stepUp.Code != http.StatusOK {
		t.Fatalf("step-up status = %d, want %d; body = %s", stepUp.Code, http.StatusOK, stepUp.Body.String())
	}
	stepUpResp := decodeBody[api.AuthnTokenPair](t, stepUp)
	steppedUp := &Principal{
		UserID:    deref(stepUpResp.Principal.UserID),
		TenantID:  pkgcore.TenantID(deref(stepUpResp.Principal.TenantID)),
		SessionID: deref(stepUpResp.Principal.SessionID),
		AMR:       *stepUpResp.Principal.Amr,
	}

	regen := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/recovery-codes/regenerate", nil, steppedUp)
	if regen.Code != http.StatusOK {
		t.Fatalf("regenerate status = %d, want %d; body = %s", regen.Code, http.StatusOK, regen.Body.String())
	}

	evt := findAuditEvent(t, recorder, AuditActionMFARecoveryCodesRegenerate)
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true")
	}
	if evt.Resource.Type != "mfa_recovery_codes" || evt.Resource.ID != user.ID {
		t.Errorf("Resource = %+v, want {Type: mfa_recovery_codes, ID: %s}", evt.Resource, user.ID)
	}
	if evt.Actor.ID != user.ID {
		t.Errorf("Actor.ID = %q, want %q", evt.Actor.ID, user.ID)
	}
}

// TestHandler_RevokeSession_OwnSession_RecordsSessionRevokeAuditEvent covers
// AuditActionSessionRevoke's revoke-one path, wired at AuthnRevokeSession
// but never previously observed landing on the bus
// (TestHandler_RevokeSession_AnotherUsers_Returns404 only exercises the 404
// path, never a successful revoke).
func TestHandler_RevokeSession_OwnSession_RecordsSessionRevokeAuditEvent(t *testing.T) {
	t.Parallel()
	h, f, recorder := newAuditTestHandler(t)
	user := f.registerUser(t, "audit-revoke-one@example.com", testTenantA)
	current, err := f.svc.Login(t.Context(), LoginInput{Identifier: "audit-revoke-one@example.com", Password: testPassword, Device: "laptop", IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login(current) error = %v", err)
	}
	other, err := f.svc.Login(t.Context(), LoginInput{Identifier: "audit-revoke-one@example.com", Password: testPassword, Device: "phone", IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login(other) error = %v", err)
	}

	rec := doHandlerJSON(t, h, http.MethodDelete, "/api/v1/authn/sessions/"+other.Principal.SessionID, nil, principalFor(current))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusNoContent, rec.Body.String())
	}

	evt := findAuditEvent(t, recorder, AuditActionSessionRevoke)
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true")
	}
	if evt.Resource.Type != "session" || evt.Resource.ID != other.Principal.SessionID {
		t.Errorf("Resource = %+v, want {Type: session, ID: %s}", evt.Resource, other.Principal.SessionID)
	}
	if evt.Actor.ID != user.ID {
		t.Errorf("Actor.ID = %q, want %q", evt.Actor.ID, user.ID)
	}
}

// TestHandler_RevokeOtherSessions_RecordsSessionRevokeAuditEvent covers
// AuditActionSessionRevoke's revoke-others path, wired at
// AuthnRevokeOtherSessions but never previously observed landing on the bus
// (TestHandler_RevokeOtherSessions_KeepsCurrent uses the plain
// newTestHandler). This is also the one call site whose Resource carries a
// DisplayName ("other sessions", handler.go's own comment on why: the
// revoked count is already in the response, not the individual session
// ids), which this test pins explicitly.
func TestHandler_RevokeOtherSessions_RecordsSessionRevokeAuditEvent(t *testing.T) {
	t.Parallel()
	h, f, recorder := newAuditTestHandler(t)
	user := f.registerUser(t, "audit-revoke-others@example.com", testTenantA)
	current, err := f.svc.Login(t.Context(), LoginInput{Identifier: "audit-revoke-others@example.com", Password: testPassword, Device: "laptop", IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login(current) error = %v", err)
	}
	if _, err := f.svc.Login(t.Context(), LoginInput{Identifier: "audit-revoke-others@example.com", Password: testPassword, Device: "phone", IP: "203.0.113.9"}); err != nil {
		t.Fatalf("Login(other) error = %v", err)
	}

	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/sessions/revoke-others", nil, principalFor(current))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decodeBody[api.AuthnRevokeOtherSessionsResponse](t, rec)
	if resp.RevokedCount == nil || *resp.RevokedCount != 1 {
		t.Fatalf("RevokedCount = %v, want 1", resp.RevokedCount)
	}

	evt := findAuditEvent(t, recorder, AuditActionSessionRevoke)
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true")
	}
	if evt.Resource.Type != "session" || evt.Resource.ID != current.Principal.SessionID || evt.Resource.DisplayName != "other sessions" {
		t.Errorf("Resource = %+v, want {Type: session, ID: %s, DisplayName: %q}", evt.Resource, current.Principal.SessionID, "other sessions")
	}
	if evt.Actor.ID != user.ID {
		t.Errorf("Actor.ID = %q, want %q", evt.Actor.ID, user.ID)
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

	// A bare, un-stepped-up session (the shape a stolen access token has)
	// must be refused: this is the gap that let a stolen bare token
	// silently regenerate recovery codes for an already-active factor with
	// no re-proof at all.
	regenNoStepUp := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/recovery-codes/regenerate", nil, principal)
	if regenNoStepUp.Code != http.StatusForbidden {
		t.Fatalf("regenerate (no step-up) status = %d, want %d; body = %s", regenNoStepUp.Code, http.StatusForbidden, regenNoStepUp.Body.String())
	}
	if got := decodeAuthnError(t, regenNoStepUp).Code; got == nil || *got != ErrStepUpRequired.Code {
		t.Errorf("regenerate (no step-up) error code = %v, want %q", got, ErrStepUpRequired.Code)
	}

	// The step-up response's own Principal -- carrying mfa:totp in its amr
	// -- is what a legitimate caller would present next.
	steppedUp := &Principal{
		UserID:    deref(stepUpResp.Principal.UserID),
		TenantID:  pkgcore.TenantID(deref(stepUpResp.Principal.TenantID)),
		SessionID: deref(stepUpResp.Principal.SessionID),
		AMR:       *stepUpResp.Principal.Amr,
	}
	regen := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/recovery-codes/regenerate", nil, steppedUp)
	if regen.Code != http.StatusOK {
		t.Fatalf("regenerate status = %d, want %d; body = %s", regen.Code, http.StatusOK, regen.Body.String())
	}
	regenResp := decodeBody[api.AuthnRecoveryCodesResponse](t, regen)
	if regenResp.RecoveryCodes == nil || len(*regenResp.RecoveryCodes) != recoveryCodeCount {
		t.Fatalf("regenerated RecoveryCodes = %v, want %d codes", regenResp.RecoveryCodes, recoveryCodeCount)
	}
}

// TestHandler_EnrollTOTP_ReplacingActiveFactor_RequiresStepUp proves the
// enroll endpoint itself refuses to replace an already-ACTIVE TOTP factor
// for a bare, un-stepped-up principal -- the shape a stolen access token
// has. Before the fix, a bare token could call this endpoint to delete the
// victim's active factor and enroll an attacker-known secret in its place
// with no re-proof at all.
func TestHandler_EnrollTOTP_ReplacingActiveFactor_RequiresStepUp(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "mfa-hijack@example.com", testTenantA)
	pair, err := f.svc.Login(t.Context(), LoginInput{Identifier: "mfa-hijack@example.com", Password: testPassword, IP: "203.0.113.16"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}
	principal := principalFor(pair)

	enroll := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/totp/enroll", nil, principal)
	if enroll.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, want %d; body = %s", enroll.Code, http.StatusOK, enroll.Body.String())
	}
	enrollResp := decodeBody[api.AuthnEnrollTOTPResponse](t, enroll)
	code, err := totp.Code(*enrollResp.Secret, time.Now())
	if err != nil {
		t.Fatalf("compute a TOTP code: %v", err)
	}
	confirm := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/totp/confirm", api.AuthnConfirmTOTPRequest{Code: code}, principal)
	if confirm.Code != http.StatusOK {
		t.Fatalf("confirm status = %d, want %d; body = %s", confirm.Code, http.StatusOK, confirm.Body.String())
	}

	// The factor is now ACTIVE. A bare session enrolling again -- exactly
	// the request an attacker holding a stolen access token would send --
	// must be refused rather than silently replacing it.
	reEnroll := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/totp/enroll", nil, principal)
	if reEnroll.Code != http.StatusForbidden {
		t.Fatalf("re-enroll (no step-up) status = %d, want %d; body = %s", reEnroll.Code, http.StatusForbidden, reEnroll.Body.String())
	}
	if got := decodeAuthnError(t, reEnroll).Code; got == nil || *got != ErrStepUpRequired.Code {
		t.Errorf("re-enroll (no step-up) error code = %v, want %q", got, ErrStepUpRequired.Code)
	}

	// A caller that HAS completed step-up may still replace the factor
	// (e.g. to switch authenticator apps).
	stepUpCode, err := totp.Code(*enrollResp.Secret, time.Now().Add(totp.Period))
	if err != nil {
		t.Fatalf("compute a step-up TOTP code: %v", err)
	}
	stepUp := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/step-up", api.AuthnVerifyStepUpRequest{Code: stepUpCode}, principal)
	if stepUp.Code != http.StatusOK {
		t.Fatalf("step-up status = %d, want %d; body = %s", stepUp.Code, http.StatusOK, stepUp.Body.String())
	}
	stepUpResp := decodeBody[api.AuthnTokenPair](t, stepUp)
	steppedUp := &Principal{
		UserID:    deref(stepUpResp.Principal.UserID),
		TenantID:  pkgcore.TenantID(deref(stepUpResp.Principal.TenantID)),
		SessionID: deref(stepUpResp.Principal.SessionID),
		AMR:       *stepUpResp.Principal.Amr,
	}
	reEnrollStepped := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/mfa/totp/enroll", nil, steppedUp)
	if reEnrollStepped.Code != http.StatusOK {
		t.Fatalf("re-enroll (with step-up) status = %d, want %d; body = %s", reEnrollStepped.Code, http.StatusOK, reEnrollStepped.Body.String())
	}
	reEnrollResp := decodeBody[api.AuthnEnrollTOTPResponse](t, reEnrollStepped)
	if reEnrollResp.Secret == nil || *reEnrollResp.Secret == *enrollResp.Secret {
		t.Errorf("re-enroll (with step-up) secret = %v, want a fresh one distinct from %q", reEnrollResp.Secret, *enrollResp.Secret)
	}
}

// TestHandler_PreAuthCookie_SecureAttribute pins the Secure flag on the
// pre-authentication cookie across the three topologies that matter.
//
// r.TLS is nil for every request in the most common production topology --
// TLS terminated at a reverse proxy, the Go process only ever seeing
// plaintext HTTP -- so a cookie whose Secure flag follows r.TLS alone ships
// without the attribute exactly there (the reported gap). The host knows its
// own topology and forces the attribute through WithSecureCookies; a request
// that did arrive over direct TLS still gets it without the option, and a
// plaintext listener (local development) with no option keeps issuing an
// insecure cookie as before.
func TestHandler_PreAuthCookie_SecureAttribute(t *testing.T) {
	t.Parallel()

	// issue mints a fresh pre-auth cookie from a Handler whose Service was
	// assembled with opts, over a request whose TLS state is tlsState, and
	// returns the cookie the response set. A nil r.TLS is the shape of
	// every request behind a TLS-terminating proxy.
	issue := func(t *testing.T, opts []Option, tlsState *tls.ConnectionState) *http.Cookie {
		t.Helper()
		h, _ := newTestHandler(t, opts...)
		req := httptest.NewRequest(http.MethodGet, "/api/v1/authn/social/google/authorize", nil)
		req.TLS = tlsState
		rec := httptest.NewRecorder()
		if _, err := h.ensurePreAuthCookie(rec, req); err != nil {
			t.Fatalf("ensurePreAuthCookie() error = %v", err)
		}
		cookies := rec.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("Set-Cookie count = %d, want exactly 1; headers = %v", len(cookies), rec.Header())
		}
		return cookies[0]
	}

	t.Run("plaintext behind a TLS-terminating proxy, host opted in", func(t *testing.T) {
		t.Parallel()
		cookie := issue(t, []Option{WithSecureCookies(true)}, nil)
		if !cookie.Secure {
			t.Error("cookie Secure = false, want true: behind a TLS-terminating proxy r.TLS is nil, so WithSecureCookies(true) is the only thing that can set it")
		}
	})
	t.Run("direct TLS without the option", func(t *testing.T) {
		t.Parallel()
		cookie := issue(t, nil, &tls.ConnectionState{})
		if !cookie.Secure {
			t.Error("cookie Secure = false, want true for a request that did arrive over TLS")
		}
	})
	t.Run("plaintext without the option", func(t *testing.T) {
		t.Parallel()
		cookie := issue(t, nil, nil)
		if cookie.Secure {
			t.Error("cookie Secure = true, want false for a plaintext request with no WithSecureCookies: local development runs over http")
		}
	})
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
