//go:build integration

// Package pkgcore_test holds go/pkgcore's integration tier: tests that
// exercise the Redis-backed implementations of the KVStore and EventBus
// seams against a real Redis server and the S3-backed implementation of the
// ObjectStore seam against a real MinIO server. It is physically separate
// from
// go/pkgcore's unit tests (all of which live in package pkgcore itself, one
// file per source file, per the backend coding standard's testing layout
// rule) and carries the "integration" build tag: a plain "go test ./..."
// never compiles or runs anything in this directory; it is invoked
// explicitly with "go test -tags=integration ./...". This mirrors the
// identical convention of go/jobs/integration_test and
// go/dbkit/integration_test; the container lifecycle below follows
// go/jobs/integration_test/redis_container_test.go almost line for line.
//
// Every test here spins up its own disposable container -- a Redis for the
// KVStore and EventBus tests, a MinIO for the ObjectStore tests -- and
// requires a working Docker (or Docker-API-compatible) daemon; there is no
// fallback or skip-on-missing-Docker path, matching go/jobs's own
// integration tier.
package pkgcore_test

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
)

// startRedisClient starts a disposable Redis 7 container and returns a
// go-redis client connected to it, the client and the container already
// cleaned up via t.Cleanup on test completion (pass or fail), so nothing
// leaks past its owning test. Every integration test calls this for its own
// container, keeping tests isolated from one another at the cost of a few
// seconds of startup each.
func startRedisClient(t *testing.T, ctx context.Context) *redis.Client {
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

	uri, err := container.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("redis testcontainer connection string: %v", err)
	}
	options, err := redis.ParseURL(uri)
	if err != nil {
		t.Fatalf("redis.ParseURL(%q): %v", uri, err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { client.Close() })
	return client
}
