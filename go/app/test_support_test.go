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

// testHostConfig is a minimal host configuration target: the host's own keys,
// resolved by the loader. Declared bootstrap keys are deliberately absent
// from it -- a declaration is resolved off the component that makes it, with
// no host struct field behind it.
type testHostConfig struct {
	Port  string `config:"env=TEST_PORT"`
	Token string `config:"env=TEST_TOKEN"`
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

// The declared bootstrap key the app package's own tests resolve. It is not
// a fixture: the go/config component declares config.cipher_key, and the
// package's own kernel tests import go/config, so this test binary's
// registration carries the real declaration -- the same shape a consumer
// gets by importing the module packages whose components declare their keys.
// The engine's infrastructure step builds its platform cipher from that key,
// so a binary whose registration carried no such declaration could not
// assemble (the infrastructure step reports it, naming the path).
const testCipherKeyPath = "config.cipher_key"

// testDevDefaults returns the declared defaults table the declared keys fall
// back to: a recognizable 32-byte material per key, the shape a host's
// documented non-secret development defaults take.
func testDevDefaults() map[string][]byte {
	return map[string][]byte{
		testCipherKeyPath: testKey(0x30),
	}
}

// materialOf reads the bootstrap material an assembled application published.
func materialOf(t *testing.T, a *Application) *pkgcore.BootstrapMaterial {
	t.Helper()
	material, err := pkgcore.Get[*pkgcore.BootstrapMaterial](a.reg)
	if err != nil {
		t.Fatalf("read the published bootstrap material: %v", err)
	}
	return material
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
// own), the test's environment prefix, and the fixture declared defaults
// table the registered keys fall back to. A test that needs the table empty
// passes ConfigDevDefaults(nil) later in the same list.
func testConfigOptions() []ConfigOption {
	return []ConfigOption{
		ConfigArgs([]string{}),
		ConfigEnvPrefix(testEnvPrefix),
		ConfigDevDefaults(testDevDefaults()),
	}
}

// testConfigOption returns the WithConfig option a test boots with: the host
// target, the test's loader options and whatever extra loader options the
// test's subject needs.
func testConfigOption(host *testHostConfig, extra ...ConfigOption) Option {
	return WithConfig(
		ConfigSpec{Host: host},
		append(testConfigOptions(), extra...)...,
	)
}

// testBaseOptions returns the option set a bare test boot assembles with: the
// host target plus db, and nothing else. Tests append the options their
// subject needs.
func testBaseOptions(t *testing.T, host *testHostConfig) []Option {
	t.Helper()
	*host = testHostConfig{}
	return []Option{
		testConfigOption(host),
		WithDatabase(testDatabaseSpec(t)),
	}
}

// testModule is a pkgcore.Module built entirely from test-provided pieces:
// it mounts the route and ships the migrations the test hands it, so the
// engine's stages have real declarations to work with and no business module
// is involved.
type testModule struct {
	name        string
	routePath   string
	handler     http.Handler
	migrations  embed.FS
	registerErr error
}

func (m *testModule) Name() string         { return m.name }
func (m *testModule) DependsOn() []string  { return nil }
func (m *testModule) Migrations() embed.FS { return m.migrations }
func (m *testModule) Locales() embed.FS    { return embed.FS{} }
func (m *testModule) OpenAPISpec() []byte  { return nil }
func (m *testModule) Register(reg pkgcore.Registrar) error {
	if m.registerErr != nil {
		return m.registerErr
	}
	if m.routePath != "" {
		reg.RoutesSeat().Mount(m.routePath, m.handler)
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
