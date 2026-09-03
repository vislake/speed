package authn

import (
	"embed"
	"errors"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/authn/locales"
	"github.com/vislake/speed/go/authn/migrations"
)

const (
	// moduleName is authn's pkgcore.Module.Name(). It is also the key
	// dbkit.MigrationRegistry uses in its dependency graph and the prefix
	// of every message id, error code, event type and audit action this
	// module owns.
	moduleName = "authn"

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
	// ConfigKeyImmediateRevocation switches sign-out from natural expiry
	// to the KVStore-backed revocation list Middleware consults. It is a
	// configuration value rather than a deployment-mode branch: both modes
	// work in both deployment modes.
	ConfigKeyImmediateRevocation = "authn.session_revocation_immediate"
)

// FeatureFlagPasswordLogin gates password sign-in as a channel. A deployment
// that authenticates only through enterprise SSO turns it off, and the login
// page reads the answer from the pre-auth feature endpoint before anyone has
// signed in.
const FeatureFlagPasswordLogin = "authn.password_login"

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

// options accumulates everything NewService and NewModule can be configured
// with.
type options struct {
	keys           *KeySet
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
}

// Option configures the authn module and the service inside it.
type Option func(*options)

// WithSigningKeys supplies the access-token key set. It is REQUIRED: there is
// no safe default, and a key generated at startup would invalidate every
// outstanding session on every restart and give each replica of a distributed
// deployment a different one.
func WithSigningKeys(keys *KeySet) Option {
	return func(o *options) { o.keys = keys }
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
// user belongs to a tenant. Without it, sign-in and tenant switching fail
// closed with ErrTenantMembershipUnavailable.
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

// WithRevocationMode selects natural expiry or the immediate revocation list.
// An unrecognised mode is ignored, leaving the default.
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

// newOptions applies opts over the defaults and rejects a configuration the
// module cannot run with.
func newOptions(opts []Option) (options, error) {
	cfg := options{
		now:            time.Now,
		issuer:         DefaultIssuer,
		accessTTL:      DefaultAccessTokenTTL,
		refreshTTL:     DefaultRefreshTokenTTL,
		sessionTTL:     DefaultSessionTTL,
		revocationMode: RevocationModeNatural,
		passwordParams: DefaultPasswordParams(),
		passwordPolicy: DefaultPasswordPolicy(),
		oauthStateTTL:  DefaultOAuthStateTTL,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}

	if cfg.keys == nil {
		return options{}, errors.New("authn: a signing key set is required; supply one with WithSigningKeys")
	}
	if len(cfg.blindIndexKey) != blindIndexKeySize {
		return options{}, errors.New("authn: a 32-byte blind-index key is required; supply one with WithBlindIndexKey")
	}
	return cfg, nil
}

// Module implements pkgcore.Module for authn.
type Module struct {
	db   *gorm.DB
	opts []Option
	svc  *Service
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

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module. authn depends on infrastructure
// (dbkit, tenancy, observability) rather than on another business module, and
// infrastructure is not part of the bootstrap set, so this is genuinely
// empty. In particular it does NOT depend on org: the membership question is
// asked through the injected MembershipReader seam precisely so that the
// dependency does not exist.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module. It returns nil for now: this
// module's HTTP surface is spec-first, so the fragment appears together with
// the generated server interface and the handler that implements it, and
// returning a fragment before those exist would advertise endpoints nothing
// serves.
func (m *Module) OpenAPISpec() []byte { return nil }

// Service returns the module's service. It is nil until Register has run,
// because the service needs the event bus and key-value store the registry
// carries.
func (m *Module) Service() *Service { return m.svc }

// Register implements pkgcore.Module.
//
// It performs no I/O, as the interface requires. Building the service wires
// already-constructed in-memory values together and reads two fields off the
// registry; nothing here opens a connection, sends a request or touches the
// database. Note also that reg.Locales() is deliberately not consulted: the
// merged catalog is installed only after every module has registered, so it
// is nil at this point by design.
func (m *Module) Register(reg *pkgcore.Registry) error {
	svc, err := NewService(m.db, reg.EventBus(), reg.KVStore(), m.opts...)
	if err != nil {
		return err
	}
	m.svc = svc

	if err := reg.Events.Publishes(eventDecls...); err != nil {
		return err
	}
	if err := reg.AuditActions.Add(auditActions...); err != nil {
		return err
	}
	if err := reg.Config.Add(configItems()...); err != nil {
		return err
	}
	if err := reg.Permissions.Add(PermissionSSOManage); err != nil {
		return err
	}
	return reg.Features.Add(featureFlags()...)
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
			Key:         ConfigKeyImmediateRevocation,
			Type:        "bool",
			Default:     false,
			Group:       moduleName,
			Description: "Enforces session revocation on every request through the shared key-value store, instead of waiting for access tokens to expire.",
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
	}
	return append(items, socialCredentialItems()...)
}

// socialCredentialItems is the per-channel credential schema, one client id
// and one Sensitive client secret each.
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

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
