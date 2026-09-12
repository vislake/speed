package pkgcore

import (
	"context"
	"fmt"
)

// component_override.go carries the host-facing descriptor derivation a
// host-wiring override is built on: looking one registered descriptor up by
// name, and deriving from it a renamed copy whose construction and
// requirements the host supplies. Both operations read the registration
// alone -- a descriptor is plain data -- so a host derives its override
// during its own wiring, before any stage runs.

// LookupComponent returns the descriptor registered on reg under name, and
// whether the registry carries one.
//
// It reads RegisteredComponents, so it sees exactly what the instance
// carries -- the global snapshot it was seeded with plus every descriptor
// Register added -- and it reads the registration rather than the assembly
// plan: a component registered but not selected is found, which is the
// point for a caller acting on a component's own declarations rather than
// on a plan entry (Build) or a selected name (ComponentCapabilities). The
// returned descriptor is the registered value itself, copied by value: a
// caller that renames or otherwise adjusts its copy never touches the
// registration.
func LookupComponent(reg *ComponentRegistry, name string) (Component, bool) {
	for _, c := range RegisteredComponents(reg) {
		if c.Name == name {
			return c, true
		}
	}
	return Component{}, false
}

// Override returns a descriptor derived from the one registered on reg
// under base: the same declaration surface -- Module, ConfigSchema and
// ConfigNamespace, BootstrapKeys, SystemPurposes, Capabilities, Provides,
// ProvidesMember, Migrations, Locales, OpenAPISpec and every lifecycle
// callback except New -- under the name name, constructed by construct,
// requiring base's own tokens plus extra.
//
// It is the derivation a host reaches for when a module's own construction
// is not the one this deployment needs -- a membership store, a provider
// set, an event mapping, a write-capture scope the descriptor cannot carry.
// The override declares exactly what the module package's descriptor
// declares and only constructs it differently, so capability validation,
// asset collection (locale merging, the migration ledger, the OpenAPI
// merge) and the requirement graph all read the module package's own
// declarations. extra carries the additional tokens the host's construction
// reads from the by-type context beyond what the module's own construction
// required, and is appended as a fresh list: the registered descriptor's
// own Requires slice is never mutated.
//
// name is the caller's (a component name is unique within one registry, so
// an override carries its own -- conventionally the host's prefix joined to
// the module's name, keeping the override readable as such). A base with no
// registered descriptor returns an error naming base, rather than a
// descriptor silently carrying no declarations. Nothing is registered by
// this call: the caller registers the returned descriptor like any other
// component.
func Override(reg *ComponentRegistry, base, name string, construct func(ctx context.Context, reg *ComponentRegistry, cfg ComponentConfig) (any, error), extra ...Requirement) (Component, error) {
	descriptor, ok := LookupComponent(reg, base)
	if !ok {
		return Component{}, fmt.Errorf("pkgcore: component %q has no registered descriptor to override", base)
	}
	c := descriptor
	c.Name = name
	c.New = construct
	c.Requires = append(append([]Requirement(nil), descriptor.Requires...), extra...)
	return c, nil
}
