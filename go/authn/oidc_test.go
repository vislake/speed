package authn

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/audit"
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
	// The mint is email-less by design (resolveAccount's mint branch): a
	// tenant-grade verified claim must not seat the platform-unique email
	// index, or the tenant's administrator could capture the address's true
	// owner's later trusted-provider sign-ins. The claimed address lives on
	// the identity row as display data instead.
	if created.Email != "" || created.EmailVerified {
		t.Errorf("Email = %q, EmailVerified = %v; want an email-less mint", created.Email, created.EmailVerified)
	}
	if identity.Email != "brand-new@example.com" {
		t.Errorf("Identity.Email = %q, want the claimed address kept on the identity row", identity.Email)
	}
}

// TestSSOService_Callback_RefusesToProvisionFromAnUnverifiedEmail is the
// defect this round closes: the just-in-time branch used to mint an ACTIVE
// account carrying the IdP-claimed address -- seating the platform-unique
// email index -- even when the identity provider did not assert the address
// was verified. The mint is now refused with the same error the
// existing-account branch gives for the same fact, and nothing is
// provisioned: no identity, no account, no EventUserCreated, so the
// address's true owner keeps the seat free to register it themselves.
func TestSSOService_Callback_RefusesToProvisionFromAnUnverifiedEmail(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// claim is the email_verified value the token carries; nil omits the
		// claim entirely, which is how many enterprise identity providers
		// ship by default.
		claim any
	}{
		{name: "the token claims email_verified = false", claim: false},
		{name: "the token omits email_verified", claim: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			server := testutil.NewOIDCServer(t, "enterprise-client")
			f := newSSOFixture(t, server)
			writeSSOConfig(t, f, testTenantA, server, "enterprise-client", "example.com")

			state, nonce := ssoAuthorize(t, f, testTenantA)
			server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
				Subject: "enterprise-subject-unverified", Email: "fresh-seat@example.com",
				EmailVerified: tc.claim, Nonce: nonce,
			}))

			_, err := f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
				TenantID: testTenantA, Code: "code", State: state,
			})
			assertErrorCode(t, err, ErrIdentityRequiresBinding.Code)

			if _, findErr := f.svc.Identities().FindByExternal(t.Context(), SSOChannelName(testTenantA), "enterprise-subject-unverified"); !errors.Is(findErr, ErrNotFound) {
				t.Errorf("an identity was provisioned for the unverified subject (FindByExternal error = %v); want no provisioning at all", findErr)
			}
			if _, findErr := f.svc.Users().FindByEmail(t.Context(), "fresh-seat@example.com"); !errors.Is(findErr, ErrNotFound) {
				t.Errorf("an account was provisioned seating the unverified address (FindByEmail error = %v); want no account at all", findErr)
			}
			if n := f.events.Count(EventUserCreated); n != 0 {
				t.Errorf("recorded %d %s events, want 0: an unverified claim must not mint", n, EventUserCreated)
			}
		})
	}
}

// TestSSOService_Callback_JITProvisionedMemberSignsInOnTheNextAttempt is the
// mirror of the refusal above, the scenario that must keep working: a
// VERIFIED address claim on an allowed domain still mints the account and
// publishes EventUserCreated, and once the host's membership machinery has
// made the minted account an active member -- reacting to that event, the
// way the reference app's org module does -- a later attempt over the now
// bound identity completes the sign-in.
func TestSSOService_Callback_JITProvisionedMemberSignsInOnTheNextAttempt(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "enterprise-client")
	f := newSSOFixture(t, server)
	writeSSOConfig(t, f, testTenantA, server, "enterprise-client", "example.com")

	// First attempt: the verified claim mints the account, but authn grants
	// no membership on its own, so the session is refused.
	state, nonce := ssoAuthorize(t, f, testTenantA)
	server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
		Subject: "enterprise-subject-fresh-verified", Email: "fresh-verified@example.com",
		EmailVerified: true, Nonce: nonce,
	}))
	_, err := f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: testTenantA, Code: "code", State: state,
	})
	assertErrorCode(t, err, ErrTenantMembershipRequired.Code)

	identity, findErr := f.svc.Identities().FindByExternal(t.Context(), SSOChannelName(testTenantA), "enterprise-subject-fresh-verified")
	if findErr != nil {
		t.Fatalf("the identity was not provisioned: %v", findErr)
	}
	created, userErr := f.svc.Users().FindByID(t.Context(), identity.UserID)
	if userErr != nil {
		t.Fatalf("the account was not provisioned: %v", userErr)
	}
	// Email-less mint, exactly as TestSSOService_Callback_JITProvisioningStillRequiresMembership
	// now pins: the tenant-grade verified claim must not seat the
	// platform-unique email index. The sign-in below needs no email -- it
	// resolves over the bound identity.
	if created.Email != "" || created.EmailVerified {
		t.Errorf("Email = %q, EmailVerified = %v; want an email-less mint", created.Email, created.EmailVerified)
	}

	// The host's membership machinery, reacting to EventUserCreated, grants
	// the minted account membership of the tenant.
	f.members.Add(identity.UserID, testTenantA)

	// Second attempt: the bound subject signs in without any re-resolution.
	state, nonce = ssoAuthorize(t, f, testTenantA)
	server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
		Subject: "enterprise-subject-fresh-verified", Email: "fresh-verified@example.com",
		EmailVerified: true, Nonce: nonce,
	}))
	result, err := f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: testTenantA, Code: "code", State: state,
	})
	if err != nil {
		t.Fatalf("Callback() error = %v, want the sign-in to complete once the minted account is a member", err)
	}
	if result.User.ID != created.ID {
		t.Errorf("User.ID = %q, want the minted account %q", result.User.ID, created.ID)
	}
	if result.Tokens == nil {
		t.Fatal("Tokens = nil, want a session")
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

// TestSSOService_Callback_JITMintDoesNotCaptureTheAddressOwnersLaterTrustedSignIn
// is the P1-6 regression, reproducing the takeover chain end to end against
// this module's own scaffolding:
//
// Leg 1 -- the tenant administrator of tenant A, who configures the issuer
// and the allowed domains and may run the identity provider themselves (the
// premise the membership condition on resolveAccount's linking branch exists
// to bound), lists example.com and completes an enterprise callback asserting
// victim@example.com as verified. The address has no account yet, so the
// just-in-time branch mints one and binds the administrator's subject to it;
// the host's membership machinery (reacting to EventUserCreated, the
// documented reaction that makes enterprise JIT sign-in work) then grants
// the minted account membership of tenant A, and the administrator signs in.
//
// Leg 2 -- the address's TRUE owner later signs in through a channel the
// platform trusts (the Google-shaped verified channel, the same stand-in the
// module's own social round-trip suites use) at the same address. The
// verified-and-trusted auto-link rule resolves the owner's identity against
// the platform-unique email index; the minted account must not be there.
// Pre-fix the mint seated the tenant-grade claim as a platform-verified
// address, so the owner's genuine Google identity was auto-linked INTO the
// tenant administrator's account -- the account takeover. The fix keeps the
// seat free: the mint is email-less (the claimed address lives on the
// identity row as display data only), the owner's sign-in provisions their
// own account at the address, and the tenant-minted account never gains the
// owner's identity.
func TestSSOService_Callback_JITMintDoesNotCaptureTheAddressOwnersLaterTrustedSignIn(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "enterprise-client")
	google := &stubProvider{name: ProviderGoogle, identity: &ExternalIdentity{
		ExternalID: "victim-google-subject", Email: "victim@example.com", EmailVerified: true, Name: "Victim Person",
	}}
	allowlist, err := NewRedirectAllowlist(ssoRedirectURI, testRedirectURI)
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	f := newServiceFixture(t,
		WithFederationHTTPClient(server.Client()),
		WithRedirectAllowlist(allowlist),
		WithSocialProviders(google),
		WithTrustedProviders(ProviderGoogle),
	)
	writeSSOConfig(t, f, testTenantA, server, "enterprise-client", "example.com")

	// Leg 1, attempt 1: the tenant administrator's verified claim mints an
	// account for the subject, but authn grants no membership on its own.
	state, nonce := ssoAuthorize(t, f, testTenantA)
	server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
		Subject: "attacker-sso-subject", Email: "victim@example.com", EmailVerified: true, Nonce: nonce,
	}))
	_, err = f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: testTenantA, Code: "code", State: state,
	})
	assertErrorCode(t, err, ErrTenantMembershipRequired.Code)

	ssoIdentity, findErr := f.svc.Identities().FindByExternal(t.Context(), SSOChannelName(testTenantA), "attacker-sso-subject")
	if findErr != nil {
		t.Fatalf("the enterprise identity was not provisioned: %v", findErr)
	}
	minted, userErr := f.svc.Users().FindByID(t.Context(), ssoIdentity.UserID)
	if userErr != nil {
		t.Fatalf("the account was not provisioned: %v", userErr)
	}
	// The claimed address stays on the identity row as display data; the
	// users row must NOT seat it (the platform-unique email index).
	if ssoIdentity.Email != "victim@example.com" {
		t.Errorf("Identity.Email = %q, want the claimed address on the identity row", ssoIdentity.Email)
	}
	if minted.Email != "" || minted.EmailVerified {
		t.Errorf("minted account Email = %q, EmailVerified = %v; want an email-less mint so the address's true owner keeps the seat free", minted.Email, minted.EmailVerified)
	}

	// The host's membership machinery, reacting to EventUserCreated, grants
	// the minted account membership of the tenant, and the administrator's
	// next attempt completes the sign-in: the account is genuinely theirs.
	f.members.Add(minted.ID, testTenantA)
	state, nonce = ssoAuthorize(t, f, testTenantA)
	server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
		Subject: "attacker-sso-subject", Email: "victim@example.com", EmailVerified: true, Nonce: nonce,
	}))
	controlled, err := f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: testTenantA, Code: "code", State: state,
	})
	if err != nil {
		t.Fatalf("the tenant administrator's own sign-in failed: %v", err)
	}
	if controlled.User.ID != minted.ID || controlled.Tokens == nil {
		t.Fatalf("the administrator's sign-in did not land in the minted account (user %q, tokens nil = %v)", controlled.User.ID, controlled.Tokens == nil)
	}

	// Leg 2: the address's true owner signs in through the trusted Google
	// channel. The one thing that must be true afterwards: the owner's
	// identity and account are their own, never the tenant-minted account's.
	_, _ = socialSignIn(t, f, google, testTenantA)

	googleIdentity, findErr := f.svc.Identities().FindByExternal(t.Context(), ProviderGoogle, "victim-google-subject")
	if findErr != nil {
		t.Fatalf("the owner's google identity was not provisioned: %v", findErr)
	}
	if googleIdentity.UserID == minted.ID {
		t.Fatalf("account takeover: the address's true owner's trusted google identity was auto-linked INTO the tenant-minted account %q", minted.ID)
	}
	victimsAccount, findErr := f.svc.Users().FindByEmail(t.Context(), "victim@example.com")
	if findErr != nil {
		t.Fatalf("the address's true owner has no account at their own address: %v", findErr)
	}
	if victimsAccount.ID == minted.ID {
		t.Fatalf("account takeover: the address's true owner's sign-in resolved to the tenant-minted account %q", minted.ID)
	}
	if !victimsAccount.EmailVerified || victimsAccount.Email != "victim@example.com" {
		t.Errorf("the owner's own account Email = %q, EmailVerified = %v; want the address seated verified by the TRUSTED channel", victimsAccount.Email, victimsAccount.EmailVerified)
	}
	if googleIdentity.UserID != victimsAccount.ID {
		t.Errorf("the owner's google identity is bound to %q, want their own account %q", googleIdentity.UserID, victimsAccount.ID)
	}
}

// TestSSOService_Discover_DoesNotHoldTheMutexAcrossTheNetworkCall is the
// P3-11 regression: discover() used to hold the service-wide s.mu across the
// whole discovery round trip to the tenant-supplied issuer -- a fetch whose
// only bound is the HTTP client's own timeout. One tenant's black-holed
// issuer therefore queued every other tenant's discovery behind it (both
// AuthorizeURL and Callback call discover), so every other tenant's SSO
// sign-in stalled for as long as the slow issuer took to time out.
//
// The test reproduces the hazard deterministically: the slow issuer's
// discovery handler parks on the wire (testutil.OIDCServer's GateDiscovery)
// while a first discovery for it is in flight -- under the old code s.mu is
// held for that whole park -- and a second tenant's discovery of a healthy
// issuer must still complete while the slow one is parked. The blocking is a
// lock-ordering guarantee under the old code, not a timing race: discover of
// the healthy issuer simply cannot pass s.mu until the slow fetch releases
// it, which the test only does after the assertion.
func TestSSOService_Discover_DoesNotHoldTheMutexAcrossTheNetworkCall(t *testing.T) {
	slow := testutil.NewOIDCServer(t, "slow-client")
	fast := testutil.NewOIDCServer(t, "fast-client")

	entered := make(chan struct{})
	released := make(chan struct{})
	slow.GateDiscovery(entered, released)

	f := newSSOFixture(t, slow)
	svc := f.svc.SSO()

	// A first-time discovery of the slow issuer starts and parks on the
	// network round trip. The entered signal means the fetch is genuinely on
	// the wire -- and, under the old code, that s.mu is held.
	first := make(chan error, 1)
	go func() {
		_, err := svc.discover(t.Context(), slow.URL())
		first <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow issuer's discovery request never reached the server")
	}
	// The blocked request must ALWAYS be released, whatever the assertions
	// below say, or the parked handler keeps the server from shutting down
	// at the end of the test. The Once makes the release idempotent across
	// the early-return paths (t.Fatal above and in the select below) and
	// the explicit release before the final joins.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(released) }) }
	defer release()

	// Another tenant's discovery of a healthy issuer must complete while the
	// slow one is still parked.
	second := make(chan error, 1)
	go func() {
		_, err := svc.discover(t.Context(), fast.URL())
		second <- err
	}()

	secondDone := false
	select {
	case err := <-second:
		secondDone = true
		if err != nil {
			t.Fatalf("discover(healthy issuer) error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("discover(healthy issuer) did not complete while the slow issuer's discovery was in flight: the discovery fetch must not hold the service-wide mutex (P3-11)")
	}

	// Now the slow fetch may finish; join both discoveries so no goroutine
	// outlives the test.
	release()
	if err := <-first; err != nil {
		t.Errorf("discover(slow issuer) error = %v", err)
	}
	if !secondDone {
		if err := <-second; err != nil {
			t.Errorf("discover(healthy issuer, after release) error = %v", err)
		}
	}
}

// TestSSOService_Discover_ForgetDuringAnInFlightFetchIsNotUndone is the
// P2-18 regression: a forget() that runs while a discovery fetch is on the
// wire must win over that fetch's store-back. The double-checked re-check
// alone cannot see the difference between "never memoized" and "just
// forgotten" -- both leave the map empty -- so the store-back resurrects
// the document the forget evicted, and the eviction silently never takes
// effect. (The old comment claimed the forget won; the code proved
// otherwise.)
func TestSSOService_Discover_ForgetDuringAnInFlightFetchIsNotUndone(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "enterprise-client")
	f := newSSOFixture(t, server)
	svc := f.svc.SSO()
	ctx := pkgcore.WithTenant(t.Context(), testTenantA)
	issuer := server.URL()

	// entered is buffered so a discovery request issued AFTER the gate is
	// released (the post-forget refetch below) can signal and proceed
	// without needing a reader; the first fetch's signal is consumed by
	// the select below.
	entered := make(chan struct{}, 2)
	released := make(chan struct{})
	server.GateDiscovery(entered, released)
	// The parked request must ALWAYS be released, whatever the assertions
	// below say, or it keeps the server from shutting down at the end of
	// the test; the Once makes the release idempotent across the early
	// return paths.
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(released) }) }
	defer release()

	// A first-time discovery of the issuer starts and parks on the wire.
	first := make(chan error, 1)
	go func() {
		_, err := svc.discover(ctx, issuer)
		first <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the discovery request never reached the server")
	}

	// A configuration change evicts the issuer while the fetch is in
	// flight.
	svc.forget(issuer)

	// The in-flight fetch may now finish.
	release()
	if err := <-first; err != nil {
		t.Fatalf("discover() (in flight during the forget) error = %v", err)
	}

	// The next discovery of the same issuer must fetch again: had the
	// in-flight fetch stored its document after the forget ran, this call
	// would be a memo hit and the server would never hear from it.
	if _, err := svc.discover(ctx, issuer); err != nil {
		t.Fatalf("discover() after the forget error = %v", err)
	}
	if got := server.DiscoveryRequests(); got != 2 {
		t.Errorf("discovery requests across the forget = %d, want 2: the in-flight fetch stored the document the forget evicted (P2-18)", got)
	}
}

// TestSSOService_SaveConfig_IssuerChangeEvictsTheOldIssuerFromTheMemo is the
// P2-19 regression: SaveConfig's forget must target the issuer the row held
// BEFORE the update. An A-to-B issuer change whose forget evicts the NEW
// issuer (a no-op, never memoized) leaves A's discovery document memoized
// forever -- silently reused if the tenant ever points back at A, stale
// however long the provider was away.
func TestSSOService_SaveConfig_IssuerChangeEvictsTheOldIssuerFromTheMemo(t *testing.T) {
	t.Parallel()

	serverA := testutil.NewOIDCServer(t, "issuer-a-client")
	f := newSSOFixture(t, serverA)
	svc := f.svc.SSO()
	ctx := pkgcore.WithTenant(t.Context(), testTenantA)
	issuerA := serverA.URL()

	writeSSOConfig(t, f, testTenantA, serverA, "issuer-a-client", "example.com")

	// Discovery memoizes issuer A.
	if _, err := svc.discover(ctx, issuerA); err != nil {
		t.Fatalf("discover(issuer A) error = %v", err)
	}
	if got := serverA.DiscoveryRequests(); got != 1 {
		t.Fatalf("discovery requests = %d, want 1", got)
	}

	// The tenant switches providers, A -> B. B is a public literal address
	// the SSRF guard admits without DNS; this test never fetches it.
	if _, err := svc.SaveConfig(ctx, SSOConfigInput{
		Issuer: "https://93.184.216.34/oidc", ClientID: "issuer-b-client", Enabled: true,
	}); err != nil {
		t.Fatalf("SaveConfig(A -> B) error = %v", err)
	}

	// A discovery of the OLD issuer must now be a real fetch, not a memo
	// hit: before the fix, A's document survived the save (the forget
	// evicted the never-memoized B), and this call would have been
	// answered from the memo with no request reaching the server.
	if _, err := svc.discover(ctx, issuerA); err != nil {
		t.Fatalf("discover(issuer A) after the issuer change error = %v", err)
	}
	if got := serverA.DiscoveryRequests(); got != 2 {
		t.Errorf("discovery requests for the old issuer after the change = %d, want 2: the old issuer's document was not evicted (P2-19)", got)
	}
}

// compile-time reminder that TenantSSOConfig stays tenant-scoped data; a
// change here would be caught by TestSSOConfigRepository_AssertIsolated too,
// but the type assertion makes the requirement readable without running it.
var _ dbkit.TenantScoped = TenantSSOConfig{}

// TestSSOService_Callback_BoundsOverWidthClaims is the enterprise-SSO twin
// of the social channels' provider-field truncation regressions
// (identity_test.go's TestService_SocialSignIn_BoundsAnOverWidthProviderProfile
// and its grown-profile companion): the subject, name and picture an identity
// provider puts in its ID token are third-party strings written into the same
// fixed-width user_identities columns, so an over-width first token must not
// succeed on SQLite and fail on PostgreSQL (SQLSTATE 22001) at first bind --
// and a later token whose profile has GROWN must not break the binding it
// refreshes.
func TestSSOService_Callback_BoundsOverWidthClaims(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "enterprise-client")
	f := newSSOFixture(t, server)
	writeSSOConfig(t, f, testTenantA, server, "enterprise-client", "example.com")
	member := f.registerUser(t, "member@example.com", testTenantA)

	overWidthSubject := strings.Repeat("s", identityExternalIDWidth) + strings.Repeat("t", 59)
	overWidthName := strings.Repeat("名", identityDisplayNameWidth) + strings.Repeat("尾", 72)
	overWidthPicture := strings.Repeat("p", identityAvatarURLWidth) + strings.Repeat("q", 98)

	state, nonce := ssoAuthorize(t, f, testTenantA)
	server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
		Subject:       overWidthSubject,
		Email:         "member@example.com",
		EmailVerified: true,
		Name:          overWidthName,
		Picture:       overWidthPicture,
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
	if result.Tokens == nil {
		t.Fatal("Tokens = nil, want a session")
	}

	identity, err := f.svc.Identities().FindByExternal(t.Context(), SSOChannelName(testTenantA), overWidthSubject)
	if err != nil {
		t.Fatalf("FindByExternal(original over-width subject) error = %v", err)
	}
	if identity.ID != result.Identity.ID {
		t.Errorf("identity row = %q, want the sign-in's %q", identity.ID, result.Identity.ID)
	}
	if want := strings.Repeat("s", identityExternalIDWidth); identity.ExternalID != want {
		t.Errorf("stored external_id = %q-ish, want the %d-rune head of the subject", identity.ExternalID, identityExternalIDWidth)
	}
	if want := strings.Repeat("名", identityDisplayNameWidth); identity.DisplayName != want {
		t.Errorf("stored display_name = %q-ish, want the %d-rune head of the claimed name", identity.DisplayName, identityDisplayNameWidth)
	}
	if want := strings.Repeat("p", identityAvatarURLWidth); identity.AvatarURL != want {
		t.Errorf("stored avatar_url = %q-ish, want the %d-rune head of the claimed picture", identity.AvatarURL, identityAvatarURLWidth)
	}

	// A second callback whose token reports a grown name and picture still
	// signs the member in: the refresh writes bounded values (TouchLogin),
	// never a 22001 on PostgreSQL, and resolves to the same identity.
	state2, nonce2 := ssoAuthorize(t, f, testTenantA)
	server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
		Subject:       overWidthSubject,
		Email:         "member@example.com",
		EmailVerified: true,
		Name:          overWidthName,
		Picture:       overWidthPicture,
		Nonce:         nonce2,
	}))
	result2, err := f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: testTenantA, Code: "the-code-2", State: state2,
	})
	if err != nil {
		t.Fatalf("second Callback() error = %v", err)
	}
	if result2.Identity.ID != result.Identity.ID {
		t.Errorf("second sign-in resolved to identity %q, want the first sign-in's %q", result2.Identity.ID, result.Identity.ID)
	}
	if result2.Tokens == nil {
		t.Fatal("second sign-in returned no session")
	}
}

// The width-regression fixtures below (and the regressions themselves) close
// the two authn batch-2 findings that are the truncation round's siblings:
// the same dual-dialect write divergence, on the two remaining surfaces
// where the semantic is REFUSE, not cut.
//
//   - The tenant-id budget: user_identities.provider is VARCHAR(64)
//     (migration 0005) and the enterprise channel name is "oidc:" plus the
//     tenant id (ProviderOIDCPrefix, five runes), so a tenant id of 59
//     runes is the longest enterprise SSO can serve and 60 runes is the
//     shortest that overflows -- pinned by oidc.go's ssoTenantIDMaxWidth.
//   - The configuration field widths are tenant_sso_configs' own columns
//     (migration 0006): issuer VARCHAR(512), client_id VARCHAR(255),
//     allowed_domains VARCHAR(1024), pinned by oidc.go's ssoIssuerWidth,
//     ssoClientIDWidth and ssoAllowedDomainsWidth.
//
// The fixtures are plain rune counts rather than expressions over those
// constants, and the expected error codes are string literals rather than
// the sentinels, so the regressions compile and run unchanged against the
// pre-fix code during a genuine fail-before pass (the constants and the
// sentinels only exist after the fix). PostgreSQL counts characters against
// a VARCHAR(n) width, so every fixture is built in runes.
const (
	// ssoTenantBudgetRunes is the longest tenant id the enterprise channel
	// can serve (see the comment above).
	ssoTenantBudgetRunes = 59
	// ssoIssuerColumnWidth, ssoClientIDColumnWidth and
	// ssoAllowedDomainsColumnWidth restate the tenant_sso_configs column
	// widths from migration 0006.
	ssoIssuerColumnWidth         = 512
	ssoClientIDColumnWidth       = 255
	ssoAllowedDomainsColumnWidth = 1024
)

var (
	// overLongSSOTenantID is a tenant id one rune past the budget.
	overLongSSOTenantID = pkgcore.TenantID(strings.Repeat("t", ssoTenantBudgetRunes+1))
	// boundarySSOTenantID is a tenant id exactly at the budget -- the
	// longest one the enterprise channel can represent.
	boundarySSOTenantID = pkgcore.TenantID(strings.Repeat("t", ssoTenantBudgetRunes))
)

// ssoPublicIssuerLiteral is a publicly reachable literal https address the
// SSRF guard admits without DNS, so the SaveConfig regressions below never
// touch the network; the same literal the issuer-change memo test uses.
const ssoPublicIssuerLiteral = "https://93.184.216.34/"

var (
	// overWidthIssuer is a valid https URL one rune past issuer's width.
	overWidthIssuer = ssoPublicIssuerLiteral + strings.Repeat("i", ssoIssuerColumnWidth+1-len(ssoPublicIssuerLiteral))
	// boundaryIssuer is a valid https URL exactly at issuer's width.
	boundaryIssuer = ssoPublicIssuerLiteral + strings.Repeat("i", ssoIssuerColumnWidth-len(ssoPublicIssuerLiteral))
	// overWidthClientID is one rune past client_id's width.
	overWidthClientID = strings.Repeat("c", ssoClientIDColumnWidth+1)
	// boundaryClientID is exactly at client_id's width.
	boundaryClientID = strings.Repeat("c", ssoClientIDColumnWidth)
	// overWidthAllowedDomains joins to one rune past allowed_domains'
	// width.
	overWidthAllowedDomains = []string{strings.Repeat("d", ssoAllowedDomainsColumnWidth+1)}
	// boundaryAllowedDomains joins to exactly allowed_domains' width.
	boundaryAllowedDomains = []string{strings.Repeat("d", ssoAllowedDomainsColumnWidth)}
)

// The expected codes for the width refusals, as literals (see the fixtures
// comment above).
const (
	ssoTenantIDTooLongCode       = "authn.sso_tenant_id_too_long"
	ssoIssuerTooLongCode         = "authn.sso_issuer_too_long"
	ssoClientIDTooLongCode       = "authn.sso_client_id_too_long"
	ssoAllowedDomainsTooLongCode = "authn.sso_allowed_domains_too_long"
)

// TestSSOService_SaveConfig_RefusesAnOverLongTenantID is finding (1)'s
// configuration-time regression: a tenant id of 60 runes makes the synthetic
// "oidc:<tenant>" provider name overflow user_identities.provider
// (VARCHAR(64)) at the identity write of the first sign-in -- before the
// fix, SaveConfig accepted and stored the configuration on SQLite, and the
// tenant's first enterprise login then diverged: it worked on SQLite and was
// refused by PostgreSQL with SQLSTATE 22001. Truncating the provider name is
// not an option (it would collide under the (provider, external_id) unique
// index and silently merge distinct tenants' identities), so the refusal
// belongs at configuration time, where the host learns which tenant name is
// too long -- never at a random login. The same named refusal must answer on
// both dialects; the PostgreSQL leg re-runs this in the integration tier.
func TestSSOService_SaveConfig_RefusesAnOverLongTenantID(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := pkgcore.WithTenant(t.Context(), overLongSSOTenantID)

	_, err := f.svc.SSO().SaveConfig(ctx, SSOConfigInput{
		Issuer: "https://93.184.216.34/oidc", ClientID: "client-id", Enabled: true,
	})
	assertErrorCode(t, err, ssoTenantIDTooLongCode)

	if _, findErr := f.svc.SSO().Configs().Current(ctx); findErr == nil {
		t.Error("a refused configuration must not have been persisted")
	}
}

// TestSSOService_SaveConfig_RefusesOverWidthConfigFields is finding (2)'s
// regression: a tenant administrator's configuration value longer than its
// column (issuer VARCHAR(512), client_id VARCHAR(255), allowed_domains
// VARCHAR(1024), migration 0006) is REFUSED with an error naming the field,
// on both dialects -- before the fix, SQLite stored the value and PostgreSQL
// refused the write with a raw 22001. Truncation is not the answer for an
// administrator's own specification: a silently shortened issuer URL would
// point enterprise single sign-on at the wrong endpoint.
func TestSSOService_SaveConfig_RefusesOverWidthConfigFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		wantCode string
		in       SSOConfigInput
	}{
		{
			name:     "issuer one rune past 512",
			wantCode: ssoIssuerTooLongCode,
			in:       SSOConfigInput{Issuer: overWidthIssuer, ClientID: "client-id", Enabled: true},
		},
		{
			name:     "client id one rune past 255",
			wantCode: ssoClientIDTooLongCode,
			in: SSOConfigInput{
				Issuer: "https://93.184.216.34/oidc", ClientID: overWidthClientID, Enabled: true,
			},
		},
		{
			name:     "allowed domains one rune past 1024",
			wantCode: ssoAllowedDomainsTooLongCode,
			in: SSOConfigInput{
				Issuer: "https://93.184.216.34/oidc", ClientID: "client-id",
				AllowedDomains: overWidthAllowedDomains, Enabled: true,
			},
		},
	}

	f := newServiceFixture(t)
	ctx := pkgcore.WithTenant(t.Context(), testTenantA)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.SSO().SaveConfig(ctx, tc.in)
			assertErrorCode(t, err, tc.wantCode)
			if _, findErr := f.svc.SSO().Configs().Current(ctx); findErr == nil {
				t.Error("a refused configuration must not have been persisted")
			}
		})
	}
}

// TestSSOService_SaveConfig_AcceptsValuesAtTheColumnWidths is the honest
// acceptance boundary of finding (2): values exactly AT their columns'
// widths (512-rune issuer, 255-rune client id, a 1024-rune domain list)
// still save and read back end to end.
func TestSSOService_SaveConfig_AcceptsValuesAtTheColumnWidths(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	ctx := pkgcore.WithTenant(t.Context(), testTenantA)

	if _, err := f.svc.SSO().SaveConfig(ctx, SSOConfigInput{
		Issuer: boundaryIssuer, ClientID: boundaryClientID,
		AllowedDomains: boundaryAllowedDomains, Enabled: true,
	}); err != nil {
		t.Fatalf("SaveConfig(boundary-width values) error = %v", err)
	}

	stored, err := f.svc.SSO().Configs().Current(ctx)
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if stored.Issuer != boundaryIssuer {
		t.Errorf("stored issuer is %d runes, want the %d-rune value", len([]rune(stored.Issuer)), ssoIssuerColumnWidth)
	}
	if len([]rune(stored.ClientID)) != ssoClientIDColumnWidth {
		t.Errorf("stored client id is %d runes, want %d", len([]rune(stored.ClientID)), ssoClientIDColumnWidth)
	}
	if len([]rune(stored.AllowedDomains)) != ssoAllowedDomainsColumnWidth {
		t.Errorf("stored allowed domains are %d runes, want %d", len([]rune(stored.AllowedDomains)), ssoAllowedDomainsColumnWidth)
	}
}

// TestSSOService_SaveConfig_RecordsTheWriteAsAuditActionSSOConfigure is the
// consumer-shaped proof that AuditActionSSOConfigure is genuinely emitted:
// SaveConfig has no in-repo callers, so this test is its first real one,
// driving the write through the same construction a host uses -- a Module
// registered on a real pkgcore.Registry, exactly module.go's Register runs
// in production -- and asserting the rows land on the shared bus under the
// declared action.
//
// The operator's identity travels the way every audit carrier in this
// module travels: a pkgcore.Actor (and pkgcore.OnBehalfOf, when an
// impersonation flow installs one) on ctx, the carriers audit.Emit itself
// reads back -- handler.go's recordAudit layers the same shape. The tenant
// is the ctx tenant SaveConfig itself validated. Before the fix this round
// ships, the action was declared on the registry but no code emitted it
// anywhere: the round that wired the other eight concluded SaveConfig had
// "no site" because no HTTP handler exists for it, missing that the service
// layer is where this module's own write happens and where the audit calls
// of every other module's services legitimately live. The pre-fix code
// therefore fails this test with "recorded 0 authn.sso.configure rows
// across the create and the update, want 2".
func TestSSOService_SaveConfig_RecordsTheWriteAsAuditActionSSOConfigure(t *testing.T) {
	t.Parallel()

	module := newTestModule(t)
	bus := pkgcore.NewMemoryEventBus()
	recorder := testutil.NewEventRecorder()
	recorder.Subscribe(bus, audit.EventRecorded)
	reg := pkgcore.NewRegistry(bus, pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	if err := module.Register(reg); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	svc := module.Service()

	// The writing operator: a real account, attested on ctx exactly as the
	// audit carriers demand. SaveConfig itself performs no authorization --
	// who may write a tenant's SSO configuration is the caller's gate (a
	// future HTTP surface behind PermissionSSOManage) -- so the recorded
	// attribution is precisely the identity the caller vouched for.
	user, err := svc.Register(t.Context(), RegisterInput{
		Email: "sso-operator@example.com", Password: testPassword, DisplayName: "SSO Operator",
	})
	if err != nil {
		t.Fatalf("Register(operator) error = %v", err)
	}
	actor := pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: user.ID, DisplayName: user.DisplayName}
	ctx := pkgcore.WithTenant(pkgcore.WithActor(t.Context(), actor), testTenantA)

	const firstIssuer = "https://93.184.216.34/oidc"
	const secondIssuer = "https://93.184.216.34/idp2"

	config, err := svc.SSO().SaveConfig(ctx, SSOConfigInput{
		Issuer: firstIssuer, ClientID: "client-id",
		ClientSecret: "the-client-secret", Enabled: true,
		AllowedDomains: []string{"example.com"},
	})
	if err != nil {
		t.Fatalf("SaveConfig(create) error = %v", err)
	}

	// The same tenant written again goes down the update branch: the row is
	// the existing one, now disabled and repointed at the second issuer.
	// Turning enterprise single sign-on OFF is an authentication-boundary
	// change too, and the second record must say so.
	if _, err := svc.SSO().SaveConfig(ctx, SSOConfigInput{
		Issuer: secondIssuer, ClientID: "client-id", Enabled: false,
	}); err != nil {
		t.Fatalf("SaveConfig(update) error = %v", err)
	}

	var rows []audit.RecordedEvent
	for _, evt := range recorder.Events() {
		if evt.Type != audit.EventRecorded {
			continue
		}
		recorded, ok := evt.Payload.(audit.RecordedEvent)
		if !ok {
			t.Fatalf("audit.EventRecorded payload has type %T, want audit.RecordedEvent", evt.Payload)
		}
		if recorded.Action == AuditActionSSOConfigure {
			rows = append(rows, recorded)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("recorded %d %s rows across the create and the update, want 2; all events: %+v",
			len(rows), AuditActionSSOConfigure, recorder.Events())
	}

	// Both rows carry the writing operator and the tenant the write
	// happened in, and name the same configuration row.
	for i, row := range rows {
		if row.Actor != actor {
			t.Errorf("row %d actor = %+v, want the writing operator %+v", i, row.Actor, actor)
		}
		auditTenantIs(t, row, testTenantA)
		if row.Resource.Type != "sso_config" || row.Resource.ID != config.ID {
			t.Errorf("row %d resource = %+v, want type %q id %q", i, row.Resource, "sso_config", config.ID)
		}
		if !row.Result.Success {
			t.Errorf("row %d result = %+v, want success", i, row.Result)
		}
	}

	// The create row names the configuration as first written, the update
	// row names it as rewritten -- and neither row ever carries the client
	// secret, whose plaintext must not enter the permanent trail.
	first := rows[0].Changes.After
	if first["issuer"] != firstIssuer || first["client_id"] != "client-id" ||
		first["enabled"] != true || first["allowed_domains"] != "example.com" {
		t.Errorf("create row changes = %v, want issuer %q client_id %q enabled true allowed_domains %q",
			first, firstIssuer, "client-id", "example.com")
	}
	second := rows[1].Changes.After
	if second["issuer"] != secondIssuer || second["enabled"] != false {
		t.Errorf("update row changes = %v, want issuer %q and enabled false", second, secondIssuer)
	}
	for i, after := range []map[string]any{first, second} {
		if _, leaked := after["client_secret"]; leaked {
			t.Errorf("row %d changes carry a client_secret key; the secret never enters the trail", i)
		}
		for key, value := range after {
			if text, ok := value.(string); ok && strings.Contains(text, "the-client-secret") {
				t.Errorf("row %d changes leak the client secret under key %q", i, key)
			}
		}
	}
}

// TestSSOConfigRepository_RefusesOverWidthConfigRows pins the repository as
// the backstop write surface: a direct Create or Update carrying an
// over-width value meets the same named refusal SaveConfig answers, so no
// write path can land a value SQLite would store and PostgreSQL would refuse.
func TestSSOConfigRepository_RefusesOverWidthConfigRows(t *testing.T) {
	t.Parallel()

	db := testutil.NewDB(t)
	repo := NewSSOConfigRepository(db)
	tenantID := pkgcore.TenantID("tenant-widths-backstop")
	ctx := pkgcore.WithTenant(t.Context(), tenantID)

	valid := &TenantSSOConfig{
		TenantID: string(tenantID), ID: newID(),
		Issuer: "https://idp.example.com", ClientID: "client-id", Enabled: true,
	}
	valid.SetAllowedDomains([]string{"example.com"})
	if err := repo.Create(ctx, valid); err != nil {
		t.Fatalf("create a within-width configuration: %v", err)
	}

	// An over-width Update is refused and leaves the stored row unchanged.
	valid.Issuer = overWidthIssuer
	assertErrorCode(t, repo.Update(ctx, valid), ssoIssuerTooLongCode)
	stored, err := repo.Current(ctx)
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if stored.Issuer != "https://idp.example.com" {
		t.Errorf("a refused update changed the stored issuer to %q", stored.Issuer)
	}

	// A direct over-width Create is refused and persists nothing.
	over := &TenantSSOConfig{
		TenantID: string(tenantID), ID: newID(),
		Issuer: overWidthIssuer, ClientID: "client-id", Enabled: true,
	}
	assertErrorCode(t, repo.Create(ctx, over), ssoIssuerTooLongCode)
}

// TestSSOService_AuthorizeURL_RefusesAnOverLongTenantID is finding (1)'s
// entry-time regression: a configuration row for the over-long tenant can
// exist (it was written before the SaveConfig gate, or straight through the
// repository), so the entry path refuses the tenant itself before any state
// is issued -- a state issued for such a tenant would only lead the member
// to a callback whose identity write cannot succeed on PostgreSQL.
func TestSSOService_AuthorizeURL_RefusesAnOverLongTenantID(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "enterprise-client")
	f := newSSOFixture(t, server)
	writeSSOConfig(t, f, overLongSSOTenantID, server, "enterprise-client", "example.com")

	_, err := f.svc.SSO().AuthorizeURL(
		pkgcore.WithTenant(t.Context(), overLongSSOTenantID), ssoRedirectURI, "")
	assertErrorCode(t, err, ssoTenantIDTooLongCode)
}

// TestSSOService_Callback_RefusesAnOverLongTenantID is finding (1)'s
// last-line regression on the callback path: an over-long tenant id is
// refused with the named error before anything else, so a flow begun before
// the gates existed answers the same refusal instead of a raw 22001 from
// the identity write it would otherwise reach.
func TestSSOService_Callback_RefusesAnOverLongTenantID(t *testing.T) {
	t.Parallel()

	f := newServiceFixture(t)
	_, err := f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: overLongSSOTenantID, Code: "code", State: "state",
	})
	assertErrorCode(t, err, ssoTenantIDTooLongCode)
}

// TestSSOService_ServesTheLongestRepresentableTenantID is finding (1)'s
// honest acceptance boundary, end to end: a tenant id of exactly 59 runes --
// whose "oidc:<tenant>" provider name is exactly 64 runes, the width of
// user_identities.provider -- completes the full enterprise sign-in round
// trip, resolving to an identity stored under SSOChannelName(boundaryTenant).
func TestSSOService_ServesTheLongestRepresentableTenantID(t *testing.T) {
	t.Parallel()

	server := testutil.NewOIDCServer(t, "enterprise-client")
	f := newSSOFixture(t, server)
	writeSSOConfig(t, f, boundarySSOTenantID, server, "enterprise-client", "example.com")
	member := f.registerUser(t, "member@example.com", boundarySSOTenantID)

	state, nonce := ssoAuthorize(t, f, boundarySSOTenantID)
	server.QueueIDToken(server.SignIDToken(t, testutil.IDTokenClaims{
		Subject: "enterprise-subject-boundary", Email: "member@example.com",
		EmailVerified: true, Nonce: nonce,
	}))

	result, err := f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: boundarySSOTenantID, Code: "the-code", State: state,
	})
	if err != nil {
		t.Fatalf("Callback() error = %v", err)
	}
	if result.User.ID != member.ID {
		t.Errorf("User.ID = %q, want the existing tenant member %q", result.User.ID, member.ID)
	}
	if result.Tokens == nil {
		t.Fatal("Tokens = nil, want a session")
	}
	if result.Identity.Provider != SSOChannelName(boundarySSOTenantID) {
		t.Errorf("Identity.Provider = %q, want %q", result.Identity.Provider, SSOChannelName(boundarySSOTenantID))
	}
}
