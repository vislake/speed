package app

import (
	"context"
	"embed"
	"fmt"
	"reflect"

	"github.com/vislake/speed/go/pkgcore"
)

// legacy_bridge.go carries the transition bridge between the module world
// (pkgcore.Module, the module Registry, Kernel.Bootstrap) and the component
// world (pkgcore.Component, ComponentRegistry, the seven-stage drive), for
// as long as the business modules have not migrated. It is one path, never a
// fork:
//
//   - A legacy module is wrapped as a component whose declaration entry point
//     is still the module's one Register call, run during the component's
//     Init callback -- the single declaration path. The wrapper contributes
//     the module's migrations and OpenAPI fragment to the component asset
//     set (its locale resources merge through the asset stand-ins, under the
//     module's own name), maps its DependsOn names onto the dependency
//     tokens the assembly orders by, and produces the module instance the
//     host's module-construction callback already built as its product.
//   - The registry the modules declare into is the module Registry a
//     Kernel.Bootstrap returned, with its declaration seats re-pointed at the
//     component assembly's seats. Reads and writes therefore travel the same
//     registrar objects the component world uses -- the assemblies' own
//     validation (seat consistency, the feature graph, one key per layer)
//     runs over the very declarations legacy modules made -- and the view is
//     a type adaptation, not a second declaration path.
//   - The Kernel the modules' host options name is bootstrapped once, over
//     asset-carrying stand-ins of the module set: that call resolves the four
//     infrastructure seams (preset or host-injected, validated against the
//     deployment mode, announced and warned exactly as before) and merges the
//     module set's locale resources into the message catalog the view
//     answers. The stand-ins declare nothing -- their Register is a no-op --
//     so the module Registry Bootstrap returns carries no module
//     declarations; the real modules declare through the wrapped components'
//     Init callbacks into the same registry object.

// buildLegacyRegistryView re-points the module Registry's ten declaration
// seats at the component assembly's seats, so every declaration a legacy
// module makes lands in the assembly's own registrars. The Bootstrap seat
// stays the module Registry's own registrar: the declaration face carries no
// bootstrap-key seat (a component declares BootstrapKeys statically, and a
// module declares them on its component descriptor), so the seat collects
// only what a host declares on it directly. The module-set keys are resolved
// by the loader over the registered components, wrappers included.
func buildLegacyRegistryView(base *pkgcore.Registry, reg *pkgcore.ComponentRegistry) *pkgcore.Registry {
	base.Routes = reg.Routes
	base.Config = reg.Config
	base.Features = reg.Features
	base.Permissions = reg.Permissions
	base.Jobs = reg.Jobs
	base.Notifications = reg.Notifications
	base.Events = reg.Events
	base.AuditActions = reg.AuditActions
	base.Retention = reg.Retention
	base.Schedules = reg.Schedules
	return base
}

// assetStandIn is one module's asset-carrying stand-in for the Kernel
// bootstrap: it forwards the module's identity, dependency declarations and
// embedded assets, and declares nothing. Bootstrap's locale merge and
// DependsOn validation therefore run over the real module set, while the
// declarations themselves arrive later, through the wrapped components'
// Init callbacks, into the registry the bootstrap returned.
type assetStandIn struct {
	module pkgcore.Module
}

func (s assetStandIn) Name() string         { return s.module.Name() }
func (s assetStandIn) DependsOn() []string  { return s.module.DependsOn() }
func (s assetStandIn) Migrations() embed.FS { return s.module.Migrations() }
func (s assetStandIn) Locales() embed.FS    { return s.module.Locales() }
func (s assetStandIn) OpenAPISpec() []byte  { return s.module.OpenAPISpec() }

// Register declares nothing: the stand-ins exist for the bootstrap's asset
// merge and dependency validation, not for registration.
func (s assetStandIn) Register(pkgcore.Registrar) error { return nil }

// legacyComponentPrefix names every transition-bridge component. The prefix
// keeps wrapped modules from colliding with the components the module
// packages grow in their own migration (a wrapped authn module becomes
// "legacy.authn" beside the component world's "authn"), and makes every
// artifact the bridge introduces visible as such.
const legacyComponentPrefix = "legacy."

// wrapLegacyModules wraps the host's module set as components. wrapped is the
// registry view the wrapped modules declare into and reg the component
// assembly whose registrations the wrappers read their modules' descriptors
// from; each wrapper's Init calls exactly the module's Register, the module's
// one declaration entry point.
func wrapLegacyModules(modules []pkgcore.Module, wrapped *pkgcore.Registry, reg *pkgcore.ComponentRegistry) ([]pkgcore.Component, error) {
	provided := make(map[string]any, len(modules))
	for _, m := range modules {
		provided[m.Name()] = productToken(m)
	}

	components := make([]pkgcore.Component, 0, len(modules))
	for _, m := range modules {
		requires, err := requirementTokens(m, provided)
		if err != nil {
			return nil, err
		}
		module := m
		descriptor, _ := moduleDescriptor(reg, module.Name())
		components = append(components, pkgcore.Component{
			Name: legacyComponentPrefix + module.Name(),
			// Migrations and the OpenAPI fragment ride the wrapper (their
			// consumers key by the component's own name), but Locales does
			// not: a locale file's ids are prefixed with the MODULE name, and
			// the wrapper's component name carries the transition prefix --
			// the assembly's locale validation refuses that mismatch. The
			// module set's locale resources therefore merge and validate
			// through the asset stand-ins' kernel bootstrap, under the
			// module's own name, exactly where they merged before.
			Migrations:  module.Migrations(),
			OpenAPISpec: module.OpenAPISpec(),
			// The bootstrap-key and system-purpose declarations are static
			// descriptor data, so the wrapper carries the module's descriptor
			// copies of them: the assembly's own readers (the loader's key
			// resolution, the Init-closing purpose collection) then see the
			// wrapper declare exactly what the module's component declares.
			BootstrapKeys:  descriptor.BootstrapKeys,
			SystemPurposes: descriptor.SystemPurposes,
			Requires:       requires,
			Provides:       []any{provided[module.Name()]},
			New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
				return module, nil
			},
			// The purpose registration sits in the module's Init turn, right
			// after Register: a host step later in the assembly (the
			// reference app's platform-credential write among them) acts
			// under a system purpose, so the module's purposes must be
			// registered before any host step, which is the moment the
			// module's own Register used to register them. RegisterSystemPurpose
			// is idempotent, so the assembly's own Init-closing collection
			// over the same declarations registers them again harmlessly.
			Init: func(_ context.Context, _ *pkgcore.ComponentRegistry, _ any) error {
				if regErr := module.Register(wrapped); regErr != nil {
					return regErr
				}
				for _, purpose := range descriptor.SystemPurposes {
					pkgcore.RegisterSystemPurpose(purpose)
				}
				return nil
			},
		})
	}
	return components, nil
}

// moduleDescriptor returns the component descriptor a wrapped module
// describes itself with: the registration entry carrying the module's name as
// both its component name and its module name. A module with no such
// descriptor reports ok == false, and its wrapper then carries no static
// declarations.
func moduleDescriptor(reg *pkgcore.ComponentRegistry, moduleName string) (pkgcore.Component, bool) {
	for _, c := range pkgcore.RegisteredComponents(reg) {
		if c.Name == moduleName && c.Module == moduleName {
			return c, true
		}
	}
	return pkgcore.Component{}, false
}

// productToken is the dependency token of one module's product: a typed nil
// pointer to the module value's own type, the shape every Provides entry and
// Requirement.Token takes.
func productToken(m pkgcore.Module) any {
	return reflect.Zero(reflect.TypeOf(m)).Interface()
}

// requirementTokens maps a module's DependsOn names onto dependency tokens:
// one Requirement per named module, resolved against the products of the
// modules in the set. A DependsOn naming a module that is not part of the
// set fails, mirroring the module bootstrap's own rule.
func requirementTokens(m pkgcore.Module, provided map[string]any) ([]pkgcore.Requirement, error) {
	dependsOn := m.DependsOn()
	if len(dependsOn) == 0 {
		return nil, nil
	}
	requires := make([]pkgcore.Requirement, 0, len(dependsOn))
	for _, name := range dependsOn {
		token, ok := provided[name]
		if !ok {
			return nil, fmt.Errorf("%w: module %q depends on module %q, which is not part of the composed module set",
				pkgcore.ErrMissingDependency, m.Name(), name)
		}
		requires = append(requires, pkgcore.Requirement{Token: token})
	}
	return requires, nil
}
