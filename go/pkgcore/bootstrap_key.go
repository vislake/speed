package pkgcore

// bootstrap_key.go carries the bootstrap-key declaration: the statement a
// module makes about the process-start configuration it consumes, carried on
// its component descriptor (Component.BootstrapKeys). It is the counterpart
// of the runtime configuration seat (ConfigSchemaRegistrar, registry.go), and
// the two layers are mutually exclusive per key -- one key belongs to exactly
// one layer, enforced where both are visible (the component assembly's
// one-key-one-layer check after every declaration turn).

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
	// it that way. A Sensitive key must state it.
	Description string
	// Group buckets related keys together in the generated reference.
	// Modules conventionally use their own name.
	Group string
	// Example is the suggested value the generated .env.example renders for
	// the key; empty means the recommendation is to leave it unset.
	Example string
}
