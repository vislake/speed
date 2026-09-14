package http

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/vislake/speed/pkg/config"
)

// configNamespace is where this module's input items are mounted in the config
// data. It is a constant rather than the module name: the name is a parameter
// of the descriptor, and a section that moved with it would rename a host's
// configuration file whenever the implementation subpackage changed.
const configNamespace = "http"

// configPath is the complete path of the carrier struct, which is what Decode
// is given.
const configPath = configNamespace

// endpointsItem is the key of the endpoint set, relative to the namespace. The
// disabled stance names it, so the reader is told which key to write.
const endpointsItem = "endpoints"

// configGroup is the section the help output lists this module's items under.
const configGroup = "HTTP"

// moduleConfig is the set of input items this module accepts. It is not
// exported: the surface carries net/http's types and this package's own, and a
// configuration struct on it would make a host's file shape part of the API.
type moduleConfig struct {
	// Endpoints is one entry per listening endpoint, keyed by the endpoint
	// name registrants use.
	Endpoints map[string]endpointConfig `config:"endpoints"`
}

// endpointConfig is one listening endpoint as configuration gives it.
//
// The durations and the body limit are pointers because the endpoint set is a
// container leaf: the primary source replaces the whole map rather than merging
// into it, so a value type could not tell "the key is absent" from "the key is
// 0", and 0 is the legal way to switch most of the timeouts off. A duration is
// written as a string time.ParseDuration accepts; config refuses a bare number,
// because 300 would read as five minutes and mean three hundred nanoseconds.
// Two of the durations refuse an explicit 0 instead of switching off, and
// resolve says which and why.
type endpointConfig struct {
	// Address is the address to listen on, in net.Listen's form, for
	// example ":8080" or "127.0.0.1:8080".
	Address string `config:"address"`

	// ReadHeaderTimeout bounds how long the request headers may take to
	// arrive. It is one of the two durations that cannot be switched off: a
	// server with no header timeout is held open indefinitely by a client
	// that sends its headers one byte at a time.
	ReadHeaderTimeout *time.Duration `config:"read-header-timeout"`
	// ReadTimeout bounds the whole request read, 0 for no bound.
	ReadTimeout *time.Duration `config:"read-timeout"`
	// WriteTimeout bounds the response write, 0 for no bound.
	WriteTimeout *time.Duration `config:"write-timeout"`
	// IdleTimeout bounds how long a kept-alive connection may sit idle, 0
	// for no bound.
	IdleTimeout *time.Duration `config:"idle-timeout"`
	// DrainTimeout is how long Close waits for this endpoint's in-flight
	// requests. It is the other duration that cannot be switched off: it is
	// the only bound the drain has, because the context Stop and Close
	// receive has its cancellation stripped.
	DrainTimeout *time.Duration `config:"drain-timeout"`

	// MaxBodyBytes is the request body limit the outermost layer of the
	// chain imposes, 0 for no limit. It applies to every handler on the
	// endpoint, including the ones that never call Decode.
	MaxBodyBytes *int64 `config:"max-body-bytes"`

	// TLSCertFile and TLSKeyFile are where the certificate and its key are
	// read from. Both empty serves plain HTTP; one of them alone is a
	// startup failure. Where the certificate comes from and when it is
	// replaced are outside this module.
	TLSCertFile string `config:"tls-cert-file"`
	TLSKeyFile  string `config:"tls-key-file"`
}

// The defaults of the optional per-endpoint items, applied after decoding
// rather than laid into the prototype: a container leaf is replaced as a whole,
// so the prototype's values do not survive a host writing the endpoint set.
const (
	defaultReadHeaderTimeout = 10 * time.Second
	defaultReadTimeout       = 30 * time.Second
	defaultWriteTimeout      = 30 * time.Second
	defaultIdleTimeout       = 120 * time.Second
	defaultDrainTimeout      = 30 * time.Second
	defaultMaxBodyBytes      = int64(1 << 20)
)

// configDefaults is the prototype this module declares. The endpoint set is
// empty in it, which is what makes the disabled stance reachable: a default
// endpoint would open a port on every host that never asked for one, and the
// "no endpoint is configured" branch could never be taken.
var configDefaults = moduleConfig{}

// schema is this module's input item declaration.
//
// endpoints takes the primary config source alone. It is a map, and such a leaf
// has no flat form the environment or the command line could give; declaring
// either of them on it is ErrInvalidSchema. The endpoint set is therefore
// changed in the config file or the remote config centre, never by an
// environment variable.
func schema() config.Schema {
	return config.Schema{
		Namespace: configNamespace,
		Mounts:    []config.Mount{{Value: &configDefaults}},
		Items: map[string]config.Item{
			endpointsItem: {
				Origins:     config.OriginPrimary,
				Group:       configGroup,
				Description: "the listening endpoints, each with its own address, timeouts and body limit",
			},
		},
	}
}

// endpointSettings is one endpoint's configuration, validated and completed:
// every optional item carries a number, and a 0 means that item is switched
// off, which is also what an explicit 0 in the configuration means. The header
// timeout and the drain timeout never arrive as 0 — an explicit 0 for either is
// refused before any of this is built.
type endpointSettings struct {
	name    string
	address string

	readHeaderTimeout time.Duration
	readTimeout       time.Duration
	writeTimeout      time.Duration
	idleTimeout       time.Duration
	drainTimeout      time.Duration

	maxBodyBytes int64

	tlsCertFile string
	tlsKeyFile  string
}

// tls reports whether this endpoint serves over TLS.
func (s endpointSettings) tls() bool { return s.tlsCertFile != "" || s.tlsKeyFile != "" }

// resolve validates the configuration and fills in the optional items,
// returning the endpoints in name order. The order is fixed so that a startup
// failure, a diagnostic line and the order in which addresses are bound do not
// depend on Go's map iteration.
func (c moduleConfig) resolve() ([]endpointSettings, error) {
	out := make([]endpointSettings, 0, len(c.Endpoints))
	for _, name := range slices.Sorted(maps.Keys(c.Endpoints)) {
		resolved, err := c.Endpoints[name].resolve(name)
		if err != nil {
			return nil, err
		}
		out = append(out, resolved)
	}
	return out, nil
}

// resolve validates one endpoint. Every message names the endpoint and the key
// to change: a startup failure that only says "missing address" leaves the host
// to work out which of its endpoints it came from.
func (e endpointConfig) resolve(name string) (endpointSettings, error) {
	if e.Address == "" {
		return endpointSettings{}, fmt.Errorf(
			"http: endpoint %q gives no address to listen on. Write %s.%s.%s.address, "+
				"for example \":8080\"", name, configNamespace, endpointsItem, name)
	}

	// A negative duration is not the same mistake as 0: 0 switches the item
	// off on purpose, while a negative one is a typo that net/http would
	// treat as an already-expired deadline and fail every request with.
	durations := [...]struct {
		key   string
		given *time.Duration
	}{
		{"read-header-timeout", e.ReadHeaderTimeout},
		{"read-timeout", e.ReadTimeout},
		{"write-timeout", e.WriteTimeout},
		{"idle-timeout", e.IdleTimeout},
		{"drain-timeout", e.DrainTimeout},
	}
	for _, d := range durations {
		if d.given != nil && *d.given < 0 {
			return endpointSettings{}, fmt.Errorf(
				"http: endpoint %q gives %s the value %s; 0 switches the bound off and a "+
					"positive duration sets it", name, d.key, *d.given)
		}
	}
	// Two of the durations refuse an explicit 0 rather than reading it as
	// "switch this bound off". The criterion is who carries the consequence
	// of switching the item off: for the rest it falls on the endpoint that
	// asked for it, and for these two it falls on the host's ability to go
	// on serving and to stop at all.
	cannotBeSwitchedOff := [...]struct {
		key    string
		given  *time.Duration
		def    time.Duration
		reason string
	}{
		{"read-header-timeout", e.ReadHeaderTimeout, defaultReadHeaderTimeout,
			"without it a client that sends its headers one byte at a time holds a " +
				"connection open indefinitely, and the endpoint goes on reporting that " +
				"it accepts requests while the connections pile up"},
		{"drain-timeout", e.DrainTimeout, defaultDrainTimeout,
			"without it Close waits for ever on an in-flight request that cannot " +
				"finish: the context Stop and Close receive has its cancellation " +
				"stripped and the registry imposes no shutdown timeout of its own, so a " +
				"host blocked in Run is left with nothing to interrupt it with"},
	}
	for _, d := range cannotBeSwitchedOff {
		if d.given != nil && *d.given == 0 {
			return endpointSettings{}, fmt.Errorf(
				"http: endpoint %q switches %s off with 0, and it is one of the two bounds "+
					"that have to stay on: %s. Give %s.%s.%s.%s a positive duration, or drop "+
					"the key to take the default of %s",
				name, d.key, d.reason, configNamespace, endpointsItem, name, d.key, d.def)
		}
	}
	if e.MaxBodyBytes != nil && *e.MaxBodyBytes < 0 {
		return endpointSettings{}, fmt.Errorf(
			"http: endpoint %q gives max-body-bytes the value %d; 0 removes the limit and a "+
				"positive number sets it", name, *e.MaxBodyBytes)
	}
	if (e.TLSCertFile == "") != (e.TLSKeyFile == "") {
		return endpointSettings{}, fmt.Errorf(
			"http: endpoint %q gives only one of tls-cert-file and tls-key-file. Give both to "+
				"serve over TLS, or neither to serve plain HTTP", name)
	}

	return endpointSettings{
		name:              name,
		address:           e.Address,
		readHeaderTimeout: valueOrDefault(e.ReadHeaderTimeout, defaultReadHeaderTimeout),
		readTimeout:       valueOrDefault(e.ReadTimeout, defaultReadTimeout),
		writeTimeout:      valueOrDefault(e.WriteTimeout, defaultWriteTimeout),
		idleTimeout:       valueOrDefault(e.IdleTimeout, defaultIdleTimeout),
		drainTimeout:      valueOrDefault(e.DrainTimeout, defaultDrainTimeout),
		maxBodyBytes:      valueOrDefault(e.MaxBodyBytes, defaultMaxBodyBytes),
		tlsCertFile:       e.TLSCertFile,
		tlsKeyFile:        e.TLSKeyFile,
	}, nil
}

// valueOrDefault reads an optional item in its three states: absent takes the
// default, an explicit 0 switches the item off, and any other value is taken as
// given. The two items that cannot be switched off never reach it with a 0:
// resolve has refused such a configuration before it gets here.
func valueOrDefault[T any](given *T, def T) T {
	if given == nil {
		return def
	}
	return *given
}
