// Command saasctl is the speed consumer CLI. Consumers manage their own
// code; saasctl manages the boundary where a project meets the speed
// modules it pulls in: shaping new consumer projects (new), rewriting a
// project's speed module requires when a new lockstep release lands
// (upgrade), and maintaining a generated project's database and dynamic
// configuration (db migrate and config print, planned).
//
// The exit-code contract is uniform across commands: 0 for success and
// help, 2 for usage errors (a malformed invocation of a command that
// exists), 1 for execution errors and for commands this build does not
// wire yet.
package main

import (
	"fmt"
	"io"
	"os"

	newcmd "github.com/vislake/speed/go/saasctl/internal/new"
	"github.com/vislake/speed/go/saasctl/internal/upgrade"
)

const rootUsage = `Usage: saasctl <command> [args]

saasctl manages the boundary where a speed consumer project meets the
speed modules it pulls in: it shapes new projects, rewrites a project's
speed module requires when a new lockstep release lands, and maintains a
generated project's database and dynamic configuration.

Commands:

  new        Materialize the project skeleton into a new consumer project
  upgrade    Rewrite a project's speed dependencies to one version for a
             new release
  db         Database maintenance for a generated project (planned):
             saasctl db migrate
  config     Configuration maintenance for a generated project (planned):
             saasctl config print

This build wires new and upgrade; the remaining planned commands fail
with a clear not-implemented message until their milestones land. Run
"saasctl <command> -h" for each command's flags.

Exit codes: 0 success or help, 2 usage error, 1 execution error.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one invocation and returns its process exit code. The
// root-level exit-code shape mirrors the per-command one: no arguments,
// help, -h and --help print the root usage to stdout and exit 0; a
// command this build does not wire prints one line to stderr and exits 1;
// an unknown command prints the unknown-command error plus the root usage
// to stderr and exits 2. Usage and error writes are best-effort -- the
// only realistic failure is a closed pipe, and the returned exit code is
// the whole contract either way -- so each call blank-assigns the write
// error (the repository's errcheck config runs with check-blank off).
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
	case "db", "config":
		_, _ = fmt.Fprintf(stderr, "saasctl: %s is not implemented in this build (only new and upgrade are wired); it lands with its own milestone\n", args[0])
		return 1
	default:
		_, _ = fmt.Fprintf(stderr, "saasctl: unknown command %q\n\n", args[0])
		_, _ = fmt.Fprint(stderr, rootUsage)
		return 2
	}
}
