package authn

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// wechatStub is a local stand-in for WeChat's two endpoints.
type wechatStub struct {
	Server *httptest.Server

	mu         sync.Mutex
	token      map[string]any
	userInfo   map[string]any
	tokenQuery url.Values
	infoQuery  url.Values
}

func newWeChatStub(t *testing.T) *wechatStub {
	t.Helper()

	stub := &wechatStub{
		token: map[string]any{
			"access_token": "wx-access-token",
			"openid":       "wx-openid-app-1",
			"unionid":      "wx-unionid",
			"scope":        "snsapi_login",
		},
		userInfo: map[string]any{
			"openid": "wx-openid-app-1", "unionid": "wx-unionid",
			"nickname": "A Person", "headimgurl": "https://example.com/wx.png",
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/sns/oauth2/access_token", func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.tokenQuery = r.URL.Query()
		body := stub.token
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc("/sns/userinfo", func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.infoQuery = r.URL.Query()
		body := stub.userInfo
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})

	stub.Server = httptest.NewServer(mux)
	t.Cleanup(stub.Server.Close)
	return stub
}

func (s *wechatStub) provider() *WeChatProvider {
	return NewWeChatProvider("wx-appid", "wx-secret",
		WithProviderAuthBaseURL(s.Server.URL),
		WithProviderAPIBaseURL(s.Server.URL),
		WithProviderHTTPClient(s.Server.Client()),
	)
}

func TestWeChatProvider_NameAndAuthorizeURL(t *testing.T) {
	t.Parallel()

	stub := newWeChatStub(t)
	provider := stub.provider()

	if got := provider.Name(); got != ProviderWeChat {
		t.Errorf("Name() = %q, want %q", got, ProviderWeChat)
	}

	raw := provider.AuthorizeURL("state-value", "https://app.example.com/callback")
	// WeChat requires this exact trailing fragment; without it the
	// authorization page does not render inside the WeChat client.
	if !strings.HasSuffix(raw, "#wechat_redirect") {
		t.Errorf("AuthorizeURL() = %q, want it to end with #wechat_redirect", raw)
	}

	query := parseQuery(t, strings.TrimSuffix(raw, "#wechat_redirect"))
	// WeChat calls the client id "appid", which is the whole reason this
	// channel cannot go through a standard OAuth2 client.
	if got := query.Get("appid"); got != "wx-appid" {
		t.Errorf("appid = %q", got)
	}
	if got := query.Get("client_id"); got != "" {
		t.Error("the authorization URL uses client_id; WeChat expects appid")
	}
	if got := query.Get("state"); got != "state-value" {
		t.Errorf("state = %q", got)
	}
	if got := query.Get("secret"); got != "" {
		t.Error("the authorization URL carries the app secret")
	}
}

func TestWeChatProvider_Exchange_UsesTheUnionID(t *testing.T) {
	t.Parallel()

	stub := newWeChatStub(t)
	identity, err := stub.provider().Exchange(t.Context(), "the-code", "https://app.example.com/callback")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}

	if identity.ExternalID != "wx-unionid" {
		t.Errorf("ExternalID = %q, want the unionid", identity.ExternalID)
	}
	if identity.ExternalID == "wx-openid-app-1" {
		t.Error("ExternalID is the openid, which is per application: the same person in a second application would become a second account")
	}
	// WeChat reports no address, so there is nothing to link on and
	// nothing to claim was verified.
	if identity.Email != "" || identity.EmailVerified {
		t.Errorf("Email = %q, EmailVerified = %v; WeChat reports neither", identity.Email, identity.EmailVerified)
	}
	if identity.Name != "A Person" {
		t.Errorf("Name = %q", identity.Name)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if got := stub.tokenQuery.Get("appid"); got != "wx-appid" {
		t.Errorf("token exchange appid = %q", got)
	}
	if got := stub.tokenQuery.Get("secret"); got != "wx-secret" {
		t.Errorf("token exchange secret = %q", got)
	}
	if got := stub.infoQuery.Get("openid"); got != "wx-openid-app-1" {
		t.Errorf("user info openid = %q; the per-application id is the right key for THIS call", got)
	}
}

// TestWeChatProvider_Exchange_RefusesAnOpenIDOnlyResponse is the single most
// important WeChat test.
//
// A response with an openid and no unionid works perfectly in a
// single-application deployment and then, the day a second application is
// added, silently splits every existing user into two accounts -- no error, no
// log line, and no way to merge them afterwards because by then both accounts
// have data. Refusing turns that into a configuration error an operator can
// fix on day one.
func TestWeChatProvider_Exchange_RefusesAnOpenIDOnlyResponse(t *testing.T) {
	t.Parallel()

	stub := newWeChatStub(t)
	stub.mu.Lock()
	delete(stub.token, "unionid")
	delete(stub.userInfo, "unionid")
	stub.mu.Unlock()

	_, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
	assertErrorCode(t, err, ErrSocialIdentityIncomplete.Code)
}

// TestWeChatProvider_Exchange_FallsBackToTheTokenUnionID covers the case where
// the union id arrives on the token response but not on the user document.
func TestWeChatProvider_Exchange_FallsBackToTheTokenUnionID(t *testing.T) {
	t.Parallel()

	stub := newWeChatStub(t)
	stub.mu.Lock()
	delete(stub.userInfo, "unionid")
	stub.mu.Unlock()

	identity, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
	if err != nil {
		t.Fatalf("Exchange() error = %v", err)
	}
	if identity.ExternalID != "wx-unionid" {
		t.Errorf("ExternalID = %q, want the unionid from the token response", identity.ExternalID)
	}
}

// TestWeChatProvider_Exchange_TreatsAnErrcodeAsAFailure covers WeChat's habit
// of reporting failure with HTTP 200 and an errcode in the body. A client that
// only checked the status would treat a refusal as a successful sign-in with
// empty fields.
func TestWeChatProvider_Exchange_TreatsAnErrcodeAsAFailure(t *testing.T) {
	t.Parallel()

	t.Run("on the token endpoint", func(t *testing.T) {
		t.Parallel()
		stub := newWeChatStub(t)
		stub.mu.Lock()
		stub.token = map[string]any{"errcode": 40029, "errmsg": "invalid code"}
		stub.mu.Unlock()

		_, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
		assertErrorCode(t, err, ErrSocialExchangeFailed.Code)
	})

	t.Run("on the user endpoint", func(t *testing.T) {
		t.Parallel()
		stub := newWeChatStub(t)
		stub.mu.Lock()
		stub.userInfo = map[string]any{"errcode": 40003, "errmsg": "invalid openid"}
		stub.mu.Unlock()

		_, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
		assertErrorCode(t, err, ErrSocialExchangeFailed.Code)
	})

	t.Run("on an incomplete token response", func(t *testing.T) {
		t.Parallel()
		stub := newWeChatStub(t)
		stub.mu.Lock()
		stub.token = map[string]any{"access_token": "", "openid": ""}
		stub.mu.Unlock()

		_, err := stub.provider().Exchange(t.Context(), "code", "https://app.example.com/callback")
		assertErrorCode(t, err, ErrSocialExchangeFailed.Code)
	})
}
