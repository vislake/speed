// Fixture for tools/semgrep_rules/deployment-mode-branch.yml.
// Clean control: none of these patterns may fire.
package fixture

import (
	"os"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
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

// Capability branching -- the deployment-composition retrofit's new
// vocabulary -- is deliberately outside this rule's scope. Deployment mode
// no longer selects implementations; capability-declaring implementations
// are what assembly validates against the mode's required capabilities, so
// a branch on a capability is not a mode decision. No architecture-
// discipline row governs capability branching yet, so these shapes must
// stay clean: a future row ships its own rule, and these cases move to
// that rule's positive fixture.
func capabilityGate(caps pkgcore.Capability) string {
	if caps.Has(pkgcore.MultiReplicaSafe) { // stays clean: capability, not mode value
		return "multi-replica-safe path"
	}
	if caps.Has(pkgcore.SurvivesRestart) { // stays clean
		return "survives-restart path"
	}
	switch {
	case caps.Has(pkgcore.MultiReplicaSafe): // stays clean
		return "multi-replica-safe case"
	}
	return "neither"
}
