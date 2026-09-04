//go:build integration

// Package redis_test holds go/pkgcore/eventbus/redis's integration tier:
// tests that exercise EventBus against a real Redis server. It is physically
// separate from the package's unit tests (all of which live in package
// redis itself, one file per source file, per the backend coding standard's
// testing layout rule) and carries the "integration" build tag: a plain
// "go test ./..." never compiles or runs anything in this directory; it is
// invoked explicitly with "go test -tags=integration ./...". This mirrors
// go/pkgcore's own (pre-split) integration tier, which this directory's
// tests are moved out of, and go/jobs/integration_test's identical
// convention.
//
// Every test here spins up its own disposable Redis container and requires
// a working Docker (or Docker-API-compatible) daemon; there is no fallback
// or skip-on-missing-Docker path.
package redis_test

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
//
// kv/redis's own integration tier carries an identical copy of this helper:
// the two packages' integration tiers are independent of each other and
// neither owns a shared package worth introducing just for a dozen lines
// (go/pkgcore/AGENTS.md's packaging rule, the same reasoning behind
// register.go's duplicated clientFromConfig).
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
