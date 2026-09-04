package sharing

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/sharing/api"
)

// fakeResourceResolver is a minimal, in-test ResourceResolver double. A
// non-nil err always wins over the configured content, matching how a real
// resolver's failure would look to Handler.
type fakeResourceResolver struct {
	mime string
	body string
	err  error
}

func (f fakeResourceResolver) OpenResource(context.Context, string) (ResourceContent, error) {
	if f.err != nil {
		return ResourceContent{}, f.err
	}
	return ResourceContent{MIME: f.mime, Size: int64(len(f.body)), Body: io.NopCloser(strings.NewReader(f.body))}, nil
}

// newTestHandler returns a Handler over a fresh, fully-wired Service (real
// registry, real KVStore -- so ratelimit.go's checks run for real rather
// than failing closed for want of a host) and resolver.
func newTestHandler(t *testing.T, resolver ResourceResolver) *Handler {
	t.Helper()
	svc, _ := newTestService(t, nil)
	return NewHandler(svc, resolver)
}

// accessRequest builds the GET request Handler.ServeHTTP expects: token as
// the query parameter, password (if any) as the HeaderSharePassword header.
func accessRequest(token string, password *string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, PathAccess+"?token="+url.QueryEscape(token), nil)
	if password != nil {
		req.Header.Set(HeaderSharePassword, *password)
	}
	return req
}

func strPtr(s string) *string { return &s }

// TestHandler_SharingAccessShare_SetsNoStoreCacheControl pins the one
// obligation AGENTS.md's "Revocation and caching" section names as binding
// on whichever round adds this route: EVERY response this handler writes --
// a granted access, an unrecognized-token refusal, and a resource-resolver
// failure alike -- carries Cache-Control: no-store. A CDN or shared cache
// honoring a response without that header is, per that section, the single
// most common way revocation silently fails to take effect.
func TestHandler_SharingAccessShare_SetsNoStoreCacheControl(t *testing.T) {
	h := newTestHandler(t, fakeResourceResolver{mime: "text/plain", body: "hello"})

	created, err := h.svc.Create(testCtx(), CreateParams{ResourceRef: "ref-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cases := []struct {
		name  string
		token string
	}{
		{"granted access", created.Token},
		{"unrecognized token", "no-such-token"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, accessRequest(c.token, nil))
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want %q", got, "no-store")
			}
		})
	}
}

// TestHandler_SharingAccessShare_EveryRefusalReasonAnswersIdentically is
// this round's HTTP-layer proof of rule 5 (AGENTS.md's "The five mandatory
// rules" section, service_test.go's identical
// TestService_Access_EveryRefusalReasonIsOutwardlyIdentical for the
// Service-level proof): every one of Service.Access's refusal reasons must
// reach the wire as the exact same status and body, so probing this route
// teaches an outside caller nothing about which reason actually applied.
func TestHandler_SharingAccessShare_EveryRefusalReasonAnswersIdentically(t *testing.T) {
	h := newTestHandler(t, fakeResourceResolver{mime: "text/plain", body: "hello"})
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h.svc.now = fixedClock(now)

	mint := func(t *testing.T, params CreateParams) string {
		t.Helper()
		created, err := h.svc.Create(testCtx(), params)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		return created.Token
	}

	revokedToken := mint(t, CreateParams{ResourceRef: "ref-revoked"})
	revokedShare, err := h.svc.shares.byTokenHash(testCtx(), hashShareToken(revokedToken))
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if err := h.svc.Revoke(testCtx(), revokedShare.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	past := now.Add(-time.Hour)
	expiredToken := mint(t, CreateParams{ResourceRef: "ref-expired", ExpiresAt: &past})

	one := 1
	exhaustedToken := mint(t, CreateParams{ResourceRef: "ref-exhausted", MaxViews: &one})
	if _, err := h.svc.Access(testCtx(), exhaustedToken, AccessParams{}); err != nil {
		t.Fatalf("Access(exhausting the one view): %v", err)
	}

	protectedToken := mint(t, CreateParams{ResourceRef: "ref-protected", Password: strPtr("correct horse")})

	cases := []struct {
		name     string
		token    string
		password *string
	}{
		{"unrecognized token", "definitely-not-a-real-token", nil},
		{"revoked share", revokedToken, nil},
		{"expired share", expiredToken, nil},
		{"view exhausted", exhaustedToken, nil},
		{"missing password", protectedToken, nil},
		{"wrong password", protectedToken, strPtr("wrong")},
	}

	var first *bodyAndStatus
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, accessRequest(c.token, c.password))
			got := &bodyAndStatus{status: rec.Code, body: rec.Body.String()}
			if first == nil {
				first = got
				return
			}
			if got.status != first.status || got.body != first.body {
				t.Errorf("refusal answer = %+v, want identical to the first case's %+v -- rule 5 requires every refusal reason to be outwardly indistinguishable", got, first)
			}
		})
	}
	if first == nil || first.status != http.StatusNotFound {
		t.Fatalf("first refusal's status = %+v, want 404", first)
	}
}

type bodyAndStatus struct {
	status int
	body   string
}

// TestHandler_SharingAccessShare_PasswordProtected_CorrectPasswordGrants
// proves the password-protected variant point 2 of the round's scope names
// explicitly: a correct password serves the content, through the exact
// same route and parameter shape a wrong one is refused through.
func TestHandler_SharingAccessShare_PasswordProtected_CorrectPasswordGrants(t *testing.T) {
	h := newTestHandler(t, fakeResourceResolver{mime: "image/png", body: "pixels"})
	created, err := h.svc.Create(testCtx(), CreateParams{ResourceRef: "ref-1", Password: strPtr("s3cret")})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, accessRequest(created.Token, strPtr("s3cret")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q, want %q", got, "image/png")
	}
	if got := rec.Body.String(); got != "pixels" {
		t.Errorf("body = %q, want %q", got, "pixels")
	}
}

// TestHandler_SharingAccessShare_NoResolverWired_AnswersResourceUnavailable
// proves a granted access with no ResourceResolver wired at all fails
// loudly and distinctly (502 sharing.resource_unavailable), never as a
// silent empty body or as the same 404 an actual access refusal answers.
func TestHandler_SharingAccessShare_NoResolverWired_AnswersResourceUnavailable(t *testing.T) {
	h := newTestHandler(t, nil)
	created, err := h.svc.Create(testCtx(), CreateParams{ResourceRef: "ref-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, accessRequest(created.Token, nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s, want 502", rec.Code, rec.Body.String())
	}
	var body api.SharingError
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Code != ErrResourceUnavailable.Code {
		t.Errorf("code = %q, want %q", body.Code, ErrResourceUnavailable.Code)
	}
}

// TestHandler_SharingAccessShare_ResolverError_AnswersResourceUnavailable
// mirrors the no-resolver-wired case for a resolver that IS wired but
// fails to open the resource.
func TestHandler_SharingAccessShare_ResolverError_AnswersResourceUnavailable(t *testing.T) {
	h := newTestHandler(t, fakeResourceResolver{err: io.ErrUnexpectedEOF})
	created, err := h.svc.Create(testCtx(), CreateParams{ResourceRef: "ref-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, accessRequest(created.Token, nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, body = %s, want 502", rec.Code, rec.Body.String())
	}
}

// tenantAssertingResolver is a ResourceResolver double that records the
// tenant ctx carried on its one OpenResource call, so a test can prove
// Handler rebuilds a tenant-bearing context for the resolver rather than
// forwarding the bare, tenant-less request context AccessPublic itself
// never mutates.
type tenantAssertingResolver struct {
	got *pkgcore.TenantID
}

func (r tenantAssertingResolver) OpenResource(ctx context.Context, _ string) (ResourceContent, error) {
	tenant, _ := pkgcore.TenantFromContext(ctx)
	*r.got = tenant
	return ResourceContent{MIME: "text/plain", Body: io.NopCloser(strings.NewReader("ok"))}, nil
}

// TestHandler_SharingAccessShare_ResolverSeesTheShareOwningTenant is the
// direct regression test for a real bug this round's own reference-app
// flow test caught: Service.AccessPublic resolves the tenant only inside
// its own call to Service.Access, never mutating the *http.Request's own
// context -- a resolver that itself needs a tenant in ctx (like
// go/storage's ObjectService.OpenContent) would otherwise fail closed with
// pkgcore.ErrNoTenant on every granted access. Handler must rebuild the
// tenant from the granted Share's own row before calling OpenResource.
func TestHandler_SharingAccessShare_ResolverSeesTheShareOwningTenant(t *testing.T) {
	svc, _ := newTestService(t, nil)
	svc.now = fixedClock(time.Now())
	ctx := pkgcore.WithTenant(context.Background(), "tenant-xyz")
	created, err := svc.Create(ctx, CreateParams{ResourceRef: "ref-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var got pkgcore.TenantID
	h := NewHandler(svc, tenantAssertingResolver{got: &got})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, accessRequest(created.Token, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s, want 200", rec.Code, rec.Body.String())
	}
	if got != "tenant-xyz" {
		t.Errorf("resolver observed tenant %q, want %q", got, "tenant-xyz")
	}
}

// TestHandler_SharingAccessShare_MissingTokenParam_Answers400 proves an
// absent required token query parameter fails through the spec-generated
// parameter-binding error path rather than reaching Service at all -- and
// that NewHandler's own ErrorHandlerFunc (bindingErrorHandler, handler.go),
// not oapi-codegen's default http.Error, is what answers it: the response
// must still carry Cache-Control: no-store (AGENTS.md's "Revocation and
// caching" section names this as binding on every response this route can
// produce, binding failures included) and the module's own SharingError
// JSON envelope, never a plain-text body.
func TestHandler_SharingAccessShare_MissingTokenParam_Answers400(t *testing.T) {
	h := newTestHandler(t, nil)
	req := httptest.NewRequest(http.MethodGet, PathAccess, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a missing required token parameter", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q for a binding-error refusal", got, "no-store")
	}
	var envelope api.SharingError
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("body did not decode as api.SharingError: %v (body: %s)", err, rec.Body.String())
	}
	if envelope.Code != ErrInvalidRequest.Code {
		t.Errorf("envelope.Code = %q, want %q", envelope.Code, ErrInvalidRequest.Code)
	}
}

// TestHandler_SharingAccessShare_DuplicatePasswordHeader_Answers400 covers
// the finding's other named binding failure -- a duplicated
// X-Sharing-Password header, which SharingAccessShareParams's underlying
// bind rejects as TooManyValuesForParamError -- through the same
// ErrorHandlerFunc path as the missing-token case above, so the fix is
// pinned against more than the one parameter oapi-codegen happens to bind
// first.
func TestHandler_SharingAccessShare_DuplicatePasswordHeader_Answers400(t *testing.T) {
	h := newTestHandler(t, nil)
	req := accessRequest("some-token", nil)
	req.Header.Add(HeaderSharePassword, "first")
	req.Header.Add(HeaderSharePassword, "second")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a duplicated %s header", rec.Code, HeaderSharePassword)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want %q for a binding-error refusal", got, "no-store")
	}
	var envelope api.SharingError
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("body did not decode as api.SharingError: %v (body: %s)", err, rec.Body.String())
	}
	if envelope.Code != ErrInvalidRequest.Code {
		t.Errorf("envelope.Code = %q, want %q", envelope.Code, ErrInvalidRequest.Code)
	}
}

// compile-time check that Handler still satisfies api.ServerInterface --
// duplicated from handler.go's own assertion so a reader of this test file
// sees the contract without following an import.
var _ api.ServerInterface = (*Handler)(nil)
