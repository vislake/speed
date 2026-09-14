// Command dbhost is the test host the end-to-end cases in this directory drive
// as a child process. It is not part of the product: it lives under internal/,
// so no consumer can import it, and nothing but this package's own tests builds
// it.
//
// A child process is what the cases need rather than a preference, for the
// reason the log host states: the configuration loader reads os.Args[1:]
// unconditionally, and a test binary is always handed arguments of its own,
// which the loader rejects as a malformed command line. A real assembly
// therefore cannot be started inside a test binary. Running the host as a
// process is also what makes the concurrency case possible at all: the failure
// the migration mutex exists to prevent happens between replicas of one
// deployment, and replicas are processes, so two goroutines in one test binary
// could never reach it.
//
// Two constraints follow from the same direction. The scenario is chosen by an
// environment variable that does not start with this host's prefix: a variable
// under the prefix that no input item reads is reported on the same stream the
// cases read the startup diagnostics from. And the configuration document is
// JSON, which is what the cases write it with: a fixture and the run it feeds
// agree on one format, and neither has to escape a path by hand.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/vislake/speed/pkg/core"

	// The transports and the format the cases' configuration documents arrive
	// through, and both implementations, so that a run may name either engine.
	_ "github.com/vislake/speed/pkg/config/format/json"
	_ "github.com/vislake/speed/pkg/config/source/file"
	_ "github.com/vislake/speed/pkg/db/postgres"
	_ "github.com/vislake/speed/pkg/db/sqlite"
)

// The markers a case looks for when the run is expected to fail. errors.Is does
// not survive a process boundary, so the host judges the error itself and prints
// a fixed word the case can look for; what the case cannot read off the marker
// it reads off the diagnostic text the error yields on standard error.
const (
	// exclusiveViolatedMarker reports a startup that failed because two
	// implementations of the database capability were both enabled.
	exclusiveViolatedMarker = "dbhost: exclusive-violated"
	// missingProviderMarker reports a startup that failed because a module
	// required the database capability and no enabled implementation
	// delivered one.
	missingProviderMarker = "dbhost: missing-provider"
)

// finish ends the run. The host's whole job is one scenario, so the module that
// ran it cancels the context and lets Run shut every module down in order, the
// way an interrupt would.
var finish context.CancelFunc

func main() { os.Exit(run()) }

// run drives the lifecycle and turns its outcome into an exit status. It is a
// function of its own so the deferred cancel runs before the process leaves:
// os.Exit skips deferred calls.
func run() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finish = cancel

	if err := core.ProcessRegistry.Run(ctx); err != nil {
		switch {
		case errors.Is(err, core.ErrExclusiveViolated):
			say(exclusiveViolatedMarker)
		case errors.Is(err, core.ErrMissingProvider):
			say(missingProviderMarker)
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// say writes one line of this host's own narration, as opposed to the startup
// diagnostics core and the modules write.
//
// The dropped write error is stated once here rather than at each call site:
// stdout is the output channel itself, so a failed write to it has nowhere left
// to be reported.
func say(line string) {
	//nolint:errcheck // dropping the error is what this function is for; the
	// doc comment above says why there is nothing to do with it.
	fmt.Fprintln(os.Stdout, line)
}
