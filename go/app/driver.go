package app

import (
	"context"
	"errors"
	"os/signal"
	"syscall"

	"github.com/vislake/speed/go/pkgcore"
)

// driver.go carries the engine's stage driver -- the piece of the engine
// that only orchestrates. Assemble walks a populated ComponentRegistry
// through the assembly's first six stages; Shutdown performs the two-phase
// close; RunAssembly is the sugar that creates the registry, runs the
// loader, calls the host's serve step (the ServeFunc) and drives both around
// a context's lifetime.
//
// The driver contains no HTTP assembly and no listening: the http
// component (go/app/httpserve) composes the routes the declaration faces
// accumulated and owns the listener, exactly as any component owns the
// resources it starts.
// The engine only calls the host's serve step and surrounds it with the
// signal overlay and the two-beat close.

// Assemble drives one assembly over reg: the loader runs first (the
// configuration load of the Prepare stage's first beat), then the registry
// is walked through Prepare, Construct, Verify, Init, Start and Serve in
// order. The drive ends at Serve -- the entry points accepting external
// requests are up, so a caller's serve step runs against the assembled
// whole -- where the returned registry still holds the live assembly.
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
	if err := reg.Start(ctx); err != nil {
		return err
	}
	return reg.Serve(ctx)
}

// Shutdown performs the two-phase close every speed application shares: the
// non-blocking Stop notification goes out in reverse dependency order, in
// its two beats -- the components that declared Serve (the entry points)
// first, then every other component -- with failures ignored (a
// notification that cannot be delivered must not keep the Close that waits
// for the drain from running), then the Close stage waits out the drain and
// releases every constructed component's resources, in reverse dependency
// order, aggregating failures. Both phases run exactly once, whatever the
// registry's state: a registry whose assembly never constructed anything
// closes as a no-op.
//
// ctx bounded the whole shutdown -- the caller's own bound, if any, sits on
// top of the per-step timeout each component applies internally.
func Shutdown(ctx context.Context, reg *pkgcore.ComponentRegistry) error {
	_ = reg.Stop(ctx)
	return reg.Close(ctx)
}

// ServeFunc is a host's serve step: the callback RunAssembly invokes once the
// assembly is up, handing it the lifecycle context and the live registry. It
// runs after the assembly's Serve round -- the registry stage in which the
// components that accept external requests are started -- so the host's own
// wait begins against the assembled whole. It is where a process that serves
// states its own serving lifetime -- an HTTP face of its own, a worker loop,
// anything the host holds open -- while the engine keeps owning the signal
// overlay and the two-beat close around it.
//
// The callback returns when serving should end; the engine then runs the
// close. ctx carries the engine's SIGINT/SIGTERM overlay, so the ordinary
// serve step waits it out; reg is the assembled registry, read-only by
// convention (the declaration seats and the plan are settled by this point).
type ServeFunc func(ctx context.Context, reg *pkgcore.ComponentRegistry) error

// RunAssembly is the engine's Run sugar for a process: it creates a
// ComponentRegistry from the global component registration plus extra, runs
// the loader over spec, drives the assembly through Serve, runs the host's
// serve step (the serve callback; nil waits the lifecycle context out), and
// then shuts the assembly down in the two phases of Shutdown (the
// non-blocking Stop notification, then the blocking Close). Signal handling
// and the shutdown are the platform's: SIGINT and SIGTERM are overlaid on
// ctx, so a caller passes its base context and never builds a signal context
// of its own.
//
// The serve callback is what a host whose process serves composes with: the
// callback is called in the serve beat between the registry's Serve round
// and the close, handed the signal-derived context and the live registry,
// and its return ends the serve phase. A serve failure does not skip the
// close -- the two-beat shutdown runs regardless and the serve error joins
// its result, so a teardown is never silently dropped and a serve failure
// still reaches the caller's exit path. A nil callback is the no-serve-step
// shape: the engine waits for ctx itself, exactly as it did before the
// callback existed.
//
// A host whose application component owns a listener passes the component
// set as extra and a serve step that waits the lifecycle out (the listener
// itself is the component's); a host that drives the registry itself
// composes Assemble, its serve, then Shutdown instead.
func RunAssembly(ctx context.Context, spec LoadSpec, serve ServeFunc, extra ...pkgcore.Component) error {
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

	if serve != nil {
		if err := serve(ctx, reg); err != nil {
			return errors.Join(err, Shutdown(context.WithoutCancel(ctx), reg))
		}
	} else {
		<-ctx.Done()
	}
	return Shutdown(context.WithoutCancel(ctx), reg)
}
