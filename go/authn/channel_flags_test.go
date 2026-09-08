package authn

// The declared sign-in channel feature flags (module.go's FeatureFlag*
// constants) must be enforced at request time through the FeatureGate seam,
// not remain declarations nothing reads. These tests pin that enforcement:
// with a gate wired and a channel's flag off, the channel refuses at every
// one of its entry points; with the flag on, behavior is unchanged; and a
// gate that cannot be read fails closed.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// scriptedFeatureGate is the FeatureGate test double: a fixed value per
// flag key, an optional forced failure, and a record of every key actually
// asked -- so a test can also pin WHICH flag a channel consults.
type scriptedFeatureGate struct {
	mu     sync.Mutex
	values map[string]bool
	fail   error
	reads  []string
}

// IsEnabled implements FeatureGate.
func (g *scriptedFeatureGate) IsEnabled(_ context.Context, key string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reads = append(g.reads, key)
	if g.fail != nil {
		return false, g.fail
	}
	return g.values[key], nil
}

// asked returns the keys the gate has been consulted for, under the lock.
func (g *scriptedFeatureGate) asked() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]string, len(g.reads))
	copy(out, g.reads)
	return out
}

// newScriptedFeatureGate builds a gate whose answers come from overrides,
// defaulting every declared flag to true so an unrelated test channel never
// trips.
func newScriptedFeatureGate(overrides map[string]bool) *scriptedFeatureGate {
	values := make(map[string]bool, len(overrides))
	for _, key := range []string{
		FeatureFlagPasswordLogin,
		FeatureFlagSMSLogin,
		FeatureFlagSocialGoogle,
		FeatureFlagSocialGitHub,
		FeatureFlagSocialWeChat,
		FeatureFlagSocialDingTalk,
		FeatureFlagSocialFeishu,
		FeatureFlagEnterpriseSSO,
	} {
		values[key] = true
	}
	for key, on := range overrides {
		values[key] = on
	}
	return &scriptedFeatureGate{values: values}
}

// TestService_Login_PasswordChannelDisabled_Refuses pins the gate's
// enforcement: the declared authn.password_login flag is read at request
// time, so a deployment turning password sign-in off (to force enterprise
// SSO, say) does more than hide the form from the login page -- POSTing the
// password endpoint must not issue tokens, or the "disabled" channel stays
// fully callable by anyone who knows the URL. With a feature gate wired
// and the flag off, Login must refuse with authn.channel_disabled before
// any password work, rate-limit burn or lockout accounting happens.
func TestService_Login_PasswordChannelDisabled_Refuses(t *testing.T) {
	t.Parallel()

	gate := newScriptedFeatureGate(map[string]bool{FeatureFlagPasswordLogin: false})
	f := newServiceFixture(t, WithFeatureGate(gate))
	f.registerUser(t, "password-disabled@example.com", testTenantA)

	_, err := f.svc.Login(t.Context(), LoginInput{
		Identifier: "password-disabled@example.com", Password: testPassword,
	})
	if !hasCode(err, ErrChannelDisabled.Code) {
		t.Fatalf("Login() with the password channel disabled error = %v, want code %q", err, ErrChannelDisabled.Code)
	}

	if reads := gate.asked(); len(reads) != 1 || reads[0] != FeatureFlagPasswordLogin {
		t.Errorf("the gate was asked %v, want exactly [%s]", reads, FeatureFlagPasswordLogin)
	}
}

// TestService_Login_PasswordChannelEnabled_StillSucceeds guards the gate
// against over-refusing: with the flag on, a correct password must keep
// issuing tokens -- the flag is a per-channel switch, not a new way for
// logins to break.
func TestService_Login_PasswordChannelEnabled_StillSucceeds(t *testing.T) {
	t.Parallel()

	gate := newScriptedFeatureGate(map[string]bool{FeatureFlagPasswordLogin: true})
	f := newServiceFixture(t, WithFeatureGate(gate))
	f.registerUser(t, "password-enabled@example.com", testTenantA)

	pair, err := f.svc.Login(t.Context(), LoginInput{
		Identifier: "password-enabled@example.com", Password: testPassword,
	})
	if err != nil {
		t.Fatalf("Login() with the password channel enabled error = %v, want success", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" {
		t.Errorf("Login() returned an incomplete token pair: %+v", pair)
	}
}

// TestService_SMSLoginChannelDisabled_Refuses covers the sms_login flag's
// two entry points: requesting a code (which would otherwise keep spending
// real SMS deliveries and verification-code rows on a channel the deployment
// turned off) and redeeming one (which would otherwise start a session).
func TestService_SMSLoginChannelDisabled_Refuses(t *testing.T) {
	t.Parallel()

	gate := newScriptedFeatureGate(map[string]bool{FeatureFlagSMSLogin: false})
	f := newServiceFixture(t, WithFeatureGate(gate))

	err := f.svc.RequestSMSCode(t.Context(), RequestSMSCodeInput{Phone: "+15550001234", IP: "203.0.113.10"})
	if !hasCode(err, ErrChannelDisabled.Code) {
		t.Fatalf("RequestSMSCode() with the SMS channel disabled error = %v, want code %q", err, ErrChannelDisabled.Code)
	}

	_, err = f.svc.LoginWithSMSCode(t.Context(), SMSLoginInput{
		Phone: "+15550001234", Code: "123456", IP: "203.0.113.10",
	})
	if !hasCode(err, ErrChannelDisabled.Code) {
		t.Fatalf("LoginWithSMSCode() with the SMS channel disabled error = %v, want code %q", err, ErrChannelDisabled.Code)
	}
}

// TestService_SocialChannelDisabled_Refuses covers the per-channel social
// flags on both halves of the flow: the authorize step must not mint a
// state value or point the browser at the provider, and a callback that
// arrives anyway (a flow started before the flag was turned off) must not
// complete as a sign-in. Disabling one provider's channel must leave the
// rest of the family alone.
func TestService_SocialChannelDisabled_Refuses(t *testing.T) {
	t.Parallel()

	allowlist, err := NewRedirectAllowlist(testRedirectURI)
	if err != nil {
		t.Fatalf("NewRedirectAllowlist() error = %v", err)
	}
	gate := newScriptedFeatureGate(map[string]bool{FeatureFlagSocialGitHub: false})
	f := newServiceFixture(t,
		WithFeatureGate(gate),
		WithSocialProviders(&stubProvider{name: ProviderGitHub}, &stubProvider{name: ProviderGoogle}),
		WithRedirectAllowlist(allowlist),
	)

	_, err = f.svc.SocialAuthorizeURL(t.Context(), SocialAuthorizeInput{
		Provider:    ProviderGitHub,
		RedirectURI: testRedirectURI,
	})
	if !hasCode(err, ErrChannelDisabled.Code) {
		t.Fatalf("SocialAuthorizeURL() with the channel disabled error = %v, want code %q", err, ErrChannelDisabled.Code)
	}

	_, err = f.svc.SocialCallback(t.Context(), SocialCallbackInput{
		Provider: ProviderGitHub, Code: "code", State: "state",
	})
	if !hasCode(err, ErrChannelDisabled.Code) {
		t.Fatalf("SocialCallback() with the channel disabled error = %v, want code %q", err, ErrChannelDisabled.Code)
	}

	// The Google channel shares the deployment but not the flag: it must
	// still authorize normally.
	url, err := f.svc.SocialAuthorizeURL(t.Context(), SocialAuthorizeInput{
		Provider:    ProviderGoogle,
		RedirectURI: testRedirectURI,
	})
	if err != nil {
		t.Fatalf("SocialAuthorizeURL(google) error = %v, want success -- disabling github must not disable google", err)
	}
	if url == "" {
		t.Error("SocialAuthorizeURL(google) returned an empty URL")
	}
}

// TestService_EnterpriseSSO_FlagDisabled_Refuses covers authn.sso.oidc on
// the enterprise relying party's two host-facing entry points: building the
// authorization URL and completing a callback.
func TestService_EnterpriseSSO_FlagDisabled_Refuses(t *testing.T) {
	t.Parallel()

	gate := newScriptedFeatureGate(map[string]bool{FeatureFlagEnterpriseSSO: false})
	f := newServiceFixture(t, WithFeatureGate(gate))

	_, err := f.svc.SSO().AuthorizeURL(t.Context(), "https://app.example.com/callback", "binding")
	if !hasCode(err, ErrChannelDisabled.Code) {
		t.Fatalf("SSO AuthorizeURL() with the flag disabled error = %v, want code %q", err, ErrChannelDisabled.Code)
	}

	_, err = f.svc.SSO().Callback(t.Context(), SSOCallbackInput{
		TenantID: pkgcore.TenantID("tenant-sso"), Code: "code", State: "state",
	})
	if !hasCode(err, ErrChannelDisabled.Code) {
		t.Fatalf("SSO Callback() with the flag disabled error = %v, want code %q", err, ErrChannelDisabled.Code)
	}
}

// TestService_ChannelGateFailure_FailsClosed pins the "unreadable gate
// denies" half of the channel gate's contract: a config outage must not
// silently re-enable every channel an operator disabled.
func TestService_ChannelGateFailure_FailsClosed(t *testing.T) {
	t.Parallel()

	gate := newScriptedFeatureGate(nil)
	gate.fail = errors.New("config store unreachable")
	f := newServiceFixture(t, WithFeatureGate(gate))
	f.registerUser(t, "gate-down@example.com", testTenantA)

	_, err := f.svc.Login(t.Context(), LoginInput{
		Identifier: "gate-down@example.com", Password: testPassword,
	})
	if !hasCode(err, ErrInternal.Code) {
		t.Fatalf("Login() with an unreadable gate error = %v, want code %q (fail closed)", err, ErrInternal.Code)
	}
}
