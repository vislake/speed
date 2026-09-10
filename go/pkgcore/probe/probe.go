// Package probe issues one container self-probe: a GET request to the
// process's own HTTP server over the loopback interface.
//
// The deployment shape it serves is a container built FROM a distroless/static
// runtime image -- no shell, no curl, no wget -- whose Dockerfile HEALTHCHECK
// has nothing but the process's own binary to re-invoke. The pattern: the
// binary's entry point takes a sentinel argument, and instead of booting the
// server it calls Check against the server it would otherwise have become.
//
// The loopback host is structural, never a parameter: Check dials 127.0.0.1
// and nothing else, so the port and path its caller configures can only
// select which local listener is probed, never which host answers. Everything
// environment-shaped stays with the caller -- this package reads no
// environment variable and carries no port fallback -- a host resolves its
// own configuration and passes the resolved port and endpoint path in, the
// same way it hands them to its listener.
package probe

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// loopbackHost is the one host every probe dials. Keeping it a package
// constant is what makes the probe's reach structural: no parameter (and
// therefore no caller input) can widen it to another host, and port/path only
// ever select a local listener and an endpoint on it.
const loopbackHost = "127.0.0.1"

// ErrUnexpectedStatus reports a probe whose request completed but whose
// response carried a status other than the expected one -- the degraded-server
// shape a transport-error-only check would report healthy. Check wraps it, so
// a caller can classify with errors.Is.
var ErrUnexpectedStatus = errors.New("probe: unexpected status")

// settings carries one Check call's caller-adjustable parameters.
type settings struct {
	timeout    time.Duration
	wantStatus int
}

// Option adjusts one probe's parameters. The zero set is the ordinary
// container probe: no bound of its own, expecting 200.
type Option func(*settings)

// WithTimeout bounds the whole probe attempt -- connect, request and response
// -- independent of ctx, so a wedged server is reported unhealthy rather than
// waited on forever by a caller whose context has no deadline of its own. A
// zero or negative timeout adds no bound, leaving ctx as the only one.
func WithTimeout(timeout time.Duration) Option {
	return func(s *settings) { s.timeout = timeout }
}

// WithWantStatus declares the status a healthy answer carries, defaulting to
// 200. It exists for a host whose liveness endpoint deliberately answers
// something else; the check is an exact comparison, never a 2xx range, so a
// degraded answer cannot pass as healthy.
func WithWantStatus(status int) Option {
	return func(s *settings) { s.wantStatus = status }
}

// Check issues one GET request to path at 127.0.0.1:port and reports whether
// the response answered with the expected status (200 unless WithWantStatus
// says otherwise).
//
// port is the caller-resolved listening port as a dial string ("8080"), path
// the endpoint to ask (an absolute path like "/healthz"). ctx bounds the
// probe; WithTimeout adds this package's own bound on top.
//
// A nil error means the server answered with the expected status. Every other
// outcome is an error: the request could not be sent, or it completed with a
// different status (wrapping ErrUnexpectedStatus), or the bound elapsed.
func Check(ctx context.Context, port, path string, opts ...Option) error {
	s := settings{wantStatus: http.StatusOK}
	for _, opt := range opts {
		opt(&s)
	}
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	target := "http://" + net.JoinHostPort(loopbackHost, port) + path
	// #nosec G704 -- gosec's taint analysis flags this as SSRF because port
	// and path are parameters, but the host part of the URL is the literal
	// constant 127.0.0.1 above, never anything a parameter can influence:
	// port and path only select which local listener and endpoint this
	// binary's own server is probed on, and their values come from the
	// host's process configuration, never from a request this binary serves.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req) // #nosec G704 -- see the request construction above
	if err != nil {
		return fmt.Errorf("request %s: %w", target, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != s.wantStatus {
		return fmt.Errorf("%s answered %d, want %d: %w", target, resp.StatusCode, s.wantStatus, ErrUnexpectedStatus)
	}
	return nil
}
