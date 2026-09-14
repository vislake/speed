// Command minimal-host is the smallest host that assembles a run out of the
// module mechanism: it writes a main, imports the modules it wants, and drives
// the lifecycle. Nothing else about the program comes from the framework.
//
// What the host itself contributes is one module holding its identity
// resource, plus the imports below. Everything else in this directory is an
// ordinary module: two implementations of one capability, and an application
// module that takes the capability up.
//
// The two blank imports are how a transport and a format reach the
// configuration module. Each registers itself in init and delivers its
// implementation as a resource, so importing the package is the whole of the
// wiring, and a host that configures itself in JSON never imports the YAML
// parser or pays for its dependency.
//
// What the run demonstrates, each with a case in e2e_test.go:
//
//   - --help renders the arguments the assembled modules declared, and exits
//     successfully: asking for help is not a failure.
//   - A config file and an environment variable reach the same input item,
//     and the environment wins.
//   - Two providers deliver one capability; the built-in one stands down as
//     soon as the other is configured, and the startup diagnostics say which
//     module is not running and why.
//   - Cancelling the context stops and closes every module in reverse
//     dependency order.
//   - A record says which module wrote it, without the module putting its own
//     name into the call: a module that declares the logging dependency takes
//     its logger by name, and the same declaration is what puts logging ahead
//     of it at construction and behind it at shutdown, so a record written
//     from New or from Close still reaches the destination.
//   - A path that carries a context takes its logger from the step that
//     created the context, and every call below it only asks the context. The
//     injection carries the attributes of the unit of work along with it.
//   - The configuration decides where records go: by default one text record
//     per line on standard output, and configured to a JSON file they leave
//     standard output altogether. The process default logger is not part of
//     that: it writes to standard output whatever the configuration says,
//     which is how a path that never had a logger injected is spotted.
//   - Redaction masks the keys a module registered as sensitive, in every
//     destination at once: a credential given on the command line reaches no
//     stream and no file, while the record still says a credential was given.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/log"

	_ "github.com/vislake/speed/pkg/config/format/yaml"
	_ "github.com/vislake/speed/pkg/config/source/file"
)

// stoppedMsg is the record main writes once the run is over.
const stoppedMsg = "stopped"

func main() { os.Exit(run()) }

// run drives the lifecycle and turns its outcome into an exit status. It is a
// function of its own so that the deferred stop runs before the process
// leaves: os.Exit skips deferred calls.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := core.ProcessRegistry.Run(ctx); err != nil {
		// Asking for help stops the startup without failing it. The help
		// output itself has already been written to stdout by the module
		// that renders it, so there is nothing left to print here and the
		// status is a successful one.
		if errors.Is(err, config.ErrHelpRequested) {
			return 0
		}
		// A startup that failed may have failed before logging was
		// assembled, so the report goes to standard error directly rather
		// than through a chain that might not exist.
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	// main is not a module: it holds no product of the registry and it is
	// handed no context carrying a logger, so this line goes through the
	// process default logger. That one writes to standard output over the
	// bootstrap chain whatever destinations the run was configured with, and
	// it still works here although every module, logging included, has
	// already been closed: the reference the bootstrap chain holds on the
	// standard output writer belongs to the root package and is never
	// released.
	log.Default().Info(stoppedMsg)
	return 0
}
