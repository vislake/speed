package eventbustest

// A second in-package driver of the shared suite, this time with the
// cross-instance checks enabled: two eventbus/redis instances sharing one
// miniredis server are a genuinely two-replica deployment (each instance
// has its own consumer group on every stream, and delivery between them
// travels over real Redis Streams wire round trips), so AssertConforms can
// run against them under the MultiReplicaSafe declaration the
// implementation honestly carries -- the same suite shape this package's
// Redis integration leg runs against a real container, here in-process.

import (
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
	redisbus "github.com/vislake/speed/go/pkgcore/eventbus/redis"
)

// TestAssertConforms_RedisBusPairOverMiniredis proves AssertConforms passes
// end to end -- cross-instance checks included -- against a pair of
// eventbus/redis instances sharing one miniredis server, the honest
// two-replica model of a deployment whose "MultiReplicaSafe" declaration
// this pair actually satisfies.
func TestAssertConforms_RedisBusPairOverMiniredis(t *testing.T) {
	mini := miniredis.RunT(t)

	var (
		mu      sync.Mutex
		pairs   []*redisbus.EventBus
		clients []*redis.Client
	)
	factory := func() (pkgcore.EventBus, pkgcore.EventBus) {
		clientA := redis.NewClient(&redis.Options{Addr: mini.Addr()})
		clientB := redis.NewClient(&redis.Options{Addr: mini.Addr()})
		busA := redisbus.NewEventBus(clientA)
		busB := redisbus.NewEventBus(clientB)
		mu.Lock()
		pairs = append(pairs, busA, busB)
		clients = append(clients, clientA, clientB)
		mu.Unlock()
		return busA, busB
	}
	t.Cleanup(func() {
		mu.Lock()
		defer mu.Unlock()
		for _, bus := range pairs {
			bus.Close()
		}
		for _, client := range clients {
			_ = client.Close()
		}
	})

	AssertConforms(t, pkgcore.MultiReplicaSafe, factory)
}
