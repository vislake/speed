package authn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

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

	// mu guards the memoized discovery results, keyed by issuer URL.
	// Discovery is a round trip to a third party whose answer changes
	// almost never; repeating it per sign-in would put that third party on
	// the latency path of every login.
	mu         sync.Mutex
	discovered map[string]*oidc.Provider
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
// The issuer is validated through the SSRF guard BEFORE anything is stored,
// so a tenant administrator cannot persist a URL pointing at the deployment's
// own network and have the server fetch it later. Validating at write time
// rather than only at use time also means the administrator sees the error
// while they are looking at the form.
func (s *SSOService) SaveConfig(ctx context.Context, in SSOConfigInput) (*TenantSSOConfig, error) {
	tenantID, ok := pkgcore.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return nil, ErrTenantMembershipRequired
	}
	if _, err := s.guard.ValidateURL(ctx, in.Issuer); err != nil {
		return nil, ErrSSOIssuerNotAllowed.WithCause(err)
	}
	if strings.TrimSpace(in.ClientID) == "" {
		return nil, ErrSSOIssuerNotAllowed
	}

	existing, err := s.configs.Current(ctx)
	switch {
	case err == nil:
		existing.Issuer = strings.TrimSpace(in.Issuer)
		existing.ClientID = strings.TrimSpace(in.ClientID)
		existing.ClientSecret = in.ClientSecret
		existing.Enabled = in.Enabled
		existing.SetAllowedDomains(in.AllowedDomains)
		if updateErr := s.configs.Update(ctx, existing); updateErr != nil {
			return nil, updateErr
		}
		s.forget(existing.Issuer)
		return existing, nil
	case errors.Is(err, ErrNotFound):
		created := &TenantSSOConfig{
			TenantID:     string(tenantID),
			ID:           newID(),
			Issuer:       strings.TrimSpace(in.Issuer),
			ClientID:     strings.TrimSpace(in.ClientID),
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
// The account resolution here is deliberately stricter than the social one.
// An existing account is linked automatically only when all three hold: the
// identity provider asserts the address is verified, the address's domain is
// one the tenant registered, AND the existing account is already an ACTIVE
// MEMBER of that tenant.
//
// The third condition is the one that is easy to leave out and expensive to
// leave out. Without it, a tenant administrator -- who is the person that
// configures the issuer and the allowed domains, and who may run the identity
// provider themselves -- could allowlist a public email domain and sign
// straight into the account of any platform user who happens to use an
// address there. With it, the worst they can do is take over an account that
// was already inside their own tenant, which they already administer.
func (s *SSOService) Callback(ctx context.Context, in SSOCallbackInput) (*SocialLoginResult, error) {
	if in.TenantID == "" {
		return nil, ErrSSONotConfigured
	}
	if in.Code == "" {
		return nil, ErrOAuthStateInvalid
	}
	config, err := s.enabledConfig(pkgcore.WithTenant(ctx, in.TenantID))
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
// belongs to. See Callback's doc comment for the three linking conditions and
// why the membership one is there.
func (s *SSOService) resolveAccount(ctx context.Context, config *TenantSSOConfig, external *ExternalIdentity) (*User, bool, error) {
	email := strings.TrimSpace(external.Email)
	if email == "" {
		return nil, false, ErrSSOTokenInvalid
	}
	if !config.AllowsDomain(email) {
		return nil, false, ErrSSODomainNotAllowed
	}

	existing, err := s.svc.users.FindByEmail(ctx, email)
	switch {
	case err == nil:
		member, memberErr := s.memberOf(ctx, existing.ID, pkgcore.TenantID(config.TenantID))
		if memberErr != nil {
			return nil, false, memberErr
		}
		if !external.EmailVerified || !member {
			obs.FromContext(ctx).Info("sso sign-in refused an automatic account link",
				"tenant_id", config.TenantID,
				"user_id", existing.ID,
				"email_verified", external.EmailVerified,
				"already_a_member", member,
			)
			return nil, false, ErrIdentityRequiresBinding
		}
		return existing, false, nil
	case errors.Is(err, ErrNotFound):
		// Just-in-time provisioning. The address is stored as verified
		// because the identity provider that asserted it is one this
		// tenant's own administrator configured AND the address is in a
		// domain they registered -- which together is the strongest
		// assertion about an address this module ever gets.
		user := &User{
			DisplayName:   external.Name,
			Status:        UserStatusActive,
			Email:         email,
			EmailVerified: bool(external.EmailVerified),
		}
		if createErr := s.svc.users.Create(ctx, user); createErr != nil {
			return nil, false, createErr
		}
		s.svc.publish(ctx, pkgcore.Event{
			Type:     EventUserCreated,
			TenantID: pkgcore.TenantID(config.TenantID),
			Payload:  UserCreatedPayload{UserID: user.ID, HasEmail: true},
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
func (s *SSOService) discover(ctx context.Context, issuer string) (*oidc.Provider, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if provider, ok := s.discovered[issuer]; ok {
		return provider, nil
	}
	provider, err := oidc.NewProvider(oidc.ClientContext(ctx, s.httpClient), issuer)
	if err != nil {
		return nil, ErrSSOIssuerNotAllowed.WithCause(fmt.Errorf("sso discovery: %w", err))
	}
	s.discovered[issuer] = provider
	return provider, nil
}

// forget drops a memoized discovery document, so a configuration change takes
// effect without a restart.
func (s *SSOService) forget(issuer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.discovered, issuer)
}

// SSOChannelName returns the provider name an enterprise identity is stored
// under for tenantID. It is exported so a support tool can look a binding up
// without reconstructing the convention by hand.
func SSOChannelName(tenantID pkgcore.TenantID) string {
	return ProviderOIDCPrefix + string(tenantID)
}
