package main

import (
	"context"
	"fmt"
	"os"

	"github.com/vislake/speed/pkg/core"
)

// appModuleName is the application module's name.
const appModuleName = "app"

// readyMarker is the line this host prints once every module has been started
// and the entry points are open. It is the signal an operator, or a test,
// waits for before treating the process as running.
const readyMarker = appModuleName + ": ready"

// init registers the application module.
func init() { core.ProcessRegistry.Register(appModule()) }

// appModule is the consumer: it requires the greeter capability and never
// names an implementation of it. Which one it gets is settled by resolution,
// out of what the host imported and what the configuration says, and this
// module changes in neither case.
//
// The requirement is also what orders the run. The provider is constructed
// before this module and shut down after it, in both directions without
// anybody writing the order down.
func appModule() core.Module {
	return core.Module{
		Name:     appModuleName,
		Requires: []core.Requirement{{Token: (*Greeter)(nil)}},
		New: func(_ context.Context, reg *core.Registry) (any, error) {
			greeter, err := core.Resolve[Greeter](reg)
			if err != nil {
				return nil, err
			}
			return &application{greeter: greeter}, nil
		},
		Start: func(_ context.Context, _ *core.Registry, instance any) error {
			a := instance.(*application)
			fmt.Fprintln(os.Stdout, appModuleName+": greeting: "+a.greeter.Greet("world"))
			fmt.Fprintln(os.Stdout, appModuleName+": endpoint: "+a.greeter.Endpoint())
			return nil
		},
		Serve: func(_ context.Context, _ *core.Registry, _ any) error {
			// Serve opens the entry points and returns. A real host
			// would start its listener on a goroutine here; blocking
			// would starve every module after it in the stage.
			fmt.Fprintln(os.Stdout, readyMarker)
			return nil
		},
		Stop: func(_ context.Context, _ *core.Registry, _ any) error {
			fmt.Fprintln(os.Stdout, appModuleName+": stop")
			return nil
		},
		Close: func(_ context.Context, _ *core.Registry, _ any) error {
			fmt.Fprintln(os.Stdout, appModuleName+": close")
			return nil
		},
	}
}

// application is the product of the application module: whatever the host's
// own program is, holding the capabilities it took up.
type application struct {
	greeter Greeter
}
