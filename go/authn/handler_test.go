package authn

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn/api"
	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/authn/internal/totp"
	"github.com/vislake/speed/go/dbkit/audit"
	obs "github.com/vislake/speed/go/observability"
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
// pins decodeJSON's answer for a malformed request body -- returned by
// every one of this module's operations that reads one, register included.
// The answer must be a cataloged error: an uncataloged code would give a
// client no locale text to localize, only a raw key to render. The answer
// is ErrInvalidRequestBody (errors.go), cataloged and bilingually rendered
// like every other coded error this module returns.
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

// TestHandler_Register_OversizedBody_RefusedWithInvalidRequestBody pins
// the MaxBytesReader bound: an unauthenticated register (or login) endpoint
// must not read an arbitrarily large body in full -- unbounded buffering of
// an attacker's payload before any validation has run -- nor let an
// over-width display name reach the database. The body below is valid JSON
// whose display_name field alone exceeds the byte bound; every other field
// is within policy, so the ONLY thing that can refuse it is the body bound,
// and the refusal must surface as the catalogued ErrInvalidRequestBody
// rather than a successful account creation.
func TestHandler_Register_OversizedBody_RefusedWithInvalidRequestBody(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t)

	var body strings.Builder
	body.WriteString(`{"email":"oversized@example.com","password":"aaaaaaaaaaaa","display_name":"`)
	body.WriteString(strings.Repeat("x", maxRequestBodyBytes+1))
	body.WriteString(`"}`)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/register", strings.NewReader(body.String()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (an over-bound body must be refused); body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	errBody := decodeAuthnError(t, rec)
	if errBody.Code == nil || *errBody.Code != ErrInvalidRequestBody.Code {
		t.Errorf("error code = %v, want %s", errBody.Code, ErrInvalidRequestBody.Code)
	}

	// Nothing was created: the same email still registers afterwards.
	rec = doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/register", api.AuthnRegisterRequest{
		Email: strPtr("oversized@example.com"), Password: testPassword,
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Errorf("register after the refused oversized body = %d, want %d (the oversized request must not have created an account)", rec.Code, http.StatusCreated)
	}
}

// TestHandler_Register_DisplayNameLengthIsValidated pins the display-name
// half of the body bound: the schema declares no maxLength for display_name,
// but the users.display_name column the value lands in is VARCHAR(128) --
// the module's displayNameWidth constant, enforced by PostgreSQL and
// ignored by SQLite -- so an over-width name must be refused here with the
// catalogued ErrDisplayNameTooLong, and a name exactly at the width must
// still be accepted.
func TestHandler_Register_DisplayNameLengthIsValidated(t *testing.T) {
	t.Parallel()

	t.Run("one rune over the column width is refused", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHandler(t)
		rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/register", api.AuthnRegisterRequest{
			Email:       strPtr("longname@example.com"),
			Password:    testPassword,
			DisplayName: strPtr(strings.Repeat("n", displayNameWidth+1)),
		}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
		}
		errBody := decodeAuthnError(t, rec)
		if errBody.Code == nil || *errBody.Code != ErrDisplayNameTooLong.Code {
			t.Errorf("error code = %v, want %s", errBody.Code, ErrDisplayNameTooLong.Code)
		}
	})

	t.Run("exactly the column width is accepted", func(t *testing.T) {
		t.Parallel()
		h, _ := newTestHandler(t)
		rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/register", api.AuthnRegisterRequest{
			Email:       strPtr("boundary@example.com"),
			Password:    testPassword,
			DisplayName: strPtr(strings.Repeat("n", displayNameWidth)),
		}, nil)
		if rec.Code != http.StatusCreated {
			t.Errorf("status = %d, want %d (a display name exactly at users.display_name's width must be accepted); body = %s", rec.Code, http.StatusCreated, rec.Body.String())
		}
	})
}

// TestHandler_EnsurePreAuthCookie_MaxAgeTracksConfiguredStateTTL pins the
// cookie's Max-Age to the state TTL: the cookie accompanies a state record
// issued with the CONFIGURED cfg.oauthStateTTL, so a Max-Age that
// hard-coded the default would give a host that raised the TTL for slow
// identity providers a cookie that dies before its state -- stranding the
// callback without its binding. With the state TTL configured above the
// default, the minted cookie must carry that longer Max-Age.
func TestHandler_EnsurePreAuthCookie_MaxAgeTracksConfiguredStateTTL(t *testing.T) {
	t.Parallel()

	const configuredTTL = 25 * time.Minute
	h, _ := newTestHandler(t, WithOAuthStateTTL(configuredTTL))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/authn/social/google/authorize", nil)
	rec := httptest.NewRecorder()
	if _, err := h.ensurePreAuthCookie(rec, req); err != nil {
		t.Fatalf("ensurePreAuthCookie() error = %v", err)
	}

	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == preAuthCookieName {
			cookie = c
			break
		}
	}
	if cookie == nil {
		t.Fatal("no pre-auth cookie was set")
	}
	if want := int(configuredTTL / time.Second); cookie.MaxAge != want {
		t.Errorf("pre-auth cookie Max-Age = %d, want %d (the configured state TTL, not DefaultOAuthStateTTL's 600)", cookie.MaxAge, want)
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
// Action matches, failing the test when none is found -- the audit-record
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

// auditTenantIs asserts that evt's tenant_id equals want -- the
// audit-record tenant assertions' shared helper. authn's routes are not
// downstream
// of tenancy.Middleware, so the tenant on every audit row it records comes
// from the call site itself (recordAudit's tenant argument, handler.go);
// these assertions pin that each site really names the tenant it decided
// on, rather than leaving the row tenant-less.
func auditTenantIs(t *testing.T, evt audit.RecordedEvent, want pkgcore.TenantID) {
	t.Helper()
	if evt.TenantID != string(want) {
		t.Errorf("TenantID = %q, want %q", evt.TenantID, string(want))
	}
}

// auditTenantIsEmpty is auditTenantIs's counterpart for the sites that
// deliberately stamp no tenant -- an account-level event recorded at an
// unauthenticated callback (a social bind), a failed sign-in, and a
// registration whose caller was anonymous. Empty is the fail-closed answer
// there: the tenant a pre-auth request merely asserts is not an
// attestation, and an unauthenticated caller must not be able to stamp rows
// into a tenant's ledger by naming it. A registration whose caller WAS
// authenticated carries the caller's attested tenant instead
// (TestHandler_Register_AuthenticatedCaller_AuditRowCarriesTheAttestedTenant).
func auditTenantIsEmpty(t *testing.T, evt audit.RecordedEvent) {
	t.Helper()
	if evt.TenantID != "" {
		t.Errorf("TenantID = %q, want empty: no tenant is attested for this event", evt.TenantID)
	}
}

// TestHandler_LoginWithPassword_ValidCredentials_RecordsLoginAuditEvent
// pins the login-success audit record: the declared audit actions are only
// declarations until a real call site emits, so the assertion here proves a
// real password sign-in leaves an AuditEvent behind.
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
	// A human subject's audit record must carry the account's display name,
	// resolved from the users table at record time (recordAudit's own doc
	// comment in handler.go). An empty Actor.DisplayName means the trail can
	// no longer say who this actor was once the account is renamed or
	// deleted -- the very readability pkgcore.Actor.DisplayName exists for.
	if evt.Actor.DisplayName != user.DisplayName {
		t.Errorf("Actor.DisplayName = %q, want %q (the registered account's own display name)",
			evt.Actor.DisplayName, user.DisplayName)
	}
	// The new session resolved tenant A (the caller's only membership),
	// so the login record must carry it: tenant-scoped audit reads
	// (audit.Repository.ListByTenant) filter on tenant_id, and authn's
	// routes carry no ambient tenant for Emit to read.
	auditTenantIs(t, evt, testTenantA)
}

// TestHandler_LoginWithPassword_WrongPassword_RecordsLoginFailureAuditEvent
// pins the login-failure audit record: a failed sign-in attempt is exactly
// as security-relevant as a successful one and must leave the same kind of
// record behind.
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
	// A failed sign-in is a pre-auth event: no tenant is attested, and
	// stamping the tenant this request's own body asserted would let an
	// unauthenticated caller write rows into any tenant's ledger.
	auditTenantIsEmpty(t, evt)
}

// TestHandler_LoginWithPassword_Locked_Returns429WithRetryAfter is the
// HTTP-translation proof: ratelimit.go's progressive lockout is
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
	h, f := newTestHandler(t, WithSMSSender(pkgcore.NewConsoleSMSSender(&out)))
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

// TestHandler_RequestSMSCode_GatewayDown_RegisteredPhoneStillAnswers202
// pins the response-status layer of the oracle: a registered phone whose
// SMS gateway failed must not answer 500 while an unregistered phone
// answers 202 under the same outage -- a response-status split would tell
// an attacker a number is registered exactly when the platform can least
// afford to admit it. The gateway failure is logged server-side and both
// requests answer 202 with an empty body.
func TestHandler_RequestSMSCode_GatewayDown_RegisteredPhoneStillAnswers202(t *testing.T) {
	t.Parallel()

	h, f := newTestHandler(t, WithSMSSender(failingSMSSender{}))
	f.registerUser(t, "smsoracle@example.com", testTenantA)
	if err := f.svc.Users().Save(t.Context(), mustSetPhone(t, f, "smsoracle@example.com", "+15550000009")); err != nil {
		t.Fatalf("save the registered phone: %v", err)
	}

	known := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/login/sms/request", api.AuthnRequestSMSCodeRequest{Phone: "+15550000009"}, nil)
	if known.Code != http.StatusAccepted {
		t.Fatalf("registered-phone-under-gateway-outage status = %d, want %d; body = %s", known.Code, http.StatusAccepted, known.Body.String())
	}
	if known.Body.Len() != 0 {
		t.Errorf("registered-phone response body = %q, want empty (an error envelope would disclose the failure)", known.Body.String())
	}

	unknown := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/login/sms/request", api.AuthnRequestSMSCodeRequest{Phone: "+15559999999"}, nil)
	if unknown.Code != http.StatusAccepted {
		t.Fatalf("unknown-phone status = %d, want %d (must equal the registered phone's answer)", unknown.Code, http.StatusAccepted)
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
	h, f, recorder := newAuditTestHandler(t, WithSMSSender(pkgcore.NewConsoleSMSSender(&out)))
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
	// The SMS sign-in's audit record is this module's login action with the
	// tenant the new session resolved, exactly like the password leg --
	// pinned here because this call site is otherwise unpinned.
	evt := findAuditEvent(t, recorder, AuditActionUserLogin)
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true")
	}
	auditTenantIs(t, evt, testTenantA)
}

// smsCodeRunPattern matches a maximal run of ASCII digits, so
// extractSMSCode can find the one run that is EXACTLY smsCodeDigits long --
// the code itself -- rather than the first digit it sees, which the console
// sender's "SMS to <phone>: <text>" record framing (pkgcore's sms.go) puts
// a PHONE NUMBER'S digits before the code.
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

// TestHandler_Logout_ValidPrincipal_RecordsSessionRevokeAuditEvent pins the
// "session revocation" representative: AuditActionSessionRevoke must be
// emitted for the module's three revoke paths (logout, revoke-one,
// revoke-others), and this test covers the logout path.
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
	// The acting principal's tenant claim -- authn's only source of a
	// tenant on its own routes (recordAudit's tenant argument).
	auditTenantIs(t, evt, pair.Principal.TenantID)
}

// TestHandler_NilBus_LogsTheInoperativeAuditStateOnce pins recordAudit's
// nil-bus branch against being quieter than its Emit-failure branch: an
// Emit that fails mid-publish logs at Error, while a Handler constructed
// without a bus -- a permanent state in which NONE of the module's declared
// audit actions will ever be recorded -- must not return silently on every
// audited operation, or a host that mis-wired its Handler by hand would
// never hear about it. The inoperative state is announced once per Handler,
// at Error level, on the first audited operation (see recordAudit and
// nilBusWarned).
//
// The deliberate once-per-Handler shape is itself pinned here: a second
// audited operation on the same bus-less Handler must not add a second
// line, since the NewHandler contract sanctions a bus-less construction and
// per-operation Error lines would drown the operator who chose it.
func TestHandler_NilBus_LogsTheInoperativeAuditStateOnce(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	h := NewHandler(f.svc, nil, nil)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	register := func(email string) {
		t.Helper()
		raw, err := json.Marshal(api.AuthnRegisterRequest{Email: strPtr(email), Password: testPassword})
		if err != nil {
			t.Fatalf("marshal register body: %v", err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/register", bytes.NewReader(raw))
		req = req.WithContext(obs.WithLogger(req.Context(), logger))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
		}
	}

	// Two audited operations on the same bus-less Handler: the first
	// announces the inoperative state, the second must stay quiet.
	register("nil-bus@example.com")
	register("nil-bus-2@example.com")

	const wantLine = "authn audit recording is inoperative"
	if got := strings.Count(buf.String(), wantLine); got != 1 {
		t.Fatalf("the inoperative-audit line appeared %d time(s) after two audited operations on a nil-bus Handler, want exactly 1; log:\n%s", got, buf.String())
	}
}

// The tests below extend the audit-record proofs beyond the actions the
// tests above verify (AuditActionUserLogin, and AuditActionSessionRevoke's
// logout path). Each drives the real HTTP path that calls recordAudit and
// asserts on the recorded event's Action, Resource, Actor and Result --
// exactly as the ones above do -- so a wrong resource id, actor or action
// string at any of these call sites fails a test rather than going
// unnoticed.

// TestHandler_Register_ValidBody_RecordsUserRegisterAuditEvent pins
// AuditActionUserRegister's emission on AuthnRegister's HTTP path
// (TestHandler_Register_ValidBody_ReturnsCreatedUser uses the plain
// newTestHandler, which discards its EventRecorder).
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
	// Registration is pre-tenant, and THIS caller is anonymous -- no
	// Principal is present, so no tenant is attested and the row carries
	// none. An authenticated register caller's attested tenant is a
	// different case: TestHandler_Register_AuthenticatedCaller_AuditRowCarriesTheAttestedTenant
	// pins what this row must do when one is present.
	auditTenantIsEmpty(t, evt)
}

// TestHandler_Register_AuthenticatedCaller_AuditRowCarriesTheAttestedTenant
// pins the register audit row's tenant when the caller arrived
// authenticated: the acting Principal's TenantID claim is the one tenant
// the request actually attested, and the row records it -- so tenant X's
// own audit reader sees that an account creation was initiated by one of
// X's members. A row that hard-coded an empty tenant would drop that
// attestation, while the same request's authn.user.created event stays
// tenant-less (Service.Register publishes it through the tenant-less
// funnel, whatever the context holds): the row is this handler's request
// ledger, not the account's seat. The multi-account-per-person shape is
// what keeps the register itself succeeding for an authenticated caller
// (the attested tenant is recorded, never acted on: the event stays
// tenant-less and the account keeps no membership in the caller's tenant).
func TestHandler_Register_AuthenticatedCaller_AuditRowCarriesTheAttestedTenant(t *testing.T) {
	t.Parallel()
	h, _, recorder := newAuditTestHandler(t)

	attested := &Principal{UserID: "attested-caller", TenantID: testTenantA, SessionID: "session-1"}
	rec := doHandlerJSON(t, h, http.MethodPost, "/api/v1/authn/register", api.AuthnRegisterRequest{
		Email: strPtr("attested-register@example.com"), Password: testPassword, DisplayName: strPtr("Attested Register"),
	}, attested)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d: registration is a public pre-tenant operation and must succeed for an "+
			"authenticated caller too (the multi-account-per-person shape); body = %s",
			rec.Code, http.StatusCreated, rec.Body.String())
	}
	resp := decodeBody[api.AuthnUser](t, rec)
	if resp.ID == nil || *resp.ID == "" {
		t.Fatal("response ID is missing")
	}

	evt := findAuditEvent(t, recorder, AuditActionUserRegister)
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true")
	}
	if evt.Actor.ID != *resp.ID {
		t.Errorf("Actor.ID = %q, want %q", evt.Actor.ID, *resp.ID)
	}
	auditTenantIs(t, evt, testTenantA)
}

// TestHandler_SwitchTenant_ActiveMember_RecordsTenantSwitchAuditEvent pins
// AuditActionTenantSwitch's emission at AuthnSwitchTenant's HTTP path
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
	// The row is stamped with the tenant the session acted in BEFORE the
	// switch (the acting principal's claim, tenant A), not the switch's
	// destination B -- the choice handler.go's recordAudit doc comment
	// records for this site.
	auditTenantIs(t, evt, testTenantA)
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
	// The bind completes at an unauthenticated callback (the signed-in
	// authorize step's state is what authenticates it), so the caller's
	// tenant is not attested at recording time and the row carries none.
	auditTenantIsEmpty(t, evt)

	// The EventIdentityBound the same flow announces is pinned empty for
	// the same reason: the binding is an account-level fact recorded at a
	// pre-auth callback where no tenant is attested -- the mirror of the
	// audit row above. Only an operation that can attest a tenant (the
	// session events, from the session row; the protected user events, from
	// the layered principal) stamps one.
	boundEvent, ok := f.events.First(EventIdentityBound)
	if !ok {
		t.Fatalf("no %s event was published", EventIdentityBound)
	}
	if boundEvent.TenantID != "" {
		t.Errorf("%s TenantID = %q, want empty: a binding made at an unauthenticated callback must not be stamped into a tenant by the request's own assertion", EventIdentityBound, boundEvent.TenantID)
	}
}

// TestHandler_UnbindIdentity_OwnedBySelfWithPasswordRemaining_RecordsIdentityUnbindAuditEvent
// pins AuditActionIdentityUnbind's emission at AuthnUnbindIdentity
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
	auditTenantIs(t, evt, testTenantA)

	// The business event the same protected operation announces must carry
	// the same tenant as its audit row: the handler layers the acting
	// principal's tenant onto the ctx it hands the service, and
	// Service.publish reads it back. An empty TenantID would strand a
	// tenant-scoped subscriber that must route on the event's tenant.
	unboundEvent, ok := f.events.First(EventIdentityUnbound)
	if !ok {
		t.Fatalf("no %s event was published", EventIdentityUnbound)
	}
	if unboundEvent.TenantID != testTenantA {
		t.Errorf("%s TenantID = %q, want %q (the acting principal's tenant)", EventIdentityUnbound, unboundEvent.TenantID, testTenantA)
	}
}

// TestHandler_ConfirmTOTP_ValidCode_RecordsMFAEnrollAuditEvent pins
// AuditActionMFAEnroll's emission on AuthnConfirmTOTP's HTTP path
// (TestHandler_MFAEnrollConfirmStepUp_FullRoundTrip uses the plain
// newTestHandler).
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
	auditTenantIs(t, evt, testTenantA)

	// The business event the same protected operation announces must carry
	// the same tenant as its audit row; an empty TenantID would strand a
	// tenant-scoped subscriber that must route on the event's tenant.
	enrolled, ok := f.events.First(EventMFAEnrolled)
	if !ok {
		t.Fatalf("no %s event was published", EventMFAEnrolled)
	}
	if enrolled.TenantID != testTenantA {
		t.Errorf("%s TenantID = %q, want %q (the acting principal's tenant)", EventMFAEnrolled, enrolled.TenantID, testTenantA)
	}
}

// TestHandler_RegenerateRecoveryCodes_SteppedUp_RecordsMFARecoveryCodesRegenerateAuditEvent
// pins AuditActionMFARecoveryCodesRegenerate's emission on
// AuthnRegenerateRecoveryCodes' HTTP path.
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
	auditTenantIs(t, evt, testTenantA)

	// The business event the same protected operation announces must carry
	// the same tenant as its audit row; an empty TenantID would strand a
	// tenant-scoped subscriber that must route on the event's tenant.
	regenerated, ok := f.events.First(EventMFARecoveryCodesRegenerated)
	if !ok {
		t.Fatalf("no %s event was published", EventMFARecoveryCodesRegenerated)
	}
	if regenerated.TenantID != testTenantA {
		t.Errorf("%s TenantID = %q, want %q (the acting principal's tenant)", EventMFARecoveryCodesRegenerated, regenerated.TenantID, testTenantA)
	}
}

// TestHandler_RevokeSession_OwnSession_RecordsSessionRevokeAuditEvent pins
// AuditActionSessionRevoke's revoke-one path at AuthnRevokeSession
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
	auditTenantIs(t, evt, testTenantA)
}

// TestHandler_RevokeOtherSessions_RecordsSessionRevokeAuditEvent pins
// AuditActionSessionRevoke's revoke-others path at AuthnRevokeOtherSessions
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
	auditTenantIs(t, evt, testTenantA)
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
// is carried onto the second request. It proves that the cookie-derived
// SessionBinding actually gates the callback end to end, through the HTTP
// surface rather than by calling SocialAuthorizeURL and SocialCallback
// directly the way identity_test.go's socialSignIn does.
func TestHandler_SocialSignIn_FullRoundTrip(t *testing.T) {
	t.Parallel()

	// The callback auto-links to a PRE-REGISTERED account (verified email,
	// trusted provider) rather than letting the flow JIT-provision a brand
	// new one: a freshly provisioned account has no tenant membership yet
	// (membership is org's own concern, which authn never grants on its own)
	// and would fail to mint a session for exactly that reason, which is not
	// what this test is proving.
	provider := &stubProvider{name: ProviderGoogle, identity: &ExternalIdentity{
		ExternalID: "ext-1", Email: "social@example.com", EmailVerified: true, Name: "Social Person",
	}}
	allowlist, err := NewRedirectAllowlist(testRedirectURI)
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	h, f, recorder := newAuditTestHandler(t, WithSocialProviders(provider), WithRedirectAllowlist(allowlist), WithTrustedProviders(ProviderGoogle))
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
	// A social sign-in starts a real session, so its audit record is the
	// login action stamped with the tenant that session resolved -- this
	// callback branch (the non-Bound one) is otherwise unpinned.
	evt := findAuditEvent(t, recorder, AuditActionUserLogin)
	if !evt.Result.Success {
		t.Errorf("Result.Success = false, want true")
	}
	auditTenantIs(t, evt, testTenantA)
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
	// must be refused: without the step-up gate, a stolen bare token could
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
// has. Without the step-up gate, a bare token could delete the victim's
// active factor and enroll an attacker-known secret in its place with no
// re-proof at all.
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
// without the attribute exactly there. The host knows its own topology and
// forces the attribute through WithSecureCookies; a request that did arrive
// over direct TLS still gets it without the option, and a plaintext
// listener (local development) with no option keeps issuing an insecure
// cookie.
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

func TestHandler_ListSessions_ExposesSessionExpiry(t *testing.T) {
	t.Parallel()
	// Every listed row must carry its stored expires_at: the sessions
	// list is what tells an expired session (expiry is checked at use
	// time and never written back to the row, so an active row can have
	// an expires_at in the past) from a live device. A response that
	// drops the field would render such a session as a live device
	// forever.
	h, f := newTestHandler(t)
	f.registerUser(t, "expiry@example.com", testTenantA)
	login, err := f.svc.Login(t.Context(), LoginInput{Identifier: "expiry@example.com", Password: testPassword, Device: "laptop", IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login() error = %v", err)
	}

	rows, err := f.svc.ListSessions(t.Context(), login.Principal.UserID)
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	expiryByID := make(map[string]time.Time, len(rows))
	for _, row := range rows {
		expiryByID[row.ID] = row.ExpiresAt
	}

	rec := doHandlerJSON(t, h, http.MethodGet, "/api/v1/authn/sessions", nil, principalFor(login))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	resp := decodeBody[api.AuthnListSessionsResponse](t, rec)
	if resp.Sessions == nil || len(*resp.Sessions) != len(rows) {
		t.Fatalf("Sessions = %v, want %d rows", resp.Sessions, len(rows))
	}
	for _, s := range *resp.Sessions {
		if s.ID == nil {
			t.Errorf("a session row carries no id")
			continue
		}
		stored, ok := expiryByID[*s.ID]
		if !ok {
			t.Errorf("response carries a session (%s) the service did not list", *s.ID)
			continue
		}
		if s.ExpiresAt == nil {
			t.Errorf("session %s carries no expires_at, want %v", *s.ID, stored)
			continue
		}
		if !stored.Equal(*s.ExpiresAt) {
			t.Errorf("session %s expires_at = %v, want the stored %v", *s.ID, *s.ExpiresAt, stored)
		}
	}
}

// TestHandler_ListSessions_TieredRevokeReasonExport is the wire-level proof
// of the tiered revoke-reason export contract. Three sessions of one
// account end by the three routes a revoked row can take: one is killed by
// a GENUINE replay (the refresh token is consumed by a normal refresh, then
// the consumed token is presented again -- the detection path, not a direct
// revoke call), one by the owner signing that device out (Service.Logout,
// the same call the logout endpoint makes), and the viewer's own session
// stays active. Listing sessions as the viewer must then show the tiered
// projection: the logout reason exports verbatim ("logout"); the replay
// reason NEVER exports as itself -- it folds into the schema's single
// generic value ("security_revoked"), so the response bytes cannot even
// contain "replay_detected"; and the active session carries no
// revoke_reason at all. The same test re-proves the storage half of the
// contract: the session rows themselves -- the in-process read of the same
// data -- keep the real reasons verbatim, replay_detected included,
// because the fold is an API-projection-only change and the column remains
// the forensics record.
func TestHandler_ListSessions_TieredRevokeReasonExport(t *testing.T) {
	t.Parallel()
	h, f := newTestHandler(t)
	f.registerUser(t, "tiered@example.com", testTenantA)

	viewer, err := f.svc.Login(t.Context(), LoginInput{Identifier: "tiered@example.com", Password: testPassword, Device: "laptop", IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login(viewer) error = %v", err)
	}
	replayed, err := f.svc.Login(t.Context(), LoginInput{Identifier: "tiered@example.com", Password: testPassword, Device: "phone", IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login(replayed) error = %v", err)
	}
	loggedOut, err := f.svc.Login(t.Context(), LoginInput{Identifier: "tiered@example.com", Password: testPassword, Device: "tablet", IP: "203.0.113.9"})
	if err != nil {
		t.Fatalf("Login(loggedOut) error = %v", err)
	}

	// The phone session's thief replays its consumed refresh token: the
	// first refresh rotates the family, the second presents the
	// now-consumed token again -- the module's actual detection path.
	_, err = f.svc.Refresh(t.Context(), replayed.RefreshToken)
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	_, err = f.svc.Refresh(t.Context(), replayed.RefreshToken)
	if !hasCode(err, ErrRefreshTokenReused.Code) {
		t.Fatalf("replayed Refresh() error = %v, want code %q", err, ErrRefreshTokenReused.Code)
	}
	// The tablet's owner signs it out from the device itself.
	if err = f.svc.Logout(t.Context(), loggedOut.Principal.SessionID); err != nil {
		t.Fatalf("Logout() error = %v", err)
	}

	// Storage is untouched by the fold: the rows this handler projects
	// from still carry the real reasons, replay_detected verbatim.
	stored, err := f.svc.ListSessions(t.Context(), viewer.Principal.UserID)
	if err != nil {
		t.Fatalf("ListSessions() error = %v", err)
	}
	storedReason := make(map[string]string, len(stored))
	for _, row := range stored {
		storedReason[row.ID] = row.RevokeReason
	}
	if got := storedReason[replayed.Principal.SessionID]; got != RevokeReasonReplay {
		t.Errorf("stored revoke reason of the replay-revoked session = %q, want %q verbatim", got, RevokeReasonReplay)
	}
	if got := storedReason[loggedOut.Principal.SessionID]; got != RevokeReasonLogout {
		t.Errorf("stored revoke reason of the logged-out session = %q, want %q", got, RevokeReasonLogout)
	}

	rec := doHandlerJSON(t, h, http.MethodGet, "/api/v1/authn/sessions", nil, principalFor(viewer))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("replay_detected")) {
		t.Error("the response carries the stored reason replay_detected; the security-class reason must fold on the wire")
	}
	resp := decodeBody[api.AuthnListSessionsResponse](t, rec)
	if resp.Sessions == nil || len(*resp.Sessions) != 3 {
		t.Fatalf("Sessions = %v, want 3 rows", resp.Sessions)
	}
	for _, s := range *resp.Sessions {
		if s.ID == nil {
			t.Error("a session row carries no id")
			continue
		}
		switch *s.ID {
		case replayed.Principal.SessionID:
			if s.RevokeReason == nil || *s.RevokeReason != api.SecurityRevoked {
				t.Errorf("replay-revoked session revoke_reason = %v, want the folded generic %q", s.RevokeReason, api.SecurityRevoked)
			}
		case loggedOut.Principal.SessionID:
			if s.RevokeReason == nil || *s.RevokeReason != api.Logout {
				t.Errorf("logged-out session revoke_reason = %v, want %q exported as itself", s.RevokeReason, api.Logout)
			}
		case viewer.Principal.SessionID:
			if s.RevokeReason != nil {
				t.Errorf("active session revoke_reason = %q, want it absent", *s.RevokeReason)
			}
		default:
			t.Errorf("response carries a session (%s) this test never created", *s.ID)
		}
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
// deployment mode -- the SMS sender (pkgcore.NewConsoleSMSSender for the
// standalone deployment mode, pkgcore.NewHTTPSMSSender for the distributed
// one, see module.go's WithDeploymentMode and
// ErrMissingDistributedSMSSender) -- must produce identical outcomes: the
// same status codes and the same error codes at every step. Token values
// themselves are never compared -- they are randomly generated on both
// wirings by design -- only the observable behaviour a caller sees.
func TestHandler_DeploymentModeConsistency_SMSFlow(t *testing.T) {
	t.Parallel()

	var consoleOut bytes.Buffer
	standalone, standaloneFixture := newTestHandler(t, WithSMSSender(pkgcore.NewConsoleSMSSender(&consoleOut)))

	// TLS, not plaintext: the HTTP SMS sender refuses a plaintext gateway
	// endpoint before any request (see pkgcore's own sms_test.go,
	// TestHTTPSMSSender_PlaintextEndpoint_RefusedBeforeAnyRequest), so the
	// distributed leg's gateway must be a TLS test server for the flow to
	// exercise a real delivery.
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(gateway.Close)
	distributed, distributedFixture := newTestHandler(t,
		// The default client is SSRF-guarded (pkgcore/safehttp), which
		// correctly refuses this httptest server's loopback address; a
		// real deployment's gateway is a public endpoint, so this
		// override -- like WithFederationHTTPClient's identical test-only
		// use elsewhere in this package -- is test-only. Replacing the
		// client replaces only that dial-time address guard, never the
		// endpoint's https requirement (see pkgcore.NewHTTPSMSSender).
		WithSMSSender(pkgcore.NewHTTPSMSSender(gateway.URL, pkgcore.WithHTTPSMSSenderClient(gateway.Client()))),
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

// ---------------------------------------------------------------------------
// Trusted-proxy client-IP derivation (handler.go's clientIP). Header
// reading is gated on a host-declared trusted-proxy list
// (WithTrustedProxies): with the proxies declared, a request from one of
// them records the forwarded client address; without them (or from a peer
// that is not one), a request carrying spoofed forwarding headers still
// records its connection address. The derivation tests below exercise
// clientIP directly; the flow tests drive whole sign-in and registration
// paths and read the recorded rows and responses back.
//
// WHICH headers may be read is host-declared per header: X-Forwarded-For --
// the self-protecting chain walk, whose rightmost entry a trusted proxy
// itself appended -- is the one forwarding header read under the
// trusted-peer gate alone, while a single-hop vendor header (Fly-Client-IP)
// is read ONLY when the host opted into that specific header
// (WithVendorClientIPHeaders) for a deployment whose proxy genuinely
// overwrites it on every request. Reading a vendor header for ANY declared
// proxy would hand the client the recorded and rate-limited address through
// any generic reverse proxy that forwards unknown headers verbatim (nginx,
// ALB, Envoy, Cloudflare). The XFF cases below pin the default shape; the
// vendor-header cases have tests of their own.

// signInFrom issues a password sign-in for identifier on h over a request
// whose direct connection address is peer (r.RemoteAddr) and which carries
// the given headers -- the request shape clientIP's trusted-proxy gate
// decides on. doHandlerJSON cannot produce one: httptest.NewRequest always
// originates from its own fixed 192.0.2.1 address.
func signInFrom(h *Handler, identifier, peer string, headers [][2]string) *httptest.ResponseRecorder {
	body, err := json.Marshal(api.AuthnLoginWithPasswordRequest{
		Identifier: identifier,
		Password:   testPassword,
	})
	if err != nil {
		panic(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", bytes.NewReader(body))
	req.RemoteAddr = peer
	for _, kv := range headers {
		req.Header.Set(kv[0], kv[1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestHandler_ClientIP_DeclaredProxyXForwardedFor_ResolvesTheRealClient
// pins the X-Forwarded-For half of the trusted-proxy shape at the
// derivation level: with the proxies declared
// (WithTrustedProxies("203.0.113.0/24")), a request whose direct peer is
// one of them carries the real client in the chain the proxy appended to
// X-Forwarded-For, and clientIP must return that address -- not the proxy
// itself. The chain is walked from the right, stripping entries that name
// declared proxies, so a client's own spoofed prefix entries can never
// displace the address the trusted proxy appended. X-Forwarded-For is read
// under the trusted-peer gate alone, no per-header host declaration
// required: the walk itself is the protection, the rightmost entries being
// the work of the declared proxies only.
func TestHandler_ClientIP_DeclaredProxyXForwardedFor_ResolvesTheRealClient(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t, WithTrustedProxies("203.0.113.0/24"))

	t.Run("x-forwarded-for single value", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", nil)
		req.RemoteAddr = "203.0.113.10:443"
		req.Header.Set(headerXForwardedFor, "198.51.100.7")
		if got := h.clientIP(req); got != "198.51.100.7" {
			t.Errorf("clientIP = %q, want the X-Forwarded-For value %q", got, "198.51.100.7")
		}
	})
	t.Run("x-forwarded-for chain strips trailing trusted proxies", func(t *testing.T) {
		// client -> proxy A -> this proxy: A appended the client, this
		// proxy appended A. Walking from the right strips A (declared)
		// and finds the client.
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", nil)
		req.RemoteAddr = "203.0.113.10:443"
		req.Header.Set(headerXForwardedFor, "198.51.100.7, 203.0.113.9")
		if got := h.clientIP(req); got != "198.51.100.7" {
			t.Errorf("clientIP = %q, want the address the leftmost trusted proxy saw, %q", got, "198.51.100.7")
		}
	})
	t.Run("client spoof entries left of the appended address change nothing", func(t *testing.T) {
		// The client sent its own X-Forwarded-For; the trusted proxy
		// appended the real peer behind it. The rightmost untrusted
		// entry is the proxy's own answer.
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", nil)
		req.RemoteAddr = "203.0.113.10:443"
		req.Header.Set(headerXForwardedFor, "192.0.2.66, 198.51.100.7")
		if got := h.clientIP(req); got != "198.51.100.7" {
			t.Errorf("clientIP = %q, want %q (the appended address, never the client-supplied prefix)", got, "198.51.100.7")
		}
	})
	t.Run("no forwarding headers falls back to the peer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", nil)
		req.RemoteAddr = "203.0.113.10:443"
		if got := h.clientIP(req); got != "203.0.113.10" {
			t.Errorf("clientIP = %q, want the peer %q when no header carries an address", got, "203.0.113.10")
		}
	})
	t.Run("malformed chain falls back to the peer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", nil)
		req.RemoteAddr = "203.0.113.10:443"
		req.Header.Set(headerXForwardedFor, "198.51.100.7, not-an-ip")
		if got := h.clientIP(req); got != "203.0.113.10" {
			t.Errorf("clientIP = %q, want the peer %q for a chain no trusted proxy wrote", got, "203.0.113.10")
		}
	})
	t.Run("all-trusted chain falls back to the peer", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", nil)
		req.RemoteAddr = "203.0.113.10:443"
		req.Header.Set(headerXForwardedFor, "203.0.113.9, 203.0.113.10")
		if got := h.clientIP(req); got != "203.0.113.10" {
			t.Errorf("clientIP = %q, want the peer %q when the chain names only proxies", got, "203.0.113.10")
		}
	})
}

// TestHandler_ClientIP_VendorHeader_RequiresHostOptIn pins the opt-in at
// the derivation level: a single-hop vendor header (Fly-Client-IP) is read
// ONLY when the host opted into that specific header
// (WithVendorClientIPHeaders), never merely because the request's direct
// peer is a declared proxy. The deployment this test stands in for
// declared its GENERIC reverse proxy (nginx/ALB/Envoy/Cloudflare -- the
// shapes that forward unknown headers verbatim, never stripping a
// Fly-specific one), so a client-chosen Fly-Client-IP must never become
// the recorded address: with the honest X-Forwarded-For chain in hand it
// is the chain's answer that wins, and without one it is the peer. A
// gate-only read would return the client-chosen value 192.0.2.66 over the
// chain's own 198.51.100.7, and would accept 192.0.2.66, 8.8.8.8,
// 203.0.113.250 (inside the declared trust range itself --
// X-Forwarded-For would have stripped it) and ::1.
func TestHandler_ClientIP_VendorHeader_RequiresHostOptIn(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t, WithTrustedProxies("203.0.113.0/24"))

	t.Run("client-chosen fly client ip never becomes the address", func(t *testing.T) {
		// No X-Forwarded-For at all: the ordinary shape for a generic
		// proxy on a request the client crafted. The only address this
		// deployment actually observed is the peer.
		for _, chosen := range []string{"192.0.2.66", "8.8.8.8", "203.0.113.250", "::1"} {
			req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", nil)
			req.RemoteAddr = "203.0.113.10:443"
			req.Header.Set(headerFlyClientIP, chosen)
			if got := h.clientIP(req); got != "203.0.113.10" {
				t.Errorf("client-chosen Fly-Client-IP %q became the recorded address %q; want the peer 203.0.113.10", chosen, got)
			}
		}
	})
	t.Run("x-forwarded-for truth wins over a smuggled fly client ip", func(t *testing.T) {
		// The strongest exploit shape: the honest chain the deployment's
		// own proxy appended is PRESENT and names the true client
		// (198.51.100.7), while the client has additionally smuggled a
		// Fly-Client-IP the proxy did not strip. A gate-only read would have
		// the truth in hand and prefer the attacker's value.
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", nil)
		req.RemoteAddr = "203.0.113.10:443"
		req.Header.Set(headerXForwardedFor, "198.51.100.7")
		req.Header.Set(headerFlyClientIP, "192.0.2.66")
		if got := h.clientIP(req); got != "198.51.100.7" {
			t.Errorf("clientIP = %q, want the X-Forwarded-For truth %q over the client-chosen Fly-Client-IP", got, "198.51.100.7")
		}
	})
	t.Run("malformed fly client ip changes nothing", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", nil)
		req.RemoteAddr = "203.0.113.10:443"
		req.Header.Set(headerXForwardedFor, "198.51.100.7")
		req.Header.Set(headerFlyClientIP, "garbage")
		if got := h.clientIP(req); got != "198.51.100.7" {
			t.Errorf("clientIP = %q, want the X-Forwarded-For truth %q", got, "198.51.100.7")
		}
	})
}

// TestHandler_ClientIP_OptedInVendorHeader_PreservesTheLegitimateFlyShape
// pins the legitimate side of the opt-in at the derivation level: the host
// that opted into headerFlyClientIP (WithVendorClientIPHeaders) is a REAL
// Fly deployment whose proxy overwrites the header on every request it
// forwards, and that legitimate shape must keep working -- a request from
// the declared proxy carrying the proxy-written Fly-Client-IP records it.
// The opt-in is what the handler built in the vendor-opt-in test above
// deliberately lacks; the two tests together pin both sides of the
// host-declared contract.
func TestHandler_ClientIP_OptedInVendorHeader_PreservesTheLegitimateFlyShape(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t,
		WithTrustedProxies("203.0.113.0/24"),
		WithVendorClientIPHeaders(VendorClientIPHeaderFlyClientIP))

	t.Run("proxy-overwritten fly client ip is recorded", func(t *testing.T) {
		// The request shape of a real Fly deployment: the proxy wrote the
		// header, no X-Forwarded-For is in play, and the recorded address
		// is the forwarded client's.
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", nil)
		req.RemoteAddr = "203.0.113.10:443"
		req.Header.Set(headerFlyClientIP, "198.51.100.7")
		if got := h.clientIP(req); got != "198.51.100.7" {
			t.Errorf("clientIP = %q, want the Fly-Client-IP value %q (the opted-in Fly shape)", got, "198.51.100.7")
		}
	})
	t.Run("chain walk answers first and agrees with the fly header", func(t *testing.T) {
		// Fly's proxy appends the client to X-Forwarded-For too, so the
		// protected chain walk answers before the vendor header is even
		// consulted -- the ordering that guarantees the unprotected
		// single-hop read can never short-circuit the self-protecting
		// path. Both sources carry the same truth here.
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", nil)
		req.RemoteAddr = "203.0.113.10:443"
		req.Header.Set(headerXForwardedFor, "198.51.100.7, 203.0.113.9")
		req.Header.Set(headerFlyClientIP, "198.51.100.7")
		if got := h.clientIP(req); got != "198.51.100.7" {
			t.Errorf("clientIP = %q, want the chain's %q with the Fly header agreeing", got, "198.51.100.7")
		}
	})
	t.Run("unparseable fly value falls through to x-forwarded-for", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", nil)
		req.RemoteAddr = "203.0.113.10:443"
		req.Header.Set(headerFlyClientIP, "garbage")
		req.Header.Set(headerXForwardedFor, "198.51.100.7")
		if got := h.clientIP(req); got != "198.51.100.7" {
			t.Errorf("clientIP = %q, want the X-Forwarded-For value %q when Fly-Client-IP is malformed", got, "198.51.100.7")
		}
	})
}

// TestHandler_ClientIP_SpoofedHeaders_NeverBeatTheConnectionAddress pins
// the no-trust side at the derivation level: WITHOUT the trusted-proxy
// configuration -- or with it, but for a request whose peer is not a
// declared proxy -- a direct request carrying a spoofed Fly-Client-IP or
// X-Forwarded-For still resolves to the connection address. An
// implementation that read the headers unconditionally fails this test; it
// exists alongside the trusted-proxy cases above so the two together pin
// the honest shape.
func TestHandler_ClientIP_SpoofedHeaders_NeverBeatTheConnectionAddress(t *testing.T) {
	t.Parallel()

	t.Run("no proxies declared", func(t *testing.T) {
		h, _ := newTestHandler(t)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", nil)
		req.RemoteAddr = "198.51.100.9:4500"
		req.Header.Set(headerXForwardedFor, "203.0.113.99")
		req.Header.Set(headerFlyClientIP, "203.0.113.99")
		if got := h.clientIP(req); got != "198.51.100.9" {
			t.Errorf("clientIP = %q, want the connection address %q (no proxy was declared to trust)", got, "198.51.100.9")
		}
	})
	t.Run("peer is not a declared proxy", func(t *testing.T) {
		h, _ := newTestHandler(t, WithTrustedProxies("203.0.113.0/24"))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/login/password", nil)
		req.RemoteAddr = "198.51.100.9:4500"
		req.Header.Set(headerXForwardedFor, "203.0.113.99")
		req.Header.Set(headerFlyClientIP, "203.0.113.99")
		if got := h.clientIP(req); got != "198.51.100.9" {
			t.Errorf("clientIP = %q, want the connection address %q (a direct client is not a declared proxy)", got, "198.51.100.9")
		}
	})
}

// TestHandler_LoginThroughDeclaredProxy_RecordsAndReportsTheClientAddress
// pins the trusted-proxy shape end to end: through the real sign-in
// handler, with the proxy declared, a request from the proxy carrying the
// forwarded client address lands that address -- not the proxy's -- in the
// session row, the login-history row, the sessions response and the
// login-history response; and a direct request carrying a spoofed header
// still lands its own connection address in all four.
func TestHandler_LoginThroughDeclaredProxy_RecordsAndReportsTheClientAddress(t *testing.T) {
	h, f := newTestHandler(t, WithTrustedProxies("203.0.113.0/24"))
	const realClient = "198.51.100.7"
	const directPeer = "192.0.2.55:4321"

	// The forwarded leg: a request from the declared proxy. The recorded
	// address must be the client's, not the proxy's.
	forwarded := f.registerUser(t, "forwarded@example.com", testTenantA)
	rec := signInFrom(h, "forwarded@example.com", "203.0.113.10:443",
		[][2]string{{headerXForwardedFor, realClient}})
	if rec.Code != http.StatusOK {
		t.Fatalf("forwarded login status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	wirePair := decodeBody[api.AuthnTokenPair](t, rec)
	forwardedPrincipal := Principal{
		UserID:    forwarded.ID,
		TenantID:  testTenantA,
		SessionID: *wirePair.Principal.SessionID,
	}

	// The direct leg: no proxy in front, a spoofed header. The recorded
	// address must stay the connection address.
	direct := f.registerUser(t, "direct@example.com", testTenantA)
	spoofRec := signInFrom(h, "direct@example.com", directPeer,
		[][2]string{{headerFlyClientIP, "6.6.6.6"}, {headerXForwardedFor, "6.6.6.6"}})
	if spoofRec.Code != http.StatusOK {
		t.Fatalf("direct login status = %d, want %d; body = %s", spoofRec.Code, http.StatusOK, spoofRec.Body.String())
	}
	spoofWirePair := decodeBody[api.AuthnTokenPair](t, spoofRec)
	directPrincipal := Principal{
		UserID:    direct.ID,
		TenantID:  testTenantA,
		SessionID: *spoofWirePair.Principal.SessionID,
	}

	rows, err := f.svc.ListSessions(t.Context(), forwarded.ID)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if got := rowIP(rows, *wirePair.Principal.SessionID); got != realClient {
		t.Errorf("session row IP = %q, want the forwarded client %q (was the proxy before the fix)", got, realClient)
	}
	attempts, err := f.svc.ListLoginHistory(t.Context(), forwarded.ID, 0)
	if err != nil {
		t.Fatalf("ListLoginHistory: %v", err)
	}
	attempt := attemptRow(attempts, *wirePair.Principal.SessionID)
	if attempt == nil {
		t.Fatalf("no login-history row for session %q in %+v", *wirePair.Principal.SessionID, attempts)
	}
	if attempt.IP != realClient {
		t.Errorf("login-history row IP = %q, want the forwarded client %q (was the proxy before the fix)", attempt.IP, realClient)
	}
	directRows, err := f.svc.ListSessions(t.Context(), direct.ID)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if got := rowIP(directRows, *spoofWirePair.Principal.SessionID); got != "192.0.2.55" {
		t.Errorf("spoofed direct session row IP = %q, want the connection address %q", got, "192.0.2.55")
	}

	// The responses carry the same addresses the rows do.
	var sessionsResp api.AuthnListSessionsResponse
	rec2 := doHandlerJSON(t, h, http.MethodGet, "/api/v1/authn/sessions", nil, &forwardedPrincipal)
	if rec2.Code != http.StatusOK {
		t.Fatalf("list sessions status = %d, want %d", rec2.Code, http.StatusOK)
	}
	sessionsResp = decodeBody[api.AuthnListSessionsResponse](t, rec2)
	if got := sessionResponseIP(*sessionsResp.Sessions, *wirePair.Principal.SessionID); got != realClient {
		t.Errorf("sessions response IP = %q, want the forwarded client %q", got, realClient)
	}

	var historyResp api.AuthnListLoginHistoryResponse
	historyRec := doHandlerJSON(t, h, http.MethodGet, "/api/v1/authn/login-history", nil, &forwardedPrincipal)
	if historyRec.Code != http.StatusOK {
		t.Fatalf("list login history status = %d, want %d", historyRec.Code, http.StatusOK)
	}
	historyResp = decodeBody[api.AuthnListLoginHistoryResponse](t, historyRec)
	if got := attemptResponseIP(*historyResp.Attempts, attempt.ID); got != realClient {
		t.Errorf("login-history response IP = %q, want the forwarded client %q", got, realClient)
	}

	var directSessions api.AuthnListSessionsResponse
	directSessionsRec := doHandlerJSON(t, h, http.MethodGet, "/api/v1/authn/sessions", nil, &directPrincipal)
	if directSessionsRec.Code != http.StatusOK {
		t.Fatalf("list sessions (direct) status = %d, want %d", directSessionsRec.Code, http.StatusOK)
	}
	directSessions = decodeBody[api.AuthnListSessionsResponse](t, directSessionsRec)
	if got := sessionResponseIP(*directSessions.Sessions, *spoofWirePair.Principal.SessionID); got != "192.0.2.55" {
		t.Errorf("spoofed direct sessions response IP = %q, want the connection address %q", got, "192.0.2.55")
	}
}

// registerFrom issues a registration for email on h over a request whose
// direct connection address is peer (r.RemoteAddr) and which carries the
// given headers -- the request shape clientIP's trusted-proxy gate decides
// on, and the one register's per-IP rate-limit bucket is keyed by.
// doHandlerJSON cannot produce one: httptest.NewRequest always originates
// from its own fixed 192.0.2.1 address.
func registerFrom(h *Handler, email, peer string, headers [][2]string) *httptest.ResponseRecorder {
	body, err := json.Marshal(api.AuthnRegisterRequest{
		Email:    strPtr(email),
		Password: testPassword,
	})
	if err != nil {
		panic(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/register", bytes.NewReader(body))
	req.RemoteAddr = peer
	for _, kv := range headers {
		req.Header.Set(kv[0], kv[1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestHandler_RegisterThroughDeclaredGenericProxy_RateLimitIgnoresTheClientChosenFlyHeader
// pins the register bucket end to end: CheckRegister's per-IP
// bucket (10 registrations per hour, IP the one dimension -- there is no
// account yet to key a second one on) must keep counting the deployment's
// own proxy address when a client behind a declared GENERIC proxy rotates
// a smuggled Fly-Client-IP on every attempt. Each attempt keyed on its own
// client-chosen value would give every attempt its own bucket and let the
// eleventh registration succeed -- unlimited account creation through one
// HTTP header; with no vendor header opted in, every attempt keys on the
// peer, and the eleventh is refused with authn.rate_limited.
func TestHandler_RegisterThroughDeclaredGenericProxy_RateLimitIgnoresTheClientChosenFlyHeader(t *testing.T) {
	t.Parallel()
	h, _ := newTestHandler(t, WithTrustedProxies("203.0.113.0/24"))

	// Ten successful registrations from the declared proxy peer, each with
	// a DIFFERENT client-chosen Fly-Client-IP: the per-IP register budget
	// (10/hour) is exhausted on the peer regardless of the header values.
	for i := 1; i <= 10; i++ {
		spoof := fmt.Sprintf("192.0.2.%d", i)
		rec := registerFrom(h, fmt.Sprintf("bucket-%d@example.com", i), "203.0.113.10:443",
			[][2]string{{headerFlyClientIP, spoof}})
		if rec.Code != http.StatusCreated {
			t.Fatalf("registration %d status = %d, want %d; body = %s", i, rec.Code, http.StatusCreated, rec.Body.String())
		}
	}
	rec := registerFrom(h, "bucket-11@example.com", "203.0.113.10:443",
		[][2]string{{headerFlyClientIP, "192.0.2.99"}})
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("eleventh registration status = %d, want %d (a client-chosen Fly-Client-IP must not buy a fresh register bucket); body = %s",
			rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}
	if got := deref(decodeAuthnError(t, rec).Code); got != "authn.rate_limited" {
		t.Errorf("eleventh registration error code = %q, want %q", got, "authn.rate_limited")
	}
}

// rowIP returns the IP of the first of rows whose session id is sessionID,
// or "" when no row matches.
func rowIP(rows []Session, sessionID string) string {
	for i := range rows {
		if rows[i].ID == sessionID {
			return rows[i].IP
		}
	}
	return ""
}

// attemptRow returns the first attempt whose session id is sessionID --
// the successful sign-in's own login-history row -- or nil when no attempt
// matches.
func attemptRow(attempts []LoginAttempt, sessionID string) *LoginAttempt {
	for i := range attempts {
		if attempts[i].SessionID == sessionID {
			return &attempts[i]
		}
	}
	return nil
}

// sessionResponseIP returns the dereferenced IP of the first session whose
// id is sessionID, or "" when no session matches.
func sessionResponseIP(sessions []api.AuthnSession, sessionID string) string {
	for i := range sessions {
		if sessions[i].ID != nil && *sessions[i].ID == sessionID {
			if sessions[i].IP != nil {
				return *sessions[i].IP
			}
			return ""
		}
	}
	return ""
}

// attemptResponseIP is sessionResponseIP for the login-history response,
// keyed on the attempt's own id (the wire type carries no session id).
func attemptResponseIP(attempts []api.AuthnLoginAttempt, attemptID string) string {
	for i := range attempts {
		if attempts[i].ID != nil && *attempts[i].ID == attemptID {
			if attempts[i].IP != nil {
				return *attempts[i].IP
			}
			return ""
		}
	}
	return ""
}
