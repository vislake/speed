package pkgcore

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
)

// component.go carries the descriptor half of the config-driven component
// assembly: the Component type, its dependency declaration (Requirement and
// Provides), and the package-level registration entry points every component
// package calls from init to make itself part of the binary. The assembly
// half -- the ComponentRegistry, its value store and its seven-stage
// lifecycle -- lives in component_registry.go; the structured configuration a
// component receives lives in config.go.

// ErrDuplicateComponent is returned by Register and by
// (*ComponentRegistry).Register when a component Name is already registered
// on the registry the call targets -- the package-level global registration
// for Register, the instance's own registration set for the method. It names
// the component. Nothing is registered when it is returned.
var ErrDuplicateComponent = errors.New("pkgcore: duplicate component")

// ErrInvalidComponent is returned by Register and by
// (*ComponentRegistry).Register when a descriptor is not well formed: an
// empty Name, a missing New callback (the one callback every component must
// carry), a ConfigSchema that is not a pointer to a config struct, or a
// Requires/Provides entry that is not a pointer to the contract type it
// names. Such a descriptor could never be selected, constructed or resolved,
// so it is refused where it enters a registry rather than failing an
// assembly later. Nothing is registered when it is returned.
var ErrInvalidComponent = errors.New("pkgcore: invalid component")

// Component is one unit of the assembly: the implementation of a module (the
// interface or contract it provides, for example the mailer module), or an
// assembly-time step of the host application. It is plain data plus
// callbacks, so a component is registered as a value and its product is a
// plain value too (a *gorm.DB, a *authn.Module, an http.Handler).
//
// # Declaring a callback means participating in that stage
//
// The seven callbacks are the component's participation in the seven-stage
// lifecycle ComponentRegistry drives (see its Prepare through Close methods
// for what each stage does as a whole). New is the one required callback --
// it produces the component's product; every other callback is optional, and
// a component that does not declare one simply skips that stage. Verify,
// Init, Start, Stop and Close receive the instance New produced as their
// third parameter, so a callback acts on its own product directly instead of
// retrieving it from the registry; Prepare runs before any instance exists
// and New is the callback that creates one, so neither receives it.
//
// # What a component provides and requires
//
// Provides declares the product types this component contributes to the
// registry's by-type context, each as a typed nil pointer to the contract
// type (for example (*gorm.DB)(nil) or (*pkgcore.Mailer)(nil)). The assembly
// resolves every Requirement.Token of every selected component against the
// selected components' Provides declarations -- the token matches a declared
// product type when the type, or the type behind the pointer, is assignable
// to the token's type -- and the resolved matches both order the assembly
// (a component is constructed after every component providing one of its
// tokens) and drive auto-pull. At construction the registry puts each
// component's product into the by-type context itself, so runtime Get[T]
// answers from the same data the declaration promised.
//
// # Identity
//
// Name is the selection key in a composition configuration: "authn" for a
// single-implementation module, "owner.thing" for one implementation of a
// multi-implementation module ("mailer.smtp"). Module names the module this
// component implements, when it implements one; MemberNames reads it to
// answer which members of a directory-style module are selected.
type Component struct {
	// Name is the component's unique identifier within a registry, and the
	// key a composition configuration selects it under: "authn",
	// "mailer.smtp", "seed.demo". By convention a single-implementation
	// module's component carries the module's own name.
	Name string

	// Prepare runs in the Prepare stage, before anything is constructed and
	// before any instance exists. It is where behavior that must happen
	// before the database opens belongs: a component building its own PII
	// cipher from key material, registering a serializer. Prepare callbacks
	// execute in registration order, after the assembly has parsed and
	// validated the composition.
	Prepare func(ctx context.Context, reg *ComponentRegistry) error

	// New constructs the component's product -- the required callback.
	// cfg carries the component's resolved configuration, the
	// components.<name> subtree of the composition; a component with no
	// configuration block receives an empty config. The product may carry
	// resources it owns; returning no product -- (nil, nil), or a typed nil
	// pointer such as (*T)(nil) -- is refused, because a nil product would
	// leave every dependent requirement reading a present-but-nil value
	// instead of failing at the construction that produced it. The registry
	// puts the returned product into the by-type context, so New itself must
	// not.
	New func(ctx context.Context, reg *ComponentRegistry, cfg ComponentConfig) (any, error)

	// Verify runs after every product is constructed and the database is
	// reachable, in dependency order (so a database component applies
	// migrations here, before any other Verify runs). It is for checking
	// the component's own preconditions -- schema consistency, an external
	// condition -- never for initialization: a Verify callback that fails
	// rolls the whole assembly back.
	Verify func(ctx context.Context, reg *ComponentRegistry, instance any) error

	// Init declares, wires and publishes: the declaration seats (Routes,
	// Config and the eight others on the ComponentRegistry) accept writes
	// only while this stage runs, and a component's Init callback is where
	// its declarations go in -- and where a runtime service that could not
	// exist as a construction value is published with Put. After every Init
	// callback the assembly runs its final cross-seat validation.
	Init func(ctx context.Context, reg *ComponentRegistry, instance any) error

	// Start begins serving: listeners, workers, schedulers. It runs after
	// the Init stage's closing validation, so everything declared in Init
	// is complete when a Start callback reads the seats.
	Start func(ctx context.Context, reg *ComponentRegistry, instance any) error

	// Stop is the non-blocking half of shutdown: stop accepting new work,
	// begin draining. Stop callbacks run in reverse dependency order and
	// their failures are ignored, because a stop notification that fails
	// must not block the Close that follows it.
	Stop func(ctx context.Context, reg *ComponentRegistry, instance any) error

	// Close releases resources, once, in reverse dependency order after the
	// Stop notification has gone out. Close callback failures are
	// aggregated and reported together. A component that owns nothing
	// closable simply does not declare Close.
	Close func(ctx context.Context, reg *ComponentRegistry, instance any) error

	// Requires declares the contract tokens this component consumes: one
	// Requirement per token. Dependencies are declared against products
	// (token types), never against component identity, so the assembly can
	// derive both construction order and auto-pull from them; a required
	// token with no provider fails the Prepare stage, and an optional one
	// simply leaves the consumer to run with a zero value.
	Requires []Requirement

	// Provides declares the token set this component delivers into the
	// registry by the end of its construction, each as a typed nil pointer
	// to the contract type -- (*gorm.DB)(nil), (*pkgcore.Mailer)(nil). The
	// declared set is the full delivery: the product New returns, plus any
	// value New itself puts (a module handing out a second construction
	// value, such as a service built in the constructor, declares and puts
	// it). The declaration is what lets the assembly resolve Requirements,
	// detect ambiguity and auto-pull a unique provider before anything is
	// constructed; when the component's construction completes, the
	// assembly asserts EVERY declared token was delivered -- a declaration
	// nothing delivered fails the construction, because a declaration is a
	// promise a plan-time resolution rests on. Services published at Init
	// are not part of this set (services never resolve token
	// requirements).
	Provides []any

	// Capabilities is what this component declares about itself. The
	// assembly compares it against the deployment mode's requirement during
	// Prepare; a component missing a required capability fails the
	// assembly, naming the component, the missing bits and the mode.
	Capabilities Capability

	// ConfigSchema is a typed nil pointer to the struct the component's
	// configuration decodes into ((*mailerConfig)(nil)); nil means the
	// component takes no configuration, and any key in its composition
	// block is then unknown. The assembly decodes the component's
	// components.<name> subtree into the schema strictly -- an undeclared
	// key fails the Prepare stage and the error lists the accepted keys --
	// before anything is constructed.
	ConfigSchema any

	// BootstrapKeys declares the process-start key material this component
	// consumes: one key path plus the purpose it derives under. The
	// declaration is read before anything is constructed -- the loader
	// resolves every registered component's declared keys on its
	// resolution/derivation chain and publishes the results as a
	// by-purpose material source -- so it is deliberately not a seat:
	// nothing a component must have before construction can wait for the
	// Init gate. A declared key path carries no value and no secret, only
	// the path that is part of the key material's identity (a rename is a
	// rotation, per BootstrapKeyPurpose's stability contract).
	BootstrapKeys []BootstrapKey

	// SystemPurposes declares the system-context purposes this component
	// acts under: the reasons it may legitimately operate outside an
	// ordinary tenant context. The assembly registers every selected
	// component's declarations together at the Init stage's entry, before
	// any Init callback runs -- a callback may open a system context under
	// a purpose another selected component declared. A purpose two selected
	// components both declare fails the assembly before anything is
	// registered, so the set WithSystemContext accepts is exactly the set
	// the assembly agreed on.
	SystemPurposes []SystemPurpose

	// Migrations carries the component's versioned SQL migrations, one
	// subdirectory per dialect ("postgres", "sqlite"), each holding
	// lexically ordered *.sql files. The zero embed.FS means the component
	// carries none. A database component applies the selected set in the
	// Verify stage.
	Migrations embed.FS

	// Locales carries the component's translation resources, one
	// <language>.toml file per catalog language, ids prefixed with the
	// component's name. The zero embed.FS means the component renders no
	// message. The Prepare stage validates key-set parity across the files;
	// the catalog merge itself is the host's step.
	Locales embed.FS

	// OpenAPISpec is the component's OpenAPI document fragment, which the
	// host merges into the application-wide specification. Empty means the
	// component serves no HTTP surface of its own.
	OpenAPISpec []byte

	// Module names the module (contract) this component implements, when
	// it implements one: "mailer" for both mailer.smtp and mailer.console,
	// "gateway" for every payment-channel member, "authn" for authn. It is
	// empty for an assembly-time step that implements no module. MemberNames
	// reads it to list a module's selected members.
	Module string
}

// Requirement is one contract token a component consumes, declared against
// the product that satisfies it rather than against the component that
// provides it. The assembly resolves it to the selected components whose
// Provides declarations match the token: exactly one provider orders the
// assembly, none fails the assembly for a required token (or triggers
// auto-pull of a uniquely-available registered component), and several fail
// it as ambiguous.
type Requirement struct {
	// Token identifies the consumed contract as a typed nil pointer to its
	// type -- (*gorm.DB)(nil), (*authn.KeySource)(nil),
	// (*pkgcore.Mailer)(nil). The value itself is never read; only its type
	// matters, and it must be a pointer.
	Token any

	// Optional declares that the dependency may go unsatisfied: the
	// assembly does not fail and does not auto-pull a provider for a token
	// no selected component provides, and the consumer runs with a zero
	// value it must treat as fail-closed. An optional token that several
	// selected components do provide is still ambiguous.
	Optional bool
}

// globalComponents is the process-wide registration every package writes
// through Register (hand-written hosts) or MustRegister (init self-registration).
// NewComponentRegistry seeds each instance from a snapshot of it.
var globalComponents struct {
	mu     sync.RWMutex
	order  []string
	byName map[string]Component
}

// Register adds c to the package-level global registration. Import decides
// which components a binary contains and a composition configuration decides
// which of them are selected, so registration is the one step a component
// package performs unconditionally at init time.
//
// It returns an error wrapping ErrInvalidComponent for a malformed
// descriptor and an error wrapping ErrDuplicateComponent when c.Name is
// already registered -- a duplicate is a programming error, not a merge, so
// every registry holds one descriptor per name. Nothing is registered when
// the call returns an error. Register is safe for concurrent use; components
// registered after NewComponentRegistry returns are not seen by that
// instance's snapshot.
func Register(c Component) error {
	if err := validateComponent(c); err != nil {
		return err
	}

	globalComponents.mu.Lock()
	defer globalComponents.mu.Unlock()

	if _, exists := globalComponents.byName[c.Name]; exists {
		return fmt.Errorf("%w (stage registration): component %q is already registered; component names are unique", ErrDuplicateComponent, c.Name)
	}
	if globalComponents.byName == nil {
		globalComponents.byName = make(map[string]Component)
	}
	globalComponents.byName[c.Name] = c
	globalComponents.order = append(globalComponents.order, c.Name)
	return nil
}

// MustRegister is Register for the init-time self-registration path, where a
// failure (a malformed descriptor or a duplicate name) is a programming
// error inside the binary that cannot be corrected at runtime and must stop
// the process at startup rather than leave a component silently absent from
// the global registration.
func MustRegister(c Component) {
	if err := Register(c); err != nil {
		panic(fmt.Sprintf("pkgcore: component registration failed: %v", err))
	}
}

// validateComponent reports whether c is a well-formed descriptor: a
// non-empty Name, a New callback, a ConfigSchema that is either nil or a
// pointer to a struct, and Requires/Provides entries that are pointers.
func validateComponent(c Component) error {
	if c.Name == "" {
		return fmt.Errorf("%w: component has an empty name", ErrInvalidComponent)
	}
	if c.New == nil {
		return fmt.Errorf("%w: component %q declares no New callback, and New is the one required callback", ErrInvalidComponent, c.Name)
	}
	if c.ConfigSchema != nil {
		t := reflect.TypeOf(c.ConfigSchema)
		if t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
			return fmt.Errorf("%w: component %q has a ConfigSchema of type %s, want a pointer to a config struct such as (*mailerConfig)(nil), or nil for a component that takes no configuration", ErrInvalidComponent, c.Name, t)
		}
	}
	for i, req := range c.Requires {
		if err := checkTokenShape(req.Token); err != nil {
			return fmt.Errorf("%w: component %q requirement %d: %w", ErrInvalidComponent, c.Name, i, err)
		}
	}
	for i, provided := range c.Provides {
		if err := checkTokenShape(provided); err != nil {
			return fmt.Errorf("%w: component %q Provides entry %d: %w", ErrInvalidComponent, c.Name, i, err)
		}
	}
	return nil
}

// checkTokenShape reports whether tok is a typed pointer usable as a contract
// token: a Requirement.Token or a Provides entry.
func checkTokenShape(tok any) error {
	t := reflect.TypeOf(tok)
	if t == nil || t.Kind() != reflect.Pointer {
		return fmt.Errorf("%s is not a contract token, want a typed pointer to the contract type such as (*pkgcore.Mailer)(nil)", typeName(t))
	}
	return nil
}

// typeName renders a type for error text, using "an untyped nil" for the null
// type.
func typeName(t reflect.Type) string {
	if t == nil {
		return "an untyped nil"
	}
	return t.String()
}

// productMatchesToken reports whether a product of type product satisfies the
// contract token of type token: either the product type is assignable to the
// token type itself, or -- the shape every interface contract takes, since a
// nil interface value cannot carry its own type -- to the type the token
// points at. (*gorm.DB)(nil) matches a *gorm.DB product directly;
// (*pkgcore.Mailer)(nil) matches any product that implements Mailer.
func productMatchesToken(product, token reflect.Type) bool {
	if product.AssignableTo(token) {
		return true
	}
	if token.Kind() == reflect.Pointer && product.AssignableTo(token.Elem()) {
		return true
	}
	return false
}

// tokenDisplay renders a token type for error text: the contract's own name,
// with the conventional pointer stripped, so (*authn.KeySource)(nil) reads as
// "authn.KeySource" and (*gorm.DB)(nil) as "gorm.DB".
func tokenDisplay(token reflect.Type) string {
	if token.Kind() == reflect.Pointer {
		return token.Elem().String()
	}
	return token.String()
}

// closestName returns the registered name closest to name -- the suggestion a
// misspelled selection key gets -- or "" when nothing is close enough to
// suggest. Distance is Levenshtein distance over the names, accepted up to 2
// edits; ties go to the shorter name, then the lexicographically smaller one,
// so the suggestion is deterministic.
func closestName(name string, candidates []string) string {
	const maxDistance = 2
	best, bestDistance := "", maxDistance+1
	for _, candidate := range candidates {
		d := levenshtein(name, candidate)
		if d < bestDistance || (d == bestDistance && betterCandidate(candidate, best)) {
			best, bestDistance = candidate, d
		}
	}
	if bestDistance > maxDistance {
		return ""
	}
	return best
}

// betterCandidate reports whether candidate should replace current as the
// closest-name suggestion: the shorter name wins, then the lexicographically
// smaller one.
func betterCandidate(candidate, current string) bool {
	if len(candidate) != len(current) {
		return len(candidate) < len(current)
	}
	return candidate < current
}

// levenshtein returns the edit distance between a and b.
func levenshtein(a, b string) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, min(curr[j-1]+1, prev[j-1]+cost))
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// globalComponent returns the globally registered component named name, and
// whether one exists.
func globalComponent(name string) (Component, bool) {
	globalComponents.mu.RLock()
	defer globalComponents.mu.RUnlock()
	c, ok := globalComponents.byName[name]
	return c, ok
}

// joinNames renders a name list for error text, or a placeholder for the
// empty list.
func joinNames(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
