package main

import (
	"context"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/db"
)

// envPrefix is this host's environment variable prefix. Every derived variable
// name starts with it, and so does the one that names the config source,
// DBHOST_CONFIG. The three variables a scenario reads deliberately do not: one
// that did would be reported as an input item nobody reads, on the same stream
// the cases read the startup diagnostics from.
const envPrefix = "DBHOST"

func init() {
	core.ProcessRegistry.Register(hostModule())
	core.ProcessRegistry.Register(probeModule())
}

// hostModule carries what precedes all configuration: the environment prefix.
// There is no default locator, so a run with no --config argument at all is a
// legal one and the carrier struct's own values are what a module reads.
func hostModule() core.Module {
	return core.Module{
		Name:      "host",
		Resources: []any{config.HostIdentity{Prefix: envPrefix}},
	}
}

// probeModule is the dependant every scenario is built around. It takes up the
// database capability, so it is constructed after the implementation that
// delivers one and reaches its Migrate stage after the migrations have been
// applied; what each scenario makes of that position is in cases.go.
//
// It is also what gives the two startup-failure cases their teeth. A run that
// configured no engine at all would otherwise start up cleanly with every
// implementation disabled, and a case that only read the diagnostics could not
// tell that run from one that deliberately carries no database.
func probeModule() core.Module {
	return core.Module{
		Name:      probeModuleName,
		Requires:  []core.Requirement{{Token: (*db.Database)(nil)}},
		Resources: []any{declaredMigrations()},
		New:       probeNew,
		Migrate:   probeMigrate,
		// The scenario is the whole of this host's work, so ending it ends the
		// run: Run shuts every module down in order, releases the connection
		// pool with the module that holds it, and returns nil.
		//
		// Serve must not block, and this does not: cancelling is what this
		// module has left to do, and the driver advances through the stage
		// while the shutdown it starts is on its way.
		Serve: func(context.Context, *core.Registry, any) error {
			finish()
			return nil
		},
	}
}
