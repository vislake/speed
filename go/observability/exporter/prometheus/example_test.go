package prometheus_test

// Runnable documentation for this subpackage's one public contract: the
// side effect its init() function has on go/observability's own Init and
// MetricsHandler. Compiled AND executed by `go test`, matching
// go/observability's own example_test.go convention.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"

	obs "github.com/vislake/speed/go/observability"
	_ "github.com/vislake/speed/go/observability/exporter/prometheus"
)

// Example shows the whole of what blank-importing this package buys a
// host: obs.Init's no-endpoint (local exporters) path starts serving real
// Prometheus output from obs.MetricsHandler instead of the
// not-configured 404 go/observability's own root-package tests pin as the
// default without this import.
func Example() {
	// obs.Init's local exporters bind os.Stdout as their writer AT
	// CONSTRUCTION TIME (stdouttrace.WithWriter(os.Stdout) inside
	// go/observability's own initLocalExporters), so they keep writing to
	// whatever os.Stdout pointed to at that moment for their whole
	// lifetime -- including the periodic dump and the flush Init's
	// returned shutdown function performs, neither of which is relevant
	// to what this example demonstrates, and both of which would
	// otherwise make its captured output nondeterministic. Pointing
	// os.Stdout at the null device only around the Init call, then
	// restoring it immediately, is what keeps every later
	// fmt.Println in this function landing in the real captured output
	// while every stdout-exporter write for the rest of this example's
	// life is silently discarded.
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		fmt.Println("open devnull:", err)
		return
	}
	realStdout := os.Stdout
	os.Stdout = devNull
	shutdown, err := obs.Init(context.Background())
	os.Stdout = realStdout
	if err != nil {
		fmt.Println("init:", err)
		_ = devNull.Close()
		return
	}

	rr := httptest.NewRecorder()
	obs.MetricsHandler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	fmt.Println("metrics status:", rr.Code)

	_ = shutdown(context.Background())
	_ = devNull.Close()

	// Output:
	// metrics status: 200
}
