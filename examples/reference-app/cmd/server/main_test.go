package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestRunHealthcheck_OKResponse_Succeeds proves the success half of
// runHealthcheck's contract: a listener answering healthzPath with 200
// reports no error. This is the shape the running server itself produces
// (healthzHandler in server.go always answers 200 with no tenant
// required), and it is the shape this example's Dockerfile's HEALTHCHECK
// depends on to report the container healthy.
func TestRunHealthcheck_OKResponse_Succeeds(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(healthzPath, func(w http.ResponseWriter, r *http.Request) {
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
// own healthzPath answers a non-200 status -- exactly the shape a
// half-initialized or degraded server can produce. runHealthcheck must
// treat that as a failure.
func TestRunHealthcheck_NonOKResponse_Fails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(healthzPath, func(w http.ResponseWriter, r *http.Request) {
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
// argument (the shape os.Getenv("PORT") returns when PORT is unset, exactly
// configFromEnv's own default-handling for the running server) falls back
// to defaultPort rather than probing an empty or malformed address.
func TestRunHealthcheck_EmptyPort_UsesDefaultPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:"+defaultPort)
	if err != nil {
		t.Skipf("defaultPort %s is not free on this machine: %v", defaultPort, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc(healthzPath, func(w http.ResponseWriter, r *http.Request) {
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
