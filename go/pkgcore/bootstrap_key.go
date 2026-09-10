package pkgcore

// bootstrap_key.go carries the bootstrap-key declaration seat: the channel a
// module declares the process-start configuration it consumes on. It is the
// counterpart of the runtime configuration seat (ConfigSchemaRegistrar,
// registry.go), and the two seats are mutually exclusive per key -- one key
// belongs to exactly one layer, enforced where both are visible
// (validateBootstrapKeySeparation, called from Kernel.Bootstrap).

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

var (
	// ErrInvalidBootstrapKey reports a declaration whose fields contradict one
	// another: an empty Key, an unknown Format, or a Sensitive key with no
	// Description. Nothing is registered when Add returns it.
	ErrInvalidBootstrapKey = errors.New("pkgcore: invalid bootstrap key")

	// ErrDuplicateBootstrapKey is returned when two modules register the same
	// bootstrap key, or one module registers it twice. Two modules owning one
	// key is a bug rather than a merge, exactly as it is for a configuration
	// key: there is one process environment, no registration order that can
	// make two readings coherent.
	ErrDuplicateBootstrapKey = errors.New("pkgcore: duplicate bootstrap key")
)

// bootstrapKeyFormats is the closed set a declaration's Format may name.
// "hexkey" is a 32-byte key's hexadecimal text, the encoding every key
// material in this repository travels in; widening the set is a platform
// change, not a module's choice.
var bootstrapKeyFormats = map[string]struct{}{
	"string": {},
	"int":    {},
	"bool":   {},
	"hexkey": {},
}

// BootstrapKey describes one process-start configuration key a module
// consumes: input the host resolves once, before the process is wired, from
// command-line flags, the environment, an optional config file or the
// defaults on the host's own loader target struct (go/pkgcore/config).
//
// A declaration states the module's contract for the key, never its value:
// the module does not read the key itself (the host resolves it and injects
// the result), so a declaration is documentation, validation and the
// generated configuration reference's source, all at once.
//
// The field set deliberately mirrors what a rendered reference needs and
// stops there. Default is a documented statement of the fallback behaviour,
// not a Go value: the authoritative default is whatever the host's loader
// target struct carries, and a second runnable default here would drift from
// it. Constraints such as "required", bounds and public exposure belong to
// the host's binding side or to the runtime configuration seat, neither of
// which exists at this layer.
type BootstrapKey struct {
	// Key is the dotted key path, the same naming surface the loader uses for
	// flags and config files, for example "authn.pii_cipher_key". By
	// convention it carries the owning module's name as its first segment, so
	// a bootstrap key and a runtime configuration item are distinguishable in
	// documentation at a glance.
	Key string
	// Format names the value's shape. The set is closed: "string", "int",
	// "bool" or "hexkey" (a 32-byte key's hexadecimal text).
	Format string
	// Default states in operator terms what happens when no source supplies
	// the key, for example "documented non-secret development default". Empty
	// means the module documents no fallback.
	Default string
	// Sensitive marks key material: a value the outputs never print and a
	// real deployment must feed from a secret store. It changes presentation
	// and deployment guidance only, never what the module does with the
	// value.
	Sensitive bool
	// Description is the English contract text: what the key protects, why it
	// exists separately from its neighbours, and which isolation rule keeps
	// it that way. A Sensitive key must state it; see Add.
	Description string
	// Group buckets related keys together in the generated reference.
	// Modules conventionally use their own name.
	Group string
	// Example is the suggested value the generated .env.example renders for
	// the key; empty means the recommendation is to leave it unset.
	Example string
}

// BootstrapRegistrar collects the bootstrap keys modules declare, mirroring
// ConfigSchemaRegistrar for the process-start layer. A module declares the
// keys it consumes during Register and never resolves them itself.
//
// One dotted key belongs to exactly one layer: a key registered here is
// process-start input, resolved once and fixed for the process's lifetime,
// while an item registered on ConfigSchemaRegistrar is a per-tenant value an
// operator edits at runtime. Kernel.Bootstrap refuses a key declared on both.
type BootstrapRegistrar interface {
	// Add registers bootstrap keys, validating every declaration first. A
	// declaration with an empty Key, an unknown Format, or Sensitive set
	// while Description is empty is rejected with an error wrapping
	// ErrInvalidBootstrapKey: a sensitive key whose contract goes undocumented
	// is the one shape a reader cannot act on. A key already registered (by an
	// earlier call, or twice within this one) is rejected with an error
	// wrapping ErrDuplicateBootstrapKey. Nothing is registered when the call
	// returns an error.
	Add(keys ...BootstrapKey) error
	// Keys returns every bootstrap key registered so far, in registration
	// order.
	Keys() []BootstrapKey
}

// validateBootstrapKey reports the first contradiction in a declaration.
func validateBootstrapKey(key BootstrapKey) error {
	if key.Key == "" {
		return fmt.Errorf("%w: a key without a key path cannot be registered", ErrInvalidBootstrapKey)
	}
	if _, ok := bootstrapKeyFormats[key.Format]; !ok {
		return fmt.Errorf("%w: key %q has format %q, want one of string, int, bool or hexkey",
			ErrInvalidBootstrapKey, key.Key, key.Format)
	}
	// A sensitive declaration is the one a reader cannot verify against
	// anything else: its value never appears in an output, so the contract
	// text is the whole of what an operator has to work from.
	if key.Sensitive && key.Description == "" {
		return fmt.Errorf("%w: key %q is Sensitive but carries no Description; a secret key's contract cannot be left unwritten",
			ErrInvalidBootstrapKey, key.Key)
	}
	return nil
}

// validateBootstrapKeySeparation reports every key declared on both the
// bootstrap seat and the runtime configuration seat. One dotted key can only
// mean one thing: a runtime item is a per-tenant value edited at run time, a
// bootstrap key is a process input fixed at startup, and an operator editing
// "that key" in an admin console must be able to tell which layer they are
// changing. The two seats cannot see each other while modules register, so
// the conflict is caught here, where both are complete.
func validateBootstrapKeySeparation(reg *Registry) error {
	if reg.Config == nil || reg.Bootstrap == nil {
		return nil
	}
	runtimeKeys := make(map[string]struct{})
	for _, item := range reg.Config.Items() {
		runtimeKeys[item.Key] = struct{}{}
	}
	var conflicts []error
	for _, key := range reg.Bootstrap.Keys() {
		if _, both := runtimeKeys[key.Key]; !both {
			continue
		}
		conflicts = append(conflicts, fmt.Errorf(
			"key %q is declared by both the runtime configuration seat (reg.Config) and the bootstrap seat (reg.Bootstrap); a key belongs to exactly one layer",
			key.Key,
		))
	}
	if len(conflicts) == 0 {
		return nil
	}
	return fmt.Errorf("pkgcore: configuration key declared on two layers:\n  %w", errors.Join(conflicts...))
}

type memoryBootstrapRegistrar struct {
	mu   sync.Mutex
	keys map[string]struct{}
	all  []BootstrapKey
}

func (r *memoryBootstrapRegistrar) Add(keys ...BootstrapKey) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Validation comes before the duplicate check, for the same reason
	// memoryConfigRegistrar documents: a contradictory declaration reports
	// itself as invalid rather than as a collision with whatever an earlier
	// caller registered under the same key. Either way the whole call
	// registers nothing.
	for _, key := range keys {
		if err := validateBootstrapKey(key); err != nil {
			return err
		}
	}
	keyOf := func(key BootstrapKey) string { return key.Key }
	if err := checkUnique(r.keys, keys, keyOf, ErrDuplicateBootstrapKey); err != nil {
		return err
	}
	for _, key := range keys {
		r.keys[key.Key] = struct{}{}
		r.all = append(r.all, key)
	}
	return nil
}

func (r *memoryBootstrapRegistrar) Keys() []BootstrapKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.all)
}
