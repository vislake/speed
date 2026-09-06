//go:build integration

// Package memcached_test holds go/pkgcore/kv/memcached's integration tier:
// tests that exercise KVStore against a real Memcached server. It is
// physically separate from the package's unit tests (all of which live in
// package memcached itself, one file per source file, per the backend
// coding standard's testing layout rule) and carries the "integration" build
// tag: a plain "go test ./..." never compiles or runs anything in this
// directory; it is invoked explicitly with "go test -tags=integration
// ./...". This mirrors kv/redis's own integration_test/ layout exactly.
//
// Every test here spins up its own disposable Memcached container and
// requires a working Docker (or Docker-API-compatible) daemon; there is no
// fallback or skip-on-missing-Docker path.
package memcached_test

import (
	"context"
	"testing"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/testcontainers/testcontainers-go"
	tcmemcached "github.com/testcontainers/testcontainers-go/modules/memcached"
)

// startMemcachedClient starts a disposable Memcached container and returns a
// gomemcache client connected to it, the client's underlying connections and
// the container already cleaned up via t.Cleanup on test completion (pass or
// fail), so nothing leaks past its owning test. Every integration test calls
// this for its own container, keeping tests isolated from one another at the
// cost of a few seconds of startup each -- the same trade kv/redis's own
// startRedisClient makes.
func startMemcachedClient(t *testing.T, ctx context.Context) *memcache.Client {
	t.Helper()

	container, err := tcmemcached.Run(ctx, "memcached:1.6-alpine")
	if err != nil {
		t.Fatalf("start memcached testcontainer: %v", err)
	}
	t.Cleanup(func() {
		if terminateErr := testcontainers.TerminateContainer(container); terminateErr != nil {
			t.Errorf("terminate memcached testcontainer: %v", terminateErr)
		}
	})

	hostPort, err := container.HostPort(ctx)
	if err != nil {
		t.Fatalf("memcached testcontainer host port: %v", err)
	}
	client := memcache.New(hostPort)
	t.Cleanup(func() { client.Close() })
	return client
}
