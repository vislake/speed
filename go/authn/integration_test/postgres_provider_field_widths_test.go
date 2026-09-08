//go:build integration

package authn_test

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

// The two surfaces this file pins sit in the same seam: the dual-dialect
// write boundary for third-party-provided strings. The provider-reported
// profile fields on user_identities (external_id, display_name, avatar_url)
// and the provider-minted users.display_name are written straight into
// fixed-width columns, so a social or SSO sign-in carrying an over-width
// profile would succeed on SQLite (which ignores a declared width) and fail
// on real PostgreSQL with SQLSTATE 22001 -- and TouchLogin's every-login
// rewrite of display_name/avatar_url would break an EXISTING identity the
// moment its provider's profile grew past the column. The unit tier
// (identity_test.go, package authn) pins the bounded storage on SQLite;
// these legs re-run the same flows against real PostgreSQL, where the
// unguarded behaviour is not a wrong stored value but a refused write.
//
// The over-width shapes below mirror the unit tier's (identity_test.go's
// overWidthName/overWidthAvatar/overWidthSubject) and the migrations'
// VARCHAR widths (0001_create_users.sql, 0005_create_user_identities.sql) --
// this external test package cannot import the module's unexported width
// constants, the same reason postgres_refresh_rotation_test.go restates
// testPassword.
const (
	pgExternalIDWidth  = 191
	pgDisplayNameWidth = 128
	pgAvatarURLWidth   = 512
)

var (
	pgOverWidthName    = strings.Repeat("名", pgDisplayNameWidth) + strings.Repeat("尾", 72)
	pgOverWidthAvatar  = strings.Repeat("a", pgAvatarURLWidth) + strings.Repeat("b", 98)
	pgOverWidthSubject = strings.Repeat("x", pgExternalIDWidth) + strings.Repeat("y", 59)
)

// widthProbeProvider is a scripted social channel carrying a mutable
// provider profile, the external-package twin of the unit tier's stubProvider
// (provider_test.go): the module's SocialProvider interface is the only
// exported surface an external test package may drive a social sign-in
// through, and mutating the profile between two sign-ins is what models a
// provider whose reports grow over time.
type widthProbeProvider struct {
	name     string
	identity *authn.ExternalIdentity
}

func (p *widthProbeProvider) Name() string { return p.name }

func (p *widthProbeProvider) AuthorizeURL(state, redirectURI string) string {
	return "https://provider.example.test/authorize?state=" + url.QueryEscape(state) +
		"&redirect_uri=" + url.QueryEscape(redirectURI)
}

func (p *widthProbeProvider) Exchange(_ context.Context, code, redirectURI string) (*authn.ExternalIdentity, error) {
	clone := *p.identity
	return &clone, nil
}

// newProviderWidthService assembles an authn.Service over db with the full
// federation wiring a social sign-in needs: the scripted channel, the
// verified-and-trusted auto-link rule, a redirect allowlist, signing keys,
// the blind-index key, members as the membership reader, and fast argon2id
// parameters. It is the external twin of the unit tier's
// newFederationFixture.
func newProviderWidthService(t *testing.T, db *gorm.DB, members *testutil.Memberships, provider *widthProbeProvider, allowlist authn.RedirectAllowlist) *authn.Service {
	t.Helper()

	keys := testutil.NewKeySource(t, "kid-provider-widths")
	svc, err := authn.NewService(db, pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(),
		authn.WithKeySource(keys),
		authn.WithBlindIndexKey(testutil.BlindIndexKey()),
		authn.WithMembershipReader(members),
		authn.WithPasswordParams(authn.PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}),
		authn.WithSocialProviders(provider),
		authn.WithTrustedProviders(provider.Name()),
		authn.WithRedirectAllowlist(allowlist),
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return svc
}

// socialCallback runs one authorize-then-callback round trip against
// provider's channel on svc, the same two-step shape a browser follows.
func socialCallback(t *testing.T, svc *authn.Service, provider *widthProbeProvider, tenantID pkgcore.TenantID) (*authn.SocialLoginResult, error) {
	t.Helper()
	authorizeURL, err := svc.SocialAuthorizeURL(t.Context(), authn.SocialAuthorizeInput{
		Provider:    provider.Name(),
		RedirectURI: "https://app.example.com/callback",
	})
	if err != nil {
		t.Fatalf("SocialAuthorizeURL() error = %v", err)
	}
	query, err := url.ParseQuery(strings.TrimPrefix(authorizeURL, "https://provider.example.test/authorize?"))
	if err != nil {
		t.Fatalf("ParseQuery(authorizeURL) error = %v", err)
	}
	state := query.Get("state")
	if state == "" {
		t.Fatal("SocialAuthorizeURL() produced no state parameter")
	}
	return svc.SocialCallback(t.Context(), authn.SocialCallbackInput{
		Provider: provider.Name(),
		Code:     "test-code",
		State:    state,
		TenantID: tenantID,
	})
}

// TestSocialSignIn_OverWidthProviderProfile_Postgres re-runs the unit tier's
// first-bind shape against real PostgreSQL: a social sign-in whose
// provider reports a 200-rune name, 610-rune avatar and 250-rune subject
// must SUCCEED here -- an unguarded identity-row insert would be refused
// with SQLSTATE 22001 (value too long for type character varying) where
// SQLite stores the values verbatim -- and the row must hold each bounded
// head. The second callback proves the over-width subject still resolves to
// the same identity on the next login.
func TestSocialSignIn_OverWidthProviderProfile_Postgres(t *testing.T) {
	t.Parallel()

	db := testutil.NewPostgresDB(t)
	members := testutil.NewMemberships()
	provider := &widthProbeProvider{name: authn.ProviderGoogle, identity: &authn.ExternalIdentity{
		ExternalID:    pgOverWidthSubject,
		Email:         "shared@example.com",
		EmailVerified: true,
		Name:          pgOverWidthName,
		Avatar:        pgOverWidthAvatar,
	}}
	allowlist, err := authn.NewRedirectAllowlist("https://app.example.com/callback")
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	svc := newProviderWidthService(t, db, members, provider, allowlist)

	user, err := svc.Register(t.Context(), authn.RegisterInput{
		Email: "shared@example.com", Password: testPassword,
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	members.Add(user.ID, pkgcore.TenantID("tenant-a"))

	result, err := socialCallback(t, svc, provider, pkgcore.TenantID("tenant-a"))
	if err != nil {
		t.Fatalf("SocialCallback(over-width profile) error = %v", err)
	}
	if result.Tokens == nil {
		t.Fatal("Tokens = nil, want a session for the successful sign-in")
	}

	identity, err := svc.Identities().FindByExternal(t.Context(), authn.ProviderGoogle, pgOverWidthSubject)
	if err != nil {
		t.Fatalf("FindByExternal(original over-width subject) error = %v", err)
	}
	if want := strings.Repeat("x", pgExternalIDWidth); identity.ExternalID != want {
		t.Errorf("stored external_id = %q-ish, want the %d-rune head of the subject", identity.ExternalID, pgExternalIDWidth)
	}
	if want := strings.Repeat("名", pgDisplayNameWidth); identity.DisplayName != want {
		t.Errorf("stored display_name = %q-ish, want the %d-rune head of the name", identity.DisplayName, pgDisplayNameWidth)
	}
	if want := strings.Repeat("a", pgAvatarURLWidth); identity.AvatarURL != want {
		t.Errorf("stored avatar_url = %q-ish, want the %d-rune head of the avatar", identity.AvatarURL, pgAvatarURLWidth)
	}

	// The same over-width subject on the next sign-in must find this same
	// identity: the lookup compares in the same bounded form the write stored.
	result2, err := socialCallback(t, svc, provider, pkgcore.TenantID("tenant-a"))
	if err != nil {
		t.Fatalf("second SocialCallback() error = %v", err)
	}
	if result2.Identity.ID != result.Identity.ID {
		t.Errorf("second sign-in resolved to identity %q, want the first sign-in's %q", result2.Identity.ID, result.Identity.ID)
	}
	if result2.Tokens == nil {
		t.Fatal("second sign-in returned no session")
	}
}

// TestSocialSignIn_GrownProviderProfile_Postgres re-runs the unit tier's
// grown-profile regression against real PostgreSQL: the identity signs in
// while the provider's profile fits its columns, the provider then grows the
// name and avatar past VARCHAR(128) and VARCHAR(512), and the next login --
// which refreshes the stored profile through TouchLogin -- must still
// succeed with the grown values bounded at the write. An unguarded refresh
// would refuse that very login with SQLSTATE 22001 and turn the refresh
// into a self-inflicted breaker of an existing identity.
func TestSocialSignIn_GrownProviderProfile_Postgres(t *testing.T) {
	t.Parallel()

	db := testutil.NewPostgresDB(t)
	members := testutil.NewMemberships()
	provider := &widthProbeProvider{name: authn.ProviderGoogle, identity: &authn.ExternalIdentity{
		ExternalID:    "google-grown-1",
		Email:         "grow@example.com",
		EmailVerified: true,
		Name:          "Short Provider Name",
		Avatar:        "https://a.example/small.png",
	}}
	allowlist, err := authn.NewRedirectAllowlist("https://app.example.com/callback")
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	svc := newProviderWidthService(t, db, members, provider, allowlist)

	user, err := svc.Register(t.Context(), authn.RegisterInput{
		Email: "grow@example.com", Password: testPassword,
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	members.Add(user.ID, pkgcore.TenantID("tenant-a"))

	if _, err := socialCallback(t, svc, provider, pkgcore.TenantID("tenant-a")); err != nil {
		t.Fatalf("first SocialCallback() error = %v", err)
	}

	// The provider's profile grows past both columns between two sign-ins.
	provider.identity.Name = pgOverWidthName
	provider.identity.Avatar = pgOverWidthAvatar

	if _, err := socialCallback(t, svc, provider, pkgcore.TenantID("tenant-a")); err != nil {
		t.Fatalf("second SocialCallback() error = %v (the grown profile must not break an existing identity's login)", err)
	}

	identity, err := svc.Identities().FindByExternal(t.Context(), authn.ProviderGoogle, "google-grown-1")
	if err != nil {
		t.Fatalf("FindByExternal() error = %v", err)
	}
	if want := strings.Repeat("名", pgDisplayNameWidth); identity.DisplayName != want {
		t.Errorf("stored display_name = %q-ish, want the %d-rune head of the grown name", identity.DisplayName, pgDisplayNameWidth)
	}
	if want := strings.Repeat("a", pgAvatarURLWidth); identity.AvatarURL != want {
		t.Errorf("stored avatar_url = %q-ish, want the %d-rune head of the grown URL", identity.AvatarURL, pgAvatarURLWidth)
	}
}

// TestSocialSignIn_MintOverWidthDisplayName_Postgres covers the users
// display_name writer the account mint owns: a first social sign-in with an
// unmatched address provisions a brand-new user whose display name is the
// PROVIDER's name, and on real PostgreSQL an over-width name would refuse
// the mint's users insert outright (SQLSTATE 22001) where SQLite stores it.
// The mint must land with the bounded head, and the sign-in must fail --
// if at all -- with the SAME answer on both dialects: the membership refusal
// that follows successful provisioning.
func TestSocialSignIn_MintOverWidthDisplayName_Postgres(t *testing.T) {
	t.Parallel()

	db := testutil.NewPostgresDB(t)
	members := testutil.NewMemberships()
	provider := &widthProbeProvider{name: authn.ProviderGoogle, identity: &authn.ExternalIdentity{
		ExternalID:    "google-mint-1",
		Email:         "brand-new-mint@example.com",
		EmailVerified: true,
		Name:          pgOverWidthName,
	}}
	allowlist, err := authn.NewRedirectAllowlist("https://app.example.com/callback")
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	svc := newProviderWidthService(t, db, members, provider, allowlist)

	_, err = socialCallback(t, svc, provider, pkgcore.TenantID("tenant-a"))
	if err == nil {
		t.Fatal("social sign-in unexpectedly succeeded; a fresh account has no membership yet")
	}
	appErr, ok := apperr.As(err)
	if !ok || appErr.Code != authn.ErrTenantMembershipRequired.Code {
		t.Fatalf("social sign-in error = %v, want the membership refusal that follows successful provisioning", err)
	}

	created, err := svc.Users().FindByEmail(t.Context(), "brand-new-mint@example.com")
	if err != nil {
		t.Fatalf("the minted account was not provisioned: %v", err)
	}
	if want := strings.Repeat("名", pgDisplayNameWidth); created.DisplayName != want {
		t.Errorf("users.display_name = %q-ish, want the %d-rune head of the provider's name", created.DisplayName, pgDisplayNameWidth)
	}

	identity, err := svc.Identities().FindByExternal(t.Context(), authn.ProviderGoogle, "google-mint-1")
	if err != nil {
		t.Fatalf("the minted identity was not provisioned: %v", err)
	}
	if want := strings.Repeat("名", pgDisplayNameWidth); identity.DisplayName != want {
		t.Errorf("user_identities.display_name = %q-ish, want the %d-rune head of the provider's name", identity.DisplayName, pgDisplayNameWidth)
	}
}

// TestSSOSignIn_OverWidthClaims_Postgres is the enterprise-SSO leg of the
// same write boundary: the subject, name and picture an identity provider
// puts in its ID token are third-party strings landing in the same
// fixed-width user_identities columns. A full relying-party round trip
// against a real OIDCServer with an over-width first token must bind
// successfully with each value bounded, and a second token carrying the same
// over-width subject must resolve to the same identity.
func TestSSOSignIn_OverWidthClaims_Postgres(t *testing.T) {
	t.Parallel()

	const tenant = "tenant-a"
	tenantID := pkgcore.TenantID(tenant)

	db := testutil.NewPostgresDB(t)
	members := testutil.NewMemberships()
	server := testutil.NewOIDCServer(t, "enterprise-client")

	keys := testutil.NewKeySource(t, "kid-sso-widths")
	ssoAllowlist, err := authn.NewRedirectAllowlist("https://app.example.com/sso/callback")
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	svc, err := authn.NewService(db, pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(),
		authn.WithKeySource(keys),
		authn.WithBlindIndexKey(testutil.BlindIndexKey()),
		authn.WithMembershipReader(members),
		authn.WithPasswordParams(authn.PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}),
		authn.WithFederationHTTPClient(server.Client()),
		authn.WithRedirectAllowlist(ssoAllowlist),
	)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}

	member, err := svc.Register(t.Context(), authn.RegisterInput{
		Email: "member@example.com", Password: testPassword,
	})
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	members.Add(member.ID, tenantID)

	ctx := pkgcore.WithTenant(t.Context(), tenantID)
	config := &authn.TenantSSOConfig{
		TenantID:     tenant,
		ID:           "sso-config-1",
		Issuer:       server.URL(),
		ClientID:     "enterprise-client",
		ClientSecret: "sso-client-secret",
		Enabled:      true,
	}
	config.SetAllowedDomains([]string{"example.com"})
	if err := svc.SSO().Configs().Create(ctx, config); err != nil {
		t.Fatalf("write the tenant sso config: %v", err)
	}

	ssoSignIn := func(code string) (*authn.SocialLoginResult, error) {
		t.Helper()
		authorizeURL, authorizeErr := svc.SSO().AuthorizeURL(ctx, "https://app.example.com/sso/callback", "")
		if authorizeErr != nil {
			t.Fatalf("AuthorizeURL() error = %v", authorizeErr)
		}
		parsed, parseErr := url.Parse(authorizeURL)
		if parseErr != nil {
			t.Fatalf("Parse(authorizeURL) error = %v", parseErr)
		}
		state, nonce := parsed.Query().Get("state"), parsed.Query().Get("nonce")
		if state == "" || nonce == "" {
			t.Fatalf("AuthorizeURL() = %q, missing state or nonce", authorizeURL)
		}
		server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
			Subject:       pgOverWidthSubject,
			Email:         "member@example.com",
			EmailVerified: true,
			Name:          pgOverWidthName,
			Picture:       pgOverWidthAvatar,
			Nonce:         nonce,
		}))
		return svc.SSO().Callback(t.Context(), authn.SSOCallbackInput{
			TenantID: tenantID, Code: code, State: state,
		})
	}

	result, err := ssoSignIn("the-code")
	if err != nil {
		t.Fatalf("SSO Callback(over-width claims) error = %v", err)
	}
	if result.Tokens == nil {
		t.Fatal("Tokens = nil, want a session")
	}

	identity, err := svc.Identities().FindByExternal(t.Context(), authn.SSOChannelName(tenantID), pgOverWidthSubject)
	if err != nil {
		t.Fatalf("FindByExternal(original over-width subject) error = %v", err)
	}
	if want := strings.Repeat("x", pgExternalIDWidth); identity.ExternalID != want {
		t.Errorf("stored external_id = %q-ish, want the %d-rune head of the subject", identity.ExternalID, pgExternalIDWidth)
	}
	if want := strings.Repeat("名", pgDisplayNameWidth); identity.DisplayName != want {
		t.Errorf("stored display_name = %q-ish, want the %d-rune head of the claimed name", identity.DisplayName, pgDisplayNameWidth)
	}
	if want := strings.Repeat("a", pgAvatarURLWidth); identity.AvatarURL != want {
		t.Errorf("stored avatar_url = %q-ish, want the %d-rune head of the claimed picture", identity.AvatarURL, pgAvatarURLWidth)
	}

	result2, err := ssoSignIn("the-code-2")
	if err != nil {
		t.Fatalf("second SSO Callback() error = %v", err)
	}
	if result2.Identity.ID != result.Identity.ID {
		t.Errorf("second sign-in resolved to identity %q, want the first sign-in's %q", result2.Identity.ID, result.Identity.ID)
	}
}
