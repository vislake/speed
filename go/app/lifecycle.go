package app

import (
	"context"
	"errors"
	"fmt"
)

// Close drains the application in one fixed order and releases every resource
// the assembly built. It is idempotent: the first call runs the shutdown, and
// every later call returns the same result without repeating a step. New's
// stage rollback is this same shutdown, so a failed assembly and a graceful
// exit tear down identically.
//
// The order is:
//
//	① the HTTP server stops accepting requests (ShutdownTimeout)
//	② the worker stops and its queue drains (ShutdownTimeout)
//	③ the kernel shuts down -- the closers of every preset-resolved seam --
//	   and then the database closes, the reverse of their construction
//	④ observability shuts down last, flushing buffered spans and metrics
//
// Each step is attempted even when an earlier one failed; the joined result is
// returned and cached. ctx must be a live context -- a cancelled one aborts
// every step that honors it -- and the caller's own bound on the whole
// shutdown, if any, sits on top of the per-step ShutdownTimeout.
func (a *Application) Close(ctx context.Context) error {
	a.closeOnce.Do(func() {
		var errs []error

		// ① The HTTP server: stop accepting and drain in-flight requests.
		if a.server != nil {
			shutdownCtx, cancel := context.WithTimeout(ctx, ShutdownTimeout)
			if err := a.server.Shutdown(shutdownCtx); err != nil {
				errs = append(errs, fmt.Errorf("app: shut the HTTP server down: %w", err))
			}
			cancel()
		}

		// ② The worker: stop it and let it drain, bounded like the HTTP
		// drain. A worker the engine never started is closed too -- Close is
		// safe before Start by the worker contract.
		if a.worker != nil {
			closeCtx, cancel := context.WithTimeout(ctx, ShutdownTimeout)
			if err := a.worker.Close(closeCtx); err != nil {
				errs = append(errs, fmt.Errorf("app: close the background worker: %w", err))
			}
			cancel()
		}

		// ③ Kernel, then infrastructure: the reverse of their construction
		// order.
		if a.kernel != nil {
			if err := a.kernel.Shutdown(); err != nil {
				errs = append(errs, fmt.Errorf("app: shut the kernel down: %w", err))
			}
		}
		if a.db != nil {
			sqlDB, err := a.db.DB()
			if err != nil {
				errs = append(errs, fmt.Errorf("app: reach the database handle for closing: %w", err))
			} else if sqlDB != nil {
				if err := sqlDB.Close(); err != nil {
					errs = append(errs, fmt.Errorf("app: close the database: %w", err))
				}
			}
		}

		// ④ Observability last, so everything the steps above emitted is
		// still exported.
		if a.obsShutdown != nil {
			if err := a.obsShutdown(ctx); err != nil {
				errs = append(errs, fmt.Errorf("app: shut observability down: %w", err))
			}
		}

		a.closeErr = errors.Join(errs...)
	})
	return a.closeErr
}
