package nats

// construction.go carries this package's construction helpers: the exported
// capability declaration and the shared constructors the "eventbus.nats" component
// (component.go) builds from -- kept beside the implementation they adapt,
// the file-locality this package always had.
import (
	"io"

	"github.com/nats-io/nats.go"

	"github.com/vislake/speed/go/pkgcore"
)

// Capabilities is what "eventbus.nats" declares about itself: many replicas
// may share one NATS deployment, and the JetStream state behind the bus --
// messages committed to file-backed streams -- outlives any one process,
// exactly what the two bits mean. The component descriptor (component.go)
// declares this one exported value, and its component_test.go pins the
// descriptor's declaration to it, so the bits a host reads off this package
// and the assembly validates cannot drift apart.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

// closableEventBus is the value the "eventbus.nats" component's New returns:
// the bus itself (whose promoted methods satisfy pkgcore.EventBus) plus the
// Close() error method that releases the connection that New dialed, which
// the assembly's close stage runs since the component descriptor declares
// no Close callback. A host that calls NewEventBus itself gets the bare
// *EventBus and keeps owning its connection, exactly as that constructor's
// own doc comment promises; only the component-built value carries this
// closer.
type closableEventBus struct {
	*EventBus
	closeConn func()
}

// The product's Close() error is the ownership declaration the assembly's
// close stage reads, so the compile-time assertion keeps it from being
// dropped silently.
var _ io.Closer = (*closableEventBus)(nil)

// Close stops the bus and then releases the connection the component's New
// dialed.
func (b *closableEventBus) Close() error {
	b.EventBus.Close()
	if b.closeConn != nil {
		b.closeConn()
	}
	return nil
}

// newConn dials the *nats.Conn for the "eventbus.nats" settings the
// "eventbus.nats" component (component.go) resolves. The url fallback and
// the retry posture live here; this is the connection dial nats.Connect
// performs synchronously.
func newConn(url, token, user, password string) (*nats.Conn, error) {
	if url == "" {
		url = nats.DefaultURL
	}

	opts := []nats.Option{
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1), // never give up reconnecting in the background
	}
	if token != "" {
		opts = append(opts, nats.Token(token))
	}
	if user != "" {
		opts = append(opts, nats.UserInfo(user, password))
	}

	return nats.Connect(url, opts...)
}
