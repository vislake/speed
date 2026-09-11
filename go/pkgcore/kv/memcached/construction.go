package memcached

// construction.go carries this package's construction helpers: the exported
// capability declaration and the shared constructors the "kv.memcached" component
// (component.go) builds from -- kept beside the implementation they adapt,
// the file-locality this package always had.
import (
	"strings"

	"github.com/bradfitz/gomemcache/memcache"

	"github.com/vislake/speed/go/pkgcore"
)

// Capabilities is what "kv.memcached" declares about itself: honestly
// MultiReplicaSafe alone, because many processes sharing one Memcached
// deployment see the same state, while Memcached has no persistence
// mechanism of any kind, so SurvivesRestart is deliberately absent -- see
// the package doc comment. The component descriptor (component.go)
// declares this one exported value, and its component_test.go pins the
// descriptor's declaration to it, so the bits a host reads off this package
// and the assembly validates cannot drift apart.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe

// closableKVStore is the value the "kv.memcached" component's New returns:
// the store itself (whose promoted methods satisfy pkgcore.KVStore) plus the
// Close() error method that releases the client that New built, and the
// component's own Close callback releases it through this method. A host
// that calls NewKVStore itself gets the bare store and keeps owning its
// client, exactly as that constructor's own doc comment promises; only the
// component-built value carries this closer. Close releases the client's
// idle pooled connections -- gomemcache dials per operation, so there is no
// other long-lived state to stop.
type closableKVStore struct {
	pkgcore.KVStore
	closeClient func() error
}

// Close releases the client the component's New built.
func (s *closableKVStore) Close() error {
	if s.closeClient != nil {
		return s.closeClient()
	}
	return nil
}

// newClient builds the *memcache.Client for the "kv.memcached" settings the
// component (component.go) resolves: its typed configuration carries the
// same comma-separated addrs spelling. The fallback address and the sharding
// list live here; nothing is dialed, per gomemcache's own per-operation
// laziness.
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
