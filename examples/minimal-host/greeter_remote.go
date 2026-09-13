package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
)

// The two greeter modules share a namespace and keep their paths disjoint,
// which is what lets the configuration of one capability's implementations sit
// together under greeter.
const (
	greeterNamespace = "greeter"
	greeterGroup     = "Greeter"
)

// remoteModuleName is the alternative implementation's module name.
const remoteModuleName = "greeter.remote"

// remotePath is where this module's input items live in the config data.
const remotePath = greeterNamespace + ".remote"

// remoteOptions is the carrier struct of the remote implementation.
type remoteOptions struct {
	Addr  string
	Token string
}

// remoteDefaults is the prototype this module declares. Addr has no default:
// it is the input that decides whether this implementation can run at all, and
// a default would make "nobody gave it" indistinguishable from a value.
var remoteDefaults = remoteOptions{Token: "no-token-configured"}

// init registers the module.
func init() { core.ProcessRegistry.Register(remoteGreeterModule()) }

// remoteGreeterModule is the greeter that talks to a service. It is the
// implementation a host chooses by configuring it: give it an address and it
// enables itself, and the built-in greeter stands down.
//
// It claims the capability exclusively as well, but its stance is a judgement
// rather than StateAuto: with its required input given it states StateEnabled,
// and without it StateDisabled with the reason the startup diagnostics print.
// A missing address is therefore not a failure and not a silence either.
func remoteGreeterModule() core.Module {
	return core.Module{
		Name:     remoteModuleName,
		Provides: []core.Provision{{Token: (*Greeter)(nil), Exclusive: true}},
		Resources: []any{config.Schema{
			Namespace: greeterNamespace,
			Mounts:    []config.Mount{{Path: "remote", Value: &remoteDefaults}},
			Items: map[string]config.Item{
				"remote.addr": {
					Origins:     config.OriginPrimary | config.OriginEnv | config.OriginFlag,
					FlagName:    "remote-addr",
					Placeholder: "HOST:PORT",
					Group:       greeterGroup,
					Description: "the address of the greeting service",
					Required:    true,
				},
				"remote.token": {
					Origins:     config.OriginPrimary | config.OriginEnv | config.OriginFlag,
					FlagName:    "remote-token",
					Placeholder: "TOKEN",
					Group:       greeterGroup,
					Description: "the credential the greeting service authenticates with",
					Sensitive:   true,
				},
			},
		}},
		Prepare: func(_ context.Context, reg *core.Registry) (core.Enablement, error) {
			if _, err := remoteConfig(reg); err != nil {
				// A required input nobody gave is the module's own
				// answer to whether it runs, not a startup failure:
				// a host that never configured the service never
				// asked for it.
				if errors.Is(err, config.ErrMissingRequired) {
					return core.Enablement{
						State: core.StateDisabled,
						Reason: "no connection address is configured: give " + remotePath +
							".addr in the config source, MINIHOST_GREETER__REMOTE__ADDR or " +
							"--remote-addr to run the remote greeter",
					}, nil
				}
				return core.Enablement{}, err
			}
			return core.Enablement{State: core.StateEnabled}, nil
		},
		New: func(_ context.Context, reg *core.Registry) (any, error) {
			// Reading the same values a second time: configuration is a
			// snapshot taken at startup, so the stance and the product
			// are built from the same answers.
			opts, err := remoteConfig(reg)
			if err != nil {
				return nil, err
			}
			return &remoteGreeter{addr: opts.Addr}, nil
		},
		Stop: func(_ context.Context, _ *core.Registry, _ any) error {
			fmt.Fprintln(os.Stdout, remoteModuleName+": stop")
			return nil
		},
		Close: func(_ context.Context, _ *core.Registry, _ any) error {
			fmt.Fprintln(os.Stdout, remoteModuleName+": close")
			return nil
		},
	}
}

// remoteConfig reads this module's own values back. The required rule is
// enforced by Decode, so the caller learns about a missing address from the
// error rather than by inspecting the result.
func remoteConfig(reg *core.Registry) (remoteOptions, error) {
	cfg, err := core.Resolve[config.Reader](reg)
	if err != nil {
		return remoteOptions{}, err
	}
	var opts remoteOptions
	if err := cfg.Decode(remotePath, &opts); err != nil {
		return remoteOptions{}, err
	}
	return opts, nil
}

// remoteGreeter is the product this module constructs. The example does not
// open a connection: what it demonstrates is which implementation the assembly
// picked, and the address it was configured with is the visible evidence.
type remoteGreeter struct {
	addr string
}

var _ Greeter = (*remoteGreeter)(nil)

// Greet renders the greeting the service is said to have produced.
func (g *remoteGreeter) Greet(name string) string {
	return "Greetings from " + g.addr + ", " + name
}

// Endpoint reports the configured address.
func (g *remoteGreeter) Endpoint() string { return g.addr }
