// Package db implements the `saasctl db` command group: database
// maintenance for the consumer projects saasctl generates and upgrades.
// This build wires the group's migrate subcommand, which applies the SQL
// migrations of the speed modules a project requires to the project's
// SQLite database -- the same Apply the generated app runs at startup,
// driven by the operator instead of by the app's first boot.
//
// The group shares the exit-code contract of the sibling commands and
// groups: 0 for success and help, 2 for usage errors (an unknown
// subcommand, a malformed invocation), 1 for execution errors.
package db

import (
	"fmt"
	"io"
)

// usage is the db group's help text.
const usage = `Usage: saasctl db <command> [args]

Database maintenance for the consumer projects saasctl generates.

Commands:

  migrate   Apply the speed modules' SQL migrations to the project's
            SQLite database

Run "saasctl db <command> -h" for each command's flags.

Exit codes: 0 success or help, 2 usage error, 1 execution error.
`

// Run implements the db command group: the bare invocation, the help
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
	case "migrate":
		return runMigrate(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "saasctl db: unknown command %q\n\n", args[0])
		_, _ = fmt.Fprint(stderr, usage)
		return 2
	}
}
