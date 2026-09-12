package nats

// construction.go carries this package's construction helpers: the exported
// capability declaration and the shared constructors the "kv.nats" component
// (component.go) builds from -- kept beside the implementation they adapt,
// the file-locality this package always had.
import (
	"fmt"
	"io"

	"github.com/nats-io/nats.go"

	"github.com/vislake/speed/go/pkgcore"
)

// defaultBucket is the JetStream KV bucket name a zero-configuration composition
// falls back to -- the "kv.nats" twin of kv/redis's "localhost:6379" default
// address.
const defaultBucket = "speed-kv"

// Capabilities is what "kv.nats" declares about itself: many replicas may
// share one NATS deployment, and the JetStream KV buckets the store writes
// to (file storage, the same configuration NewKVStore provisions and
// adopts) outlive any one process -- exactly what the two bits mean. The
// component descriptor (component.go) declares this one exported value, and
// its component_test.go pins the descriptor's declaration to it, so the
// bits a host reads off this package and the assembly validates cannot
// drift apart.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

// closableKVStore is the value the "kv.nats" component's New returns: the
// store itself (whose promoted methods satisfy pkgcore.KVStore) plus the
// Close() error method that releases the connection that New dialed, which
// the assembly's close stage runs since the component descriptor declares
// no Close callback. A host that calls NewKVStore itself gets the bare
// store and keeps owning its connection, exactly as that constructor's own
// doc comment promises; only the component-built value carries this closer.
type closableKVStore struct {
	pkgcore.KVStore
	closeConn func()
}

// The product's Close() error is the ownership declaration the assembly's
// close stage reads, so the compile-time assertion keeps it from being
// dropped silently.
var _ io.Closer = (*closableKVStore)(nil)

// Close releases the connection the component's New dialed.
func (s *closableKVStore) Close() error {
	if s.closeConn != nil {
		s.closeConn()
	}
	return nil
}

// natsURLOrDefault applies the zero-configuration URL fallback: an empty url
// resolves to nats.DefaultURL, the only sensible default for a seam a
// zero-configuration caller must still be able to build something for.
func natsURLOrDefault(url string) string {
	if url != "" {
		return url
	}
	return nats.DefaultURL
}

// natsBucketOrDefault applies the bucket fallback: an empty bucket resolves
// to defaultBucket, so a zero-configuration build and the component's own
// default agree on the bucket a store provisions.
func natsBucketOrDefault(bucket string) string {
	if bucket != "" {
		return bucket
	}
	return defaultBucket
}

// newConn dials the *nats.Conn the "kv.nats" component (component.go)
// adapts onto NewKVStore: its typed configuration carries the four fields
// below. The url fallback and the client name live here; this is the one
// real network round trip at construction, since nats.Connect dials
// synchronously (unlike kv/redis's lazily-connecting client). A host that
// needs a TLS config, a custom dial timeout or any other *nats.Option this
// narrow configuration cannot express calls kvnats.NewKVStore directly and
// puts the store into the assembly's by-type context.
func newConn(url, user, password, token string) (*nats.Conn, error) {
	url = natsURLOrDefault(url)

	opts := []nats.Option{nats.Name("speed-pkgcore-kv")}
	if user != "" {
		opts = append(opts, nats.UserInfo(user, password))
	}
	if token != "" {
		opts = append(opts, nats.Token(token))
	}

	conn, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect to nats at %q: %w", url, err)
	}
	return conn, nil
}
