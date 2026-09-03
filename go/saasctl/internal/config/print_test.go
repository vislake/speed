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
// bootstrap reads -- the five variables appconfig resolves, exported for
// the tests and examples that must clear or restore them all. The list
// mirrors the one internal/db's migrate tests carry, each package's copy
// sitting next to the code that uses it.
var bootstrapEnvKeys = []string{
	appconfig.DeploymentModeEnv,
	appconfig.PortEnv,
	appconfig.DBPathEnv,
	appconfig.ConfigKeyEnv,
	appconfig.OrgIndexKeyEnv,
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
// <app name>.db path and the two development key bytes -- one line per
// value, each sourced line naming the default it fell back to. The key
// rows show only the [redacted] marker in the value column.
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
		"sqlite path      cli-app.db   unset or empty (default cli-app.db)\n" +
		"config key       [redacted]   unset or empty (development default)\n" +
		"org index key    [redacted]   unset or empty (development default)\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestPrintReportsEveryValueThatCameFromTheEnvironment: with every
// bootstrap variable set, each of the five lines shows the resolved value
// and names the variable that carried it -- the generated app's own
// resolution, reported with its provenance.
func TestPrintReportsEveryValueThatCameFromTheEnvironment(t *testing.T) {
	code, stdout, stderr := drivePrint(t, []string{fixture(t, "print.mod")}, map[string]string{
		appconfig.DeploymentModeEnv: "distributed",
		appconfig.PortEnv:           "9090",
		appconfig.DBPathEnv:         "db.sqlite",
		appconfig.ConfigKeyEnv:      "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
		appconfig.OrgIndexKeyEnv:    "ffe0f1d2c3b4a5968778695a4b3c2d1e0f00112233445566778899aabbccddee",
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	want := "deployment mode  distributed  from SPEED_DEPLOYMENT_MODE\n" +
		"port             9090         from PORT\n" +
		"sqlite path      db.sqlite    from SPEED_DB_PATH\n" +
		"config key       [redacted]   from SPEED_CONFIG_KEY\n" +
		"org index key    [redacted]   from SPEED_ORG_INDEX_KEY\n"
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestPrintNeverRendersTheKeyBytes: however the key variables are set,
// their hex never appears anywhere in the output -- the [redacted] marker
// is the whole story the value column tells, and the provenance column
// names only the variable, never its contents.
func TestPrintNeverRendersTheKeyBytes(t *testing.T) {
	configKeyHex := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	orgIndexKeyHex := "ffe0f1d2c3b4a5968778695a4b3c2d1e0f00112233445566778899aabbccddee"
	code, stdout, stderr := drivePrint(t, []string{fixture(t, "print.mod")}, map[string]string{
		appconfig.ConfigKeyEnv:   configKeyHex,
		appconfig.OrgIndexKeyEnv: orgIndexKeyHex,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	for _, hex := range []string{configKeyHex, orgIndexKeyHex} {
		if strings.Contains(stdout, hex) {
			t.Errorf("stdout leaks a key variable's hex; it must render only [redacted] markers")
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
// SPEED_CONFIG_KEY fails with the generated app's own error text -- app
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
	want := "saasctl config print: cli-app: SPEED_CONFIG_KEY must hold 64 hex characters (a 32-byte key), got 3\n"
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
