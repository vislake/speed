package authn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/authn/internal/safehttp"
	obs "github.com/vislake/speed/go/observability"
)

// PermissionSSOManage is the permission a tenant administrator needs to read
// or write their tenant's enterprise single sign-on configuration.
//
// It is the only permission authn declares. Everything else this module
// serves is self-service -- a person acting on their own account -- and needs
// authentication rather than authorization. Configuring SSO is different: it
// decides how every member of a tenant signs in, so it is an administrative
// act on the tenant, not on the caller.
const PermissionSSOManage = "authn:sso_manage"

// TenantSSOConfig is one tenant's enterprise OpenID Connect relying-party
// configuration: which identity provider the tenant federates with, under
// which client credentials, and which email domains that provider is
// authoritative for.
//
// It is the one TENANT-domain table this module owns, so unlike every other
// model here it implements dbkit.TenantScoped and is reached exclusively
// through dbkit.Repository[T], which injects the tenant filter. The tenancy
// suite that pins that is in oidc_test.go.
//
// TenantID is declared DIRECTLY with a primaryKey tag rather than by
// embedding dbkit.TenantModel. That is required, not stylistic: TenantModel's
// own tag omits primaryKey, and shadowing the promoted field to add one
// silently breaks GetTenantID -- dbkit's tenant_scope.go documents exactly
// how, and the failure mode is that FindByID denies the row's legitimate
// owner.
type TenantSSOConfig struct {
	// TenantID is the owning tenant and the leftmost column of the
	// composite primary key, per the backend standard's rule for
	// tenant-scoped tables.
	//
	// It deliberately carries no separate uniqueness constraint of its own.
	// docs/internal/05 specifies one configuration per tenant as a product
	// rule, enforced by SaveConfig reading Current before deciding whether
	// to create or update -- but a DB-level UNIQUE index on tenant_id alone
	// would reject the second of two rows the mandatory
	// tenancytest.AssertIsolated suite deliberately creates per tenant to
	// prove List actually filters (a single-row list cannot distinguish
	// "correctly scoped" from "returned everything"). The suite is not
	// negotiable; the constraint that could not coexist with it is. See
	// Current's doc comment for how "at most one" is kept true in the
	// normal path despite the database no longer enforcing it.
	TenantID string `gorm:"column:tenant_id;primaryKey;size:64"`

	// ID is an application-generated UUID, the second half of the
	// composite key.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// Enabled reports whether the tenant's members may sign in through
	// this provider. A configuration that exists but is disabled behaves
	// exactly like no configuration at all.
	Enabled bool `gorm:"column:enabled;not null"`

	// Issuer is the identity provider's OpenID Connect issuer URL. It is
	// typed by a tenant administrator, which makes every outbound request
	// derived from it a server-side request forgery candidate -- see
	// internal/safehttp.
	Issuer string `gorm:"column:issuer;size:512;not null"`

	// ClientID is the relying-party client identifier the provider issued.
	ClientID string `gorm:"column:client_id;size:255;not null"`

	// ClientSecret is the matching secret, ENCRYPTED at rest through
	// dbkit's serializer. A tenant administrator's secret sitting in
	// plaintext in a shared table is a breach waiting for one careless
	// database export.
	ClientSecret string `gorm:"column:client_secret;serializer:authn_pii"`

	// AllowedDomains is the whitespace-delimited list of email domains
	// this provider is authoritative for. Read and write it through
	// AllowedDomainList and SetAllowedDomains.
	//
	// It is a delimited string rather than a native array (PostgreSQL
	// only, banned) or a JSON document that would then have to be filtered
	// with JSONB operators (also banned, and nothing filters on it anyway).
	AllowedDomains string `gorm:"column:allowed_domains;size:1024;not null"`

	CreatedAt time.Time `gorm:"autoCreateTime;not null"`
	UpdatedAt time.Time `gorm:"autoUpdateTime;not null"`
}

// TableName pins the table name.
func (TenantSSOConfig) TableName() string { return "tenant_sso_configs" }

// GetTenantID implements dbkit.TenantScoped.
func (c TenantSSOConfig) GetTenantID() pkgcore.TenantID { return pkgcore.TenantID(c.TenantID) }

// AllowedDomainList returns the configured email domains, lowercased.
func (c *TenantSSOConfig) AllowedDomainList() []string {
	fields := strings.Fields(c.AllowedDomains)
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		out = append(out, strings.ToLower(field))
	}
	return out
}

// SetAllowedDomains stores domains, lowercased and whitespace-delimited.
func (c *TenantSSOConfig) SetAllowedDomains(domains []string) {
	fields := make([]string, 0, len(domains))
	for _, domain := range domains {
		for _, part := range strings.Fields(domain) {
			fields = append(fields, strings.ToLower(strings.TrimPrefix(part, "@")))
		}
	}
	c.AllowedDomains = strings.Join(fields, " ")
}

// AllowsDomain reports whether email's domain is one this provider is
// authoritative for.
func (c *TenantSSOConfig) AllowsDomain(email string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 || at == len(email)-1 {
		return false
	}
	domain := strings.ToLower(email[at+1:])
	for _, allowed := range c.AllowedDomainList() {
		if allowed == domain {
			return true
		}
	}
	return false
}

// compile-time check that TenantSSOConfig is tenant-scoped data.
var _ dbkit.TenantScoped = TenantSSOConfig{}

// The tenant_sso_configs and provider-column widths below are the schema
// authority for the REFUSALS they feed, the same way model.go's column-width
// constants are for the identity tables': PostgreSQL enforces a declared
// width (SQLSTATE 22001 on an over-width write) while SQLite ignores it, so
// a value that fits on one dialect and overflows on the other is a real
// dual-dialect divergence. Whether an over-width value is refused or cut
// depends on who supplied it -- model.go's doc comment makes the per-column
// decision for the provider-reported and diagnostic strings (cut). A
// tenant-administrator's OWN configuration values take the other branch:
// they are REFUSED with a named error naming the field, never truncated.
// Silently shortening an issuer URL would point enterprise single sign-on
// at the wrong endpoint; a truncated client id or domain list would simply
// not work. The refusals therefore live at both write surfaces --
// SSOService.SaveConfig, where the administrator sees the answer while they
// are looking at the form, and SSOConfigRepository's own Create and Update,
// so no write path can slip an over-width value past the boundary.
//
// PostgreSQL counts CHARACTERS against a VARCHAR(n) width, so every bound
// is applied in runes, never bytes.
const (
	// ssoIssuerWidth is the VARCHAR width of tenant_sso_configs.issuer
	// (migration 0006).
	ssoIssuerWidth = 512
	// ssoClientIDWidth is the VARCHAR width of
	// tenant_sso_configs.client_id (migration 0006).
	ssoClientIDWidth = 255
	// ssoAllowedDomainsWidth is the VARCHAR width of
	// tenant_sso_configs.allowed_domains (migration 0006) -- the stored
	// whitespace-delimited list SetAllowedDomains builds, not the input
	// slice.
	ssoAllowedDomainsWidth = 1024
	// ssoTenantIDMaxWidth is the longest tenant id the enterprise channel
	// can serve. The synthetic "oidc:<tenant>" provider name an SSO
	// identity is stored under lands in user_identities.provider,
	// VARCHAR(64) (migration 0005), whose width leaves 59 characters for
	// the tenant after the "oidc:" prefix. Truncating the provider name is
	// not an option: the (provider, external_id) unique index would then
	// conflate two tenants' identities, silently merging distinct tenants'
	// sign-ins. A tenant id longer than the budget therefore cannot use
	// enterprise SSO at all, and it is REFUSED with ErrSSOTenantIDTooLong
	// wherever it enters the SSO path (SaveConfig, AuthorizeURL and
	// Callback) -- the host learns WHICH tenant name is too long at
	// configuration/entry time, never through a broken identity write at
	// some later sign-in.
	ssoTenantIDMaxWidth = identityProviderWidth - len(ProviderOIDCPrefix)
)

// validateSSOConfigWidths refuses a configuration whose stored form would
// overflow one of its columns on PostgreSQL, naming the field in the error
// (ErrSSOIssuerTooLong, ErrSSOClientIDTooLong, ErrSSOAllowedDomainsTooLong,
// each carrying a "max_length" parameter). See the width constants above for
// why a configuration value is refused rather than truncated. It is the
// single check behind both SSOConfigRepository.Create and .Update, so
// SaveConfig and any direct repository writer meet the same refusal.
func validateSSOConfigWidths(config *TenantSSOConfig) error {
	if utf8.RuneCountInString(config.Issuer) > ssoIssuerWidth {
		return ErrSSOIssuerTooLong.WithParam("max_length", ssoIssuerWidth)
	}
	if utf8.RuneCountInString(config.ClientID) > ssoClientIDWidth {
		return ErrSSOClientIDTooLong.WithParam("max_length", ssoClientIDWidth)
	}
	if utf8.RuneCountInString(config.AllowedDomains) > ssoAllowedDomainsWidth {
		return ErrSSOAllowedDomainsTooLong.WithParam("max_length", ssoAllowedDomainsWidth)
	}
	return nil
}

// validateSSOTenantID refuses a tenant id the enterprise channel cannot
// represent: one longer than ssoTenantIDMaxWidth runes would make the
// synthetic "oidc:<tenant>" provider name overflow user_identities.provider
// (VARCHAR(64)) at the identity write, which PostgreSQL would refuse with a
// raw 22001 where SQLite silently stored the value. The refusal
// (ErrSSOTenantIDTooLong, carrying "max_length") fires at the three points
// where a tenant id enters the SSO path -- SaveConfig, AuthorizeURL and
// Callback -- so the host is told at configuration/entry time, never at a
// random login.
func validateSSOTenantID(tenantID pkgcore.TenantID) error {
	if utf8.RuneCountInString(string(tenantID)) > ssoTenantIDMaxWidth {
		return ErrSSOTenantIDTooLong.WithParam("max_length", ssoTenantIDMaxWidth)
	}
	return nil
}

// SSOConfigRepository is the tenant-scoped repository for TenantSSOConfig.
//
// It embeds dbkit.Repository[TenantSSOConfig] rather than holding a *gorm.DB,
// which is what injects the tenant filter into every read and write. Current
// is expressed through the embedded List rather than through a hand-written
// query for the same reason: List carries the filter, a query built here
// would not.
type SSOConfigRepository struct {
	*dbkit.Repository[TenantSSOConfig]
}

// NewSSOConfigRepository returns a repository backed by db, which is expected
// to come from dbkit.Open so that the isolation plugin is installed.
func NewSSOConfigRepository(db *gorm.DB) *SSOConfigRepository {
	return &SSOConfigRepository{Repository: dbkit.NewRepository[TenantSSOConfig](db)}
}

// Create validates the configuration's column widths before delegating to
// the embedded repository, so no write path -- SaveConfig or a direct
// caller -- can land a value that SQLite would store and PostgreSQL would
// refuse with SQLSTATE 22001. The refusal names the field
// (validateSSOConfigWidths).
func (r *SSOConfigRepository) Create(ctx context.Context, config *TenantSSOConfig) error {
	if err := validateSSOConfigWidths(config); err != nil {
		return err
	}
	return r.Repository.Create(ctx, config)
}

// Update validates the configuration's column widths before delegating to
// the embedded repository, the same gate Create applies (see its doc
// comment).
func (r *SSOConfigRepository) Update(ctx context.Context, config *TenantSSOConfig) error {
	if err := validateSSOConfigWidths(config); err != nil {
		return err
	}
	return r.Repository.Update(ctx, config)
}

// Current returns the configuration of the tenant in ctx, or ErrNotFound.
//
// A tenant has at most one under the normal path -- SaveConfig always reads
// Current first and updates the existing row rather than creating a second
// one -- but nothing at the database enforces that (see TenantID's doc
// comment for why), so a rare race between two concurrent first-time
// SaveConfig calls could momentarily leave two rows for the same tenant.
// Current resolves that deterministically rather than arbitrarily: the
// most recently updated row wins, ties broken by ID, so every reader agrees
// on the same answer and the very next SaveConfig collapses back to one row
// by updating whichever Current returned.
func (r *SSOConfigRepository) Current(ctx context.Context) (*TenantSSOConfig, error) {
	configs, err := r.List(ctx)
	if err != nil {
		return nil, err
	}
	if len(configs) == 0 {
		return nil, ErrNotFound
	}
	current := &configs[0]
	for i := 1; i < len(configs); i++ {
		candidate := &configs[i]
		if candidate.UpdatedAt.After(current.UpdatedAt) ||
			(candidate.UpdatedAt.Equal(current.UpdatedAt) && candidate.ID > current.ID) {
			current = candidate
		}
	}
	return current, nil
}

// SSOConfigInput is what an administrator submits to configure their tenant's
// identity provider.
type SSOConfigInput struct {
	// Issuer is the provider's OpenID Connect issuer URL.
	Issuer string
	// ClientID and ClientSecret are the relying-party credentials.
	ClientID     string
	ClientSecret string
	// AllowedDomains are the email domains the provider is authoritative
	// for. They gate automatic linking, so an empty list means no linking
	// happens at all.
	AllowedDomains []string
	// Enabled turns the configuration on.
	Enabled bool
}

// SSOCallbackInput describes an enterprise single sign-on callback.
type SSOCallbackInput struct {
	// TenantID is the tenant whose configuration the flow belongs to. It
	// comes from the callback route, which is per tenant, and is verified
	// against the server-side state record rather than trusted.
	TenantID pkgcore.TenantID
	// Code is the authorization code the provider returned.
	Code string
	// State is the value the provider echoed back.
	State string
	// SessionBinding must equal what the authorization request supplied.
	SessionBinding string
	// Device, UserAgent and IP describe the client.
	Device    string
	UserAgent string
	IP        string
}

// SSOService is the enterprise OpenID Connect relying party.
//
// It is deliberately separate from the social channels. docs/internal/05 is
// explicit that the two are different mechanisms with different configuration
// levels -- SSO is per tenant and configured by the tenant's own
// administrator, social login is per platform and configured by the operator
// -- and collapsing them into one abstraction would mean a tenant
// administrator's settings form could reach the platform's channels.
type SSOService struct {
	svc        *Service
	configs    *SSOConfigRepository
	httpClient *http.Client
	guard      *safehttp.Guard

	// mu guards the memoized discovery results, keyed by issuer URL, the
	// forgotten generation counter below, and nothing else. Discovery
	// itself is a round trip to a third party whose answer changes almost
	// never; repeating it per sign-in would put that third party on the
	// latency path of every login. But memoizing it must never mean holding
	// mu across the round trip: the only bound on that fetch is the HTTP
	// client's own timeout, so a lock held across it would queue every
	// other tenant's discovery -- and with it every other tenant's SSO
	// sign-in -- behind one issuer that stopped answering. discover()
	// therefore fetches OUTSIDE the lock and re-checks the map afterwards
	// (double-checked memoization); see its own doc comment.
	mu         sync.Mutex
	discovered map[string]*oidc.Provider
	// forgotten counts every forget() that has ever run. discover captures
	// it when it decides to fetch and compares it before storing, so a
	// forget that lands while a fetch is in flight wins over that fetch's
	// store -- see discover's own doc comment.
	forgotten uint64
}

// newSSOService assembles the relying party from a Service's own wiring.
func newSSOService(svc *Service, db *gorm.DB, cfg options) (*SSOService, error) {
	if db == nil {
		return nil, errors.New("authn: newSSOService requires a database handle")
	}
	client := cfg.federationClient
	guard := safehttp.NewGuard()
	if client == nil {
		client = guard.Client()
	}
	return &SSOService{
		svc:        svc,
		configs:    NewSSOConfigRepository(db),
		httpClient: client,
		guard:      guard,
		discovered: make(map[string]*oidc.Provider),
	}, nil
}

// Configs returns the tenant-scoped configuration repository.
func (s *SSOService) Configs() *SSOConfigRepository { return s.configs }

// SaveConfig writes the calling tenant's configuration.
//
// The tenant id in context is the first thing validated: one longer than the
// "oidc:<tenant>" channel name can represent (ssoTenantIDMaxWidth runes) is
// REFUSED here, at configuration time, because no identity it ever resolves
// could be stored under its tenant's channel name without overflowing
// user_identities.provider on PostgreSQL (see validateSSOTenantID).
//
// The configuration values are then width-checked in their STORED forms
// (issuer and client id are stored trimmed, the domain list lowercased and
// joined), refused with an error naming the field rather than truncated:
// this is a tenant administrator's own specification, and a silently
// shortened issuer URL would point enterprise single sign-on at the wrong
// endpoint (see the width constants' comment block). The repository applies
// the same refusal as a backstop, so no write path can bypass it.
//
// Finally the issuer is validated through the SSRF guard BEFORE anything is
// stored, so a tenant administrator cannot persist a URL pointing at the
// deployment's own network and have the server fetch it later. Validating
// at write time rather than only at use time also means the administrator
// sees the error while they are looking at the form.
func (s *SSOService) SaveConfig(ctx context.Context, in SSOConfigInput) (*TenantSSOConfig, error) {
	tenantID, ok := pkgcore.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return nil, ErrTenantMembershipRequired
	}
	if err := validateSSOTenantID(tenantID); err != nil {
		return nil, err
	}
	stored := &TenantSSOConfig{
		Issuer:   strings.TrimSpace(in.Issuer),
		ClientID: strings.TrimSpace(in.ClientID),
	}
	stored.SetAllowedDomains(in.AllowedDomains)
	if err := validateSSOConfigWidths(stored); err != nil {
		return nil, err
	}
	if _, err := s.guard.ValidateURL(ctx, in.Issuer); err != nil {
		return nil, ErrSSOIssuerNotAllowed.WithCause(err)
	}
	if stored.ClientID == "" {
		return nil, ErrSSOIssuerNotAllowed
	}

	existing, err := s.configs.Current(ctx)
	switch {
	case err == nil:
		// The issuer changes below, so the URL whose discovery document the
		// memo may hold is the one the row had BEFORE this write. Capture it
		// first: forget must evict the OLD issuer -- an A-to-B change that
		// forgot B instead would leave A's document memoized forever, and
		// switching back to A later would silently reuse a document that
		// predates the change. (The forget runs only after the update
		// commits, so a failed write leaves the memo intact; when the
		// issuer did not change, the forget drops a still-valid entry that
		// the next discovery simply refetches.)
		previousIssuer := existing.Issuer
		existing.Issuer = stored.Issuer
		existing.ClientID = stored.ClientID
		existing.ClientSecret = in.ClientSecret
		existing.Enabled = in.Enabled
		existing.SetAllowedDomains(in.AllowedDomains)
		if updateErr := s.configs.Update(ctx, existing); updateErr != nil {
			return nil, updateErr
		}
		s.forget(previousIssuer)
		return existing, nil
	case errors.Is(err, ErrNotFound):
		created := &TenantSSOConfig{
			TenantID:     string(tenantID),
			ID:           newID(),
			Issuer:       stored.Issuer,
			ClientID:     stored.ClientID,
			ClientSecret: in.ClientSecret,
			Enabled:      in.Enabled,
		}
		created.SetAllowedDomains(in.AllowedDomains)
		if createErr := s.configs.Create(ctx, created); createErr != nil {
			return nil, createErr
		}
		return created, nil
	default:
		return nil, err
	}
}

// AuthorizeURL builds the URL to send a tenant's member to their identity
// provider, issuing a single-use state and an OpenID Connect nonce.
//
// The nonce is what binds the ID token the provider eventually issues to THIS
// flow. It matters more here than for a social channel: one identity provider
// commonly serves many tenants and many relying parties, so an ID token
// captured from one flow is a plausible thing for an attacker to have.
func (s *SSOService) AuthorizeURL(ctx context.Context, redirectURI, sessionBinding string) (string, error) {
	// The tenant-width gate, before any state is issued: an over-long
	// tenant id can never complete a sign-in (its "oidc:<tenant>"
	// provider name would overflow user_identities.provider), so a state
	// issued for it would only lead a member to a broken callback. A
	// configuration row for such a tenant can only exist because it was
	// written before the SaveConfig gate -- or straight through the
	// repository -- so the entry path refuses it itself rather than
	// trusting the write path to have done so. See validateSSOTenantID.
	if tenantID, ok := pkgcore.TenantFromContext(ctx); ok {
		if err := validateSSOTenantID(tenantID); err != nil {
			return "", err
		}
	}
	// The channel gate, before the tenant's SSO configuration is even
	// read: a deployment that turned enterprise SSO off must not send
	// anyone to an identity provider. The ctx's tenant (when the host's
	// route resolved one) selects the flag's per-tenant tier; a tenant-
	// less ctx reads the platform-wide value, exactly as the pre-auth
	// features endpoint answers the login page.
	if err := s.svc.channelEnabled(ctx, FeatureFlagEnterpriseSSO); err != nil {
		return "", err
	}
	config, err := s.enabledConfig(ctx)
	if err != nil {
		return "", err
	}
	if !s.svc.redirects.Allows(redirectURI) {
		return "", ErrRedirectURINotAllowed
	}

	provider, err := s.discover(ctx, config.Issuer)
	if err != nil {
		return "", err
	}

	nonce, err := NewOAuthNonce()
	if err != nil {
		return "", ErrInternal.WithCause(err)
	}
	state, err := s.svc.states.Issue(ctx, StateBinding{
		Provider:       ProviderOIDCPrefix + config.TenantID,
		RedirectURI:    redirectURI,
		SessionBinding: sessionBinding,
		Nonce:          nonce,
	})
	if err != nil {
		return "", ErrInternal.WithCause(err)
	}

	conf := oauth2.Config{
		ClientID:    config.ClientID,
		Endpoint:    provider.Endpoint(),
		RedirectURL: redirectURI,
		Scopes:      []string{oidc.ScopeOpenID, "email", "profile"},
	}
	return conf.AuthCodeURL(state, oidc.Nonce(nonce)), nil
}

// ssoClaims is the subset of an enterprise ID token this module reads.
type ssoClaims struct {
	Subject       string   `json:"sub"`
	Email         string   `json:"email"`
	EmailVerified flexBool `json:"email_verified"`
	Name          string   `json:"name"`
	Picture       string   `json:"picture"`
}

// Callback completes an enterprise sign-in and returns the new session.
//
// An address claim the identity provider did not assert as verified is worth
// nothing in this resolution: it neither links an existing account nor mints
// a new one, and the refusal is ErrIdentityRequiresBinding either way -- the
// same error, before any account lookup, so the answer never discloses
// whether the address is registered. Many enterprise identity providers omit
// the email_verified claim from their ID tokens by default; a tenant that
// configures one of those gets its enterprise sign-ins refused until the
// addresses are verified through the identity provider, which is the
// intended safe state.
//
// An existing account is linked automatically only when the identity provider
// asserts the address is verified, the address's domain is one the tenant
// registered, AND the existing account is already an ACTIVE MEMBER of that
// tenant. The membership condition is the one that is easy to leave out and
// expensive to leave out. Without it, a tenant administrator -- who is the
// person that configures the issuer and the allowed domains, and who may run
// the identity provider themselves -- could allowlist a public email domain
// and sign straight into the account of any platform user who happens to use
// an address there. With it, the worst they can do is take over an account
// that was already inside their own tenant, which they already administer.
//
// A verified address claim on an allowed domain, for a subject that has no
// account yet, is provisioned just-in-time: an account is created, its
// subject bound to it, and EventUserCreated published -- but the account is
// deliberately created with NO email, and the claimed address is kept only
// on the identity row as display data (see resolveAccount's mint branch for
// the reasoning: the assertion behind it is tenant-grade, and seating it in
// the platform-unique verified-email index would let the tenant's
// administrator capture the address's true owner's later trusted-provider
// sign-ins). The membership condition cannot apply to that mint -- the
// account does not exist yet, so there is nothing to be a member of -- but
// no session and no membership is granted by it either: authn never grants
// tenant membership on its own, the host's membership machinery grants it in
// reaction to EventUserCreated, and the subject's next sign-in attempt
// completes the session.
func (s *SSOService) Callback(ctx context.Context, in SSOCallbackInput) (*SocialLoginResult, error) {
	if in.TenantID == "" {
		return nil, ErrSSONotConfigured
	}
	// The tenant-width gate: the callback route's tenant id becomes the
	// "oidc:<tenant>" provider name an identity row would be stored under,
	// so an over-long one is refused with the named error here rather than
	// left to break the identity write with a raw 22001 on PostgreSQL.
	// Under the fixed code paths this is unreachable -- SaveConfig and
	// AuthorizeURL both refuse such a tenant before a flow can start -- and
	// it exists so a flow begun before those gates existed answers the same
	// named refusal instead of a random database error. See
	// validateSSOTenantID.
	if err := validateSSOTenantID(in.TenantID); err != nil {
		return nil, err
	}
	// The channel gate on the same tenant-bearing context enabledConfig
	// resolves with below, so a flow started while the flag was on cannot
	// complete as a sign-in after it was turned off.
	tenantCtx := pkgcore.WithTenant(ctx, in.TenantID)
	if err := s.svc.channelEnabled(tenantCtx, FeatureFlagEnterpriseSSO); err != nil {
		return nil, err
	}
	if in.Code == "" {
		return nil, ErrOAuthStateInvalid
	}
	config, err := s.enabledConfig(tenantCtx)
	if err != nil {
		return nil, err
	}

	channel := ProviderOIDCPrefix + config.TenantID
	binding, err := s.svc.states.Consume(ctx, in.State, channel, in.SessionBinding)
	if err != nil {
		return nil, err
	}

	provider, err := s.discover(ctx, config.Issuer)
	if err != nil {
		return nil, err
	}

	clientCtx := oidc.ClientContext(ctx, s.httpClient)
	conf := oauth2.Config{
		ClientID:     config.ClientID,
		ClientSecret: config.ClientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  binding.RedirectURI,
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
	token, err := conf.Exchange(clientCtx, in.Code)
	if err != nil {
		return nil, ErrSSOTokenInvalid.WithCause(fmt.Errorf("sso token exchange: %w", err))
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, ErrSSOTokenInvalid.WithCause(errors.New("the identity provider returned no id token"))
	}

	idToken, err := provider.VerifierContext(clientCtx, &oidc.Config{ClientID: config.ClientID}).Verify(clientCtx, rawIDToken)
	if err != nil {
		return nil, ErrSSOTokenInvalid.WithCause(fmt.Errorf("sso id token: %w", err))
	}
	if idToken.Nonce != binding.Nonce {
		// An ID token that does not carry this flow's nonce is an ID
		// token from some other flow.
		return nil, ErrSSOTokenInvalid
	}

	var claims ssoClaims
	if err := idToken.Claims(&claims); err != nil {
		return nil, ErrSSOTokenInvalid.WithCause(err)
	}
	if claims.Subject == "" {
		return nil, ErrSSOTokenInvalid
	}

	external := &ExternalIdentity{
		Provider:      channel,
		ExternalID:    claims.Subject,
		Email:         strings.TrimSpace(claims.Email),
		EmailVerified: bool(claims.EmailVerified) && claims.Email != "",
		Name:          claims.Name,
		Avatar:        claims.Picture,
	}
	return s.signIn(ctx, config, external, in)
}

// signIn resolves the ID token's subject to an account and starts a session.
func (s *SSOService) signIn(ctx context.Context, config *TenantSSOConfig, external *ExternalIdentity, in SSOCallbackInput) (*SocialLoginResult, error) {
	tenantID := pkgcore.TenantID(config.TenantID)
	result := &SocialLoginResult{}

	identity, err := s.svc.identities.FindByExternal(ctx, external.Provider, external.ExternalID)
	switch {
	case err == nil:
		user, findErr := s.svc.users.FindByID(ctx, identity.UserID)
		if findErr != nil {
			return nil, findErr
		}
		// Merge what this sign-in's provider just claimed onto the stored
		// identity before TouchLogin below persists it (the same refresh
		// the social channels' own sign-in applies): display data only --
		// the identity row's email never resolves an account -- and a field
		// the provider no longer reports arrives empty and clears the
		// stored value instead of leaving it stale.
		identity.Email = strings.TrimSpace(external.Email)
		identity.DisplayName = external.Name
		identity.AvatarURL = external.Avatar
		result.User, result.Identity = user, identity
	case errors.Is(err, ErrNotFound):
		user, created, linkErr := s.resolveAccount(ctx, config, external)
		if linkErr != nil {
			return nil, linkErr
		}
		newIdentity, createErr := s.svc.createIdentity(ctx, user.ID, external)
		if createErr != nil {
			return nil, createErr
		}
		result.User, result.Identity = user, newIdentity
		result.Created = created
		result.AutoLinked = !created
		s.svc.publishIdentityBound(ctx, newIdentity, result.AutoLinked)
	default:
		return nil, err
	}

	if result.User.Status != UserStatusActive {
		return nil, ErrInvalidCredentials
	}

	tokens, err := s.svc.startExternalSession(ctx, result.User,
		[]string{MethodOIDC}, MethodOIDC, tenantID, in.Device, in.UserAgent, in.IP)
	if err != nil {
		return nil, err
	}
	result.Tokens = tokens

	if touchErr := s.svc.identities.TouchLogin(ctx, result.Identity, s.svc.now()); touchErr != nil {
		obs.FromContext(ctx).Warn("sso identity last-login could not be recorded",
			"user_id", result.User.ID, "error", touchErr)
	}
	return result, nil
}

// resolveAccount decides which account an unrecognised enterprise subject
// belongs to, and refuses everything an unverified claim must not reach:
//
//   - An address the identity provider did not assert as verified is refused
//     with ErrIdentityRequiresBinding before any account lookup, so the
//     answer never discloses whether the address is registered and nothing
//     is ever provisioned from such a claim.
//   - An address outside the tenant's domain allowlist is refused with
//     ErrSSODomainNotAllowed.
//   - An existing account is linked only when its holder is already an ACTIVE
//     MEMBER of the tenant that configured the identity provider; the
//     verified bar above already cleared, the same ErrIdentityRequiresBinding
//     covers a would-be link to a non-member.
//   - Only a verified, domain-allowed address with no account is minted
//     just-in-time -- and the mint deliberately creates the account with NO
//     email, keeping the claimed address on the identity row as display data
//     only. EventUserCreated is published and the caller binds the subject
//     to the account; the account's own flows need no email (the subject
//     signs in over the bound identity), and seating a tenant-grade verified
//     claim in the platform-unique email index is exactly the manufactured
//     "platform-verified" seat the social channels' auto-link rule must
//     never see (see the mint branch's own comment).
//
// The membership condition cannot apply to the mint -- the account does not
// exist yet, so there is nothing to be a member of -- but the mint grants
// neither membership nor a session (see Callback's doc comment).
func (s *SSOService) resolveAccount(ctx context.Context, config *TenantSSOConfig, external *ExternalIdentity) (*User, bool, error) {
	email := strings.TrimSpace(external.Email)
	if email == "" {
		return nil, false, ErrSSOTokenInvalid
	}
	if !config.AllowsDomain(email) {
		return nil, false, ErrSSODomainNotAllowed
	}
	if !external.EmailVerified {
		obs.FromContext(ctx).Info("sso sign-in refused an unverified address claim",
			"tenant_id", config.TenantID,
			"domain_allowed", true,
		)
		return nil, false, ErrIdentityRequiresBinding
	}

	existing, err := s.svc.users.FindByEmail(ctx, email)
	switch {
	case err == nil:
		member, memberErr := s.memberOf(ctx, existing.ID, pkgcore.TenantID(config.TenantID))
		if memberErr != nil {
			return nil, false, memberErr
		}
		if !member {
			obs.FromContext(ctx).Info("sso sign-in refused an automatic account link",
				"tenant_id", config.TenantID,
				"user_id", existing.ID,
				"already_a_member", member,
			)
			return nil, false, ErrIdentityRequiresBinding
		}
		return existing, false, nil
	case errors.Is(err, ErrNotFound):
		// Just-in-time provisioning, reached only with the verified
		// assertion from above. The account is deliberately created with
		// NO email: the address stays on the identity row, where it is
		// display data, and is never seated in the platform-unique
		// verified-email index.
		//
		// The verified assertion above is a TENANT-grade one -- an
		// identity provider this tenant's own administrator configured,
		// for a domain they registered, an administrator who may run the
		// identity provider themselves (the same premise the memberOf
		// gate on the linking branch above exists to bound). The
		// platform-unique email index is what the SOCIAL channels'
		// verified-and-trusted auto-link rule resolves against: an
		// account seated there with a verified address reads to that rule
		// as platform-grade evidence that its holder controls the
		// address. Seating a tenant-grade claim would let a tenant
		// administrator mint a "verified" account at any address whose
		// domain they listed -- say a public domain -- and the true
		// owner's later, genuinely verified sign-in through a trusted
		// social channel would then be absorbed INTO that account. The
		// mint must therefore leave the address free for its true owner,
		// exactly as the unverified-claim refusal above leaves it free:
		// the account exists for the tenant's own SSO flows (the subject
		// signs in over the bound identity), but nothing about it can
		// capture the owner's later sign-ins. The memberOf gate the
		// linking branch applies is structurally impossible here: the
		// account does not exist yet, and nothing about minting it grants
		// membership or a session.
		user := &User{
			DisplayName: external.Name,
			Status:      UserStatusActive,
		}
		if createErr := s.svc.users.Create(ctx, user); createErr != nil {
			return nil, false, createErr
		}
		s.svc.publish(ctx, pkgcore.Event{
			Type:     EventUserCreated,
			TenantID: pkgcore.TenantID(config.TenantID),
			Payload:  UserCreatedPayload{UserID: user.ID, HasEmail: false},
		})
		return user, true, nil
	default:
		return nil, false, err
	}
}

// memberOf asks the membership seam, failing closed when it cannot answer.
func (s *SSOService) memberOf(ctx context.Context, userID string, tenantID pkgcore.TenantID) (bool, error) {
	if s.svc.membership == nil {
		return false, ErrTenantMembershipUnavailable
	}
	member, err := s.svc.membership.ActiveMembership(ctx, userID, tenantID)
	if err != nil {
		return false, ErrTenantMembershipUnavailable.WithCause(err)
	}
	return member, nil
}

// enabledConfig returns the calling tenant's configuration, or
// ErrSSONotConfigured when there is none or it is turned off.
func (s *SSOService) enabledConfig(ctx context.Context) (*TenantSSOConfig, error) {
	config, err := s.configs.Current(ctx)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrSSONotConfigured
		}
		return nil, err
	}
	if !config.Enabled {
		return nil, ErrSSONotConfigured
	}
	return config, nil
}

// discover memoizes an issuer's OpenID Connect discovery document.
//
// The fetch happens OUTSIDE s.mu, and the map is only ever touched under
// the lock for the brief reads and the single map store below -- see the
// mu field's doc comment for why holding the lock across the round trip
// would let one tenant's black-holed issuer queue every other tenant's
// sign-in behind its timeout. Callers see a double-checked lookup: read the
// memo, fetch when it misses, then re-check before storing.
//
// Two concurrent first-time discoveries of the SAME issuer can each fetch,
// since the miss is checked without a per-issuer in-flight registry; that
// duplicate is bounded by concurrent first-time sign-ins for one issuer and
// harmless next to the cost of the fetch itself. The re-check under the
// lock makes the map converge on one stored result either way: whichever
// discover stores first wins, and the other keeps its own equally valid
// document for its caller without overwriting the stored one.
//
// A forget() that runs while a fetch is in flight must WIN over that
// fetch's store, or an eviction would be silently undone -- the memoized
// document resurrected -- by a discovery the eviction was meant to make
// stale. The forgotten generation counter makes that precise: the fetch
// captures the counter when it sees its miss and stores only when no
// forget has run since. The counter is deliberately coarse (any forget,
// not just one for this issuer, suppresses the store): the cost of the
// imprecision is one extra fetch of an unrelated issuer, and the cost of
// tracking per-issuer forgets would be unbounded bookkeeping for no
// benefit. The in-flight caller still receives the document it fetched --
// it asked for it and the fetch succeeded -- it is simply not memoized,
// so the next caller fetches afresh.
func (s *SSOService) discover(ctx context.Context, issuer string) (*oidc.Provider, error) {
	s.mu.Lock()
	provider, ok := s.discovered[issuer]
	generation := s.forgotten
	s.mu.Unlock()
	if ok {
		return provider, nil
	}

	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, s.httpClient), issuer)
	if err != nil {
		return nil, ErrSSOIssuerNotAllowed.WithCause(fmt.Errorf("sso discovery: %w", err))
	}

	// Re-check rather than storing unconditionally: a concurrent discover
	// may have stored its own result for this issuer while this one was
	// fetching, and a concurrent forget() may have run -- the re-check must
	// not resurrect a document the forget evicted (see this method's doc
	// comment).
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.discovered[issuer]; ok {
		return existing, nil
	}
	if s.forgotten != generation {
		return provider, nil
	}
	s.discovered[issuer] = provider
	return provider, nil
}

// forget drops a memoized discovery document, so a configuration change takes
// effect without a restart, and bumps the forgotten generation counter so any
// discovery fetch already in flight for the evicted issuer does not store its
// now-stale document back (see discover's doc comment).
func (s *SSOService) forget(issuer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.discovered, issuer)
	s.forgotten++
}

// SSOChannelName returns the provider name an enterprise identity is stored
// under for tenantID. It is exported so a support tool can look a binding up
// without reconstructing the convention by hand.
//
// The name lands in user_identities.provider (VARCHAR(64)), so it only
// exists for a tenant id within ssoTenantIDMaxWidth runes; SSOService
// refuses an over-long tenant id at every point where it enters the SSO
// path (ErrSSOTenantIDTooLong), so no binding is ever stored under a name
// this function would return for such a tenant.
func SSOChannelName(tenantID pkgcore.TenantID) string {
	return ProviderOIDCPrefix + string(tenantID)
}
