package probe_test

// Runnable documentation for the probe public API. Every example here is
// compiled and executed by `go test`, so an API change that invalidates the
// documented usage fails the build instead of silently rotting.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	"github.com/vislake/speed/go/pkgcore/probe"
)

// ExampleCheck shows a container self-probe's two outcomes against the
// process's own server: healthy when the liveness endpoint answers 200, and
// an error classified through ErrUnexpectedStatus when it answers anything
// else -- the degraded endpoint a transport-error-only check would miss.
func ExampleCheck() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port)

	if err := probe.Check(context.Background(), port, "/healthz"); err != nil {
		fmt.Println("unhealthy:", err)
	} else {
		fmt.Println("healthy")
	}

	err := probe.Check(context.Background(), port, "/not-the-endpoint")
	fmt.Println("unhealthy:", errors.Is(err, probe.ErrUnexpectedStatus))

	// Output:
	// healthy
	// unhealthy: true
}

// ExampleCheck_options shows the two parameters a container HEALTHCHECK
// typically pins: the bound the probe gives itself, so a wedged server is
// reported unhealthy rather than waited on forever, and the status its
// liveness endpoint is declared to answer (204 here, not the 200 default).
func ExampleCheck_options() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	port := strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port)

	err := probe.Check(context.Background(), port, "/healthz",
		probe.WithTimeout(3*time.Second),
		probe.WithWantStatus(http.StatusNoContent),
	)
	fmt.Println("healthy:", err == nil)

	// Output:
	// healthy: true
}
