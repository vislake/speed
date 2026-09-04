package redis

// Hermetic unit tests for the Redis-backed KVStore: everything here runs
// without a Redis server. Behaviour that needs a real server lives in the
// integration tier (integration_test/kv_test.go); what belongs here is what
// is local to the store itself -- a nil client is a wiring error reported at
// construction, and a cancelled context fails every operation with the
// context's error before any command reaches the wire.

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
)

// TestNewKVStore_PanicsOnNilClient pins that a nil client is a wiring error
// reported at construction, not a failure deferred to the first operation.
func TestNewKVStore_PanicsOnNilClient(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewKVStore(nil) did not panic, want it to")
		}
	}()
	NewKVStore(nil)
}

// TestKVStore_CancelledContext pins the contract that no operation runs on a
// cancelled context: every method returns the context's error instead of
// performing the operation, mirroring pkgcore's in-memory store for the
// distributed-mode store. The client points at a closed port (the
// 127.0.0.1:1 address this package's own example uses), so an operation that
// ignored the context and reached for the server would have to fail with a
// connection error, never the context's error this table asserts. go-redis
// honours a cancelled context before dialing too, so what the test pins is
// the store's early return as the public contract: a store that swapped in a
// context-less operation would turn it red.
func TestKVStore_CancelledContext(t *testing.T) {
	t.Parallel()

	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	t.Cleanup(func() { client.Close() })
	store := NewKVStore(client)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		op   func() error
	}{
		{"Get", func() error {
			_, _, err := store.Get(ctx, "k")
			return err
		}},
		{"Set", func() error {
			return store.Set(ctx, "k", []byte("v"), 0)
		}},
		{"Delete", func() error {
			return store.Delete(ctx, "k")
		}},
		{"IncrByFloat", func() error {
			_, err := store.IncrByFloat(ctx, "k", 1)
			return err
		}},
		{"CompareAndSwap", func() error {
			_, err := store.CompareAndSwap(ctx, "k", nil, []byte("v"))
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.op(); !errors.Is(err, context.Canceled) {
				t.Errorf("%s on a cancelled context error = %v, want context.Canceled", tt.name, err)
			}
		})
	}
}
