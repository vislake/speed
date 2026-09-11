package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/examples/reference-app/internal/testutil"

	obs "github.com/vislake/speed/go/observability"
)

// TestRunHealthcheck_OKResponse_Succeeds proves the success half of
// runHealthcheck's contract: a listener answering HealthzPath with 200
// reports no error. This is the shape the running server itself produces
// (obs.HealthzHandler, which obs.MountLiveness mounts here, always answers
// 200 with no tenant required), and it is the shape this example's Dockerfile's HEALTHCHECK
// depends on to report the container healthy.
func TestRunHealthcheck_OKResponse_Succeeds(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(obs.HealthzPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	port := startLoopbackServer(t, mux)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runHealthcheck(ctx, port); err != nil {
		t.Fatalf("runHealthcheck: %v", err)
	}
}

// TestRunHealthcheck_NonOKResponse_Fails is the bug this test guards
// against: a naive healthcheck that only checks "did the request succeed"
// (a nil transport error) would report a container healthy even while its
// own HealthzPath answers a non-200 status -- exactly the shape a
// half-initialized or degraded server can produce. runHealthcheck must
// treat that as a failure.
func TestRunHealthcheck_NonOKResponse_Fails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(obs.HealthzPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	port := startLoopbackServer(t, mux)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runHealthcheck(ctx, port); err == nil {
		t.Fatal("runHealthcheck: got nil error for a 503 response, want an error")
	}
}

// TestRunHealthcheck_NothingListening_Fails proves the other failure shape:
// no server reachable at all (the container's own process died, or has not
// finished binding its listener yet) must report an error too, never a
// false "healthy".
func TestRunHealthcheck_NothingListening_Fails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	// Port 0 never has a real listener bound to it by the time this dials,
	// and unlike an ephemeral free port it needs no allocation dance to stay
	// deterministic.
	if err := runHealthcheck(ctx, "0"); err == nil {
		t.Fatal("runHealthcheck: got nil error with nothing listening, want an error")
	}
}

// TestRunHealthcheck_EmptyPort_UsesDefaultPort proves the empty-port
// fallback runHealthcheck's own doc comment describes: an empty port
// argument (the shape an explicitly emptied PORT variable resolves to,
// exactly ConfigFromEnv's own default-handling for the running server)
// falls back to DefaultPort rather than probing an empty or malformed
// address.
func TestRunHealthcheck_EmptyPort_UsesDefaultPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:"+app.DefaultPort)
	if err != nil {
		t.Skipf("DefaultPort %s is not free on this machine: %v", app.DefaultPort, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(obs.HealthzPath, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := &httptest.Server{Listener: listener, Config: &http.Server{Handler: mux}}
	srv.Start()
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := runHealthcheck(ctx, ""); err != nil {
		t.Fatalf("runHealthcheck(ctx, \"\"): %v", err)
	}
}

// startLoopbackServer starts an httptest server bound to 127.0.0.1 (matching
// the loopback address runHealthcheck itself dials) and returns the port it
// bound, as a string ready to pass to runHealthcheck. It registers its own
// cleanup via t.Cleanup, so callers need no defer of their own.
func startLoopbackServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse httptest server URL %q: %v", srv.URL, err)
	}
	port := u.Port()
	if port == "" {
		t.Fatalf("httptest server URL %q carries no port", srv.URL)
	}
	// runHealthcheck always dials 127.0.0.1 explicitly (see its own doc
	// comment); httptest.NewServer already binds 127.0.0.1, so this is a
	// sanity check that assumption still holds rather than a behavior this
	// test relies on beyond that.
	if !strings.Contains(srv.URL, "127.0.0.1") {
		t.Fatalf("httptest server URL %q is not on 127.0.0.1", srv.URL)
	}
	return port
}

// TestRun_BootsServesHealthzAndShutsDownCleanly drives main.go's ordinary
// boot path end to end, the one behavior this file's other tests reach
// only in parts: ConfigFromEnv resolves the zero-environment standalone
// defaults, run boots the whole composed server on the chosen port, the
// server answers its own HealthzPath -- the same probe this example's
// Dockerfile HEALTHCHECK runs -- and a cancelled base context takes it
// down through the graceful-shutdown path, run returning nil. Every
// configuration variable the bootstrap reads is explicitly cleared (the
// same discipline TestConfigFromEnv_Defaults documents), so the boot's
// outcome never depends on the ambient environment; PORT and APP_DB_PATH
// are then pinned to a free port and a fresh per-test database file, so
// the boot cannot collide with a real developer database or a parallel
// listener.
func TestRun_BootsServesHealthzAndShutsDownCleanly(t *testing.T) {
	testutil.ClearBootstrapEnv(t)

	port := freeTCPPort(t)
	t.Setenv("PORT", port)
	t.Setenv("APP_DB_PATH", filepath.Join(t.TempDir(), "run-test.sqlite"))

	// The boot's own logger, discarded: this test asserts process-lifecycle
	// behavior, not log output (main attaches a JSON logger to baseCtx in
	// production; an io.Discard sink is the equivalent context shape here).
	baseCtx := obs.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()

	// The server is healthy when its own healthz answers 200 -- polled with
	// the very probe the Dockerfile HEALTHCHECK uses.
	deadline := time.Now().Add(120 * time.Second)
	healthy := false
	for time.Now().Before(deadline) {
		if err := runHealthcheck(context.Background(), port); err == nil {
			healthy = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !healthy {
		t.Fatal("run() booted no server answering healthz within 120s")
	}

	// A cancelled base context is the shutdown signal: run must return nil
	// through the graceful-shutdown path, not hang and not error.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() after cancel error = %v, want nil (a clean shutdown)", err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("run() did not return within 120s of the cancel")
	}
}

// TestRun_PortAlreadyTaken_ReturnsTheServeError drives run's serve-failure
// exit: with the configured port already bound by another listener,
// ListenAndServe answers immediately with the bind refusal, the serve
// channel carries it back, and run returns the serve error -- the
// process-lifecycle failure shape an operator sees when two instances
// race for one port, distinct from a clean shutdown.
func TestRun_PortAlreadyTaken_ReturnsTheServeError(t *testing.T) {
	testutil.ClearBootstrapEnv(t)

	// Hold the port on the wildcard address -- the exact address the boot's
	// own ":port" listener binds -- so the second bind is genuinely
	// refused. (A loopback-only hold would not conflict: on macOS a
	// wildcard bind coexists with a loopback-specific one on the same port
	// number, which is why the hold must be wildcard-shaped.)
	held, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer func() { _ = held.Close() }()
	port := strconv.Itoa(held.Addr().(*net.TCPAddr).Port)
	t.Setenv("PORT", port)
	t.Setenv("APP_DB_PATH", filepath.Join(t.TempDir(), "run-port-taken.sqlite"))

	baseCtx := obs.WithLogger(context.Background(), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run() with the port already taken returned nil, want the serve error")
		}
		if !strings.Contains(err.Error(), "serve") {
			t.Fatalf("run() error = %q, want the serve error naming the bind refusal", err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("run() with the port already taken did not return within 120s")
	}
}

// freeTCPPort returns a currently-free TCP port number, for tests that
// must hand a concrete PORT to code binding its own listener. The
// reservation lasts only until the listener is closed -- a small
// reallocation race, acceptable where the alternative (no other way to
// name a port a boot binds itself) is worse.
func freeTCPPort(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate a free port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release the probe listener: %v", err)
	}
	return strconv.Itoa(port)
}
