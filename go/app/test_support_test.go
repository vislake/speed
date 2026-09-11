package app

// Fixtures the root package's tests assemble assemblies from: a host
// configuration target, the loader options a bare test boot runs with, and
// the marker product the stage-recording components hand back. They live in
// one file rather than beside each test because every test file in this
// package builds on the same pieces.

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
// a fixture: the go/config module declares config.cipher_key, and the
// package's own kernel tests import go/config, so this test binary's
// registration carries the real declaration -- the same shape a consumer
// gets by importing the module packages whose components declare their keys.
const testCipherKeyPath = "config.cipher_key"

// testDevDefaults returns the declared defaults table the declared keys fall
// back to: a recognizable 32-byte material per key, the shape a host's
// documented non-secret development defaults take.
func testDevDefaults() map[string][]byte {
	return map[string][]byte{
		testCipherKeyPath: testKey(0x30),
	}
}

// testConfigOptions returns the loader options every test's load runs with:
// no process arguments (the test binary owns flags of its own), the test's
// environment prefix, and the fixture declared defaults table the registered
// keys fall back to. A test that needs the table empty passes
// ConfigDevDefaults(nil) later in the same list.
func testConfigOptions() []ConfigOption {
	return []ConfigOption{
		ConfigArgs([]string{}),
		ConfigEnvPrefix(testEnvPrefix),
		ConfigDevDefaults(testDevDefaults()),
	}
}

// testMarker is the trivial product of a test component that carries
// behavior but no value -- a stage recorder, a probe.
type testMarker struct{ name string }
