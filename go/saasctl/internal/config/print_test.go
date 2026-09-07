package config

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/saasctl/internal/appconfig"
)

// bootstrapEnvKeys lists the environment surface a generated project's
// bootstrap reads -- the full twenty variables appconfig resolves,
// exported for the tests and examples that must clear or restore them all.
// The list mirrors the one internal/db's migrate tests carry, each
// package's copy sitting next to the code that uses it.
var bootstrapEnvKeys = []string{
	appconfig.DeploymentModeEnv,
	appconfig.PortEnv,
	appconfig.DBPathEnv,
	appconfig.ConfigKeyEnv,
	appconfig.OrgIndexKeyEnv,
	appconfig.AuthnBlindIndexKeyEnv,
	appconfig.AuthnPIICipherKeyEnv,
	appconfig.PKILocalKeyCipherKeyEnv,
	appconfig.RedisAddrEnv,
	appconfig.S3EndpointEnv,
	appconfig.S3BucketEnv,
	appconfig.S3AccessKeyEnv,
	appconfig.S3SecretKeyEnv,
	appconfig.S3RegionEnv,
	appconfig.S3UseSSLEnv,
	appconfig.SMTPHostEnv,
	appconfig.SMTPPortEnv,
	appconfig.SMTPUsernameEnv,
	appconfig.SMTPPasswordEnv,
	appconfig.SMSGatewayURLEnv,
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
// fixed app.db path, the five development key byte sequences (the two
// original key materials plus the three authn/pki ones), and every
// infrastructure seam left on its Preset default -- one line per value,
// each sourced line naming the default (or the seam) it fell back to. The
// five key rows and the S3 secret key / SMTP password rows show only the
// [redacted] marker in the value column.
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
		"sqlite path      app.db       unset or empty (default app.db)\n" +
		"config key       [redacted]   unset or empty (development default)\n" +
		"org index key    [redacted]   unset or empty (development default)\n" +
		"authn blind index key [redacted]   unset or empty (development default)\n" +
		"authn pii cipher key [redacted]   unset or empty (development default)\n" +
		"pki local key cipher key [redacted]   unset or empty (development default)\n" +
		"redis addr                    unset or empty (eventbus/kv stay on the in-process default)\n" +
		"s3 endpoint                   unset or empty (objectstore stays on the local-directory default)\n" +
		"s3 bucket                     unset or empty (objectstore stays on the local-directory default)\n" +
		"s3 access key                 unset or empty (objectstore stays on the local-directory default)\n" +
		"s3 secret key    [redacted]   unset or empty (objectstore stays on the local-directory default)\n" +
		"s3 region                     unset or empty (optional S3 refinement; used only when the group above is set)\n" +
		"s3 use ssl       false        unset or empty (default false)\n" +
		"smtp host                     unset or empty (mailer stays on the console default)\n" +
		"smtp port                     unset or empty (mailer stays on the console default)\n" +
		"smtp username                 unset or empty (optional SMTP refinement; used only when the group above is set)\n" +
		"smtp password    [redacted]   unset or empty (optional SMTP refinement; used only when the group above is set)\n" +
		"sms gateway url               unset or empty (SMS sender seam left unwired: console default under standalone, refused under distributed)\n"
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
// per-row "from APP_*" provenance would have caught.
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
		appconfig.RedisAddrEnv:            "redis.internal:6379",
		appconfig.S3EndpointEnv:           "s3.internal:9000",
		appconfig.S3BucketEnv:             "smiles",
		appconfig.S3AccessKeyEnv:          "AKIAEXAMPLE",
		appconfig.S3SecretKeyEnv:          "s3cr3t",
		appconfig.S3RegionEnv:             "us-east-1",
		appconfig.S3UseSSLEnv:             "true",
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
		"sqlite path      db.sqlite    from APP_DB_PATH\n" +
		"config key       [redacted]   from APP_CONFIG_KEY\n" +
		"org index key    [redacted]   from APP_ORG_INDEX_KEY\n" +
		"authn blind index key [redacted]   from APP_AUTHN_BLIND_INDEX_KEY\n" +
		"authn pii cipher key [redacted]   from APP_AUTHN_PII_CIPHER_KEY\n" +
		"pki local key cipher key [redacted]   from APP_PKI_LOCAL_KEY_CIPHER_KEY\n" +
		"redis addr       redis.internal:6379 from APP_REDIS_ADDR\n" +
		"s3 endpoint      s3.internal:9000 from APP_S3_ENDPOINT\n" +
		"s3 bucket        smiles       from APP_S3_BUCKET\n" +
		"s3 access key    AKIAEXAMPLE  from APP_S3_ACCESS_KEY\n" +
		"s3 secret key    [redacted]   from APP_S3_SECRET_KEY\n" +
		"s3 region        us-east-1    from APP_S3_REGION\n" +
		"s3 use ssl       true         from APP_S3_USE_SSL\n" +
		"smtp host        smtp.internal from APP_SMTP_HOST\n" +
		"smtp port        587          from APP_SMTP_PORT\n" +
		"smtp username    mailer       from APP_SMTP_USERNAME\n" +
		"smtp password    [redacted]   from APP_SMTP_PASSWORD\n" +
		"sms gateway url  http://sms.internal/send from APP_SMS_GATEWAY_URL\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestPrintRefusesIncompleteS3Group: an S3 group missing one of its four
// required members fails print exactly as it fails the generated app's own
// bootstrap -- the coded completeness error naming which variables are
// missing -- rather than printing five (or seventeen) lines and exiting 0
// on an environment the generated app would refuse to boot on. Before the
// appconfig twin validated the S3 group, print silently ignored these
// variables and always exited 0; this is the exact misdiagnosis the audit
// finding names.
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
// names only the variable, never its contents. The S3 secret key and SMTP
// password join the same check: they are secret-shaped rows too, and must
// never start printing plaintext just because they arrived through the
// twin's newly-covered infrastructure surface.
func TestPrintNeverRendersTheKeyBytes(t *testing.T) {
	configKeyHex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	orgIndexKeyHex := "ffe0f1d2c3b4a5968778695a4b3c2d1e0f00112233445566778899aabbccddee"
	authnBlindIndexKeyHex := "404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f"
	authnPIICipherKeyHex := "606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f"
	pkiLocalKeyCipherKeyHex := "808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9f"
	s3Secret := "correct-horse-battery-staple-s3"
	smtpPassword := "correct-horse-battery-staple-smtp"
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
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	for _, secret := range []string{configKeyHex, orgIndexKeyHex, authnBlindIndexKeyHex, authnPIICipherKeyHex, pkiLocalKeyCipherKeyHex, s3Secret, smtpPassword} {
		if strings.Contains(stdout, secret) {
			t.Errorf("stdout leaks a secret variable's value; it must render only [redacted] markers")
		}
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
// APP_CONFIG_KEY fails with the generated app's own error text -- app
// name and variable named, the required shape stated.
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
	want := "saasctl config print: cli-app: APP_CONFIG_KEY must hold 64 hex characters (a 32-byte key), got 3\n"
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
