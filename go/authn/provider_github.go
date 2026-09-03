package authn

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/oauth2"
)

// Production endpoints for the GitHub channel. GitHub splits them across two
// hosts, which is why providerConfig carries two base URLs.
const (
	githubAuthHost      = "https://github.com"
	githubAPIHost       = "https://api.github.com"
	githubAuthorizePath = "/login/oauth/authorize"
	githubTokenPath     = "/login/oauth/access_token" //nolint:gosec // an endpoint path, not a credential.
	githubUserPath      = "/user"
	githubEmailsPath    = "/user/emails"

	// githubAPIVersionHeader pins the REST API version, so a future
	// default at GitHub cannot change the shape of the two documents this
	// provider parses.
	githubAPIVersionHeader = "2022-11-28"
)

// GitHubProvider is the GitHub social-login channel.
//
// GitHub is plain OAuth2 and issues no ID token at all, so everything about
// the person arrives as an ordinary API response fetched with the access
// token. The consequence for the account-linking rule is that GitHub's
// "verified" flag is a field in a JSON body rather than a claim in a signed
// document -- a weaker assertion than Google's, though still an assertion the
// provider is making, which is what the trusted list exists to let a
// deployment weigh.
type GitHubProvider struct {
	clientID     string
	clientSecret string
	cfg          providerConfig
}

// NewGitHubProvider returns the GitHub channel configured with a deployment's
// OAuth app credentials.
func NewGitHubProvider(clientID, clientSecret string, opts ...ProviderOption) *GitHubProvider {
	return &GitHubProvider{
		clientID:     clientID,
		clientSecret: clientSecret,
		cfg:          newProviderConfig(githubAuthHost, githubAPIHost, opts...),
	}
}

// Name implements SocialProvider.
func (p *GitHubProvider) Name() string { return ProviderGitHub }

// AuthorizeURL implements SocialProvider.
func (p *GitHubProvider) AuthorizeURL(state, redirectURI string) string {
	query := url.Values{}
	query.Set("client_id", p.clientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("state", state)
	// "user:email" is requested because GitHub omits a private primary
	// address from /user entirely; without it a user whose address is
	// private looks to this module like an account with no email at all.
	query.Set("scope", "read:user user:email")
	return p.cfg.authBase + githubAuthorizePath + "?" + query.Encode()
}

// githubUser is the subset of GitHub's /user document this module reads.
type githubUser struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	AvatarURL string `json:"avatar_url"`
}

// githubEmail is one entry of GitHub's /user/emails document.
type githubEmail struct {
	Email    string `json:"email"`
	Primary  bool   `json:"primary"`
	Verified bool   `json:"verified"`
}

// Exchange implements SocialProvider.
func (p *GitHubProvider) Exchange(ctx context.Context, code, redirectURI string) (*ExternalIdentity, error) {
	conf := oauth2.Config{
		ClientID:     p.clientID,
		ClientSecret: p.clientSecret,
		Endpoint: oauth2.Endpoint{
			AuthURL:  p.cfg.authBase + githubAuthorizePath,
			TokenURL: p.cfg.authBase + githubTokenPath,
		},
		RedirectURL: redirectURI,
		Scopes:      []string{"read:user", "user:email"},
	}
	token, err := conf.Exchange(context.WithValue(ctx, oauth2.HTTPClient, p.cfg.httpClient), code)
	if err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("github token exchange: %w", err))
	}
	if token.AccessToken == "" {
		return nil, ErrSocialExchangeFailed.WithCause(errors.New("github returned no access token"))
	}

	headers := map[string]string{
		"Authorization":        "Bearer " + token.AccessToken,
		"X-GitHub-Api-Version": githubAPIVersionHeader,
	}

	var user githubUser
	if userErr := getJSON(ctx, p.cfg.httpClient, p.cfg.apiBase+githubUserPath, headers, &user); userErr != nil {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("github user: %w", userErr))
	}
	if user.ID == 0 {
		// GitHub's numeric id is the only identifier that survives a
		// rename; the login does not, and keying on it would hand a
		// renamed account's identity to whoever claims the freed name.
		return nil, ErrSocialIdentityIncomplete.WithParam("provider", ProviderGitHub)
	}

	var emails []githubEmail
	if emailsErr := getJSON(ctx, p.cfg.httpClient, p.cfg.apiBase+githubEmailsPath, headers, &emails); emailsErr != nil {
		return nil, ErrSocialExchangeFailed.WithCause(fmt.Errorf("github emails: %w", emailsErr))
	}

	address, verified := githubPrimaryEmail(emails)
	if address == "" {
		// Fall back to whatever /user carried, but never as VERIFIED:
		// that document says nothing about verification, and inferring
		// it would be exactly the guess the linking rule forbids.
		address, verified = strings.TrimSpace(user.Email), false
	}

	name := user.Name
	if name == "" {
		name = user.Login
	}

	raw, err := json.Marshal(user)
	if err != nil {
		return nil, ErrSocialExchangeFailed.WithCause(err)
	}

	return &ExternalIdentity{
		Provider:      ProviderGitHub,
		ExternalID:    strconv.FormatInt(user.ID, 10),
		Email:         address,
		EmailVerified: verified && address != "",
		Name:          name,
		Avatar:        user.AvatarURL,
		Raw:           raw,
	}, nil
}

// githubPrimaryEmail returns the account's PRIMARY address and whether GitHub
// reports it verified.
//
// Only the primary entry is considered. A GitHub account may carry several
// verified addresses, and accepting any of them would let somebody who added
// (and verified) a second address at GitHub link into whichever account here
// happens to use it. The primary address is the one GitHub itself treats as
// the account's identity.
func githubPrimaryEmail(emails []githubEmail) (address string, verified bool) {
	for _, entry := range emails {
		if entry.Primary {
			return strings.TrimSpace(entry.Email), entry.Verified
		}
	}
	return "", false
}

// compile-time check that *GitHubProvider satisfies the channel interface.
var _ SocialProvider = (*GitHubProvider)(nil)
