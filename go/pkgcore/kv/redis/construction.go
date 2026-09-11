package redis

// construction.go carries this package's construction helpers: the exported
// capability declaration and the shared constructors the "kv.redis" component
// (component.go) builds from -- kept beside the implementation they adapt,
// the file-locality this package always had.
import (
	"github.com/redis/go-redis/v9"

	"github.com/vislake/speed/go/pkgcore"
)

// Capabilities is what "kv.redis" declares about itself: many replicas may
// share one Redis deployment, and the key/value state the store writes
// outlives any one process (Redis's own AOF/RDB persistence is the service
// premise, the same one the distributed deployment mode's Redis-backed
// event bus relies on). The built-in registration below and the Registration
// factory a host wraps a self-built client in both declare this one exported
// value, so the declaration a host reads off this package and the one
// assembly validates cannot drift apart. A host injecting a hand-built store
// through the by-type context passes it as the value's capability declaration.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

// closableKVStore is the value "kv.redis"'s registration returns: the store
// itself (whose promoted methods satisfy pkgcore.KVStore) plus the Close()
// error method that releases the client the registration built, per the
// Registration-level resource-ownership contract. A host that calls
// NewKVStore itself gets the bare store and keeps owning its client,
// exactly as that constructor's own doc comment promises; only the
// preset-built value carries the registration's closer.
type closableKVStore struct {
	pkgcore.KVStore
	closeClient func() error
}

// Close releases the client the registration built.
func (s *closableKVStore) Close() error {
	if s.closeClient != nil {
		return s.closeClient()
	}
	return nil
}

// newClient builds the go-redis client for the "kv.redis" settings both
// configuration channels resolve to: the flat pkgcore.Config adapter above
// and the "kv.redis" component (component.go), whose typed configuration
// carries the same three fields. The addr fallback lives here, so the two
// channels cannot drift on it; nothing is dialed, per NewKVStore's own
// construction contract.
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
