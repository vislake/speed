package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/saasctl/internal/appconfig"
	"github.com/vislake/speed/go/saasctl/internal/appconfig/appconfigtest"
)

// bootstrapEnvKeys lists the environment surface a generated project's
// bootstrap reads -- the full twenty-three variables appconfig resolves,
// exported for the tests and examples that must clear or restore them all.
// Each package that clears the surface carries its own copy next to the
// code that uses it.
var bootstrapEnvKeys = []string{
	appconfig.DeploymentModeEnv,
	appconfig.PortEnv,
	appconfig.DBPathEnv,
	appconfig.ConfigKeyEnv,
	appconfig.OrgIndexKeyEnv,
	appconfig.AuthnBlindIndexKeyEnv,
	appconfig.AuthnPIICipherKeyEnv,
	appconfig.PKILocalKeyCipherKeyEnv,
	appconfig.NotificationIndexKeyEnv,
	appconfig.RedisAddrEnv,
	appconfig.OTLPEndpointEnv,
	appconfig.S3EndpointEnv,
	appconfig.S3BucketEnv,
	appconfig.S3AccessKeyEnv,
	appconfig.S3SecretKeyEnv,
	appconfig.S3RegionEnv,
	appconfig.S3UseSSLEnv,
	appconfig.S3BucketLookupEnv,
	appconfig.SMTPHostEnv,
	appconfig.SMTPPortEnv,
	appconfig.SMTPUsernameEnv,
	appconfig.SMTPPasswordEnv,
	appconfig.SMSGatewayURLEnv,
}

// TestBootstrapEnvKeysMatchesTheAppConfigSurface: the list must name exactly
// the environment variables appconfig.Load resolves -- no more, no fewer,
// none twice -- so a variable joining or leaving the bootstrap surface fails
// here until this list moves with it. The expectation is appconfig.Load's
// own behavior, derived by appconfigtest, never a second hand-maintained
// list.
func TestBootstrapEnvKeysMatchesTheAppConfigSurface(t *testing.T) {
	appconfigtest.AssertKeys(t, bootstrapEnvKeys)
}

// clearBootstrapEnv empties every bootstrap variable through t.Setenv, so
// a test starts from the same all-defaults state a fresh shell would be
// in. Empty counts as unset -- matching os.Getenv, the generated app's
// own view of the environment.
func clearBootstrapEnv(t *testing.T) {
	t.Helper()
	for _, key := range bootstrapEnvKeys {
		t.Setenv(key, "")
	}
}

// drivePrint runs the print subcommand through the group's Run under a
// cleared bootstrap environment plus env, and returns its exit code and
// the two captured output streams.
func drivePrint(t *testing.T, extraArgs []string, env map[string]string) (code int, stdout, stderr string) {
	t.Helper()
	clearBootstrapEnv(t)
	for key, value := range env {
		t.Setenv(key, value)
	}
	args := []string{"print"}
	args = append(args, extraArgs...)
	var out, errOut bytes.Buffer
	code = Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// fixture returns the absolute path of one go.mod fixture in testdata.
func fixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", name)
}

// TestPrintResolvesAndRendersTheDocumentedDefaults: with an empty
// environment, print renders what the generated app boots on with no
// environment at all -- the standalone deployment mode, port 8080, the
// fixed app.db path, the six development key byte sequences (the two
// original key materials plus the four authn/pki/notification ones), and
// every infrastructure seam left on its builtin default -- one line per
// value, each sourced line naming the default (or the seam) it fell back
// to. The six key rows and the S3 secret key / SMTP password / SMS
// gateway URL rows show only the [redacted] marker in the value column.
// The sqlite path row's value column shows the EFFECTIVE file -- the
// relative app.db default anchored to the fixture go.mod's directory
// (testdata/, the directory the app is documented to run from) -- while
// its source column keeps the raw
// default literal; the fixture's go.mod argument is relative, so the
// anchored file reads "testdata/app.db" (the absolute form of the same
// file, for an absolute go.mod argument, is the regression test below's
// subject).
//
// Before the appconfig twin covered the full bootstrap surface, this
// rendered only the first five lines: the rows below are the regression
// proof that config print now resolves the SAME environment the generated
// app's own configFromEnv resolves, not a truncated subset of it.
func TestPrintResolvesAndRendersTheDocumentedDefaults(t *testing.T) {
	code, stdout, stderr := drivePrint(t, []string{fixture(t, "print.mod")}, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	want := "deployment mode  standalone   unset or empty (default standalone)\n" +
		"port             8080         unset or empty (default 8080)\n" +
		"sqlite path      testdata/app.db unset or empty (default app.db)\n" +
		"config key       [redacted]   unset or empty (development default)\n" +
		"org index key    [redacted]   unset or empty (development default)\n" +
		"authn blind index key [redacted]   unset or empty (development default)\n" +
		"authn pii cipher key [redacted]   unset or empty (development default)\n" +
		"pki local key cipher key [redacted]   unset or empty (development default)\n" +
		"notification index key [redacted]   unset or empty (development default)\n" +
		"redis addr                    unset or empty (eventbus/kv stay on the in-process default)\n" +
		"otlp endpoint                 unset or empty (observability stays on the local exporters)\n" +
		"s3 endpoint                   unset or empty (objectstore stays on the local-directory default)\n" +
		"s3 bucket                     unset or empty (objectstore stays on the local-directory default)\n" +
		"s3 access key                 unset or empty (objectstore stays on the local-directory default)\n" +
		"s3 secret key    [redacted]   unset or empty (objectstore stays on the local-directory default)\n" +
		"s3 region                     unset or empty (optional S3 refinement; used only when the group above is set)\n" +
		"s3 use ssl       false        unset or empty (default false)\n" +
		"s3 bucket lookup auto         unset or empty (default auto)\n" +
		"smtp host                     unset or empty (mailer stays on the console default)\n" +
		"smtp port                     unset or empty (mailer stays on the console default)\n" +
		"smtp username                 unset or empty (optional SMTP refinement; used only when the group above is set)\n" +
		"smtp password    [redacted]   unset or empty (optional SMTP refinement; used only when the group above is set)\n" +
		"sms gateway url  [redacted]   unset or empty (SMS sender seam left unwired: console default under standalone, refused under distributed)\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestPrintReportsEveryValueThatCameFromTheEnvironment: with every
// bootstrap variable set -- including a complete S3 group, a complete SMTP
// pair and their optional refinements -- every line shows the resolved
// value and names the variable that carried it: the generated app's own
// resolution, reported with its provenance. This is the mirror of the
// defaults case above: before the twin covered the full surface, setting
// these twelve infrastructure variables changed nothing about print's
// output (they were silently ignored), which this test's line count and
// per-row "from APP_*" provenance would have caught. The sqlite path row
// shows both sides of its value: the effective file -- the relative
// APP_DB_PATH anchored to the fixture go.mod's directory (testdata/, the
// file a boot from that directory opens) -- in the value column, and the
// raw value it came from in the source column's parenthetical, since the
// two differ whenever APP_DB_PATH is relative.
func TestPrintReportsEveryValueThatCameFromTheEnvironment(t *testing.T) {
	code, stdout, stderr := drivePrint(t, []string{fixture(t, "print.mod")}, map[string]string{
		appconfig.DeploymentModeEnv:       "distributed",
		appconfig.PortEnv:                 "9090",
		appconfig.DBPathEnv:               "db.sqlite",
		appconfig.ConfigKeyEnv:            "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
		appconfig.OrgIndexKeyEnv:          "ffe0f1d2c3b4a5968778695a4b3c2d1e0f00112233445566778899aabbccddee",
		appconfig.AuthnBlindIndexKeyEnv:   "404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f",
		appconfig.AuthnPIICipherKeyEnv:    "606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f",
		appconfig.PKILocalKeyCipherKeyEnv: "808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9f",
		appconfig.NotificationIndexKeyEnv: "202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f",
		appconfig.RedisAddrEnv:            "redis.internal:6379",
		appconfig.OTLPEndpointEnv:         "collector.internal:4317",
		appconfig.S3EndpointEnv:           "s3.internal:9000",
		appconfig.S3BucketEnv:             "smiles",
		appconfig.S3AccessKeyEnv:          "AKIAEXAMPLE",
		appconfig.S3SecretKeyEnv:          "s3cr3t",
		appconfig.S3RegionEnv:             "us-east-1",
		appconfig.S3UseSSLEnv:             "true",
		appconfig.S3BucketLookupEnv:       "virtual_host",
		appconfig.SMTPHostEnv:             "smtp.internal",
		appconfig.SMTPPortEnv:             "587",
		appconfig.SMTPUsernameEnv:         "mailer",
		appconfig.SMTPPasswordEnv:         "hunter2",
		appconfig.SMSGatewayURLEnv:        "http://sms.internal/send",
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	want := "deployment mode  distributed  from APP_DEPLOYMENT_MODE\n" +
		"port             9090         from PORT\n" +
		"sqlite path      testdata/db.sqlite from APP_DB_PATH (raw value: db.sqlite)\n" +
		"config key       [redacted]   from APP_CONFIG__CIPHER_KEY\n" +
		"org index key    [redacted]   from APP_ORG__INVITATION_EMAIL_INDEX_KEY\n" +
		"authn blind index key [redacted]   from APP_AUTHN__BLIND_INDEX_KEY\n" +
		"authn pii cipher key [redacted]   from APP_AUTHN__PII_CIPHER_KEY\n" +
		"pki local key cipher key [redacted]   from APP_PKI__LOCAL_KEY_CIPHER_KEY\n" +
		"notification index key [redacted]   from APP_NOTIFICATION__CONTACT_INDEX_KEY\n" +
		"redis addr       redis.internal:6379 from APP_REDIS_ADDR\n" +
		"otlp endpoint    collector.internal:4317 from APP_OTLP_ENDPOINT\n" +
		"s3 endpoint      s3.internal:9000 from APP_S3_ENDPOINT\n" +
		"s3 bucket        smiles       from APP_S3_BUCKET\n" +
		"s3 access key    AKIAEXAMPLE  from APP_S3_ACCESS_KEY\n" +
		"s3 secret key    [redacted]   from APP_S3_SECRET_KEY\n" +
		"s3 region        us-east-1    from APP_S3_REGION\n" +
		"s3 use ssl       true         from APP_S3_USE_SSL\n" +
		"s3 bucket lookup virtual_host from APP_S3_BUCKET_LOOKUP\n" +
		"smtp host        smtp.internal from APP_SMTP_HOST\n" +
		"smtp port        587          from APP_SMTP_PORT\n" +
		"smtp username    mailer       from APP_SMTP_USERNAME\n" +
		"smtp password    [redacted]   from APP_SMTP_PASSWORD\n" +
		"sms gateway url  [redacted]   from APP_SMS_GATEWAY_URL\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestPrintRefusesIncompleteS3Group: an S3 group missing one of its four
// required members fails print exactly as it fails the generated app's own
// bootstrap -- the coded completeness error naming which variables are
// missing -- rather than printing partial rows and exiting 0 on an
// environment the generated app would refuse to boot on.
func TestPrintRefusesIncompleteS3Group(t *testing.T) {
	code, stdout, stderr := drivePrint(t, []string{fixture(t, "print.mod")}, map[string]string{
		appconfig.S3EndpointEnv:  "s3.internal:9000",
		appconfig.S3BucketEnv:    "smiles",
		appconfig.S3AccessKeyEnv: "AKIAEXAMPLE",
		// APP_S3_SECRET_KEY deliberately left unset: an incomplete group.
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty (a refused environment must print nothing)", stdout)
	}
	want := "saasctl config print: cli-app: an S3 ObjectStore composition needs APP_S3_SECRET_KEY set too " +
		"(got some but not all of APP_S3_ENDPOINT/APP_S3_BUCKET/APP_S3_ACCESS_KEY/APP_S3_SECRET_KEY)\n"
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// TestPrintRefusesIncompleteSMTPPair: an SMTP pair missing its port fails
// print with the generated app's own SMTP completeness error, the pair's
// mirror of TestPrintRefusesIncompleteS3Group above.
func TestPrintRefusesIncompleteSMTPPair(t *testing.T) {
	code, stdout, stderr := drivePrint(t, []string{fixture(t, "print.mod")}, map[string]string{
		appconfig.SMTPHostEnv: "smtp.internal",
		// APP_SMTP_PORT deliberately left unset: an incomplete pair.
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty (a refused environment must print nothing)", stdout)
	}
	want := "saasctl config print: cli-app: an SMTP Mailer composition needs both APP_SMTP_HOST and APP_SMTP_PORT set\n"
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// TestPrintNeverRendersTheKeyBytes: however the key variables are set,
// their hex never appears anywhere in the output -- the [redacted] marker
// is the whole story the value column tells, and the provenance column
// names only the variable, never its contents. The S3 secret key, the SMTP
// password and the SMS gateway URL join the same check: they are
// secret-shaped rows too, and must never start printing plaintext just
// because they arrived through the twin's newly-covered infrastructure
// surface -- the gateway URL because authn's HTTP SMS transport has no
// credential channel separate from its endpoint, so an operator who must
// authenticate to the gateway puts the credentials inside the URL.
func TestPrintNeverRendersTheKeyBytes(t *testing.T) {
	configKeyHex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	orgIndexKeyHex := "ffe0f1d2c3b4a5968778695a4b3c2d1e0f00112233445566778899aabbccddee"
	authnBlindIndexKeyHex := "404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f"
	authnPIICipherKeyHex := "606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f"
	pkiLocalKeyCipherKeyHex := "808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9f"
	s3Secret := "correct-horse-battery-staple-s3"
	smtpPassword := "correct-horse-battery-staple-smtp"
	smsGatewayURL := "https://sms-user:sms-secret@sms.internal/send"
	code, stdout, stderr := drivePrint(t, []string{fixture(t, "print.mod")}, map[string]string{
		appconfig.ConfigKeyEnv:            configKeyHex,
		appconfig.OrgIndexKeyEnv:          orgIndexKeyHex,
		appconfig.AuthnBlindIndexKeyEnv:   authnBlindIndexKeyHex,
		appconfig.AuthnPIICipherKeyEnv:    authnPIICipherKeyHex,
		appconfig.PKILocalKeyCipherKeyEnv: pkiLocalKeyCipherKeyHex,
		appconfig.S3EndpointEnv:           "s3.internal:9000",
		appconfig.S3BucketEnv:             "smiles",
		appconfig.S3AccessKeyEnv:          "AKIAEXAMPLE",
		appconfig.S3SecretKeyEnv:          s3Secret,
		appconfig.SMTPHostEnv:             "smtp.internal",
		appconfig.SMTPPortEnv:             "587",
		appconfig.SMTPPasswordEnv:         smtpPassword,
		appconfig.SMSGatewayURLEnv:        smsGatewayURL,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	for _, secret := range []string{configKeyHex, orgIndexKeyHex, authnBlindIndexKeyHex, authnPIICipherKeyHex, pkiLocalKeyCipherKeyHex, s3Secret, smtpPassword, smsGatewayURL} {
		if strings.Contains(stdout, secret) {
			t.Errorf("stdout leaks a secret variable's value; it must render only [redacted] markers")
		}
	}
}

// TestPrintRedactsTheSMSGatewayURL pins the one credential-shaped row
// whose value must never print verbatim: config print under an environment
// carrying APP_SMS_GATEWAY_URL must not render the URL as any other
// connection-topology row. The URL is not topology: authn's
// HTTP SMS transport (go/authn's NewHTTPSMSSender) has no credential
// channel separate from its endpoint, so an operator who must
// authenticate to the gateway has nowhere to put the credentials except
// inside the URL itself -- the URL is the credential, and an accidental
// paste of a config print into a log or ticket would ship it. The sms
// row must therefore render the [redacted] marker like the S3 secret
// key and SMTP password rows, with the value's bytes nowhere in the
// output.
func TestPrintRedactsTheSMSGatewayURL(t *testing.T) {
	url := "https://gateway-user:gateway-secret@sms.internal/send"
	code, stdout, stderr := drivePrint(t, []string{fixture(t, "print.mod")}, map[string]string{
		appconfig.SMSGatewayURLEnv: url,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "sms gateway url  [redacted]   from APP_SMS_GATEWAY_URL\n") {
		t.Errorf("stdout does not render the sms gateway url row's value column as [redacted]:\n%s", stdout)
	}
	if strings.Contains(stdout, url) {
		t.Errorf("stdout leaks APP_SMS_GATEWAY_URL's value %q; it must render only the [redacted] marker", url)
	}
}

// envNameInProse matches an APP_* bootstrap variable name wherever it
// appears in the usage text's folded prose.
var envNameInProse = regexp.MustCompile(`APP_[A-Z0-9_]+`)

// TestPrintRedactedEnvParagraphEnumeratesTheList is the gate tying the
// print usage text's secret-variable enumeration to print.go's actual
// redactedEnv declaration -- the declaration, a role-by-role list in its
// doc comment, and the usage paragraph must not be three copies of one
// list with nothing comparing them, or a variable added to or dropped
// from the declaration while a copy stays stale would pass every test.
// This test reads the actual list -- the redactedEnv map this package
// compiles from print.go -- and asserts the usage paragraph's
// parenthesized enumerations name exactly its key set, in both
// directions: a paragraph that forgets a declared secret, or names a
// variable the declaration does not redact, fails here. Each member's
// role gloss lives in the usage text's variable table above the
// paragraph, the one place a member's role is stated.
func TestPrintRedactedEnvParagraphEnumeratesTheList(t *testing.T) {
	// The enumeration region runs from the paragraph's fixed opening to
	// the colon that closes "are secrets:". Whitespace is folded first so
	// the region's wrapped lines parse as one list.
	const opening = "The six key variables (APP_CONFIG__CIPHER_KEY"
	start := strings.Index(printUsage, opening)
	if start < 0 {
		t.Fatalf("printUsage no longer opens the secret enumeration with %q", opening)
	}
	region := printUsage[start:]
	colon := strings.Index(region, ":")
	if colon < 0 {
		t.Fatalf("printUsage's secret enumeration never reaches the colon that closes %q", "are secrets:")
	}
	flat := strings.Join(strings.Fields(region[:colon]), " ")
	named := map[string]bool{}
	for _, name := range envNameInProse.FindAllString(flat, -1) {
		named[name] = true
	}
	for name := range redactedEnv {
		if !named[name] {
			t.Errorf("the usage paragraph's secret enumeration omits %s, which redactedEnv (print.go) declares a secret", name)
		}
	}
	for name := range named {
		if !redactedEnv[name] {
			t.Errorf("the usage paragraph's secret enumeration names %s, which redactedEnv (print.go) does not declare a secret", name)
		}
	}
	if len(named) != len(redactedEnv) {
		t.Errorf("the usage paragraph names %d variables, redactedEnv (print.go) holds %d", len(named), len(redactedEnv))
	}
}

// envNameInVariableTable matches one variable name at the start of a line
// in the usage text's variable table: two spaces of indent, the name, and
// the whitespace before its description.
var envNameInVariableTable = regexp.MustCompile(`(?m)^  ([A-Z][A-Z0-9_]*) `)

// TestPrintUsageVariableTableListsTheRenderedRows pins the print usage
// text's bootstrap-variable table against the rows print actually
// renders -- same names, same order, nothing on either side unaccounted
// for. The table is the operator's read of the surface and printRows is
// the command's behavior, so the two are one list in two spellings;
// nothing else compares them, and a name added, dropped or reordered on
// either side must fail here rather than leave the help text describing a
// surface the command does not print.
func TestPrintUsageVariableTableListsTheRenderedRows(t *testing.T) {
	const (
		tableOpening = "The bootstrap variables:"
		tableClosing = "The six key variables"
	)
	start := strings.Index(printUsage, tableOpening)
	if start < 0 {
		t.Fatalf("printUsage no longer opens the variable table with %q", tableOpening)
	}
	rest := printUsage[start+len(tableOpening):]
	end := strings.Index(rest, tableClosing)
	if end < 0 {
		t.Fatalf("printUsage's variable table never reaches %q", tableClosing)
	}
	var listed []string
	for _, match := range envNameInVariableTable.FindAllStringSubmatch(rest[:end], -1) {
		listed = append(listed, match[1])
	}

	cfg, err := appconfig.Load("cli-app", func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatalf("load the all-defaults bootstrap environment: %v", err)
	}
	var rendered []string
	for _, row := range printRows(cfg, cfg.EffectiveDBPath(defaultModPath)) {
		rendered = append(rendered, row.env)
	}

	if !slices.Equal(listed, rendered) {
		t.Errorf("the usage table lists %v, print renders %v", listed, rendered)
	}
}

// TestPrintMalformedDeploymentModeIsReportedVerbatim: a value that is not
// a deployment mode at all fails with pkgcore's own parse error -- the
// same error the generated app's bootstrap would surface -- prefixed with
// the command name.
func TestPrintMalformedDeploymentModeIsReportedVerbatim(t *testing.T) {
	code, stdout, stderr := drivePrint(t, []string{fixture(t, "print.mod")}, map[string]string{
		appconfig.DeploymentModeEnv: "banana",
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	want := fmt.Sprintf("saasctl config print: %v: %q (valid values are %q and %q)\n",
		pkgcore.ErrInvalidDeploymentMode, "banana", pkgcore.DeploymentModeStandalone, pkgcore.DeploymentModeDistributed)
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// TestPrintMalformedConfigKeyNamesTheAppAndVariable: a malformed
// APP_CONFIG__CIPHER_KEY fails under the twin's own error text -- app
// name and variable named, the required shape stated. The generated app's
// own refusal for the same input is the loader's (naming the key path and
// the derived variable), so this pins the twin's message, which is what
// the CLI's caller sees.
func TestPrintMalformedConfigKeyNamesTheAppAndVariable(t *testing.T) {
	code, stdout, stderr := drivePrint(t, []string{fixture(t, "print.mod")}, map[string]string{
		appconfig.ConfigKeyEnv: "abc",
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	want := "saasctl config print: cli-app: APP_CONFIG__CIPHER_KEY must hold 64 hex characters (a 32-byte key), got 3\n"
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// TestPrintMissingGoModNamesThePath: a go.mod argument that does not
// exist fails with the read-prefixed error naming the path, like the
// sibling commands' file errors.
func TestPrintMissingGoModNamesThePath(t *testing.T) {
	mod := filepath.Join(t.TempDir(), "no-such-go.mod")
	code, _, stderr := drivePrint(t, []string{mod}, nil)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.HasPrefix(stderr, "saasctl config print: read "+mod+":") {
		t.Errorf("stderr = %q, want the read %s: prefix", stderr, mod)
	}
}

// TestPrintTooManyGoModArgumentsIsAUsageError: print takes at most one
// go.mod path; two are a usage error -- the message plus the print usage
// on stderr, exit 2.
func TestPrintTooManyGoModArgumentsIsAUsageError(t *testing.T) {
	code, stdout, stderr := drivePrint(t, []string{"one.mod", "two.mod"}, nil)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	want := "saasctl config print: expected at most one go.mod path, got 2\n\n" + printUsage
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// TestPrintSqlitePathRowShowsTheEffectiveFileAnchoredAtTheGoModArgument
// pins the sqlite path row: print exists to tell the operator which file
// the generated app would actually use, so the value column must carry
// the relative APP_DB_PATH resolved the way db migrate resolves it --
// anchored to the go.mod argument's directory before opening (its own
// relative-path regression test pins that half). Reporting the raw
// relative value instead would leave an operator reading "db.sqlite" from
// print while migrate operated on /path/to/project/db.sqlite, exactly the
// misunderstanding print exists to prevent. The operator shape is the one
// the anchoring rule exists for:
// the project's go.mod lives in another directory, the command runs from
// this one, and APP_DB_PATH carries a relative value. The row's value
// column must carry the effective file -- anchored to the go.mod
// argument's directory, byte-identical to the path db migrate would open
// -- compared here through cfg.EffectiveDBPath, the single shared
// resolution both commands use, and the raw value must stay visible in the
// source column.
func TestPrintSqlitePathRowShowsTheEffectiveFileAnchoredAtTheGoModArgument(t *testing.T) {
	projectDir := t.TempDir()
	mod := filepath.Join(projectDir, "go.mod")
	fixtureContent, err := os.ReadFile(fixture(t, "print.mod"))
	if err != nil {
		t.Fatalf("read the print.mod fixture: %v", err)
	}
	if err = os.WriteFile(mod, fixtureContent, 0o644); err != nil {
		t.Fatalf("write the project's go.mod: %v", err)
	}

	// The operator runs the command from a directory that is not the
	// project's, pointing at the project's go.mod by absolute path, with a
	// RELATIVE APP_DB_PATH.
	callerDir := t.TempDir()
	t.Chdir(callerDir)
	code, stdout, stderr := drivePrint(t, []string{mod}, map[string]string{
		appconfig.DBPathEnv: "db.sqlite",
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}

	// The value column carries the effective file -- derived through the
	// shared resolution function db migrate opens its database through, so
	// the comparison is against the one function, never a reimplementation
	// beside it -- and the effective file is exactly the file a boot from
	// the project's own directory opens.
	cfg, err := appconfig.Load("cli-app", os.LookupEnv)
	if err != nil {
		t.Fatalf("resolve the bootstrap environment the command ran under: %v", err)
	}
	effective := cfg.EffectiveDBPath(mod)
	if want := filepath.Join(projectDir, "db.sqlite"); effective != want {
		t.Fatalf("EffectiveDBPath = %q, want %q (the shared resolution itself moved)", effective, want)
	}
	if !strings.Contains(stdout, "sqlite path      "+effective) {
		t.Errorf("stdout does not carry the effective file %q in the sqlite path row's value column:\n%s", effective, stdout)
	}
	// The raw value stays visible in the source column when it differs
	// from the effective file.
	if !strings.Contains(stdout, "from APP_DB_PATH (raw value: db.sqlite)") {
		t.Errorf("stdout does not carry the raw value with its provenance in the sqlite path row:\n%s", stdout)
	}
	// And nothing about the caller's directory leaked into the answer.
	if strings.Contains(stdout, callerDir) {
		t.Errorf("stdout carries the caller's directory %q; the effective file anchors at the go.mod argument's directory", callerDir)
	}
}

// TestPrintSqlitePathRowAbsoluteDBPathUsedVerbatim pins the other half of
// the anchoring rule: an ABSOLUTE APP_DB_PATH is used exactly as the
// generated app would use it -- raw and effective coincide, the value
// column carries the absolute value, and no raw parenthetical is needed.
func TestPrintSqlitePathRowAbsoluteDBPathUsedVerbatim(t *testing.T) {
	projectDir := t.TempDir()
	mod := filepath.Join(projectDir, "go.mod")
	fixtureContent, err := os.ReadFile(fixture(t, "print.mod"))
	if err != nil {
		t.Fatalf("read the print.mod fixture: %v", err)
	}
	if err = os.WriteFile(mod, fixtureContent, 0o644); err != nil {
		t.Fatalf("write the project's go.mod: %v", err)
	}
	callerDir := t.TempDir()
	t.Chdir(callerDir)
	absolute := filepath.Join(projectDir, "fixed.db")
	code, stdout, stderr := drivePrint(t, []string{mod}, map[string]string{
		appconfig.DBPathEnv: absolute,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	if !strings.Contains(stdout, "sqlite path      "+absolute) {
		t.Errorf("stdout does not carry the absolute APP_DB_PATH %q in the sqlite path row's value column:\n%s", absolute, stdout)
	}
	if strings.Contains(stdout, "raw value") {
		t.Errorf("stdout renders a raw-value parenthetical for an absolute %s; raw and effective coincide there:\n%s", appconfig.DBPathEnv, stdout)
	}
}
