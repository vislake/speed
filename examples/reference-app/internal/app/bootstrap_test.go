package app

// bootstrap_test.go pins the bootstrap target's shape: the loader key path of
// every leaf field of hostConfig, the correspondence between those paths and
// the bootstrap surface the app itself binds -- the host keys the app owns --
// and the environment-variable census testutil clears, against the target
// whose variables that census claims to be. The platform's six key materials
// are not part of the host target: each declaring module's component carries
// the declaration, and bootstrap_material_test.go pins the material that
// resolves off it.
//
// The file also drives the resolution the target feeds, with hand-built
// hostConfig values instead of a process environment: serverConfigFrom,
// splitTrustedProxies, hostConfigDefaults and loadHostConfig are exercised
// directly, so every refusal path and precedence tier is pinned here as well
// as end to end through ConfigFromEnv in flowtests.

import (
	"errors"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/testutil"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/config"
)

// hostConfigKeyPaths walks hostConfig the way the loader walks a target: each
// exported field contributes its lowercased name as a key segment, nested
// structs descend into path segments of their own, every other type is a
// leaf. The host's own fields are strings, ints and bools, so the walk needs
// none of the loader's subtler leaf rules (maps, scalar structs, unexported
// fields); the loader's own describe is the authority for what it will
// actually fill.
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
// target's key set rests on: the host target's own leaf key set is exactly
// the host keys the app owns -- no key without a field, and no field without
// a key (the direction no boot can see).
func TestHostConfigBindsExactlyItsBootstrapSurface(t *testing.T) {
	want := append([]string{}, hostBootstrapKeys...)
	sort.Strings(want)

	got := hostConfigKeyPaths(t, reflect.TypeOf(hostConfig{}))
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hostConfig key paths = %v, want exactly %v", got, want)
	}
}

// TestHostConfigPinsEveryLeafField pins the other half of the target's shape:
// every one of the host's leaf fields states the exact variable it reads, so
// no field can fall back to the loader's prefix derivation -- which spells
// the app's flat, single-underscored variable names differently -- without
// failing here.
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

// TestBootstrapEnvCensusMatchesTheTarget pins testutil's bootstrap-variable
// census against the target it describes: every env pin of hostConfig's leaf
// fields (whose exhaustiveness TestHostConfigPinsEveryLeafField pins on the
// field side), the root-key variable loadHostConfig hands the loader beside
// the target, and the six declared key materials' names derived the way the
// loader derives them -- config.EnvName over the declared key paths, which
// BootstrapDevDefaults' keys are. A variable the target reads but the census
// omits survives ClearBootstrapEnv, so an ambient value could skew a boot
// test; a census entry no field reads is cleared for nothing.
func TestBootstrapEnvCensusMatchesTheTarget(t *testing.T) {
	var want []string

	var collectPins func(t2 reflect.Type)
	collectPins = func(t2 reflect.Type) {
		for i := 0; i < t2.NumField(); i++ {
			field := t2.Field(i)
			if !field.IsExported() {
				continue
			}
			if field.Type.Kind() == reflect.Struct {
				collectPins(field.Type)
				continue
			}
			var name string
			for _, option := range strings.Split(field.Tag.Get("config"), ",") {
				if pinned, found := strings.CutPrefix(option, "env="); found {
					name = pinned
					break
				}
			}
			if name == "" {
				t.Fatalf("leaf field %s of %s carries no env pin", field.Name, t2)
			}
			want = append(want, name)
		}
	}
	collectPins(reflect.TypeOf(hostConfig{}))

	for keyPath := range BootstrapDevDefaults() {
		want = append(want, config.EnvName(envPrefix, keyPath))
	}
	want = append(want, rootKeyEnv)
	sort.Strings(want)

	got := testutil.BootstrapEnvNames()
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("testutil.BootstrapEnvNames() = %v, want the target's own variable surface %v", got, want)
	}
}

// TestBootstrapDevDefaults_NamesTheSixDeclaredKeyPaths pins the table's
// key set: the six declared key paths the composed modules' components carry,
// each spelled exactly as its declaration spells it -- a misspelling would
// leave the entry inert, and the key would resolve to nothing.
func TestBootstrapDevDefaults_NamesTheSixDeclaredKeyPaths(t *testing.T) {
	got := slices.Sorted(maps.Keys(BootstrapDevDefaults()))
	want := []string{
		"authn.blind_index_key",
		"authn.pii_cipher_key",
		"config.cipher_key",
		"notification.contact_index_key",
		"org.invitation_email_index_key",
		"pki.local_key_cipher_key",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("BootstrapDevDefaults() keys = %v, want the six declared key paths %v", got, want)
	}
}

// TestConfigFromEnv_RefusesAMalformedRootKey pins the loader's root-key
// refusal as this app's host pass delivers it: a malformed APP_ROOT_KEY fails
// the load with the loader's dedicated config.ErrInvalidRootKey, naming the
// variable the operator must fix. The refusal text itself belongs to the
// loader, so the assertion is the sentinel and the named variable, not the
// message.
func TestConfigFromEnv_RefusesAMalformedRootKey(t *testing.T) {
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
}

// TestLoadHostConfig_ResolvesTheHostsOwnFieldsOnly pins the host pass's
// scope: loadHostConfig fills the host's own keys from the environment and
// leaves the declared key materials alone -- they resolve in the assembly's
// own pass (bootstrap_material_test.go) -- so a host field still wins its
// source while the declared material carries no host-side copy.
func TestLoadHostConfig_ResolvesTheHostsOwnFieldsOnly(t *testing.T) {
	testutil.ClearBootstrapEnv(t)
	t.Setenv("PORT", "4321")

	hc, err := loadHostConfig()
	if err != nil {
		t.Fatalf("loadHostConfig: %v", err)
	}
	if hc.Port != "4321" {
		t.Errorf("Port = %q, want the injected PORT value", hc.Port)
	}
	if hc.S3Endpoint != "" || hc.SMTPHost != "" {
		t.Errorf("hostConfig carries values for unset optional variables: S3Endpoint=%q SMTPHost=%q", hc.S3Endpoint, hc.SMTPHost)
	}
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
			name:    "an SMTP reply-to without a target",
			mutate:  func(hc *hostConfig) { hc.SMTPReplyTo = "support@example.test" },
			wantErr: "APP_SMTP_HOST",
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

// TestServerConfigFrom_CompleteSMTPCarriesTheTargetWithoutBuildingAMailer
// pins the complete-composition branch's resolution shape: a host and port
// together carry the SMTP target fields on the resolved config, and
// deliberately pre-build no Mailer. The mailer itself is composed from these
// fields through the builtin composition's config channel in BuildServer (its
// composition-options comment has the full split), so constructing
// pkgcore.NewSMTPMailer here would only duplicate what the "mailer.smtp"
// registration already does from the same four values.
func TestServerConfigFrom_CompleteSMTPCarriesTheTargetWithoutBuildingAMailer(t *testing.T) {
	hc := hostConfigDefaults()
	hc.SMTPHost = "smtp.example.test"
	hc.SMTPPort = 587
	hc.SMTPUsername = "mailer@example.test"
	hc.SMTPPassword = "smtp-password"
	hc.SMTPReplyTo = "support@example.test"

	cfg, err := serverConfigFrom(hc)
	if err != nil {
		t.Fatalf("serverConfigFrom with a complete APP_SMTP_* composition: %v", err)
	}
	if cfg.Mailer != nil {
		t.Error("Mailer is non-nil with a complete APP_SMTP_* composition, want the host to pre-build nothing: BuildServer composes \"mailer.smtp\" through the composition channel from the SMTP fields")
	}
	if cfg.SMTPHost != "smtp.example.test" || cfg.SMTPPort != 587 || cfg.SMTPUsername != "mailer@example.test" || cfg.SMTPPassword != "smtp-password" || cfg.SMTPReplyTo != "support@example.test" {
		t.Errorf("SMTP target = %s:%d user=%q reply-to=%q, want the complete APP_SMTP_* group carried through for the composition channel",
			cfg.SMTPHost, cfg.SMTPPort, cfg.SMTPUsername, cfg.SMTPReplyTo)
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
