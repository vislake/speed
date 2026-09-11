package app

// Fixtures the root package's tests assemble applications from: a host
// configuration target, a module built out of test-provided pieces, a worker
// that records what the engine did to it, and the option set a bare test boot
// runs with. They live in one file rather than beside each test because every
// test file in this package builds on the same pieces.

import (
	"context"
	"embed"
	"net/http"
	"path/filepath"
	"sync"
	"testing"

	"github.com/vislake/speed/go/dbkit"

	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so the tests' DatabaseSpecs have a driver to build from. The engine's
	// own code deliberately carries no dialect package -- which dialects a
	// binary contains is the assembling application's decision.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/app/internal/testutil"
)

// testEnvPrefix is the environment prefix the tests' loader runs under, so a
// test-controlled variable is spelled TEST_<KEY> and never collides with a
// variable the ambient environment might carry.
const testEnvPrefix = "TEST_"

// testHostConfig is a minimal host configuration target: the platform key
// material embedded and skipped -- the engine loads that value as its own
// target, which keeps the six declared key paths unprefixed -- beside one
// host key of its own.
type testHostConfig struct {
	PlatformConfig `config:"-"`
	Port           string `config:"env=TEST_PORT"`
	Token          string `config:"env=TEST_TOKEN"`
}

// testKey returns a distinct 32-byte key material for seed; every material a
// test boots with is valid for dbkit.NewCipher by construction.
func testKey(seed byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return b
}

// testPlatformConfig returns a PlatformConfig with all six materials
// pre-filled: the loader's struct-default source, which is what a test
// assembles a boot with when the key material itself is not under test.
func testPlatformConfig() PlatformConfig {
	return PlatformConfig{
		Authn: PlatformAuthnKeyMaterial{
			Blind_Index_Key: testKey(0x10),
			PII_Cipher_Key:  testKey(0x20),
		},
		Config: PlatformConfigKeyMaterial{Cipher_Key: testKey(0x30)},
		Notification: PlatformNotificationKeyMaterial{
			Contact_Index_Key: testKey(0x40),
		},
		Org: PlatformOrgKeyMaterial{Invitation_Email_Index_Key: testKey(0x50)},
		PKI: PlatformPKIKeyMaterial{Local_Key_Cipher_Key: testKey(0x60)},
	}
}

// testDatabaseSpec returns a DatabaseSpec pointing at a fresh SQLite file
// under the test's temporary directory.
func testDatabaseSpec(t *testing.T) DatabaseSpec {
	t.Helper()
	return DatabaseSpec{
		Dialect: dbkit.DialectSQLite,
		DSN:     filepath.Join(t.TempDir(), "app-test.db"),
	}
}

// testConfigOptions returns the loader options every test's configuration
// stage runs with: no process arguments (the test binary owns flags of its
// own) and the test's environment prefix.
func testConfigOptions() []ConfigOption {
	return []ConfigOption{
		ConfigArgs([]string{}),
		ConfigEnvPrefix(testEnvPrefix),
	}
}

// testConfigOption returns the WithConfig option a test boots with: the host
// target, the platform key material embedded in it, the test's loader options
// and whatever extra loader options the test's subject needs.
func testConfigOption(host *testHostConfig, extra ...ConfigOption) Option {
	return WithConfig(
		ConfigSpec{Host: host, Platform: &host.PlatformConfig},
		append(testConfigOptions(), extra...)...,
	)
}

// testBaseOptions returns the option set a bare test boot assembles with:
// the host target carrying prefilled platform material plus db, and nothing
// else. Tests append the options their subject needs.
func testBaseOptions(t *testing.T, host *testHostConfig) []Option {
	t.Helper()
	*host = testHostConfig{PlatformConfig: testPlatformConfig()}
	return []Option{
		testConfigOption(host),
		WithDatabase(testDatabaseSpec(t)),
	}
}

// testModule is a pkgcore.Module built entirely from test-provided pieces:
// it declares the bootstrap keys, mounts the route and ships the migrations
// the test hands it, so the engine's stages have real declarations to work
// with and no business module is involved.
type testModule struct {
	name        string
	routePath   string
	handler     http.Handler
	keys        []pkgcore.BootstrapKey
	migrations  embed.FS
	registerErr error
}

func (m *testModule) Name() string         { return m.name }
func (m *testModule) DependsOn() []string  { return nil }
func (m *testModule) Migrations() embed.FS { return m.migrations }
func (m *testModule) Locales() embed.FS    { return embed.FS{} }
func (m *testModule) OpenAPISpec() []byte  { return nil }
func (m *testModule) Register(reg *pkgcore.Registry) error {
	if m.registerErr != nil {
		return m.registerErr
	}
	if len(m.keys) > 0 {
		if err := reg.Bootstrap.Add(m.keys...); err != nil {
			return err
		}
	}
	if m.routePath != "" {
		reg.Routes.Mount(m.routePath, m.handler)
	}
	return nil
}

var _ pkgcore.Module = (*testModule)(nil)

// testMigrations returns the fixture migration set: a real sqlite/ migration
// file the engine's migration stage applies.
func testMigrations() embed.FS { return testutil.Migrations }

// testWorker records what the engine did to it: how many times Start and
// Close ran, and what the Close body observed.
type testWorker struct {
	mu       sync.Mutex
	starts   int
	closes   int
	lastCtx  context.Context
	startErr error
	onClose  func(ctx context.Context) error
}

func (w *testWorker) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.starts++
	w.lastCtx = ctx
	return w.startErr
}

func (w *testWorker) Close(ctx context.Context) error {
	w.mu.Lock()
	w.closes++
	w.lastCtx = ctx
	onClose := w.onClose
	w.mu.Unlock()
	if onClose != nil {
		return onClose(ctx)
	}
	return nil
}

func (w *testWorker) counts() (starts, closes int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.starts, w.closes
}
