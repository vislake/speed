// Fixture for tools/semgrep_rules/deployment-mode-branch.yml.
// Clean control: none of these patterns may fire.
package fixture

import (
	"os"
	"strings"
)

const (
	logLevelEnv  = "LOG_LEVEL" // an env var whose value is not the deployment-mode variable
	sqliteFlavor = "sqlite"    // a config value that is not a deployment mode
)

// Semgrep constant-propagates same-package literal-valued consts, so the
// two uses below resolve to "sqlite" and "LOG_LEVEL" respectively --
// matching is by VALUE, not by identifier name. Config decisions that do
// not involve a deployment-mode value stay clean.
func configDriven(flavor string) string {
	if flavor == sqliteFlavor {
		return "sqlite flavor configured"
	}
	return strings.ToUpper(os.Getenv(logLevelEnv))
}

func logMessage(mode string) string {
	return "started in " + mode + " mode" // string building for a message, no compare
}

func unrelatedSwitch(status int) string {
	switch status {
	case 200:
		return "ok"
	}
	return "unknown"
}
