// Command minimal-host is the smallest host that assembles a run out of the
// module mechanism: it writes a main, imports the modules it wants, and drives
// the lifecycle. Nothing else about the program comes from the framework.
//
// What the host itself contributes is one module holding its identity
// resource, plus the four lines below. Everything else in this directory is an
// ordinary module: two implementations of one capability, and an application
// module that takes the capability up.
//
// The two blank imports are how a transport and a format reach the
// configuration module. Each registers itself in init and delivers its
// implementation as a resource, so importing the package is the whole of the
// wiring, and a host that configures itself in JSON never imports the YAML
// parser or pays for its dependency.
//
// Four things the run demonstrates, each with a case in e2e_test.go:
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

	_ "github.com/vislake/speed/pkg/config/format/yaml"
	_ "github.com/vislake/speed/pkg/config/source/file"
)

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
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
