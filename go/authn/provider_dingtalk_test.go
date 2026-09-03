package authn

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// dingtalkStub is a local stand-in for DingTalk's two endpoints.
type dingtalkStub struct {
	Server *httptest.Server

	mu          sync.Mutex
	token       map[string]any
	user        map[string]any
	tokenBody   map[string]any
	tokenHeader string
	tokenStatus int
	userStatus  int
}

func newDingTalkStub(t *testing.T) *dingtalkStub {
	t.Helper()

	stub := &dingtalkStub{
		token: map[string]any{"accessToken": "dt-user-token", "expireIn": 7200},
		user: map[string]any{
			"nick": "A Person", "unionId": "dt-union-id", "openId": "dt-open-id",
			"avatarUrl": "https://example.com/dt.png", "email": "person@example.com",
			"mobile": "+8613800000000",
		},
		tokenStatus: http.StatusOK,
		userStatus:  http.StatusOK,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1.0/oauth2/userAccessToken", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)

		stub.mu.Lock()
		stub.tokenBody = body
		response, status := stub.token, stub.tokenStatus
		stub.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	})
	mux.HandleFunc("/v1.0/contact/users/me", func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.tokenHeader = r.Header.Get(dingtalkTokenHeader)
		response, status := stub.user, stub.userStatus
		stub.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(response)
	})

	stub.Server = httptest.NewServer(mux)
	t.Cleanup(stub.Server.Close)
	return stub
}

func (s *dingtalkStub) provider() *DingTalkProvider {
	return NewDingTalkProvider("dt-client-id", "dt-client-secret",
		WithProviderAuthBaseURL(s.Server.URL),
		WithProviderAPIBaseURL(s.Server.URL),
		WithProviderHTTPClient(s.Server.Client()),
	)
}

func TestDingTalkProvider_NameAndAuthorizeURL(t *testing.T) {
	t.Parallel()

	stub := newDingTalkStub(t)
	provider := stub.provider()

	if got := provider.Name(); got != ProviderDingTalk {
		t.Errorf("Name() = %q, want %q", got, ProviderDingTalk)
	}

	query := parseQuery(t, provider.AuthorizeURL("state-value", "https://app.example.com/callback"))
	if got := query.Get("client_id"); got != "dt-client-id" {
		t.Errorf("client_id = %q", got)
	}
	if got := query.Get("state"); got != "state-value" {
		t.Errorf("state = %q", got)
	}
	if got := query.Get("client_secret"); got != "" {
		t.Error("the authorization URL carries the client secret")
	}
}

func TestDingTalkProvider_Exchange_PostsJSONAndUsesItsOwnTokenHeader(t *testing.T) {
	t.Parallel()

	stub := newDingTalkStub(t)
	identity, err := stub.provider().Exchange(t.Context(), "the-code", "https://app.example.com/callback")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}

	if identity.ExternalID != "dt-union-id" {
		t.Errorf("ExternalID = %q, want the unionId; openId is per application", identity.ExternalID)
	}
	if identity.Email != "person@example.com" {
		t.Errorf("Email = %q", identity.Email)
	}
	// DingTalk makes no verification assertion, so reporting the address
	// as verified would be inventing one -- which is exactly what the
	// account-linking rule forbids.
	if identity.EmailVerified {
		t.Error("EmailVerified = true; DingTalk asserts nothing about verification")
	}
	if identity.Name != "A Person" {
		t.Errorf("Name = %q", identity.Name)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	// The token endpoint takes a JSON body, not a form: that difference
	// is why this channel cannot go through a standard OAuth2 client.
	if got := stub.tokenBody["clientId"]; got != "dt-client-id" {
		t.Errorf("token body clientId = %v", got)
	}
	if got := stub.tokenBody["grantType"]; got != "authorization_code" {
		t.Errorf("token body grantType = %v", got)
	}
	if got := stub.tokenBody["code"]; got != "the-code" {
		t.Errorf("token body code = %v", got)
	}
	// The contact API takes its bearer token in its own header rather
	// than in Authorization.
	if stub.tokenHeader != "dt-user-token" {
		t.Errorf("%s header = %q", dingtalkTokenHeader, stub.tokenHeader)
	}
}

// TestDingTalkProvider_Exchange_DropsTheMobileNumber pins a privacy decision.
// DingTalk returns a phone number this module has no use for, and a number
// that arrived without a verification round trip here must never become a
// login identifier.
func TestDingTalkProvider_Exchange_DropsTheMobileNumber(t *testing.T) {
	t.Parallel()

	stub := newDingTalkStub(t)
	identity, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(identity.Raw, &raw); err != nil {
		t.Fatalf("decode Raw: %v", err)
	}
	if mobile, ok := raw["mobile"].(string); ok && mobile != "" {
		t.Errorf("Raw carries the mobile number %q", mobile)
	}
}

func TestDingTalkProvider_Exchange_RefusesAnUnusableResponse(t *testing.T) {
	t.Parallel()

	t.Run("the token endpoint failed", func(t *testing.T) {
		t.Parallel()
		stub := newDingTalkStub(t)
		stub.mu.Lock()
		stub.tokenStatus = http.StatusBadRequest
		stub.mu.Unlock()

		_, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
		assertErrorCode(t, err, ErrSocialExchangeFailed.Code)
	})

	t.Run("no access token came back", func(t *testing.T) {
		t.Parallel()
		stub := newDingTalkStub(t)
		stub.mu.Lock()
		stub.token = map[string]any{"expireIn": 7200}
		stub.mu.Unlock()

		_, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
		assertErrorCode(t, err, ErrSocialExchangeFailed.Code)
	})

	t.Run("the contact api failed", func(t *testing.T) {
		t.Parallel()
		stub := newDingTalkStub(t)
		stub.mu.Lock()
		stub.userStatus = http.StatusForbidden
		stub.mu.Unlock()

		_, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
		assertErrorCode(t, err, ErrSocialExchangeFailed.Code)
	})

	t.Run("the user document carries no union id", func(t *testing.T) {
		t.Parallel()
		stub := newDingTalkStub(t)
		stub.mu.Lock()
		delete(stub.user, "unionId")
		stub.mu.Unlock()

		_, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
		assertErrorCode(t, err, ErrSocialIdentityIncomplete.Code)
	})
}
