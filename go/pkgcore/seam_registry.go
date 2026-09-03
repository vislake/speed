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

// Config carries the flat scalar settings a Preset hands to a seam
// implementation's constructor: what a preset file or environment can
// naturally provide, keyed by whatever name the implementation documents.
// The type is deliberately shallow -- a host constructor that wants a typed
// configuration builds it from these strings -- because it is the boundary
// between two things that must stay separate: composing which implementation
// runs (this package's business) and sourcing that implementation's
// credentials (the host's business, out of scope for pkgcore, which never
// reads environment or files itself).
type Config map[string]string

// Registration is one named implementation of a seam: the name assembly
// resolves through a Preset, the Capability it declares about itself, and the
// constructor that builds it from a Config.
type Registration[T any] struct {
	// Name identifies the implementation within its seam, for example
	// "eventbus.memory" or "eventbus.redis". It is what a Preset's per-seam
	// value names, and what ErrUnknownImplementation and
	// ErrCapabilityUnsatisfied echo back in their error text.
	Name string

	// Capabilities is what this implementation declares about itself. See
	// Capability's own doc comment for what each bit means and how
	// Kernel.Bootstrap uses the declaration.
	Capabilities Capability

	// New builds one instance of the implementation from cfg. It is called
	// once per SeamRegistry.Build call; nothing in SeamRegistry retries it or
	// caches the result.
	New func(cfg Config) (T, error)
}

// SeamRegistry is a name-to-constructor registry for one infrastructure seam,
// mirroring the database/sql driver-registration pattern: every built-in and
// host-supplied implementation of a seam registers itself under a name, and
// assembly picks one by name -- through a Preset -- rather than switching on
// a fixed, closed set of types. pkgcore pre-populates one SeamRegistry per
// seam (EventBusRegistry, KVStoreRegistry, MailerRegistry,
// ObjectStoreRegistry) with its built-in implementations in
// builtin_implementations.go; a host registers its own implementation the
// same way, by calling Register on the matching package-level registry
// before it bootstraps a Kernel that names it in a Preset.
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
// declared at registration -- the pairing Kernel.Bootstrap's capability
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
