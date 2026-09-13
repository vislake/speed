// Command loghost is the test host the end-to-end cases in this directory
// drive as a child process. It is not part of the product: it lives under
// internal/ so no consumer can import it, and nothing but this package's own
// tests builds it.
//
// A child process is what the cases need rather than a preference. The
// configuration loader reads os.Args[1:] unconditionally, and a test binary is
// always given arguments of its own (-test.paniconexit0 among them), which the
// loader rejects as a malformed command line; a real assembly therefore cannot
// be started inside a test binary. Running the host as a process also gives
// the cases the two output streams and the exit status, and it lets each case
// have a registry of its own, since a registry goes through its lifecycle once.
//
// Two implementation constraints come from the same direction. The scenario is
// chosen by an environment variable that does not start with this host's
// prefix: positional arguments are rejected by the loader, and a variable
// under the prefix that no input item reads is reported in the startup
// diagnostics, which the cases read. The configuration format is JSON rather
// than YAML so that the module's dependency scan keeps seeing a standard
// library alone.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/vislake/speed/pkg/core"

	_ "github.com/vislake/speed/pkg/config/format/json"
	_ "github.com/vislake/speed/pkg/config/source/file"
	_ "github.com/vislake/speed/pkg/log"
)

// exclusiveMarker is printed when the startup failed over two enabled
// providers of the logging capability. errors.Is does not survive a process
// boundary, so the host judges the error itself and prints a fixed word the
// case can look for.
const exclusiveMarker = "loghost: exclusive-violated"

// finish ends the run. The host's whole job is one scenario, so the module
// that ran it cancels the context and lets Run shut every module down in
// order, the way an interrupt would.
var finish context.CancelFunc

func main() { os.Exit(run()) }

// run drives the lifecycle and turns its outcome into an exit status. It is a
// function of its own so the deferred cancel runs before the process leaves:
// os.Exit skips deferred calls.
func run() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finish = cancel

	if scenario() == caseRivalProvider {
		core.ProcessRegistry.Register(rivalLoggerModule())
	}

	if err := core.ProcessRegistry.Run(ctx); err != nil {
		if errors.Is(err, core.ErrExclusiveViolated) {
			say(exclusiveMarker)
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// say writes one line of this host's own narration, as opposed to a log
// record.
//
// The dropped write error is stated once here rather than at each call site:
// stdout is the output channel itself, so a failed write to it has nowhere
// left to be reported.
func say(line string) {
	//nolint:errcheck // dropping the error is what this function is for; the
	// doc comment above says why there is nothing to do with it.
	fmt.Fprintln(os.Stdout, line)
}
