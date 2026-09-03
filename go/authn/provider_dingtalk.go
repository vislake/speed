package authn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Production endpoints for the DingTalk channel.
const (
	dingtalkAuthHost      = "https://login.dingtalk.com"
	dingtalkAPIHost       = "https://api.dingtalk.com"
	dingtalkAuthorizePath = "/oauth2/auth"
	dingtalkTokenPath     = "/v1.0/oauth2/userAccessToken" //nolint:gosec // an endpoint path, not a credential.
	dingtalkUserPath      = "/v1.0/contact/users/me"

	// dingtalkTokenHeader is the header DingTalk's contact API takes its
	// bearer token in. It is not "Authorization", which is exactly the
	// kind of per-channel difference SocialProvider exists to absorb.
	dingtalkTokenHeader = "x-acs-dingtalk-access-token" //nolint:gosec // a header name, not a credential.
)

// DingTalkProvider is the DingTalk social-login channel.
//
// DingTalk's token endpoint takes a JSON body rather than a form, and its
// contact API takes the access token in its own header rather than in
// Authorization. Neither difference is expressible through a standard OAuth2
// client, which is why this provider speaks HTTP directly.
//
// DingTalk reports an email address for some accounts but makes no
// verification assertion about it, so EmailVerified is always false here and a
// DingTalk identity never auto-links. Reporting the address as verified
// because DingTalk holds it would be inventing an assertion the provider did
// not make -- which is the precise mistake the linking rule exists to stop.
type DingTalkProvider struct {
	clientID     string
	clientSecret string
	cfg          providerConfig
}

// NewDingTalkProvider returns the DingTalk channel configured with an
// application's credentials.
func NewDingTalkProvider(clientID, clientSecret string, opts ...ProviderOption) *DingTalkProvider {
	return &DingTalkProvider{
		clientID:     clientID,
		clientSecret: clientSecret,
		cfg:          newProviderConfig(dingtalkAuthHost, dingtalkAPIHost, opts...),
	}
}

// Name implements SocialProvider.
func (p *DingTalkProvider) Name() string { return ProviderDingTalk }

// AuthorizeURL implements SocialProvider.
func (p *DingTalkProvider) AuthorizeURL(state, redirectURI string) string {
	query := url.Values{}
	query.Set("client_id", p.clientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("response_type", "code")
	query.Set("scope", "openid")
	query.Set("state", state)
	query.Set("prompt", "consent")
	return p.cfg.authBase + dingtalkAuthorizePath + "?" + query.Encode()
}

// dingtalkTokenRequest is the JSON body DingTalk's token endpoint expects.
type dingtalkTokenRequest struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
	Code         string `json:"code"`
	GrantType    string `json:"grantType"`
}

// dingtalkTokenResponse is that endpoint's answer.
type dingtalkTokenResponse struct {
	AccessToken string `json:"accessToken"`
	ExpireIn    int64  `json:"expireIn"`
}

// dingtalkUser is the subset of the contact API's document this module reads.
type dingtalkUser struct {
	Nick      string `json:"nick"`
	UnionID   string `json:"unionId"`
	OpenID    string `json:"openId"`
	AvatarURL string `json:"avatarUrl"`
	Email     string `json:"email"`
	Mobile    string `json:"mobile"`
}

// Exchange implements SocialProvider.
func (p *DingTalkProvider) Exchange(ctx context.Context, code, _ string) (*ExternalIdentity, error) {
	// DingTalk's token endpoint takes no redirect_uri, so the parameter is
	// discarded here. The allowlist check still runs before the flow
	// starts.
	var token dingtalkTokenResponse
	body := dingtalkTokenRequest{
		ClientID:     p.clientID,
		ClientSecret: p.clientSecret,
		Code:         code,
		GrantType:    "authorization_code",
	}
	if err := postJSON(ctx, p.cfg.httpClient, p.cfg.apiBase+dingtalkTokenPath, nil, body, &token); err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("dingtalk token exchange: %w", err))
	}
	if token.AccessToken == "" {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("dingtalk token exchange: no access token"))
	}

	var user dingtalkUser
	headers := map[string]string{dingtalkTokenHeader: token.AccessToken}
	if err := getJSON(ctx, p.cfg.httpClient, p.cfg.apiBase+dingtalkUserPath, headers, &user); err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("dingtalk user: %w", err))
	}

	// unionId is stable across every application of one DingTalk
	// organization; openId is per application, exactly as WeChat's openid
	// is. The same refusal applies for the same reason.
	unionID := strings.TrimSpace(user.UnionID)
	if unionID == "" {
		return nil, ErrSocialIdentityIncomplete.WithParam("provider", ProviderDingTalk)
	}

	// The mobile number DingTalk may return is deliberately dropped rather
	// than carried into ExternalIdentity: it is personal data this module
	// has no use for, and a phone number that arrived without a
	// verification round trip here must never become a login identifier.
	user.Mobile = ""

	raw, err := json.Marshal(user)
	if err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(err)
	}

	return &ExternalIdentity{
		Provider:      ProviderDingTalk,
		ExternalID:    unionID,
		Email:         strings.TrimSpace(user.Email),
		EmailVerified: false,
		Name:          user.Nick,
		Avatar:        user.AvatarURL,
		Raw:           raw,
	}, nil
}

// compile-time check that *DingTalkProvider satisfies the channel interface.
var _ SocialProvider = (*DingTalkProvider)(nil)
