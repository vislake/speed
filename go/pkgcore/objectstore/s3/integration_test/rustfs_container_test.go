//go:build integration

// Package s3_test holds go/pkgcore/objectstore/s3's integration tier: tests
// that exercise ObjectStore against a real RustFS server. It is physically
// separate from the package's unit tests (all of which live in package s3
// itself, one file per source file, per the backend coding standard's
// testing layout rule) and carries the "integration" build tag: a plain
// "go test ./..." never compiles or runs anything in this directory; it is
// invoked explicitly with "go test -tags=integration ./...". This mirrors
// go/pkgcore's own (pre-split) integration tier, which this directory's
// tests are moved out of, and go/jobs/integration_test's identical
// convention.
//
// Every test here spins up its own disposable RustFS container and requires
// a working Docker (or Docker-API-compatible) daemon; there is no fallback
// or skip-on-missing-Docker path. RustFS (https://github.com/rustfs/rustfs)
// is the S3-API-compatible server this repository's own tests and demos run
// against; it has no dedicated testcontainers-go module the way MinIO does
// (tcminio), so the container is started through testcontainers' own
// generic ContainerRequest instead, exactly the shape this repository
// already uses for images with no dedicated module (see
// examples/reference-app/integration_test/distributed_mode_test.go's own
// mailhogImage doc comment for the identical pattern applied to Mailpit).
package s3_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/objectstore/s3"
)

// rustfsImage pins the RustFS image the ObjectStore integration tier runs
// against to the same release tag this repository standardizes on for
// every Docker-backed S3-compatible tier (see docker-compose.distributed.yml
// and go/storage/integration_test/rustfs_leg_test.go's identical constant).
const rustfsImage = "rustfs/rustfs:1.0.0-rc.5"

// startRustfsObjectStore starts a disposable RustFS container, creates a
// fresh bucket on it, and returns an S3-backed ObjectStore pointed at that
// bucket, the client and the container already cleaned up via t.Cleanup on
// test completion (pass or fail), so nothing leaks past its owning test.
// Every integration test calls this for its own container, keeping tests
// isolated from one another at the cost of a few seconds of startup each.
// The bucket is created here rather than by the store: provisioning a
// bucket is a hosting operation, and s3.NewObjectStore deliberately never
// provisions its own. RustFS's own documented default root credential
// value, rustfsadmin/rustfsadmin (docs.rustfs.com's Docker installation
// page), is set explicitly below via RUSTFS_ACCESS_KEY/RUSTFS_SECRET_KEY
// rather than relied on as a default.
func startRustfsObjectStore(t *testing.T, ctx context.Context) pkgcore.ObjectStore {
	t.Helper()

	req := testcontainers.ContainerRequest{
		Image:        rustfsImage,
		ExposedPorts: []string{"9000/tcp"},
		Env: map[string]string{
			"RUSTFS_ACCESS_KEY":     "rustfsadmin",
			"RUSTFS_SECRET_KEY":     "rustfsadmin",
			"RUSTFS_ADDRESS":        ":9000",
			"RUSTFS_CONSOLE_ENABLE": "false",
		},
		Cmd: []string{"/data"},
		// RustFS's own documented health check (docs.rustfs.com's Docker
		// installation page): a plain HTTP GET against /health on the S3
		// API port, verified directly against this image pin before relying
		// on it here.
		WaitingFor: wait.ForHTTP("/health").WithPort("9000/tcp").WithStartupTimeout(60 * time.Second),
	}
	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start rustfs testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate rustfs testcontainer: %v", terminateErr)
		}
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("rustfs testcontainer host: %v", err)
	}
	mappedPort, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("rustfs testcontainer mapped port: %v", err)
	}
	endpoint := fmt.Sprintf("%s:%s", host, mappedPort.Port())

	const bucket = "objects"
	const accessKey = "rustfsadmin"
	const secretKey = "rustfsadmin"
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("build a minio-go client for %q: %v", endpoint, err)
	}
	if err := client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("create bucket %q on the rustfs testcontainer: %v", bucket, err)
	}

	return s3.NewObjectStore(s3.Config{
		Endpoint:  endpoint,
		Bucket:    bucket,
		AccessKey: accessKey,
		SecretKey: secretKey,
	})
}
