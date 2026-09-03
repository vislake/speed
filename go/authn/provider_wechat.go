package authn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Production endpoints for the WeChat Open Platform channel.
const (
	wechatAuthHost      = "https://open.weixin.qq.com"
	wechatAPIHost       = "https://api.weixin.qq.com"
	wechatAuthorizePath = "/connect/qrconnect"
	wechatTokenPath     = "/sns/oauth2/access_token" //nolint:gosec // an endpoint path, not a credential.
	wechatUserInfoPath  = "/sns/userinfo"
)

// WeChatProvider is the WeChat Open Platform social-login channel.
//
// It is the clearest example of why SocialProvider is not a standard OIDC
// client. WeChat sends "appid" and "secret" where OAuth2 says "client_id" and
// "client_secret"; its token endpoint is a GET; its errors arrive with HTTP
// 200 and an "errcode" in the body; and it reports no email address at all.
//
// The last of those has a direct consequence for account linking: a WeChat
// identity always has EmailVerified false and no address, so it can never
// auto-link to an existing account. That is not a limitation to work around.
// It is the correct outcome -- there is nothing to link ON.
type WeChatProvider struct {
	appID     string
	appSecret string
	cfg       providerConfig
}

// NewWeChatProvider returns the WeChat channel. appID and appSecret are the
// Open Platform application's credentials -- WeChat's own names for what
// other channels call the client id and client secret.
func NewWeChatProvider(appID, appSecret string, opts ...ProviderOption) *WeChatProvider {
	return &WeChatProvider{
		appID:     appID,
		appSecret: appSecret,
		cfg:       newProviderConfig(wechatAuthHost, wechatAPIHost, opts...),
	}
}

// Name implements SocialProvider.
func (p *WeChatProvider) Name() string { return ProviderWeChat }

// AuthorizeURL implements SocialProvider.
//
// The "#wechat_redirect" fragment is required by WeChat and is appended after
// the query string, which is why the URL is assembled by hand rather than
// through url.URL: it is part of WeChat's contract, not a fragment this
// module chose to carry.
func (p *WeChatProvider) AuthorizeURL(state, redirectURI string) string {
	query := url.Values{}
	query.Set("appid", p.appID)
	query.Set("redirect_uri", redirectURI)
	query.Set("response_type", "code")
	query.Set("scope", "snsapi_login")
	query.Set("state", state)
	return p.cfg.authBase + wechatAuthorizePath + "?" + query.Encode() + "#wechat_redirect"
}

// wechatError is the error envelope WeChat returns with HTTP 200.
type wechatError struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// wechatToken is WeChat's token-endpoint response.
type wechatToken struct {
	wechatError
	AccessToken string `json:"access_token"`
	OpenID      string `json:"openid"`
	UnionID     string `json:"unionid"`
	Scope       string `json:"scope"`
}

// wechatUserInfo is WeChat's user-information response.
type wechatUserInfo struct {
	wechatError
	OpenID     string `json:"openid"`
	UnionID    string `json:"unionid"`
	Nickname   string `json:"nickname"`
	HeadImgURL string `json:"headimgurl"`
}

// Exchange implements SocialProvider.
func (p *WeChatProvider) Exchange(ctx context.Context, code, _ string) (*ExternalIdentity, error) {
	// WeChat's token endpoint ignores redirect_uri entirely, which is why
	// the parameter is discarded here rather than passed on. The redirect
	// URI is still checked against the allowlist before the flow starts.
	tokenQuery := url.Values{}
	tokenQuery.Set("appid", p.appID)
	tokenQuery.Set("secret", p.appSecret)
	tokenQuery.Set("code", code)
	tokenQuery.Set("grant_type", "authorization_code")

	var token wechatToken
	endpoint := p.cfg.apiBase + wechatTokenPath + "?" + tokenQuery.Encode()
	if err := getJSON(ctx, p.cfg.httpClient, endpoint, nil, &token); err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("wechat token exchange: %w", err))
	}
	if token.ErrCode != 0 {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("wechat token exchange: errcode %d", token.ErrCode))
	}
	if token.AccessToken == "" || token.OpenID == "" {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("wechat token exchange: incomplete response"))
	}

	infoQuery := url.Values{}
	infoQuery.Set("access_token", token.AccessToken)
	infoQuery.Set("openid", token.OpenID)

	var info wechatUserInfo
	infoEndpoint := p.cfg.apiBase + wechatUserInfoPath + "?" + infoQuery.Encode()
	if err := getJSON(ctx, p.cfg.httpClient, infoEndpoint, nil, &info); err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("wechat user info: %w", err))
	}
	if info.ErrCode != 0 {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("wechat user info: errcode %d", info.ErrCode))
	}

	// THE WECHAT TRAP, and the reason this provider refuses rather than
	// falls back. openid identifies a person WITHIN ONE APPLICATION: the
	// same human signing in to a second application of the same Open
	// Platform account gets a different openid. unionid is the identifier
	// that is stable across them.
	//
	// Keying on openid works perfectly in a single-application deployment
	// and then, the day a second application is added, silently splits
	// every existing user into two accounts -- with no error, no log line
	// and no way to merge them afterwards, because by then both accounts
	// have data. Refusing here turns that into a configuration error at
	// integration time: unionid is absent when the application has not
	// been bound to an Open Platform account, which is a thing an operator
	// can fix in ten minutes on day one and cannot fix at all on day 400.
	unionID := strings.TrimSpace(firstNonEmpty(info.UnionID, token.UnionID))
	if unionID == "" {
		return nil, ErrSocialIdentityIncomplete.WithParam("provider", ProviderWeChat)
	}

	raw, err := json.Marshal(info)
	if err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(err)
	}

	return &ExternalIdentity{
		Provider:   ProviderWeChat,
		ExternalID: unionID,
		// WeChat reports no email address, so there is nothing to
		// verify and nothing to link on. Both fields are left at their
		// zero values deliberately.
		Email:         "",
		EmailVerified: false,
		Name:          info.Nickname,
		Avatar:        info.HeadImgURL,
		Raw:           raw,
	}, nil
}

// firstNonEmpty returns the first non-empty value, or the empty string.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// compile-time check that *WeChatProvider satisfies the channel interface.
var _ SocialProvider = (*WeChatProvider)(nil)
