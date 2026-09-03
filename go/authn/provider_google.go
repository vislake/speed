package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// Production endpoints for the Google channel.
const (
	// googleIssuer is Google's OpenID Connect issuer, from which the token
	// endpoint and the JWKS location are discovered.
	googleIssuer = "https://accounts.google.com"
	// googleAuthorizePath is the authorization endpoint's path. It is a
	// constant rather than a discovered value because AuthorizeURL builds
	// a URL synchronously, with no context to make a discovery request on
	// and no way to report that the request failed.
	googleAuthorizePath = "/o/oauth2/v2/auth"
	// googleAuthorizeHost is the host the authorization endpoint lives on.
	googleAuthorizeHost = "https://accounts.google.com"
)

// GoogleProvider is the Google social-login channel.
//
// Google is the one channel here that is genuinely standard OpenID Connect,
// so this is the only provider that verifies a signed ID token rather than
// trusting a user-info document fetched with a bearer token. That matters for
// the account-linking rule: the "email_verified" claim arrives inside a
// document signed by Google, which is a materially stronger assertion than a
// field in a JSON body -- and it is why Google is the channel a deployment
// would sensibly put on the trusted list first.
type GoogleProvider struct {
	clientID     string
	clientSecret string
	cfg          providerConfig

	// mu guards the memoized discovery result. Discovery is one HTTP round
	// trip whose answer changes about once a decade, and repeating it on
	// every sign-in would add a third-party dependency to the latency of
	// every login.
	mu       sync.Mutex
	provider *oidc.Provider
}

// NewGoogleProvider returns the Google channel configured with the OAuth
// client credentials a deployment registered with Google.
//
// The default HTTP client is the SSRF-guarded one, which cannot reach a
// private address; a test pointing this provider at a local server injects a
// plain client with WithProviderHTTPClient.
func NewGoogleProvider(clientID, clientSecret string, opts ...ProviderOption) *GoogleProvider {
	return &GoogleProvider{
		clientID:     clientID,
		clientSecret: clientSecret,
		cfg:          newProviderConfig(googleAuthorizeHost, googleIssuer, opts...),
	}
}

// Name implements SocialProvider.
func (p *GoogleProvider) Name() string { return ProviderGoogle }

// AuthorizeURL implements SocialProvider.
//
// No "nonce" is sent. The replay protection here is the state value, which is
// single-use, server-side and bound to the browser that started the flow; a
// nonce would additionally bind the ID TOKEN to the flow, but SocialProvider's
// AuthorizeURL takes no nonce and the enterprise OIDC relying party -- which
// builds its own authorization URL and does use one -- is where that matters,
// because there the same IdP serves many tenants. Recorded rather than left
// for a reader to wonder about.
func (p *GoogleProvider) AuthorizeURL(state, redirectURI string) string {
	query := url.Values{}
	query.Set("client_id", p.clientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("response_type", "code")
	query.Set("scope", "openid email profile")
	query.Set("state", state)
	// Google returns no refresh token and re-prompts nothing by default;
	// this module wants neither, so no access_type or prompt is sent.
	return p.cfg.authBase + googleAuthorizePath + "?" + query.Encode()
}

// discover memoizes the OpenID Connect discovery document.
func (p *GoogleProvider) discover(ctx context.Context) (*oidc.Provider, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.provider != nil {
		return p.provider, nil
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, p.cfg.httpClient), p.cfg.apiBase)
	if err != nil {
		return nil, fmt.Errorf("google discovery: %w", err)
	}
	p.provider = provider
	return provider, nil
}

// googleClaims is the subset of Google's ID token this module reads.
type googleClaims struct {
	Subject       string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified flexBool `json:"email_verified"`
	Name          string   `json:"name"`
	Picture       string   `json:"picture"`
}

// Exchange implements SocialProvider.
func (p *GoogleProvider) Exchange(ctx context.Context, code, redirectURI string) (*ExternalIdentity, error) {
	provider, err := p.discover(ctx)
	if err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(err)
	}

	clientCtx := oidc.ClientContext(ctx, p.cfg.httpClient)
	conf := oauth2.Config{
		ClientID:     p.clientID,
		ClientSecret: p.clientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  redirectURI,
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
	token, err := conf.Exchange(clientCtx, code)
	if err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("google token exchange: %w", err))
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, ErrSocialExchangeFailed.WithCause(errors.New("google returned no id token"))
	}

	idToken, err := provider.VerifierContext(clientCtx, &oidc.Config{ClientID: p.clientID}).Verify(clientCtx, rawIDToken)
	if err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("google id token: %w", err))
	}

	var claims googleClaims
	if claimsErr := idToken.Claims(&claims); claimsErr != nil {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("google id token claims: %w", claimsErr))
	}
	if claims.Subject == "" {
		return nil, ErrSocialIdentityIncomplete.WithParam("provider", ProviderGoogle)
	}

	raw, err := json.Marshal(claims)
	if err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(err)
	}

	return &ExternalIdentity{
		Provider:      ProviderGoogle,
		ExternalID:    claims.Subject,
		Email:         strings.TrimSpace(claims.Email),
		EmailVerified: bool(claims.EmailVerified) && claims.Email != "",
		Name:          claims.Name,
		Avatar:        claims.Picture,
		Raw:           raw,
	}, nil
}

// compile-time check that *GoogleProvider satisfies the channel interface.
var _ SocialProvider = (*GoogleProvider)(nil)
