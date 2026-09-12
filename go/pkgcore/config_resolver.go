package pkgcore

// config_resolver.go declares the seam through which the assembling engine's
// five-source configuration parser participates in the component assembly
// without becoming a dependency of this package: pkgcore stays at the
// dependency floor and imports no parser (no koanf, no go/pkgcore/config --
// the root package's zero-third-party-dependency property, measured and
// documented in the module's AGENTS.md), so it defines only the zero-
// dependency interface and looks the implementation up in the registry's
// by-type context. The engine puts its implementation before the Prepare
// stage; a registry without one keeps the plain file-block-only behaviour.

// ComponentConfigResolver upgrades one component's configuration block into
// the five-source-merged result -- flags over environment over an optional
// config file over root-key derivation over declared defaults -- the value
// that component's New receives as its cfg.
//
// The assembly calls it during the Prepare stage, for every selected
// component whose ConfigSchema is non-nil, before the block is validated or
// anything is constructed; a component that declares no schema has nothing
// to resolve and is not passed. fileConfig is the component's configuration
// block exactly as the composition carried it, and the returned
// ComponentConfig replaces it wholesale -- the returned value is what the
// assembly decodes against the schema strictly and what New receives, so it
// carries a key for every field some source supplied.
//
// Two contract points the assembly's own checks rest on:
//
//   - The returned tree must include the resolved value -- the derived key
//     material itself -- for a derive-tagged field, not only for fields an
//     explicit source spelled out. A required field is judged by the
//     presence of its key path in the returned tree, so a value the
//     resolver consumed into a derivation's output and dropped from the tree
//     would read as never supplied.
//   - A returned tree carrying an undeclared key fails the assembly's strict
//     decode of the component's block, exactly as one in the composition
//     configuration would: the schema is the block's whole accepted key set.
//
// An error aborts the assembly, wrapped with the component's name; a nil
// schema argument is not a case implementations need to handle, since the
// assembly never passes one.
type ComponentConfigResolver interface {
	ResolveComponentConfig(componentName string, schema any, fileConfig ComponentConfig) (ComponentConfig, error)
}
