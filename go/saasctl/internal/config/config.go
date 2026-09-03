// Package config implements the `saasctl config` command group: the
// bootstrap configuration of the consumer projects saasctl generates and
// upgrades. This build wires the group's print subcommand, which renders
// how a project's bootstrap configuration resolves -- the deployment mode,
// the port, the SQLite database path and the two key variables -- together
// with each value's provenance: whether it came from the environment, and
// if not, which default it resolved to. The two key variables are secrets
// and render as [redacted] whatever the environment holds.
//
// The group shares the exit-code contract of the sibling commands and
// groups: 0 for success and help, 2 for usage errors (an unknown
// subcommand, a malformed invocation), 1 for execution errors.
package config

import (
	"fmt"
	"io"
)

// usage is the config group's help text.
const usage = `Usage: saasctl config <command> [args]

The bootstrap configuration of the consumer projects saasctl generates.

Commands:

  print     Show how a project's bootstrap configuration resolves, with
            each value's provenance

Run "saasctl config <command> -h" for each command's flags.

Exit codes: 0 success or help, 2 usage error, 1 execution error.
`

// Run implements the config command group: the bare invocation, the help
// forms and an unknown subcommand follow the root command's conventions
// (usage on stdout for help, an unknown-command message plus the group
// usage on stderr for a usage error), and each known subcommand receives
// its own arguments.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, usage)
		return 0
	case "print":
		return runPrint(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "saasctl config: unknown command %q\n\n", args[0])
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
}
