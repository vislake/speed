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
// the <app name>.db path -- plus the documented development key bytes, and
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
	if cfg.SQLitePath != "cli-app.db" {
		t.Errorf("SQLitePath = %q, want the <app name>.db default", cfg.SQLitePath)
	}
	if !bytes.Equal(cfg.ConfigKey, devConfigKey) {
		t.Errorf("ConfigKey is not the ascending 0x00..0x1f development default")
	}
	if !bytes.Equal(cfg.OrgIndexKey, devOrgIndexKey) {
		t.Errorf("OrgIndexKey is not the descending 0xff..0xe0 development default")
	}
	if cfg.DeploymentModeFromEnv || cfg.PortFromEnv || cfg.SQLitePathFromEnv ||
		cfg.ConfigKeyFromEnv || cfg.OrgIndexKeyFromEnv {
		t.Error("an empty environment must record every field as not-from-env")
	}
}

// TestLoadReadsSetVariables: each variable that carries a non-empty value
// is parsed and recorded as from-env, with the two key variables decoding
// their hex into the 32 bytes they encode.
func TestLoadReadsSetVariables(t *testing.T) {
	configKeyHex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	orgIndexKeyHex := "ffe0f1d2c3b4a5968778695a4b3c2d1e0f00112233445566778899aabbccddee"
	cfg, err := Load("cli-app", envFromMap(map[string]string{
		DeploymentModeEnv: "Distributed",
		PortEnv:           "9090",
		DBPathEnv:         "/var/data/smile.db",
		ConfigKeyEnv:      configKeyHex,
		OrgIndexKeyEnv:    orgIndexKeyHex,
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
	if !cfg.DeploymentModeFromEnv || !cfg.PortFromEnv || !cfg.SQLitePathFromEnv ||
		!cfg.ConfigKeyFromEnv || !cfg.OrgIndexKeyFromEnv {
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

// TestLoadMalformedConfigKeyErrorMatchesTheTemplate: a SPEED_CONFIG_KEY of
// the wrong encoded length fails with the template's exact message, app
// name where the template has its __APP_NAME__ token.
func TestLoadMalformedConfigKeyErrorMatchesTheTemplate(t *testing.T) {
	_, err := Load("cli-app", envFromMap(map[string]string{ConfigKeyEnv: "abc"}))
	if err == nil {
		t.Fatal("Load accepted a short config key")
	}
	want := "cli-app: SPEED_CONFIG_KEY must hold 64 hex characters (a 32-byte key), got 3"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// TestLoadNonHexConfigKeyErrorNamesTheVariable: a SPEED_CONFIG_KEY of the
// right length that is not valid hex fails under the template's decode
// message, naming the variable so an operator knows which secret is
// malformed.
func TestLoadNonHexConfigKeyErrorNamesTheVariable(t *testing.T) {
	encoded := strings.Repeat("z", 64)
	_, err := Load("cli-app", envFromMap(map[string]string{ConfigKeyEnv: encoded}))
	if err == nil {
		t.Fatal("Load accepted a non-hex config key")
	}
	if !strings.HasPrefix(err.Error(), "cli-app: SPEED_CONFIG_KEY: ") {
		t.Errorf("error = %q, want the cli-app: SPEED_CONFIG_KEY: prefix", err)
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
	want := "cli-app: SPEED_ORG_INDEX_KEY must hold 64 hex characters (a 32-byte key), got 3"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
}

// TestLoadSetButEmptyCountsAsUnset: a variable that is present but empty
// resolves to the same default as an absent one and is not recorded as
// from-env -- os.Getenv's own semantics, which the generated server and
// this twin share.
func TestLoadSetButEmptyCountsAsUnset(t *testing.T) {
	cfg, err := Load("cli-app", envFromMap(map[string]string{
		DeploymentModeEnv: "",
		PortEnv:           "",
		DBPathEnv:         "",
		ConfigKeyEnv:      "",
		OrgIndexKeyEnv:    "",
	}))
	if err != nil {
		t.Fatalf("Load with all-empty variables failed: %v", err)
	}
	if cfg.DeploymentMode != pkgcore.DeploymentModeStandalone || cfg.Port != "8080" || cfg.SQLitePath != "cli-app.db" {
		t.Errorf("empty variables must resolve to the defaults, got %q/%q/%q", cfg.DeploymentMode, cfg.Port, cfg.SQLitePath)
	}
	if cfg.DeploymentModeFromEnv || cfg.PortFromEnv || cfg.SQLitePathFromEnv ||
		cfg.ConfigKeyFromEnv || cfg.OrgIndexKeyFromEnv {
		t.Error("set-but-empty variables must not be recorded as from-env")
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

	// The five variable names and the two scalar defaults, declared in the
	// template as <local name> = "<value>" inside its const block.
	for local, want := range map[string]string{
		"deploymentModeEnv": DeploymentModeEnv,
		"portEnv":           PortEnv,
		"dbPathEnv":         DBPathEnv,
		"configKeyEnv":      ConfigKeyEnv,
		"orgIndexKeyEnv":    OrgIndexKeyEnv,
		"defaultPort":       defaultPort,
	} {
		decl := fmt.Sprintf(`%s = "%s"`, local, want)
		if !strings.Contains(src, decl) {
			t.Errorf("template does not declare %s (want %q); the twin has drifted", local, want)
		}
	}
	decl := fmt.Sprintf("configKeyHexLength = %d", configKeyHexLength)
	if !strings.Contains(src, decl) {
		t.Errorf("template does not declare %s; the twin has drifted", decl)
	}
	if !strings.Contains(src, `defaultSQLitePath = "__APP_NAME__.db"`) {
		t.Error("template's default SQLite path is not the __APP_NAME__.db token form")
	}

	// The error texts: Load must produce byte-identical messages to the
	// template's, with the real app name in the token's place. The length
	// message is pinned end to end here; the template's format string is
	// asserted separately so a rewording fails on both sides at once.
	if _, err := Load("__APP_NAME__", envFromMap(map[string]string{ConfigKeyEnv: "abc"})); err == nil {
		t.Fatal("Load accepted a short key during the parity check")
	} else if want := `__APP_NAME__: SPEED_CONFIG_KEY must hold 64 hex characters (a 32-byte key), got 3`; err.Error() != want {
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

	// The development key bytes, asserted as their byte-for-byte hex
	// literals after whitespace normalization, so a template edit that
	// reorders, adds or drops a byte fails here.
	ascending := keyByteLiteral(devConfigKey)
	descending := keyByteLiteral(devOrgIndexKey)
	normalized := regexp.MustCompile(`\s+`).ReplaceAllString(src, "")
	if !strings.Contains(normalized, ascending) {
		t.Error("template's devConfigKey bytes drifted from the twin's ascending 0x00..0x1f sequence")
	}
	if !strings.Contains(normalized, descending) {
		t.Error("template's devOrgIndexKey bytes drifted from the twin's descending 0xff..0xe0 sequence")
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
	parseOrder := []string{"deploymentModeEnv", "portEnv", "dbPathEnv", "configKeyEnv", "orgIndexKeyEnv"}
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
