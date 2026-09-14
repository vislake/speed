package http

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	nethttp "net/http"
	"sync"
	"time"

	"github.com/vislake/speed/pkg/log"
)

// listener is everything about one endpoint that exists only between Serve and
// Close: the server, the socket, and the drain that outlives Stop.
//
// Its state is read by Accepting from any goroutine at any moment and written
// by Serve, by Stop and by the background drain, so all of it is behind the
// mutex.
type listener struct {
	mu sync.Mutex
	// name and drainTimeout are copied from the settings when the address is
	// bound, so the messages the background drain writes carry them without
	// reaching back into the endpoint.
	name         string
	drainTimeout time.Duration

	server *nethttp.Server
	socket net.Listener

	// bound says the address was bound at least once. Stop and Close on an
	// endpoint that never got there return nil: a startup failure rolls back
	// every constructed instance whatever stage it reached.
	bound bool
	// accepting is what Accepting reports. It goes false on Stop and on an
	// accept loop that exited on its own.
	accepting bool
	// stopping records that Stop ran, which is what tells a normal shutdown
	// apart from an accept loop that died by itself.
	stopping bool

	// drained is closed when the background drain has finished, and drainErr
	// holds what it came back with. Close waits on the channel rather than
	// draining itself, because the drain starts in Stop.
	drained  chan struct{}
	drainErr error

	// logger is where this endpoint's own diagnostics go. It is the module's
	// logger, not the one injected into a request context.
	logger *slog.Logger
	// listen opens the socket. It is a field so that a test can hand in a
	// socket whose Accept fails, which is the only way to reach the accept
	// loop's anomalous exit from outside.
	listen func(network, address string) (net.Listener, error)
}

// start binds the address and puts the assembled handler behind it.
//
// The bind is synchronous and the accept loop is not: the Serve callback must
// not block, because the driver advances through the stage before it enters
// running state, while a failure to bind has to terminate the startup rather
// than surface in a goroutine nobody is watching. Loading the certificate is
// synchronous for the same reason, which is why the TLS listener is built here
// instead of letting the server's own ServeTLS do it after the callback has
// returned.
func (l *listener) start(s endpointSettings, h nethttp.Handler, logger *slog.Logger) error {
	if logger == nil {
		logger = log.Default()
	}
	open := l.listen
	if open == nil {
		open = net.Listen
	}

	socket, err := open("tcp", s.address)
	if err != nil {
		return fmt.Errorf("%w: endpoint %q on %q: %w", ErrListen, s.name, s.address, err)
	}
	if s.tls() {
		// A certificate that cannot be loaded is a bind failure of this
		// endpoint and joins ErrListen rather than adding a sentinel: the
		// class a caller acts on is "this endpoint did not come up", and
		// the text carries which half of it failed.
		cert, certErr := tls.LoadX509KeyPair(s.tlsCertFile, s.tlsKeyFile)
		if certErr != nil {
			closeErr := socket.Close()
			return errors.Join(fmt.Errorf("%w: endpoint %q could not load the certificate %q "+
				"with the key %q: %w", ErrListen, s.name, s.tlsCertFile, s.tlsKeyFile, certErr), closeErr)
		}
		socket = tls.NewListener(socket, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		})
	}

	server := &nethttp.Server{
		Addr:              s.address,
		Handler:           h,
		ReadHeaderTimeout: s.readHeaderTimeout,
		ReadTimeout:       s.readTimeout,
		WriteTimeout:      s.writeTimeout,
		IdleTimeout:       s.idleTimeout,
	}
	// The socket is wrapped so that closing it twice is closing it once.
	// Stop closes it and starts the Shutdown that follows straight away,
	// while the accept loop only stops tracking the socket once it has
	// returned; the two overlap, and in that window Shutdown closes a socket
	// Stop has already closed. Unwrapped, the second close answers "use of
	// closed network connection" and a clean shutdown reports a failure.
	socket = &onceCloser{Listener: socket}

	l.mu.Lock()
	l.name = s.name
	l.drainTimeout = s.drainTimeout
	l.server = server
	l.socket = socket
	l.bound = true
	l.accepting = true
	l.drained = make(chan struct{})
	l.logger = logger
	l.mu.Unlock()

	go l.accept(server, socket)
	return nil
}

// accept runs the accept loop and records how it ended.
//
// An exit this module caused — Stop closed the socket, or the server was shut
// down — is the normal path and says nothing. Any other exit is an anomaly:
// the address was bound successfully, so there is no startup to terminate any
// more, and Run is by now blocked waiting for the host to cancel, with nothing
// this module could wake it with. What is left is to write it down and to stop
// claiming the endpoint accepts requests. Whether that amounts to an unhealthy
// process is for whoever reads Accepting to decide; this module does not judge
// it and does not restart anything.
func (l *listener) accept(server *nethttp.Server, socket net.Listener) {
	err := server.Serve(socket)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.accepting = false
	if l.stopping || errors.Is(err, nethttp.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
		return
	}
	l.logger.Error("the accept loop of an HTTP endpoint exited on its own; "+
		"the endpoint no longer accepts requests and the process keeps running",
		"endpoint", l.name, "error", err)
}

// stop closes the socket in the calling goroutine and leaves the drain running
// in the background.
//
// Closing the socket is not put in the goroutine: when Stop returns, "no new
// request is accepted" has to be true already, and when a goroutine gets
// scheduled is not up to the caller. Waiting for the drain is not done here
// either: Stop is called on one module after another, so a drain of half a
// minute inside an entry point would hold back the "stop taking new work" of
// every module behind it — and those are still taking work while it waits.
func (l *listener) stop(ctx context.Context) error {
	l.mu.Lock()
	if !l.bound || l.stopping {
		// Never bound, or stopped already. Both are ordinary: rollback
		// stops instances that never served, and a host may stop twice.
		l.mu.Unlock()
		return nil
	}
	l.stopping = true
	l.accepting = false
	name, server, socket := l.name, l.server, l.socket
	drained, timeout := l.drained, l.drainTimeout
	l.mu.Unlock()

	closeErr := socket.Close()
	go l.drain(ctx, server, drained, timeout)
	if closeErr != nil {
		return fmt.Errorf("http: endpoint %q could not close its listening socket: %w", name, closeErr)
	}
	return nil
}

// drain waits for the in-flight requests, bounded by this endpoint's own
// configured timeout.
//
// The bound cannot come from the context: the one Stop and Close receive has
// its cancellation stripped, precisely so that a drain honouring its context
// does not return the instant the host cancels. The clock starts here, where
// the draining really begins, so a slow Stop in some other module does not eat
// into this endpoint's drain budget.
func (l *listener) drain(ctx context.Context, server *nethttp.Server, drained chan struct{}, timeout time.Duration) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	err := server.Shutdown(ctx)
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		// Shutdown gives up waiting but leaves the connections it was
		// waiting for running. Close is what actually cuts them, and
		// without it the caller is told the endpoint was cut while the
		// requests carry on.
		cutErr := server.Close()
		err = fmt.Errorf("%w: endpoint %q still had requests in flight when its drain-timeout "+
			"of %s expired, and the connections still open were cut. Raise %s.%s.%s.drain-timeout "+
			"if the requests this endpoint serves legitimately take longer",
			ErrDrainTimeout, l.name, timeout, configNamespace, endpointsItem, l.name)
		if cutErr != nil {
			err = errors.Join(err, fmt.Errorf(
				"http: endpoint %q could not cut its remaining connections: %w", l.name, cutErr))
		}
	case err != nil:
		err = fmt.Errorf("http: endpoint %q failed to drain: %w", l.name, err)
	}

	l.mu.Lock()
	l.drainErr = err
	l.mu.Unlock()
	close(drained)
}

// close waits for this endpoint's drain to finish and reports what it came back
// with. An endpoint that never bound returns nil, and so does one that drained
// cleanly.
//
// A Close that was not preceded by a Stop starts the drain itself: rollback
// runs Stop and Close over every constructed instance, but a host driving the
// stages by hand may skip straight to Close, and an endpoint left listening
// after that would outlive the process's shutdown.
func (l *listener) close(ctx context.Context) error {
	l.mu.Lock()
	bound, stopping := l.bound, l.stopping
	l.mu.Unlock()
	if !bound {
		return nil
	}

	var stopErr error
	if !stopping {
		stopErr = l.stop(ctx)
	}

	l.mu.Lock()
	drained := l.drained
	l.mu.Unlock()
	<-drained

	l.mu.Lock()
	defer l.mu.Unlock()
	return errors.Join(stopErr, l.drainErr)
}

// isAccepting reports whether this endpoint is still taking requests.
func (l *listener) isAccepting() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.accepting
}

// addr is the address the socket really bound, which is what a test that asked
// for port 0 connects to. It is empty on an endpoint that never bound.
func (l *listener) addr() net.Addr {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.socket == nil {
		return nil
	}
	return l.socket.Addr()
}

// onceCloser closes the wrapped listener once and hands the same result back
// to every later call. Two closes race by construction here: Stop closes the
// socket and starts the drain, and the drain's Shutdown closes every listener
// the server is still tracking, which this one is until the accept loop has
// returned. The window is short and the failure it produces is intermittent,
// which is what makes it worth closing rather than diagnosing.
type onceCloser struct {
	net.Listener
	once sync.Once
	err  error
}

func (o *onceCloser) Close() error {
	o.once.Do(func() { o.err = o.Listener.Close() })
	return o.err
}
