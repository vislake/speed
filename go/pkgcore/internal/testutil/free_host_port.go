package testutil

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
)

// maxFreeHostPortAttempts bounds startOnFreeHostPortCore's retry budget.
// Each attempt draws a fresh port that the OS reported free, so a
// collision requires another contender to take that exact port in the
// window between the probe's close and the container's bind; five attempts
// leave the budget far beyond any realistic number of concurrent
// contenders on one host.
const maxFreeHostPortAttempts = 5

// StartOnFreeHostPort runs start with successively chosen free host ports
// until one of them sticks, and returns the host port that did.
//
// The callers are the integration tiers' restart fixtures, whose
// survives-restart proofs pin their container's client port in the
// container's PortBindings: testcontainers' own random host-port allocation
// is re-resolved on every container start (the same container answers on a
// different host port after Stop then Start), while the clients under test
// (go-redis, nats.go, pgx, a minio client) only ever redial addresses they
// already know, so the one stable advertised address is what makes the
// reconnect after the restart deterministic. A host port pinned by hand is
// the one resource those fixtures contend for, and a fixed or
// randomly-drawn pin makes concurrent container starts on one Docker host
// collide: the loser's container start fails with the daemon's "address
// already in use" bind error. Collisions cannot be reserved against --
// Docker must bind the host port itself, so no test-process hold can
// precede it -- which is why this helper makes the collision an event the
// loop absorbs rather than a failure: each attempt asks the OS for a port
// that is free at that moment (binding 0.0.0.0:0, the same wildcard
// address the fixtures bind), so the draw comes from the full ephemeral
// range and two simultaneous probes cannot even receive the same port, and
// a bind error on the container start is treated as a retryable collision
// with a fresh draw rather than a fatal. Only a non-collision start error
// (a bad image, a missing daemon) fails immediately, and exhausting the
// retry budget fails with the last error, so a real defect is never masked
// as retry noise. The decision loop lives in startOnFreeHostPortCore so
// every branch of it is unit-testable without a live container;
// free_host_port_test.go pins the contracts.
func StartOnFreeHostPort(t *testing.T, what string, start func(hostPort string) error) string {
	t.Helper()

	hostPort, retries, err := startOnFreeHostPortCore(start)
	if err != nil {
		t.Fatalf("start %s: %v", what, err)
	}
	if retries > 0 {
		t.Logf("start %s: the host port binding collided %d time(s) with a concurrent start before a fresh free port stuck (host port %s)", what, retries, hostPort)
	}
	return hostPort
}

// startOnFreeHostPortCore is the decision loop behind
// StartOnFreeHostPort: probe a free host port, run start against it, and
// classify the outcome. A nil start error returns the working host port
// and the number of collisions the loop absorbed; a non-collision start
// error returns immediately (never retried into noise) wrapped with the
// port it failed on; persistent collisions exhaust maxFreeHostPortAttempts
// and return the last collision error wrapped with the budget.
func startOnFreeHostPortCore(start func(hostPort string) error) (hostPort string, retries int, err error) {
	var lastErr error
	for attempt := 1; attempt <= maxFreeHostPortAttempts; attempt++ {
		hostPort, err = freeHostPort()
		if err != nil {
			return "", retries, fmt.Errorf("allocate a free host port: %w", err)
		}
		if lastErr = start(hostPort); lastErr == nil {
			return hostPort, retries, nil
		}
		if !isHostPortBindCollision(lastErr) {
			return "", retries, fmt.Errorf("start on host port %s: %w", hostPort, lastErr)
		}
		retries++
	}
	return "", retries, fmt.Errorf("the host port binding collided on all %d attempts (each drawn port was taken before the container bound it); last error: %w",
		maxFreeHostPortAttempts, lastErr)
}

// HostPortFree reports whether hostPort can be bound on 0.0.0.0 right now,
// by attempting exactly that bind and closing the listener again on
// success. It is the pre-flight check for the one fixture shape that
// cannot pick its port -- the integration legs that
// bind the literal default ports (6379/4222) because the
// zero-configuration seam constructors under test dial exactly those
// addresses, so the collision-free fix there is to skip when the required
// port is genuinely occupied rather than fail the run.
func HostPortFree(hostPort string) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", hostPort))
	if err != nil {
		return false
	}
	return l.Close() == nil
}

// freeHostPort asks the OS for a currently-unbound TCP port on 0.0.0.0 and
// releases it again, returning it as a string. The kernel hands out
// ephemeral ports atomically, so two concurrent probes never receive the
// same port; the port can still be taken after the probe closes, which is
// precisely the race startOnFreeHostPortCore's retry loop absorbs.
func freeHostPort() (string, error) {
	l, err := net.Listen("tcp", "0.0.0.0:0") //nolint:gosec // G102: the probe deliberately binds the same 0.0.0.0 wildcard the fixtures' Docker port bindings use, so its conflict check is exact; it accepts no traffic and lives only in test support
	if err != nil {
		return "", err
	}
	tcpAddr, ok := l.Addr().(*net.TCPAddr)
	if !ok {
		if closeErr := l.Close(); closeErr != nil {
			return "", closeErr
		}
		return "", errors.New("free host port probe did not receive a TCP address")
	}
	port := tcpAddr.Port
	if closeErr := l.Close(); closeErr != nil {
		return "", closeErr
	}
	return strconv.Itoa(port), nil
}

// isHostPortBindCollision reports whether err is the Docker daemon's
// refusal to bind a host port that is already taken: either by another
// process on the host ("address already in use", the wording this
// repository's CI collision surfaced with) or by another container's
// binding ("port is already allocated", the daemon's own container-to-
// container wording). Any other start error is a real defect and must not
// be retried into noise.
func isHostPortBindCollision(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "port is already allocated")
}
