package authn

import (
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// testRedirectURI is the one redirect URI every fixture in this file
// registers, so a test only has to say which provider and identity it wants.
const testRedirectURI = "https://app.example.com/callback"

// newFederationFixture assembles a Service wired with provider as its one
// social channel, a redirect allowlist covering testRedirectURI, and
// whichever channels trusted names as eligible for automatic account
// linking.
func newFederationFixture(t *testing.T, provider SocialProvider, trusted ...string) *serviceFixture {
	t.Helper()
	allowlist, err := NewRedirectAllowlist(testRedirectURI)
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	return newServiceFixture(t,
		WithSocialProviders(provider),
		WithRedirectAllowlist(allowlist),
		WithTrustedProviders(trusted...),
	)
}

// socialSignIn drives a full authorize-then-callback round trip against
// provider's channel, the same two-step shape a browser follows. Exercising
// SocialAuthorizeURL rather than fabricating a state value directly is what
// proves the state store's issue/consume pair actually gates the callback.
func socialSignIn(t *testing.T, f *serviceFixture, provider SocialProvider, tenantID pkgcore.TenantID) (*SocialLoginResult, error) {
	t.Helper()
	authorizeURL, err := f.svc.SocialAuthorizeURL(t.Context(), SocialAuthorizeInput{
		Provider:    provider.Name(),
		RedirectURI: testRedirectURI,
	})
	if err != nil {
		t.Fatalf("SocialAuthorizeURL() error = %v", err)
	}
	state := parseQuery(t, authorizeURL).Get("state")
	if state == "" {
		t.Fatal("SocialAuthorizeURL() produced no state parameter")
	}
	return f.svc.SocialCallback(t.Context(), SocialCallbackInput{
		Provider: provider.Name(),
		Code:     "test-code",
		State:    state,
		TenantID: tenantID,
	})
}

// TestService_SocialSignIn_AutoLinkRules is the round's most important
// security test: docs/internal/05's rule that an existing account may be
// linked to a new external identity automatically ONLY when the provider
// asserts the address is verified AND the channel is on the platform's
// trusted list. Every other combination must refuse rather than sign the
// caller into somebody else's account.
func TestService_SocialSignIn_AutoLinkRules(t *testing.T) {
	t.Parallel()

	const sharedEmail = "shared@example.com"

	cases := []struct {
		name          string
		emailVerified bool
		trusted       bool
		wantLinked    bool
		wantErr       string
	}{
		{name: "verified and trusted links the existing account", emailVerified: true, trusted: true, wantLinked: true},
		{name: "verified but untrusted is refused", emailVerified: true, trusted: false, wantErr: ErrIdentityRequiresBinding.Code},
		{name: "trusted but unverified is refused", emailVerified: false, trusted: true, wantErr: ErrIdentityRequiresBinding.Code},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var trustedList []string
			if tc.trusted {
				trustedList = []string{ProviderGoogle}
			}
			provider := &stubProvider{name: ProviderGoogle, identity: &ExternalIdentity{
				ExternalID:    "external-1",
				Email:         sharedEmail,
				EmailVerified: tc.emailVerified,
				Name:          "External Person",
			}}
			f := newFederationFixture(t, provider, trustedList...)
			existing := f.registerUser(t, sharedEmail, testTenantA)

			result, err := socialSignIn(t, f, provider, testTenantA)

			if tc.wantErr != "" {
				assertErrorCode(t, err, tc.wantErr)
				if result != nil {
					t.Errorf("SocialCallback() returned a result alongside an error: %+v", result)
				}
				if f.events.Count(EventIdentityBound) != 0 {
					t.Error("a refused auto-link must not publish authn.identity.bound")
				}
				return
			}
			if err != nil {
				t.Fatalf("SocialCallback() error = %v", err)
			}
			if !result.AutoLinked {
				t.Error("AutoLinked = false, want true for a verified, trusted match")
			}
			if result.Created {
				t.Error("Created = true, want the EXISTING account to be reused")
			}
			if result.User.ID != existing.ID {
				t.Errorf("User.ID = %q, want the existing account %q", result.User.ID, existing.ID)
			}
			if result.Tokens == nil {
				t.Fatal("Tokens = nil, want a session for a successful link")
			}
			if f.events.Count(EventIdentityBound) != 1 {
				t.Errorf("authn.identity.bound published %d times, want 1", f.events.Count(EventIdentityBound))
			}
		})
	}
}

// TestService_SocialSignIn_NoEmailNeverAutoLinks covers the WeChat shape: a
// channel that reports no email address at all. resolveSocialAccount cannot
// look an existing account up by an address it was never given, so the only
// safe behaviour is to provision a brand new account rather than guessing --
// which is this rule's own "refuse to link" for a channel that has nothing to
// link on.
func TestService_SocialSignIn_NoEmailNeverAutoLinks(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{name: ProviderWeChat, identity: &ExternalIdentity{
		ExternalID:    "wechat-unionid-1",
		Email:         "",
		EmailVerified: false,
		Name:          "WeChat Person",
	}}
	// WeChat is even on the trusted list here, which must not matter: with
	// no email at all there is nothing for "trusted" to act on.
	f := newFederationFixture(t, provider, ProviderWeChat)
	existing := f.registerUser(t, "shared@example.com", testTenantA)

	// The new account has no membership of testTenantA yet (membership is
	// the org module's concern, not this one -- see AGENTS.md "Fail closed
	// on membership"), so the sign-in itself is refused. That refusal must
	// not stop the identity and its account from having been provisioned:
	// this test is about who they resolved to, not about the session.
	if _, err := socialSignIn(t, f, provider, testTenantA); err == nil {
		t.Fatal("socialSignIn() unexpectedly succeeded; a fresh account has no membership yet")
	}

	identity, err := f.svc.Identities().FindByExternal(t.Context(), ProviderWeChat, "wechat-unionid-1")
	if err != nil {
		t.Fatalf("the identity was not provisioned: %v", err)
	}
	if identity.UserID == existing.ID {
		t.Fatal("the new sign-in resolved to the pre-existing account; an email-less identity must never do that")
	}
	created, err := f.svc.Users().FindByID(t.Context(), identity.UserID)
	if err != nil {
		t.Fatalf("the account was not provisioned: %v", err)
	}
	if created.Email != "" {
		t.Errorf("User.Email = %q, want empty for an identity that reported none", created.Email)
	}
}

// TestService_SocialSignIn_UnmatchedEmailCreatesAVerifiedAccount is the
// companion happy path to the auto-link cases: a verified, trusted identity
// whose address matches NOBODY yet. There is nothing to link to, so a new
// account is provisioned, and because the assertion is trusted, the address
// is stored pre-verified rather than requiring a separate verification step.
func TestService_SocialSignIn_UnmatchedEmailCreatesAVerifiedAccount(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{name: ProviderGoogle, identity: &ExternalIdentity{
		ExternalID:    "external-2",
		Email:         "brand-new@example.com",
		EmailVerified: true,
	}}
	f := newFederationFixture(t, provider, ProviderGoogle)

	result, err := socialSignIn(t, f, provider, testTenantA)
	if err == nil {
		t.Fatalf("expected ErrTenantMembershipRequired for a fresh account with no membership yet, got a result: %+v", result)
	}
	assertErrorCode(t, err, ErrTenantMembershipRequired.Code)

	// The account itself must still have been provisioned and marked
	// verified, even though the session could not be started: membership
	// is a separate concern this module intentionally never grants on its
	// own (see AGENTS.md "Fail closed on membership").
	created, findErr := f.svc.Users().FindByEmail(t.Context(), "brand-new@example.com")
	if findErr != nil {
		t.Fatalf("the account was not provisioned: %v", findErr)
	}
	if !created.EmailVerified {
		t.Error("EmailVerified = false, want true: the trusted provider's assertion is strong enough to skip a separate verification step")
	}
}

// TestService_SocialCallback_UnknownProvider proves an unregistered channel
// name is refused before anything else runs.
func TestService_SocialCallback_UnknownProvider(t *testing.T) {
	t.Parallel()

	f := newFederationFixture(t, &stubProvider{name: ProviderGoogle})
	_, err := f.svc.SocialCallback(t.Context(), SocialCallbackInput{
		Provider: ProviderGitHub,
		Code:     "code",
		State:    "irrelevant",
	})
	assertErrorCode(t, err, ErrProviderUnknown.Code)
}

// TestService_SocialAuthorizeURL_RefusesAnUnlistedRedirect proves the
// redirect allowlist is enforced before a state value is even issued.
func TestService_SocialAuthorizeURL_RefusesAnUnlistedRedirect(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{name: ProviderGoogle}
	f := newFederationFixture(t, provider)
	_, err := f.svc.SocialAuthorizeURL(t.Context(), SocialAuthorizeInput{
		Provider:    ProviderGoogle,
		RedirectURI: "https://attacker.example.com/callback",
	})
	assertErrorCode(t, err, ErrRedirectURINotAllowed.Code)
}

// TestService_SocialCallback_OverridesAMisreportedProviderName proves that
// whatever channel name a SocialProvider implementation puts on the identity
// it returns is discarded in favour of the channel the caller actually asked
// for. A provider that got this wrong -- by bug or by malice -- must not be
// able to bind an identity under a name nothing looks it up by.
func TestService_SocialCallback_OverridesAMisreportedProviderName(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{name: ProviderGoogle, identity: &ExternalIdentity{
		Provider:      "not-google-at-all",
		ExternalID:    "external-3",
		Email:         "misreported@example.com",
		EmailVerified: true,
	}}
	f := newFederationFixture(t, provider, ProviderGoogle)
	f.registerUser(t, "misreported@example.com", testTenantA)

	result, err := socialSignIn(t, f, provider, testTenantA)
	if err != nil {
		t.Fatalf("SocialCallback() error = %v", err)
	}
	if result.Identity.Provider != ProviderGoogle {
		t.Errorf("stored Provider = %q, want the requested channel %q", result.Identity.Provider, ProviderGoogle)
	}
}

// TestService_BindExternalIdentity_IsIdempotentAndRefusesADifferentAccount
// covers an already-signed-in user attaching a new identity: binding the
// same external account twice is a no-op, and binding an external account
// already claimed by somebody else is refused.
func TestService_BindExternalIdentity_IsIdempotentAndRefusesADifferentAccount(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{name: ProviderGitHub, identity: &ExternalIdentity{
		ExternalID: "gh-external-1",
		Email:      "owner@example.com",
	}}
	f := newFederationFixture(t, provider)
	owner := f.registerUser(t, "owner@example.com", testTenantA)
	other := f.registerUser(t, "other@example.com", testTenantA)

	bind := func(userID string) (*SocialLoginResult, error) {
		authorizeURL, err := f.svc.SocialAuthorizeURL(t.Context(), SocialAuthorizeInput{
			Provider:    provider.Name(),
			RedirectURI: testRedirectURI,
			LinkUserID:  userID,
		})
		if err != nil {
			t.Fatalf("SocialAuthorizeURL() error = %v", err)
		}
		state := parseQuery(t, authorizeURL).Get("state")
		return f.svc.SocialCallback(t.Context(), SocialCallbackInput{
			Provider: provider.Name(), Code: "code", State: state,
		})
	}

	first, err := bind(owner.ID)
	if err != nil {
		t.Fatalf("first bind() error = %v", err)
	}
	if !first.Bound || first.Tokens != nil {
		t.Errorf("first bind = %+v, want Bound=true and no session started", first)
	}

	second, err := bind(owner.ID)
	if err != nil {
		t.Fatalf("re-binding the same identity to its own owner must be a no-op, got error = %v", err)
	}
	if second.Identity.ID != first.Identity.ID {
		t.Error("re-binding created a second row instead of reusing the existing one")
	}

	_, err = bind(other.ID)
	assertErrorCode(t, err, ErrIdentityAlreadyBound.Code)
}

// TestService_UnbindIdentity_RefusesTheLastLoginMethod is docs/internal/05's
// fourth social-login rule: removing a binding must never leave an account
// with zero ways to sign in, because there is no self-service recovery from
// that state.
func TestService_UnbindIdentity_RefusesTheLastLoginMethod(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{name: ProviderGoogle, identity: &ExternalIdentity{
		ExternalID: "external-only-method", Email: "solo@example.com", EmailVerified: true,
	}}
	f := newFederationFixture(t, provider, ProviderGoogle)

	// A social-only account: created by an unmatched, trusted sign-in, so
	// it has no password at all and exactly one identity. Signing in fails
	// closed on missing membership (see the unmatched-email test above),
	// which is irrelevant here -- the account and its identity are
	// provisioned regardless, and that is what this test exercises.
	if _, err := socialSignIn(t, f, provider, testTenantA); err == nil {
		t.Fatal("socialSignIn() unexpectedly succeeded; a fresh account has no membership yet")
	}
	created, findErr := f.svc.Users().FindByEmail(t.Context(), "solo@example.com")
	if findErr != nil {
		t.Fatalf("account was not provisioned: %v", findErr)
	}
	identities, listErr := f.svc.ListIdentities(t.Context(), created.ID)
	if listErr != nil || len(identities) != 1 {
		t.Fatalf("ListIdentities() = %v, %v, want exactly one bound identity", identities, listErr)
	}

	unbindErr := f.svc.UnbindIdentity(t.Context(), created.ID, identities[0].ID)
	assertErrorCode(t, unbindErr, ErrLastLoginMethod.Code)

	still, listErr := f.svc.ListIdentities(t.Context(), created.ID)
	if listErr != nil || len(still) != 1 {
		t.Errorf("identity was removed despite the refusal: %v, %v", still, listErr)
	}
}

// TestService_UnbindIdentity_AllowedWithAPasswordRemaining proves the
// counting rule itself: a password counts as one login method, so removing
// the one bound identity from an account that also has a password is safe
// and must succeed.
func TestService_UnbindIdentity_AllowedWithAPasswordRemaining(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{name: ProviderGitHub, identity: &ExternalIdentity{
		ExternalID: "gh-external-2", Email: "has-password@example.com",
	}}
	f := newFederationFixture(t, provider)
	user := f.registerUser(t, "has-password@example.com", testTenantA)

	authorizeURL, err := f.svc.SocialAuthorizeURL(t.Context(), SocialAuthorizeInput{
		Provider: provider.Name(), RedirectURI: testRedirectURI, LinkUserID: user.ID,
	})
	if err != nil {
		t.Fatalf("SocialAuthorizeURL() error = %v", err)
	}
	state := parseQuery(t, authorizeURL).Get("state")
	bound, err := f.svc.SocialCallback(t.Context(), SocialCallbackInput{
		Provider: provider.Name(), Code: "code", State: state,
	})
	if err != nil {
		t.Fatalf("SocialCallback() (bind) error = %v", err)
	}

	if unbindErr := f.svc.UnbindIdentity(t.Context(), user.ID, bound.Identity.ID); unbindErr != nil {
		t.Fatalf("UnbindIdentity() error = %v, want success: a password remains as a login method", unbindErr)
	}
	remaining, err := f.svc.ListIdentities(t.Context(), user.ID)
	if err != nil || len(remaining) != 0 {
		t.Errorf("ListIdentities() = %v, %v, want none left", remaining, err)
	}
	if f.events.Count(EventIdentityUnbound) != 1 {
		t.Errorf("authn.identity.unbound published %d times, want 1", f.events.Count(EventIdentityUnbound))
	}
}

// TestService_UnbindIdentity_AnotherUsersIdentityLooksLikeNotFound proves the
// no-existence-disclosure rule: unbinding an identity that belongs to someone
// else must answer exactly like unbinding one that does not exist at all.
func TestService_UnbindIdentity_AnotherUsersIdentityLooksLikeNotFound(t *testing.T) {
	t.Parallel()

	provider := &stubProvider{name: ProviderGitHub, identity: &ExternalIdentity{
		ExternalID: "gh-external-3", Email: "victim@example.com",
	}}
	f := newFederationFixture(t, provider)
	victim := f.registerUser(t, "victim@example.com", testTenantA)
	attacker := f.registerUser(t, "attacker@example.com", testTenantA)

	authorizeURL, err := f.svc.SocialAuthorizeURL(t.Context(), SocialAuthorizeInput{
		Provider: provider.Name(), RedirectURI: testRedirectURI, LinkUserID: victim.ID,
	})
	if err != nil {
		t.Fatalf("SocialAuthorizeURL() error = %v", err)
	}
	state := parseQuery(t, authorizeURL).Get("state")
	bound, err := f.svc.SocialCallback(t.Context(), SocialCallbackInput{
		Provider: provider.Name(), Code: "code", State: state,
	})
	if err != nil {
		t.Fatalf("SocialCallback() (bind) error = %v", err)
	}

	// Somebody else's identity and an identity that does not exist at all
	// must produce the SAME code, so neither answer discloses that the
	// binding is real.
	err = f.svc.UnbindIdentity(t.Context(), attacker.ID, bound.Identity.ID)
	assertErrorCode(t, err, ErrIdentityNotFound.Code)

	err = f.svc.UnbindIdentity(t.Context(), attacker.ID, "no-such-identity")
	assertErrorCode(t, err, ErrIdentityNotFound.Code)
}

// TestLoginMethodCount pins the counting rule UnbindIdentity relies on: a
// password counts once, a verified phone counts once and an unverified one
// does not, and every bound identity counts once.
func TestLoginMethodCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		user          *User
		identityCount int
		want          int
	}{
		{name: "nil user", user: nil, identityCount: 3, want: 0},
		{name: "password only", user: &User{PasswordHash: "x"}, identityCount: 0, want: 1},
		{name: "unverified phone does not count", user: &User{Phone: "+861380000000", PhoneVerified: false}, identityCount: 0, want: 0},
		{name: "verified phone counts", user: &User{Phone: "+861380000000", PhoneVerified: true}, identityCount: 0, want: 1},
		{name: "password plus two identities", user: &User{PasswordHash: "x"}, identityCount: 2, want: 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := LoginMethodCount(tc.user, tc.identityCount); got != tc.want {
				t.Errorf("LoginMethodCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestUserIdentityModel_IsNotTenantScoped is the mandatory isolation
// assertion for user_identities: an external identity belongs to the
// person, who may act inside several tenants, so it must stay visible
// whatever tenant happens to be in the calling context.
//
// This closes a gap the federation round's own doc comment (identity.go's
// UserIdentityRepository) claimed was already covered here and was not:
// repository.go's file comment states every identity-domain repository is
// compensated by exactly this suite, and until now user_identities was the
// one table in this module that carried no such test at all.
func TestUserIdentityModel_IsNotTenantScoped(t *testing.T) {
	t.Parallel()

	db := testutil.NewDB(t)
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

	tenancytest.AssertNotTenantScoped(t, db, UserIdentity{},
		func(db *gorm.DB) error {
			return db.Create(&UserIdentity{
				ID: newID(), UserID: newID(), Provider: ProviderGoogle, ExternalID: newID(),
				CreatedAt: now, UpdatedAt: now,
			}).Error
		},
		countOf[UserIdentity],
	)
}
