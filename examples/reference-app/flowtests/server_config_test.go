package flowtests

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/examples/reference-app/internal/testutil"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/jobs"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pki"
)

// TestConfigFromEnv_Defaults verifies ConfigFromEnv's zero-environment
// defaults. Every other test in this file drives BuildServer directly
// through testConfig(t), bypassing ConfigFromEnv (and its loader-driven
// bootstrap) entirely, so ConfigFromEnv itself is covered.
//
// testutil.ClearBootstrapEnv clears every variable ConfigFromEnv reads,
// rather than leaving them untouched, so this test's outcome does not depend
// on the ambient environment ConfigFromEnv happens to run in -- PORT in
// particular is commonly preset by hosting platforms, and an ambient value
// would make this test spuriously fail (or, worse, spuriously pass for the
// wrong reason) outside a clean shell. Every cleared variable is restored
// automatically once the test finishes.
func TestConfigFromEnv_Defaults(t *testing.T) {
	testutil.ClearBootstrapEnv(t)

	cfg, err := app.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.DeploymentMode != pkgcore.DeploymentModeStandalone {
		t.Fatalf("DeploymentMode = %q, want %q", cfg.DeploymentMode, pkgcore.DeploymentModeStandalone)
	}
	if cfg.Port != app.DefaultPort {
		t.Fatalf("Port = %q, want %q", cfg.Port, app.DefaultPort)
	}
	if cfg.SQLitePath != app.DefaultSQLitePath {
		t.Fatalf("SQLitePath = %q, want %q", cfg.SQLitePath, app.DefaultSQLitePath)
	}
	if !bytes.Equal(cfg.ConfigKey, app.DevConfigKey) {
		t.Fatalf("ConfigKey = %x, want the dev default %x", cfg.ConfigKey, app.DevConfigKey)
	}
	if cfg.RedisAddr != "" {
		t.Fatalf("RedisAddr = %q, want the empty default (in-process bus)", cfg.RedisAddr)
	}
	if cfg.OTLPEndpoint != "" {
		t.Fatalf("OTLPEndpoint = %q, want the empty default (local exporters)", cfg.OTLPEndpoint)
	}
	if cfg.DemoUsersPassword != "" {
		t.Fatalf("DemoUsersPassword = %q, want the empty default (demo-user seed skipped)", cfg.DemoUsersPassword)
	}
	if cfg.ObjectStoreRoot != "" {
		t.Fatalf("ObjectStoreRoot = %q, want the empty default (Preset local-store directory)", cfg.ObjectStoreRoot)
	}
	if cfg.DisableDemoUserHeader {
		t.Fatal("DisableDemoUserHeader = true, want false (the default: app.DemoUserHeader keeps winning, unchanged)")
	}
	if cfg.ReadFlyClientIP {
		t.Fatal("ReadFlyClientIP = true, want false (the default: no vendor header is read, authn's fail-closed shape)")
	}
	if cfg.FailSelfServiceProvision != nil {
		t.Fatal("FailSelfServiceProvision armed with an unset APP_FAIL_SELF_SERVICE_PROVISION: an absent variable must leave the self-service provisioning untouched (production behaviour unchanged)")
	}
}

// TestConfigFromEnv_FailSelfServiceProvision_ParseAndDisableSemantics pins
// APP_FAIL_SELF_SERVICE_PROVISION's parse contract (see the
// FailSelfServiceProvision bootstrap field's own doc comment in
// internal/app/bootstrap.go):
// absent or "0" leaves the
// self-service provisioning uninjected, a positive integer N arms an
// injection whose first N provisioning attempts of each account fail and
// whose later attempts of the same account succeed, and anything else
// (not a number, or a negative count) refuses boot with the variable
// named. The regression it pins: an unparsed variable is inert -- every
// value class would behave like the absent one -- so each class below
// must land in a distinct state.
func TestConfigFromEnv_FailSelfServiceProvision_ParseAndDisableSemantics(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")

	t.Run("0 disables the injection", func(t *testing.T) {
		t.Setenv("APP_FAIL_SELF_SERVICE_PROVISION", "0")
		cfg, err := app.ConfigFromEnv()
		if err != nil {
			t.Fatalf("ConfigFromEnv() error = %v", err)
		}
		if cfg.FailSelfServiceProvision != nil {
			t.Fatal("FailSelfServiceProvision armed with the variable at 0, want the disabled default")
		}
	})
	t.Run("a positive count arms the injection with exactly that budget", func(t *testing.T) {
		t.Setenv("APP_FAIL_SELF_SERVICE_PROVISION", "2")
		cfg, err := app.ConfigFromEnv()
		if err != nil {
			t.Fatalf("ConfigFromEnv() error = %v", err)
		}
		if cfg.FailSelfServiceProvision == nil {
			t.Fatal("FailSelfServiceProvision nil with the variable at 2, want the armed injection")
		}
		// The first two provisioning attempts of one account fail...
		if err := cfg.FailSelfServiceProvision("budget-account"); err == nil {
			t.Fatal("the armed injection's first attempt of an account did not fail")
		}
		if err := cfg.FailSelfServiceProvision("budget-account"); err == nil {
			t.Fatal("the armed injection's second attempt of an account did not fail")
		}
		// ...and the third succeeds: the budget is per account, so the
		// count is the number of failed attempts, never a permanent block.
		if err := cfg.FailSelfServiceProvision("budget-account"); err != nil {
			t.Fatalf("the armed injection failed an attempt past its budget: %v", err)
		}
		// A different account starts its own fresh budget of N.
		if err := cfg.FailSelfServiceProvision("another-budget-account"); err == nil {
			t.Fatal("the armed injection's first attempt of a second account did not fail")
		}
	})
	t.Run("not a number refuses boot", func(t *testing.T) {
		t.Setenv("APP_FAIL_SELF_SERVICE_PROVISION", "one")
		_, err := app.ConfigFromEnv()
		if err == nil {
			t.Fatal("ConfigFromEnv() error = nil, want a parse refusal naming APP_FAIL_SELF_SERVICE_PROVISION")
		}
		if !strings.Contains(err.Error(), "APP_FAIL_SELF_SERVICE_PROVISION") {
			t.Fatalf("parse refusal does not name the variable: %v", err)
		}
	})
	t.Run("a negative count refuses boot", func(t *testing.T) {
		t.Setenv("APP_FAIL_SELF_SERVICE_PROVISION", "-1")
		_, err := app.ConfigFromEnv()
		if err == nil {
			t.Fatal("ConfigFromEnv() error = nil, want a refusal of a negative count")
		}
		if !strings.Contains(err.Error(), "APP_FAIL_SELF_SERVICE_PROVISION") {
			t.Fatalf("negative-count refusal does not name the variable: %v", err)
		}
	})
}

// TestConfigFromEnv_ReadsOverrides verifies each environment variable
// ConfigFromEnv reads is actually honored.
func TestConfigFromEnv_ReadsOverrides(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", string(pkgcore.DeploymentModeDistributed))
	t.Setenv("PORT", "9999")
	t.Setenv("APP_DB_PATH", "/tmp/reference-app-configfromenv-test.db")
	t.Setenv("APP_CONFIG_KEY", "0f0e0d0c0b0a090807060504030201001f1e1d1c1b1a19181716151413121110")
	t.Setenv("APP_REDIS_ADDR", "127.0.0.1:6380")
	t.Setenv("APP_OTLP_ENDPOINT", "collector.example.internal:4317")
	t.Setenv("APP_DEMO_USERS_PASSWORD", "env demo seed passphrase")
	t.Setenv("APP_OBJECT_STORE_ROOT", "/var/lib/reference-app/objects")
	t.Setenv("APP_DISABLE_DEMO_USER_HEADER", "1")
	t.Setenv("APP_TRUSTED_PROXIES", " 172.16.0.0/12, 203.0.113.10 , ")
	t.Setenv("APP_READ_FLY_CLIENT_IP", "true")
	t.Setenv("APP_FAIL_SELF_SERVICE_PROVISION", "2")

	cfg, err := app.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.DeploymentMode != pkgcore.DeploymentModeDistributed {
		t.Fatalf("DeploymentMode = %q, want %q", cfg.DeploymentMode, pkgcore.DeploymentModeDistributed)
	}
	if cfg.Port != "9999" {
		t.Fatalf("Port = %q, want %q", cfg.Port, "9999")
	}
	if cfg.SQLitePath != "/tmp/reference-app-configfromenv-test.db" {
		t.Fatalf("SQLitePath = %q, want %q", cfg.SQLitePath, "/tmp/reference-app-configfromenv-test.db")
	}
	if cfg.RedisAddr != "127.0.0.1:6380" {
		t.Fatalf("RedisAddr = %q, want %q", cfg.RedisAddr, "127.0.0.1:6380")
	}
	if cfg.OTLPEndpoint != "collector.example.internal:4317" {
		t.Fatalf("OTLPEndpoint = %q, want the APP_OTLP_ENDPOINT value", cfg.OTLPEndpoint)
	}
	if cfg.DemoUsersPassword != "env demo seed passphrase" {
		t.Fatalf("DemoUsersPassword = %q, want the APP_DEMO_USERS_PASSWORD value", cfg.DemoUsersPassword)
	}
	if cfg.ObjectStoreRoot != "/var/lib/reference-app/objects" {
		t.Fatalf("ObjectStoreRoot = %q, want the APP_OBJECT_STORE_ROOT value", cfg.ObjectStoreRoot)
	}
	if !cfg.DisableDemoUserHeader {
		t.Fatal("DisableDemoUserHeader = false, want true (APP_DISABLE_DEMO_USER_HEADER set to a non-empty value)")
	}
	wantProxies := []string{"172.16.0.0/12", "203.0.113.10"}
	if len(cfg.TrustedProxies) != len(wantProxies) {
		t.Fatalf("TrustedProxies = %v, want %v", cfg.TrustedProxies, wantProxies)
	}
	for i := range wantProxies {
		if cfg.TrustedProxies[i] != wantProxies[i] {
			t.Errorf("TrustedProxies[%d] = %q, want %q", i, cfg.TrustedProxies[i], wantProxies[i])
		}
	}
	if !cfg.ReadFlyClientIP {
		t.Fatal("ReadFlyClientIP = false, want true (APP_READ_FLY_CLIENT_IP set to 'true' alongside the proxy declaration)")
	}
	wantKey := []byte{
		0x0f, 0x0e, 0x0d, 0x0c, 0x0b, 0x0a, 0x09, 0x08,
		0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01, 0x00,
		0x1f, 0x1e, 0x1d, 0x1c, 0x1b, 0x1a, 0x19, 0x18,
		0x17, 0x16, 0x15, 0x14, 0x13, 0x12, 0x11, 0x10,
	}
	if !bytes.Equal(cfg.ConfigKey, wantKey) {
		t.Fatalf("ConfigKey = %x, want the decoded APP_CONFIG_KEY %x", cfg.ConfigKey, wantKey)
	}
	if cfg.FailSelfServiceProvision == nil {
		t.Fatal("FailSelfServiceProvision nil with APP_FAIL_SELF_SERVICE_PROVISION=2, want the armed injection")
	}
	// The variable's value is the number of attempts to fail: with N=2 the
	// first two attempts of one account fail and the third succeeds.
	for attempt := 1; attempt <= 2; attempt++ {
		if err := cfg.FailSelfServiceProvision("override-account"); err == nil {
			t.Fatalf("the armed injection's attempt %d of an account did not fail (N=2)", attempt)
		}
	}
	if err := cfg.FailSelfServiceProvision("override-account"); err != nil {
		t.Fatalf("the armed injection failed an attempt past its two-attempt budget: %v", err)
	}
}

// TestConfigFromEnv_ReadFlyClientIPWithoutTrustedProxies_ReturnsError pins
// the declaration-pair rule the ReadFlyClientIP bootstrap field's own doc
// comment states:
// reading Fly-Client-IP is authorized only for a deployment whose proxy is
// declared, so 'true' with an empty APP_TRUSTED_PROXIES refuses boot --
// the pair would never read the header and would silently keep recording
// the proxy itself, the defect the declaration exists to fix. A value that
// is not a strict bool is refused the same way.
func TestConfigFromEnv_ReadFlyClientIPWithoutTrustedProxies_ReturnsError(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")
	t.Setenv("APP_TRUSTED_PROXIES", "")

	t.Run("true with no proxy declared", func(t *testing.T) {
		t.Setenv("APP_READ_FLY_CLIENT_IP", "true")
		if _, err := app.ConfigFromEnv(); err == nil {
			t.Fatal("ConfigFromEnv() error = nil, want a refusal naming both variables")
		}
	})
	t.Run("not a strict bool", func(t *testing.T) {
		t.Setenv("APP_READ_FLY_CLIENT_IP", "yes")
		if _, err := app.ConfigFromEnv(); err == nil {
			t.Fatal("ConfigFromEnv() error = nil, want a bool-parse refusal")
		}
	})
	t.Run("false with no proxy declared stays accepted", func(t *testing.T) {
		t.Setenv("APP_READ_FLY_CLIENT_IP", "false")
		cfg, err := app.ConfigFromEnv()
		if err != nil {
			t.Fatalf("ConfigFromEnv() error = %v", err)
		}
		if cfg.ReadFlyClientIP {
			t.Fatal("ReadFlyClientIP = true, want false")
		}
	})
}

// TestConfigFromEnv_ObjectStoreRootWithS3_ReturnsError proves the
// "objectstore"-seam ambiguity rule the ObjectStoreRoot bootstrap field's own
// doc comment
// states: a complete APP_S3_* composition and APP_OBJECT_STORE_ROOT both
// name a store for the one seam, so ConfigFromEnv refuses the combination
// loudly -- never by silently preferring one -- while each composition on
// its own stays accepted. TestConfigFromEnv_ReadsOverrides already pins
// the root-alone side; the S3-alone control below is the other half,
// proving the refusal is caused by the combination rather than by the S3
// set itself.
func TestConfigFromEnv_ObjectStoreRootWithS3_ReturnsError(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")

	t.Setenv("APP_S3_ENDPOINT", "https://objects.example.test")
	t.Setenv("APP_S3_BUCKET", "bucket")
	t.Setenv("APP_S3_ACCESS_KEY", "key")
	t.Setenv("APP_S3_SECRET_KEY", "secret")

	t.Setenv("APP_OBJECT_STORE_ROOT", "/var/lib/reference-app/objects")
	if _, err := app.ConfigFromEnv(); err == nil {
		t.Fatal("ConfigFromEnv with both APP_OBJECT_STORE_ROOT and a complete APP_S3_* composition: want error, got nil")
	} else if !strings.Contains(err.Error(), "APP_OBJECT_STORE_ROOT") {
		t.Fatalf("ConfigFromEnv error = %v, want it to name %s", err, "APP_OBJECT_STORE_ROOT")
	}

	t.Setenv("APP_OBJECT_STORE_ROOT", "")
	if _, err := app.ConfigFromEnv(); err != nil {
		t.Fatalf("ConfigFromEnv with the complete S3 composition alone: %v", err)
	}
}

// TestConfigFromEnv_ConfigKeyRejectsMalformedValues proves ConfigFromEnv
// fails configuration loading on a malformed APP_CONFIG_KEY -- too short
// to be a 32-byte key, or not hex at all -- with a precise error, rather
// than letting a subtly wrong key reach dbkit.NewCipher (whose error would
// name only the key size) or, worse, silently sealing values with a key
// the operator did not intend.
func TestConfigFromEnv_ConfigKeyRejectsMalformedValues(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")

	for name, encoded := range map[string]string{
		"too short": "00ff",                                                             // 1 byte, not 32
		"not hex":   "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", // 64 chars, not hex
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("APP_CONFIG_KEY", encoded)
			if _, err := app.ConfigFromEnv(); err == nil {
				t.Fatalf("ConfigFromEnv with APP_CONFIG_KEY=%q: want error, got nil", encoded)
			}
		})
	}
}

// TestConfigFromEnv_InvalidDeploymentMode_ReturnsError proves the
// pkgcore.ParseDeploymentMode error path actually propagates out of
// ConfigFromEnv: an invalid APP_DEPLOYMENT_MODE value must fail
// configuration loading -- and therefore run() in main.go -- rather than
// silently fall back to the standalone default or panic.
func TestConfigFromEnv_InvalidDeploymentMode_ReturnsError(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "not-a-real-deployment-mode")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")

	_, err := app.ConfigFromEnv()
	if err == nil {
		t.Fatal("ConfigFromEnv with APP_DEPLOYMENT_MODE=not-a-real-deployment-mode: want error, got nil")
	}
	if !errors.Is(err, pkgcore.ErrInvalidDeploymentMode) {
		t.Fatalf("ConfigFromEnv error = %v, want it to wrap %v", err, pkgcore.ErrInvalidDeploymentMode)
	}
}

// rootKeyEnvVars lists every environment variable an explicit individual
// key can be set through, in the same order loadHostConfig's own doc comment
// documents the six key materials' three-tier resolution. The root-key tests
// below clear all six before setting APP_ROOT_KEY, so every key is proven to
// resolve through the derivation path with nothing left over from the
// ambient environment.
var rootKeyEnvVars = []string{
	"APP_CONFIG_KEY", "APP_ORG_INDEX_KEY", "APP_NOTIFICATION_INDEX_KEY",
	"APP_PKI_LOCAL_KEY_CIPHER_KEY", "APP_AUTHN_BLIND_INDEX_KEY", "APP_AUTHN_PII_CIPHER_KEY",
}

// clearRootKeyOverrides sets every one of rootKeyEnvVars to "" via
// t.Setenv, so a root-key test's outcome depends only on APP_ROOT_KEY
// and never on an individual override left set by a previous test or the
// ambient shell.
func clearRootKeyOverrides(t *testing.T) {
	t.Helper()
	for _, env := range rootKeyEnvVars {
		t.Setenv(env, "")
	}
}

// TestConfigFromEnv_RootKey_DerivesAllSixKeys proves APP_ROOT_KEY alone
// -- no individual key env var set -- derives every one of the six key
// materials loadHostConfig's own doc comment documents, and that
// ConfigFromEnv's derivation matches the platform composition
// (pkgcore.BootstrapKeyPurpose over the declared key path, then
// dbkit.DeriveKey over the root): not merely "some non-default bytes landed
// in cfg", but the exact key a caller who knew the root and the declared
// path could reproduce independently through the platform API.
func TestConfigFromEnv_RootKey_DerivesAllSixKeys(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")
	clearRootKeyOverrides(t)

	rootKey := sha256.Sum256([]byte("TestConfigFromEnv_RootKey_DerivesAllSixKeys root secret"))
	t.Setenv("APP_ROOT_KEY", hex.EncodeToString(rootKey[:]))

	cfg, err := app.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}

	for _, tt := range []struct {
		name    string
		got     []byte
		keyPath string
	}{
		{"ConfigKey", cfg.ConfigKey, "config.cipher_key"},
		{"OrgIndexKey", cfg.OrgIndexKey, "org.invitation_email_index_key"},
		{"NotificationIndexKey", cfg.NotificationIndexKey, "notification.contact_index_key"},
		{"PKILocalKeyCipherKey", cfg.PKILocalKeyCipherKey, "pki.local_key_cipher_key"},
		{"AuthnBlindIndexKey", cfg.AuthnBlindIndexKey, "authn.blind_index_key"},
		{"AuthnPIICipherKey", cfg.AuthnPIICipherKey, "authn.pii_cipher_key"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := deriveDeclaredKeyMaterial(t, rootKey[:], tt.keyPath)
			if !bytes.Equal(tt.got, want) {
				t.Fatalf("cfg.%s = %x, want the composed derivation over %q = %x", tt.name, tt.got, tt.keyPath, want)
			}
		})
	}

	// The six derived keys must also be pairwise distinct -- a purpose
	// string collision (or a loadHostConfig wiring mistake reusing one
	// derived value for two fields) would silently reintroduce the exact
	// key-material reuse dbkit's key-separation rule forbids.
	keys := map[string][]byte{
		"ConfigKey": cfg.ConfigKey, "OrgIndexKey": cfg.OrgIndexKey,
		"NotificationIndexKey": cfg.NotificationIndexKey, "PKILocalKeyCipherKey": cfg.PKILocalKeyCipherKey,
		"AuthnBlindIndexKey": cfg.AuthnBlindIndexKey, "AuthnPIICipherKey": cfg.AuthnPIICipherKey,
	}
	seen := make(map[string]string, len(keys))
	for name, key := range keys {
		digest := hex.EncodeToString(key)
		if other, ok := seen[digest]; ok {
			t.Fatalf("%s and %s derived to the identical key %s", name, other, digest)
		}
		seen[digest] = name
	}
}

// TestConfigFromEnv_RootKey_IndividualOverrideWins proves the precedence
// order loadHostConfig's own doc comment states: with both APP_ROOT_KEY and
// one individual key env var (APP_CONFIG_KEY) set, the explicit
// individual value wins for that one key, while every other key still
// resolves through the root-key derivation -- the "power users can still
// override any single one" half of the design.
func TestConfigFromEnv_RootKey_IndividualOverrideWins(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")
	clearRootKeyOverrides(t)

	rootKey := sha256.Sum256([]byte("TestConfigFromEnv_RootKey_IndividualOverrideWins root secret"))
	t.Setenv("APP_ROOT_KEY", hex.EncodeToString(rootKey[:]))

	explicitConfigKey := sha256.Sum256([]byte("TestConfigFromEnv_RootKey_IndividualOverrideWins explicit APP_CONFIG_KEY"))
	t.Setenv("APP_CONFIG_KEY", hex.EncodeToString(explicitConfigKey[:]))

	cfg, err := app.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}

	if !bytes.Equal(cfg.ConfigKey, explicitConfigKey[:]) {
		t.Fatalf("cfg.ConfigKey = %x, want the explicit %s value %x (it must win over the APP_ROOT_KEY derivation)",
			cfg.ConfigKey, "APP_CONFIG_KEY", explicitConfigKey[:])
	}

	wantOrgIndexKey := deriveDeclaredKeyMaterial(t, rootKey[:], "org.invitation_email_index_key")
	if !bytes.Equal(cfg.OrgIndexKey, wantOrgIndexKey) {
		t.Fatalf("cfg.OrgIndexKey = %x, want it to still resolve through the APP_ROOT_KEY derivation (%x) since %s was never set",
			cfg.OrgIndexKey, wantOrgIndexKey, "APP_ORG_INDEX_KEY")
	}
}

// deriveDeclaredKeyMaterial builds an expectation by composing the two
// platform contracts a host derives through -- the declared key path's
// purpose string (pkgcore.BootstrapKeyPurpose), then the 32-byte material
// under it (dbkit.DeriveKey) -- so the assertions above and below compare
// against the exact bytes a caller who knew the root and the declared path
// could reproduce independently, never against bytes copied from the app's
// own resolution.
func deriveDeclaredKeyMaterial(t *testing.T, rootKey []byte, keyPath string) []byte {
	t.Helper()

	purpose, err := pkgcore.BootstrapKeyPurpose(keyPath)
	if err != nil {
		t.Fatalf("pkgcore.BootstrapKeyPurpose(%q): %v", keyPath, err)
	}
	derived, err := dbkit.DeriveKey(rootKey, purpose)
	if err != nil {
		t.Fatalf("dbkit.DeriveKey(rootKey, %q): %v", purpose, err)
	}
	return derived
}

// TestDeclaredBootstrapKeys_ReconcileWithResolvedKeyMaterial reconciles the
// transform's derivation call sites against the declarations: it boots a
// kernel over the modules that declare the six hexkey bootstrap keys, then
// requires every declared key's material -- derived from APP_ROOT_KEY
// through the platform API under the module's own declared path -- to be
// exactly the bytes the resolved ServerConfig field carries. The
// declaration is the oracle, so a call site whose path literal diverges
// from what its module declares (a misspelling, a stale name) derives
// under a path no module declared and fails here naming the key.
func TestDeclaredBootstrapKeys_ReconcileWithResolvedKeyMaterial(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")
	clearRootKeyOverrides(t)

	rootKey := sha256.Sum256([]byte("TestDeclaredBootstrapKeys_ReconcileWithResolvedKeyMaterial root secret"))
	t.Setenv("APP_ROOT_KEY", hex.EncodeToString(rootKey[:]))

	cfg, err := app.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}

	// The resolved material per declared key path, keyed by the path
	// spelling the declaration uses.
	resolved := map[string][]byte{
		"config.cipher_key":              cfg.ConfigKey,
		"org.invitation_email_index_key": cfg.OrgIndexKey,
		"notification.contact_index_key": cfg.NotificationIndexKey,
		"pki.local_key_cipher_key":       cfg.PKILocalKeyCipherKey,
		"authn.blind_index_key":          cfg.AuthnBlindIndexKey,
		"authn.pii_cipher_key":           cfg.AuthnPIICipherKey,
	}

	reg := newDeclaringModuleRegistry(t)
	declared := 0
	for _, key := range reg.Bootstrap.Keys() {
		if key.Format != "hexkey" {
			continue
		}
		declared++
		got, mapped := resolved[key.Key]
		if !mapped {
			t.Fatalf("a module declares the hexkey bootstrap key %q and this app resolves no key material for it", key.Key)
		}
		want := deriveDeclaredKeyMaterial(t, rootKey[:], key.Key)
		if !bytes.Equal(got, want) {
			t.Fatalf("resolved material for the declared key %q = %x, want the composed derivation over %q = %x: the derivation call site must use the key path the module declared",
				key.Key, got, key.Key, want)
		}
	}
	if declared != len(resolved) {
		t.Fatalf("the booted registry declared %d hexkey bootstrap keys, want the %d this app derives", declared, len(resolved))
	}
}

// newDeclaringModuleRegistry boots a kernel over the module types that
// declare the six hexkey bootstrap keys -- the same five types BuildServer
// composes -- so their declarations are reachable the way the running app's
// own registry carries them. Each module's required seams are wired with
// recognizable test placeholders, the way BuildServer wires the real ones;
// the reconciliation exercises none of them, it only reads the declarations
// Register contributes.
func newDeclaringModuleRegistry(t *testing.T) *pkgcore.Registry {
	t.Helper()
	ctx := context.Background()
	db := dbtest.NewSQLite(t)

	pkiModule := pki.NewModule(db)
	t.Cleanup(func() {
		if closeErr := pkiModule.Close(); closeErr != nil {
			t.Errorf("close pki module: %v", closeErr)
		}
	})

	authnModule, err := authn.NewModule(db,
		authn.WithKeySource(pkiModule.Service()),
		authn.WithBlindIndexKey(bytes.Repeat([]byte{0x11}, 32)),
	)
	if err != nil {
		t.Fatalf("authn.NewModule: %v", err)
	}

	orgIndexer, err := dbkit.NewBlindIndexer(org.EmailIndexColumn, bytes.Repeat([]byte{0x22}, 32), dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer(org.EmailIndexColumn): %v", err)
	}
	contactEmailIndexer, err := dbkit.NewBlindIndexer(notification.AddressIndexColumn, bytes.Repeat([]byte{0x33}, 32), dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer(notification.AddressIndexColumn): %v", err)
	}
	contactPhoneIndexer, err := dbkit.NewBlindIndexer(notification.AddressIndexColumn, bytes.Repeat([]byte{0x33}, 32), dbkit.NormalizePhoneE164)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer(notification.AddressIndexColumn, phone): %v", err)
	}

	deliveryQueue := jobs.NewStandaloneQueue(db)
	t.Cleanup(func() {
		if closeErr := deliveryQueue.Close(ctx); closeErr != nil {
			t.Errorf("close delivery queue: %v", closeErr)
		}
	})

	reg, err := pkgcore.NewKernel().Bootstrap(ctx,
		config.NewModule(db),
		authnModule,
		org.NewModule(db, org.WithEmailIndexer(orgIndexer), org.WithInvitationEmailDisabled()),
		notification.NewModule(db,
			notification.WithSMSSender(pkgcore.NewConsoleSMSSender(io.Discard)),
			notification.WithMailFrom("notifications@example.test"),
			notification.WithContactEmailIndexer(contactEmailIndexer),
			notification.WithContactPhoneIndexer(contactPhoneIndexer),
			notification.WithDeliveryQueue(deliveryQueue),
			notification.WithUserAddressResolver(noAddressesResolver{}),
		),
		pkiModule,
	)
	if err != nil {
		t.Fatalf("bootstrap the modules declaring the six hexkey keys: %v", err)
	}
	return reg
}

// noAddressesResolver satisfies the user-address seam notification's
// Register requires; the reconciliation never dispatches a notification, so
// every user simply has no address on file.
type noAddressesResolver struct{}

// Resolve implements notification.UserAddressResolver.
func (noAddressesResolver) Resolve(context.Context, string) (notification.UserAddresses, error) {
	return notification.UserAddresses{}, nil
}

// TestBuildServer_RootKeyAlone_AllSixDerivedKeysWorkForTheirRealPurpose is
// the end-to-end proof: APP_ROOT_KEY set alone (every
// individual key env var cleared), ConfigFromEnv resolves all six key
// materials through the composed platform derivation
// (pkgcore.BootstrapKeyPurpose + dbkit.DeriveKey), BuildServer boots a
// real composed server from the result, and every one of the six derived keys is
// exercised through the real mechanism it protects -- never merely "no
// error from NewCipher/NewBlindIndexer".
//
// ConfigKey, OrgIndexKey and NotificationIndexKey are proven with a real
// encrypt/decrypt or Index/Equal round trip through the exact dbkit
// primitive (and, for the two blind-index keys, the same normalizer and
// column-name argument BuildServer itself wires them with -- org's over
// the org.EmailIndexColumn constant, notification's over the
// notification.AddressIndexColumn constant, each referenced by this
// file's replicas and internal/app/server.go's call sites alike so neither can drift
// apart from the wiring; Equal's returned column is never executed
// against a database here, which is exactly why each module's own suite
// pins its constant to the real migrated column
// (go/org/email_index_column_drift_test.go,
// go/notification/address_index_column_test.go)).
// PKILocalKeyCipherKey, AuthnBlindIndexKey and
// AuthnPIICipherKey are proven together by a real register-then-login
// round trip through the actual composed HTTP stack: registration
// encrypts the new user's email under AuthnPIICipherKey and blind-indexes
// it under AuthnBlindIndexKey, and login can only succeed if the very
// same derived AuthnBlindIndexKey both wrote and reads back that index
// value -- while the returned, verified access token proves
// PKILocalKeyCipherKey correctly round-tripped pki's persisted signing
// key well enough to both mint and verify a real EdDSA-signed token.
func TestBuildServer_RootKeyAlone_AllSixDerivedKeysWorkForTheirRealPurpose(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "0")
	t.Setenv("APP_DB_PATH", filepath.Join(t.TempDir(), "reference-app-rootkey-e2e-test.db"))
	t.Setenv("APP_REDIS_ADDR", "")
	t.Setenv("APP_DEMO_USERS_PASSWORD", "")
	clearRootKeyOverrides(t)

	rootKey := sha256.Sum256([]byte("TestBuildServer_RootKeyAlone root secret"))
	t.Setenv("APP_ROOT_KEY", hex.EncodeToString(rootKey[:]))

	cfg, err := app.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	cfg.HostTenants = app.DemoHostTenants
	cfg.Memberships = app.NewSignInMemberships()

	// ConfigKey: the exact mechanism go/config's Sensitive values are
	// sealed with (config.WithCipher over dbkit.NewCipher, per
	// see go/config's own docs) -- a real Encrypt/Decrypt round trip under the
	// derived key.
	configCipher, err := dbkit.NewCipher(cfg.ConfigKey)
	if err != nil {
		t.Fatalf("dbkit.NewCipher(cfg.ConfigKey): %v", err)
	}
	const configPlaintext = "sensitive config value protected by the derived ConfigKey"
	ciphertext, err := configCipher.Encrypt([]byte(configPlaintext))
	if err != nil {
		t.Fatalf("configCipher.Encrypt: %v", err)
	}
	decrypted, err := configCipher.Decrypt(ciphertext)
	if err != nil {
		t.Fatalf("configCipher.Decrypt: %v", err)
	}
	if string(decrypted) != configPlaintext {
		t.Fatalf("configCipher round trip = %q, want %q", decrypted, configPlaintext)
	}

	// OrgIndexKey and NotificationIndexKey: the exact BlindIndexer
	// construction (column name and normalizer included) BuildServer
	// itself wires org.WithEmailIndexer and the notification contact
	// indexers from -- a real Index/Equal round trip under each derived
	// key.
	orgIndexer, err := dbkit.NewBlindIndexer(org.EmailIndexColumn, cfg.OrgIndexKey, dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer(org.EmailIndexColumn): %v", err)
	}
	assertBlindIndexRoundTrip(t, orgIndexer, "invitee@example.com")

	contactEmailIndexer, err := dbkit.NewBlindIndexer(notification.AddressIndexColumn, cfg.NotificationIndexKey, dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer(notification.AddressIndexColumn): %v", err)
	}
	assertBlindIndexRoundTrip(t, contactEmailIndexer, "contact@example.com")

	contactPhoneIndexer, err := dbkit.NewBlindIndexer(notification.AddressIndexColumn, cfg.NotificationIndexKey, dbkit.NormalizePhoneE164)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer(notification.AddressIndexColumn): %v", err)
	}
	assertBlindIndexRoundTrip(t, contactPhoneIndexer, "+15550100")

	// PKILocalKeyCipherKey, AuthnBlindIndexKey and AuthnPIICipherKey,
	// together: boot the real composed server and drive a real
	// register-then-login round trip through it.
	handler, cleanup, _, err := app.BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer with APP_ROOT_KEY alone: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("cleanup: %v", cleanupErr)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "root-key-proof")
	if token == "" {
		t.Fatal("registerAndAuthenticate returned an empty access token")
	}
}

// assertBlindIndexRoundTrip proves indexer genuinely functions as a blind
// index: Index(raw) is deterministic, and Equal(raw)'s returned condition
// carries exactly the value Index(raw) computed -- the write side and the
// query side agreeing is what makes "WHERE <column> = ?" actually find the
// row Index wrote, the real purpose a blind-index key exists to serve.
func assertBlindIndexRoundTrip(t *testing.T, indexer *dbkit.BlindIndexer, raw string) {
	t.Helper()

	indexed, err := indexer.Index(raw)
	if err != nil {
		t.Fatalf("Index(%q): %v", raw, err)
	}
	if indexed == "" {
		t.Fatalf("Index(%q) returned an empty index", raw)
	}

	cond, err := indexer.Equal(raw)
	if err != nil {
		t.Fatalf("Equal(%q): %v", raw, err)
	}
	if cond.Value != indexed {
		t.Fatalf("Equal(%q) condition value = %v, want it to match Index(%q) = %q", raw, cond.Value, raw, indexed)
	}
}

// TestSocialChannelFlagKey_MapsEveryGatedProvider pins the host-side
// mirror of authn's channel-flag mapping (internal/app/server.go's
// SocialChannelFlagKey doc comment): every provider authn gates maps to
// that provider's own feature-flag key -- the very key
// openConfiguredAuthnChannels writes true at boot -- and a provider
// authn does not gate maps to "", so the boot-time channel-opening loop
// skips it instead of writing an unknown config key.
func TestSocialChannelFlagKey_MapsEveryGatedProvider(t *testing.T) {
	cases := map[string]string{
		authn.ProviderGoogle:       authn.FeatureFlagSocialGoogle,
		authn.ProviderGitHub:       authn.FeatureFlagSocialGitHub,
		authn.ProviderWeChat:       authn.FeatureFlagSocialWeChat,
		authn.ProviderDingTalk:     authn.FeatureFlagSocialDingTalk,
		authn.ProviderFeishu:       authn.FeatureFlagSocialFeishu,
		"authn.provider.not_gated": "",
		"":                         "",
	}
	for name, want := range cases {
		if got := app.SocialChannelFlagKey(name); got != want {
			t.Errorf("SocialChannelFlagKey(%q) = %q, want %q", name, got, want)
		}
	}
}

// TestOrgFeatureGate_NotAttachedYet_FailsClosed pins OrgFeatureGate's
// ordering safety net directly: before BuildServer's configModule.Attach
// has filled the service pointer (or when the gate outlives the variable
// it points at), IsEnabled answers a coded error rather than panicking on
// a nil *config.Service -- the failure mode the **service-pointer
// indirection exists to avoid (internal/app/server.go's own doc comment).
func TestOrgFeatureGate_NotAttachedYet_FailsClosed(t *testing.T) {
	var svc *config.Service
	gate := app.OrgFeatureGate{Service: &svc}
	enabled, err := gate.IsEnabled(context.Background(), "any.key")
	if err == nil {
		t.Fatal("IsEnabled before attach error = nil, want the not-attached-yet error")
	}
	if enabled {
		t.Fatal("IsEnabled before attach = true, want false")
	}
}

// TestConfigFromEnv_PartialInfrastructureCompositionsAreRefused pins the
// partial-composition refusals of the optional mail/S3/object-store
// wiring: an SMTP composition with only one of its two variables set, an
// S3 composition with some but not all of its four variables set, and
// non-parseable boolean/port values each refuse boot naming the variable
// -- never a half-composed seam silently falling back to the in-process
// default, which would hide the misconfiguration until first use.
func TestConfigFromEnv_PartialInfrastructureCompositionsAreRefused(t *testing.T) {
	testutil.ClearBootstrapEnv(t)

	cases := []struct {
		name   string
		env    map[string]string
		wantIn string
	}{
		{
			name:   "smtp_host_without_port",
			env:    map[string]string{"APP_SMTP_HOST": "smtp.example.com"},
			wantIn: "APP_SMTP_PORT",
		},
		{
			name:   "smtp_port_without_host",
			env:    map[string]string{"APP_SMTP_PORT": "25"},
			wantIn: "APP_SMTP_HOST",
		},
		{
			name:   "smtp_port_not_a_number",
			env:    map[string]string{"APP_SMTP_HOST": "smtp.example.com", "APP_SMTP_PORT": "not-a-port"},
			wantIn: "APP_SMTP_PORT",
		},
		{
			name:   "s3_partial_set",
			env:    map[string]string{"APP_S3_ENDPOINT": "http://s3.example.com"},
			wantIn: "APP_S3_BUCKET",
		},
		{
			name: "s3_use_ssl_not_a_bool",
			env: map[string]string{
				"APP_S3_ENDPOINT": "http://s3.example.com", "APP_S3_BUCKET": "b",
				"APP_S3_ACCESS_KEY": "k", "APP_S3_SECRET_KEY": "s", "APP_S3_USE_SSL": "sometimes",
			},
			wantIn: "APP_S3_USE_SSL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			_, err := app.ConfigFromEnv()
			if err == nil {
				t.Fatal("ConfigFromEnv succeeded, want the partial-composition refusal")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantIn)
			}
		})
	}
}

// TestConfigFromEnv_CompleteSMTPComposition_ResolvesAMailer pins the
// success half of the SMTP wiring: with both variables set and a
// parseable port, ConfigFromEnv resolves a real SMTP Mailer carrying the
// host and the credentials -- the composition the config's Mailer field
// and MailerCapabilities hand to Kernel.Bootstrap.
func TestConfigFromEnv_CompleteSMTPComposition_ResolvesAMailer(t *testing.T) {
	testutil.ClearBootstrapEnv(t)
	t.Setenv("APP_SMTP_HOST", "smtp.example.com")
	t.Setenv("APP_SMTP_PORT", "587")
	t.Setenv("APP_SMTP_USERNAME", "mailer@example.com")
	t.Setenv("APP_SMTP_PASSWORD", "smtp-secret")

	cfg, err := app.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Mailer == nil {
		t.Fatal("Mailer = nil, want the resolved SMTP mailer")
	}
	if cfg.MailerCapabilities&pkgcore.SurvivesRestart == 0 {
		t.Error("MailerCapabilities lacks SurvivesRestart, want the capability-honest SMTP declaration")
	}
}

// TestConfigFromEnv_MalformedIndividualKeysAreRefused pins the
// individual-override parse for the five key materials beyond
// APP_CONFIG_KEY (whose malformed values TestConfigFromEnv_ConfigKeyRejectsMalformedValues
// already covers): a malformed individual variable must refuse boot
// naming the variable, never fall through to the dev default silently.
func TestConfigFromEnv_MalformedIndividualKeysAreRefused(t *testing.T) {
	testutil.ClearBootstrapEnv(t)

	for _, key := range []string{
		"APP_ORG_INDEX_KEY", "APP_NOTIFICATION_INDEX_KEY", "APP_PKI_LOCAL_KEY_CIPHER_KEY",
		"APP_AUTHN_BLIND_INDEX_KEY", "APP_AUTHN_PII_CIPHER_KEY",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "not-64-hex-chars")
			_, err := app.ConfigFromEnv()
			if err == nil {
				t.Fatalf("ConfigFromEnv with a malformed %s succeeded, want the refusal", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error = %q, want it to name %q", err, key)
			}
		})
	}
}
