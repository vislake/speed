package authn

import (
	"errors"
	"io/fs"
	"slices"
	"testing"
	"time"

	"github.com/vislake/speed/go/authn/internal/testutil"
	"github.com/vislake/speed/go/pkgcore"
)

// newTestModule builds a Module with the mandatory options supplied.
func newTestModule(t *testing.T, extra ...Option) *Module {
	t.Helper()

	keys, _ := newTestKeySet(t)
	opts := append([]Option{
		WithSigningKeys(keys),
		WithBlindIndexKey(testutil.BlindIndexKey()),
		WithPasswordParams(testParams()),
	}, extra...)

	module, err := NewModule(testutil.NewDB(t), opts...)
	if err != nil {
		t.Fatalf("NewModule() error = %v", err)
	}
	return module
}

// newTestRegistry returns a Registry wired to the standalone deployment
// mode's own implementations, which double as the test doubles.
func newTestRegistry() *pkgcore.Registry {
	return pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
}

func TestModule_Identity(t *testing.T) {
	t.Parallel()

	module := newTestModule(t)

	if got := module.Name(); got != "authn" {
		t.Errorf("Name() = %q, want %q", got, "authn")
	}
	// authn must NOT declare a dependency on org: the membership question
	// is asked through the injected MembershipReader seam precisely so
	// that the dependency does not exist. A non-empty list here means
	// someone reintroduced it.
	if got := module.DependsOn(); len(got) != 0 {
		t.Errorf("DependsOn() = %v, want none", got)
	}
	if got := module.OpenAPISpec(); got != nil {
		t.Errorf("OpenAPISpec() = %q, want nil until the spec fragment and its generated interface land together", got)
	}
}

// TestModule_MigrationsShipBothDialects proves neither dialect was forgotten,
// and that the two sets carry the same file names -- a migration present for
// one dialect only produces a schema that exists in one deployment mode.
func TestModule_MigrationsShipBothDialects(t *testing.T) {
	t.Parallel()

	module := newTestModule(t)
	migrationsFS := module.Migrations()

	names := func(dialect string) []string {
		entries, err := fs.ReadDir(migrationsFS, dialect)
		if err != nil {
			t.Fatalf("read the %s migrations: %v", dialect, err)
		}
		var out []string
		for _, entry := range entries {
			out = append(out, entry.Name())
		}
		slices.Sort(out)
		return out
	}

	postgres, sqlite := names("postgres"), names("sqlite")
	if len(postgres) == 0 {
		t.Fatal("no postgres migrations are embedded")
	}
	if !slices.Equal(postgres, sqlite) {
		t.Errorf("the two dialects carry different migration files:\n  postgres: %v\n  sqlite:   %v", postgres, sqlite)
	}
}

func TestModule_LocalesShipBothLanguages(t *testing.T) {
	t.Parallel()

	module := newTestModule(t)
	entries, err := fs.ReadDir(module.Locales(), ".")
	if err != nil {
		t.Fatalf("read the embedded locales: %v", err)
	}

	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"en-US.toml", "zh-CN.toml"}) {
		t.Errorf("embedded locale files = %v, want exactly en-US.toml and zh-CN.toml", names)
	}
}

// TestModule_RegisterDeclaresItsSurface pins everything Register contributes,
// so a declaration silently dropped in a later edit fails here rather than
// showing up as a missing admin-console form or an unmapped public event.
func TestModule_RegisterDeclaresItsSurface(t *testing.T) {
	t.Parallel()

	module := newTestModule(t)
	reg := newTestRegistry()
	if err := module.Register(reg); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	if module.Service() == nil {
		t.Fatal("Service() is nil after Register()")
	}

	published := make(map[string]string, len(reg.Events.Published()))
	for _, decl := range reg.Events.Published() {
		published[decl.Type] = decl.PayloadType
		if decl.Description == "" {
			t.Errorf("event %q has no description; the generated catalog renders it", decl.Type)
		}
	}
	for _, want := range []string{
		EventUserCreated, EventUserLoggedIn, EventLoginFailed,
		EventSessionRevoked, EventSessionReplayDetected, EventTenantSwitched,
	} {
		if _, ok := published[want]; !ok {
			t.Errorf("event %q was not declared", want)
		}
	}

	actions := reg.AuditActions.Actions()
	for _, want := range []string{AuditActionUserRegister, AuditActionUserLogin, AuditActionSessionRevoke, AuditActionTenantSwitch} {
		if !slices.Contains(actions, want) {
			t.Errorf("audit action %q was not registered (have %v)", want, actions)
		}
	}

	items := make(map[string]pkgcore.ConfigItem, len(reg.Config.Items()))
	for _, item := range reg.Config.Items() {
		items[item.Key] = item
		if item.Description == "" {
			t.Errorf("config item %q has no description; the configuration reference is generated from it", item.Key)
		}
		if item.Group != "authn" {
			t.Errorf("config item %q is grouped under %q, want %q", item.Key, item.Group, "authn")
		}
		// A Sensitive item may never also be Public: config.Add rejects
		// the combination, and the reason is that the pre-authentication
		// public endpoint would then serve a secret to anyone.
		if item.Sensitive && item.Public {
			t.Errorf("config item %q is both Sensitive and Public", item.Key)
		}
	}

	// The Sensitive items are the social-channel client secrets, and the
	// decision to have any at all is deliberate: config.Attach refuses a
	// cipher-less startup as soon as one exists, so wiring authn makes a
	// configuration cipher mandatory for every host. This assertion pins
	// exactly which keys carry that cost, so an accidental sixth one is a
	// test failure rather than a surprise at somebody else's startup.
	var sensitive []string
	for _, item := range reg.Config.Items() {
		if item.Sensitive {
			sensitive = append(sensitive, item.Key)
		}
	}
	slices.Sort(sensitive)
	wantSensitive := []string{
		ConfigKeyDingTalkClientSecret, ConfigKeyFeishuClientSecret,
		ConfigKeyGitHubClientSecret, ConfigKeyGoogleClientSecret,
		ConfigKeyWeChatClientSecret,
	}
	slices.Sort(wantSensitive)
	if !slices.Equal(sensitive, wantSensitive) {
		t.Errorf("sensitive config items = %v, want %v", sensitive, wantSensitive)
	}
	for _, want := range []string{
		ConfigKeyPasswordMinLength, ConfigKeyPasswordMaxLength,
		ConfigKeyAccessTokenTTL, ConfigKeyRefreshTokenTTL,
		ConfigKeySessionTTL, ConfigKeyImmediateRevocation,
		ConfigKeyTrustedProviders, ConfigKeyOAuthStateTTL,
		ConfigKeyGoogleClientID, ConfigKeyGoogleClientSecret,
		ConfigKeyGitHubClientID, ConfigKeyGitHubClientSecret,
		ConfigKeyWeChatClientID, ConfigKeyWeChatClientSecret,
		ConfigKeyDingTalkClientID, ConfigKeyDingTalkClientSecret,
		ConfigKeyFeishuClientID, ConfigKeyFeishuClientSecret,
	} {
		if _, ok := items[want]; !ok {
			t.Errorf("config item %q was not declared", want)
		}
	}

	// The trusted-provider list must default to empty. A non-empty default
	// would silently enable automatic account linking on every deployment
	// that never looked at the setting, which is the one decision in this
	// module that must be made deliberately rather than inherited.
	if got := items[ConfigKeyTrustedProviders].Default; got != "" {
		t.Errorf("%s defaults to %v, want an empty list so automatic account linking is off until a deployment enables it", ConfigKeyTrustedProviders, got)
	}

	flags := make(map[string]pkgcore.FeatureFlag, len(reg.Features.Flags()))
	for _, flag := range reg.Features.Flags() {
		flags[flag.Key] = flag
		if flag.Description == "" {
			t.Errorf("feature flag %q has no description", flag.Key)
		}
	}
	if !flags[FeatureFlagPasswordLogin].Default {
		t.Error("the password-login flag defaults to off; a fresh deployment would have no way in")
	}
	// Every federated channel defaults to OFF: a channel with no
	// credentials configured must not be rendered on the login page, where
	// clicking it fails at the provider with an error nobody can act on.
	for _, key := range []string{
		FeatureFlagSocialGoogle, FeatureFlagSocialGitHub, FeatureFlagSocialWeChat,
		FeatureFlagSocialDingTalk, FeatureFlagSocialFeishu, FeatureFlagEnterpriseSSO,
	} {
		flag, ok := flags[key]
		if !ok {
			t.Errorf("feature flag %q was not declared", key)
			continue
		}
		if flag.Default {
			t.Errorf("feature flag %q defaults to on; an unconfigured channel would be offered on the login page", key)
		}
	}

	// The single permission this module declares. Everything else it
	// serves is self-service -- a person acting on their own account --
	// and needs authentication rather than authorization.
	if perms := reg.Permissions.Permissions(); !slices.Equal(perms, []string{PermissionSSOManage}) {
		t.Errorf("Permissions() = %v, want exactly %v", perms, []string{PermissionSSOManage})
	}
}

// TestModule_RegisterTwiceReportsTheCollision proves a duplicate declaration
// surfaces as an error rather than as a silently merged registry.
func TestModule_RegisterTwiceReportsTheCollision(t *testing.T) {
	t.Parallel()

	module := newTestModule(t)
	reg := newTestRegistry()
	if err := module.Register(reg); err != nil {
		t.Fatalf("the first Register() error = %v", err)
	}
	if err := module.Register(reg); err == nil {
		t.Fatal("the second Register() on the same registry error = nil, want a duplicate-declaration error")
	}
}

// TestModule_RegisterDoesNoIO is the pkgcore.Module contract ("It must not
// perform I/O; it only declares"). It is checked by handing Register a
// database whose connection is already closed: any query at all would fail,
// and Register must not notice.
func TestModule_RegisterDoesNoIO(t *testing.T) {
	t.Parallel()

	module := newTestModule(t)
	sqlDB, err := module.db.DB()
	if err != nil {
		t.Fatalf("reach the underlying *sql.DB: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close the database: %v", err)
	}

	if err := module.Register(newTestRegistry()); err != nil {
		t.Fatalf("Register() error = %v against a closed database; Register must only declare, never query", err)
	}
}

func TestNewModule_RejectsAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()

	keys, _ := newTestKeySet(t)
	db := testutil.NewDB(t)

	cases := []struct {
		name string
		opts []Option
	}{
		{name: "no signing keys", opts: []Option{WithBlindIndexKey(testutil.BlindIndexKey())}},
		{name: "no blind-index key", opts: []Option{WithSigningKeys(keys)}},
		{name: "a short blind-index key", opts: []Option{WithSigningKeys(keys), WithBlindIndexKey([]byte("nope"))}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewModule(db, tc.opts...); err == nil {
				t.Error("NewModule() error = nil, want a rejection")
			}
		})
	}

	if _, err := NewModule(nil, WithSigningKeys(keys), WithBlindIndexKey(testutil.BlindIndexKey())); err == nil {
		t.Error("NewModule(nil db) error = nil, want a rejection")
	}
}

// TestOptions_IgnoreMeaninglessValues pins the "a nil or non-positive value
// leaves the default in place" contract every option documents, so a caller
// passing a zero cannot silently disable a timeout.
func TestOptions_IgnoreMeaninglessValues(t *testing.T) {
	t.Parallel()

	keys, _ := newTestKeySet(t)
	cfg, err := newOptions([]Option{
		WithSigningKeys(keys),
		WithBlindIndexKey(testutil.BlindIndexKey()),
		WithClock(nil),
		WithIssuer(""),
		WithAccessTokenTTL(0),
		WithRefreshTokenTTL(-time.Hour),
		WithSessionTTL(0),
		WithRevocationMode("not-a-mode"),
		nil,
	})
	if err != nil {
		t.Fatalf("newOptions() error = %v", err)
	}

	if cfg.now == nil {
		t.Error("WithClock(nil) cleared the clock")
	}
	if cfg.issuer != DefaultIssuer {
		t.Errorf("issuer = %q, want the default %q", cfg.issuer, DefaultIssuer)
	}
	if cfg.accessTTL != DefaultAccessTokenTTL {
		t.Errorf("accessTTL = %v, want %v", cfg.accessTTL, DefaultAccessTokenTTL)
	}
	if cfg.refreshTTL != DefaultRefreshTokenTTL {
		t.Errorf("refreshTTL = %v, want %v", cfg.refreshTTL, DefaultRefreshTokenTTL)
	}
	if cfg.sessionTTL != DefaultSessionTTL {
		t.Errorf("sessionTTL = %v, want %v", cfg.sessionTTL, DefaultSessionTTL)
	}
	if cfg.revocationMode != RevocationModeNatural {
		t.Errorf("revocationMode = %q, want %q", cfg.revocationMode, RevocationModeNatural)
	}
}

// TestWithBlindIndexKey_CopiesTheCallersSlice proves the option does not
// retain the caller's array, so a caller that zeroes its key buffer after
// wiring cannot silently break every lookup.
func TestWithBlindIndexKey_CopiesTheCallersSlice(t *testing.T) {
	t.Parallel()

	keys, _ := newTestKeySet(t)
	key := testutil.BlindIndexKey()

	cfg, err := newOptions([]Option{WithSigningKeys(keys), WithBlindIndexKey(key)})
	if err != nil {
		t.Fatalf("newOptions() error = %v", err)
	}
	for i := range key {
		key[i] = 0
	}
	if slices.Equal(cfg.blindIndexKey, key) {
		t.Error("zeroing the caller's slice also zeroed the stored key; WithBlindIndexKey must copy")
	}
}

// TestConfigItems_AreValidDeclarations runs every declared item through the
// same validation pkgcore applies at registration, one at a time, so a
// failure names the item rather than reporting that the batch was rejected.
func TestConfigItems_AreValidDeclarations(t *testing.T) {
	t.Parallel()

	for _, item := range configItems() {
		t.Run(item.Key, func(t *testing.T) {
			reg := newTestRegistry()
			if err := reg.Config.Add(item); err != nil {
				t.Errorf("Add(%+v) error = %v", item, err)
				if errors.Is(err, pkgcore.ErrInvalidConfigItem) {
					t.Log("the declaration contradicts itself; see pkgcore.ConfigItem's field documentation")
				}
			}
		})
	}
}
