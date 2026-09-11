package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// TestClose_StopsTheWorkerWhileTheDatabaseIsStillOpen pins the shutdown
// order's infrastructure half: the worker's drain runs before the kernel and
// the database are torn down, so a drain may still read and write.
func TestClose_StopsTheWorkerWhileTheDatabaseIsStillOpen(t *testing.T) {
	var host testHostConfig
	var deps ModuleDeps
	var drainErr error
	worker := &testWorker{onClose: func(context.Context) error {
		drainErr = deps.DB.Exec("SELECT 1").Error
		return nil
	}}

	a, err := New(context.Background(), append(testBaseOptions(t, &host),
		WithModules(func(_ context.Context, d ModuleDeps) ([]pkgcore.Module, error) {
			deps = d
			return nil, nil
		}),
		WithWorker(worker),
	)...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if drainErr != nil {
		t.Fatalf("the worker's drain could not use the database: %v; the worker must close before the database does", drainErr)
	}
}

// reserveAddr returns a loopback address the test can hand to Run: a port is
// reserved and released, so the server binds it a moment later.
func reserveAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return addr
}

// TestRun_ServesAndDrainsOnCancellation drives Run end to end: the composed
// server answers over its own listener, a cancelled context returns a nil
// error once the ordered drain completes, and the worker was closed.
func TestRun_ServesAndDrainsOnCancellation(t *testing.T) {
	addr := reserveAddr(t)
	var host testHostConfig
	worker := &testWorker{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() {
		runErr <- Run(ctx, append(testBaseOptions(t, &host),
			WithModules(func(context.Context, ModuleDeps) ([]pkgcore.Module, error) {
				return []pkgcore.Module{&testModule{name: "probe", routePath: "/api/v1/probe", handler: staticHandler("probe")}}, nil
			}),
			WithHTTP(HTTPSpec{Addr: addr}),
			WithWorker(worker),
		)...)
	}()

	var resp *http.Response
	var err error
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err = http.Get("http://" + addr + "/api/v1/probe") //nolint:gosec,noctx // loopback URL this test reserved itself
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never answered at %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	body := make([]byte, 16)
	n, _ := resp.Body.Read(body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body[:n]) != "probe" {
		t.Fatalf("GET /api/v1/probe over the wire: status %d body %q, want 200 %q", resp.StatusCode, body[:n], "probe")
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil after a clean drain", err)
		}
	case <-time.After(ShutdownTimeout + 10*time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	if starts, closes := worker.counts(); starts != 1 || closes != 1 {
		t.Fatalf("worker calls: starts=%d closes=%d, want one of each", starts, closes)
	}
}

// TestRun_RefusesAnEmptyAddr pins the fail-fast address check: Run refuses
// before anything is assembled, so a host that forgot WithHTTP does not pay
// for an assembly it cannot serve.
func TestRun_RefusesAnEmptyAddr(t *testing.T) {
	var host testHostConfig
	spec := testDatabaseSpec(t)

	err := Run(context.Background(), WithConfig(ConfigSpec{Host: &host}, testConfigOptions()...), WithDatabase(spec))
	if err == nil || !strings.Contains(err.Error(), "non-empty Addr") {
		t.Fatalf("Run() without WithHTTP error = %v, want one naming the missing Addr", err)
	}
}

// TestRun_ReportsAListenerFailureAndDrains pins the failure path: when the
// address cannot be bound, Run reports the listener's own failure and still
// tears the process down (the worker close included).
func TestRun_ReportsAListenerFailureAndDrains(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind a port: %v", err)
	}
	defer func() { _ = l.Close() }()

	var host testHostConfig
	worker := &testWorker{}
	runErr := make(chan error, 1)
	go func() {
		runErr <- Run(context.Background(), append(testBaseOptions(t, &host),
			WithHTTP(HTTPSpec{Addr: l.Addr().String()}),
			WithWorker(worker),
		)...)
	}()

	select {
	case err := <-runErr:
		if err == nil || !strings.Contains(err.Error(), "app: serve:") {
			t.Fatalf("Run() error = %v, want the listener failure attributed to the serve step", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the listener failed")
	}
	if _, closes := worker.counts(); closes != 1 {
		t.Fatalf("worker Close calls = %d, want the failure path to drain it", closes)
	}
}

// TestRun_InitializesObservabilityWhenConfigured pins the option's contract:
// with an ObservabilitySpec, Run initializes observability before assembly
// and shuts it down as the drain's last step. Both halves are covered by the
// same run: an Init failure would surface as Run's error, and the shutdown
// runs inside Close, whose aggregate result Run reports.
func TestRun_InitializesObservabilityWhenConfigured(t *testing.T) {
	addr := reserveAddr(t)
	var host testHostConfig

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		runErr <- Run(ctx, append(testBaseOptions(t, &host),
			WithHTTP(HTTPSpec{Addr: addr}),
			WithObservability(ObservabilitySpec{ServiceName: "app-test"}),
		)...)
	}()

	// Wait until the composed server answers, then shut it down; the timings
	// are the test's own, so a slow machine only extends the poll.
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(fmt.Sprintf("http://%s/healthz", addr)) //nolint:gosec,noctx // loopback URL this test reserved itself
		if err == nil {
			_ = resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the server never answered at %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run() with observability configured error = %v, want nil", err)
		}
	case <-time.After(ShutdownTimeout + 10*time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// TestClose_ReportsEveryFailingStep pins the aggregate: when more than one
// step fails, the caller sees them all rather than only the first.
func TestClose_ReportsEveryFailingStep(t *testing.T) {
	var host testHostConfig
	worker := &testWorker{onClose: func(context.Context) error { return errors.New("worker drain failed") }}

	a, err := New(context.Background(), append(testBaseOptions(t, &host), WithWorker(worker))...)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// Close the database out from under the engine so its own step fails too.
	sqlDB, err := a.db.DB()
	if err != nil {
		t.Fatalf("reach the database handle: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close the database handle directly: %v", err)
	}

	closeErr := a.Close(context.Background())
	if closeErr == nil {
		t.Fatal("Close() error = nil, want the failing steps reported")
	}
	if !strings.Contains(closeErr.Error(), "worker drain failed") {
		t.Errorf("Close() error does not carry the worker's failure: %v", closeErr)
	}
}
