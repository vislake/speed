package flowtests

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
	"github.com/vislake/speed/examples/reference-app/internal/testutil"
	speedapp "github.com/vislake/speed/go/app"
	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
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
	material, err := resolveDeclaredMaterial(t)
	if err != nil {
		t.Fatalf("resolve the declared key material: %v", err)
	}
	if got := declaredMaterialKey(t, material, "config.cipher_key"); !bytes.Equal(got, app.DevConfigKey) {
		t.Fatalf("config.cipher_key material = %x, want the dev default %x", got, app.DevConfigKey)
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
		t.Fatalf("ObjectStoreRoot = %q, want the empty default (builtin local-store directory)", cfg.ObjectStoreRoot)
	}
	if cfg.DisableDemoUserHeader {
		t.Fatal("DisableDemoUserHeader = true, want false (the default: demo.DemoUserHeader keeps winning, unchanged)")
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
	t.Setenv("APP_CONFIG__CIPHER_KEY", "0f0e0d0c0b0a090807060504030201001f1e1d1c1b1a19181716151413121110")
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
	material, err := resolveDeclaredMaterial(t)
	if err != nil {
		t.Fatalf("resolve the declared key material: %v", err)
	}
	if got := declaredMaterialKey(t, material, "config.cipher_key"); !bytes.Equal(got, wantKey) {
		t.Fatalf("config.cipher_key material = %x, want the decoded APP_CONFIG__CIPHER_KEY %x", got, wantKey)
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

// resolveDeclaredMaterial runs the assembly's declared-key resolution over
// the process environment: the same loader options a boot's LoadSpec carries
// (the APP_ prefix, the APP_ROOT_KEY source, the platform derivation and the
// app's documented development defaults), against the components this test
// binary's imports register. It is the read side the key-material tests
// compare through.
func resolveDeclaredMaterial(t *testing.T) (*pkgcore.BootstrapMaterial, error) {
	t.Helper()

	var host struct{}
	reg := pkgcore.NewComponentRegistry()
	err := speedapp.Load(context.Background(), reg, speedapp.LoadSpec{
		Host: &host,
		Options: []speedapp.ConfigOption{
			speedapp.ConfigEnvPrefix("APP_"),
			speedapp.ConfigRootKeyEnv("APP_ROOT_KEY"),
			speedapp.ConfigKeyDerivation(dbkit.DeriveBootstrapKey),
			speedapp.ConfigDevDefaults(app.BootstrapDevDefaults()),
		},
		Args: []string{},
	})
	if err != nil {
		return nil, err
	}
	return pkgcore.BootstrapMaterialOf(reg)
}

// declaredMaterialKey reads one declared key's material out of a resolved
// source, failing the test when the assembly resolved no value for it.
func declaredMaterialKey(t *testing.T, material *pkgcore.BootstrapMaterial, keyPath string) []byte {
	t.Helper()
	value, ok := material.Material(keyPath)
	if !ok {
		t.Fatalf("the assembly resolved no material for the declared bootstrap key %q", keyPath)
	}
	return value
}

// TestDeclaredMaterial_ConfigKeyRejectsMalformedValues proves the assembly's
// declared-key resolution fails on a malformed config.cipher_key value (read
// from APP_CONFIG__CIPHER_KEY) -- too short to be a 32-byte key, or not hex
// at all -- with a precise error, rather than letting a subtly wrong key
// reach dbkit.NewCipher (whose error would name only the key size) or, worse,
// silently sealing values with a key the operator did not intend.
func TestDeclaredMaterial_ConfigKeyRejectsMalformedValues(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")

	for name, encoded := range map[string]string{
		"too short": "00ff",                                                             // 1 byte, not 32
		"not hex":   "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz", // 64 chars, not hex
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("APP_CONFIG__CIPHER_KEY", encoded)
			if _, err := resolveDeclaredMaterial(t); err == nil {
				t.Fatalf("resolving the declared key material with APP_CONFIG__CIPHER_KEY=%q: want error, got nil", encoded)
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
// key can be set through -- the loader's own derivation from each declared
// key path (the dot between the path's two segments spells as a double
// underscore) -- in the same order loadHostConfig's own doc comment
// documents the six key materials' three-tier resolution. The root-key tests
// below clear all six before setting APP_ROOT_KEY, so every key is proven to
// resolve through the derivation path with nothing left over from the
// ambient environment.
var rootKeyEnvVars = []string{
	"APP_CONFIG__CIPHER_KEY", "APP_ORG__INVITATION_EMAIL_INDEX_KEY", "APP_NOTIFICATION__CONTACT_INDEX_KEY",
	"APP_PKI__LOCAL_KEY_CIPHER_KEY", "APP_AUTHN__BLIND_INDEX_KEY", "APP_AUTHN__PII_CIPHER_KEY",
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

// TestDeclaredMaterial_RootKey_DerivesAllSixKeys proves APP_ROOT_KEY alone
// -- no individual key env var set -- derives every one of the six declared
// key materials a boot resolves, and that the assembly's derivation matches
// the platform composition (pkgcore.BootstrapKeyPurpose over the declared key
// path, then dbkit.DeriveKey over the root): not merely "some non-default
// bytes landed in the material", but the exact key a caller who knew the root
// and the declared path could reproduce independently through the platform
// API.
func TestDeclaredMaterial_RootKey_DerivesAllSixKeys(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")
	clearRootKeyOverrides(t)

	rootKey := sha256.Sum256([]byte("TestDeclaredMaterial_RootKey_DerivesAllSixKeys root secret"))
	t.Setenv("APP_ROOT_KEY", hex.EncodeToString(rootKey[:]))

	if _, err := app.ConfigFromEnv(); err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	material, err := resolveDeclaredMaterial(t)
	if err != nil {
		t.Fatalf("resolve the declared key material: %v", err)
	}

	for _, keyPath := range []string{
		"config.cipher_key",
		"org.invitation_email_index_key",
		"notification.contact_index_key",
		"pki.local_key_cipher_key",
		"authn.blind_index_key",
		"authn.pii_cipher_key",
	} {
		t.Run(keyPath, func(t *testing.T) {
			got := declaredMaterialKey(t, material, keyPath)
			want := deriveDeclaredKeyMaterial(t, rootKey[:], keyPath)
			if !bytes.Equal(got, want) {
				t.Fatalf("material at %q = %x, want the composed derivation %x", keyPath, got, want)
			}
		})
	}
}

// TestDeclaredMaterial_RootKey_IndividualOverrideWins proves the precedence
// order the loader applies: with both APP_ROOT_KEY and one individual key env
// var (APP_CONFIG__CIPHER_KEY) set, the explicit individual value wins for
// that one key, while every other key still resolves through the root-key
// derivation -- the "power users can still override any single one" half of
// the design.
func TestDeclaredMaterial_RootKey_IndividualOverrideWins(t *testing.T) {
	t.Setenv("APP_DEPLOYMENT_MODE", "")
	t.Setenv("PORT", "")
	t.Setenv("APP_DB_PATH", "")
	clearRootKeyOverrides(t)

	rootKey := sha256.Sum256([]byte("TestDeclaredMaterial_RootKey_IndividualOverrideWins root secret"))
	t.Setenv("APP_ROOT_KEY", hex.EncodeToString(rootKey[:]))

	explicitConfigKey := sha256.Sum256([]byte("TestDeclaredMaterial_RootKey_IndividualOverrideWins explicit APP_CONFIG__CIPHER_KEY"))
	t.Setenv("APP_CONFIG__CIPHER_KEY", hex.EncodeToString(explicitConfigKey[:]))

	if _, err := app.ConfigFromEnv(); err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	material, err := resolveDeclaredMaterial(t)
	if err != nil {
		t.Fatalf("resolve the declared key material: %v", err)
	}

	if got := declaredMaterialKey(t, material, "config.cipher_key"); !bytes.Equal(got, explicitConfigKey[:]) {
		t.Fatalf("config.cipher_key material = %x, want the explicit APP_CONFIG__CIPHER_KEY value %x (it must win over the APP_ROOT_KEY derivation)",
			got, explicitConfigKey[:])
	}

	wantOrgIndexKey := deriveDeclaredKeyMaterial(t, rootKey[:], "org.invitation_email_index_key")
	if got := declaredMaterialKey(t, material, "org.invitation_email_index_key"); !bytes.Equal(got, wantOrgIndexKey) {
		t.Fatalf("org.invitation_email_index_key material = %x, want it to still resolve through the APP_ROOT_KEY derivation (%x) since APP_ORG__INVITATION_EMAIL_INDEX_KEY was never set",
			got, wantOrgIndexKey)
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

// TestBuildServer_RootKeyAlone_AllSixDerivedKeysWorkForTheirRealPurpose is
// the end-to-end proof: APP_ROOT_KEY set alone (every individual key env var
// cleared), the assembly's declared-key resolution derives all six key
// materials through the composed platform derivation
// (pkgcore.BootstrapKeyPurpose + dbkit.DeriveKey), BuildServer boots a real
// composed server over the result, and every one of the six derived keys is
// exercised through the real mechanism it protects -- never merely "no error
// from NewCipher/NewBlindIndexer".
//
// config.cipher_key, org.invitation_email_index_key and
// notification.contact_index_key are proven with a real encrypt/decrypt or
// Index/Equal round trip through the exact dbkit primitive (and, for the two
// blind-index keys, the same normalizer and column-name argument BuildServer
// itself wires them with -- org's over the org.EmailIndexColumn constant,
// notification's over the notification.AddressIndexColumn constant, each
// referenced by this file's replicas and internal/app/modules.go's call
// sites alike so neither can drift apart from the wiring; Equal's returned
// column is never executed against a database here, which is exactly why
// each module's own suite pins its constant to the real migrated column
// (go/org/email_index_column_drift_test.go,
// go/notification/address_index_column_test.go)).
// pki.local_key_cipher_key, authn.blind_index_key and authn.pii_cipher_key
// are proven together by a real register-then-login round trip through the
// actual composed HTTP stack: registration encrypts the new user's email
// under authn.pii_cipher_key and blind-indexes it under
// authn.blind_index_key, and login can only succeed if the very same derived
// authn.blind_index_key both wrote and reads back that index value -- while
// the returned, verified access token proves pki.local_key_cipher_key
// correctly round-tripped pki's persisted signing key well enough to both
// mint and verify a real EdDSA-signed token.
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
	cfg.HostTenants = demo.DemoHostTenants
	cfg.Memberships = app.NewSignInMemberships()

	material, err := resolveDeclaredMaterial(t)
	if err != nil {
		t.Fatalf("resolve the declared key material: %v", err)
	}

	// config.cipher_key: the exact mechanism go/config's Sensitive values
	// are sealed with (config.WithCipher over dbkit.NewCipher, per
	// see go/config's own docs) -- a real Encrypt/Decrypt round trip under the
	// derived key.
	configCipher, err := dbkit.NewCipher(declaredMaterialKey(t, material, "config.cipher_key"))
	if err != nil {
		t.Fatalf("dbkit.NewCipher(config.cipher_key material): %v", err)
	}
	const configPlaintext = "sensitive config value protected by the derived config.cipher_key"
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

	// org.invitation_email_index_key and notification.contact_index_key:
	// the exact BlindIndexer construction (column name and normalizer
	// included) BuildServer itself wires org.WithEmailIndexer and the
	// notification contact indexers from -- a real Index/Equal round trip
	// under each derived key.
	orgIndexer, err := dbkit.NewBlindIndexer(org.EmailIndexColumn, declaredMaterialKey(t, material, "org.invitation_email_index_key"), dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer(org.EmailIndexColumn): %v", err)
	}
	assertBlindIndexRoundTrip(t, orgIndexer, "invitee@example.com")

	contactIndexKey := declaredMaterialKey(t, material, "notification.contact_index_key")
	contactEmailIndexer, err := dbkit.NewBlindIndexer(notification.AddressIndexColumn, contactIndexKey, dbkit.NormalizeEmail)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer(notification.AddressIndexColumn): %v", err)
	}
	assertBlindIndexRoundTrip(t, contactEmailIndexer, "contact@example.com")

	contactPhoneIndexer, err := dbkit.NewBlindIndexer(notification.AddressIndexColumn, contactIndexKey, dbkit.NormalizePhoneE164)
	if err != nil {
		t.Fatalf("dbkit.NewBlindIndexer(notification.AddressIndexColumn): %v", err)
	}
	assertBlindIndexRoundTrip(t, contactPhoneIndexer, "+15550100")

	// pki.local_key_cipher_key, authn.blind_index_key and
	// authn.pii_cipher_key, together: boot the real composed server and
	// drive a real register-then-login round trip through it.
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

// TestHostFeatureGate_BeforeConfigAttach_FailsClosed pins the host's gate
// wiring as the host composes it: BuildServer adapts the config module's
// lazy handle into org's and authn's FeatureGate seams
// (org.FeatureGateFunc(configHandle.IsEnabled) and its authn twin,
// internal/app/server.go), so before configModule.Attach has run the gate
// must answer the config module's coded not-attached refusal rather than
// panicking or fabricating a default.
func TestHostFeatureGate_BeforeConfigAttach_FailsClosed(t *testing.T) {
	handle := config.NewModule(nil).Handle()

	// The same handle read, adapted through each module's own
	// identically shaped seam -- exactly what BuildServer wires.
	gates := map[string]interface {
		IsEnabled(ctx context.Context, key string) (bool, error)
	}{
		"org":   org.FeatureGateFunc(handle.IsEnabled),
		"authn": authn.FeatureGateFunc(handle.IsEnabled),
	}

	for name, gate := range gates {
		enabled, err := gate.IsEnabled(context.Background(), "any.key")
		if err == nil {
			t.Fatalf("%s gate: IsEnabled before config attach error = nil, want the not-attached refusal", name)
		}
		if !apperr.HasCode(err, config.ErrServiceNotAttached.Code) {
			t.Fatalf("%s gate: IsEnabled before config attach error = %v, want code %q", name, err, config.ErrServiceNotAttached.Code)
		}
		if enabled {
			t.Fatalf("%s gate: IsEnabled before config attach = true, want false", name)
		}
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
			name:   "smtp_reply_to_without_a_target",
			env:    map[string]string{"APP_SMTP_REPLY_TO": "support@example.com"},
			wantIn: "APP_SMTP_HOST",
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

// TestConfigFromEnv_CompleteSMTPComposition_CarriesTheTarget pins the
// success half of the SMTP wiring: with both variables set and a
// parseable port, ConfigFromEnv carries the host, the credentials and the
// optional reply-to as the
// resolved SMTP target fields -- the values BuildServer hands the preset
// channel -- and pre-builds no Mailer of its own.
func TestConfigFromEnv_CompleteSMTPComposition_CarriesTheTarget(t *testing.T) {
	testutil.ClearBootstrapEnv(t)
	t.Setenv("APP_SMTP_HOST", "smtp.example.com")
	t.Setenv("APP_SMTP_PORT", "587")
	t.Setenv("APP_SMTP_USERNAME", "mailer@example.com")
	t.Setenv("APP_SMTP_PASSWORD", "smtp-secret")
	t.Setenv("APP_SMTP_REPLY_TO", "support@example.com")

	cfg, err := app.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.Mailer != nil {
		t.Error("Mailer non-nil, want ConfigFromEnv to pre-build nothing: BuildServer composes \"mailer.smtp\" through the preset channel from the SMTP fields")
	}
	if cfg.SMTPHost != "smtp.example.com" || cfg.SMTPPort != 587 || cfg.SMTPUsername != "mailer@example.com" || cfg.SMTPPassword != "smtp-secret" || cfg.SMTPReplyTo != "support@example.com" {
		t.Errorf("SMTP target = %s:%d user=%q reply-to=%q, want the complete APP_SMTP_* group carried through", cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUsername, cfg.SMTPReplyTo)
	}
}

// TestDeclaredMaterial_MalformedIndividualKeysAreRefused pins the
// individual-override parse for the five key materials beyond
// config.cipher_key (whose malformed values
// TestDeclaredMaterial_ConfigKeyRejectsMalformedValues already covers): a
// malformed individual variable must refuse the assembly's resolution naming
// the variable, never fall through to the dev default silently.
func TestDeclaredMaterial_MalformedIndividualKeysAreRefused(t *testing.T) {
	testutil.ClearBootstrapEnv(t)

	for _, key := range []string{
		"APP_ORG__INVITATION_EMAIL_INDEX_KEY", "APP_NOTIFICATION__CONTACT_INDEX_KEY", "APP_PKI__LOCAL_KEY_CIPHER_KEY",
		"APP_AUTHN__BLIND_INDEX_KEY", "APP_AUTHN__PII_CIPHER_KEY",
	} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "not-64-hex-chars")
			_, err := resolveDeclaredMaterial(t)
			if err == nil {
				t.Fatalf("resolving the declared key material with a malformed %s succeeded, want the refusal", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error = %q, want it to name %q", err, key)
			}
		})
	}
}
