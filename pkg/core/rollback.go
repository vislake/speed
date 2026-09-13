package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// shutdown runs Stop and then Close over every constructed instance, in
// reverse construction order, and returns the primary error with whatever the
// cleanup added.
//
// Every constructed instance is covered whatever stage the failure happened
// in. Rolling back only the modules that completed the failing stage would
// leave behind the ones constructed earlier that never got that far, holding
// connections and goroutines nobody releases. That is also why Stop and Close
// have to tolerate an instance that was never initialised or started.
//
// A Stop failure does not abort the stage: a failed drain notice must not hold
// back the Close that follows. Cleanup failures are aggregated behind the
// primary error rather than replacing it, because what terminated the startup
// is the first failure, not a secondary one on the way out. With no primary
// error and a clean cleanup the result is nil: cancellation is the normal way
// to stop, and reporting it as an error would make every host special-case a
// move it just made itself.
func (r *Registry) shutdown(ctx context.Context, primary error) error {
	r.mu.RLock()
	order := slices.Clone(r.lifecycle)
	r.mu.RUnlock()
	slices.Reverse(order)

	failures := []error{primary}
	for _, name := range order {
		m, ok := r.Lookup(name)
		if !ok || m.Stop == nil {
			continue
		}
		if err := m.Stop(ctx, r, r.instanceOf(name)); err != nil {
			failures = append(failures, fmt.Errorf("core: module %q Stop: %w", name, err))
		}
	}
	for _, name := range order {
		m, ok := r.Lookup(name)
		if !ok || m.Close == nil {
			continue
		}
		if err := m.Close(ctx, r, r.instanceOf(name)); err != nil {
			failures = append(failures, fmt.Errorf("core: module %q Close: %w", name, err))
		}
	}
	return errors.Join(failures...)
}

// instanceOf returns the product a module was constructed with, or nil for a
// module that has no New callback.
func (r *Registry) instanceOf(name string) any {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.instances[name]
}
