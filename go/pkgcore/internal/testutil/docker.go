package testutil

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"testing"
	"time"
)

// defaultDockerSocket is the Unix socket dockerHostAddress dials when
// DOCKER_HOST is unset. It is the conventional local Docker Engine API
// endpoint: a plain Docker Engine install on Linux listens there
// directly, and Docker Desktop and Colima both arrange for it to exist
// too (as a symlink to their own VM-side socket, on the platforms that
// need one) so that tooling written against this one default keeps
// working unmodified.
const defaultDockerSocket = "/var/run/docker.sock"

// dockerDialTimeout bounds how long dockerAvailable waits for a
// connection before concluding the daemon is not reachable. It is short
// and non-configurable on purpose: this check only decides between
// running a real container and skipping cleanly, never waits out a slow
// daemon.
const dockerDialTimeout = 2 * time.Second

// RequireDocker skips the test when no Docker (or Docker-API-compatible)
// daemon appears reachable, for suites whose only failure mode without a
// daemon would be a wall of low-level connection errors. The caller's
// container constructors carry no skip path of their own (a tier that
// spins up real infrastructure fails loudly when one is genuinely
// missing); this helper is the deliberate exception that keeps a plain
// unit run with no Docker green.
func RequireDocker(t *testing.T) {
	t.Helper()
	if err := dockerAvailable(); err != nil {
		t.Skipf("skipping container-backed test: %v", err)
	}
}

// dockerAvailable reports whether a Docker daemon appears reachable, by
// attempting a plain network dial -- no Engine API request, just the
// connection itself -- against the endpoint dockerHostAddress resolves.
//
// A successful dial only proves something is listening at the resolved
// address, not that it is genuinely a Docker daemon; the authoritative
// check is the real container start the caller performs right after. A
// false positive here just becomes an ordinary (non-skip) test failure
// at that later, real step. This function exists only to catch the
// common case -- no daemon at all -- cleanly.
func dockerAvailable() error {
	network, address, err := dockerHostAddress()
	if err != nil {
		return err
	}

	conn, err := net.DialTimeout(network, address, dockerDialTimeout)
	if err != nil {
		return fmt.Errorf("no Docker daemon reachable at %s %s: %w", network, address, err)
	}
	return conn.Close()
}

// dockerHostAddress returns the network ("unix" or "tcp") and address
// dockerAvailable should dial to reach the Docker daemon: the target
// named by the DOCKER_HOST environment variable when it is set, following
// the same "unix://" / "tcp://" scheme convention the Docker CLI and
// testcontainers-go both accept, or defaultDockerSocket when it is unset.
//
// This covers the two forms every mainstream local or CI setup this
// codebase targets (macOS/Linux dev, Linux CI runners) actually uses. It
// deliberately does not attempt every alternative testcontainers-go's own
// resolution chain understands -- a `~/.testcontainers.properties` file,
// a `docker context` other than the current one, Windows named pipes --
// since those exist to support environments outside this project's target
// platforms; an environment relying on one of those and expecting a
// container-backed test to run rather than skip should set DOCKER_HOST
// explicitly, which this function honors.
func dockerHostAddress() (network, address string, err error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		return "unix", defaultDockerSocket, nil
	}

	u, err := url.Parse(host)
	if err != nil {
		return "", "", fmt.Errorf("parse DOCKER_HOST %q: %w", host, err)
	}

	switch u.Scheme {
	case "unix":
		path := u.Path
		if path == "" {
			path = u.Opaque
		}
		return "unix", path, nil
	case "tcp", "http", "https":
		return "tcp", u.Host, nil
	default:
		return "", "", fmt.Errorf("unsupported DOCKER_HOST scheme %q in %q", u.Scheme, host)
	}
}
