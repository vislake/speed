package main

import (
	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
)

// envPrefix is the environment variable prefix of this host. Every derived
// variable name starts with it, and so does the one that names the config
// source, MINIHOST_CONFIG.
const envPrefix = "MINIHOST"

// init registers the host's own module. What a host hands the assembly travels
// through the module mechanism like everything else: the identity is a
// resource on a module the host registers, not an argument of Run, so the
// signature of Run does not grow as hosts declare more.
func init() { core.ProcessRegistry.Register(hostModule()) }

// hostModule carries the two inputs that precede all configuration and can
// therefore come from nowhere else.
//
// DefaultLocator is left empty on purpose: this host has no installed config
// file to fall back on, and a run with no primary config source is a legal
// one. The locator then comes from MINIHOST_CONFIG or --config, or from
// nowhere, and the defaults, the environment and the command line carry the
// run on their own.
func hostModule() core.Module {
	return core.Module{
		Name:      "host",
		Resources: []any{config.HostIdentity{Prefix: envPrefix}},
	}
}
