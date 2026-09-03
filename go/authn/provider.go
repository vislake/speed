package authn

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/authn/internal/safehttp"
)

// Provider names for the channels this module ships. They are the values of
// UserIdentity.Provider and the suffix of every per-channel configuration key
// and feature flag, so they are constants rather than strings typed at each
// call site.
const (
	// ProviderGoogle is Google's standard OpenID Connect channel.
	ProviderGoogle = "google"
	// ProviderGitHub is GitHub's plain OAuth2 channel, which issues no ID
	// token at all.
	ProviderGitHub = "github"
	// ProviderWeChat is the WeChat Open Platform channel (open.weixin.qq.com).
	ProviderWeChat = "wechat"
	// ProviderDingTalk is the DingTalk channel.
	ProviderDingTalk = "dingtalk"
	// ProviderFeishu is the Feishu / Lark channel.
	ProviderFeishu = "feishu"

	// ProviderOIDCPrefix is the prefix of the synthetic provider name an
	// ENTERPRISE single sign-on identity is stored under: "oidc:<tenant>".
	//
	// Enterprise SSO is per tenant, and two tenants may federate with
	// IdPs that hand out the same subject identifier. user_identities is
	// unique on (provider, external_id) -- a global constraint, because
	// the table is identity-domain data with no tenant column -- so the
	// tenant has to appear on one side of that pair. It goes in the
	// provider name rather than in external_id so that external_id stays
	// exactly the "sub" claim the IdP issued, which is what an operator
	// will compare against their own directory.
	ProviderOIDCPrefix = "oidc:"
)

// ExternalIdentity is what a provider reports about the person who just
// authorized. It is the shape docs/internal/05-identity-and-access.md pins,
// and every field of it is UNTRUSTED input from a third party.
//
// EmailVerified is the field the whole account-linking rule turns on, and it
// means one specific thing: "the provider asserts it checked that this person
// controls this address". A provider that does not say so, or that has no
// concept of email at all, yields false -- never a hopeful default.
type ExternalIdentity struct {
	// Provider is the channel this identity came from, one of the
	// Provider* constants.
	Provider string

	// ExternalID is the provider's stable unique identifier for the
	// person. It must be stable across logins and across applications --
	// see the WeChat unionid rule in this module's AGENTS.md.
	ExternalID string

	// Email is the address the provider reported, or empty.
	Email string

	// EmailVerified reports whether the provider asserts the address was
	// verified by the provider itself.
	EmailVerified bool

	// Name is the display name the provider reported.
	Name string

	// Avatar is a URL to the person's picture at the provider. It is
	// stored as given and never fetched by this module.
	Avatar string

	// Raw is the provider's own user document, kept for support and for
	// mapping fields this struct does not model. It is not indexed and
	// never returned by an API.
	Raw json.RawMessage
}

// SocialProvider is one social-login channel.
//
// The interface exists instead of a standard OIDC client because the channels
// are not the same protocol. Google is OpenID Connect; GitHub is plain OAuth2
// with no ID token; WeChat sends "appid" and "secret" rather than "client_id"
// and "client_secret" and answers with its own JSON shape; DingTalk posts a
// JSON body to its token endpoint; Feishu needs an application token before
// it will exchange a user's code at all. Every one of those differences is
// load-bearing, and an abstraction that hid them would have to be reopened
// for each channel anyway.
//
// Exchange takes redirectURI rather than holding one as construction state:
// OAuth2 requires the redirect_uri presented at the token endpoint to equal
// the one used at the authorization endpoint, and this module allows a
// deployment to configure SEVERAL (a web origin and a mobile scheme, say),
// checked against RedirectAllowlist. A provider that captured a single
// redirect URI at construction could not serve both. This is the one place
// the interface deviates from the sketch in
// docs/internal/05-identity-and-access.md, which predates the allowlist rule.
type SocialProvider interface {
	// Name returns the channel's Provider* constant.
	Name() string

	// AuthorizeURL builds the URL to send the browser to. state is the
	// single-use, session-bound value StateStore issued.
	AuthorizeURL(state, redirectURI string) string

	// Exchange trades an authorization code for the person's external
	// identity. Every error it returns is a wrapped ErrSocialExchangeFailed
	// or ErrSocialIdentityIncomplete -- a provider's own error text never
	// reaches a client.
	Exchange(ctx context.Context, code, redirectURI string) (*ExternalIdentity, error)
}

// ProviderRegistry is the set of channels a deployment has wired.
//
// It is immutable after construction: the channels available to a login page
// are decided during host bootstrap, and a registry that could grow at
// runtime would need locking on the hot path to answer a question that never
// changes.
type ProviderRegistry struct {
	byName map[string]SocialProvider
}

// NewProviderRegistry indexes providers by name, rejecting a nil entry, an
// entry with no name and a duplicate -- each of which is a wiring mistake
// that would otherwise surface as a channel silently missing from the login
// page.
func NewProviderRegistry(providers ...SocialProvider) (*ProviderRegistry, error) {
	byName := make(map[string]SocialProvider, len(providers))
	for i, provider := range providers {
		if provider == nil {
			return nil, fmt.Errorf("authn: social provider at index %d is nil", i)
		}
		name := provider.Name()
		if name == "" {
			return nil, fmt.Errorf("authn: social provider at index %d has no name", i)
		}
		if _, exists := byName[name]; exists {
			return nil, fmt.Errorf("authn: social provider %q is registered twice", name)
		}
		byName[name] = provider
	}
	return &ProviderRegistry{byName: byName}, nil
}

// Get returns the provider registered under name.
func (r *ProviderRegistry) Get(name string) (SocialProvider, bool) {
	if r == nil {
		return nil, false
	}
	provider, ok := r.byName[name]
	return provider, ok
}

// Names returns every registered channel name, sorted, for the login page's
// enabled-channel list.
func (r *ProviderRegistry) Names() []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.byName))
	for name := range r.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// providerConfig is the transport and endpoint configuration every provider
// shares.
//
// The two base URLs exist because every one of these channels splits its
// endpoints across two hosts: GitHub authorizes on github.com and answers API
// calls on api.github.com, WeChat authorizes on open.weixin.qq.com and
// answers on api.weixin.qq.com, and so on. Both default to the channel's
// production hosts and both are overridable, which is what lets every
// provider test in this module run against an httptest server and make no
// network call at all.
type providerConfig struct {
	httpClient *http.Client
	authBase   string
	apiBase    string
}

// ProviderOption configures a social provider.
type ProviderOption func(*providerConfig)

// WithProviderHTTPClient replaces the HTTP client a provider talks to its
// channel with. The default is the SSRF-guarded client, which cannot reach a
// private address -- so a test pointing a provider at an httptest server on
// loopback MUST inject a plain client, and does.
func WithProviderHTTPClient(client *http.Client) ProviderOption {
	return func(c *providerConfig) {
		if client != nil {
			c.httpClient = client
		}
	}
}

// WithProviderAuthBaseURL overrides the host the authorization endpoint lives
// on. An empty value is ignored.
func WithProviderAuthBaseURL(base string) ProviderOption {
	return func(c *providerConfig) {
		if base != "" {
			c.authBase = strings.TrimSuffix(base, "/")
		}
	}
}

// WithProviderAPIBaseURL overrides the host the token and user-info endpoints
// live on. An empty value is ignored.
func WithProviderAPIBaseURL(base string) ProviderOption {
	return func(c *providerConfig) {
		if base != "" {
			c.apiBase = strings.TrimSuffix(base, "/")
		}
	}
}

// newProviderConfig applies opts over the channel's production defaults.
func newProviderConfig(authBase, apiBase string, opts ...ProviderOption) providerConfig {
	cfg := providerConfig{
		httpClient: safehttp.NewClient(),
		authBase:   strings.TrimSuffix(authBase, "/"),
		apiBase:    strings.TrimSuffix(apiBase, "/"),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	return cfg
}

// maxProviderResponseBytes bounds a provider's response body. A third party
// that answers with an unbounded stream must not be able to exhaust this
// process's memory, and no legitimate user document is anywhere near this
// size.
const maxProviderResponseBytes = 1 << 20

// doJSON performs req and decodes a JSON response into out.
//
// It bounds the body, refuses a non-2xx status, and -- importantly -- returns
// an error carrying only what this module chose to say. A provider's own
// error body frequently contains the client secret it was sent or an internal
// hostname, so it is never propagated verbatim.
func doJSON(ctx context.Context, client *http.Client, req *http.Request, out any) error {
	if client == nil {
		return errors.New("authn: social provider has no http client")
	}
	// #nosec G704 -- gosec's taint analysis cannot see that newProviderConfig's
	// default client is safehttp.NewClient(), which resolves and dials the
	// exact validated IP (internal/safehttp), rejecting private/loopback/
	// link-local/CGNAT ranges and defeating DNS rebinding. A caller who
	// overrides it via WithProviderHTTPClient does so explicitly -- tests use
	// this to point at an httptest server, the one legitimate reason to
	// bypass the guard.
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("request %s: %w", req.URL.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxProviderResponseBytes))
	if err != nil {
		return fmt.Errorf("read the response from %s: %w", req.URL.Host, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%s answered with status %d", req.URL.Host, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode the response from %s: %w", req.URL.Host, err)
	}
	return nil
}

// getJSON issues a GET and decodes the JSON answer, applying each header.
func getJSON(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, out any) error {
	// #nosec G704 -- the same false positive doJSON's own #nosec comment
	// below justifies: gosec's taint analysis flags request construction
	// with a variable URL regardless of the client that later executes it,
	// and cannot see that client is safehttp.NewClient() by default (or an
	// explicit test override) either way.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build the request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return doJSON(ctx, client, req, out)
}

// postJSON issues a POST with a JSON body and decodes the JSON answer.
func postJSON(ctx context.Context, client *http.Client, endpoint string, headers map[string]string, in, out any) error {
	encoded, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("encode the request: %w", err)
	}
	// #nosec G704 -- the same false positive as getJSON's identical comment
	// just above.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("build the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return doJSON(ctx, client, req, out)
}

// RedirectAllowlist is the set of redirect URIs a deployment permits an
// authorization flow to come back to.
//
// Matching is EXACT string equality, deliberately. Prefix matching on a
// redirect URI is the single most common way an OAuth deployment is taken
// over: "https://app.example.com" as a prefix also allows
// "https://app.example.com.attacker.test", and a path prefix also allows an
// open redirector further down the same site. An allowlist that a reader has
// to reason about is not an allowlist.
type RedirectAllowlist struct {
	uris []string
}

// NewRedirectAllowlist validates and stores uris. Each must be an absolute
// URL with no fragment: the fragment is where an implicit-flow token would
// land, and a registered URI carrying one is always a mistake.
func NewRedirectAllowlist(uris ...string) (RedirectAllowlist, error) {
	cleaned := make([]string, 0, len(uris))
	for _, raw := range uris {
		uri := strings.TrimSpace(raw)
		if uri == "" {
			continue
		}
		parsed, err := url.Parse(uri)
		if err != nil {
			return RedirectAllowlist{}, fmt.Errorf("authn: redirect uri %q is not a URL: %w", uri, err)
		}
		if !parsed.IsAbs() || parsed.Host == "" {
			return RedirectAllowlist{}, fmt.Errorf("authn: redirect uri %q must be absolute", uri)
		}
		if parsed.Fragment != "" || strings.Contains(uri, "#") {
			return RedirectAllowlist{}, fmt.Errorf("authn: redirect uri %q must not carry a fragment", uri)
		}
		if !slices.Contains(cleaned, uri) {
			cleaned = append(cleaned, uri)
		}
	}
	return RedirectAllowlist{uris: cleaned}, nil
}

// Allows reports whether uri is registered, by exact match.
func (a RedirectAllowlist) Allows(uri string) bool {
	return slices.Contains(a.uris, uri)
}

// URIs returns the registered redirect URIs.
func (a RedirectAllowlist) URIs() []string { return slices.Clone(a.uris) }

// Empty reports whether nothing is registered, which makes every redirect URI
// refused. That is the correct closed default: a deployment that has not said
// where an authorization flow may return to has not enabled social login.
func (a RedirectAllowlist) Empty() bool { return len(a.uris) == 0 }

// DefaultOAuthStateTTL bounds how long an authorization flow may take. Ten
// minutes is long enough for a person to type a password and approve a
// consent screen at a provider, and short enough that a state value captured
// from a browser's history or a proxy log is usually already dead.
const DefaultOAuthStateTTL = 10 * time.Minute

// oauthStateKeyPrefix namespaces state records in the shared key-value store.
const oauthStateKeyPrefix = "authn:oauth_state:"

// stateConsumedMarker is what a consumed state record is swapped to. The
// record is not deleted, so a replay within the TTL is DISTINGUISHABLE from a
// state that never existed -- which is what lets a replay be reported rather
// than silently look like an expired flow.
var stateConsumedMarker = []byte("consumed")

// StateBinding is what a state value commits the eventual callback to.
type StateBinding struct {
	// Provider is the channel the flow was started for. A callback that
	// arrives at a different channel's endpoint is refused, so a code
	// issued by one provider cannot be presented to another.
	Provider string

	// RedirectURI is the exact redirect URI the authorization request
	// used, which the token exchange must repeat.
	RedirectURI string

	// SessionBinding ties the flow to the browser that started it -- the
	// pre-authentication cookie's value, hashed by the caller. Without it
	// an attacker can start a flow of their own and trick a victim's
	// browser into completing it, which silently binds the ATTACKER's
	// social account to the victim's session.
	SessionBinding string

	// LinkUserID is set when an already-signed-in user started the flow to
	// BIND a new identity rather than to sign in. Its presence is what
	// makes the callback a binding rather than a login, and it comes from
	// the server-side record rather than from the callback request, so a
	// caller cannot turn a login into a binding by editing a parameter.
	LinkUserID string

	// Nonce is the OpenID Connect nonce for channels that issue an ID
	// token, echoed in the token and compared here.
	Nonce string
}

// StateStore issues and consumes the single-use "state" values that make an
// authorization callback non-forgeable.
type StateStore struct {
	kv  pkgcore.KVStore
	ttl time.Duration
}

// NewStateStore binds kv, which is the pkgcore seam, so the same code runs in
// both deployment modes: an in-memory store in the standalone one, Redis in
// the distributed one, and neither is named here.
func NewStateStore(kv pkgcore.KVStore, ttl time.Duration) (*StateStore, error) {
	if kv == nil {
		return nil, errors.New("authn: NewStateStore requires a key-value store")
	}
	if ttl <= 0 {
		ttl = DefaultOAuthStateTTL
	}
	return &StateStore{kv: kv, ttl: ttl}, nil
}

// Issue mints a state value for binding and records it.
//
// The value returned to the browser is 256 bits of randomness; what is stored
// is keyed by its SHA-256 digest. That means a dump of the key-value store
// does not yield usable state values, for the same reason refresh tokens are
// stored hashed.
func (s *StateStore) Issue(ctx context.Context, binding StateBinding) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("authn: generate oauth state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(raw)

	encoded, err := json.Marshal(binding)
	if err != nil {
		return "", fmt.Errorf("authn: encode oauth state: %w", err)
	}
	if err := s.kv.Set(ctx, stateKey(state), encoded, s.ttl); err != nil {
		return "", fmt.Errorf("authn: store oauth state: %w", err)
	}
	return state, nil
}

// Consume validates state exactly once and returns what it was bound to.
//
// Single use is enforced with CompareAndSwap rather than with a read followed
// by a delete: two callbacks arriving with the same state at the same instant
// must produce exactly one winner, and only the store can arbitrate that.
// CompareAndSwap is on the KVStore interface precisely because both
// deployment modes can honour it.
//
// sessionBinding must equal what Issue recorded. A caller with no
// pre-authentication cookie passes the empty string, which matches only a
// flow that was started without one.
func (s *StateStore) Consume(ctx context.Context, state, provider, sessionBinding string) (StateBinding, error) {
	if state == "" {
		return StateBinding{}, ErrOAuthStateInvalid
	}
	key := stateKey(state)

	stored, found, err := s.kv.Get(ctx, key)
	if err != nil {
		return StateBinding{}, ErrOAuthStateInvalid.WithCause(err)
	}
	if !found || bytes.Equal(stored, stateConsumedMarker) {
		// Expired, never issued, or already used. All three are the
		// same answer to the caller.
		return StateBinding{}, ErrOAuthStateInvalid
	}

	swapped, err := s.kv.CompareAndSwap(ctx, key, stored, stateConsumedMarker)
	if err != nil {
		return StateBinding{}, ErrOAuthStateInvalid.WithCause(err)
	}
	if !swapped {
		// Somebody else consumed it between the read and the swap.
		return StateBinding{}, ErrOAuthStateInvalid
	}

	var binding StateBinding
	if err := json.Unmarshal(stored, &binding); err != nil {
		return StateBinding{}, ErrOAuthStateInvalid.WithCause(err)
	}
	if binding.Provider != provider {
		return StateBinding{}, ErrOAuthStateInvalid
	}
	if binding.SessionBinding != sessionBinding {
		return StateBinding{}, ErrOAuthStateInvalid
	}
	return binding, nil
}

// stateKey is the key a state value is stored under.
func stateKey(state string) string {
	digest := sha256.Sum256([]byte(state))
	return oauthStateKeyPrefix + hex.EncodeToString(digest[:])
}

// BindingFromCookie derives the SessionBinding value from a pre-authentication
// cookie, so the raw cookie is never written into the key-value store.
func BindingFromCookie(value string) string {
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

// NewOAuthNonce returns a fresh OpenID Connect nonce.
func NewOAuthNonce() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("authn: generate oidc nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// flexBool decodes a JSON boolean that some identity providers send as a
// string.
//
// The OpenID Connect specification says "email_verified" is a boolean, and
// most providers send one. Several well-known ones send "true" or "false" as
// a string instead. A decoder that rejected those would fail the login
// outright; a decoder that treated a decode failure as false would silently
// downgrade a verified address to unverified, which changes what the
// account-linking rule does. Neither is acceptable, so the string form is
// accepted explicitly and anything else is an error.
//
// Note what is NOT accepted: "1", "yes", "TRUE". Widening the set here widens
// what counts as a verified email address, which is the exact input the
// auto-link rule turns on. A JSON null decodes to false, which is the safe
// direction -- an absent claim is not an assertion.
type flexBool bool

// UnmarshalJSON implements json.Unmarshaler.
func (b *flexBool) UnmarshalJSON(data []byte) error {
	var asBool bool
	if err := json.Unmarshal(data, &asBool); err == nil {
		*b = flexBool(asBool)
		return nil
	}
	var asString string
	if err := json.Unmarshal(data, &asString); err != nil {
		return fmt.Errorf("authn: %q is neither a boolean nor a string", string(data))
	}
	switch asString {
	case "true":
		*b = true
	case "false":
		*b = false
	default:
		return fmt.Errorf("authn: %q is not a boolean", asString)
	}
	return nil
}

// MarshalJSON implements json.Marshaler, so a decoded claim set can be
// re-encoded into ExternalIdentity.Raw without changing shape.
func (b flexBool) MarshalJSON() ([]byte, error) { return json.Marshal(bool(b)) }
