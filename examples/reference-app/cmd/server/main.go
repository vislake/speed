package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

// main is deliberately thin process-lifecycle glue (signal handling,
// http.Server start/stop) with no independently testable pure logic of its
// own -- the testable seam is buildServer (server.go), which
// server_test.go covers directly, and this file's own end-to-end behavior
// is additionally proven by literally running it and curling it (see this
// example's README.md). It has no main_test.go for that reason, matching
// ordinary Go practice of not unit-testing os.Exit/signal-handling glue.
func main() {
	// Every slog.Default() call in this file (here and throughout run
	// below) stands in for backend coding standard §11's rule of always
	// taking the logger from the context (obs.FromContext(ctx)): the
	// observability module is still a stub at this stage (root CLAUDE.md's
	// M0 status; go/observability/AGENTS.md literally says "Not yet
	// implemented"), so there is no obs.FromContext to call yet. Message
	// and key-value shape otherwise already follow §11; switch every call
	// site in this file to obs.FromContext(ctx) once that module lands.
	if err := run(); err != nil {
		slog.Default().Error("reference-app server exited with error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := configFromEnv()
	if err != nil {
		return fmt.Errorf("reference-app: load configuration: %w", err)
	}

	handler, cleanup, err := buildServer(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		if err := cleanup(); err != nil {
			// slog.Default() M0 stopgap -- see main's comment above.
			slog.Default().Error("reference-app: cleanup failed", "error", err)
		}
	}()

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	serveErr := make(chan error, 1)
	go func() {
		// slog.Default() M0 stopgap -- see main's comment above.
		slog.Default().Info("reference-app server listening", "addr", srv.Addr, "profile", string(cfg.Profile))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		// slog.Default() M0 stopgap -- see main's comment above.
		slog.Default().Info("reference-app: shutdown signal received")
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("reference-app: serve: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("reference-app: graceful shutdown: %w", err)
	}
	// slog.Default() M0 stopgap -- see main's comment above.
	slog.Default().Info("reference-app: server stopped cleanly")
	return nil
}
