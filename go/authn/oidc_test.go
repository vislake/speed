package authn

import (
	"testing"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// ssoRedirectURI is the redirect URI every enterprise single sign-on fixture
// in this file registers.
const ssoRedirectURI = "https://app.example.com/sso/callback"

// newSSOFixture assembles a Service whose enterprise relying party talks to
// server with no SSRF guard in front of it. The guard is proven separately in
// internal/safehttp and in TestSSOService_SaveConfig_RefusesAnSSRFCandidateIssuer;
// every other test here is about what happens once a tenant's issuer has
// already been accepted.
func newSSOFixture(t *testing.T, server *testutil.OIDCServer) *serviceFixture {
	t.Helper()
	allowlist, err := NewRedirectAllowlist(ssoRedirectURI)
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	return newServiceFixture(t,
		WithFederationHTTPClient(server.Client()),
		WithRedirectAllowlist(allowlist),
	)
}

func TestSSOConfigRepository_AssertIsolated(t *testing.T) {
	t.Parallel()

	db := testutil.NewDB(t)
	repo := NewSSOConfigRepository(db)

	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *TenantSSOConfig {
		config := &TenantSSOConfig{
			TenantID: string(tenant),
			ID:       newID(),
			Issuer:   "https://idp.example.com",
			ClientID: "client-id",
			Enabled:  true,
		}
		config.SetAllowedDomains([]string{"example.com"})
		return config
	})
}

// TestTenantSSOConfig_ClientSecretIsEncryptedAtRest proves the secret column
// holds ciphertext, the same way TestUser_EmailIsEncryptedAtRest proves it for
// the identity domain's own encrypted columns.
func TestTenantSSOConfig_ClientSecretIsEncryptedAtRest(t *testing.T) {
	t.Parallel()

	db := testutil.NewDB(t)
	repo := NewSSOConfigRepository(db)
	tenantID := pkgcore.TenantID("tenant-secret-at-rest")

	config := &TenantSSOConfig{
		TenantID:     string(tenantID),
		ID:           newID(),
		Issuer:       "https://idp.example.com",
		ClientID:     "client-id",
		ClientSecret: "the-actual-secret",
		Enabled:      true,
	}
	if err := repo.Create(pkgcore.WithTenant(t.Context(), tenantID), config); err != nil {
		t.Fatalf("create: %v", err)
	}

	var stored []byte
	if err := db.Raw("SELECT client_secret FROM tenant_sso_configs WHERE id = ?", config.ID).Row().Scan(&stored); err != nil {
		t.Fatalf("read the raw client_secret column: %v", err)
	}
	if len(stored) == 0 {
		t.Fatal("the raw client_secret column is empty")
	}
	if string(stored) == "the-actual-secret" {
		t.Fatal("the raw client_secret column holds the plaintext secret")
	}

	readBack, err := repo.FindByID(pkgcore.WithTenant(t.Context(), tenantID), config.ID)
	if err != nil {
		t.Fatalf("find by id: %v", err)
	}
	if readBack.ClientSecret != "the-actual-secret" {
		t.Fatalf("decrypted client secret = %q, want the original", readBack.ClientSecret)
	}
}

// TestSSOService_SaveConfig_RequiresTenantContext proves the write path fails
// closed with no tenant in context, which is what stops a caller from
// choosing which tenant's configuration it writes.
func TestSSOService_SaveConfig_RequiresTenantContext(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	_, err := f.svc.SSO().SaveConfig(t.Context(), SSOConfigInput{
		Issuer: "https://idp.example.com", ClientID: "client-id", Enabled: true,
	})
	assertErrorCode(t, err, ErrTenantMembershipRequired.Code)
}

// TestSSOService_SaveConfig_RefusesAnSSRFCandidateIssuer is the write-time
// half of internal/safehttp's contract: a tenant administrator's issuer URL
// is validated BEFORE it is ever stored, so the server-side request forgery
// candidate never reaches the database, let alone a later fetch.
func TestSSOService_SaveConfig_RefusesAnSSRFCandidateIssuer(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t) // no federation http client override: the real guard is in effect
	ctx := pkgcore.WithTenant(t.Context(), testTenantA)

	_, err := f.svc.SSO().SaveConfig(ctx, SSOConfigInput{
		Issuer:   "http://169.254.169.254/latest/meta-data/",
		ClientID: "client-id",
		Enabled:  true,
	})
	assertErrorCode(t, err, ErrSSOIssuerNotAllowed.Code)

	if _, findErr := f.svc.SSO().Configs().Current(ctx); findErr == nil {
		t.Error("a refused issuer must not have been persisted")
	}
}

// TestSSOService_AuthorizeURL_RefusesAnUnlistedRedirect mirrors the social
// channel's own redirect-allowlist rule for the enterprise relying party.
func TestSSOService_AuthorizeURL_RefusesAnUnlistedRedirect(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "enterprise-client")
	f := newSSOFixture(t, server)
	ctx := pkgcore.WithTenant(t.Context(), testTenantA)
	writeSSOConfig(t, f, testTenantA, server, "enterprise-client", "example.com")

	_, err := f.svc.SSO().AuthorizeURL(ctx, "https://attacker.example.com/callback", "")
	assertErrorCode(t, err, ErrRedirectURINotAllowed.Code)
}

// TestSSOService_Callback_RefusesWhenNotConfigured covers both shapes of "not
// configured": no row at all, and a row that exists but is disabled. Neither
// may be distinguished from the other by an unauthenticated caller.
func TestSSOService_Callback_RefusesWhenNotConfigured(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "enterprise-client")
	f := newSSOFixture(t, server)

	_, err := f.svc.SSO().AuthorizeURL(pkgcore.WithTenant(t.Context(), testTenantA), ssoRedirectURI, "")
	assertErrorCode(t, err, ErrSSONotConfigured.Code)

	_, err = f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: testTenantA, Code: "code", State: "irrelevant",
	})
	assertErrorCode(t, err, ErrSSONotConfigured.Code)

	config := writeSSOConfig(t, f, testTenantA, server, "enterprise-client", "example.com")
	config.Enabled = false
	if updateErr := f.svc.SSO().Configs().Update(pkgcore.WithTenant(t.Context(), testTenantA), config); updateErr != nil {
		t.Fatalf("disable the config: %v", updateErr)
	}
	_, err = f.svc.SSO().AuthorizeURL(pkgcore.WithTenant(t.Context(), testTenantA), ssoRedirectURI, "")
	assertErrorCode(t, err, ErrSSONotConfigured.Code)
}

// ssoAuthorize starts an enterprise sign-in flow for tenantID and returns the
// state and nonce a compliant identity provider is handed.
func ssoAuthorize(t *testing.T, f *serviceFixture, tenantID pkgcore.TenantID) (state, nonce string) {
	t.Helper()
	authorizeURL, err := f.svc.SSO().AuthorizeURL(pkgcore.WithTenant(t.Context(), tenantID), ssoRedirectURI, "")
	if err != nil {
		t.Fatalf("AuthorizeURL() error = %v", err)
	}
	query := parseQuery(t, authorizeURL)
	state, nonce = query.Get("state"), query.Get("nonce")
	if state == "" || nonce == "" {
		t.Fatalf("AuthorizeURL() = %q, missing state or nonce", authorizeURL)
	}
	return state, nonce
}

// TestSSOService_Callback_FullRoundTrip is the round's full relying-party
// proof: a real ID token, signed by a locally generated key, served through a
// real discovery document and JWKS, verified end to end -- and the account it
// resolves to must already be an active member of the tenant that configured
// this identity provider, which is the third linking condition
// Callback's doc comment explains.
func TestSSOService_Callback_FullRoundTrip(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "enterprise-client")
	f := newSSOFixture(t, server)
	writeSSOConfig(t, f, testTenantA, server, "enterprise-client", "example.com")
	member := f.registerUser(t, "member@example.com", testTenantA)

	state, nonce := ssoAuthorize(t, f, testTenantA)
	server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
		Subject:       "enterprise-subject-1",
		Email:         "member@example.com",
		EmailVerified: true,
		Nonce:         nonce,
	}))

	result, err := f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: testTenantA, Code: "the-code", State: state,
	})
	if err != nil {
		t.Fatalf("Callback() error = %v", err)
	}
	if result.User.ID != member.ID {
		t.Errorf("User.ID = %q, want the existing tenant member %q", result.User.ID, member.ID)
	}
	if !result.AutoLinked {
		t.Error("AutoLinked = false, want true: verified, allowed domain, already a member")
	}
	if result.Tokens == nil {
		t.Fatal("Tokens = nil, want a session")
	}
	if result.Identity.Provider != SSOChannelName(testTenantA) {
		t.Errorf("Identity.Provider = %q, want %q", result.Identity.Provider, SSOChannelName(testTenantA))
	}

	forms := server.TokenForms()
	if len(forms) != 1 || forms[0].Get("redirect_uri") != ssoRedirectURI {
		t.Errorf("token exchange forms = %v, want one call carrying redirect_uri=%q", forms, ssoRedirectURI)
	}
}

// TestSSOService_Callback_RefusesWhenNotAMember is the third linking
// condition on its own: a verified address in an allowed domain is still not
// enough when the account is not already inside the tenant that configured
// the identity provider. Without this check a tenant administrator could
// allowlist a public email domain and sign into any platform user's account
// at that domain.
func TestSSOService_Callback_RefusesWhenNotAMember(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "enterprise-client")
	f := newSSOFixture(t, server)
	writeSSOConfig(t, f, testTenantA, server, "enterprise-client", "example.com")
	f.registerUser(t, "outsider@example.com") // no membership of testTenantA

	state, nonce := ssoAuthorize(t, f, testTenantA)
	server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
		Subject: "enterprise-subject-2", Email: "outsider@example.com", EmailVerified: true, Nonce: nonce,
	}))

	_, err := f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: testTenantA, Code: "code", State: state,
	})
	assertErrorCode(t, err, ErrIdentityRequiresBinding.Code)
}

// TestSSOService_Callback_RefusesAnUnverifiedEmail is the first linking
// condition: the identity provider must itself assert the address was
// verified. Domain allowlisting is a tenant's word about where its accounts
// live, not a check of any one person's control over an address.
func TestSSOService_Callback_RefusesAnUnverifiedEmail(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "enterprise-client")
	f := newSSOFixture(t, server)
	writeSSOConfig(t, f, testTenantA, server, "enterprise-client", "example.com")
	member := f.registerUser(t, "member2@example.com", testTenantA)
	_ = member

	state, nonce := ssoAuthorize(t, f, testTenantA)
	server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
		Subject: "enterprise-subject-3", Email: "member2@example.com", EmailVerified: false, Nonce: nonce,
	}))

	_, err := f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: testTenantA, Code: "code", State: state,
	})
	assertErrorCode(t, err, ErrIdentityRequiresBinding.Code)
}

// TestSSOService_Callback_RefusesADomainNotOnTheTenantsAllowlist is the
// second linking condition, and it is checked before any account lookup:
// a tenant only speaks for the domains it registered.
func TestSSOService_Callback_RefusesADomainNotOnTheTenantsAllowlist(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "enterprise-client")
	f := newSSOFixture(t, server)
	writeSSOConfig(t, f, testTenantA, server, "enterprise-client", "example.com")

	state, nonce := ssoAuthorize(t, f, testTenantA)
	server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
		Subject: "enterprise-subject-4", Email: "person@not-example.com", EmailVerified: true, Nonce: nonce,
	}))

	_, err := f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: testTenantA, Code: "code", State: state,
	})
	assertErrorCode(t, err, ErrSSODomainNotAllowed.Code)
}

// TestSSOService_Callback_StateIsScopedToItsOwnTenantsChannel proves that a
// state value issued for one tenant's identity provider cannot be redeemed
// against a different tenant's callback -- the channel name folds the tenant
// in for exactly this reason (ProviderOIDCPrefix + tenant id).
func TestSSOService_Callback_StateIsScopedToItsOwnTenantsChannel(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "enterprise-client")
	f := newSSOFixture(t, server)
	writeSSOConfig(t, f, testTenantA, server, "enterprise-client", "example.com")
	writeSSOConfig(t, f, testTenantB, server, "enterprise-client", "example.com")

	state, _ := ssoAuthorize(t, f, testTenantA)

	_, err := f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: testTenantB, Code: "code", State: state,
	})
	assertErrorCode(t, err, ErrOAuthStateInvalid.Code)
}

// TestSSOService_Callback_RefusesATokenItCannotTrust walks the ID token
// failure modes, mirroring the Google channel's own such test: a compliant
// relying party must refuse every one of these rather than fall back to
// trusting an unverifiable claim.
func TestSSOService_Callback_RefusesATokenItCannotTrust(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		arrange func(t *testing.T, server *testutil.OIDCServer, nonce string)
	}{
		{
			name: "the token endpoint refused the code",
			arrange: func(_ *testing.T, server *testutil.OIDCServer, _ string) {
				server.FailTokenExchange()
			},
		},
		{
			name: "no id token came back",
			arrange: func(_ *testing.T, server *testutil.OIDCServer, _ string) {
				server.OmitIDToken()
			},
		},
		{
			name: "the nonce does not match this flow",
			arrange: func(t *testing.T, server *testutil.OIDCServer, _ string) {
				server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
					Subject: "enterprise-subject-5", Nonce: "a-different-flows-nonce",
				}))
			},
		},
		{
			name: "the id token carries no subject",
			arrange: func(t *testing.T, server *testutil.OIDCServer, nonce string) {
				server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
					Nonce: nonce,
				}))
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := testutil.NewOIDCServer(t, "enterprise-client")
			f := newSSOFixture(t, server)
			writeSSOConfig(t, f, testTenantA, server, "enterprise-client", "example.com")

			state, nonce := ssoAuthorize(t, f, testTenantA)
			tc.arrange(t, server, nonce)

			_, err := f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
				TenantID: testTenantA, Code: "code", State: state,
			})
			assertErrorCode(t, err, ErrSSOTokenInvalid.Code)
		})
	}
}

// TestSSOService_Callback_JITProvisioningStillRequiresMembership documents
// the same fail-closed limitation the social channels have: an enterprise
// identity provider can assert a brand new person's verified address on an
// allowed domain, and this module WILL provision an account for them, but it
// still refuses to start a session until something makes them an active
// member of the tenant -- authn never grants tenant membership on its own.
func TestSSOService_Callback_JITProvisioningStillRequiresMembership(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "enterprise-client")
	f := newSSOFixture(t, server)
	writeSSOConfig(t, f, testTenantA, server, "enterprise-client", "example.com")

	state, nonce := ssoAuthorize(t, f, testTenantA)
	server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
		Subject: "enterprise-subject-6", Email: "brand-new@example.com", EmailVerified: true, Nonce: nonce,
	}))

	_, err := f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: testTenantA, Code: "code", State: state,
	})
	assertErrorCode(t, err, ErrTenantMembershipRequired.Code)

	identity, findErr := f.svc.Identities().FindByExternal(t.Context(), SSOChannelName(testTenantA), "enterprise-subject-6")
	if findErr != nil {
		t.Fatalf("the identity was not provisioned: %v", findErr)
	}
	created, userErr := f.svc.Users().FindByID(t.Context(), identity.UserID)
	if userErr != nil {
		t.Fatalf("the account was not provisioned: %v", userErr)
	}
	if !created.EmailVerified {
		t.Error("EmailVerified = false, want true: the tenant's own identity provider asserted it")
	}
}

// writeSSOConfig is putSSOConfig specialised to the common case: an enabled
// configuration pointing at server, with domains as the allowlist.
func writeSSOConfig(t *testing.T, f *serviceFixture, tenantID pkgcore.TenantID, server *testutil.OIDCServer, clientID string, domains ...string) *TenantSSOConfig {
	t.Helper()
	config := &TenantSSOConfig{
		TenantID:     string(tenantID),
		ID:           newID(),
		Issuer:       server.URL(),
		ClientID:     clientID,
		ClientSecret: "sso-client-secret",
		Enabled:      true,
	}
	config.SetAllowedDomains(domains)
	if err := f.svc.SSO().Configs().Create(pkgcore.WithTenant(t.Context(), tenantID), config); err != nil {
		t.Fatalf("write the tenant sso config: %v", err)
	}
	return config
}

// compile-time reminder that TenantSSOConfig stays tenant-scoped data; a
// change here would be caught by TestSSOConfigRepository_AssertIsolated too,
// but the type assertion makes the requirement readable without running it.
var _ dbkit.TenantScoped = TenantSSOConfig{}
