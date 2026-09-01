package dbtest

import (
	"context"
	"testing"
)

// TestDockerHostAddress_DockerHostUnset_ReturnsConventionalSocket covers
// the default dockerAvailable uses on any machine that has not customized
// DOCKER_HOST -- the common case for local dev and CI alike.
func TestDockerHostAddress_DockerHostUnset_ReturnsConventionalSocket(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")

	network, address, err := dockerHostAddress()
	if err != nil {
		t.Fatalf("dockerHostAddress() error = %v", err)
	}
	if network != "unix" || address != defaultDockerSocket {
		t.Errorf("dockerHostAddress() = (%q, %q), want (\"unix\", %q)", network, address, defaultDockerSocket)
	}
}

// TestDockerHostAddress_UnixDockerHost_ParsesSocketPath covers an
// explicitly-set unix:// DOCKER_HOST, the form Colima and a manually
// configured rootless daemon typically use.
func TestDockerHostAddress_UnixDockerHost_ParsesSocketPath(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///home/dev/.colima/default/docker.sock")

	network, address, err := dockerHostAddress()
	if err != nil {
		t.Fatalf("dockerHostAddress() error = %v", err)
	}
	if network != "unix" || address != "/home/dev/.colima/default/docker.sock" {
		t.Errorf("dockerHostAddress() = (%q, %q), want (\"unix\", \"/home/dev/.colima/default/docker.sock\")", network, address)
	}
}

// TestDockerHostAddress_TCPDockerHost_ParsesHostPort covers a remote or
// TCP-exposed daemon, the form a CI runner using "docker:dind" often sets.
func TestDockerHostAddress_TCPDockerHost_ParsesHostPort(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:2375")

	network, address, err := dockerHostAddress()
	if err != nil {
		t.Fatalf("dockerHostAddress() error = %v", err)
	}
	if network != "tcp" || address != "127.0.0.1:2375" {
		t.Errorf("dockerHostAddress() = (%q, %q), want (\"tcp\", \"127.0.0.1:2375\")", network, address)
	}
}

// TestDockerHostAddress_UnsupportedScheme_ReturnsError covers a
// DOCKER_HOST scheme this function does not understand (Windows named
// pipes, in this example): it must fail with a clear error rather than
// silently dialing the wrong thing or panicking.
func TestDockerHostAddress_UnsupportedScheme_ReturnsError(t *testing.T) {
	t.Setenv("DOCKER_HOST", "npipe:////./pipe/docker_engine")

	if _, _, err := dockerHostAddress(); err == nil {
		t.Error("dockerHostAddress() error = nil, want an error for an unsupported DOCKER_HOST scheme")
	}
}

// TestDockerAvailable_DeliberatelyWrongUnixSocket_ReturnsError is this
// package's skip-detection proof: Docker is genuinely available in this
// development/CI environment (NewPostgres's own happy-path test in
// dbtest_test.go proves that end to end), so "no Docker" cannot be
// simulated at the OS level here. Instead, this test points dockerAvailable
// at a Unix socket path that is guaranteed not to exist and confirms it
// reports unavailable -- proving the check itself fails closed the same
// way it would in a genuinely Docker-less environment, which is what
// NewPostgres relies on to t.Skip cleanly there instead of failing.
//
// See dockerAvailable's own doc comment for why this test sets DOCKER_HOST
// directly rather than going through testcontainers-go's own provider
// health check: that machinery caches its resolved host process-wide, so
// doing it that way here would permanently break Docker detection for
// every later test in this binary, including NewPostgres's.
func TestDockerAvailable_DeliberatelyWrongUnixSocket_ReturnsError(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/dbtest-docker-probe-test/does-not-exist.sock")

	if err := dockerAvailable(context.Background()); err == nil {
		t.Error("dockerAvailable() error = nil, want an error for a deliberately-wrong Docker host")
	}
}

// TestDockerAvailable_DeliberatelyWrongTCPHost_ReturnsError is the TCP
// counterpart of the test above: nothing listens on this loopback port, so
// the dial itself must fail (connection refused), independent of the
// Unix-socket code path.
func TestDockerAvailable_DeliberatelyWrongTCPHost_ReturnsError(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")

	if err := dockerAvailable(context.Background()); err == nil {
		t.Error("dockerAvailable() error = nil, want an error for a deliberately-wrong Docker host")
	}
}
