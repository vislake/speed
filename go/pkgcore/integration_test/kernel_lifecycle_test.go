//go:build integration

// Package pkgcore_test holds go/pkgcore's own root integration tier: tests
// that exercise the root package's Kernel seam lifecycle against real
// infrastructure. It is physically separate from the package's unit tests
// and carries the "integration" build tag: a plain "go test ./..." never
// compiles or runs anything in this directory; it is invoked explicitly
// with "go test -tags=integration ./...". Every test here spins up its own
// disposable container and requires a working Docker (or
// Docker-API-compatible) daemon; there is no fallback or skip-on-missing-
// Docker path, matching the integration tiers of the eventbus/* and kv/*
// subpackages.
//
// The unit-tier half of this story lives in kernel_shutdown_test.go (fake
// closeable implementations proving that the Kernel records preset-resolved
// seams' closers, runs them at Shutdown, and closes them on its own failure
// path). This tier proves the same mechanism against the real
// implementations, whose dialed connections are countable on the real
// servers:
//
//   - a successful Bootstrap under a Redis-backed preset followed by
//     Kernel.Shutdown must return the Redis server's connected-clients
//     count to its baseline (before the seam lifecycle existed, nothing
//     anywhere closed the client the registration built, so those
//     connections stayed open for the process's lifetime);
//   - the same shape over a real NATS server must drop the server's
//     connection count to zero on its own monitoring endpoint, and the
//     closed seam values must fail their next operation;
//   - a Bootstrap that fails after resolving real NATS seams must close
//     the connections it already dialed before returning the error
//     (pre-fix, the dialed connections had no close path at all).
//
// # Why the containers bind the default client ports
//
// The registration constructors under test ("eventbus.redis", "kv.redis",
// "eventbus.nats", "kv.nats") build their clients from a Preset's Config,
// which is always empty today (the preset layer's per-implementation
// parameter channel is deliberately deferred -- PresetDistributed's own doc
// comment records that gap). Their zero-configuration fallbacks are
// "localhost:6379" and nats.DefaultURL ("nats://127.0.0.1:4222"), so these
// legs publish the container's client port to exactly that host port -- the
// same "a zero-configuration composition against local infrastructure"
// shape those fallbacks exist for, and the only shape a Preset-built Kernel
// can reach at all today. The host ports are checked for conflicts at
// container start (Docker refuses the bind loudly rather than silently
// sharing a server).
package pkgcore_test

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vislake/speed/go/pkgcore"
	// Importing the Redis- and NATS-backed seam packages registers their
	// implementations on pkgcore's shared registries (each init() does the
	// registration) -- the same imports a host making such a Preset must
	// carry. The named imports reach the concrete packages' exported
	// sentinels; the blank ones make the registration side effect explicit.
	_ "github.com/vislake/speed/go/pkgcore/eventbus/nats"
	natsbus "github.com/vislake/speed/go/pkgcore/eventbus/nats"
	_ "github.com/vislake/speed/go/pkgcore/eventbus/redis"
	redisbus "github.com/vislake/speed/go/pkgcore/eventbus/redis"
	"github.com/vislake/speed/go/pkgcore/internal/testutil"
	_ "github.com/vislake/speed/go/pkgcore/kv/nats"
	_ "github.com/vislake/speed/go/pkgcore/kv/redis"
)

// lifecycleProbeModule is a minimal Module whose Register subscribes a
// counting handler on the registry's bus -- Subscribe is the operation that
// makes a broker-backed bus start using its connection (its readers dial
// the server).
type lifecycleProbeModule struct {
	deliveries *atomic.Int64
}

func (lifecycleProbeModule) Name() string         { return "lifecycleprobe" }
func (lifecycleProbeModule) DependsOn() []string  { return nil }
func (lifecycleProbeModule) Migrations() embed.FS { return embed.FS{} }
func (lifecycleProbeModule) Locales() embed.FS    { return embed.FS{} }
func (lifecycleProbeModule) OpenAPISpec() []byte  { return nil }

func (m lifecycleProbeModule) Register(reg *pkgcore.Registry) error {
	reg.Events.Subscribe("kernel.lifecycle.probe", func(context.Context, pkgcore.Event) error {
		m.deliveries.Add(1)
		return nil
	})
	return nil
}

// redisPreset names the Redis-backed composition the Redis leg bootstraps:
// both distributed-mode implementations under test, plus the in-process
// mailer and local object store for the two seams this test is not about.
func redisPreset() pkgcore.Preset {
	return pkgcore.Preset{
		"eventbus":    "eventbus.redis",
		"kv":          "kv.redis",
		"mailer":      "mailer.console",
		"objectstore": "objectstore.local",
	}
}

// natsPreset names the NATS-backed composition the NATS legs bootstrap.
func natsPreset() pkgcore.Preset {
	return pkgcore.Preset{
		"eventbus":    "eventbus.nats",
		"kv":          "kv.nats",
		"mailer":      "mailer.console",
		"objectstore": "objectstore.local",
	}
}

// startRedisOnDefaultPort starts a disposable Redis 7 container with its
// client port published to the host's 6379 -- the address the
// zero-configuration "kv.redis"/"eventbus.redis" constructors dial (see the
// package doc comment) -- and returns an admin client on the same address
// for server-side connection counting. Client and container are cleaned up
// via t.Cleanup.
func startRedisOnDefaultPort(t *testing.T, ctx context.Context) *redis.Client {
	t.Helper()

	// Host port 6379 is this fixture's contract, not a choice: the
	// zero-configuration constructors under test dial localhost:6379, so
	// the pin cannot move to a free port the way the restart fixtures'
	// pins can (free_host_port.go's doc comment). A genuinely occupied
	// 6379 -- a local redis, or a concurrent run of this same tier on a
	// shared Docker host -- is therefore an environment fact this test
	// yields to rather than fails on, exactly the guard main_test.go's
	// healthcheck test applies to its own required literal port.
	if !testutil.HostPortFree("6379") {
		t.Skipf("host port 6379 is already bound and the zero-configuration seam constructors under test dial it; skipping this leg")
	}

	container, err := tcredis.Run(ctx, "redis:7-alpine",
		testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
			hc.PortBindings = network.PortMap{
				network.MustParsePort("6379/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "6379"}},
			}
		}),
	)
	if err != nil {
		t.Fatalf("start redis testcontainer on port 6379: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate redis testcontainer: %v", terminateErr)
		}
	})

	admin := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379"})
	t.Cleanup(func() { admin.Close() })
	if err := admin.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping redis testcontainer: %v", err)
	}
	return admin
}

// redisConnectedClients reads the server's connected_clients counter.
func redisConnectedClients(t *testing.T, admin *redis.Client) int {
	t.Helper()

	info, err := admin.Info(ctxOf(t), "clients").Result()
	if err != nil {
		t.Fatalf("INFO clients: %v", err)
	}
	for _, line := range strings.Split(info, "\r\n") {
		if v, ok := strings.CutPrefix(line, "connected_clients:"); ok {
			n, err := parseCount(v)
			if err != nil {
				t.Fatalf("parse connected_clients %q: %v", v, err)
			}
			return n
		}
	}
	t.Fatalf("INFO clients did not report connected_clients:\n%s", info)
	return 0
}

func parseCount(v string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &n)
	return n, err
}

// startNATSOnDefaultPort starts a disposable JetStream-enabled NATS
// container with its client port published to the host's 4222 (the address
// the zero-configuration "eventbus.nats"/"kv.nats" constructors dial -- see
// the package doc comment) and its HTTP monitoring port (started with
// -m 8222, which the nats-server CLI requires before /connz answers)
// published on a random host port. It returns the monitoring endpoint
// ("http://127.0.0.1:<port>") for server-side connection counting.
func startNATSOnDefaultPort(t *testing.T, ctx context.Context) string {
	t.Helper()

	// Host port 4222 is this fixture's contract, not a choice: the
	// zero-configuration constructors under test dial nats.DefaultURL
	// ("nats://127.0.0.1:4222"), so the pin cannot move to a free port the
	// way the restart fixtures' pins can (free_host_port.go's doc
	// comment). A genuinely occupied 4222 is an environment fact this test
	// yields to rather than fails on, exactly the guard
	// startRedisOnDefaultPort applies to its own required literal port.
	if !testutil.HostPortFree("4222") {
		t.Skipf("host port 4222 is already bound and the zero-configuration seam constructors under test dial it; skipping this leg")
	}

	// GenericContainer rather than the testcontainers nats module: the
	// module's own default command ("-DV -js") never starts the HTTP
	// monitoring server these legs count connections through.
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "nats:2.11.7-alpine",
			Cmd:          []string{"-DV", "-js", "-m", "8222"},
			ExposedPorts: []string{"4222/tcp", "8222/tcp"},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.PortBindings = network.PortMap{
					network.MustParsePort("4222/tcp"): {{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "4222"}},
				}
			},
			WaitingFor: wait.ForLog("Listening for client connections").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start nats testcontainer on port 4222: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate nats testcontainer: %v", terminateErr)
		}
	})

	// The monitoring port (8222/tcp inside the container) is published on a
	// random host port.
	mapped, err := container.MappedPort(ctx, "8222/tcp")
	if err != nil {
		t.Fatalf("nats monitoring port mapping: %v", err)
	}
	return fmt.Sprintf("http://127.0.0.1:%s", mapped.Port())
}

// natsConnCount returns the server's total current client connection count
// from its monitoring endpoint.
func natsConnCount(t *testing.T, monitor string) int {
	t.Helper()

	resp, err := http.Get(monitor + "/connz")
	if err != nil {
		t.Fatalf("GET %s/connz: %v", monitor, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /connz body: %v", err)
	}
	var out struct {
		NumConnections int `json:"num_connections"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode /connz body %q: %v", body, err)
	}
	return out.NumConnections
}

// waitForConnCount polls monitor until the server's connection count equals
// want, or within elapses.
func waitForConnCount(t *testing.T, monitor string, want int, within time.Duration) {
	t.Helper()

	deadline := time.Now().Add(within)
	for {
		got := natsConnCount(t, monitor)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("server connection count stayed %d, want %d within %v", got, want, within)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestKernel_Shutdown_RealRedisConnectionsReleased drives the successful
// half of the seam lifecycle against a real Redis server: Bootstrap
// resolves the Redis-backed seams from a Preset (each registration builds
// its own *redis.Client over the server), a module subscribes and an event
// is published -- the operations that make the client actually dial -- and
// Kernel.Shutdown must then release every connection: the server's
// connected-clients count returns to the admin client's baseline. Before
// the seam lifecycle existed nothing anywhere closed those clients, and the
// connections stayed open for the process's lifetime.
func TestKernel_Shutdown_RealRedisConnectionsReleased(t *testing.T) {
	ctx := context.Background()
	admin := startRedisOnDefaultPort(t, ctx)
	baseline := redisConnectedClients(t, admin)

	var deliveries atomic.Int64
	kernel := pkgcore.NewKernel(pkgcore.WithPreset(redisPreset()))
	reg, err := kernel.Bootstrap(ctx, lifecycleProbeModule{deliveries: &deliveries})
	if err != nil {
		t.Fatalf("Bootstrap() error = %v, want nil", err)
	}

	// Subscribe started the bus's readers; publishing forces an append and
	// a synchronous local delivery, so the registration-built client has
	// genuinely dialed by the time the count is taken.
	if err := reg.EventBus().Publish(ctx, pkgcore.Event{Type: "kernel.lifecycle.probe", Payload: "x"}); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}

	// The eventbus client (readers + this publish) must have dialed at
	// least one connection beyond the admin client's baseline.
	deadline := time.Now().Add(5 * time.Second)
	for redisConnectedClients(t, admin) <= baseline {
		if time.Now().After(deadline) {
			t.Fatalf("eventbus client never dialed: connected_clients stayed at baseline %d", baseline)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if deliveries.Load() != 1 {
		t.Errorf("handler deliveries = %d, want 1", deliveries.Load())
	}

	if err := kernel.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}

	// Every connection the registration-built seams dialed must be gone;
	// only the admin client's own baseline connection remains.
	deadline = time.Now().Add(10 * time.Second)
	for redisConnectedClients(t, admin) != baseline {
		if time.Now().After(deadline) {
			t.Fatalf("connected_clients = %d after Shutdown, want baseline %d: the registration-built Redis client was not closed", redisConnectedClients(t, admin), baseline)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// The closed bus answers further publishes with its closed sentinel.
	if err := reg.EventBus().Publish(ctx, pkgcore.Event{Type: "kernel.lifecycle.probe", Payload: "y"}); err == nil {
		t.Error("Publish() after Shutdown succeeded, want ErrEventBusClosed")
	} else if !strings.Contains(err.Error(), redisbus.ErrEventBusClosed.Error()) {
		t.Errorf("Publish() after Shutdown error = %v, want it to report the closed bus", err)
	}
}

// TestKernel_Shutdown_RealNATSConnectionsReleased drives the same
// successful half against a real NATS server: after Bootstrap resolves the
// NATS-backed seams (each registration dials its own live *nats.Conn), a
// module subscribes and an event is published, and Shutdown must close both
// registered connections -- the server's own monitoring endpoint counts
// them back down to zero, and the closed seam values fail their next
// operation.
func TestKernel_Shutdown_RealNATSConnectionsReleased(t *testing.T) {
	ctx := context.Background()
	monitor := startNATSOnDefaultPort(t, ctx)

	var deliveries atomic.Int64
	kernel := pkgcore.NewKernel(pkgcore.WithPreset(natsPreset()))
	reg, err := kernel.Bootstrap(ctx, lifecycleProbeModule{deliveries: &deliveries})
	if err != nil {
		t.Fatalf("Bootstrap() error = %v, want nil", err)
	}

	// Both seam constructors dial synchronously at Bootstrap, so the server
	// already holds two connections (bus + kv) by now.
	waitForConnCount(t, monitor, 2, 10*time.Second)

	if err := reg.EventBus().Publish(ctx, pkgcore.Event{Type: "kernel.lifecycle.probe", Payload: "x"}); err != nil {
		t.Fatalf("Publish() error = %v, want nil", err)
	}
	if err := reg.KVStore().Set(ctx, "kernel:lifecycle:probe", []byte("1"), 0); err != nil {
		t.Fatalf("KVStore().Set() error = %v, want nil", err)
	}
	if deliveries.Load() != 1 {
		t.Errorf("handler deliveries = %d, want 1", deliveries.Load())
	}

	if err := kernel.Shutdown(); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}

	// Both dialed connections must be closed: the server counts zero.
	waitForConnCount(t, monitor, 0, 10*time.Second)

	// And the closed seam values fail their next operations.
	if err := reg.EventBus().Publish(ctx, pkgcore.Event{Type: "kernel.lifecycle.probe", Payload: "y"}); err == nil {
		t.Error("Publish() after Shutdown succeeded, want ErrEventBusClosed")
	} else if !strings.Contains(err.Error(), natsbus.ErrEventBusClosed.Error()) {
		t.Errorf("Publish() after Shutdown error = %v, want it to report the closed bus", err)
	}
	if err := reg.KVStore().Set(ctx, "kernel:lifecycle:after", []byte("1"), 0); err == nil {
		t.Error("KVStore().Set() after Shutdown succeeded, want it to fail on the closed connection")
	}
}

// TestBootstrapFailure_RealNATSConnectionsClosed is the failure-path proof
// against a real NATS server: Bootstrap resolves "eventbus.nats" (a real
// dialed connection) and "kv.nats" (another), then fails resolving the
// mailer seam. The two already-resolved seams' connections must be closed
// before the error is returned -- the server's monitoring endpoint counts
// them back down to zero. Before the seam lifecycle existed, the dialed
// connections had no close path at all and leaked for the process's
// lifetime.
func TestBootstrapFailure_RealNATSConnectionsClosed(t *testing.T) {
	ctx := context.Background()
	monitor := startNATSOnDefaultPort(t, ctx)

	preset := natsPreset()
	preset["mailer"] = "test.lifecycle.no.such.mailer"
	kernel := pkgcore.NewKernel(pkgcore.WithPreset(preset))

	if _, err := kernel.Bootstrap(ctx); err == nil {
		t.Fatal("Bootstrap() succeeded, want the unknown mailer name to fail it")
	}

	// The two seams resolved before the failure (bus first, then kv) dialed
	// real connections; the failed Bootstrap must have closed them. The
	// server counts zero connections once the close lands.
	waitForConnCount(t, monitor, 0, 10*time.Second)
}

// ctxOf is a tiny helper giving the count pollers a context tied to the
// test's deadline.
func ctxOf(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}
