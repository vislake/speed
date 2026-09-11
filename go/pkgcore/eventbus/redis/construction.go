package redis

// construction.go carries this package's construction helpers: the exported
// capability declaration and the shared constructors the "eventbus.redis" component
// (component.go) builds from -- kept beside the implementation they adapt,
// the file-locality this package always had.
import (
	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
)

// Capabilities is what "eventbus.redis" declares about itself: many replicas
// may share one Redis deployment, and the Redis Streams state the bus writes
// outlives any one process, exactly what the two bits mean. The built-in
// registration below and the Registration factory a host wraps a self-built
// client in both declare this one exported value, so the declaration a host
// reads off this package and the one assembly validates cannot drift apart.
// A host injecting a hand-built bus does so through the by-type context, passing it as
// the injection's capability argument.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

// closableEventBus is the value "eventbus.redis"'s registration returns: the
// bus itself (whose promoted methods satisfy pkgcore.EventBus) plus the
// Close() error method that releases the client the registration built, per
// the Registration-level resource-ownership contract. A host that calls NewEventBus itself gets the bare *EventBus
// and keeps owning its client, exactly as that constructor's own doc
// comment promises; the component's own value carries this closer.
type closableEventBus struct {
	*EventBus
	closeClient func() error
}

// Close stops the bus and then releases the client the registration built.
func (b *closableEventBus) Close() error {
	b.EventBus.Close()
	if b.closeClient != nil {
		return b.closeClient()
	}
	return nil
}

// newClient builds the go-redis client for the "eventbus.redis" settings
// both configuration channels resolve to: the flat pkgcore.Config adapter
// above and the "eventbus.redis" component (component.go), whose typed
// configuration carries the same three fields. The addr fallback lives
// here, so the two channels cannot drift on it; nothing is dialed, per
// NewEventBus's own construction contract.
func newClient(addr, password string, db int) *redis.Client {
	if addr == "" {
		addr = "localhost:6379"
	}
	return redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: password,
		DB:       db,
	})
}
