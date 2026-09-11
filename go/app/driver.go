package app

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/vislake/speed/go/pkgcore"
)

// driver.go carries the engine's stage driver -- the piece of the engine
// that only orchestrates. Assemble walks a populated ComponentRegistry
// through the assembly's first five stages; Shutdown performs the two-phase
// close; RunAssembly is the sugar that creates the registry, runs the
// loader and drives both around a context's lifetime.
//
// The driver contains no HTTP assembly and no listening: a host's own
// application component assembles the routes from the declaration seats and
// owns the listener, exactly as any component owns the resources it starts.

// Assemble drives one assembly over reg: the loader runs first (the
// configuration load of the Prepare stage's first beat), then the registry
// is walked through Prepare, Construct, Verify, Init and Start in order.
//
// A failure at any stage is returned as-is. The registry itself owns the
// failure semantics: a failed Prepare leaves the registry exactly as it was
// (nothing was constructed), and a failure from Construct on has already
// rolled the assembly back -- every constructed component closed in reverse
// order, exactly once -- before the error is returned, so a caller never has
// to attempt a teardown of its own.
func Assemble(ctx context.Context, reg *pkgcore.ComponentRegistry, spec LoadSpec) error {
	if err := Load(ctx, reg, spec); err != nil {
		return err
	}
	if err := reg.Prepare(ctx); err != nil {
		return err
	}
	if err := reg.Construct(ctx); err != nil {
		return err
	}
	if err := reg.Verify(ctx); err != nil {
		return err
	}
	if err := reg.Init(ctx); err != nil {
		return err
	}
	return reg.Start(ctx)
}

// Shutdown performs the two-phase close every speed application shares: the
// non-blocking Stop notification goes out in reverse dependency order
// (failures ignored -- a notification that cannot be delivered must not keep
// the Close that waits for the drain from running), then the Close stage
// waits out the drain and releases every constructed component's resources,
// in reverse dependency order, aggregating failures. Both phases run exactly
// once, whatever the registry's state: a registry whose assembly never
// constructed anything closes as a no-op.
//
// ctx bounded the whole shutdown -- the caller's own bound, if any, sits on
// top of the per-step timeout each component applies internally.
func Shutdown(ctx context.Context, reg *pkgcore.ComponentRegistry) error {
	_ = reg.Stop(ctx)
	return reg.Close(ctx)
}

// RunAssembly is the engine's Run sugar for a host with no HTTP face of its
// own: it creates a ComponentRegistry from the global component
// registration plus extra, runs the loader over spec, drives the assembly
// through Start, waits until ctx is done (SIGINT and SIGTERM are overlaid on
// it, so a caller passes its base context), and then shuts the assembly down
// in the two phases of Shutdown. Signal handling and the shutdown are the
// platform's; the caller never builds a signal context of its own.
//
// A host whose application component owns a listener composes the same
// pieces itself: Assemble, its own serve step, then Shutdown.
func RunAssembly(ctx context.Context, spec LoadSpec, extra ...pkgcore.Component) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	reg := pkgcore.NewComponentRegistry()
	for _, c := range extra {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	if err := Assemble(ctx, reg, spec); err != nil {
		return err
	}

	<-ctx.Done()
	return Shutdown(context.WithoutCancel(ctx), reg)
}
