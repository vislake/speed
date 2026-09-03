// Fixture for tools/semgrep_rules/deployment-mode-branch.yml.
// Planted violations: every pattern shape must fire on this file.
// This file is NOT shipped code -- it proves the rule fires.
package fixture

import (
	"os"

	"github.com/vislake/speed/go/pkgcore"
)

func businessLogic(mode string) string {
	if mode == "standalone" { // fires: string literal compare
		return "a"
	}
	if "distributed" == mode { // fires: reversed operand order
		return "b"
	}
	if mode != "standalone" { // fires: != form
		return "c"
	}
	switch mode {
	case "standalone": // fires: case label
		return "d"
	case "distributed": // fires: case label
		return "e"
	}
	return "f"
}

func kernelEscaper(mode pkgcore.DeploymentMode) bool {
	if mode == pkgcore.DeploymentModeStandalone { // fires: qualified const
		return true
	}
	if pkgcore.DeploymentModeDistributed == mode { // fires: reversed
		return true
	}
	if mode != pkgcore.DeploymentModeDistributed { // fires: != qualified
		return true
	}
	switch mode {
	case pkgcore.DeploymentModeStandalone: // fires: qualified case label
		return true
	}
	return false
}

func envReader() string {
	return os.Getenv("SPEED_DEPLOYMENT_MODE") // fires: env read outside command entry
}

const (
	// Const indirection: semgrep constant-propagates same-package
	// literal-valued consts, so both uses below resolve to the mode
	// literal / env name and fire -- matching is by value, not by
	// identifier name.
	modeAliasStandalone = "standalone"
	modeAliasEnv        = "SPEED_DEPLOYMENT_MODE"
)

func constEscaper(mode string) string {
	if mode == modeAliasStandalone { // fires: const-propagated literal compare
		return "g"
	}
	return os.Getenv(modeAliasEnv) // fires: const-propagated env read
}
