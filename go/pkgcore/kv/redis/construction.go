package redis

// construction.go carries this package's construction helpers: the exported
// capability declaration and the shared constructors the "kv.redis" component
// (component.go) builds from -- kept beside the implementation they adapt,
// the file-locality this package always had.
import (
	"io"

	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
)

// Capabilities is what "kv.redis" declares about itself: many replicas may
// share one Redis deployment, and the key/value state the store writes
// outlives any one process (Redis's own AOF/RDB persistence is the service
// premise, the same one the distributed deployment mode's Redis-backed
// event bus relies on). The component descriptor (component.go) declares
// this one exported value, and its component_test.go pins the descriptor's
// declaration to it, so the bits a host reads off this package and the
// assembly validates cannot drift apart.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

// closableKVStore is the value the "kv.redis" component's New returns: the
// store itself (whose promoted methods satisfy pkgcore.KVStore) plus the
// Close() error method that releases the client that New built, which the
// assembly's close stage runs since the component descriptor declares no
// Close callback. A host that calls NewKVStore itself gets the bare store
// and keeps owning its client, exactly as that constructor's own doc
// comment promises; only the component-built value carries this closer.
type closableKVStore struct {
	pkgcore.KVStore
	closeClient func() error
}

// The product's Close() error is the ownership declaration the assembly's
// close stage reads, so the compile-time assertion keeps it from being
// dropped silently.
var _ io.Closer = (*closableKVStore)(nil)

// Close releases the client the component's New built.
func (s *closableKVStore) Close() error {
	if s.closeClient != nil {
		return s.closeClient()
	}
	return nil
}

// newClient builds the go-redis client for the "kv.redis" settings the
// component (component.go) resolves: its typed configuration carries these
// three fields. The addr fallback lives here; nothing is dialed, per
// NewKVStore's own construction contract.
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
