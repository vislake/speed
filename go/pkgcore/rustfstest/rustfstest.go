// Package rustfstest provides disposable RustFS containers for
// integration tests: any module's tests (or a consuming application's)
// that exercise an S3-backed ObjectStore against a real server build
// their fixture through this package, so the image pin, the server's
// credential and health-check contract and the cleanup discipline live in
// one place and every tier exercises the same server behavior.
//
// RustFS (https://github.com/rustfs/rustfs) is the S3-API-compatible
// server this repository's tests run against; it has no dedicated
// testcontainers-go module the way MinIO does (tcminio), so the container
// is started through testcontainers' own generic ContainerRequest
// instead. The package deliberately holds nothing but container
// plumbing -- the objects a tier stores and the assertions it makes stay
// in that tier.
//
// Every constructor starts its own container and terminates it via
// t.Cleanup when the test completes, pass or fail, so nothing leaks past
// its owning test. Callers need a working Docker (or Docker-API-
// compatible) daemon, and these constructors carry no
// skip-on-missing-Docker path: a tier that spins up real infrastructure
// fails loudly when it is missing rather than passing vacuously. (This
// package's own unit suite probes for a daemon and skips instead, so a
// plain `go test ./...` run without Docker stays green.)
package rustfstest

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/internal/testutil"
)

// Image pins the RustFS release every container in this package runs to
// the same tag the repository's Docker-backed S3-compatible tiers
// standardize on, so a behavior any tier observes is a behavior of the
// same server.
const Image = "rustfs/rustfs:1.0.0-rc.5"

// AccessKey and SecretKey are RustFS's own documented default root
// credential values (docs.rustfs.com's Docker installation page), set
// into the container explicitly via RUSTFS_ACCESS_KEY/RUSTFS_SECRET_KEY
// rather than relied on as a default, and handed back to callers so a
// client or a subprocess environment can present them.
const (
	AccessKey = "rustfsadmin"
	SecretKey = "rustfsadmin"
)

// Start starts a disposable RustFS container and returns the endpoint
// ("host:port", no scheme -- what objectstore/s3.Config.Endpoint and an
// APP_S3_ENDPOINT-style variable both want), the container already
// cleaned up via t.Cleanup when the test completes (pass or fail). The
// container is created with its RustFS default root credentials and its
// own documented health check (a plain HTTP GET against /health on the
// S3 API port) as the startup wait.
func Start(t *testing.T, ctx context.Context) string {
	t.Helper()

	started, err := startContainer(ctx, nil)
	if err != nil {
		t.Fatalf("start rustfs testcontainer: %v", err)
	}
	terminateOnCleanup(t, started)
	return endpointOf(t, ctx, started)
}

// StartPersistent is Start's counterpart for tests that stop and restart
// their own container mid-test (a survives-restart proof): the S3 port is
// published to a fixed, rather than random, host port, and the container
// itself is returned alongside the endpoint. testcontainers' own random
// host-port allocation is re-resolved on every container start, and a
// minio client only ever redials addresses it already knows, so a stable
// advertised address is what makes the post-restart client call
// deterministic (the retry loop that keeps the pin collision-free is
// testutil.StartOnFreeHostPort's). RustFS stores the bucket and every
// object under the /data directory the fixture mounts as its Cmd
// argument, inside the container's own filesystem -- which survives a
// stop/start of the same container the way a real S3 service's durable
// storage survives a service restart.
func StartPersistent(t *testing.T, ctx context.Context) (testcontainers.Container, string) {
	t.Helper()

	var started testcontainers.Container
	hostPort := testutil.StartOnFreeHostPort(t, "rustfs testcontainer", func(p string) error {
		var err error
		started, err = startContainer(ctx, func(hc *container.HostConfig) {
			hc.PortBindings = network.PortMap{
				network.MustParsePort("9000/tcp"): {{HostIP: netip.IPv4Unspecified(), HostPort: p}},
			}
		})
		return err
	})
	terminateOnCleanup(t, started)
	return started, net.JoinHostPort("127.0.0.1", hostPort)
}

// NewClient builds a minio-go client for endpoint with the package's root
// credentials and TLS off, the shape every fixture here serves.
func NewClient(t *testing.T, endpoint string) *minio.Client {
	t.Helper()

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(AccessKey, SecretKey, ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("build a minio-go client for %q: %v", endpoint, err)
	}
	return client
}

// CreateBucket creates bucket on the server. Provisioning a bucket is a
// hosting operation a fixture performs, not something the store under
// test does: objectstore/s3 deliberately never provisions its own.
func CreateBucket(t *testing.T, ctx context.Context, client *minio.Client, bucket string) {
	t.Helper()

	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("create bucket %q on the rustfs testcontainer: %v", bucket, err)
	}
}

// WaitReady polls the container's /health endpoint until the server
// answers, or fails the test once the deadline passes. A restart closure
// must not return before the server is up, or the post-restart read would
// fail on a dead connection rather than on genuinely lost data.
// Accepting connections is only the first half of readiness though: see
// WaitReadsReady, which a restart closure must also pass before the
// survives-restart proof's own read runs.
func WaitReady(t *testing.T, ctx context.Context, endpoint string) {
	t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	url := "http://" + endpoint + "/health"
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err == nil {
			if resp, err := http.DefaultClient.Do(req); err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("restarted rustfs server did not answer /health within 30s")
}

// WaitReadsReady polls the restarted S3 service through store until it
// answers a read authoritatively, or fails the test once the deadline
// passes. The /health endpoint turns green before RustFS finishes its
// asynchronous startup_finalization, so a read issued as soon as /health
// answers can still fail with "Service not ready: waiting for
// startup_finalization" -- a liveness answer from the process, not a
// verdict about the stored bytes, and a restart closure must wait past
// it: a post-restart read that fails on the service's own startup window
// would report lost data where none was lost. The probe is a GetObject
// of a key the fixture never wrote: an operational service answers it
// with pkgcore.ErrObjectNotFound (S3's NoSuchKey -- the storage layer
// answering for itself), while a service still in startup_finalization
// keeps failing it with the not-ready error.
func WaitReadsReady(t *testing.T, ctx context.Context, store pkgcore.ObjectStore) {
	t.Helper()

	const probeKey = "readiness-probe"
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		reader, err := store.GetObject(ctx, probeKey)
		switch {
		case err == nil:
			_ = reader.Close()
			return
		case errors.Is(err, pkgcore.ErrObjectNotFound):
			return
		default:
			lastErr = err
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("restarted rustfs server did not answer a read (an unwritten key's NoSuchKey) within 30s; last error: %v", lastErr)
}

// startContainer starts one RustFS container through testcontainers'
// generic ContainerRequest (no dedicated module exists for the image),
// applying modifyHostConfig when the caller needs a pinned port binding.
// The error is returned unclassified so the pinned-port caller can let
// testutil.StartOnFreeHostPort absorb a bind collision with a fresh
// draw; the fixed-port caller fails on it directly.
func startContainer(ctx context.Context, modifyHostConfig func(*container.HostConfig)) (testcontainers.Container, error) {
	req := testcontainers.ContainerRequest{
		Image:        Image,
		ExposedPorts: []string{"9000/tcp"},
		Env: map[string]string{
			"RUSTFS_ACCESS_KEY":     AccessKey,
			"RUSTFS_SECRET_KEY":     SecretKey,
			"RUSTFS_ADDRESS":        ":9000",
			"RUSTFS_CONSOLE_ENABLE": "false",
		},
		Cmd: []string{"/data"},
		// RustFS's own documented health check (docs.rustfs.com's Docker
		// installation page): a plain HTTP GET against /health on the S3
		// API port, verified directly against this image pin before
		// relying on it here.
		WaitingFor:         wait.ForHTTP("/health").WithPort("9000/tcp").WithStartupTimeout(60 * time.Second),
		HostConfigModifier: modifyHostConfig,
	}
	return testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
}

// terminateOnCleanup registers started's termination via t.Cleanup, so
// the container never leaks past its owning test (pass or fail).
func terminateOnCleanup(t *testing.T, started testcontainers.Container) {
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(started); terminateErr != nil {
			t.Errorf("terminate rustfs testcontainer: %v", terminateErr)
		}
	})
}

// endpointOf resolves the container's host and mapped S3 port into the
// "host:port" endpoint callers address.
func endpointOf(t *testing.T, ctx context.Context, started testcontainers.Container) string {
	t.Helper()

	host, err := started.Host(ctx)
	if err != nil {
		t.Fatalf("rustfs testcontainer host: %v", err)
	}
	mappedPort, err := started.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("rustfs testcontainer mapped port: %v", err)
	}
	return net.JoinHostPort(host, mappedPort.Port())
}
