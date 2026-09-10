package app

// bootstrap_test.go pins the bootstrap target's shape: the loader key path of
// every leaf field of hostConfig, and the correspondence between those paths
// and the bootstrap surface the app binds -- the keys the platform modules
// declare on the registry's bootstrap seat, plus the host keys the app owns.
//
// The composition-time half of the same correspondence is
// verifyBootstrapBinding, which runs at every boot against the live registry;
// this test is the target-side half, so a field added to hostConfig without a
// key (or a key without a field) fails here even when no boot happens to
// compose the declaring module.
//
// The file also drives the resolution the target feeds, with hand-built
// hostConfig values instead of a process environment: serverConfigFrom,
// splitTrustedProxies, hostConfigDefaults and loadHostConfig are exercised
// directly, so every refusal path and precedence tier is pinned here as well
// as end to end through ConfigFromEnv in flowtests.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/testutil"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/config"
)

// platformDeclaredKeys are the bootstrap keys the platform modules declare on
// the registry's bootstrap seat and this app's loader target binds: authn's
// two key materials, org's invitation-address blind-index key, notification's
// contact-address blind-index key, pki's local-key cipher key and config's
// cipher key. Each module's own bootstrapKeyDecl is the spelling's source;
// verifyBootstrapBinding checks this same set against the live registry, so a
// module-side rename fails the boot rather than silently drifting from this
// fixture.
var platformDeclaredKeys = []string{
	"authn.blind_index_key",
	"authn.pii_cipher_key",
	"config.cipher_key",
	"notification.contact_index_key",
	"org.invitation_email_index_key",
	"pki.local_key_cipher_key",
}

// hostConfigKeyPaths walks hostConfig the way the loader walks a target: each
// exported field contributes its lowercased name as a key segment, nested
// structs descend into path segments of their own, and every other type is a
// leaf. The fields of this target are strings, ints, bools and the five
// key-group structs, so the walk needs none of the loader's subtler leaf rules
// (maps, scalar structs, unexported fields); the loader's own describe is the
// authority for what it will actually fill.
func hostConfigKeyPaths(t *testing.T, typ reflect.Type) []string {
	t.Helper()
	var paths []string
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name := strings.ToLower(field.Name)
		if field.Type.Kind() == reflect.Struct {
			for _, nested := range hostConfigKeyPaths(t, field.Type) {
				paths = append(paths, name+"."+nested)
			}
			continue
		}
		paths = append(paths, name)
	}
	return paths
}

// TestHostConfigBindsExactlyItsBootstrapSurface pins the strict equality the
// binding proof rests on: the target's leaf key set is exactly the host keys
// the app owns plus the keys the platform modules declare -- no key without a
// field (both Verify calls at boot prove that direction too) and no field
// without a key (this half, which no boot can see).
func TestHostConfigBindsExactlyItsBootstrapSurface(t *testing.T) {
	want := append(append([]string{}, hostBootstrapKeys...), platformDeclaredKeys...)
	sort.Strings(want)

	got := hostConfigKeyPaths(t, reflect.TypeOf(hostConfig{}))
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hostConfig key paths = %v, want exactly %v", got, want)
	}
}

// TestHostConfigPinsEveryLeafField pins the other half of the target's shape:
// every leaf field states the exact variable it reads, so no field can fall
// back to the loader's prefix derivation -- which spells the app's flat,
// single-underscored variable names differently -- without failing here.
func TestHostConfigPinsEveryLeafField(t *testing.T) {
	typ := reflect.TypeOf(hostConfig{})
	var unpinned []string
	var walk func(reflect.Type, string)
	walk = func(t2 reflect.Type, prefix string) {
		for i := 0; i < t2.NumField(); i++ {
			field := t2.Field(i)
			if !field.IsExported() {
				continue
			}
			name := prefix + strings.ToLower(field.Name)
			if field.Type.Kind() == reflect.Struct {
				walk(field.Type, name+".")
				continue
			}
			tag := field.Tag.Get("config")
			if !strings.Contains(tag, "env=") {
				unpinned = append(unpinned, name)
			}
		}
	}
	walk(typ, "")
	if len(unpinned) > 0 {
		t.Fatalf("fields without an env pin: %v", unpinned)
	}
}

// TestConfigFromEnv_RefusesMalformedKeyMaterial pins the loader's own
// key-material refusals as this app's wiring delivers them: a malformed
// explicit value for any of the six keys fails the load wrapping
// config.ErrInvalidValue, and a malformed APP_ROOT_KEY fails it with the
// loader's dedicated config.ErrInvalidRootKey -- each naming the variable the
// operator must fix. The refusal text itself belongs to the loader now, so
// the assertion is the sentinel and the named variable, not the message.
func TestConfigFromEnv_RefusesMalformedKeyMaterial(t *testing.T) {
	t.Run("an individual key's malformed hex text", func(t *testing.T) {
		for _, envName := range []string{
			"APP_CONFIG_KEY",
			"APP_ORG_INDEX_KEY",
			"APP_NOTIFICATION_INDEX_KEY",
			"APP_PKI_LOCAL_KEY_CIPHER_KEY",
			"APP_AUTHN_BLIND_INDEX_KEY",
			"APP_AUTHN_PII_CIPHER_KEY",
		} {
			t.Run(envName, func(t *testing.T) {
				testutil.ClearBootstrapEnv(t)
				t.Setenv(envName, "not-64-hex-characters")

				_, err := ConfigFromEnv()
				if !errors.Is(err, config.ErrInvalidValue) {
					t.Fatalf("ConfigFromEnv() error = %v, want it to wrap config.ErrInvalidValue", err)
				}
				if !strings.Contains(err.Error(), envName) {
					t.Errorf("refusal does not name %s: %v", envName, err)
				}
			})
		}
	})

	t.Run("a malformed root key", func(t *testing.T) {
		for name, value := range map[string]string{
			"the wrong length":       "zzzz",
			"64 characters, not hex": strings.Repeat("z", 64),
		} {
			t.Run(name, func(t *testing.T) {
				testutil.ClearBootstrapEnv(t)
				t.Setenv("APP_ROOT_KEY", value)

				_, err := ConfigFromEnv()
				if !errors.Is(err, config.ErrInvalidRootKey) {
					t.Fatalf("ConfigFromEnv() error = %v, want it to wrap config.ErrInvalidRootKey", err)
				}
				if !strings.Contains(err.Error(), "APP_ROOT_KEY") {
					t.Errorf("refusal does not name APP_ROOT_KEY: %v", err)
				}
			})
		}
	})
}

// TestLoadHostConfig_ResolvesKeyMaterialAndKeepsTheDevDefaultsIntact pins the
// app-side half of the loader integration. It drives the real wiring
// (loadHostConfig): APP_ROOT_KEY alone derives each of the six key materials
// under its declared key path through the platform composition
// (pkgcore.BootstrapKeyPurpose + dbkit.DeriveKey), an explicitly set
// individual variable still wins over the derivation, and with no root key
// every material falls back to its Dev* default.
//
// The hazard it guards is the loader's decode step: mapstructure writes a
// decoded value through a pre-filled []byte field's own backing array, so a
// key-material field left pre-filled from a package-level Dev* constant would
// have that constant silently rewritten with hex text -- poisoning the default
// every later zero-setup load in this process falls back to. The assertion
// after every load is that the six Dev* variables are byte-identical to their
// snapshots, so a regression to a decode-based resolution fails here.
func TestLoadHostConfig_ResolvesKeyMaterialAndKeepsTheDevDefaultsIntact(t *testing.T) {
	testutil.ClearBootstrapEnv(t)

	defaults := map[string][]byte{
		"APP_CONFIG_KEY":               DevConfigKey,
		"APP_ORG_INDEX_KEY":            DevOrgIndexKey,
		"APP_NOTIFICATION_INDEX_KEY":   DevNotificationIndexKey,
		"APP_PKI_LOCAL_KEY_CIPHER_KEY": DevPKILocalKeyCipherKey,
		"APP_AUTHN_BLIND_INDEX_KEY":    DevBlindIndexKey,
		"APP_AUTHN_PII_CIPHER_KEY":     DevPIICipherKey,
	}
	before := make(map[string][]byte, len(defaults))
	for name, key := range defaults {
		before[name] = bytes.Clone(key)
	}
	assertDefaultsIntact := func(t *testing.T) {
		t.Helper()
		for name, key := range defaults {
			if !bytes.Equal(key, before[name]) {
				t.Errorf("the %s development default was rewritten in place: got %x, want it byte-identical to %x", name, key, before[name])
			}
		}
	}

	rootKey := sha256.Sum256([]byte("TestLoadHostConfig root secret"))
	t.Setenv("APP_ROOT_KEY", hex.EncodeToString(rootKey[:]))

	hc, err := loadHostConfig()
	if err != nil {
		t.Fatalf("loadHostConfig: %v", err)
	}
	assertDefaultsIntact(t)

	// Every field's material is the composed derivation over the declared key
	// path -- reproduced independently here, so a field whose key path drifted
	// from what its module declares shows up as a value mismatch.
	for keyPath, got := range map[string][]byte{
		"config.cipher_key":              hc.Config.Cipher_Key,
		"org.invitation_email_index_key": hc.Org.Invitation_Email_Index_Key,
		"notification.contact_index_key": hc.Notification.Contact_Index_Key,
		"pki.local_key_cipher_key":       hc.Pki.Local_Key_Cipher_Key,
		"authn.blind_index_key":          hc.Authn.Blind_Index_Key,
		"authn.pii_cipher_key":           hc.Authn.PII_Cipher_Key,
	} {
		purpose, purposeErr := pkgcore.BootstrapKeyPurpose(keyPath)
		if purposeErr != nil {
			t.Fatalf("pkgcore.BootstrapKeyPurpose(%q): %v", keyPath, purposeErr)
		}
		want, deriveErr := dbkit.DeriveKey(rootKey[:], purpose)
		if deriveErr != nil {
			t.Fatalf("dbkit.DeriveKey(rootKey, %q): %v", purpose, deriveErr)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("material for %s = %x, want the composed derivation %x", keyPath, got, want)
		}
	}

	// An explicit individual variable wins over the derivation.
	explicit := sha256.Sum256([]byte("TestLoadHostConfig explicit APP_CONFIG_KEY"))
	t.Setenv("APP_CONFIG_KEY", hex.EncodeToString(explicit[:]))
	hc, err = loadHostConfig()
	if err != nil {
		t.Fatalf("loadHostConfig with an explicit override: %v", err)
	}
	if !bytes.Equal(hc.Config.Cipher_Key, explicit[:]) {
		t.Errorf("Config.Cipher_Key = %x, want the explicit override %x", hc.Config.Cipher_Key, explicit[:])
	}
	assertDefaultsIntact(t)

	// With the root key and the override emptied, every material falls back to
	// its development default.
	t.Setenv("APP_ROOT_KEY", "")
	t.Setenv("APP_CONFIG_KEY", "")
	hc, err = loadHostConfig()
	if err != nil {
		t.Fatalf("loadHostConfig with nothing set: %v", err)
	}
	if !bytes.Equal(hc.Config.Cipher_Key, DevConfigKey) {
		t.Errorf("Config.Cipher_Key = %x, want the development default %x", hc.Config.Cipher_Key, DevConfigKey)
	}
	assertDefaultsIntact(t)
}

// TestSplitTrustedProxies pins the list shape ServerConfig.TrustedProxies
// carries: empty input yields nil, entries are trimmed and empty entries
// dropped, so a trailing comma or a whitespace-only element never becomes a
// proxy entry.
func TestSplitTrustedProxies(t *testing.T) {
	if got := splitTrustedProxies(""); got != nil {
		t.Fatalf("splitTrustedProxies(empty) = %v, want nil", got)
	}
	if got := splitTrustedProxies(" , , "); got != nil {
		t.Fatalf("splitTrustedProxies with only empty entries = %v, want nil", got)
	}
	got := splitTrustedProxies(" 172.16.0.0/12 , 203.0.113.10,")
	want := []string{"172.16.0.0/12", "203.0.113.10"}
	if !slices.Equal(got, want) {
		t.Fatalf("splitTrustedProxies = %v, want %v", got, want)
	}
}

// s3Composition fills a complete APP_S3_* group on hc: the shape the
// cross-variable tests below start from when one variable of the group must
// be missing or conflicting.
func s3Composition(hc *hostConfig) {
	hc.S3Endpoint = "https://objects.example.test"
	hc.S3Bucket = "bucket"
	hc.S3AccessKey = "access-key"
	hc.S3SecretKey = "secret-key"
}

// TestServerConfigFrom_RefusesCrossVariableCombinations pins every refusal
// the loader cannot state on a single field: an incomplete or ambiguous
// composition fails the boot naming the variable an operator must change.
func TestServerConfigFrom_RefusesCrossVariableCombinations(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(hc *hostConfig)
		wantErr string
	}{
		{
			name: "an S3 composition missing one variable",
			mutate: func(hc *hostConfig) {
				s3Composition(hc)
				hc.S3Endpoint = ""
			},
			wantErr: "APP_S3_ENDPOINT",
		},
		{
			name: "an object-store root beside an S3 composition",
			mutate: func(hc *hostConfig) {
				s3Composition(hc)
				hc.ObjectStoreRoot = "/var/lib/reference-app/objects"
			},
			wantErr: "APP_OBJECT_STORE_ROOT",
		},
		{
			name:    "an SMTP port without a host",
			mutate:  func(hc *hostConfig) { hc.SMTPPort = 587 },
			wantErr: "APP_SMTP_HOST",
		},
		{
			name:    "an SMTP host without a port",
			mutate:  func(hc *hostConfig) { hc.SMTPHost = "smtp.example.test" },
			wantErr: "APP_SMTP_PORT",
		},
		{
			name: "the Fly declaration with only empty proxy entries",
			mutate: func(hc *hostConfig) {
				hc.ReadFlyClientIP = true
				hc.TrustedProxies = " , "
			},
			wantErr: "APP_TRUSTED_PROXIES",
		},
		{
			name:    "a negative provisioning-failure count",
			mutate:  func(hc *hostConfig) { hc.FailSelfServiceProvision = -1 },
			wantErr: "APP_FAIL_SELF_SERVICE_PROVISION",
		},
		{
			name:    "an unknown deployment mode",
			mutate:  func(hc *hostConfig) { hc.DeploymentMode = "not-a-deployment-mode" },
			wantErr: "not-a-deployment-mode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hc := hostConfigDefaults()
			tc.mutate(&hc)
			if _, err := serverConfigFrom(hc); err == nil {
				t.Fatalf("serverConfigFrom accepted %s", tc.name)
			} else if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("refusal does not name %s: %v", tc.wantErr, err)
			}
		})
	}
}

// TestServerConfigFrom_ResolvesEmptyValuesToTheirDefaults pins the
// ""-is-unset boundary the transform applies to every string field: an
// unset or emptied deployment mode, PORT, database path, public origin and
// switch variable all resolve to their documented fallbacks, while a set
// value is carried through and any non-empty switch value reads as on.
func TestServerConfigFrom_ResolvesEmptyValuesToTheirDefaults(t *testing.T) {
	cfg, err := serverConfigFrom(hostConfigDefaults())
	if err != nil {
		t.Fatalf("serverConfigFrom(hostConfigDefaults()): %v", err)
	}
	if cfg.DeploymentMode != pkgcore.DeploymentModeStandalone {
		t.Errorf("DeploymentMode = %q, want %q", cfg.DeploymentMode, pkgcore.DeploymentModeStandalone)
	}
	if cfg.Port != DefaultPort {
		t.Errorf("Port = %q, want the fallback %q", cfg.Port, DefaultPort)
	}
	if cfg.SQLitePath != DefaultSQLitePath {
		t.Errorf("SQLitePath = %q, want the fallback %q", cfg.SQLitePath, DefaultSQLitePath)
	}
	if want := "http://localhost:" + DefaultPort; cfg.PublicOrigin != want {
		t.Errorf("PublicOrigin = %q, want the derived %q", cfg.PublicOrigin, want)
	}
	if cfg.DisableQueueWorker || cfg.DisableDemoUserHeader {
		t.Errorf("an empty switch value read as on: DisableQueueWorker=%t, DisableDemoUserHeader=%t",
			cfg.DisableQueueWorker, cfg.DisableDemoUserHeader)
	}
	if cfg.FailSelfServiceProvision != nil {
		t.Error("FailSelfServiceProvision is armed with no count set")
	}
	if cfg.Mailer != nil {
		t.Error("Mailer is composed with no APP_SMTP_* variable set")
	}

	hc := hostConfigDefaults()
	hc.Port = "7000"
	hc.PublicOrigin = "https://app.example.test"
	hc.DisableQueueWorker = "1"
	hc.DisableDemoUserHeader = "yes"
	hc.FailSelfServiceProvision = 3
	cfg, err = serverConfigFrom(hc)
	if err != nil {
		t.Fatalf("serverConfigFrom with set values: %v", err)
	}
	if cfg.Port != "7000" {
		t.Errorf("Port = %q, want the set 7000", cfg.Port)
	}
	if cfg.PublicOrigin != "https://app.example.test" {
		t.Errorf("PublicOrigin = %q, want the set origin", cfg.PublicOrigin)
	}
	if !cfg.DisableQueueWorker || !cfg.DisableDemoUserHeader {
		t.Errorf("a non-empty switch value did not read as on: DisableQueueWorker=%t, DisableDemoUserHeader=%t",
			cfg.DisableQueueWorker, cfg.DisableDemoUserHeader)
	}
	if cfg.FailSelfServiceProvision == nil {
		t.Fatal("FailSelfServiceProvision is not armed with a positive count")
	}
	if err := cfg.FailSelfServiceProvision("account-a"); err == nil {
		t.Error("the armed injector did not fail a first provisioning attempt")
	}
}

// TestServerConfigFrom_ComposesTheSMTPMailer pins the complete-composition
// branch: a host and port together compose the real SMTP Mailer from the
// credentials, declaring the capabilities the mailer.smtp builtin declares.
func TestServerConfigFrom_ComposesTheSMTPMailer(t *testing.T) {
	hc := hostConfigDefaults()
	hc.SMTPHost = "smtp.example.test"
	hc.SMTPPort = 587
	hc.SMTPUsername = "mailer@example.test"
	hc.SMTPPassword = "smtp-password"

	cfg, err := serverConfigFrom(hc)
	if err != nil {
		t.Fatalf("serverConfigFrom with a complete APP_SMTP_* composition: %v", err)
	}
	if cfg.Mailer == nil {
		t.Fatal("Mailer is nil with a complete APP_SMTP_* composition")
	}
	if want := pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart; cfg.MailerCapabilities != want {
		t.Errorf("MailerCapabilities = %v, want %v", cfg.MailerCapabilities, want)
	}
}

// TestVerifyBootstrapBinding pins both directions of the boot-time proof: a
// registry whose declared keys all map onto the target passes, a declared key
// with no matching field fails naming that key, and a host key the target
// does not bind fails too -- the drift an edit to hostBootstrapKeys without a
// matching field would introduce.
func TestVerifyBootstrapBinding(t *testing.T) {
	registry := func(declared ...string) *pkgcore.Registry {
		reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
		if len(declared) == 0 {
			return reg
		}
		keys := make([]pkgcore.BootstrapKey, 0, len(declared))
		for _, key := range declared {
			keys = append(keys, pkgcore.BootstrapKey{Key: key, Format: "string"})
		}
		if err := reg.Bootstrap.Add(keys...); err != nil {
			t.Fatalf("declare bootstrap keys %v: %v", declared, err)
		}
		return reg
	}

	if err := verifyBootstrapBinding(registry("config.cipher_key", "authn.pii_cipher_key")); err != nil {
		t.Fatalf("verifyBootstrapBinding with declared keys the target binds: %v", err)
	}

	if err := verifyBootstrapBinding(registry("config.unbound_key")); err == nil {
		t.Fatal("verifyBootstrapBinding accepted a declared key the target does not bind")
	} else if !strings.Contains(err.Error(), "config.unbound_key") {
		t.Errorf("declared-key refusal does not name the key: %v", err)
	}

	original := hostBootstrapKeys
	t.Cleanup(func() { hostBootstrapKeys = original })
	hostBootstrapKeys = append(append([]string{}, original...), "hostkeywithoutfield")
	if err := verifyBootstrapBinding(registry("config.cipher_key")); err == nil {
		t.Fatal("verifyBootstrapBinding accepted a host key the target does not bind")
	} else if !strings.Contains(err.Error(), "hostkeywithoutfield") {
		t.Errorf("host-key refusal does not name the key: %v", err)
	}
}

// TestConfigFromEnv_PropagatesALoadRefusal pins the load half of the entry
// point: a variable the loader itself rejects -- here an APP_SMTP_PORT whose
// text is not an integer, refused while the target is being filled -- surfaces
// as ConfigFromEnv's error rather than as a partially resolved configuration.
func TestConfigFromEnv_PropagatesALoadRefusal(t *testing.T) {
	testutil.ClearBootstrapEnv(t)
	t.Setenv("APP_SMTP_PORT", "not-a-port")

	cfg, err := ConfigFromEnv()
	if err == nil {
		t.Fatalf("ConfigFromEnv accepted a non-numeric APP_SMTP_PORT: %+v", cfg)
	}
	if !errors.Is(err, config.ErrInvalidValue) {
		t.Errorf("ConfigFromEnv refusal = %v, want a load refusal wrapping config.ErrInvalidValue", err)
	}
}
