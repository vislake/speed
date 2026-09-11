// This suite lives in package unittest -- this module's dedicated unit-test
// directory for unit-tier checks with no single source file as their target
// (the backend coding standard's testing-layout rule). It exercises authn's
// dynamic-configuration wiring end to end, black-box: a REAL config module
// stores the value, the module's SettingsReader seam carries it, and the
// behavior the declaration promises -- here, the trusted-provider security
// switch over automatic account linking -- is observed through the public
// service API.
package unittest

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/config"
	configmigrations "github.com/vislake/speed/go/config/migrations"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
)

// stubProvider is a SocialProvider asserting one fixed external identity.
type stubProvider struct {
	name     string
	identity authn.ExternalIdentity
}

func (p *stubProvider) Name() string { return p.name }

func (p *stubProvider) AuthorizeURL(state, redirectURI string) string {
	return "https://provider.example.test/authorize?state=" + url.QueryEscape(state) +
		"&redirect_uri=" + url.QueryEscape(redirectURI)
}

func (p *stubProvider) Exchange(context.Context, string, string) (*authn.ExternalIdentity, error) {
	clone := p.identity
	return &clone, nil
}

const wiringTestRedirectURI = "https://app.example.com/callback"

// wiringTestTenant is the tenant the existing account belongs to.
const wiringTestTenant pkgcore.TenantID = "tenant-a"

// wiringScenario is one end-to-end configuration: a fresh authn service over
// a fresh REAL config module, differing only in what the deployment wired
// statically (constructionTrusted), whether the dynamic reader was wired at
// all, and what -- if anything -- the configs table carries for
// authn.social.trusted_providers.
type wiringScenario struct {
	t        *testing.T
	svc      *authn.Service
	provider *stubProvider
}

// newWiringScenario builds the scenario. rowSet says whether the system row
// for authn.social.trusted_providers exists; rowValue is what it carries.
// wired says whether the authn service was given the config module's handle
// as its SettingsReader -- the difference under test.
func newWiringScenario(t *testing.T, constructionTrusted []string, wired bool, rowSet bool, rowValue string) *wiringScenario {
	t.Helper()

	db := testutil.NewDB(t)
	// The config module's own tables live beside authn's: its migration set
	// is applied over the same connection, the shape a shared-database
	// assembly has.
	dbtest.Migrate(t, db, dbkit.DialectSQLite, dbtest.Migration{Module: "config", FS: configmigrations.FS})
	clock := testutil.NewClock(time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC))
	keys := testutil.NewKeySource(t, "kid-test")
	members := testutil.NewMemberships()

	cipher, err := dbkit.NewCipher(testutil.CipherKey())
	if err != nil {
		t.Fatalf("dbkit.NewCipher: %v", err)
	}
	cfgModule := config.NewModule(db, config.WithCipher(cipher), config.WithPollInterval(0))

	// The config module's schema is folded from the declarations on the
	// registry, so authn's own Register must run first -- the same order an
	// assembly drives -- and the module is the authn one this scenario will
	// not drive; it exists to declare the keys.
	authnModule, err := authn.NewModule(db,
		authn.WithKeySource(keys),
		authn.WithBlindIndexKey(testutil.BlindIndexKey()),
	)
	if err != nil {
		t.Fatalf("authn.NewModule: %v", err)
	}
	reg := componenttest.NewRegistry()
	if declareErr := componenttest.DeclareInto(reg, authnModule, cfgModule); declareErr != nil {
		t.Fatalf("declare both modules: %v", declareErr)
	}
	cfgSvc, err := cfgModule.Attach(reg)
	if err != nil {
		t.Fatalf("config module Attach: %v", err)
	}

	if rowSet {
		pkgcore.RegisterSystemPurpose(config.SystemPurposeSystemWrite)
		sysCtx, sysErr := pkgcore.WithSystemContext(context.Background(), pkgcore.SystemReason{
			Actor:   "ops-1",
			Purpose: config.SystemPurposeSystemWrite,
			Ticket:  "ticket-42",
		})
		if sysErr != nil {
			t.Fatalf("pkgcore.WithSystemContext: %v", sysErr)
		}
		if setErr := cfgSvc.Set(sysCtx, config.ScopeSystem, authn.ConfigKeyTrustedProviders,
			config.Value{Data: rowValue}, "ops-1"); setErr != nil {
			t.Fatalf("Set %s: %v", authn.ConfigKeyTrustedProviders, setErr)
		}
	}

	allowlist, err := authn.NewRedirectAllowlist(wiringTestRedirectURI)
	if err != nil {
		t.Fatalf("NewRedirectAllowlist: %v", err)
	}
	provider := &stubProvider{
		name: authn.ProviderGoogle,
		identity: authn.ExternalIdentity{
			Provider:      authn.ProviderGoogle,
			ExternalID:    "google-subject-1",
			Email:         "sam@example.com",
			EmailVerified: true,
			Name:          "Sam",
		},
	}
	opts := []authn.Option{
		authn.WithKeySource(keys),
		authn.WithBlindIndexKey(testutil.BlindIndexKey()),
		authn.WithMembershipReader(members),
		authn.WithClock(clock.Now),
		authn.WithPasswordParams(authn.PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}),
		authn.WithSocialProviders(provider),
		authn.WithRedirectAllowlist(allowlist),
		authn.WithTrustedProviders(constructionTrusted...),
	}
	if wired {
		opts = append(opts, authn.WithSettingsReader(cfgModule.Handle()))
	}
	svc, err := authn.NewService(db, reg.EventBus(), reg.KVStore(), opts...)
	if err != nil {
		t.Fatalf("authn.NewService: %v", err)
	}
	user, err := svc.Register(context.Background(), authn.RegisterInput{
		Email: "sam@example.com", Password: "correct horse battery staple", DisplayName: "Sam",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	// The existing account the external identity claims must belong to a
	// tenant, or a successful auto-link would then fail session start on
	// the separate tenant-membership check (the same wiring the module's
	// own federation fixture performs).
	members.Add(user.ID, wiringTestTenant)
	return &wiringScenario{t: t, svc: svc, provider: provider}
}

// signIn drives a full authorize-then-callback round trip for the stub
// channel, the two-step shape a browser follows.
func (s *wiringScenario) signIn() (*authn.SocialLoginResult, error) {
	s.t.Helper()
	ctx := context.Background()
	authorizeURL, err := s.svc.SocialAuthorizeURL(ctx, authn.SocialAuthorizeInput{
		Provider:    s.provider.Name(),
		RedirectURI: wiringTestRedirectURI,
	})
	if err != nil {
		s.t.Fatalf("SocialAuthorizeURL: %v", err)
	}
	parsed, err := url.Parse(authorizeURL)
	if err != nil {
		s.t.Fatalf("parse authorize URL: %v", err)
	}
	state := parsed.Query().Get("state")
	if state == "" {
		s.t.Fatal("SocialAuthorizeURL produced no state parameter")
	}
	return s.svc.SocialCallback(ctx, authn.SocialCallbackInput{
		Provider: s.provider.Name(),
		Code:     "test-code",
		State:    state,
		TenantID: wiringTestTenant,
	})
}

// TestTrustedProvidersConfigItem_ControlsAutoLinkingEndToEnd is the
// end-to-end proof the reviewer's finding demanded: the declared
// authn.social.trusted_providers item, stored in the real configs table and
// read through the module's settings seam, controls whether a verified
// social sign-in whose address already belongs to an account may link to it
// automatically. Each arm changes exactly one variable.
func TestTrustedProvidersConfigItem_ControlsAutoLinkingEndToEnd(t *testing.T) {
	t.Run("wired reader honors an explicit row (config value links)", func(t *testing.T) {
		s := newWiringScenario(t, nil, true, true, authn.ProviderGoogle)
		result, err := s.signIn()
		if err != nil {
			t.Fatalf("sign-in with the config row set: %v; want an automatic link", err)
		}
		if !result.AutoLinked {
			t.Fatal("sign-in with the config row set did not auto-link; the declared switch is not effective")
		}
	})

	t.Run("unwired reader ignores the row (the switch was dead)", func(t *testing.T) {
		s := newWiringScenario(t, nil, false, true, authn.ProviderGoogle)
		if _, err := s.signIn(); !errors.Is(err, authn.ErrIdentityRequiresBinding) {
			t.Fatalf("sign-in without the settings reader = %v; want ErrIdentityRequiresBinding (a config row alone must not link before the seam is wired)", err)
		}
	})

	t.Run("unset row falls back to the construction-time list", func(t *testing.T) {
		s := newWiringScenario(t, []string{authn.ProviderGoogle}, true, false, "")
		result, err := s.signIn()
		if err != nil {
			t.Fatalf("sign-in with only the construction-time list: %v; want an automatic link", err)
		}
		if !result.AutoLinked {
			t.Fatal("the construction-time trusted list stopped being the fallback once a reader is wired")
		}
	})

	t.Run("explicit empty row overrides the construction-time list (off switch works)", func(t *testing.T) {
		s := newWiringScenario(t, []string{authn.ProviderGoogle}, true, true, "")
		if _, err := s.signIn(); !errors.Is(err, authn.ErrIdentityRequiresBinding) {
			t.Fatalf("sign-in with an explicit empty row = %v; want ErrIdentityRequiresBinding (the operator turned linking off)", err)
		}
	})
}
