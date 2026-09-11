// Tests for the container constructors themselves. Every test starts a
// real container, so each probe-skips when no Docker daemon is reachable
// (testutil.RequireDocker) and runs the constructor end to end otherwise
// -- this is what proves the package's own plumbing (image pin, cleanup,
// connection-string parsing, the pinned-port restart fixture) rather than
// the Redis server's behavior, which the consuming tiers' contract suites
// own.
package redistest_test

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore/internal/testutil"
	"github.com/vislake/speed/go/pkgcore/redistest"
)

// TestClient_PingSucceeds proves the common constructor end to end: the
// container starts, the connection string parses, and the returned client
// reaches the server.
func TestClient_PingSucceeds(t *testing.T) {
	testutil.RequireDocker(t)

	ctx := context.Background()
	client := redistest.Client(t, ctx)
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("PING against the container-backed client: %v", err)
	}
}

// TestClientPair_PeerSeesPrimaryWrite proves the pair's two connections
// are genuinely independent clients of one server: a key written through
// the primary is visible through the peer.
func TestClientPair_PeerSeesPrimaryWrite(t *testing.T) {
	testutil.RequireDocker(t)

	ctx := context.Background()
	primary, peer := redistest.ClientPair(t, ctx)
	if err := primary.Set(ctx, "redistest-pair-key", "pair-value", 0).Err(); err != nil {
		t.Fatalf("primary SET: %v", err)
	}
	got, err := peer.Get(ctx, "redistest-pair-key").Result()
	if err != nil {
		t.Fatalf("peer GET: %v", err)
	}
	if got != "pair-value" {
		t.Fatalf("peer GET = %q, want %q", got, "pair-value")
	}
}

// TestPersistent_ServesPingsAndAddrReachesTheContainer proves the
// pinned-port restart fixture starts a usable server: the returned client
// PINGs (after WaitReady), and the address Addr reports for the container
// is connectable by an independent client -- the two properties the
// survives-restart proofs depend on across their container Stop/Start.
func TestPersistent_ServesPingsAndAddrReachesTheContainer(t *testing.T) {
	testutil.RequireDocker(t)

	ctx := context.Background()
	container, client := redistest.Persistent(t, ctx)
	redistest.WaitReady(t, ctx, client)
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("PING against the persistent fixture's client: %v", err)
	}

	addr := redistest.Addr(t, ctx, container)
	raw := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = raw.Close() })
	if err := raw.Ping(ctx).Err(); err != nil {
		t.Fatalf("PING against the address Addr reported (%s): %v", addr, err)
	}
}
