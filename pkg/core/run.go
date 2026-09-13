package core

import (
	"context"
	"fmt"
	"os"
	"reflect"
)

// Run advances every stage, runs until ctx is cancelled, then shuts down in
// reverse order and returns.
//
// Cancellation is the normal way to stop: with a clean cleanup the result is
// nil rather than ctx.Err(). A failure in any stage terminates the startup,
// rolls back every constructed instance and returns the original error. The
// lifecycle is one indivisible process rather than a pair of separately
// callable operations: what goes in comes out, and releasing resources does
// not depend on the host remembering a second call.
//
// Run may be called once. A registry that has been through the lifecycle
// cannot have its instances reused, and calling twice is a programming error
// at the call site, of the same kind as a duplicate registration.
func (r *Registry) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		panic("core: Run called more than once on the same registry. A registry that has " +
			"been through the lifecycle cannot be reused; build another one with New")
	}
	r.running = true
	r.mu.Unlock()

	if err := r.startup(ctx); err != nil {
		return r.shutdown(ctx, err)
	}
	<-ctx.Done()
	return r.shutdown(ctx, nil)
}

// startup advances Prepare through Serve. It returns the first failure; the
// caller rolls back.
func (r *Registry) startup(ctx context.Context) error {
	all := r.Modules()

	// The config module is constructed first, ahead of any stance: it
	// collects the command line and the environment, reads the
	// configuration source and validates it, and every other module reads
	// its own configuration from it while stating whether it is enabled.
	// This, together with being an implicit dependency of everyone, is the
	// whole of the special treatment, and the module name is how it is
	// recognised.
	//
	// A registry without a module named config skips the bootstrap and
	// advances the rest as usual. The config module is the answer to the
	// startup cycle, not a precondition of assembly: without it there is no
	// cycle to break. A module that needs configuration gets
	// ErrMissingProvider when it tries to take up the reader, so a missing
	// import does not decay into a silently absent piece of functionality.
	if cfg, ok := r.Lookup(configModuleName); ok {
		if err := r.construct(ctx, cfg); err != nil {
			return err
		}
	}

	stances, err := r.prepare(ctx, all)
	if err != nil {
		return err
	}

	final, err := resolveEnablement(stances)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.enablement = final
	r.mu.Unlock()
	writeDiagnostics(os.Stderr, final)

	enabled := make([]Module, 0, len(all))
	for _, m := range all {
		if final[m.Name].State != StateDisabled {
			enabled = append(enabled, m)
		}
	}
	order, err := planConstruction(enabled, all, final)
	if err != nil {
		return err
	}

	for _, name := range order {
		// The config module was constructed during Prepare, so the New
		// stage passes over it. Every later stage treats it like any
		// other module.
		if name == configModuleName {
			continue
		}
		if err := r.construct(ctx, moduleByName(enabled, name)); err != nil {
			return err
		}
	}

	stages := []struct {
		name string
		call func(Module) func(context.Context, *Registry, any) error
	}{
		{"Migrate", func(m Module) func(context.Context, *Registry, any) error { return m.Migrate }},
		{"Init", func(m Module) func(context.Context, *Registry, any) error { return m.Init }},
		{"Start", func(m Module) func(context.Context, *Registry, any) error { return m.Start }},
		// Serve runs after every Start has returned: dependency order says
		// "after my dependencies", and an entry point needs "after
		// everyone".
		{"Serve", func(m Module) func(context.Context, *Registry, any) error { return m.Serve }},
	}
	for _, stage := range stages {
		for _, name := range order {
			m := moduleByName(enabled, name)
			callback := stage.call(m)
			if callback == nil {
				continue
			}
			if err := callback(ctx, r, r.instanceOf(name)); err != nil {
				return fmt.Errorf("core: module %q %s: %w", name, stage.name, err)
			}
		}
	}
	return nil
}

// prepare runs the stance stage over every registered module except config,
// which is already constructed and counts as StateEnabled in resolution.
//
// The stage guarantees no order within itself: a module only reads its own
// configuration and states a verdict, and no instance other than the config
// one exists, so there can be no ordering between them.
func (r *Registry) prepare(ctx context.Context, all []Module) ([]stance, error) {
	stances := make([]stance, 0, len(all))
	for _, m := range all {
		if m.Name == configModuleName {
			stances = append(stances, stance{module: m, state: Enablement{State: StateEnabled}})
			continue
		}
		state := Enablement{}
		if m.Prepare != nil {
			stated, err := m.Prepare(ctx, r)
			if err != nil {
				return nil, fmt.Errorf("core: module %q Prepare: %w", m.Name, err)
			}
			state = stated
		}
		stances = append(stances, stance{module: m, state: state})
	}
	return stances, nil
}

// construct runs a module's New callback, records the product, and asserts
// that every capability it declared really arrived.
func (r *Registry) construct(ctx context.Context, m Module) error {
	var instance any
	if m.New != nil {
		product, err := m.New(ctx, r)
		if err != nil {
			return fmt.Errorf("core: module %q New: %w", m.Name, err)
		}
		instance = product
	}

	r.mu.Lock()
	if m.New != nil {
		r.instances[m.Name] = instance
	}
	r.lifecycle = append(r.lifecycle, m.Name)
	r.mu.Unlock()

	return assertDelivered(m, instance)
}

// assertDelivered checks the product against the delivery declaration, so a
// declaration that drifted from the code surfaces on the construction that
// produced it rather than on some later module that cannot find a dependency.
func assertDelivered(m Module, instance any) error {
	it := reflect.TypeOf(instance)
	for _, ct := range declaredCapabilities(m) {
		if it != nil && it.AssignableTo(ct) {
			continue
		}
		got := "nothing, because the module has no New callback"
		if it != nil {
			got = typeName(it)
		}
		return fmt.Errorf("%w: module %q declares capability %s in Provides but constructed %s",
			ErrUndeliveredCapability, m.Name, typeName(ct), got)
	}
	return nil
}
