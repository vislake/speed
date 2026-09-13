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
		New: func(_ context.Context, reg *core.Registry) (any, error) {
			reader, err := core.Resolve[config.Reader](reg)
			if err != nil {
				return nil, fmt.Errorf(
					"log: the handler chain is assembled from configuration, and no module delivers it: %w", err)
			}
			return newLogger(reader)
		},
		Close: func(_ context.Context, _ *core.Registry, instance any) error {
			return closeLogger(instance)
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

// moduleAttrKey is the key of the attribute Named binds. It identifies which
// module wrote a record, which is what the key says; it is not the logger's
// own name.
const moduleAttrKey = "module"

// logger is this module's product, the implementation of the Logger
// capability. It is not a logger object: it hands out *slog.Logger values over
// the chain it assembled, and it hands out the registration interface for
// redaction rules.
type logger struct {
	// chain is the head of the formal chain. Every logger handed out writes
	// through it, so one record is filtered, judged and fanned out once.
	chain slog.Handler
	// release gives up this assembly's references to the destination
	// writers. A destination is closed when the last reference to it is
	// gone, which is never the case for standard output: the root package
	// holds one for the bootstrap chain.
	release func()
}

var _ Logger = (*logger)(nil)

// Named returns a logger carrying the calling module's name.
//
// The attribute is bound, so the redaction layer judges it once here rather
// than on every record. That binding is also why a module registers its
// redaction rules before it asks for a logger: an attribute bound earlier is
// governed by the rules that existed at binding time.
func (l *logger) Named(name string) *slog.Logger {
	return slog.New(l.chain).With(moduleAttrKey, name)
}

// Redaction returns the process-wide registration interface. The rule set is
// shared by every chain, the bootstrap one included: a sensitive key name is a
// fact about the process, not about one assembly.
func (l *logger) Redaction() Redaction { return processRedaction }

// newLogger reads the configuration, assembles the formal chain and builds the
// product.
//
// The real work takes a config.Reader rather than the registry, so the
// descriptor callback does nothing but resolve and delegate.
func newLogger(reader config.Reader) (*logger, error) {
	var cfg Config
	if err := reader.Decode(configPath, &cfg); err != nil {
		return nil, err
	}
	resolved, err := cfg.resolve()
	if err != nil {
		return nil, err
	}
	assembled, err := newChain(resolved)
	if err != nil {
		return nil, err
	}
	if len(resolved.outputs) == 0 {
		// An explicitly empty output list is legal and means no records are
		// written anywhere. It is said out loud because a logging module
		// producing nothing at all is indistinguishable from a broken one
		// when seen from outside. An absent outputs key is a different
		// thing: the carrier struct keeps the one output to standard
		// output, and this line is not written.
		report("the configuration gives an empty output list, so no log records are written anywhere; " +
			"remove the outputs key to get the default output to standard output")
	}
	return &logger{chain: assembled.handler, release: assembled.release}, nil
}

// closeLogger gives up this assembly's references to the destination writers.
//
// A nil instance is tolerated, and so is one of another type: a startup that
// fails part-way rolls back every module whatever stage it reached, so Close
// runs on modules whose New never returned a product.
func closeLogger(instance any) error {
	product, ok := instance.(*logger)
	if !ok || product == nil {
		return nil
	}
	product.release()
	return nil
}
