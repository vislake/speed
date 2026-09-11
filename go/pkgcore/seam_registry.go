package pkgcore

import (
	"errors"
	"fmt"
	"sync"
)

// ErrDuplicateImplementation is returned by SeamRegistry.Register when
// Registration.Name is already registered on that SeamRegistry.
var ErrDuplicateImplementation = errors.New("pkgcore: duplicate seam implementation")

// ErrUnknownImplementation is returned by SeamRegistry.Build when name names
// no Registration ever added to that SeamRegistry.
var ErrUnknownImplementation = errors.New("pkgcore: unknown seam implementation")

// Config carries the flat scalar settings a composition entry hands to a
// component's constructor: what a configuration file or environment can
// naturally provide, keyed by whatever name the implementation documents.
// The type is deliberately shallow -- a host constructor that wants a typed
// configuration builds it from these strings -- because it is the boundary
// between two things that must stay separate: composing which implementation
// runs (this package's business) and sourcing that implementation's
// credentials (the host's business, out of scope for pkgcore, which never
// reads environment or files itself).
type Config map[string]string

// Registration is one named implementation of a seam: the name a resolving
// caller asks for, the Capability the implementation declares about itself,
// and the constructor that builds it from a Config.
//
// # Resource ownership and the Close contract
//
// New may create resources the instance owns -- a dialed connection, a
// client built from cfg's address, a connection pool, a temporary directory
// -- and an implementation that does should declare it by implementing
// Close() error on the value it returns, whatever its concrete type. The
// value is returned to the caller as the seam interface (EventBus, KVStore,
// ...), so the Close method lives on the concrete type and is reached by an
// interface assertion, never by the seam interface itself; whoever resolves
// an implementation through Build owns closing it, exactly as a caller that
// built the implementation itself does. A Registration whose New creates no
// owned resources simply does not implement Close, and the caller has
// nothing to record.
type Registration[T any] struct {
	// Name identifies the implementation within its seam, for example
	// "eventbus.memory" or "eventbus.redis". It is what
	// ErrUnknownImplementation and
	// ErrCapabilityUnsatisfied echo back in their error text.
	Name string

	// Capabilities is what this implementation declares about itself. See
	// Capability's own doc comment for what each bit means and how the
	// declaration is read.
	Capabilities Capability

	// New builds one instance of the implementation from cfg. It is called
	// once per SeamRegistry.Build call; nothing in SeamRegistry retries it or
	// caches the result. See the type's own doc comment for the resource
	// ownership and Close() error contract a New that creates resources it
	// owns should honour.
	New func(cfg Config) (T, error)
}

// SeamRegistry is a name-to-constructor registry for one directory of
// interchangeable implementations, mirroring the database/sql
// driver-registration pattern: each implementation registers itself under a
// name, and the resolving caller picks one by name rather than switching on
// a fixed, closed set of types. It is the machinery a module's own
// implementation directory builds on -- go/pki's SignerRegistry and the
// provider registries go/ai-gateway resolves a chat or image provider
// through, whose built-in implementations register themselves from their own
// packages' init.
//
// It is deliberately not the assembly's seam machinery: which EventBus,
// KVStore, Mailer or ObjectStore value an assembly runs is the composition
// configuration's decision, made by selecting a component and configuring
// it, and the resolved values are published into (and read from) the
// by-type context. A module-internal registry resolves a typed object at
// call time instead -- a signer chosen per key, a chat provider chosen per
// route -- which is why the Config it hands New is the flat map a
// configuration entry can express.
//
// A SeamRegistry is safe for concurrent Register and Build calls.
type SeamRegistry[T any] struct {
	mu     sync.RWMutex
	byName map[string]Registration[T]
}

// NewSeamRegistry returns an empty SeamRegistry ready for Register calls.
func NewSeamRegistry[T any]() *SeamRegistry[T] {
	return &SeamRegistry[T]{byName: make(map[string]Registration[T])}
}

// Register adds r under r.Name. It returns an error wrapping
// ErrDuplicateImplementation, naming r.Name, when that name is already
// registered; the existing registration is left untouched.
func (s *SeamRegistry[T]) Register(r Registration[T]) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.byName[r.Name]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateImplementation, r.Name)
	}
	s.byName[r.Name] = r
	return nil
}

// Build constructs the implementation registered under name, passing it cfg.
// It returns an error wrapping ErrUnknownImplementation, naming name, when
// nothing is registered under it. Otherwise it returns whatever
// Registration.New(cfg) returns, alongside the Capability the implementation
// declared at registration -- the pairing the assembly's capability
// validation needs, so a caller resolving a seam through a SeamRegistry never
// has to look the declaration up separately from the value.
func (s *SeamRegistry[T]) Build(name string, cfg Config) (T, Capability, error) {
	s.mu.RLock()
	r, ok := s.byName[name]
	s.mu.RUnlock()

	if !ok {
		var zero T
		return zero, 0, fmt.Errorf("%w: %q", ErrUnknownImplementation, name)
	}

	impl, err := r.New(cfg)
	if err != nil {
		var zero T
		return zero, 0, err
	}
	return impl, r.Capabilities, nil
}
