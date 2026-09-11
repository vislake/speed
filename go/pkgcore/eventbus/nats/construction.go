package nats

// construction.go carries this package's construction helpers: the exported
// capability declaration and the shared constructors the "eventbus.nats" component
// (component.go) builds from -- kept beside the implementation they adapt,
// the file-locality this package always had.
import (
	"github.com/nats-io/nats.go"

	"github.com/vislake/speed/go/pkgcore"
)

// Capabilities is what "eventbus.nats" declares about itself: many replicas
// may share one NATS deployment, and the JetStream state behind the bus --
// messages committed to file-backed streams -- outlives any one process,
// exactly what the two bits mean. The built-in registration below and the
// Registration factory a host wraps a self-built connection in both declare
// this one exported value, so the declaration a host reads off this package
// and the one assembly validates cannot drift apart. A host injecting a
// hand-built bus with pkgcore.WithEventBus passes it as the injection's
// capability argument.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

// closableEventBus is the value "eventbus.nats"'s registration returns: the
// bus itself (whose promoted methods satisfy pkgcore.EventBus) plus the
// Close() error method that releases the connection the registration
// dialed, per the Registration-level resource-ownership contract. A host
// that calls NewEventBus itself gets the bare *EventBus and keeps owning
// its connection, exactly as that constructor's own doc comment promises;
// only the preset-built value carries the registration's closer.
type closableEventBus struct {
	*EventBus
	closeConn func()
}

// Close stops the bus and then releases the connection the registration
// dialed.
func (b *closableEventBus) Close() error {
	b.EventBus.Close()
	if b.closeConn != nil {
		b.closeConn()
	}
	return nil
}

// newConn dials the *nats.Conn for the "eventbus.nats" settings both
// configuration channels resolve to: the flat pkgcore.Config adapter above
// and the "eventbus.nats" component (component.go), whose typed
// configuration carries the same four fields. The url fallback and the
// retry posture live here, so the two channels cannot drift on them; this
// is the connection dial nats.Connect performs synchronously, as
// connFromConfig's own doc comment explains.
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
