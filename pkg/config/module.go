package config

import (
	"context"
	"os"

	"github.com/vislake/speed/pkg/core"
)

// moduleName is the name the registry recognises. It is the whole of the
// special treatment this module gets: it is constructed first, during Prepare,
// and is an implicit dependency of every other module, neither of which shows
// up as a field of the descriptor.
const moduleName = "config"

// init registers this module with the process registry, so importing the
// package is all it takes to have configuration loaded. A registry built with
// core.New inherits nothing from init and takes Module() by hand.
func init() { core.ProcessRegistry.Register(Module()) }

// Module is this module's descriptor, exported for the registries that do not
// inherit the process-level registrations.
//
// It delivers Reader exclusively, so a second provider of the reader is
// settled during resolution rather than at the moment somebody takes it up. It
// states no stance of its own: it is constructed before any stance is taken,
// and resolution counts it as enabled, because without it nobody else can read
// their configuration.
func Module() core.Module {
	return core.Module{
		Name:     moduleName,
		Provides: []core.Provision{{Token: (*Reader)(nil), Exclusive: true}},
		New: func(ctx context.Context, reg *core.Registry) (any, error) {
			l := &loader{
				reg:     reg,
				args:    os.Args[1:],
				environ: os.Environ(),
				stdout:  os.Stdout,
				stderr:  os.Stderr,
			}
			return l.load(ctx)
		},
	}
}
