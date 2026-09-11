package nats

// construction.go carries this package's construction helpers: the exported
// capability declaration and the shared constructors the "kv.nats" component
// (component.go) builds from -- kept beside the implementation they adapt,
// the file-locality this package always had.
import (
	"fmt"

	"github.com/nats-io/nats.go"

	"github.com/vislake/speed/go/pkgcore"
)

// defaultBucket is the JetStream KV bucket name a zero-configuration Preset
// falls back to -- the "kv.nats" twin of kv/redis's "localhost:6379" default
// address.
const defaultBucket = "speed-kv"

// Capabilities is what "kv.nats" declares about itself: many replicas may
// share one NATS deployment, and the JetStream KV buckets the store writes
// to (file storage, the same configuration NewKVStore provisions and
// adopts) outlive any one process -- exactly what the two bits mean. The
// built-in registration below and the Registration factory a host wraps a
// self-built connection in both declare this one exported value, so the
// declaration a host reads off this package and the one assembly validates
// cannot drift apart. A host injecting a hand-built store with
// pkgcore.WithKVStore passes it as the injection's capability argument.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

// closableKVStore is the value "kv.nats"'s registration returns: the store
// itself (whose promoted methods satisfy pkgcore.KVStore) plus the Close()
// error method that releases the connection the registration dialed, per
// the Registration-level resource-ownership contract. A host that calls
// NewKVStore itself gets the bare store and keeps owning its connection,
// exactly as that constructor's own doc comment promises; only the
// preset-built value carries the registration's closer.
type closableKVStore struct {
	pkgcore.KVStore
	closeConn func()
}

// Close releases the connection the registration dialed.
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

// natsBucketOrDefault applies the bucket fallback shared by both
// configuration channels: an empty bucket resolves to defaultBucket, so a
// zero-configuration build and the component's own default agree on the
// bucket a store provisions.
func natsBucketOrDefault(bucket string) string {
	if bucket != "" {
		return bucket
	}
	return defaultBucket
}

// connFromConfig builds and connects the *nats.Conn "kv.nats" adapts onto
// NewKVStore. Unlike kv/redis's clientFromConfig, which builds a client
// without dialing (go-redis connects lazily), nats.Connect dials
// synchronously, so this call is where the builtin seam's one real network
// round trip at construction happens -- credentials, if any, come from cfg;
// a host that needs a TLS config, a custom dial timeout, or any other
// *nats.Option this narrow cfg map cannot express bypasses the preset layer
// entirely with pkgcore.WithKVStore(natskv.NewKVStore(ctx, conn, bucket)).
//
// eventbus/nats, if and when it lands, is expected to carry its own copy of
// an equivalent helper rather than share this one: the two packages are
// independent implementations of different seams, and neither owns the
// other, so duplicating a dozen lines is cheaper than inventing a third
// package for both to depend on (the same deliberate duplication
// kv/redis's own clientFromConfig comment argues for its eventbus/redis
// counterpart).
func connFromConfig(cfg pkgcore.Config) (*nats.Conn, error) {
	return newConn(cfg["url"], cfg["user"], cfg["password"], cfg["token"])
}

// newConn dials the *nats.Conn for the "kv.nats" settings both configuration
// channels resolve to: the flat pkgcore.Config adapter above and the
// "kv.nats" component (component.go), whose typed configuration carries the
// same four fields. The url fallback and the client name live here, so the
// two channels cannot drift on them; this is the one real network round
// trip at construction, as connFromConfig's own doc comment explains.
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
