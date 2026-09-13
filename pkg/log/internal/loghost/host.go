package main

import (
	"context"
	"log/slog"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/log"
)

// envPrefix is this host's environment variable prefix. Every derived variable
// name starts with it, and so does the one that names the config source,
// LOGHOST_CONFIG. The variable that selects the scenario deliberately does not
// start with it: one that did would be reported as an input item nobody reads,
// on the same stream the cases read the module's own notices from.
const envPrefix = "LOGHOST"

// appModuleName is the application module's name, and the name it logs under.
const appModuleName = "app"

func init() {
	core.ProcessRegistry.Register(hostModule())
	core.ProcessRegistry.Register(appModule())
}

// hostModule carries what precedes all configuration: the environment prefix.
// There is no default locator, so a run with no config source at all is a
// legal one and the carrier struct's own values are what a module reads.
func hostModule() core.Module {
	return core.Module{
		Name:      "host",
		Resources: []any{config.HostIdentity{Prefix: envPrefix}},
	}
}

// appModule is the consumer: it requires the logging capability and takes a
// named logger in New, the way a real module does.
//
// The requirement is also what orders the run, so the logging module is
// constructed, and its chain assembled from configuration, before this module
// asks for a logger.
func appModule() core.Module {
	return core.Module{
		Name:     appModuleName,
		Requires: []core.Requirement{{Token: (*log.Logger)(nil)}},
		New: func(_ context.Context, reg *core.Registry) (any, error) {
			capability, err := core.Resolve[log.Logger](reg)
			if err != nil {
				return nil, err
			}
			// Registration comes before the logger is taken: Named binds
			// an attribute, and a bound attribute is judged once at
			// binding time against the rules that exist then.
			capability.Redaction().AddKeys(sensitiveKey)
			return &application{logger: capability.Named(appModuleName)}, nil
		},
		Serve: func(ctx context.Context, _ *core.Registry, instance any) error {
			//nolint:errcheck // the instance is what this module's own New
			// returned, so a different type would be a defect in the host,
			// not a case to handle: the panic is the intended report.
			a := instance.(*application)
			runScenario(ctx, a.logger)
			// The scenario is the whole of this host's work, so ending it
			// ends the run: Run shuts every module down and returns.
			finish()
			return nil
		},
	}
}

// application is the product of the application module, holding the logger it
// took up.
type application struct {
	logger *slog.Logger
}
