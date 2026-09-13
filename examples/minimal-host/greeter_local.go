package main

import (
	"context"
	"fmt"
	"os"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
)

// localModuleName is the built-in implementation's module name. It is also the
// name the startup diagnostics report it under when it stands down.
const localModuleName = "greeter.local"

// localPath is where this module's input items live in the config data:
// the namespace plus the mount path. The module assembles it itself, which is
// also what it passes to Decode.
const localPath = greeterNamespace + ".local"

// localOptions is the carrier struct: the field structure is the declaration
// of the input items, and each field's current value is that item's default.
type localOptions struct {
	Salutation string
}

// localDefaults is the prototype this module declares. It is read for the
// structure and for the defaults and never receives the values of a run, so
// the same prototype may be declared into two registries without either
// seeing the other's values.
var localDefaults = localOptions{Salutation: "Hello"}

// init registers the module. Importing the package that holds it is all a host
// does to make it available, here and for a module that arrives through a
// dependency the host never named.
func init() { core.ProcessRegistry.Register(localGreeterModule()) }

// localGreeterModule is the built-in greeter: the implementation that runs
// when nothing else is configured.
//
// It claims the capability exclusively and states StateAuto, which is the pair
// that makes a default implementation stand down. Exclusive says no other
// provider may stay enabled alongside it; StateAuto says it has no opinion of
// its own, so resolution disables it as soon as another provider is explicitly
// enabled, rather than failing the startup over the two of them.
func localGreeterModule() core.Module {
	return core.Module{
		Name:     localModuleName,
		Provides: []core.Provision{{Token: (*Greeter)(nil), Exclusive: true}},
		Resources: []any{config.Schema{
			Namespace: greeterNamespace,
			Mounts:    []config.Mount{{Path: "local", Value: &localDefaults}},
			Items: map[string]config.Item{
				"local.salutation": {
					Origins:     config.OriginPrimary | config.OriginEnv | config.OriginFlag,
					FlagName:    "salutation",
					Placeholder: "WORD",
					Group:       greeterGroup,
					Description: "the word the built-in greeter opens with",
				},
			},
		}},
		Prepare: func(_ context.Context, _ *core.Registry) (core.Enablement, error) {
			// Nothing to judge: the built-in greeter needs no
			// configuration and takes no position on whether it runs.
			// Stating it explicitly is the same as having no Prepare
			// callback at all.
			return core.Enablement{State: core.StateAuto}, nil
		},
		New: func(_ context.Context, reg *core.Registry) (any, error) {
			cfg, err := core.Resolve[config.Reader](reg)
			if err != nil {
				return nil, err
			}
			var opts localOptions
			if err := cfg.Decode(localPath, &opts); err != nil {
				return nil, err
			}
			return &localGreeter{salutation: opts.Salutation}, nil
		},
		Stop: func(_ context.Context, _ *core.Registry, _ any) error {
			fmt.Fprintln(os.Stdout, localModuleName+": stop")
			return nil
		},
		Close: func(_ context.Context, _ *core.Registry, _ any) error {
			fmt.Fprintln(os.Stdout, localModuleName+": close")
			return nil
		},
	}
}

// localGreeter is the product this module constructs.
type localGreeter struct {
	salutation string
}

// Compile-time proof that the product delivers the capability the descriptor
// declares. The registry asserts the same thing at construction time; this
// catches it a compilation earlier.
var _ Greeter = (*localGreeter)(nil)

// Greet renders the greeting.
func (g *localGreeter) Greet(name string) string { return g.salutation + ", " + name }

// Endpoint reports that the greeting was produced in this process.
func (g *localGreeter) Endpoint() string { return "local" }
