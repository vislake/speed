package authn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Production endpoints for the Feishu / Lark channel.
const (
	feishuAuthHost        = "https://open.feishu.cn"
	feishuAPIHost         = "https://open.feishu.cn"
	feishuAuthorizePath   = "/open-apis/authen/v1/authorize"
	feishuAppTokenPath    = "/open-apis/auth/v3/app_access_token/internal" //nolint:gosec // an endpoint path, not a credential.
	feishuUserTokenPath   = "/open-apis/authen/v1/oidc/access_token"       //nolint:gosec // an endpoint path, not a credential.
	feishuUserInfoPath    = "/open-apis/authen/v1/user_info"
	feishuSuccessCodeZero = 0
)

// FeishuProvider is the Feishu (Lark) social-login channel.
//
// Feishu needs three round trips rather than two: an APPLICATION token is
// obtained first, and only that token may exchange a user's authorization
// code. The extra hop exists because Feishu authenticates the application
// separately from the user, and it is why this channel cannot be expressed as
// an oauth2.Config at all.
//
// Feishu reports an email for some accounts and makes no verification
// assertion about it, so EmailVerified is always false here for the same
// reason as DingTalk.
type FeishuProvider struct {
	appID     string
	appSecret string
	cfg       providerConfig
}

// NewFeishuProvider returns the Feishu channel configured with an
// application's credentials.
func NewFeishuProvider(appID, appSecret string, opts ...ProviderOption) *FeishuProvider {
	return &FeishuProvider{
		appID:     appID,
		appSecret: appSecret,
		cfg:       newProviderConfig(feishuAuthHost, feishuAPIHost, opts...),
	}
}

// Name implements SocialProvider.
func (p *FeishuProvider) Name() string { return ProviderFeishu }

// AuthorizeURL implements SocialProvider.
func (p *FeishuProvider) AuthorizeURL(state, redirectURI string) string {
	query := url.Values{}
	query.Set("app_id", p.appID)
	query.Set("redirect_uri", redirectURI)
	query.Set("state", state)
	return p.cfg.authBase + feishuAuthorizePath + "?" + query.Encode()
}

// feishuAppTokenRequest is the body of the application-token call.
type feishuAppTokenRequest struct {
	AppID     string `json:"app_id"`
	AppSecret string `json:"app_secret"`
}

// feishuAppTokenResponse is its answer. Feishu signals failure with a
// non-zero "code" inside an HTTP 200, like WeChat.
type feishuAppTokenResponse struct {
	Code           int    `json:"code"`
	Msg            string `json:"msg"`
	AppAccessToken string `json:"app_access_token"`
}

// feishuUserTokenRequest is the body of the user-token call.
type feishuUserTokenRequest struct {
	GrantType string `json:"grant_type"`
	Code      string `json:"code"`
}

// feishuUserTokenResponse is its answer.
type feishuUserTokenResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		AccessToken string `json:"access_token"`
	} `json:"data"`
}

// feishuUserInfoResponse is the user-information answer.
type feishuUserInfoResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Name            string `json:"name"`
		AvatarURL       string `json:"avatar_url"`
		Email           string `json:"email"`
		EnterpriseEmail string `json:"enterprise_email"`
		OpenID          string `json:"open_id"`
		UnionID         string `json:"union_id"`
	} `json:"data"`
}

// Exchange implements SocialProvider.
func (p *FeishuProvider) Exchange(ctx context.Context, code, _ string) (*ExternalIdentity, error) {
	// Feishu's token endpoints take no redirect_uri; the allowlist check
	// still runs before the flow starts.
	var appToken feishuAppTokenResponse
	appBody := feishuAppTokenRequest{AppID: p.appID, AppSecret: p.appSecret}
	if err := postJSON(ctx, p.cfg.httpClient, p.cfg.apiBase+feishuAppTokenPath, nil, appBody, &appToken); err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("feishu app token: %w", err))
	}
	if appToken.Code != feishuSuccessCodeZero || appToken.AppAccessToken == "" {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("feishu app token: code %d", appToken.Code))
	}

	appHeaders := map[string]string{"Authorization": "Bearer " + appToken.AppAccessToken}

	var userToken feishuUserTokenResponse
	userBody := feishuUserTokenRequest{GrantType: "authorization_code", Code: code}
	if err := postJSON(ctx, p.cfg.httpClient, p.cfg.apiBase+feishuUserTokenPath, appHeaders, userBody, &userToken); err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("feishu user token: %w", err))
	}
	if userToken.Code != feishuSuccessCodeZero || userToken.Data.AccessToken == "" {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("feishu user token: code %d", userToken.Code))
	}

	var info feishuUserInfoResponse
	userHeaders := map[string]string{"Authorization": "Bearer " + userToken.Data.AccessToken}
	if err := getJSON(ctx, p.cfg.httpClient, p.cfg.apiBase+feishuUserInfoPath, userHeaders, &info); err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("feishu user info: %w", err))
	}
	if info.Code != feishuSuccessCodeZero {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("feishu user info: code %d", info.Code))
	}

	// union_id is stable across the applications of one Feishu tenant;
	// open_id is per application, like WeChat's openid.
	unionID := strings.TrimSpace(info.Data.UnionID)
	if unionID == "" {
		return nil, ErrSocialIdentityIncomplete.WithParam("provider", ProviderFeishu)
	}

	raw, err := json.Marshal(info.Data)
	if err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(err)
	}

	return &ExternalIdentity{
		Provider:      ProviderFeishu,
		ExternalID:    unionID,
		Email:         strings.TrimSpace(firstNonEmpty(info.Data.EnterpriseEmail, info.Data.Email)),
		EmailVerified: false,
		Name:          info.Data.Name,
		Avatar:        info.Data.AvatarURL,
		Raw:           raw,
	}, nil
}

// compile-time check that *FeishuProvider satisfies the channel interface.
var _ SocialProvider = (*FeishuProvider)(nil)
