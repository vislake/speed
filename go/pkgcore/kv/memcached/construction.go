package memcached

// construction.go carries this package's construction helpers: the exported
// capability declaration and the shared constructors the "kv.memcached" component
// (component.go) builds from -- kept beside the implementation they adapt,
// the file-locality this package always had.
import (
	"github.com/bradfitz/gomemcache/memcache"
	"github.com/vislake/speed/go/pkgcore"
	"strings"
)

// Capabilities is what "kv.memcached" declares about itself: honestly
// MultiReplicaSafe alone, because many processes sharing one Memcached
// deployment see the same state, while Memcached has no persistence
// mechanism of any kind, so SurvivesRestart is deliberately absent -- see
// the package doc comment. The built-in registration below and the
// Registration factory a host wraps a self-built client in both declare
// this one exported value, so the declaration a host reads off this package
// and the one assembly validates cannot drift apart. A host injecting a
// hand-built store with pkgcore.WithKVStore passes it as the injection's
// capability argument.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe

// closableKVStore is the value "kv.memcached"'s registration returns: the
// store itself (whose promoted methods satisfy pkgcore.KVStore) plus the
// Close() error method that releases the client the registration built, per
// the Registration-level resource-ownership contract. A host that calls
// NewKVStore itself gets the bare store and keeps owning its client, exactly
// as that constructor's own doc comment promises; only the preset-built
// value carries the registration's closer. Close releases the client's idle
// pooled connections -- gomemcache dials per operation, so there is no other
// long-lived state to stop.
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

// newClient builds the *memcache.Client for the "kv.memcached" settings both
// configuration channels resolve to: the flat pkgcore.Config adapter above
// and the "kv.memcached" component (component.go), whose typed
// configuration carries the same comma-separated addrs spelling. The
// fallback address and the sharding list live here, so the two channels
// cannot drift on them; nothing is dialed, per gomemcache's own per-operation
// laziness (see clientFromConfig's doc comment above).
func newClient(addrs string) *memcache.Client {
	if addrs == "" {
		addrs = "localhost:11211"
	}
	servers := strings.Split(addrs, ",")
	for i, s := range servers {
		servers[i] = strings.TrimSpace(s)
	}
	return memcache.New(servers...)
}
