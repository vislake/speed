package log

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
)

// moduleName is this module's name in the registry.
const moduleName = "log"

// bootstrapLevel is the level the bootstrap chain filters by, held by the root
// package and set from configuration in Prepare.
//
// It has to be mutable: the bootstrap chain writes records before this module
// is constructed, while the configured level is not readable until Prepare, so
// the value the loggers already handed out filter by is the one that has to
// change. Its own reads and writes are atomic, so it is not covered by the
// root mutex.
var bootstrapLevel = new(slog.LevelVar)

// init registers this module with the process registry, so a host that imports
// the package has logging assembled from its configuration.
func init() { core.ProcessRegistry.Register(Module()) }

// Module is this module's descriptor, exported for the registries that do not
// inherit the process-level registrations.
//
// It delivers Logger exclusively and states StateEnabled. The pair is
// deliberate: resolution only disables a provider that claims exclusivity and
// states StateAuto, so this module never stands down. A second provider that
// stays enabled fails the startup with ErrExclusiveViolated instead of
// silently replacing this one, and silence is what has to be avoided — the
// dependants would take up the other implementation while the configured
// destinations, formats and rotation parameters lose their only consumer.
//
// config is an implicit dependency of every module, so it is not declared: the
// registry constructs it first and Prepare reads through it.
func Module() core.Module {
	return core.Module{
		Name:      moduleName,
		Provides:  []core.Provision{{Token: (*Logger)(nil), Exclusive: true}},
		Resources: []any{schema()},
		Prepare: func(_ context.Context, reg *core.Registry) (core.Enablement, error) {
			reader, err := core.Resolve[config.Reader](reg)
			if err != nil {
				return core.Enablement{}, fmt.Errorf(
					"log: the level is read from configuration, and no module delivers it: %w", err)
			}
			return prepare(reader)
		},
	}
}

// prepare reads the level and applies it to the bootstrap chain, then states
// that this module runs.
//
// The formal chain's level is not settled here. Each chain carries its own
// level layer, and the two are not the same instance: the layer's downstream is
// fixed when it is built, and the bootstrap chain's downstream is a handler
// writing to stdout while the formal chain's is the fan-out.
func prepare(reader config.Reader) (core.Enablement, error) {
	var cfg Config
	if err := reader.Decode(configPath, &cfg); err != nil {
		return core.Enablement{}, err
	}
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return core.Enablement{}, err
	}
	bootstrapLevel.Set(level)
	return core.Enablement{State: core.StateEnabled}, nil
}
