package appconfig

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/saasctl/internal/template"
)

// envFromMap is a LookupEnv over a map, the test double every Load test
// drives the five variables through.
func envFromMap(env map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

// TestLoadDefaultsResolveTheGeneratedProjectsOwnDefaults: with an empty
// environment, Load resolves exactly what the generated server resolves
// with no environment at all -- the standalone deployment mode, port 8080,
// the fixed app.db path -- plus the documented development key bytes, and
// records that nothing came from the environment.
func TestLoadDefaultsResolveTheGeneratedProjectsOwnDefaults(t *testing.T) {
	cfg, err := Load("cli-app", envFromMap(nil))
	if err != nil {
		t.Fatalf("Load with an empty environment failed: %v", err)
	}
	if cfg.DeploymentMode != pkgcore.DeploymentModeStandalone {
		t.Errorf("DeploymentMode = %q, want the standalone default", cfg.DeploymentMode)
	}
	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want the 8080 default", cfg.Port)
	}
	if cfg.SQLitePath != defaultSQLitePath {
		t.Errorf("SQLitePath = %q, want the fixed %q default", cfg.SQLitePath, defaultSQLitePath)
	}
	// The P3 rename regression: the default must NOT follow the module
	// path's final element (the appName argument). The generated app's own
	// default is frozen at materialization, so a twin default derived from
	// the CURRENT module path would fork from the app the moment a
	// consumer renamed the module -- the CLI migrating and printing one
	// file while the app opens another. Whatever the caller's appName is,
	// the default stays the one fixed literal both sides share.
	for _, renamed := range []string{"smilestudio-v2", "another-vendor/renamed-app"} {
		renamedCfg, loadErr := Load(renamed, envFromMap(nil))
		if loadErr != nil {
			t.Errorf("Load(%q): %v", renamed, loadErr)
			continue
		}
		if renamedCfg.SQLitePath != defaultSQLitePath {
			t.Errorf("Load(%q).SQLitePath = %q, want the fixed %q default even under a renamed module path", renamed, renamedCfg.SQLitePath, defaultSQLitePath)
		}
	}
	if !bytes.Equal(cfg.ConfigKey, devConfigKey) {
		t.Errorf("ConfigKey is not the ascending 0x00..0x1f development default")
	}
	if !bytes.Equal(cfg.OrgIndexKey, devOrgIndexKey) {
		t.Errorf("OrgIndexKey is not the descending 0xff..0xe0 development default")
	}
	// The three authn/pki key materials fall back to their own dev byte
	// runs when unset -- the same fallback semantics as the two keys
	// above, asserted so the three new fields cannot silently resolve to
	// something else (nil, zeros) while the template keeps its dev bytes.
	if !bytes.Equal(cfg.AuthnBlindIndexKey, devBlindIndexKey) {
		t.Error("AuthnBlindIndexKey is not the 0x40..0x5f development default")
	}
	if !bytes.Equal(cfg.AuthnPIICipherKey, devPIICipherKey) {
		t.Error("AuthnPIICipherKey is not the 0x60..0x7f development default")
	}
	if !bytes.Equal(cfg.PKILocalKeyCipherKey, devPKILocalKeyCipherKey) {
		t.Error("PKILocalKeyCipherKey is not the 0x80..0x9f development default")
	}
	if cfg.DeploymentModeFromEnv || cfg.PortFromEnv || cfg.SQLitePathFromEnv ||
		cfg.ConfigKeyFromEnv || cfg.OrgIndexKeyFromEnv ||
		cfg.AuthnBlindIndexKeyFromEnv || cfg.AuthnPIICipherKeyFromEnv || cfg.PKILocalKeyCipherKeyFromEnv {
		t.Error("an empty environment must record every field as not-from-env")
	}
}

// TestLoadReadsSetVariables: each variable that carries a non-empty value
// is parsed and recorded as from-env, with the five key variables decoding
// their hex into the 32 bytes they encode.
func TestLoadReadsSetVariables(t *testing.T) {
	configKeyHex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	orgIndexKeyHex := "ffe0f1d2c3b4a5968778695a4b3c2d1e0f00112233445566778899aabbccddee"
	authnBlindIndexKeyHex := "404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f"
	authnPIICipherKeyHex := "606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f"
	pkiLocalKeyCipherKeyHex := "808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9f"
	cfg, err := Load("cli-app", envFromMap(map[string]string{
		DeploymentModeEnv:       "Distributed",
		PortEnv:                 "9090",
		DBPathEnv:               "/var/data/smile.db",
		ConfigKeyEnv:            configKeyHex,
		OrgIndexKeyEnv:          orgIndexKeyHex,
		AuthnBlindIndexKeyEnv:   authnBlindIndexKeyHex,
		AuthnPIICipherKeyEnv:    authnPIICipherKeyHex,
		PKILocalKeyCipherKeyEnv: pkiLocalKeyCipherKeyHex,
	}))
	if err != nil {
		t.Fatalf("Load with a full environment failed: %v", err)
	}
	if cfg.DeploymentMode != pkgcore.DeploymentModeDistributed {
		t.Errorf("DeploymentMode = %q, want the parsed distributed mode", cfg.DeploymentMode)
	}
	if cfg.Port != "9090" || cfg.SQLitePath != "/var/data/smile.db" {
		t.Errorf("Port/SQLitePath = %q/%q, want the set values", cfg.Port, cfg.SQLitePath)
	}
	if want := []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f}; !bytes.Equal(cfg.ConfigKey, want) {
		t.Errorf("ConfigKey does not decode to the hex it encoded")
	}
	// The three authn/pki key materials decode into the exact 32 bytes
	// their hex encodes -- the env-set path is real, not a
	// validation-only pass that would keep the dev bytes underneath.
	if want := []byte{0x40, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48, 0x49, 0x4a, 0x4b, 0x4c, 0x4d, 0x4e, 0x4f, 0x50, 0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58, 0x59, 0x5a, 0x5b, 0x5c, 0x5d, 0x5e, 0x5f}; !bytes.Equal(cfg.AuthnBlindIndexKey, want) {
		t.Error("AuthnBlindIndexKey does not decode to the hex it encoded")
	}
	if want := []byte{0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67, 0x68, 0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f, 0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77, 0x78, 0x79, 0x7a, 0x7b, 0x7c, 0x7d, 0x7e, 0x7f}; !bytes.Equal(cfg.AuthnPIICipherKey, want) {
		t.Error("AuthnPIICipherKey does not decode to the hex it encoded")
	}
	if want := []byte{0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89, 0x8a, 0x8b, 0x8c, 0x8d, 0x8e, 0x8f, 0x90, 0x91, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97, 0x98, 0x99, 0x9a, 0x9b, 0x9c, 0x9d, 0x9e, 0x9f}; !bytes.Equal(cfg.PKILocalKeyCipherKey, want) {
		t.Error("PKILocalKeyCipherKey does not decode to the hex it encoded")
	}
	if !cfg.DeploymentModeFromEnv || !cfg.PortFromEnv || !cfg.SQLitePathFromEnv ||
		!cfg.ConfigKeyFromEnv || !cfg.OrgIndexKeyFromEnv ||
		!cfg.AuthnBlindIndexKeyFromEnv || !cfg.AuthnPIICipherKeyFromEnv || !cfg.PKILocalKeyCipherKeyFromEnv {
		t.Error("a full environment must record every field as from-env")
	}
}

// TestLoadParseModeErrorIsReturnedVerbatim: a deployment mode that does
// not parse fails Load with ParseDeploymentMode's own error, unadorned by
// any app name or wrapper -- the template's contract, in which the error
// travels raw so the boot message is exactly the parser's.
func TestLoadParseModeErrorIsReturnedVerbatim(t *testing.T) {
	_, err := Load("cli-app", envFromMap(map[string]string{DeploymentModeEnv: "banana"}))
	if err == nil {
		t.Fatal("Load accepted an unparsable deployment mode")
	}
	_, want := pkgcore.ParseDeploymentMode("banana")
	if want == nil {
		t.Fatal("ParseDeploymentMode did not reject its own invalid input")
	}
	if err.Error() != want.Error() {
		t.Errorf("error = %q, want the raw ParseDeploymentMode error %q", err, want)
	}
	if strings.HasPrefix(err.Error(), "cli-app:") {
		t.Error("the mode parse error must not carry the app name prefix")
	}
}

// TestLoadMalformedConfigKeyErrorMatchesTheTemplate: a APP_CONFIG_KEY of
// the wrong encoded length fails with the template's exact message, app
// name where the template has its __APP_NAME__ token.
func TestLoadMalformedConfigKeyErrorMatchesTheTemplate(t *testing.T) {
	_, err := Load("cli-app", envFromMap(map[string]string{ConfigKeyEnv: "abc"}))
	if err == nil {
		t.Fatal("Load accepted a short config key")
	}
	want := "cli-app: APP_CONFIG_KEY must hold 64 hex characters (a 32-byte key), got 3"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// TestLoadNonHexConfigKeyErrorNamesTheVariable: a APP_CONFIG_KEY of the
// right length that is not valid hex fails under the template's decode
// message, naming the variable so an operator knows which secret is
// malformed.
func TestLoadNonHexConfigKeyErrorNamesTheVariable(t *testing.T) {
	encoded := strings.Repeat("z", 64)
	_, err := Load("cli-app", envFromMap(map[string]string{ConfigKeyEnv: encoded}))
	if err == nil {
		t.Fatal("Load accepted a non-hex config key")
	}
	if !strings.HasPrefix(err.Error(), "cli-app: APP_CONFIG_KEY: ") {
		t.Errorf("error = %q, want the cli-app: APP_CONFIG_KEY: prefix", err)
	}
	if !strings.Contains(err.Error(), "invalid byte") {
		t.Errorf("error = %q, want hex's invalid-byte detail", err)
	}
}

// TestLoadOrgIndexKeySharesTheConfigKeyFailureShape: the org blind-index
// key variable fails with the same messages as the config master key,
// naming its own variable.
func TestLoadOrgIndexKeySharesTheConfigKeyFailureShape(t *testing.T) {
	_, err := Load("cli-app", envFromMap(map[string]string{OrgIndexKeyEnv: "abc"}))
	if err == nil {
		t.Fatal("Load accepted a short org index key")
	}
	want := "cli-app: APP_ORG_INDEX_KEY must hold 64 hex characters (a 32-byte key), got 3"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// TestLoadAuthnPKIKeyVariablesShareTheConfigKeyFailureShape: the three
// authn/pki key variables fail with the same messages as the config
// master key, each naming its own variable -- the regression proof that
// the env path these keys gained is real (length and hex validation
// included), not a passthrough that would keep the dev bytes no matter
// what the environment holds.
func TestLoadAuthnPKIKeyVariablesShareTheConfigKeyFailureShape(t *testing.T) {
	for _, tt := range []struct {
		env   string
		value string
		want  string
	}{
		{env: AuthnBlindIndexKeyEnv, value: "abc", want: "cli-app: APP_AUTHN_BLIND_INDEX_KEY must hold 64 hex characters (a 32-byte key), got 3"},
		{env: AuthnPIICipherKeyEnv, value: "abc", want: "cli-app: APP_AUTHN_PII_CIPHER_KEY must hold 64 hex characters (a 32-byte key), got 3"},
		{env: PKILocalKeyCipherKeyEnv, value: "abc", want: "cli-app: APP_PKI_LOCAL_KEY_CIPHER_KEY must hold 64 hex characters (a 32-byte key), got 3"},
	} {
		_, err := Load("cli-app", envFromMap(map[string]string{tt.env: tt.value}))
		if err == nil {
			t.Errorf("Load accepted a short %s", tt.env)
			continue
		}
		if err.Error() != tt.want {
			t.Errorf("error = %q, want %q", err, tt.want)
		}
	}
	encoded := strings.Repeat("z", 64)
	for _, envName := range []string{AuthnBlindIndexKeyEnv, AuthnPIICipherKeyEnv, PKILocalKeyCipherKeyEnv} {
		_, err := Load("cli-app", envFromMap(map[string]string{envName: encoded}))
		if err == nil {
			t.Errorf("Load accepted a non-hex %s", envName)
			continue
		}
		if !strings.HasPrefix(err.Error(), "cli-app: "+envName+": ") {
			t.Errorf("error = %q, want the cli-app: %s: prefix", err, envName)
		}
	}
}

// TestLoadSetButEmptyCountsAsUnset: a variable that is present but empty
// resolves to the same default as an absent one and is not recorded as
// from-env -- os.Getenv's own semantics, which the generated server and
// this twin share.
func TestLoadSetButEmptyCountsAsUnset(t *testing.T) {
	cfg, err := Load("cli-app", envFromMap(map[string]string{
		DeploymentModeEnv:       "",
		PortEnv:                 "",
		DBPathEnv:               "",
		ConfigKeyEnv:            "",
		OrgIndexKeyEnv:          "",
		AuthnBlindIndexKeyEnv:   "",
		AuthnPIICipherKeyEnv:    "",
		PKILocalKeyCipherKeyEnv: "",
	}))
	if err != nil {
		t.Fatalf("Load with all-empty variables failed: %v", err)
	}
	if cfg.DeploymentMode != pkgcore.DeploymentModeStandalone || cfg.Port != "8080" || cfg.SQLitePath != defaultSQLitePath {
		t.Errorf("empty variables must resolve to the defaults, got %q/%q/%q", cfg.DeploymentMode, cfg.Port, cfg.SQLitePath)
	}
	if cfg.DeploymentModeFromEnv || cfg.PortFromEnv || cfg.SQLitePathFromEnv ||
		cfg.ConfigKeyFromEnv || cfg.OrgIndexKeyFromEnv ||
		cfg.AuthnBlindIndexKeyFromEnv || cfg.AuthnPIICipherKeyFromEnv || cfg.PKILocalKeyCipherKeyFromEnv {
		t.Error("set-but-empty variables must not be recorded as from-env")
	}
}

// TestLoadReadsInfrastructureVariables: with a complete environment across
// every infrastructure group -- Redis, the full S3 group and its optional
// refinements, the SMTP pair and its optional refinements, and the SMS
// gateway URL -- Load resolves every field and records it as from-env,
// the infrastructure counterpart of TestLoadReadsSetVariables above.
func TestLoadReadsInfrastructureVariables(t *testing.T) {
	cfg, err := Load("cli-app", envFromMap(map[string]string{
		RedisAddrEnv:     "redis.internal:6379",
		S3EndpointEnv:    "s3.internal:9000",
		S3BucketEnv:      "smiles",
		S3AccessKeyEnv:   "AKIAEXAMPLE",
		S3SecretKeyEnv:   "s3cr3t",
		S3RegionEnv:      "us-east-1",
		S3UseSSLEnv:      "true",
		SMTPHostEnv:      "smtp.internal",
		SMTPPortEnv:      "587",
		SMTPUsernameEnv:  "mailer",
		SMTPPasswordEnv:  "hunter2",
		SMSGatewayURLEnv: "http://sms.internal/send",
	}))
	if err != nil {
		t.Fatalf("Load with a complete infrastructure environment failed: %v", err)
	}
	if cfg.RedisAddr != "redis.internal:6379" || !cfg.RedisAddrFromEnv {
		t.Errorf("RedisAddr = %q (fromEnv %v), want the set value recorded as from-env", cfg.RedisAddr, cfg.RedisAddrFromEnv)
	}
	if cfg.S3Endpoint != "s3.internal:9000" || cfg.S3Bucket != "smiles" ||
		cfg.S3AccessKey != "AKIAEXAMPLE" || cfg.S3SecretKey != "s3cr3t" || cfg.S3Region != "us-east-1" || !cfg.S3UseSSL {
		t.Errorf("S3 fields did not resolve to the set values: %+v", cfg)
	}
	if !cfg.S3EndpointFromEnv || !cfg.S3BucketFromEnv || !cfg.S3AccessKeyFromEnv ||
		!cfg.S3SecretKeyFromEnv || !cfg.S3RegionFromEnv || !cfg.S3UseSSLFromEnv {
		t.Error("a complete S3 group must record every field as from-env")
	}
	if cfg.SMTPHost != "smtp.internal" || cfg.SMTPPort != 587 || cfg.SMTPUsername != "mailer" || cfg.SMTPPassword != "hunter2" {
		t.Errorf("SMTP fields did not resolve to the set values: %+v", cfg)
	}
	if !cfg.SMTPHostFromEnv || !cfg.SMTPPortFromEnv || !cfg.SMTPUsernameFromEnv || !cfg.SMTPPasswordFromEnv {
		t.Error("a complete SMTP pair must record every field as from-env")
	}
	if cfg.SMSGatewayURL != "http://sms.internal/send" || !cfg.SMSGatewayURLFromEnv {
		t.Errorf("SMSGatewayURL = %q (fromEnv %v), want the set value recorded as from-env", cfg.SMSGatewayURL, cfg.SMSGatewayURLFromEnv)
	}
}

// TestLoadInfrastructureVariablesDefaultToUnwired: with an empty
// environment, every infrastructure field resolves to its zero value --
// empty strings, S3UseSSL false, SMTPPort 0 -- leaving every seam on its
// Preset default, and no field is recorded as from-env: the same "empty
// counts as unset" contract the original five variables already carry.
// TestLoadReadsInfrastructureVariables above is what proves the wired
// reading of the same fields.
func TestLoadInfrastructureVariablesDefaultToUnwired(t *testing.T) {
	cfg, err := Load("cli-app", envFromMap(nil))
	if err != nil {
		t.Fatalf("Load with an empty environment failed: %v", err)
	}
	if cfg.RedisAddr != "" || cfg.S3Endpoint != "" || cfg.S3Bucket != "" || cfg.S3AccessKey != "" ||
		cfg.S3SecretKey != "" || cfg.S3Region != "" || cfg.S3UseSSL || cfg.SMTPHost != "" ||
		cfg.SMTPPort != 0 || cfg.SMTPUsername != "" || cfg.SMTPPassword != "" || cfg.SMSGatewayURL != "" {
		t.Errorf("an empty environment must leave every infrastructure field at its zero value, got %+v", cfg)
	}
	if cfg.RedisAddrFromEnv || cfg.S3EndpointFromEnv || cfg.S3BucketFromEnv || cfg.S3AccessKeyFromEnv ||
		cfg.S3SecretKeyFromEnv || cfg.S3RegionFromEnv || cfg.S3UseSSLFromEnv || cfg.SMTPHostFromEnv ||
		cfg.SMTPPortFromEnv || cfg.SMTPUsernameFromEnv || cfg.SMTPPasswordFromEnv || cfg.SMSGatewayURLFromEnv {
		t.Error("an empty environment must record every infrastructure field as not-from-env")
	}
}

// TestLoadRefusesIncompleteS3Group: an S3 group missing two of its four
// required members fails with the template's own completeness error
// naming exactly which variables are missing -- the unit-level pin behind
// config.TestPrintRefusesIncompleteS3Group's command-level proof. Load
// must never accept a partial infrastructure group and resolve it
// silently.
func TestLoadRefusesIncompleteS3Group(t *testing.T) {
	_, err := Load("cli-app", envFromMap(map[string]string{
		S3EndpointEnv: "s3.internal:9000",
		S3BucketEnv:   "smiles",
		// S3AccessKeyEnv and S3SecretKeyEnv deliberately left unset.
	}))
	if err == nil {
		t.Fatal("Load accepted an incomplete S3 group")
	}
	want := "cli-app: an S3 ObjectStore composition needs APP_S3_ACCESS_KEY, APP_S3_SECRET_KEY set too " +
		"(got some but not all of APP_S3_ENDPOINT/APP_S3_BUCKET/APP_S3_ACCESS_KEY/APP_S3_SECRET_KEY)"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// TestLoadRefusesIncompleteSMTPPair: an SMTP pair missing its host fails
// with the template's own completeness error, the pair's mirror of
// TestLoadRefusesIncompleteS3Group above.
func TestLoadRefusesIncompleteSMTPPair(t *testing.T) {
	_, err := Load("cli-app", envFromMap(map[string]string{
		SMTPPortEnv: "587",
		// SMTPHostEnv deliberately left unset.
	}))
	if err == nil {
		t.Fatal("Load accepted an incomplete SMTP pair")
	}
	want := "cli-app: an SMTP Mailer composition needs both APP_SMTP_HOST and APP_SMTP_PORT set"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// TestLoadS3UseSSLMustBeAValidBool: a non-bool APP_S3_USE_SSL fails with
// the template's own parse error.
func TestLoadS3UseSSLMustBeAValidBool(t *testing.T) {
	_, err := Load("cli-app", envFromMap(map[string]string{S3UseSSLEnv: "maybe"}))
	if err == nil {
		t.Fatal("Load accepted a non-bool APP_S3_USE_SSL")
	}
	if !strings.HasPrefix(err.Error(), `cli-app: APP_S3_USE_SSL must be a valid bool, got "maybe": `) {
		t.Errorf("error = %q, want the template's APP_S3_USE_SSL bool-parse prefix", err)
	}
}

// TestLoadSMTPPortMustBeAValidNumber: a non-numeric APP_SMTP_PORT fails
// with the template's own parse error, once its pair partner (the host)
// is also set so the completeness check does not short-circuit first.
func TestLoadSMTPPortMustBeAValidNumber(t *testing.T) {
	_, err := Load("cli-app", envFromMap(map[string]string{
		SMTPHostEnv: "smtp.internal",
		SMTPPortEnv: "not-a-port",
	}))
	if err == nil {
		t.Fatal("Load accepted a non-numeric APP_SMTP_PORT")
	}
	if !strings.HasPrefix(err.Error(), `cli-app: APP_SMTP_PORT must be a valid port number, got "not-a-port": `) {
		t.Errorf("error = %q, want the template's APP_SMTP_PORT parse-error prefix", err)
	}
}

// envVarDeclPattern matches one "<identifier>Env = \"<VALUE>\"" constant
// declaration, the exact shape every one of the twenty bootstrap
// variable names takes in both the template's config.go and this
// package's own const block -- an identifier ending in the literal "Env"
// assigned a quoted environment-variable-name string literal, on its own
// line inside a const block. Comment lines (including the #nosec
// exceptions both sides carry on the two credential-shaped names) do not
// match, since they do not fit the "identifier = "VALUE"" shape at all.
var envVarDeclPattern = regexp.MustCompile(`(?m)^\s*[A-Za-z0-9]+Env\s*=\s*"([A-Za-z0-9_]+)"`)

// extractEnvVarNames returns the set of environment-variable-name string
// values assigned to an "...Env"-suffixed identifier in src.
func extractEnvVarNames(src string) map[string]bool {
	names := map[string]bool{}
	for _, m := range envVarDeclPattern.FindAllStringSubmatch(src, -1) {
		names[m[1]] = true
	}
	return names
}

// TestAppConfigEnvSetMatchesTheTemplateExactly is the drift-proof set
// equality between the two sides: it extracts every "...Env = "VALUE""
// declaration from the embedded template's own config.go source text --
// never a hand-maintained list this test could silently fall behind, so a
// template edit is caught even before anyone updates this file -- and
// asserts the twin's own exported Env constants cover exactly that set, in
// both directions. The twin's own side is built from the actual exported
// Go constants (not from copied string literals), so renaming or removing
// one of them fails this file to even compile, a second, stronger drift
// signal than the runtime check below.
func TestAppConfigEnvSetMatchesTheTemplateExactly(t *testing.T) {
	content, err := template.Project.ReadFile("project/cmd/server/config.go")
	if err != nil {
		t.Fatalf("read the embedded template config.go: %v", err)
	}
	templateVars := extractEnvVarNames(string(content))
	if len(templateVars) == 0 {
		t.Fatal("extracted zero environment variable names from the template; the extraction pattern itself has drifted")
	}

	twinVars := map[string]bool{
		DeploymentModeEnv: true, PortEnv: true, DBPathEnv: true,
		ConfigKeyEnv: true, OrgIndexKeyEnv: true,
		AuthnBlindIndexKeyEnv: true, AuthnPIICipherKeyEnv: true, PKILocalKeyCipherKeyEnv: true,
		RedisAddrEnv:  true,
		S3EndpointEnv: true, S3BucketEnv: true, S3AccessKeyEnv: true, S3SecretKeyEnv: true,
		S3RegionEnv: true, S3UseSSLEnv: true,
		SMTPHostEnv: true, SMTPPortEnv: true, SMTPUsernameEnv: true, SMTPPasswordEnv: true,
		SMSGatewayURLEnv: true,
	}

	for name := range templateVars {
		if !twinVars[name] {
			t.Errorf("template config.go parses %s but the appconfig twin does not support it; the twin has drifted behind the template", name)
		}
	}
	for name := range twinVars {
		if !templateVars[name] {
			t.Errorf("appconfig supports %s but the template config.go does not parse it; the twin claims a variable the app does not have", name)
		}
	}
}

// TestAppConfigIsTheGeneratedProjectsTwin re-reads the embedded template
// project's cmd/server/config.go and fails when the two sides drift: every
// variable name, the parse order, the defaults, the two error format
// strings and the two development key byte sequences. A template edit that
// changes any of these without its twin failing is an edit this test
// exists to make impossible -- a generated project that boots on values
// saasctl no longer resolves would strand every project the CLI maintains.
func TestAppConfigIsTheGeneratedProjectsTwin(t *testing.T) {
	content, err := template.Project.ReadFile("project/cmd/server/config.go")
	if err != nil {
		t.Fatalf("read the embedded template config.go: %v", err)
	}
	src := string(content)

	// The twenty variable names and the one scalar default, declared in
	// the template as <local name> = "<value>" inside its const block.
	for local, want := range map[string]string{
		"deploymentModeEnv":       DeploymentModeEnv,
		"portEnv":                 PortEnv,
		"dbPathEnv":               DBPathEnv,
		"configKeyEnv":            ConfigKeyEnv,
		"orgIndexKeyEnv":          OrgIndexKeyEnv,
		"authnBlindIndexKeyEnv":   AuthnBlindIndexKeyEnv,
		"authnPIICipherKeyEnv":    AuthnPIICipherKeyEnv,
		"pkiLocalKeyCipherKeyEnv": PKILocalKeyCipherKeyEnv,
		"redisAddrEnv":            RedisAddrEnv,
		"s3EndpointEnv":           S3EndpointEnv,
		"s3BucketEnv":             S3BucketEnv,
		"s3AccessKeyEnv":          S3AccessKeyEnv,
		"s3SecretKeyEnv":          S3SecretKeyEnv,
		"s3RegionEnv":             S3RegionEnv,
		"s3UseSSLEnv":             S3UseSSLEnv,
		"smtpHostEnv":             SMTPHostEnv,
		"smtpPortEnv":             SMTPPortEnv,
		"smtpUsernameEnv":         SMTPUsernameEnv,
		"smtpPasswordEnv":         SMTPPasswordEnv,
		"smsGatewayURLEnv":        SMSGatewayURLEnv,
		"defaultPort":             defaultPort,
	} {
		// gofmt aligns "=" across a const block's declarations, so the
		// number of spaces before it varies with the block's longest name
		// (s3EndpointEnv  = ..., s3BucketEnv    = ...); match any run of
		// whitespace there rather than the single space a literal
		// substring check would require.
		decl := regexp.MustCompile(fmt.Sprintf(`%s\s*=\s*"%s"`, regexp.QuoteMeta(local), regexp.QuoteMeta(want)))
		if !decl.MatchString(src) {
			t.Errorf("template does not declare %s (want %q); the twin has drifted", local, want)
		}
	}
	decl := fmt.Sprintf("configKeyHexLength = %d", configKeyHexLength)
	if !strings.Contains(src, decl) {
		t.Errorf("template does not declare %s; the twin has drifted", decl)
	}
	if !strings.Contains(src, `defaultSQLitePath = "`+defaultSQLitePath+`"`) {
		t.Errorf("template's defaultSQLitePath is not the twin's fixed %q literal; the two defaults must be one value (see defaultSQLitePath's doc comment for why it is fixed rather than __APP_NAME__-derived)", defaultSQLitePath)
	}
	if strings.Contains(src, `defaultSQLitePath = "__APP_NAME__.db"`) {
		t.Error("template's defaultSQLitePath still derives from the __APP_NAME__ token; a fixed literal is what keeps the twin honest across a module-path rename")
	}

	// The error texts: Load must produce byte-identical messages to the
	// template's, with the real app name in the token's place. The length
	// message is pinned end to end here; the template's format string is
	// asserted separately so a rewording fails on both sides at once.
	if _, err := Load("__APP_NAME__", envFromMap(map[string]string{ConfigKeyEnv: "abc"})); err == nil {
		t.Fatal("Load accepted a short key during the parity check")
	} else if want := `__APP_NAME__: APP_CONFIG_KEY must hold 64 hex characters (a 32-byte key), got 3`; err.Error() != want {
		t.Errorf("Load error = %q, want the template's %q", err, want)
	}
	if !strings.Contains(src, `"__APP_NAME__: %s must hold %d hex characters (a 32-byte key), got %d"`) {
		t.Error("template's length-error format string drifted from the twin's")
	}
	if !strings.Contains(src, `"__APP_NAME__: %s: %w"`) {
		t.Error("template's decode-error format string drifted from the twin's")
	}
	if !strings.Contains(src, "string(pkgcore.DeploymentModeStandalone)") {
		t.Error("template no longer defaults the mode through pkgcore.DeploymentModeStandalone")
	}

	// The infrastructure-group completeness errors: Load must produce
	// byte-identical messages to the template's for an incomplete S3 group
	// and an incomplete SMTP pair, and the template's own format strings
	// must still be there to drift against.
	if _, err := Load("__APP_NAME__", envFromMap(map[string]string{S3EndpointEnv: "e"})); err == nil {
		t.Fatal("Load accepted a lone APP_S3_ENDPOINT during the parity check")
	} else if want := `__APP_NAME__: an S3 ObjectStore composition needs APP_S3_BUCKET, APP_S3_ACCESS_KEY, APP_S3_SECRET_KEY set too ` +
		`(got some but not all of APP_S3_ENDPOINT/APP_S3_BUCKET/APP_S3_ACCESS_KEY/APP_S3_SECRET_KEY)`; err.Error() != want {
		t.Errorf("Load error = %q, want the template's %q", err, want)
	}
	if !strings.Contains(src, `"__APP_NAME__: an S3 ObjectStore composition needs %s set too (got some but not all of %s/%s/%s/%s)"`) {
		t.Error("template's S3-completeness error format string drifted from the twin's")
	}
	if _, err := Load("__APP_NAME__", envFromMap(map[string]string{SMTPHostEnv: "h"})); err == nil {
		t.Fatal("Load accepted a lone APP_SMTP_HOST during the parity check")
	} else if want := `__APP_NAME__: an SMTP Mailer composition needs both APP_SMTP_HOST and APP_SMTP_PORT set`; err.Error() != want {
		t.Errorf("Load error = %q, want the template's %q", err, want)
	}
	if !strings.Contains(src, `"__APP_NAME__: an SMTP Mailer composition needs both %s and %s set"`) {
		t.Error("template's SMTP-completeness error format string drifted from the twin's")
	}
	if !strings.Contains(src, `"__APP_NAME__: %s must be a valid bool, got %q: %w"`) {
		t.Error("template's S3-use-SSL bool-parse error format string drifted from the twin's")
	}
	if !strings.Contains(src, `"__APP_NAME__: %s must be a valid port number, got %q: %w"`) {
		t.Error("template's SMTP-port parse error format string drifted from the twin's")
	}

	// The development key bytes, asserted as their byte-for-byte hex
	// literals after whitespace normalization, so a template edit that
	// reorders, adds or drops a byte fails here. All five dev keys are
	// checked -- the three authn/pki ones joined the family when their
	// environment variables did (the keys must resolve byte-identically
	// whether the app boots or saasctl resolves them).
	normalized := regexp.MustCompile(`\s+`).ReplaceAllString(src, "")
	for _, key := range []struct {
		name string
		dev  []byte
	}{
		{"devConfigKey", devConfigKey},
		{"devOrgIndexKey", devOrgIndexKey},
		{"devBlindIndexKey", devBlindIndexKey},
		{"devPIICipherKey", devPIICipherKey},
		{"devPKILocalKeyCipherKey", devPKILocalKeyCipherKey},
	} {
		if !strings.Contains(normalized, keyByteLiteral(key.dev)) {
			t.Errorf("template's %s bytes drifted from the twin's", key.name)
		}
	}

	// The parse order: configFromEnv reads deployment mode first and the
	// org index key last, in the same order Load resolves its fields.
	// configFromEnv reaches the five variables through the constants
	// declared above the function, whose name/value pairs the const checks
	// above pin, so the order check scans the function body for the first
	// use of each constant name -- which is the read itself. Comments are
	// stripped first, so prose inside the body cannot satisfy the check by
	// text position either.
	body := src[strings.Index(src, "func configFromEnv"):]
	body = regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(body, "")
	parseOrder := []string{
		"deploymentModeEnv", "portEnv", "dbPathEnv", "configKeyEnv", "orgIndexKeyEnv",
		"authnBlindIndexKeyEnv", "authnPIICipherKeyEnv", "pkiLocalKeyCipherKeyEnv",
		"redisAddrEnv", "s3EndpointEnv", "s3BucketEnv", "s3AccessKeyEnv", "s3SecretKeyEnv",
		"s3UseSSLEnv", "smtpHostEnv", "smtpPortEnv", "s3RegionEnv", "smtpUsernameEnv",
		"smtpPasswordEnv", "smsGatewayURLEnv",
	}
	last := -1
	for _, marker := range parseOrder {
		pos := strings.Index(body, marker)
		if pos < 0 {
			t.Errorf("configFromEnv body lacks the variable %q", marker)
			continue
		}
		if pos < last {
			t.Errorf("configFromEnv reads %q out of Load's order", marker)
		}
		last = pos
	}
}

// TestEffectiveDBPath pins the anchoring rule db migrate and config print
// share through this one function: an absolute SQLitePath is used exactly
// as the generated app would use it, and a relative one -- the app.db
// default or a relative APP_DB_PATH -- joins onto the directory of the
// go.mod argument, whatever form that argument takes. A relative go.mod
// argument's directory and an absolute one denote the same directory
// (both resolve against the process working directory), so the joined
// results name the same file.
func TestEffectiveDBPath(t *testing.T) {
	tests := []struct {
		name       string
		modPath    string
		sqlitePath string
		want       string
	}{
		{name: "absolute APP_DB_PATH used verbatim", modPath: "/proj/go.mod", sqlitePath: "/db/app.db", want: "/db/app.db"},
		{name: "relative APP_DB_PATH anchored to an absolute go.mod directory", modPath: "/proj/go.mod", sqlitePath: "db.sqlite", want: "/proj/db.sqlite"},
		{name: "relative APP_DB_PATH anchored to a relative go.mod directory", modPath: "sub/go.mod", sqlitePath: "db.sqlite", want: "sub/db.sqlite"},
		{name: "default go.mod argument keeps the plain name", modPath: "go.mod", sqlitePath: "app.db", want: "app.db"},
		{name: "app.db default anchored to another project", modPath: "/other/go.mod", sqlitePath: "app.db", want: "/other/app.db"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			got := (Config{SQLitePath: tt.sqlitePath}).EffectiveDBPath(tt.modPath)
			if got != tt.want {
				t.Errorf("EffectiveDBPath(%q, %q) = %q, want %q", tt.modPath, tt.sqlitePath, got, tt.want)
			}
		})
	}
}

// keyByteLiteral renders one development key as its normalized byte
// literals, e.g. "0x00,0x01,...,0x1f", for template-source comparison.
func keyByteLiteral(key []byte) string {
	var b strings.Builder
	for i, v := range key {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "0x%02x", v)
	}
	return b.String()
}
