// Command saasctl is the speed consumer CLI. Consumers manage their own
// code; saasctl manages the boundary where a project meets the speed
// modules it pulls in: shaping new consumer projects (new), rewriting a
// project's speed module requires when a new lockstep release lands
// (upgrade), applying a generated project's speed-module migrations to its
// database (db migrate), and showing how its bootstrap configuration
// resolves (config print).
//
// The exit-code contract is uniform across commands: 0 for success and
// help, 2 for usage errors (a malformed invocation of a command that
// exists), 1 for execution errors.
package main

import (
	"fmt"
	"io"
	"os"

	configcmd "github.com/vislake/speed/go/saasctl/internal/config"
	dbcmd "github.com/vislake/speed/go/saasctl/internal/db"
	newcmd "github.com/vislake/speed/go/saasctl/internal/new"
	"github.com/vislake/speed/go/saasctl/internal/upgrade"
)

const rootUsage = `Usage: saasctl <command> [args]

saasctl manages the boundary where a speed consumer project meets the
speed modules it pulls in: it shapes new projects, rewrites a project's
speed module requires when a new lockstep release lands, applies the
speed modules' migrations to a generated project's database, and shows
how its bootstrap configuration resolves.

Commands:

  new        Materialize the project skeleton into a new consumer project
  upgrade    Rewrite a project's speed dependencies to one version for a
             new release
  db         Apply a generated project's speed-module migrations to its
             SQLite database (saasctl db migrate)
  config     Show how a generated project's bootstrap configuration
             resolves (saasctl config print)

All four commands are wired in this build. Run "saasctl <command> -h" for
each command's flags.

Exit codes: 0 success or help, 2 usage error, 1 execution error.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one invocation and returns its process exit code. The
// root-level exit-code shape mirrors the per-command one: no arguments,
// help, -h and --help print the root usage to stdout and exit 0; an
// unknown command prints the unknown-command error plus the root usage to
// stderr and exits 2. Usage and error writes are best-effort -- the only
// realistic failure is a closed pipe, and the returned exit code is the
// whole contract either way -- so each call blank-assigns the write error
// (the repository's errcheck config runs with check-blank off).
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprint(stdout, rootUsage)
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		_, _ = fmt.Fprint(stdout, rootUsage)
		return 0
	case "new":
		return newcmd.Run(args[1:], stdout, stderr)
	case "upgrade":
		return upgrade.Run(args[1:], stdout, stderr)
	case "db":
		return dbcmd.Run(args[1:], stdout, stderr)
	case "config":
		return configcmd.Run(args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "saasctl: unknown command %q\n\n", args[0])
		_, _ = fmt.Fprint(stderr, rootUsage)
		return 2
	}
}
