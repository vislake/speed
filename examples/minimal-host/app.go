package main

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/log"
)

// appModuleName is the application module's name.
const appModuleName = "app"

// The messages this host's modules log under. A message is one word, so the
// record it names is found by comparing the message rather than by matching a
// rendered line: the attributes carry everything else.
//
// readyMsg is the one an operator, or a test, waits for: it is written once
// every module has been started and the entry points are open. It is a record
// like the rest of this host's narration, so it goes wherever the run was
// configured to log, and what identifies it is the message and the module that
// wrote it rather than the text of a whole line.
const (
	greetingMsg = "greeting"
	endpointMsg = "endpoint"
	readyMsg    = "ready"
	stopMsg     = "stop"
	closeMsg    = "close"
)

// The attribute keys this host's own records carry.
const (
	greetingAttrKey   = "greeting"
	endpointAttrKey   = "endpoint"
	greetingIDAttrKey = "greeting_id"
)

// moduleAttrKey is the key of the attribute log.Logger.Named binds, the one an
// operator filters a module's records by. It is reproduced here because pkg/log
// does not export it: this host's own cases have to name it, and a host that
// filters its records in a collector has to write it down as well.
const moduleAttrKey = "module"

// greetings counts the greetings this process produced, and is what gives each
// one the identifier its records are correlated by. A real entry point mints
// one per request; this host produces exactly one.
var greetings atomic.Uint64

// init registers the application module.
func init() { core.ProcessRegistry.Register(appModule()) }

// appModule is the consumer: it requires the greeter capability and never
// names an implementation of it. Which one it gets is settled by resolution,
// out of what the host imported and what the configuration says, and this
// module changes in neither case.
//
// The requirement is also what orders the run. The provider is constructed
// before this module and shut down after it, in both directions without
// anybody writing the order down. The logger is required the same way and
// orders the run the same way: log is constructed before this module, so its
// configured destinations are already assembled here, and it is closed after
// it, so a record written from Close still reaches them.
func appModule() core.Module {
	return core.Module{
		Name: appModuleName,
		Requires: []core.Requirement{
			{Token: (*Greeter)(nil)},
		},
		New: func(_ context.Context, reg *core.Registry) (any, error) {
			greeter, err := core.Resolve[Greeter](reg)
			if err != nil {
				return nil, err
			}
			logger, err := core.Resolve[log.Logger](reg)
			if err != nil {
				return nil, err
			}
			// The field holds a *slog.Logger: the calling surface is the
			// standard library's, and this module's call sites depend on
			// log/slog rather than on the module that handed it over.
			return &application{greeter: greeter, log: logger.Named(appModuleName)}, nil
		},
		Start: func(_ context.Context, _ *core.Registry, instance any) error {
			a := appOf(instance)
			a.log.Info(endpointMsg, endpointAttrKey, a.greeter.Endpoint())
			return nil
		},
		Serve: func(ctx context.Context, _ *core.Registry, instance any) error {
			// Serve opens the entry points and returns. A real host
			// would start its listener on a goroutine here; blocking
			// would starve every module after it in the stage.
			a := appOf(instance)
			a.greet(ctx, "world")
			a.log.Info(readyMsg)
			return nil
		},
		Stop: func(_ context.Context, _ *core.Registry, instance any) error {
			appOf(instance).log.Info(stopMsg)
			return nil
		},
		Close: func(_ context.Context, _ *core.Registry, instance any) error {
			appOf(instance).log.Info(closeMsg)
			return nil
		},
	}
}

// appOf recovers this module's own product from what the driver hands back.
//
// The value is what this module's own New returned, so a different type would
// be a defect in the driver rather than a case to handle: the panic is the
// intended report. The instance is never nil either — the driver only calls the
// later stages of a module whose New returned.
func appOf(instance any) *application {
	//nolint:errcheck // the doc comment above says why the panic is the report.
	return instance.(*application)
}

// application is the product of the application module: whatever the host's
// own program is, holding the capabilities it took up.
type application struct {
	greeter Greeter
	// log is the logger this module holds for the records it writes outside
	// any request. It comes from Named, so every record it writes carries
	// this module's name and reaches the configured destinations.
	log *slog.Logger
}

// greet produces one greeting, and is where this host demonstrates the path a
// logger travels on a context.
//
// The injection happens here because this is where the unit of work begins: a
// greeting is what an HTTP request or a job would be in a real host. The logger
// injected is the one this module took through Named, never log.Default() —
// injecting the default logger would take the whole path off the configured
// destinations while still producing records, which is the failure the split is
// designed to make visible rather than silent.
//
// The stage callbacks above do not go through the context. The context they are
// given is the host's own, which carries no logger: a log.FromContext on it
// would fall back to log.Default() and quietly leave the configured
// destination.
func (a *application) greet(ctx context.Context, name string) {
	ctx = log.WithLogger(ctx, a.log)
	// The identifier is bound to the context's logger rather than passed
	// with each record: everything written under this unit of work carries
	// it, and the binding is judged for redaction once.
	ctx = log.WithAttrs(ctx, greetingIDAttrKey, greetings.Add(1))
	writeGreeting(ctx, a.greeter, name)
}

// writeGreeting is the downstream call site, and it takes no logger: whatever
// is on the context is what it writes through. That is the whole of what a
// function below an injection point has to know.
func writeGreeting(ctx context.Context, greeter Greeter, name string) {
	log.FromContext(ctx).Info(greetingMsg, greetingAttrKey, greeter.Greet(name))
}
