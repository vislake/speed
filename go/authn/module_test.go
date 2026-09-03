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
		// No sensitive item may appear here without a deliberate
		// decision: config.Attach refuses a cipher-less startup as soon
		// as one exists, which changes what every host must wire.
		if item.Sensitive {
			t.Errorf("config item %q is Sensitive; that forces a cipher on every host and needs its own decision", item.Key)
		}
	}
	for _, want := range []string{
		ConfigKeyPasswordMinLength, ConfigKeyPasswordMaxLength,
		ConfigKeyAccessTokenTTL, ConfigKeyRefreshTokenTTL,
		ConfigKeySessionTTL, ConfigKeyImmediateRevocation,
	} {
		if _, ok := items[want]; !ok {
			t.Errorf("config item %q was not declared", want)
		}
	}

	flags := reg.Features.Flags()
	if len(flags) != 1 || flags[0].Key != FeatureFlagPasswordLogin {
		t.Errorf("feature flags = %+v, want exactly %q", flags, FeatureFlagPasswordLogin)
	}
	if !flags[0].Default {
		t.Error("the password-login flag defaults to off; a fresh deployment would have no way in")
	}

	// authn declares no permissions yet: every endpoint it will serve is
	// self-service, and the tenant-scoped SSO administration that does
	// need one lands with the federation work.
	if perms := reg.Permissions.Permissions(); len(perms) != 0 {
		t.Errorf("Permissions() = %v, want none for a self-service surface", perms)
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
