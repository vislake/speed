package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/log"
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

// remoteTokenKey names the credential three times over: it is this module's
// input item, the redaction rule this module registers, and the attribute key
// the credential is logged under. One name, so a reader sees that the item
// declared Sensitive and the key the logging layer masks are the same thing,
// and so that renaming the input cannot leave the rule pointing at nothing.
const remoteTokenKey = "remote.token"

// The rest of this module's logging vocabulary.
//
// tokenGivenKey carries whether the credential this module was built with came
// from the configuration or is the placeholder default. It is derived from the
// credential without carrying any of it, which is what makes it safe to log
// beside a masked value and useful next to one: a masked attribute looks the
// same whether the host configured a credential or never gave one.
const (
	configuredMsg = "configured"
	addrAttrKey   = "addr"
	tokenGivenKey = "token_given"
)

// remoteOptions is the carrier struct of the remote implementation.
type remoteOptions struct {
	Addr  string
	Token string
}

// remoteDefaults is the prototype this module declares. Addr has no default:
// it is the input that decides whether this implementation can run at all, and
// a default would make "nobody gave it" indistinguishable from a value. Token
// does have one, and it is a placeholder that says out loud that no credential
// was given: the value a host replaces, never one it could authenticate with.
//
//nolint:gosec // G101: the placeholder above stands in for a missing credential and is not one, so there is nothing here to leak.
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
		Requires: []core.Requirement{{Token: (*log.Logger)(nil)}},
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
				remoteTokenKey: {
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
			logger, err := core.Resolve[log.Logger](reg)
			if err != nil {
				return nil, err
			}
			// Register the rule before taking the logger, and never the
			// other way round. A rule governs the judgements made after
			// it, and an attribute bound through With is judged once when
			// it is bound — Named binds one already, and the credential
			// below is bound too. Registering after the binding leaves
			// the credential in the clear in every record this module
			// writes, while the call sites go on looking correct.
			logger.Redaction().AddKeys(remoteTokenKey)
			bound := logger.Named(remoteModuleName).With(
				addrAttrKey, opts.Addr,
				remoteTokenKey, opts.Token,
				tokenGivenKey, opts.Token != remoteDefaults.Token,
			)
			// A record written from inside New, and it is already in the
			// configured destination: declaring the dependency on the
			// Logger capability is what put log ahead of this module, and
			// nobody wrote that order down anywhere.
			bound.Info(configuredMsg)
			return &remoteGreeter{addr: opts.Addr, log: bound}, nil
		},
		Stop: func(_ context.Context, _ *core.Registry, instance any) error {
			remoteOf(instance).log.Info(stopMsg)
			return nil
		},
		Close: func(_ context.Context, _ *core.Registry, instance any) error {
			remoteOf(instance).log.Info(closeMsg)
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

// remoteOf recovers this module's own product from what the driver hands back,
// for the same reason and with the same guarantee as the application module's.
func remoteOf(instance any) *remoteGreeter {
	//nolint:errcheck // the value came from this module's own New, so another
	// type would be a defect in the driver and the panic is the report.
	return instance.(*remoteGreeter)
}

// remoteGreeter is the product this module constructs. The example does not
// open a connection: what it demonstrates is which implementation the assembly
// picked, and the address it was configured with is the visible evidence.
type remoteGreeter struct {
	addr string
	// log carries this module's name and the connection it was built with,
	// the credential among them and masked by the rule registered above.
	log *slog.Logger
}

var _ Greeter = (*remoteGreeter)(nil)

// Greet renders the greeting the service is said to have produced.
func (g *remoteGreeter) Greet(name string) string {
	return "Greetings from " + g.addr + ", " + name
}

// Endpoint reports the configured address.
func (g *remoteGreeter) Endpoint() string { return g.addr }
