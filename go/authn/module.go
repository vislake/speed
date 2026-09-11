package authn

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/authn/locales"
	"github.com/vislake/speed/go/authn/migrations"
)

//go:embed api/openapi.yaml
var openAPISpecYAML []byte

const (
	// moduleName is authn's the module contract's Name(). It is also the key
	// dbkit.MigrationRegistry uses in its dependency graph and the prefix
	// of every message id, error code, event type and audit action this
	// module owns.
	moduleName = "authn"

	// apiPath is the path this module's HTTP surface is mounted at (see
	// Register below). It must agree with the path prefix declared in
	// this module's own OpenAPI fragment (api/openapi.yaml) -- for the
	// same reason notes' identical apiPath constant gives: the
	// fragment's "paths:" keys are what oapi-codegen turns into the
	// method+path patterns of the generated registration helpers (see
	// api/authn-server.gen.go's HandlerWithOptions) and into the
	// api.ServerInterface method set Handler implements, so a request can
	// only reach Handler through a route mounted at apiPath, and
	// Handler's generated inner router only serves the fragment's own
	// paths under it.
	apiPath = "/api/v1/authn"

	// blindIndexKeySize is the required length of the blind-index key, in
	// bytes. It matches dbkit's own 32-byte policy for both encryption
	// keys and HMAC index keys, so a deployment keeps one key shape in its
	// secret manager rather than two.
	blindIndexKeySize = 32
)

// Configuration keys this module owns. They are DYNAMIC configuration: values
// an operator tunes per deployment and, where the deployment allows, per
// tenant. The argon2id cost parameters are deliberately NOT here -- those are
// bootstrap configuration, for the reasons on PasswordParams.
const (
	// ConfigKeyPasswordMinLength is the fewest characters a new password
	// may have.
	ConfigKeyPasswordMinLength = "authn.password_min_length"
	// ConfigKeyPasswordMaxLength bounds a new password, and with it the
	// amount of memory-hard hashing one request can ask for.
	ConfigKeyPasswordMaxLength = "authn.password_max_length"
	// ConfigKeyAccessTokenTTL is how long an access token stays valid, and
	// therefore also the worst-case delay of a sign-out under the natural
	// revocation mode.
	//
	// #nosec G101 -- this is a configuration KEY NAME, not a credential.
	// gosec's heuristic fires on any string constant whose identifier
	// contains "Token"; renaming the constant to dodge it would make the
	// schema key harder to find, and the value is published in the
	// generated configuration reference.
	ConfigKeyAccessTokenTTL = "authn.access_token_ttl"
	// ConfigKeyRefreshTokenTTL is how long a refresh token stays usable.
	//
	// #nosec G101 -- a configuration key name, for the reason above.
	ConfigKeyRefreshTokenTTL = "authn.refresh_token_ttl"
	// ConfigKeySessionTTL bounds a session however often it is refreshed.
	ConfigKeySessionTTL = "authn.session_ttl"
)

// FeatureFlagPasswordLogin gates password sign-in as a channel. A deployment
// that authenticates only through enterprise SSO turns it off, and the login
// page reads the answer from the pre-auth feature endpoint before anyone has
// signed in.
const FeatureFlagPasswordLogin = "authn.password_login"

// FeatureFlagSMSLogin gates phone-plus-SMS-code sign-in as a channel. Unlike
// the social channels below it defaults to ON: it needs no third-party
// credentials to function -- the standalone deployment mode's console
// sender always works, and a distributed deployment cannot even finish
// constructing this module without a real one wired (see
// ErrMissingDistributedSMSSender) -- so, like password sign-in, there is no
// "configured but not yet usable" state for the flag to protect the login
// page from.
const FeatureFlagSMSLogin = "authn.sms_login"

// Feature flags gating the federated sign-in channels, one per channel plus
// one for the enterprise relying party.
//
// Every one of them defaults to OFF. A channel with no credentials configured
// must not appear on the login page, and a flag that defaulted on would put
// it there for every deployment that has not turned it off -- where it would
// fail at the provider with an error the person clicking it cannot act on.
// The login page reads these from the pre-authentication feature endpoint,
// which is why they are flags rather than a computed "is this configured"
// answer: the endpoint is served before anyone has signed in.
const (
	// FeatureFlagSocialGoogle gates the Google channel.
	FeatureFlagSocialGoogle = "authn.social.google"
	// FeatureFlagSocialGitHub gates the GitHub channel.
	FeatureFlagSocialGitHub = "authn.social.github"
	// FeatureFlagSocialWeChat gates the WeChat Open Platform channel.
	FeatureFlagSocialWeChat = "authn.social.wechat"
	// FeatureFlagSocialDingTalk gates the DingTalk channel.
	FeatureFlagSocialDingTalk = "authn.social.dingtalk"
	// FeatureFlagSocialFeishu gates the Feishu / Lark channel.
	FeatureFlagSocialFeishu = "authn.social.feishu"
	// FeatureFlagEnterpriseSSO gates the per-tenant OpenID Connect
	// relying party.
	FeatureFlagEnterpriseSSO = "authn.sso.oidc"
)

// Configuration keys for the social channels' credentials.
//
// Each channel contributes a client id and a client secret. The secrets are
// Sensitive, which has a consequence every host must know about and which is
// why it is stated here rather than buried: config.Attach refuses a
// cipher-less startup as soon as ANY registered item is Sensitive. Wiring
// authn therefore makes a configuration cipher mandatory.
//
// That is the right trade. The alternative -- reading provider secrets from
// bootstrap environment variables instead -- would mean an operator cannot
// add a login channel without a redeploy, which is exactly the "customers can
// configure this themselves" property dynamic configuration exists for.
const (
	// ConfigKeyGoogleClientID is Google's OAuth client identifier.
	ConfigKeyGoogleClientID = "authn.social.google.client_id"
	// ConfigKeyGoogleClientSecret is Google's OAuth client secret.
	ConfigKeyGoogleClientSecret = "authn.social.google.client_secret" //nolint:gosec // a configuration key name, not a credential.
	// ConfigKeyGitHubClientID is GitHub's OAuth client identifier.
	ConfigKeyGitHubClientID = "authn.social.github.client_id"
	// ConfigKeyGitHubClientSecret is GitHub's OAuth client secret.
	ConfigKeyGitHubClientSecret = "authn.social.github.client_secret" //nolint:gosec // a configuration key name, not a credential.
	// ConfigKeyWeChatClientID is the WeChat Open Platform "appid".
	ConfigKeyWeChatClientID = "authn.social.wechat.client_id"
	// ConfigKeyWeChatClientSecret is the WeChat Open Platform "secret".
	ConfigKeyWeChatClientSecret = "authn.social.wechat.client_secret" //nolint:gosec // a configuration key name, not a credential.
	// ConfigKeyDingTalkClientID is DingTalk's application key.
	ConfigKeyDingTalkClientID = "authn.social.dingtalk.client_id"
	// ConfigKeyDingTalkClientSecret is DingTalk's application secret.
	ConfigKeyDingTalkClientSecret = "authn.social.dingtalk.client_secret" //nolint:gosec // a configuration key name, not a credential.
	// ConfigKeyFeishuClientID is the Feishu "app_id".
	ConfigKeyFeishuClientID = "authn.social.feishu.client_id"
	// ConfigKeyFeishuClientSecret is the Feishu "app_secret".
	ConfigKeyFeishuClientSecret = "authn.social.feishu.client_secret" //nolint:gosec // a configuration key name, not a credential.

	// ConfigKeyTrustedProviders is the whitespace-delimited list of social
	// channels whose EmailVerified assertion may automatically link a new
	// external identity to an EXISTING account.
	//
	// Its default is EMPTY, which disables automatic linking entirely. See
	// WithTrustedProviders for why that is the safe default and why Google
	// is the channel a deployment would sensibly add first.
	ConfigKeyTrustedProviders = "authn.social.trusted_providers"

	// ConfigKeyOAuthStateTTL bounds how long an authorization flow may
	// take between leaving for a provider and coming back.
	ConfigKeyOAuthStateTTL = "authn.oauth_state_ttl"
)

// Configuration keys for phone-login verification codes. Both are dynamic
// rather than bootstrap: an operator tunes them per deployment the same way
// as the token TTLs above, and neither depends on the machine the process
// runs on the way the argon2id cost parameters do.
const (
	// ConfigKeySMSCodeTTL is how long a phone-login verification code
	// stays valid after it is sent.
	ConfigKeySMSCodeTTL = "authn.sms_code_ttl"
	// ConfigKeySMSCodeMaxAttempts is how many wrong codes a single issued
	// verification code tolerates before it locks and a fresh one must be
	// requested.
	ConfigKeySMSCodeMaxAttempts = "authn.sms_code_max_attempts"
)

// SystemPurposeSignInTenantEnumeration is the pkgcore.SystemPurpose this
// module declares for the one system context it takes: the cross-tenant
// "which tenants does this account belong to" read
// (MembershipReader.TenantsOf) a no-tenant sign-in performs. The question
// spans organizations by definition, so no tenant-scoped context could
// answer it; the read is reserved for the authn side of the house, and it
// is taken through tenancy.WithSystemContext with the account itself as
// the actor, which publishes a tenancy.system_context.entered audit event
// on every use. The component descriptor declares the purpose as its
// SystemPurposes, which the assembly registers when its Init stage closes
// (the transition bridge registers it inside the module's own registration
// turn on the module path), so a host that bootstraps this module never has
// to.
const SystemPurposeSignInTenantEnumeration pkgcore.SystemPurpose = "speed.authn.sign_in_tenant_enumeration"

// FeatureGate reports whether a feature flag is enabled for the tenant the
// context carries (or platform-wide when it carries none).
//
// It is the same no-import technique the org module uses for its own flags
// (go/org/module.go's FeatureGate, identical shape): the signature is built
// from stdlib types only, so *config.Service satisfies it structurally
// through its own IsEnabled method. authn never imports config, and config
// never learns that authn exists; the host passes one to the other, and the
// host is the only place both names appear.
//
// This seam is what makes this module's declared feature flags effective
// at request time: without the gate, a flag is a declaration with no
// enforcement, and a deployment that disabled a channel would still serve
// it. Every sign-in channel consults the gate before it lets a request
// through, and the login page's own channel visibility comes from the same
// flag values served by the config module's pre-authentication features
// endpoint, so the page and the API agree on which channels exist.
//
// A nil gate means this deployment has no feature-flag module at all -- a
// host running authn without the config module -- and every channel behaves
// as enabled; such a host's login page (which has no features endpoint to
// read) shows exactly the channels the host configured. The flags' effect
// is expressed in config, so only a host with config can turn a channel
// off, and the wiring contract is: if the config module is in the
// deployment, pass its Service here.
type FeatureGate interface {
	IsEnabled(ctx context.Context, key string) (bool, error)
}

// FeatureGateFunc adapts a plain function to FeatureGate, the same
// func-to-interface adapter shape http.HandlerFunc popularized, so a host
// need not declare a named type just to wire this seam. The config module's
// lazy Handle satisfies the gate through a method value alone
// (authn.FeatureGateFunc(handle.IsEnabled)); a host whose reader needs a
// guard of its own passes a closure instead.
type FeatureGateFunc func(ctx context.Context, key string) (bool, error)

// IsEnabled implements FeatureGate.
func (f FeatureGateFunc) IsEnabled(ctx context.Context, key string) (bool, error) {
	return f(ctx, key)
}

// compile-time check that FeatureGateFunc satisfies FeatureGate.
var _ FeatureGate = FeatureGateFunc(nil)

// options accumulates everything NewService and NewModule can be configured
// with.
type options struct {
	keySource      KeySource
	blindIndexKey  []byte
	membership     MembershipReader
	now            func() time.Time
	issuer         string
	accessTTL      time.Duration
	refreshTTL     time.Duration
	sessionTTL     time.Duration
	revocationMode RevocationMode
	passwordParams PasswordParams
	passwordPolicy PasswordPolicy

	providers        []SocialProvider
	trustedProviders []string
	redirects        RedirectAllowlist
	oauthStateTTL    time.Duration
	federationClient *http.Client

	// SMS and MFA state: the transport a phone-login code is delivered
	// through, the deployment mode NewModule enforces it against, and the
	// code lifetime/attempt budget.
	smsSender          pkgcore.SMSSender
	deploymentMode     pkgcore.DeploymentMode
	smsCodeTTL         time.Duration
	smsCodeMaxAttempts int

	// secureCookies forces the Secure attribute on the pre-authentication
	// OAuth cookie regardless of what r.TLS says. See WithSecureCookies.
	secureCookies bool

	// trustedProxies is WithTrustedProxies' raw input: the IP addresses
	// and CIDR prefixes of the reverse proxies this deployment receives
	// requests through. newOptions compiles it into trustedProxyNets,
	// refusing an entry that is neither an IP address nor a CIDR prefix.
	trustedProxies []string

	// trustedProxyNets is trustedProxies in matchable form: one
	// netip.Prefix per entry, a bare address as its own /32 or /128. It is
	// what Service carries and handler.go's clientIP consults -- see
	// WithTrustedProxies for what declaring a proxy changes.
	trustedProxyNets []netip.Prefix

	// vendorClientIPHeaders is WithVendorClientIPHeaders' input: the
	// single-hop vendor client-address headers (VendorClientIPHeader) this
	// deployment's proxy genuinely overwrites on every request, read by
	// handler.go's clientIP for a request whose peer is a declared trusted
	// proxy. newOptions validates the closed set; Service carries the
	// result.
	vendorClientIPHeaders []VendorClientIPHeader

	// featureGate makes the module's declared feature flags effective at
	// request time; nil keeps every channel enabled. See FeatureGate.
	featureGate FeatureGate

	// timezoneResolver is the registration timezone chain's IP-resolution
	// tier. Nil skips the tier; see WithTimeZoneResolver and
	// TimeZoneResolver.
	timezoneResolver TimeZoneResolver
}

// Option configures the authn module and the service inside it.
type Option func(*options)

// WithKeySource supplies the signing-key lifecycle provider access tokens
// are minted and verified through. It is REQUIRED: there is no safe
// default, and there is deliberately no second, static-injection path -- a
// fallback path is exactly the failure mode of an implicit "generate one
// myself if nothing was configured" route, which nobody could say for
// certain was or wasn't taken in production.
//
// A production deployment wires a *pki.Service here (structurally, with no
// import of go/pki from this package -- see KeySource's own doc comment);
// a unit test wires a minimal fake.
func WithKeySource(keySource KeySource) Option {
	return func(o *options) { o.keySource = keySource }
}

// WithBlindIndexKey supplies the 32-byte HMAC key the email and phone
// blind-index columns are computed under. It is REQUIRED, and it must be a
// secret used for nothing else -- in particular never the field-encryption
// key, because a deterministic index and an encrypted column are two
// constructions that were never designed to share key material.
//
// Changing it invalidates every stored index, which is a data migration
// rather than a configuration change.
func WithBlindIndexKey(key []byte) Option {
	return func(o *options) {
		o.blindIndexKey = make([]byte, len(key))
		copy(o.blindIndexKey, key)
	}
}

// WithMembershipReader supplies the seam through which authn asks whether a
// user belongs to a tenant. Without it, tenant switching and refresh fail
// closed with ErrTenantMembershipUnavailable; password sign-in refuses too,
// folding its membership failure into the uniform ErrInvalidCredentials
// every failed sign-in answers -- a nil reader would otherwise certify every
// correct password to an anonymous caller (see Service.Login's doc comment).
func WithMembershipReader(reader MembershipReader) Option {
	return func(o *options) { o.membership = reader }
}

// WithClock replaces the source of the current time throughout the module, so
// a test can expire a token or a session without sleeping. A nil function is
// ignored.
func WithClock(now func() time.Time) Option {
	return func(o *options) {
		if now != nil {
			o.now = now
		}
	}
}

// WithIssuer sets the "iss" claim tokens are signed with and verified
// against. An empty issuer is ignored.
func WithIssuer(issuer string) Option {
	return func(o *options) {
		if issuer != "" {
			o.issuer = issuer
		}
	}
}

// WithAccessTokenTTL sets how long access tokens stay valid. A non-positive
// duration is ignored.
func WithAccessTokenTTL(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.accessTTL = d
		}
	}
}

// WithRefreshTokenTTL sets how long refresh tokens stay usable. A
// non-positive duration is ignored.
func WithRefreshTokenTTL(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.refreshTTL = d
		}
	}
}

// WithSessionTTL bounds a session's total lifetime however often it is
// refreshed. A non-positive duration is ignored.
func WithSessionTTL(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.sessionTTL = d
		}
	}
}

// WithRevocationMode selects natural expiry or the immediate revocation
// list. An unrecognised mode is ignored, leaving the default.
//
// This option is the ONE selector: immediate mode records every revoked
// session on the shared key-value store and the middleware every host builds
// over Service.Verifier consults that list by default, so selecting
// RevocationModeImmediate genuinely enforces sign-out on outstanding access
// tokens -- no separate middleware option to forget. There is deliberately
// no dynamic-configuration twin of this option: the schema key once
// declared for one, authn.session_revocation_immediate, is gone because a
// value read at request time could not deliver what its description
// promised -- the mode is fixed at SessionManager construction and gates
// which revocations are even recorded.
func WithRevocationMode(mode RevocationMode) Option {
	return func(o *options) {
		if mode == RevocationModeNatural || mode == RevocationModeImmediate {
			o.revocationMode = mode
		}
	}
}

// WithPasswordParams sets the argon2id cost parameters used for NEW hashes.
// Existing hashes keep verifying under the parameters recorded inside them
// and are upgraded on their owner's next successful sign-in.
func WithPasswordParams(p PasswordParams) Option {
	return func(o *options) { o.passwordParams = p }
}

// WithPasswordPolicy sets the rules a new password must satisfy.
func WithPasswordPolicy(p PasswordPolicy) Option {
	return func(o *options) { o.passwordPolicy = p }
}

// WithSocialProviders wires the social-login channels a deployment offers.
// Constructing each provider is the host's job, because each one needs
// credentials the host reads from its own secret source.
func WithSocialProviders(providers ...SocialProvider) Option {
	return func(o *options) { o.providers = append(o.providers, providers...) }
}

// WithTrustedProviders names the social channels whose EmailVerified
// assertion is allowed to link a new external identity to an EXISTING account
// automatically.
//
// The default is EMPTY, which means no automatic linking happens at all and
// every new external identity whose address already belongs to an account is
// refused with ErrIdentityRequiresBinding. That is the fail-closed default on
// purpose: automatic linking is convenience, and the failure mode of getting
// it wrong is somebody else signing in to your account.
//
// The channel a deployment would sensibly add first is Google, which is the
// only one of the five shipped here that delivers "email_verified" inside a
// document it signed, rather than as a field in an ordinary API response.
func WithTrustedProviders(providers ...string) Option {
	return func(o *options) { o.trustedProviders = append(o.trustedProviders, providers...) }
}

// WithRedirectAllowlist registers the redirect URIs an authorization flow may
// return to. An empty allowlist refuses every flow, which is the correct
// closed default for a deployment that has not enabled social login.
func WithRedirectAllowlist(allowlist RedirectAllowlist) Option {
	return func(o *options) { o.redirects = allowlist }
}

// WithOAuthStateTTL bounds how long an authorization flow may take. A
// non-positive duration is ignored.
func WithOAuthStateTTL(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.oauthStateTTL = d
		}
	}
}

// WithSecureCookies forces the Secure attribute onto the pre-authentication
// OAuth cookie (ensurePreAuthCookie) regardless of what the inbound
// request's r.TLS says.
//
// r.TLS is nil for every request in the most common production topology --
// TLS terminated at a load balancer or reverse proxy, with the Go process
// itself only ever seeing plaintext HTTP on its own listener -- so relying
// on it alone silently drops Secure in exactly the deployment shape most
// hosts run. This is bootstrap configuration (a value that varies by
// environment), set once by the host at startup from its own knowledge of
// its topology (e.g. an SPEED_TLS_TERMINATED env var), rather than
// inferred per request from a client-controlled header -- the same reason
// handler.go's clientIP reads X-Forwarded-For only from a request whose
// peer is a declared trusted proxy (WithTrustedProxies), with no such gate
// available for a header asserting TLS: a host that is not actually behind
// TLS anywhere must never pass true here.
func WithSecureCookies(secure bool) Option {
	return func(o *options) { o.secureCookies = secure }
}

// VendorClientIPHeader names one of the single-hop client-address headers
// this module knows how to read: a platform-specific header that ONE
// vendor's proxy genuinely OVERWRITES with the client's address on every
// request it forwards. The value is single-hop by design -- no chain of
// proxy-appended entries to walk, which is exactly what makes such a
// header unsafe to read on the trusted-peer gate alone. The set of known
// headers is CLOSED: a WithVendorClientIPHeaders entry that is not one of
// the constants below is refused at wiring time (newOptions), so the
// option can never become a bare list of host-typed header names -- every
// member documents the deployment shape in which reading it is honest.
type VendorClientIPHeader string

const (
	// VendorClientIPHeaderFlyClientIP is Fly.io's proxy header, set to
	// the real client address on every request the Fly proxy forwards.
	// Opting in is honest ONLY for a deployment whose requests genuinely
	// arrive through Fly's proxy: any other reverse proxy in front of the
	// same application (nginx, ALB, Envoy, Cloudflare) forwards an
	// unknown request header verbatim, so a client could send its own
	// value through a proxy that is not Fly's.
	VendorClientIPHeaderFlyClientIP VendorClientIPHeader = "Fly-Client-IP"
)

// WithTrustedProxies names the reverse proxies requests are received
// through, so handler.go's clientIP can recover the real client address
// from the forwarding headers those proxies inject instead of recording
// the proxy itself -- the session/login-history defect the reference
// app's Fly.io deployment exposed, where every recorded address was the
// proxy's internal 172.16.x one.
//
// A request's recorded address (the rate-limiter key, the session row, the
// login-history row) is otherwise its direct connection address,
// RemoteAddr. That fallback stays, and this option only widens it for the
// requests that genuinely came through a declared proxy: the forwarding
// headers are read ONLY when the request's RemoteAddr is one of the
// declared proxies, so a direct client that sets X-Forwarded-For or a
// vendor header itself changes nothing -- the spoof this gating exists to
// keep out. This is bootstrap configuration, declared once by the host at
// startup from its own knowledge of its topology (an operator behind a
// proxy knows the proxy's address; the module never guesses), exactly like
// WithSecureCookies.
//
// Declaring a proxy authorizes exactly ONE header on its own:
// X-Forwarded-For, whose chain walk (handler.go's xForwardedForClientIP)
// is self-protecting -- the entries at the right end of the chain are
// ones the declared proxies themselves appended, so the walk never trusts
// anything a client wrote. A single-hop VENDOR header (Fly-Client-IP and
// its peers, see VendorClientIPHeader) is a different animal: the peer
// gate cannot tell "the request came from Fly's proxy" from "the request
// came from a generic proxy that forwards a client-chosen Fly-Client-IP
// verbatim", because both look identical to this process. Only the host
// knows which topology it runs, so a vendor header is read only when the
// host additionally opted into that specific header with
// WithVendorClientIPHeaders. A deployment whose generic proxy is
// configured to append to X-Forwarded-For needs nothing more than this
// option; a Fly.io deployment declares its proxy ranges here AND opts into
// headerFlyClientIP there.
//
// Each entry is an IP address or a CIDR prefix -- "203.0.113.10", or
// "172.16.0.0/12" for a whole proxy range. An entry that is neither is
// refused at wiring time (newOptions): it can never match a peer, so
// accepting it would silently keep recording the proxy address instead of
// the client's. A declared proxy must overwrite or strip the
// X-Forwarded-For it receives from its own clients, so a client cannot
// smuggle a header through the proxy it is trusted for; a generic reverse
// proxy must be configured to do the same.
//
// The default is EMPTY: no proxy declared, every request records its
// direct connection address exactly as this module always did.
func WithTrustedProxies(proxies ...string) Option {
	return func(o *options) { o.trustedProxies = append(o.trustedProxies, proxies...) }
}

// WithVendorClientIPHeaders opts the deployment into reading the named
// single-hop vendor client-address headers (VendorClientIPHeader) for a
// request whose direct peer is a declared trusted proxy
// (WithTrustedProxies). The default is NONE: X-Forwarded-For is the one
// forwarding header read under the trusted-peer gate alone, and no vendor
// header is read until the host says its own proxy genuinely overwrites
// it on every request it forwards.
//
// That declaration is the point of the option, and it is why the opt-in
// exists per KNOWN header rather than as a list of arbitrary names (the
// closed VendorClientIPHeader set, validated at wiring time): reading a
// single-hop header on the trusted-peer gate alone -- the shape this
// option replaced -- let a client smuggle Fly-Client-IP through any
// declared generic reverse proxy that forwards unknown headers verbatim
// (nginx, ALB, Envoy, Cloudflare), minting its own recorded address and
// its own rate-limiter bucket at will. The host opts in only when its
// proxy is the header's own vendor and genuinely overwrites it, the same
// topology statement WithSecureCookies makes about TLS termination --
// never a fact this module could infer from the request, since a generic
// proxy and the vendor's proxy are indistinguishable to the process
// behind them.
//
// Opting in never widens the request gate: the header is still read only
// from a request whose peer is a declared proxy, and handler.go's clientIP
// consults it only when the X-Forwarded-For chain walk -- the
// self-protecting path -- produced no answer, so a client-chosen vendor
// value can never displace the chain's truth. Entries are consulted in
// declaration order.
func WithVendorClientIPHeaders(headers ...VendorClientIPHeader) Option {
	return func(o *options) { o.vendorClientIPHeaders = append(o.vendorClientIPHeaders, headers...) }
}

// WithFederationHTTPClient replaces the HTTP client the ENTERPRISE single
// sign-on relying party talks to a tenant's identity provider with.
//
// The default is the SSRF-guarded client, and replacing it removes that
// guard: the issuer URL is typed by a tenant administrator, so the guard is
// what stops it pointing at the deployment's own network. Tests inject a
// plain client because their identity provider is an httptest server on
// loopback, which the guard correctly refuses; a deployment has no reason to.
func WithFederationHTTPClient(client *http.Client) Option {
	return func(o *options) {
		if client != nil {
			o.federationClient = client
		}
	}
}

// ErrMissingDistributedSMSSender is returned by NewModule and NewService
// when the module is being wired with WithDeploymentMode(pkgcore.
// DeploymentModeDistributed) and no SMSSender was supplied with
// WithSMSSender.
//
// It exists for exactly the reason pkgcore's own builtin composition refuses to
// resolve a distributed deployment onto an in-process seam: pkgcore.NewConsoleSMSSender
// prints to a writer nobody in a distributed deployment's replica pool is
// reading, so silently defaulting to it there would look like phone sign-in
// works right up until the first person tries to use the code that was never
// actually delivered. The SMS seam is pkgcore's, but it deliberately has no
// assembly seat (see pkgcore.SMSSender's doc comment) -- the assembly resolves
// no SMS sender for this module, which receives it through this option -- so
// the enforcement happens here, at the same wiring-time moment newOptions
// already validates WithKeySource and WithBlindIndexKey, against the
// deployment mode WithDeploymentMode records.
var ErrMissingDistributedSMSSender = errors.New("authn: distributed deployment mode requires an explicit SMS sender")

// WithSMSSender wires the transport phone-login verification codes are
// delivered through -- any pkgcore.SMSSender: pkgcore.NewConsoleSMSSender
// for the standalone deployment mode, pkgcore.NewHTTPSMSSender or one of
// the pkgcore/sms carrier adapters for a deployment with a real transport.
// See ErrMissingDistributedSMSSender for what happens when it is omitted
// under WithDeploymentMode(pkgcore.DeploymentModeDistributed); the
// standalone deployment mode, and a caller that never calls
// WithDeploymentMode at all, default to pkgcore.NewConsoleSMSSender.
func WithSMSSender(sender pkgcore.SMSSender) Option {
	return func(o *options) { o.smsSender = sender }
}

// WithDeploymentMode records which deployment mode this module is being
// wired for, solely so newOptions can enforce that a distributed deployment
// supplies an explicit SMSSender rather than silently defaulting to one
// that prints to a writer nobody in that deployment mode is reading. This
// is the one piece of deployment-mode awareness this module carries, and it
// lives entirely in this wiring-time validation function -- never in
// Service's business logic -- for the same reason capability validation
// lives in the assembly rather than in a business module: "do not branch on
// deployment mode in business logic"
// governs behavior selection inside a request, not a once-at-construction-time
// checked precondition. Omitting this option is equivalent to standalone: it
// is not itself a required option and existing callers that never call it
// keep building successfully.
func WithDeploymentMode(mode pkgcore.DeploymentMode) Option {
	return func(o *options) { o.deploymentMode = mode }
}

// WithSMSCodeTTL sets how long a phone-login verification code stays valid.
// A non-positive duration is ignored.
func WithSMSCodeTTL(d time.Duration) Option {
	return func(o *options) {
		if d > 0 {
			o.smsCodeTTL = d
		}
	}
}

// WithSMSCodeMaxAttempts sets how many wrong codes a single issued
// verification code tolerates before it locks and a fresh one must be
// requested. A non-positive value is ignored.
func WithSMSCodeMaxAttempts(n int) Option {
	return func(o *options) {
		if n > 0 {
			o.smsCodeMaxAttempts = n
		}
	}
}

// WithFeatureGate wires the reader this module's declared feature flags are
// enforced through. *config.Service satisfies FeatureGate structurally; pass
// the config module's service here when one is in the deployment. Without
// it, every sign-in channel behaves as enabled -- see FeatureGate's own doc
// comment for why that is the no-config-module shape rather than a silent
// hole.
func WithFeatureGate(gate FeatureGate) Option {
	return func(o *options) { o.featureGate = gate }
}

// WithTimeZoneResolver supplies the seam through which registration asks
// which timezone a client IP belongs to -- the registration timezone
// chain's third tier, behind the caller's own browser report and the
// provider's profile (none of the shipped social channels reports one).
//
// It is optional, and NOT fail-closed, unlike WithMembershipReader: the
// tier is a convenience whose absence degrades to "not chosen yet" (the
// platform default UTC) rather than refusing anything, because a
// registration must not fail over an optional signal and an empty timezone
// is a state the account can change at will. A resolver that errors or
// answers a name the IANA database does not know is treated identically to
// one that was never wired -- the tier is skipped and logged, never
// silently stored half-validated. A host wires one only when it actually
// runs a geo-IP data source (and has settled that source's licence); the
// module ships no implementation and the reference app wires none.
func WithTimeZoneResolver(resolver TimeZoneResolver) Option {
	return func(o *options) { o.timezoneResolver = resolver }
}

// newOptions applies opts over the defaults and rejects a configuration the
// module cannot run with.
func newOptions(opts []Option) (options, error) {
	cfg := options{
		now:                time.Now,
		issuer:             DefaultIssuer,
		accessTTL:          DefaultAccessTokenTTL,
		refreshTTL:         DefaultRefreshTokenTTL,
		sessionTTL:         DefaultSessionTTL,
		revocationMode:     RevocationModeNatural,
		passwordParams:     DefaultPasswordParams(),
		passwordPolicy:     DefaultPasswordPolicy(),
		oauthStateTTL:      DefaultOAuthStateTTL,
		smsCodeTTL:         DefaultSMSCodeTTL,
		smsCodeMaxAttempts: DefaultSMSCodeMaxAttempts,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	if cfg.keySource == nil {
		return options{}, errors.New("authn: a KeySource is required; supply one with WithKeySource")
	}
	if len(cfg.blindIndexKey) != blindIndexKeySize {
		return options{}, errors.New("authn: a 32-byte blind-index key is required; supply one with WithBlindIndexKey")
	}
	if cfg.smsSender == nil {
		if cfg.deploymentMode == pkgcore.DeploymentModeDistributed {
			return options{}, fmt.Errorf("%w: wire one with WithSMSSender", ErrMissingDistributedSMSSender)
		}
		cfg.smsSender = pkgcore.NewConsoleSMSSender(os.Stdout)
	}
	// Compile the host-declared trusted-proxy list. An entry that is
	// neither an IP address nor a CIDR prefix cannot be a proxy peer, so
	// it is refused here -- failing closed at wiring time rather than
	// silently never matching, which would quietly keep recording the
	// proxy address instead of the client's.
	for _, entry := range cfg.trustedProxies {
		prefix, err := parseTrustedProxy(entry)
		if err != nil {
			return options{}, err
		}
		cfg.trustedProxyNets = append(cfg.trustedProxyNets, prefix)
	}
	cfg.trustedProxies = nil
	// Validate the vendor-header opt-in's closed set: an entry that is
	// not one of the declared VendorClientIPHeader constants is a bare
	// host-typed header name, which would recreate -- under a new option
	// -- the single-hop trust hole (a client smuggling its own value
	// through a generic proxy) WithVendorClientIPHeaders exists to keep
	// out, so it is refused here, at wiring time.
	for _, hdr := range cfg.vendorClientIPHeaders {
		switch hdr {
		case VendorClientIPHeaderFlyClientIP:
		default:
			return options{}, fmt.Errorf("authn: %q is not a known vendor client-IP header; opt in through the declared VendorClientIPHeader constants only", hdr)
		}
	}
	return cfg, nil
}

// parseTrustedProxy validates one WithTrustedProxies entry -- an IP
// address or a CIDR prefix -- into the netip.Prefix form peer matching
// needs. A bare address becomes its own /32 or /128; anything else (a
// hostname, a range without CIDR syntax, a zoned address) is refused with
// an error naming the entry.
func parseTrustedProxy(entry string) (netip.Prefix, error) {
	if prefix, err := netip.ParsePrefix(entry); err == nil {
		return prefix.Masked(), nil
	}
	if addr, err := netip.ParseAddr(entry); err == nil && addr.Zone() == "" {
		return netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()), nil
	}
	return netip.Prefix{}, fmt.Errorf("authn: trusted proxy %q is not an IP address or CIDR prefix", entry)
}

// Module implements the module contract for authn.
type Module struct {
	db      *gorm.DB
	opts    []Option
	svc     *Service
	handler *Handler
}

// NewModule returns a Module backed by db.
//
// db is expected to come from dbkit.Open, already migrated, and -- crucially
// -- to have been opened AFTER RegisterPIISerializer ran, because GORM
// resolves a model's serializer while it parses the schema. Constructing a
// Module performs no I/O; the options are validated eagerly so a missing key
// is a startup error rather than a failure on the first sign-in.
func NewModule(db *gorm.DB, opts ...Option) (*Module, error) {
	if db == nil {
		return nil, errors.New("authn: NewModule requires a database handle")
	}
	if _, err := newOptions(opts); err != nil {
		return nil, err
	}
	return &Module{db: db, opts: opts}, nil
}

// Name implements the module contract.
func (m *Module) Name() string { return moduleName }

// DependsOn implements the module contract. authn depends on infrastructure
// (dbkit, tenancy, observability) rather than on another business module, and
// infrastructure is not part of the bootstrap set, so this is genuinely
// empty. In particular it does NOT depend on org: the membership question is
// asked through the injected MembershipReader seam precisely so that the
// dependency does not exist.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements the module contract.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements the module contract.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements the module contract: it returns the module's own
// OpenAPI fragment, embedded from api/openapi.yaml. That fragment is the
// single source of this module's API surface -- the api package's
// generated types and ServerInterface (api/authn-server.gen.go,
// regenerated by task api:gen) derive from it, and Handler implements that
// interface (see handler.go).
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Service returns the module's service. It is nil until Register has run,
// because the service needs the event bus and key-value store the registry
// carries.
func (m *Module) Service() *Service { return m.svc }

// Register implements the module contract.
//
// It performs no I/O, as the interface requires. Building the service wires
// already-constructed in-memory values together and reads two fields off the
// registry; nothing here opens a connection, sends a request or touches the
// database. Note also that reg.Locales() is deliberately not consulted: the
// merged catalog is installed only after every module has registered, so it
// is nil at this point by design. The module's own system purpose -- the
// sign-in tenant enumeration -- is descriptor data, not a declaration made
// here: the component descriptor (component.go) carries it as
// SystemPurposes, and the assembly registers it when its Init stage closes
// (the transition bridge registers it inside the module's own registration
// turn on the module path).
func (m *Module) Register(reg *pkgcore.ComponentRegistry) error {
	svc, err := NewService(m.db, reg.EventBus(), reg.KVStore(), m.opts...)
	if err != nil {
		return err
	}
	m.svc = svc

	if err := reg.AuditActionsSeat().Add(auditActions...); err != nil {
		return err
	}
	// The same registrar backs the service layer's own audit.Emit calls --
	// two sites, neither one a layer a Handler exists in. SaveConfig, the
	// one site for AuditActionSSOConfigure, writes from the SSO service
	// (oidc.go's emitConfigSavedAudit); SessionManager.handleReplay
	// records a detected refresh-token replay (session.go's
	// emitReplayAudit), which no handler could record: a refresh request
	// is credential-less, so the handler answering its 401 has no identity
	// to attribute a row to. The wire must come after the Add above --
	// Emit itself checks the action string against this registrar before
	// publishing -- and it stays nil for a Service assembled directly
	// through NewService, whose SaveConfig and replay detection then
	// record nothing, exactly like the handler's own nil-bus short-circuit
	// below.
	svc.sso.auditActions = reg.AuditActionsSeat()
	svc.sessions.auditActions = reg.AuditActionsSeat()
	// reg.AuditActions is handed to NewHandler so its own audit.Emit calls
	// (see handler.go's recordAudit) validate against the exact
	// AuditActionRegistrar the 9 actions above were just declared on --
	// Emit itself checks the action string against it before publishing
	// (see audit.Emit's own doc comment), which is what requires the
	// declaration above to run before this line, matching notes.Module's
	// identical ordering for its own single audit action.
	m.handler = NewHandler(svc, reg.EventBus(), reg.AuditActionsSeat())
	reg.RoutesSeat().Mount(apiPath, m.handler)

	if err := reg.EventsSeat().Publishes(eventDecls...); err != nil {
		return err
	}
	if err := reg.ConfigSeat().Add(configItems()...); err != nil {
		return err
	}
	// The process-start key material (bootstrapKeyDecls) is descriptor data:
	// the component descriptor carries it as BootstrapKeys, which the loader
	// resolves before anything is constructed.
	if err := reg.PermissionsSeat().Add(PermissionSSOManage); err != nil {
		return err
	}
	return reg.FeaturesSeat().Add(featureFlags()...)
}

// featureFlags is the toggle set this module declares: one for password
// sign-in, one per social channel, and one for the enterprise relying party.
func featureFlags() []pkgcore.FeatureFlag {
	return []pkgcore.FeatureFlag{
		{
			Key:         FeatureFlagPasswordLogin,
			Default:     true,
			Description: "Allows signing in with an email address or phone number and a password.",
		},
		{
			Key:         FeatureFlagSMSLogin,
			Default:     true,
			Description: "Allows signing in with a phone number and a one-time SMS code.",
		},
		{
			Key:         FeatureFlagSocialGoogle,
			Default:     false,
			Description: "Offers Google as a sign-in channel. Requires the Google client id and secret to be configured.",
		},
		{
			Key:         FeatureFlagSocialGitHub,
			Default:     false,
			Description: "Offers GitHub as a sign-in channel. Requires the GitHub client id and secret to be configured.",
		},
		{
			Key:         FeatureFlagSocialWeChat,
			Default:     false,
			Description: "Offers WeChat as a sign-in channel. Requires the WeChat Open Platform appid and secret to be configured.",
		},
		{
			Key:         FeatureFlagSocialDingTalk,
			Default:     false,
			Description: "Offers DingTalk as a sign-in channel. Requires the DingTalk application key and secret to be configured.",
		},
		{
			Key:         FeatureFlagSocialFeishu,
			Default:     false,
			Description: "Offers Feishu as a sign-in channel. Requires the Feishu app id and secret to be configured.",
		},
		{
			Key:         FeatureFlagEnterpriseSSO,
			Default:     false,
			Description: "Allows each tenant to configure an OpenID Connect identity provider its members sign in through.",
		},
	}
}

// The two declared key paths, named once so the declarations and every
// material read (component.go) cannot drift apart.
const (
	piiCipherKeyPath  = "authn.pii_cipher_key"
	blindIndexKeyPath = "authn.blind_index_key"
)

// bootstrapKeyDecls is the process-start key material this module consumes,
// declared as the authn component's BootstrapKeys (component.go).
//
// Both keys are separate secrets on purpose. The cipher key seals the PII
// columns (email, phone, TOTP secrets) and the blind-index key is the HMAC key
// over users.email_index/phone_index; dbkit's rule that an AES key never
// doubles as an HMAC key is what keeps them apart, and the same rule separates
// them from every other module's key material.
//
// The blind-index key must stay IDENTICAL across restarts: a rotation makes
// every already-stored email and phone index unfindable, so the host must feed
// it from a durable secret store rather than one that regenerates it.
var bootstrapKeyDecls = []pkgcore.BootstrapKey{
	{
		Key:         piiCipherKeyPath,
		Format:      "hexkey",
		Default:     "documented non-secret development default",
		Sensitive:   true,
		Description: "AES key sealing authn's encrypted PII columns (email, phone, TOTP secrets), deliberately separate from every other module's key material and from authn's own blind-index key below.",
		Group:       moduleName,
	},
	{
		Key:         blindIndexKeyPath,
		Format:      "hexkey",
		Default:     "documented non-secret development default",
		Sensitive:   true,
		Description: "HMAC key authn indexes its users.email_index and phone_index blind-index columns with; it must stay identical across restarts or every already-stored email and phone index becomes unfindable, and an HMAC key never doubles as a cipher key.",
		Group:       moduleName,
	},
}

// configItems is the dynamic-configuration schema this module declares.
//
// The social-channel client secrets here ARE Sensitive, which has a
// consequence for every host: config.Attach refuses a cipher-less startup as
// soon as one Sensitive item is registered. Wiring authn therefore makes a
// configuration cipher mandatory, and that is a deliberate decision rather
// than a side effect -- see the comment on the ConfigKey* block.
//
// Note the pairing rule the registry enforces: an item may be Sensitive or
// Public, never both. Client secrets are Sensitive; the client IDs beside
// them are neither, because although a client id is not a secret, nothing on
// the pre-authentication public endpoint needs it -- the login page renders a
// channel button from the FEATURE FLAG, and the id only ever appears in the
// authorization URL this module builds server-side.
func configItems() []pkgcore.ConfigItem {
	defaults := DefaultPasswordPolicy()
	items := []pkgcore.ConfigItem{
		{
			Key:         ConfigKeyPasswordMinLength,
			Type:        "int",
			Default:     defaults.MinLength,
			Min:         8,
			Max:         128,
			Group:       moduleName,
			Description: "Minimum number of characters a new password must have.",
		},
		{
			Key:         ConfigKeyPasswordMaxLength,
			Type:        "int",
			Default:     defaults.MaxLength,
			Min:         16,
			Max:         1024,
			Group:       moduleName,
			Description: "Maximum number of characters a new password may have.",
		},
		{
			Key:         ConfigKeyAccessTokenTTL,
			Type:        "duration",
			Default:     DefaultAccessTokenTTL,
			Min:         time.Minute,
			Max:         24 * time.Hour,
			Group:       moduleName,
			Description: "How long an access token stays valid, and the worst-case delay before a sign-out takes effect under natural revocation.",
		},
		{
			Key:         ConfigKeyRefreshTokenTTL,
			Type:        "duration",
			Default:     DefaultRefreshTokenTTL,
			Min:         time.Hour,
			Max:         365 * 24 * time.Hour,
			Group:       moduleName,
			Description: "How long a refresh token stays usable before the user must sign in again.",
		},
		{
			Key:         ConfigKeySessionTTL,
			Type:        "duration",
			Default:     DefaultSessionTTL,
			Min:         time.Hour,
			Max:         365 * 24 * time.Hour,
			Group:       moduleName,
			Description: "Maximum lifetime of a session, however often it is refreshed.",
		},
		{
			Key:         ConfigKeyTrustedProviders,
			Type:        "string",
			Default:     "",
			Group:       moduleName,
			Description: "Whitespace-delimited social channels whose verified-email assertion may automatically link a new sign-in to an existing account. Empty disables automatic linking entirely.",
		},
		{
			Key:         ConfigKeyOAuthStateTTL,
			Type:        "duration",
			Default:     DefaultOAuthStateTTL,
			Min:         time.Minute,
			Max:         time.Hour,
			Group:       moduleName,
			Description: "How long a social or single sign-on authorization flow may take between leaving for the provider and returning.",
		},
		{
			Key:         ConfigKeySMSCodeTTL,
			Type:        "duration",
			Default:     DefaultSMSCodeTTL,
			Min:         time.Minute,
			Max:         time.Hour,
			Group:       moduleName,
			Description: "How long a phone-login verification code stays valid after it is sent.",
		},
		{
			Key:         ConfigKeySMSCodeMaxAttempts,
			Type:        "int",
			Default:     DefaultSMSCodeMaxAttempts,
			Min:         3,
			Max:         10,
			Group:       moduleName,
			Description: "How many wrong codes a single issued verification code tolerates before it locks and a fresh one must be requested.",
		},
	}
	return append(items, socialCredentialItems()...)
}

// socialCredentialItems is the per-channel credential schema, one client id
// and one Sensitive client secret each.
//
// The secretKey field below holds a CONFIG-ITEM KEY NAME string constant
// (e.g. ConfigKeyGoogleClientSecret = "authn.social.google.client_secret",
// already //nolint:gosec'd at its declaration) -- never the secret's actual
// value, which lives encrypted in the configs table and is never held in
// this function at all. CodeQL's go/clear-text-logging traces this field
// name into an eventual log call in the reference app's main.go and flags
// it; reviewed and confirmed a false positive on both ends of that flow (see
// main.go's comment at the flagged log call for the full trace). If this
// struct's field is ever renamed, re-check that alert rather than assuming
// the reasoning still lines up.
func socialCredentialItems() []pkgcore.ConfigItem {
	channels := []struct {
		idKey, secretKey, label, idName, secretName string
	}{
		{ConfigKeyGoogleClientID, ConfigKeyGoogleClientSecret, "Google", "client id", "client secret"},
		{ConfigKeyGitHubClientID, ConfigKeyGitHubClientSecret, "GitHub", "client id", "client secret"},
		{ConfigKeyWeChatClientID, ConfigKeyWeChatClientSecret, "WeChat", "appid", "secret"},
		{ConfigKeyDingTalkClientID, ConfigKeyDingTalkClientSecret, "DingTalk", "application key", "application secret"},
		{ConfigKeyFeishuClientID, ConfigKeyFeishuClientSecret, "Feishu", "app id", "app secret"},
	}

	items := make([]pkgcore.ConfigItem, 0, 2*len(channels))
	for _, channel := range channels {
		items = append(items,
			pkgcore.ConfigItem{
				Key:         channel.idKey,
				Type:        "string",
				Default:     "",
				Group:       moduleName,
				Description: channel.label + " OAuth " + channel.idName + ".",
			},
			pkgcore.ConfigItem{
				Key:         channel.secretKey,
				Type:        "string",
				Default:     "",
				Sensitive:   true,
				Group:       moduleName,
				Description: channel.label + " OAuth " + channel.secretName + ".",
			},
		)
	}
	return items
}
