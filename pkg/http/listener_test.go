package http

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	nethttp "net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startTestEndpoint binds a listener on a loopback port and returns it with the
// address it really got, tearing it down whatever the test does.
func startTestEndpoint(t *testing.T, s endpointSettings, h nethttp.Handler, logger *slog.Logger) (*listener, string) {
	t.Helper()
	l := &listener{}
	if err := l.start(s, h, logger); err != nil {
		t.Fatalf("binding endpoint %q: %v", s.name, err)
	}
	t.Cleanup(func() { _ = l.close(context.Background()) })
	addr := l.addr()
	if addr == nil {
		t.Fatal("the endpoint bound without an address to connect to")
	}
	return l, addr.String()
}

// get issues one request and reports what came back, for the tests that need a
// request in flight rather than a response.
func get(url string) error {
	resp, err := nethttp.Get(url) //nolint:noctx // the deadline is the test's own
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

// TestBindFailureIsStartupFailure pins the synchronous half of Serve: an
// address that cannot be bound comes back from the callback, so the startup
// ends there. Bound on a goroutine instead, the process would come up with an
// entry point nobody can reach.
func TestBindFailureIsStartupFailure(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("taking a port to collide with: %v", err)
	}
	defer taken.Close()

	s := testSettings("public")
	s.address = taken.Addr().String()
	logger, _ := newRecordingLogger()

	var l listener
	err = l.start(s, nethttp.NotFoundHandler(), logger)
	if !errors.Is(err, ErrListen) {
		t.Fatalf("binding a taken address did not report ErrListen: %v", err)
	}
	mustContain(t, err.Error(), s.address, "the address that could not be bound")
	mustContain(t, err.Error(), `"public"`, "the endpoint that could not bind")
	if l.isAccepting() {
		t.Error("an endpoint that failed to bind reports that it is accepting requests")
	}
}

// TestBindFailureKeepsTheUnderlyingNetError pins the wrapping: %w all the way
// down, so a caller can still reach syscall.EADDRINUSE through the sentinel.
func TestBindFailureKeepsTheUnderlyingNetError(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("taking a port to collide with: %v", err)
	}
	defer taken.Close()

	s := testSettings("public")
	s.address = taken.Addr().String()
	logger, _ := newRecordingLogger()

	var l listener
	err = l.start(s, nethttp.NotFoundHandler(), logger)
	var opErr *net.OpError
	if !errors.As(err, &opErr) {
		t.Fatalf("the net error chain did not survive the wrapping: %v", err)
	}
	if opErr.Op != "listen" {
		t.Errorf("the underlying error is about %q, not about listening", opErr.Op)
	}
}

// TestTLSCertFailureIsSynchronous pins where a certificate is loaded. Left to
// the server's own ServeTLS, it would be loaded after the Serve callback has
// returned, and a mistyped path would become a goroutine failing where nobody
// is looking while the startup reports success.
func TestTLSCertFailureIsSynchronous(t *testing.T) {
	dir := t.TempDir()
	s := testSettings("public")
	s.tlsCertFile = filepath.Join(dir, "absent.pem")
	s.tlsKeyFile = filepath.Join(dir, "absent-key.pem")
	logger, _ := newRecordingLogger()

	var l listener
	err := l.start(s, nethttp.NotFoundHandler(), logger)
	if !errors.Is(err, ErrListen) {
		t.Fatalf("a certificate that cannot be loaded did not fail the callback: %v", err)
	}
	mustContain(t, err.Error(), s.tlsCertFile, "the certificate path that could not be read")
	if l.isAccepting() {
		t.Error("an endpoint whose certificate failed to load reports that it is accepting requests")
	}
}

// TestCertificateFailureIsAListenFailure pins how the failure is classified.
// The design folds a certificate that cannot be loaded into ErrListen instead of
// giving it a sentinel, on the criterion that the host's remedy is the same one
// a taken address calls for: change the configuration and start again. A caller
// that acts on "this endpoint did not come up" therefore needs one class, and
// the two paths in the text are what says which half to fix.
func TestCertificateFailureIsAListenFailure(t *testing.T) {
	dir := t.TempDir()
	s := testSettings("public")
	s.tlsCertFile = filepath.Join(dir, "absent.pem")
	s.tlsKeyFile = filepath.Join(dir, "absent-key.pem")
	logger, _ := newRecordingLogger()

	var l listener
	err := l.start(s, nethttp.NotFoundHandler(), logger)
	if !errors.Is(err, ErrListen) {
		t.Fatalf("a certificate that cannot be loaded was not reported as ErrListen: %v", err)
	}
	mustContain(t, err.Error(), s.tlsCertFile, "the certificate path")
	mustContain(t, err.Error(), s.tlsKeyFile, "the key path")
	mustContain(t, err.Error(), `"public"`, "the endpoint that did not come up")

	// The other half of "no sentinel of its own": a sentinel added for this
	// failure would make one of these match, and the class a caller switches
	// on would have grown without anybody noticing.
	others := []struct {
		name     string
		sentinel error
	}{
		{"ErrUnknownEndpoint", ErrUnknownEndpoint},
		{"ErrRouteConflict", ErrRouteConflict},
		{"ErrMiddlewareCycle", ErrMiddlewareCycle},
		{"ErrInvalidSpec", ErrInvalidSpec},
		{"ErrDrainTimeout", ErrDrainTimeout},
		{"ErrMalformedBody", ErrMalformedBody},
		{"ErrBodyTooLarge", ErrBodyTooLarge},
		{"ErrValidation", ErrValidation},
	}
	for _, other := range others {
		if errors.Is(err, other.sentinel) {
			t.Errorf("a certificate failure also matches %s; it is a bind failure and "+
				"nothing else", other.name)
		}
	}
}

// TestStopClosesListenerSynchronously is the first beat: when Stop returns, the
// endpoint has already stopped accepting and a new connection is refused, while
// a request that was already in flight is still being served.
func TestStopClosesListenerSynchronously(t *testing.T) {
	gate := newGateHandler(t)
	logger, _ := newRecordingLogger()
	l, addr := startTestEndpoint(t, testSettings("public"), gate, logger)

	inFlight := make(chan error, 1)
	go func() { inFlight <- get("http://" + addr + "/") }()
	waitFor(t, gate.entered, "the request to reach the handler")

	// Stop is called with a deadline of its own: an implementation that
	// drained inside Stop would otherwise park here until the whole binary
	// times out, and this assertion is exactly the one that tells the two
	// shapes apart.
	stopped := make(chan error, 1)
	go func() { stopped <- l.stop(context.Background()) }()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("Stop did not return while a request was in flight; draining belongs to Close")
	}

	if l.isAccepting() {
		t.Error("the endpoint still reports that it is accepting requests after Stop returned")
	}
	conn, err := net.DialTimeout("tcp", addr, waitDeadline)
	if err == nil {
		conn.Close()
		t.Error("a new connection was accepted after Stop returned, so the socket outlived the promise")
	}
	select {
	case <-gate.finished:
		t.Error("Stop cut off a request that was in flight; draining belongs to Close")
	default:
	}

	gate.letGo()
	if err := <-inFlight; err != nil {
		t.Errorf("the in-flight request did not finish cleanly: %v", err)
	}
}

// TestCloseWaitsForDrain is the second beat, and the two tests only tell the
// beats apart together: an implementation that drains inside Stop passes each
// of the "shuts down cleanly" assertions on its own.
func TestCloseWaitsForDrain(t *testing.T) {
	gate := newGateHandler(t)
	logger, _ := newRecordingLogger()
	l, addr := startTestEndpoint(t, testSettings("public"), gate, logger)

	inFlight := make(chan error, 1)
	go func() { inFlight <- get("http://" + addr + "/") }()
	waitFor(t, gate.entered, "the request to reach the handler")

	if err := l.stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	closed := make(chan error, 1)
	go func() { closed <- l.close(context.Background()) }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned while a request was still in flight: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	gate.letGo()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close reported a failure on a clean drain: %v", err)
		}
	case <-time.After(waitDeadline):
		t.Fatal("Close did not return after the in-flight request finished")
	}
	if err := <-inFlight; err != nil {
		t.Errorf("the in-flight request did not finish cleanly: %v", err)
	}
}

// TestDrainTimeoutCutsInflight pins the end of the drain budget: the failure is
// reported, and the connections it was waiting for are really cut. Shutdown on
// its own only stops waiting, so without the Close that follows it the caller
// would be told the requests were cut while they carried on running.
func TestDrainTimeoutCutsInflight(t *testing.T) {
	gate := newGateHandler(t)
	logger, _ := newRecordingLogger()
	s := testSettings("public")
	s.drainTimeout = 100 * time.Millisecond
	l, addr := startTestEndpoint(t, s, gate, logger)

	inFlight := make(chan error, 1)
	go func() { inFlight <- get("http://" + addr + "/") }()
	waitFor(t, gate.entered, "the request to reach the handler")

	if err := l.stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	err := l.close(context.Background())
	if !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("a drain that ran out of time did not report ErrDrainTimeout: %v", err)
	}
	mustContain(t, err.Error(), `"public"`, "the endpoint that did not drain")
	mustContain(t, err.Error(), s.drainTimeout.String(), "the timeout that expired")

	select {
	case clientErr := <-inFlight:
		if clientErr == nil {
			t.Error("the client got a complete response, so the connection was not cut")
		}
	case <-time.After(waitDeadline):
		t.Fatal("the client was left hanging, so the connection was not cut")
	}
}

// failingListener is a socket whose Accept fails for good. It is the only way
// to reach the accept loop's anomalous exit from outside: every failure this
// module causes itself is classified as a normal shutdown.
type failingListener struct {
	err    error
	closed chan struct{}
}

func (f *failingListener) Accept() (net.Conn, error) { return nil, f.err }
func (f *failingListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func (f *failingListener) Close() error {
	select {
	case <-f.closed:
	default:
		close(f.closed)
	}
	return nil
}

// TestAcceptLoopAnomalyFlipsAccepting pins the run-time failure the design
// accepts: the process stays up, the endpoint stops claiming it accepts
// requests, and the event is written down. An implementation that leaves
// Accepting true, or that says nothing, turns "visible to whoever reads it"
// into not visible at all, which is the whole of what makes the accepted cost
// bearable.
func TestAcceptLoopAnomalyFlipsAccepting(t *testing.T) {
	logger, records := newRecordingLogger()
	broken := &failingListener{err: errors.New("the socket is beyond repair"), closed: make(chan struct{})}
	l := &listener{listen: func(string, string) (net.Listener, error) { return broken, nil }}

	if err := l.start(testSettings("public"), nethttp.NotFoundHandler(), logger); err != nil {
		t.Fatalf("the bind itself failed, so the accept loop never ran: %v", err)
	}
	t.Cleanup(func() { _ = l.close(context.Background()) })

	waitUntil(t, "the endpoint to stop reporting that it accepts requests", func() bool {
		return !l.isAccepting()
	})
	waitUntil(t, "the anomalous exit to be written down", func() bool {
		return len(records.at(slog.LevelError)) > 0
	})

	written := strings.Join(records.at(slog.LevelError), "\n")
	mustContain(t, written, "public", "the endpoint whose accept loop exited")
	mustContain(t, written, "beyond repair", "what the accept loop came back with")
}

// TestNormalStopIsNotAnAnomalousExit is the other half of the same
// classification: a shutdown this module caused says nothing, or every clean
// stop would report an anomaly and the report would mean nothing.
func TestNormalStopIsNotAnAnomalousExit(t *testing.T) {
	logger, records := newRecordingLogger()
	l, _ := startTestEndpoint(t, testSettings("public"), nethttp.NotFoundHandler(), logger)

	if err := l.stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := l.close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The accept loop returns on its own goroutine, so the absence has to be
	// given time to become a presence before it is asserted.
	time.Sleep(50 * time.Millisecond)

	if written := records.at(slog.LevelError); len(written) > 0 {
		t.Errorf("a clean stop was reported as an anomalous exit: %s", strings.Join(written, "\n"))
	}
}

// TestStopToleratesNeverBound pins the rollback path: a startup that fails
// part-way stops and closes every constructed instance, including the endpoints
// that never got as far as binding.
func TestStopToleratesNeverBound(t *testing.T) {
	var l listener
	if err := l.stop(context.Background()); err != nil {
		t.Errorf("Stop on an endpoint that never bound: %v", err)
	}
	if err := l.close(context.Background()); err != nil {
		t.Errorf("Close on an endpoint that never bound: %v", err)
	}
	if l.isAccepting() {
		t.Error("an endpoint that never bound reports that it is accepting requests")
	}
}

// TestStopIsIdempotent pins the repeated call. A host may stop twice, and the
// second call must not start a second drain or report a socket that is already
// closed as a failure.
func TestStopIsIdempotent(t *testing.T) {
	logger, _ := newRecordingLogger()
	l, _ := startTestEndpoint(t, testSettings("public"), nethttp.NotFoundHandler(), logger)

	if err := l.stop(context.Background()); err != nil {
		t.Fatalf("the first Stop: %v", err)
	}
	if err := l.stop(context.Background()); err != nil {
		t.Errorf("the second Stop: %v", err)
	}
	if err := l.close(context.Background()); err != nil {
		t.Errorf("Close after two Stops: %v", err)
	}
}

// TestCloseWithoutStop pins the path a host driving the stages by hand takes.
// An endpoint left listening after Close would outlive the shutdown that was
// supposed to release it.
func TestCloseWithoutStop(t *testing.T) {
	logger, _ := newRecordingLogger()
	l, addr := startTestEndpoint(t, testSettings("public"), nethttp.NotFoundHandler(), logger)

	if err := l.close(context.Background()); err != nil {
		t.Fatalf("Close without a preceding Stop: %v", err)
	}
	if l.isAccepting() {
		t.Error("the endpoint still reports that it is accepting requests after Close")
	}
	conn, err := net.DialTimeout("tcp", addr, waitDeadline)
	if err == nil {
		conn.Close()
		t.Error("the socket was still open after Close")
	}
}

// TestDoubleCloseOfTheSocketIsNotAFailure pins the wrapper the socket is
// behind. Stop closes the socket and starts the drain, whose Shutdown closes
// every listener the server still tracks — this one, until the accept loop has
// returned. The two overlap, and unwrapped the second close comes back as "use
// of closed network connection", turning a clean shutdown into a reported
// failure on the runs where the window is hit.
func TestDoubleCloseOfTheSocketIsNotAFailure(t *testing.T) {
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("opening a socket: %v", err)
	}
	once := &onceCloser{Listener: inner}

	if err := once.Close(); err != nil {
		t.Fatalf("the first close: %v", err)
	}
	if err := once.Close(); err != nil {
		t.Errorf("the second close reported %v, and a clean shutdown would carry it as a failure", err)
	}
}

// TestDrainsOfSeveralEndpointsRunTogether pins where the drain is started, which
// no single-endpoint assertion above can reach. Started in Close instead of in
// Stop, every one of them still holds: Stop still returns while a request is in
// flight, Close still waits, the expired budget is still reported. What changes
// is that the endpoints then drain one after another — a shutdown costs the sum
// of the budgets rather than the longest one, and the budget of the endpoint
// waited on last only begins long after it stopped accepting.
func TestDrainsOfSeveralEndpointsRunTogether(t *testing.T) {
	const budget = 300 * time.Millisecond
	const endpoints = 3

	gate := newGateHandler(t)
	logger, _ := newRecordingLogger()

	bound := make([]*listener, 0, endpoints)
	for i := range endpoints {
		s := testSettings(fmt.Sprintf("endpoint-%d", i))
		s.drainTimeout = budget
		l, addr := startTestEndpoint(t, s, gate, logger)
		bound = append(bound, l)
		go func() { _ = get("http://" + addr + "/") }()
	}
	for range endpoints {
		waitFor(t, gate.entered, "a request to reach the handler")
	}

	// Every endpoint stops before any of them is waited on, which is the
	// order the lifecycle uses: the whole Stop stage runs before the Close
	// stage begins.
	for _, l := range bound {
		if err := l.stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}

	started := time.Now()
	for i, l := range bound {
		name := fmt.Sprintf("endpoint-%d", i)
		err := l.close(context.Background())
		if !errors.Is(err, ErrDrainTimeout) {
			t.Fatalf("the drain of %s ran out of time without reporting ErrDrainTimeout: %v", name, err)
		}
		mustContain(t, err.Error(), `"`+name+`"`, "the endpoint whose drain ran out of time")
	}
	elapsed := time.Since(started)

	// Drains that overlap cost the longest budget; drains that are started
	// one at a time cost their sum. The bound sits between the two, far
	// enough from either that a loaded machine does not decide it.
	if elapsed >= 2*budget {
		t.Errorf("waiting on %d endpoints with a drain budget of %s each took %s, which is the "+
			"cost of draining them one after another. Each drain has to have been running "+
			"since its own Stop, so waiting on them in turn costs the longest budget",
			endpoints, budget, elapsed)
	}
}
