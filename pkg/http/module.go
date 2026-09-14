package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/log"
)

// Module is the descriptor an implementation subpackage registers. name is the
// module's name in the registry, and newEngine builds the routing engine one
// endpoint's routes are mounted on — the seam that keeps the engine's own type
// off this package's surface.
//
// The root package registers nothing by itself. A host chooses an entry point
// by importing the subpackage that binds the engine it wants, and importing
// this package alone brings in the registration surface and the request
// helpers without opening a port.
//
// The capability is delivered exclusively and the stance is StateEnabled. The
// pair is deliberate: resolution only stands a provider down when it claims
// exclusivity and states StateAuto, so a second entry point implementation
// fails the startup instead of quietly taking the registrations that were meant
// for this one.
//
// config is an implicit dependency of every module and is not declared. The
// Logger capability is declared optional: without it nothing is injected into
// the request context, and the request path's records reach the process default
// logger's destination rather than the configured one.
func Module(name string, newEngine func() Engine) core.Module {
	if newEngine == nil {
		panic(fmt.Sprintf("http: Module(%q, nil) has no engine to build. Pass the function that "+
			"builds one routing engine per endpoint; it is what binds the engine to this "+
			"module without putting its type on the surface", name))
	}
	return core.Module{
		Name:      name,
		Requires:  []core.Requirement{{Token: (*log.Logger)(nil), Optional: true}},
		Provides:  []core.Provision{{Token: (*Router)(nil), Exclusive: true}},
		Resources: []any{schema()},

		Prepare: func(_ context.Context, reg *core.Registry) (core.Enablement, error) {
			reader, err := core.Resolve[config.Reader](reg)
			if err != nil {
				return core.Enablement{}, fmt.Errorf("http: the listening endpoints are read "+
					"from configuration, and no module delivers it: %w", err)
			}
			return prepare(reader, core.Resources[Spec](reg))
		},

		New: func(_ context.Context, reg *core.Registry) (any, error) {
			reader, err := core.Resolve[config.Reader](reg)
			if err != nil {
				return nil, fmt.Errorf("http: the listening endpoints are read from "+
					"configuration, and no module delivers it: %w", err)
			}
			injected, err := requestLogger(reg, name)
			if err != nil {
				return nil, err
			}
			return newModule(reader, newEngine, injected)
		},

		// The gate is opened in Migrate and sealed in Start, so the window
		// registrations are accepted in is exactly the Init stage. Neither
		// end of it could be this module's own Init or Serve callback: a
		// module that never declared a dependency on Router may legally
		// resolve it and register in an Init that runs before this one, and
		// the Start stage together with the Serve callbacks ordered ahead of
		// this module are all outside Init while this module has not reached
		// its own Serve yet.
		Migrate: func(_ context.Context, _ *core.Registry, instance any) error {
			if r, ok := productOf(instance); ok {
				r.open()
			}
			return nil
		},
		Start: func(_ context.Context, _ *core.Registry, instance any) error {
			if r, ok := productOf(instance); ok {
				r.seal()
			}
			return nil
		},
		Serve: func(_ context.Context, _ *core.Registry, instance any) error {
			r, ok := productOf(instance)
			if !ok {
				return nil
			}
			return r.serve()
		},
		Stop: func(ctx context.Context, _ *core.Registry, instance any) error {
			r, ok := productOf(instance)
			if !ok {
				return nil
			}
			return r.stop(ctx)
		},
		Close: func(ctx context.Context, _ *core.Registry, instance any) error {
			r, ok := productOf(instance)
			if !ok {
				return nil
			}
			return r.close(ctx)
		},
	}
}

// productOf recovers this module's product from what the registry hands back.
// A nil instance, or one of another type, is tolerated rather than asserted:
// a startup that fails part-way rolls back every constructed instance, so the
// later callbacks run on modules whose New never returned a product.
func productOf(instance any) (*router, bool) {
	r, ok := instance.(*router)
	return r, ok && r != nil
}

// prepare reads the configuration and states whether this module runs.
//
// With no endpoint configured the module stands down with a reason naming the
// key to write: an entry point that silently does nothing is the failure this
// stance exists to prevent. The declared fragments are checked only on the path
// where the module runs, since a disabled module publishes nothing for a reader
// to merge.
func prepare(reader config.Reader, specs []core.Resource[Spec]) (core.Enablement, error) {
	settings, err := readSettings(reader)
	if err != nil {
		return core.Enablement{}, err
	}
	if len(settings) == 0 {
		return core.Enablement{
			State: core.StateDisabled,
			Reason: fmt.Sprintf("no listening endpoint is configured: %s.%s is empty, "+
				"so there is no address to serve on. Declare an endpoint under that key "+
				"to switch the entry point on", configNamespace, endpointsItem),
		}, nil
	}
	if err := validateSpecs(specs); err != nil {
		return core.Enablement{}, err
	}
	return core.Enablement{State: core.StateEnabled}, nil
}

// readSettings decodes this module's section and validates it. Prepare and New
// both go through it, so the two see one set of endpoints and one set of
// defaults.
func readSettings(reader config.Reader) ([]endpointSettings, error) {
	var cfg moduleConfig
	if err := reader.Decode(configPath, &cfg); err != nil {
		return nil, err
	}
	return cfg.resolve()
}

// newModule builds the product: one endpoint per configured entry, with the
// gate still shut. Nothing is bound here — the address is bound in Serve.
func newModule(reader config.Reader, newEngine func() Engine, injected *slog.Logger) (*router, error) {
	settings, err := readSettings(reader)
	if err != nil {
		return nil, err
	}
	r := newRouter(settings, newEngine)
	r.injected = injected
	r.logger = injected
	if r.logger == nil {
		// No Logger capability in this assembly. This module's own
		// diagnostics still have somewhere to go; what the absence changes
		// is that nothing is injected into the request context.
		r.logger = log.Default()
	}
	return r, nil
}

// requestLogger takes up the optional Logger capability and returns the logger
// to inject into each request's context, or nil when no module delivers it.
//
// Absence is a legal configuration and is told apart from a real failure by the
// sentinel: anything other than "no provider" is a failure of the lookup itself
// and terminates the startup.
func requestLogger(reg *core.Registry, name string) (*slog.Logger, error) {
	delivered, err := core.Resolve[log.Logger](reg)
	if err != nil {
		if errors.Is(err, core.ErrMissingProvider) {
			return nil, nil
		}
		return nil, fmt.Errorf("http: taking up the logger to inject into request contexts: %w", err)
	}
	return delivered.Named(name), nil
}

// serve assembles every endpoint's chain and binds its address.
//
// Both are synchronous, so a failure of either terminates the startup rather
// than surfacing later in a goroutine; the accept loops are what runs on their
// own. An endpoint that fails leaves the ones bound before it listening, and
// the rollback that follows the startup failure closes them: Stop and Close run
// over every constructed instance whatever stage it reached.
func (r *router) serve() error {
	for _, name := range r.names {
		e := r.endpoints[name]
		handler, err := e.assemble(r.newEngine(), r.injected, r.logger)
		if err != nil {
			return err
		}
		if err := e.socket.start(e.settings, handler, r.logger); err != nil {
			return err
		}
	}
	return nil
}

// stop closes every endpoint's socket and leaves the drains running. It returns
// as soon as no endpoint is accepting any more, which is the whole of what Stop
// promises.
func (r *router) stop(ctx context.Context) error {
	var failures []error
	for _, name := range r.names {
		if err := r.endpoints[name].socket.stop(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// close waits for every endpoint to finish draining and reports the failures
// together.
//
// The endpoints are waited on one after another, which is the same as waiting
// on them together: each drain has been running since that endpoint's own Stop,
// so one endpoint timing out does not shorten the wait of any other.
func (r *router) close(ctx context.Context) error {
	var failures []error
	for _, name := range r.names {
		if err := r.endpoints[name].socket.close(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
