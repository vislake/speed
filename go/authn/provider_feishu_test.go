package authn

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// feishuStub is a local stand-in for Feishu's three endpoints.
type feishuStub struct {
	Server *httptest.Server

	mu             sync.Mutex
	appToken       map[string]any
	userToken      map[string]any
	userInfo       map[string]any
	appTokenBody   map[string]any
	userTokenBody  map[string]any
	userTokenAuth  string
	userInfoAuth   string
	userInfoStatus int
}

func newFeishuStub(t *testing.T) *feishuStub {
	t.Helper()

	stub := &feishuStub{
		appToken:  map[string]any{"code": 0, "msg": "ok", "app_access_token": "fs-app-token"},
		userToken: map[string]any{"code": 0, "msg": "ok", "data": map[string]any{"access_token": "fs-user-token"}},
		userInfo: map[string]any{"code": 0, "msg": "ok", "data": map[string]any{
			"name": "A Person", "avatar_url": "https://example.com/fs.png",
			"email": "personal@example.com", "enterprise_email": "person@corp.example.com",
			"open_id": "fs-open-id", "union_id": "fs-union-id",
		}},
		userInfoStatus: http.StatusOK,
	}

	decode := func(r *http.Request) map[string]any {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		return body
	}

	mux := http.NewServeMux()
	mux.HandleFunc(feishuAppTokenPath, func(w http.ResponseWriter, r *http.Request) {
		body := decode(r)
		stub.mu.Lock()
		stub.appTokenBody = body
		response := stub.appToken
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})
	mux.HandleFunc(feishuUserTokenPath, func(w http.ResponseWriter, r *http.Request) {
		body := decode(r)
		stub.mu.Lock()
		stub.userTokenBody = body
		stub.userTokenAuth = r.Header.Get("Authorization")
		response := stub.userToken
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})
	mux.HandleFunc(feishuUserInfoPath, func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.userInfoAuth = r.Header.Get("Authorization")
		response, status := stub.userInfo, stub.userInfoStatus
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

func (s *feishuStub) provider() *FeishuProvider {
	return NewFeishuProvider("fs-app-id", "fs-app-secret",
		WithProviderAuthBaseURL(s.Server.URL),
		WithProviderAPIBaseURL(s.Server.URL),
		WithProviderHTTPClient(s.Server.Client()),
	)
}

func TestFeishuProvider_NameAndAuthorizeURL(t *testing.T) {
	t.Parallel()

	stub := newFeishuStub(t)
	provider := stub.provider()

	if got := provider.Name(); got != ProviderFeishu {
		t.Errorf("Name() = %q, want %q", got, ProviderFeishu)
	}

	query := parseQuery(t, provider.AuthorizeURL("state-value", "https://app.example.com/callback"))
	// Feishu names the client id "app_id", like WeChat names it "appid".
	if got := query.Get("app_id"); got != "fs-app-id" {
		t.Errorf("app_id = %q", got)
	}
	if got := query.Get("state"); got != "state-value" {
		t.Errorf("state = %q", got)
	}
	if got := query.Get("app_secret"); got != "" {
		t.Error("the authorization URL carries the app secret")
	}
}

// TestFeishuProvider_Exchange_TakesThreeRoundTrips pins the extra hop that
// makes this channel impossible to express as an oauth2.Config: an
// APPLICATION token has to be obtained before a user's code can be exchanged
// at all.
func TestFeishuProvider_Exchange_TakesThreeRoundTrips(t *testing.T) {
	t.Parallel()

	stub := newFeishuStub(t)
	identity, err := stub.provider().Exchange(t.Context(), "the-code", "https://app.example.com/callback")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}

	if identity.ExternalID != "fs-union-id" {
		t.Errorf("ExternalID = %q, want the union_id; open_id is per application", identity.ExternalID)
	}
	// The enterprise address wins over the personal one: it is the
	// identity the person has inside the organization that federated.
	if identity.Email != "person@corp.example.com" {
		t.Errorf("Email = %q, want the enterprise address", identity.Email)
	}
	if identity.EmailVerified {
		t.Error("EmailVerified = true; Feishu asserts nothing about verification")
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if got := stub.appTokenBody["app_id"]; got != "fs-app-id" {
		t.Errorf("app token body app_id = %v", got)
	}
	if got := stub.appTokenBody["app_secret"]; got != "fs-app-secret" {
		t.Errorf("app token body app_secret = %v", got)
	}
	if stub.userTokenAuth != "Bearer fs-app-token" {
		t.Errorf("the user-token call presented %q, want the APPLICATION token", stub.userTokenAuth)
	}
	if got := stub.userTokenBody["code"]; got != "the-code" {
		t.Errorf("user token body code = %v", got)
	}
	if stub.userInfoAuth != "Bearer fs-user-token" {
		t.Errorf("the user-info call presented %q, want the USER token", stub.userInfoAuth)
	}
}

func TestFeishuProvider_Exchange_FallsBackToThePersonalAddress(t *testing.T) {
	t.Parallel()

	stub := newFeishuStub(t)
	stub.mu.Lock()
	data, _ := stub.userInfo["data"].(map[string]any)
	delete(data, "enterprise_email")
	stub.mu.Unlock()

	identity, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if identity.Email != "personal@example.com" {
		t.Errorf("Email = %q, want the personal address when no enterprise one exists", identity.Email)
	}
}

// TestFeishuProvider_Exchange_TreatsANonZeroCodeAsAFailure covers Feishu's
// habit -- shared with WeChat -- of reporting failure inside an HTTP 200.
func TestFeishuProvider_Exchange_TreatsANonZeroCodeAsAFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		arrange func(stub *feishuStub)
		wantErr string
	}{
		{
			name: "the app token call failed",
			arrange: func(stub *feishuStub) {
				stub.appToken = map[string]any{"code": 99991663, "msg": "app not found"}
			},
			wantErr: ErrSocialExchangeFailed.Code,
		},
		{
			name: "the user token call failed",
			arrange: func(stub *feishuStub) {
				stub.userToken = map[string]any{"code": 20021, "msg": "invalid code"}
			},
			wantErr: ErrSocialExchangeFailed.Code,
		},
		{
			name: "the user info call failed",
			arrange: func(stub *feishuStub) {
				stub.userInfo = map[string]any{"code": 99991672, "msg": "no permission"}
			},
			wantErr: ErrSocialExchangeFailed.Code,
		},
		{
			name: "the user info call carried no union id",
			arrange: func(stub *feishuStub) {
				data, _ := stub.userInfo["data"].(map[string]any)
				delete(data, "union_id")
			},
			wantErr: ErrSocialIdentityIncomplete.Code,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			stub := newFeishuStub(t)
			stub.mu.Lock()
			tc.arrange(stub)
			stub.mu.Unlock()

			_, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
			assertErrorCode(t, err, tc.wantErr)
		})
	}
}
