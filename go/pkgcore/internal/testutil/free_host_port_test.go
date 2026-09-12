package testutil

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

// The regression this file pins is the one the integration tiers' restart
// fixtures hit on parallel CI: a host port pinned by hand for the
// survives-restart proofs (testcontainers re-resolves its random host-port
// allocation on every container start, so the stable advertised address
// the restarting clients redial has to be pinned) collides with a
// concurrent container start, and the loser's container start used to fail
// the whole test with the daemon's "address already in use" bind error.
// startOnFreeHostPortCore exists so that collision is absorbed -- a fresh,
// OS-confirmed-free port per attempt and a retry on the bind failure --
// and these tests pin that contract: the retry happens on exactly the
// daemon's collision wordings, a non-collision start error returns
// immediately (never retried into noise), and the retry budget is bounded.
// The end-to-end shape -- occupied host ports, pre-fix failure with the
// bind error, post-fix pass -- is exercised against real Docker in the
// tiers themselves, whose fixtures all drive this same core.
func TestStartOnFreeHostPort_RetriesOnBindCollisionThenSucceeds(t *testing.T) {
	const daemonBindError = "Error response from daemon: ports are not available: exposing port TCP 0.0.0.0:49578 -> 127.0.0.1:0: listen tcp4 0.0.0.0:49578: bind: address already in use"
	const containerAllocatedError = "Error response from daemon: driver failed programming external connectivity: Bind for 0.0.0.0:44264 failed: port is already allocated"

	// The first two offered ports collide (one with each of the daemon's
	// two collision wordings); the third sticks.
	var offered []string
	hostPort, retries, err := startOnFreeHostPortCore(func(p string) error {
		offered = append(offered, p)
		switch len(offered) {
		case 1:
			return errors.New(daemonBindError)
		case 2:
			return errors.New(containerAllocatedError)
		default:
			return nil
		}
	})
	if err != nil {
		t.Fatalf("startOnFreeHostPortCore returned error %v, want nil after the collisions were absorbed", err)
	}
	if retries != 2 {
		t.Errorf("retries = %d, want 2 absorbed collisions", retries)
	}
	if hostPort != offered[2] {
		t.Errorf("hostPort = %q, want the third offered port %q", hostPort, offered[2])
	}
	if len(offered) != 3 {
		t.Fatalf("start was offered %d ports (%v), want 3: two collisions absorbed, one success", len(offered), offered)
	}
	seen := map[string]bool{}
	for _, p := range offered {
		seen[p] = true
	}
	if len(seen) != 3 {
		t.Fatalf("the three offered ports were not distinct: %v", offered)
	}
}

func TestStartOnFreeHostPortCore_FailsImmediatelyOnNonCollisionError(t *testing.T) {
	const imageErr = "docker: image pull failed: no such image"
	attempts := 0
	var offered []string
	hostPort, retries, err := startOnFreeHostPortCore(func(p string) error {
		attempts++
		offered = append(offered, p)
		return errors.New(imageErr)
	})
	if err == nil {
		t.Fatal("startOnFreeHostPortCore returned nil error, want the non-collision error back")
	}
	if !strings.Contains(err.Error(), imageErr) || !strings.Contains(err.Error(), offered[0]) {
		t.Errorf("error %q does not carry the original error and the port it failed on", err)
	}
	if attempts != 1 {
		t.Errorf("start ran %d times, want exactly 1: a non-collision error must not be retried", attempts)
	}
	if retries != 0 {
		t.Errorf("retries = %d, want 0", retries)
	}
	if hostPort != "" {
		t.Errorf("hostPort = %q, want empty on failure", hostPort)
	}
}

func TestStartOnFreeHostPortCore_ExhaustsItsRetryBudget(t *testing.T) {
	attempts := 0
	hostPort, retries, err := startOnFreeHostPortCore(func(p string) error {
		attempts++
		return errors.New("bind: address already in use")
	})
	if err == nil {
		t.Fatal("startOnFreeHostPortCore returned nil error, want exhaustion after the bounded budget")
	}
	if attempts != maxFreeHostPortAttempts {
		t.Errorf("start ran %d times, want the %d-attempt budget", attempts, maxFreeHostPortAttempts)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(maxFreeHostPortAttempts)) {
		t.Errorf("error %q does not name the exhausted %d-attempt budget", err, maxFreeHostPortAttempts)
	}
	if retries != maxFreeHostPortAttempts {
		t.Errorf("retries = %d, want %d", retries, maxFreeHostPortAttempts)
	}
	if hostPort != "" {
		t.Errorf("hostPort = %q, want empty on failure", hostPort)
	}
}

func TestIsHostPortBindCollision(t *testing.T) {
	// The two wordings a collision surfaces with: the daemon's refusal to
	// bind a host port another process holds, and its refusal to allocate
	// a port another container holds.
	collisions := []string{
		"Error response from daemon: ports are not available: exposing port TCP 0.0.0.0:44264 -> 127.0.0.1:0: listen tcp4 0.0.0.0:44264: bind: address already in use",
		"Error response from daemon: driver failed programming external connectivity on endpoint: Bind for 0.0.0.0:44264 failed: port is already allocated",
	}
	for _, msg := range collisions {
		if !isHostPortBindCollision(errors.New(msg)) {
			t.Errorf("isHostPortBindCollision(%q) = false, want true", msg)
		}
	}
	notCollisions := []string{
		"docker: image pull failed: no such image",
		"Error response from daemon: No such container: deadbeef",
		"connection refused",
	}
	for _, msg := range notCollisions {
		if isHostPortBindCollision(errors.New(msg)) {
			t.Errorf("isHostPortBindCollision(%q) = true, want false", msg)
		}
	}
}
