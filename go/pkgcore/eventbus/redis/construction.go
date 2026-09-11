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
// outlives any one process, exactly what the two bits mean. The component
// descriptor (component.go) declares this one exported value, and its
// component_test.go pins the descriptor's declaration to it, so the bits a
// host reads off this package and the assembly validates cannot drift apart.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

// closableEventBus is the value the "eventbus.redis" component's New
// returns: the bus itself (whose promoted methods satisfy pkgcore.EventBus)
// plus the Close() error method that releases the client that New built, and
// the component's own Close callback releases it through this method. A host
// that calls NewEventBus itself gets the bare *EventBus and keeps owning its
// client, exactly as that constructor's own doc comment promises; the
// component-built value carries this closer.
type closableEventBus struct {
	*EventBus
	closeClient func() error
}

// Close stops the bus and then releases the client the component's New built.
func (b *closableEventBus) Close() error {
	b.EventBus.Close()
	if b.closeClient != nil {
		return b.closeClient()
	}
	return nil
}

// newClient builds the go-redis client for the "eventbus.redis" settings the
// component (component.go) resolves: its typed configuration carries these
// three fields. The addr fallback lives here; nothing is dialed, per
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
