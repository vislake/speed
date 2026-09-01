package dbtest

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"time"
)

// defaultDockerSocket is the Unix socket dockerHostAddress dials when
// DOCKER_HOST is unset. It is the conventional local Docker Engine API
// endpoint: a plain Docker Engine install on Linux listens there directly,
// and Docker Desktop and Colima both arrange for it to exist too (as a
// symlink to their own VM-side socket, on the platforms that need one) so
// that tooling written against this one default keeps working unmodified.
// It is also the same fallback testcontainers-go's own host resolution
// uses (see its internal/core.extractDockerHost).
const defaultDockerSocket = "/var/run/docker.sock"

// dockerDialTimeout bounds how long dockerAvailable waits for a connection
// before concluding the daemon is not reachable. It is short and
// non-configurable on purpose: this check only decides between running a
// real container and skipping cleanly, never waits out a slow daemon.
const dockerDialTimeout = 2 * time.Second

// dockerAvailable reports whether a Docker (or Docker-API-compatible, e.g.
// Podman) daemon appears reachable, by attempting a plain network dial —
// no Engine API request, just the connection itself — against the
// endpoint dockerHostAddress resolves. NewPostgres calls it before ever
// invoking testcontainers-go, so it can t.Skip cleanly instead of failing
// with a wall of low-level connection errors when there is plainly no
// Docker in the environment at all.
//
// This is deliberately a small, self-contained probe rather than a call
// into testcontainers-go's own provider/host-resolution machinery
// (testcontainers.NewDockerProvider, internal/core.ExtractDockerHost):
// that machinery caches its resolved host for the lifetime of the process
// behind a sync.Once, so a test that deliberately points DOCKER_HOST at an
// unreachable address — exactly what this file's own test does, to prove
// this function fails closed — would permanently poison Docker host
// resolution for every other test sharing that process, including
// NewPostgres's own happy-path test in the same package. Reading the
// environment and dialing fresh on every call keeps this check
// side-effect-free and safe to unit-test directly with a
// deliberately-wrong host, in any order, alongside tests that need the
// real daemon.
//
// A successful dial only proves something is listening at the resolved
// address, not that it is genuinely a Docker daemon; the authoritative
// check is the real container start NewPostgres performs right after. A
// false positive here just becomes an ordinary (non-skip) test failure at
// that later, real step. This function exists only to catch the common
// case — no daemon at all — cleanly.
func dockerAvailable(ctx context.Context) error {
	network, address, err := dockerHostAddress()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, dockerDialTimeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, network, address)
	if err != nil {
		return fmt.Errorf("dbtest: docker daemon not reachable at %s %s: %w", network, address, err)
	}
	return conn.Close()
}

// dockerHostAddress returns the network ("unix" or "tcp") and address
// dockerAvailable should dial to reach the Docker daemon: the target named
// by the DOCKER_HOST environment variable when it is set, following the
// same "unix://" / "tcp://" scheme convention the Docker CLI and
// testcontainers-go both accept, or defaultDockerSocket when it is unset.
//
// This covers the two forms every mainstream local or CI setup this
// codebase targets (macOS/Linux dev, Linux CI runners) actually uses. It
// deliberately does not attempt every alternative testcontainers-go's own
// resolution chain understands — a `~/.testcontainers.properties` file, a
// `docker context` other than the current one, Windows named pipes — since
// those exist to support environments outside this project's target
// platforms; an environment relying on one of those and expecting
// NewPostgres to run rather than skip should set DOCKER_HOST explicitly,
// which this function honors.
func dockerHostAddress() (network, address string, err error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		return "unix", defaultDockerSocket, nil
	}

	u, err := url.Parse(host)
	if err != nil {
		return "", "", fmt.Errorf("dbtest: parse DOCKER_HOST %q: %w", host, err)
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
		return "", "", fmt.Errorf("dbtest: unsupported DOCKER_HOST scheme %q in %q", u.Scheme, host)
	}
}
