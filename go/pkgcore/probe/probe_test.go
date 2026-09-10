package probe

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// TestCheck_OKResponse_Succeeds proves the success half of Check's contract: a
// listener answering the requested path with 200 reports no error, with the
// zero option set (no bound of its own, expecting 200) -- the exact shape a
// running server's liveness endpoint produces.
func TestCheck_OKResponse_Succeeds(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	port := startLoopbackServer(t, mux)

	if err := Check(context.Background(), port, "/healthz"); err != nil {
		t.Fatalf("Check: %v", err)
	}
}

// TestCheck_NonOKResponse_Fails pins the check a transport-error-only probe
// would miss: a request that completes while the endpoint answers a non-200
// status must report an error, classified as ErrUnexpectedStatus.
func TestCheck_NonOKResponse_Fails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	port := startLoopbackServer(t, mux)

	err := Check(context.Background(), port, "/healthz")
	if err == nil {
		t.Fatal("Check: got nil error for a 503 response, want an error")
	}
	if !errors.Is(err, ErrUnexpectedStatus) {
		t.Fatalf("Check error = %v, want it to wrap ErrUnexpectedStatus", err)
	}
}

// TestCheck_NothingListening_Fails proves the other failure shape: nothing
// reachable at the dialed port (the container's own process died, or has not
// finished binding its listener yet) reports an error, never a false
// "healthy". Port 0 needs no allocation dance to stay deterministic.
func TestCheck_NothingListening_Fails(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := Check(ctx, "0", "/healthz"); err == nil {
		t.Fatal("Check: got nil error with nothing listening, want an error")
	}
}

// TestCheck_Timeout_BoundsAWedgedServer pins WithTimeout's bound: a server
// that accepts the connection but never answers must fail the probe within
// the timeout even when ctx carries no deadline of its own.
func TestCheck_Timeout_BoundsAWedgedServer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	})
	port := startLoopbackServer(t, mux)

	start := time.Now()
	err := Check(context.Background(), port, "/healthz", WithTimeout(50*time.Millisecond))
	if err == nil {
		t.Fatal("Check: got nil error from a server that never answers, want a timeout error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Check returned after %v, want it bounded by the 50ms timeout", elapsed)
	}
}

// TestCheck_CancelledContext_Fails proves ctx's bound is honoured: an
// already-cancelled context fails the probe instead of performing it.
func TestCheck_CancelledContext_Fails(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	port := startLoopbackServer(t, mux)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Check(ctx, port, "/healthz")
	if err == nil {
		t.Fatal("Check: got nil error on a cancelled context, want an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Check error = %v, want it to wrap context.Canceled", err)
	}
}

// TestCheck_WithWantStatus_AcceptsDeclaredStatus pins the parameterization of
// the expected status: a non-200 answer is healthy exactly when the caller
// declares it so, and an exact comparison still refuses every other status.
func TestCheck_WithWantStatus_AcceptsDeclaredStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	port := startLoopbackServer(t, mux)

	if err := Check(context.Background(), port, "/healthz", WithWantStatus(http.StatusNoContent)); err != nil {
		t.Fatalf("Check with WithWantStatus(204): %v", err)
	}
	err := Check(context.Background(), port, "/healthz")
	if err == nil || !errors.Is(err, ErrUnexpectedStatus) {
		t.Fatalf("Check with the default 200 against a 204 answer: error = %v, want ErrUnexpectedStatus", err)
	}
}

// TestCheck_DialsLoopbackAndRequestedPath pins the probe's reach
// structurally: the request arrives on 127.0.0.1 -- the package's constant,
// which no parameter can widen -- at exactly the path the caller passed.
func TestCheck_DialsLoopbackAndRequestedPath(t *testing.T) {
	gotHost, gotPath := "", ""
	mux := http.NewServeMux()
	mux.HandleFunc("/deep/health", func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotPath = r.Host, r.URL.Path
		w.WriteHeader(http.StatusOK)
	})
	port := startLoopbackServer(t, mux)

	if err := Check(context.Background(), port, "/deep/health"); err != nil {
		t.Fatalf("Check: %v", err)
	}
	host, _, err := net.SplitHostPort(gotHost)
	if err != nil {
		t.Fatalf("split Host header %q: %v", gotHost, err)
	}
	if host != loopbackHost {
		t.Errorf("probe reached host %q, want the loopback constant %q", host, loopbackHost)
	}
	if gotPath != "/deep/health" {
		t.Errorf("probe requested path %q, want %q", gotPath, "/deep/health")
	}
}

// TestCheck_UnbuildableRequest_Fails pins the request-construction error path:
// a path no URL can carry (a control character) fails before any dial, with an
// error naming the request construction.
func TestCheck_UnbuildableRequest_Fails(t *testing.T) {
	err := Check(context.Background(), "8080", "/he\nalthz")
	if err == nil {
		t.Fatal("Check: got nil error for an unbuildable request URL, want an error")
	}
}

// startLoopbackServer starts an httptest server bound to 127.0.0.1 (the
// address Check dials) and returns the port it bound, as a string ready to
// pass to Check. It registers its own cleanup via t.Cleanup.
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
	return port
}
