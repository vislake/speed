package app

import "context"

// Hooks are the host's own steps inside the assembly: the points where a
// host does what only it can do, in the one order that works. Each callback
// receives the half-built Application, so it reads the registry, the handler
// and the kernel exactly when those exist.
//
// A nil field is skipped; a callback that returns an error fails the assembly
// at its stage, and the stage rollback tears the built resources down.
type Hooks struct {
	// PostBootstrap runs in stage 5, after the kernel bootstrapped the
	// module set and the bootstrap-key binding was verified. It is where the
	// typed Attach calls belong: each module's Attach runs exactly once,
	// after Bootstrap has returned, before the first step that consumes its
	// Service (the registry's declaration seats are only complete once every
	// module registered).
	PostBootstrap func(ctx context.Context, a *Application) error

	// PostAttach runs in stage 6, after the host's typed attaches. It is
	// where the wiring that needs bootstrapped services belongs: the seam
	// bridges between modules, platform credentials written at boot, and
	// subscriptions installed onto the live bus. The HTTP face is composed
	// after this stage, so a wiring step here may not yet reach the handler.
	PostAttach func(ctx context.Context, a *Application) error

	// PreServe runs in stage 8, after the HTTP face exists and before the
	// worker starts -- and therefore before anything listens. It is where
	// the steps that must complete before the first request belongs: schemas
	// created imperatively, demo seeds registering through the composed
	// handler, and any start-up step a request could otherwise race.
	PreServe func(ctx context.Context, a *Application) error
}

// WithHooks registers the host's assembly hooks.
func WithHooks(h Hooks) Option {
	return func(c *engineConfig) { c.hooks = h }
}
