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
//     count to its baseline (without the seam lifecycle, nothing anywhere
//     closes the client the registration built, so those connections would
//     stay open for the process's lifetime);
//   - the same shape over a real NATS server must drop the server's
//     connection count to zero on its own monitoring endpoint, and the
//     closed seam values must fail their next operation;
//   - a Bootstrap that fails after resolving real NATS seams must close
//     the connections it already dialed before returning the error
//     (without the failure-path close, the dialed connections would have
//     no close path at all).
//
// # The containers' own addresses travel through the preset channel
//
// Each leg's preset entries carry the disposable container's mapped address
// (and its NATS URL) as the entry's Config -- the same channel a host's own
// configuration layer resolves and hands a Kernel over through -- so every
// container publishes its client port on a free host port and nothing here
// needs a fixed one. That makes these legs the end-to-end proof of the
// preset parameter channel as well: the "eventbus.redis"/"kv.redis" and
// "eventbus.nats"/"kv.nats" registrations build their clients from the
// Config values supplied here, never from their own "localhost:6379" /
// nats.DefaultURL zero-configuration fallbacks, and the connection counts
// below are read off the very server those Config values name.
package pkgcore_test

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func (m lifecycleProbeModule) Register(reg pkgcore.Registrar) error {
	reg.EventsSeat().Subscribe("kernel.lifecycle.probe", func(context.Context, pkgcore.Event) error {
		m.deliveries.Add(1)
		return nil
	})
	return nil
}

// redisPreset names the Redis-backed composition the Redis leg bootstraps:
// both distributed-mode implementations under test, each carrying the
// container's own address in its entry's Config, plus the in-process mailer
// and local object store for the two seams this test is not about.
func redisPreset(addr string) pkgcore.Preset {
	cfg := pkgcore.Config{"addr": addr}
	return pkgcore.Preset{
		"eventbus":    pkgcore.SeamPreset{Implementation: "eventbus.redis", Config: cfg},
		"kv":          pkgcore.SeamPreset{Implementation: "kv.redis", Config: cfg},
		"mailer":      pkgcore.SeamPreset{Implementation: "mailer.console"},
		"objectstore": pkgcore.SeamPreset{Implementation: "objectstore.local"},
	}
}

// natsPreset names the NATS-backed composition the NATS legs bootstrap, with
// each NATS-backed entry carrying the container's client URL in its Config.
func natsPreset(url string) pkgcore.Preset {
	cfg := pkgcore.Config{"url": url}
	return pkgcore.Preset{
		"eventbus":    pkgcore.SeamPreset{Implementation: "eventbus.nats", Config: cfg},
		"kv":          pkgcore.SeamPreset{Implementation: "kv.nats", Config: cfg},
		"mailer":      pkgcore.SeamPreset{Implementation: "mailer.console"},
		"objectstore": pkgcore.SeamPreset{Implementation: "objectstore.local"},
	}
}

// startRedis starts a disposable Redis 7 container on a free host port and
// returns an admin client on the container's mapped address -- for
// server-side connection counting -- together with the preset naming both
// Redis-backed seams at that same address (see the package doc comment:
// the address travels through the preset channel). Container and client are
// cleaned up via t.Cleanup.
func startRedis(t *testing.T, ctx context.Context) (*redis.Client, pkgcore.Preset) {
	t.Helper()

	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("start redis testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate redis testcontainer: %v", terminateErr)
		}
	})

	mapped, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("redis port mapping: %v", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%s", mapped.Port())

	admin := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { admin.Close() })
	if err := admin.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping redis testcontainer at %s: %v", addr, err)
	}
	return admin, redisPreset(addr)
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

// startNATS starts a disposable JetStream-enabled NATS container on a free
// host port for the client and a free one for its HTTP monitoring endpoint
// (started with -m 8222, which the nats-server CLI requires before /connz
// answers). It returns the monitoring endpoint ("http://127.0.0.1:<port>")
// for server-side connection counting together with the preset naming both
// NATS-backed seams at the container's own client URL (see the package doc
// comment: the URL travels through the preset channel).
func startNATS(t *testing.T, ctx context.Context) (string, pkgcore.Preset) {
	t.Helper()

	// GenericContainer rather than the testcontainers nats module: the
	// module's own default command ("-DV -js") never starts the HTTP
	// monitoring server these legs count connections through.
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        "nats:2.11.7-alpine",
			Cmd:          []string{"-DV", "-js", "-m", "8222"},
			ExposedPorts: []string{"4222/tcp", "8222/tcp"},
			WaitingFor:   wait.ForLog("Listening for client connections").WithStartupTimeout(90 * time.Second),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start nats testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate nats testcontainer: %v", terminateErr)
		}
	})

	// Both ports (the 4222/tcp client port and the 8222/tcp monitoring port
	// inside the container) are published on free host ports.
	clientMapped, err := container.MappedPort(ctx, "4222/tcp")
	if err != nil {
		t.Fatalf("nats client port mapping: %v", err)
	}
	monitorMapped, err := container.MappedPort(ctx, "8222/tcp")
	if err != nil {
		t.Fatalf("nats monitoring port mapping: %v", err)
	}
	return fmt.Sprintf("http://127.0.0.1:%s", monitorMapped.Port()),
		natsPreset(fmt.Sprintf("nats://127.0.0.1:%s", clientMapped.Port()))
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
// resolves the Redis-backed seams from a Preset whose entries carry the
// container's own address (each registration builds its own *redis.Client
// over that address), a module subscribes and an event is published -- the
// operations that make the client actually dial -- and Kernel.Shutdown must
// then release every connection: the server's connected-clients count
// returns to the admin client's baseline. Before the seam lifecycle existed
// nothing anywhere closed those clients, and the connections stayed open
// for the process's lifetime.
func TestKernel_Shutdown_RealRedisConnectionsReleased(t *testing.T) {
	ctx := context.Background()
	admin, preset := startRedis(t, ctx)
	baseline := redisConnectedClients(t, admin)

	var deliveries atomic.Int64
	kernel := pkgcore.NewKernel(pkgcore.WithPreset(preset))
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
// NATS-backed seams from a Preset whose entries carry the container's own
// URL (each registration dials its own live *nats.Conn over it), a module
// subscribes and an event is published, and Shutdown must close both
// registered connections -- the server's own monitoring endpoint counts
// them back down to zero, and the closed seam values fail their next
// operation.
func TestKernel_Shutdown_RealNATSConnectionsReleased(t *testing.T) {
	ctx := context.Background()
	monitor, preset := startNATS(t, ctx)

	var deliveries atomic.Int64
	kernel := pkgcore.NewKernel(pkgcore.WithPreset(preset))
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
// them back down to zero. Without the failure-path close, the dialed
// connections would have no close path at all and would leak for the
// process's lifetime.
func TestBootstrapFailure_RealNATSConnectionsClosed(t *testing.T) {
	ctx := context.Background()
	monitor, preset := startNATS(t, ctx)

	preset["mailer"] = pkgcore.SeamPreset{Implementation: "test.lifecycle.no.such.mailer"}
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
