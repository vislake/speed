package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
	pkgconfig "github.com/vislake/speed/go/pkgcore/config"
)

// loader.go carries the engine's loader: the bootstrap root of every
// assembly. It runs before the first stage of the seven-stage drive, because
// what it resolves is what the Prepare stage plans from and what every
// component's Prepare callback reads -- the assembly cannot even choose its
// components until the composition configuration exists.
//
// The loader does four things, in order:
//
//  1. It loads the host's configuration target through a pkgcore/config
//     Loader -- the same options and the same sources the declared bootstrap
//     keys resolve on -- so a host key and a declared key can never resolve
//     differently for one assembly.
//  2. It resolves the composition configuration from its five sources
//     (builtin defaults, project file, environment, command line and the
//     host's code override) and publishes it, with the host's configuration
//     target, into the registry.
//  3. It resolves every registered component's declared key material on the
//     same loader chain -- through the declaration-driven entry, with no
//     host struct field behind it -- and publishes the results as the
//     assembly's by-key-path BootstrapMaterial source. A component declares
//     that material on its BootstrapKeys seat or as the derive fields of its
//     ConfigSchema; both fold into one declaration list, at the one key path
//     each resolves at. A declaration that is not resolvable as one schema
//     fails the load, naming the stage, the declaring component, the reason
//     and the remedy -- before any component is constructed.
//  4. It publishes the engine's component configuration resolver
//     (component_config.go), which the Prepare stage reads to upgrade each
//     selected component's configuration block into the five-source-merged
//     value its New receives. A resolver the registry already carries -- a
//     host's own implementation, put before the load -- stays the one the
//     Prepare stage reads.
//
// The loader is engine-provided and runs unconditionally: no composition can
// deselect it, because nothing can be chosen before it has run.

// LoadSpec names what the loader loads: the host's bootstrap configuration
// target and the pkgcore/config options the load runs with. It is the
// configuration half of an assembly the engine carries. The declared
// bootstrap keys are not named here: the loader reads them off the
// registered components, and their values resolve on the same chain into
// the published BootstrapMaterial.
type LoadSpec struct {
	// Host is the host's bootstrap configuration target: a non-nil pointer
	// to the struct the loader fills with the host's own keys. Required.
	Host any

	// Options are the loader options the load runs with -- the same set
	// ConfigOption names (ConfigFile, ConfigArgs, ConfigEnvPrefix,
	// ConfigRootKey, ConfigRootKeyEnv, ConfigKeyDerivation,
	// ConfigDevDefaults). They are required: the engine ships no implicit
	// source configuration.
	Options []ConfigOption

	// Args are the command-line arguments the composition layer's component
	// flags are scanned from, in os.Args[1:] form. Nil reads the process's
	// own arguments, which is the production shape; a test injects an empty
	// slice the way ConfigArgs does for the target loads.
	Args []string

	// Overrides, when non-nil, is the code-override layer the loader
	// publishes into the registry before reading it back: the spelling for
	// a caller that does not hold the registry itself (RunAssembly), where
	// a caller that does simply puts a CompositionOverrides value directly.
	// At most one override layer may exist; both spellings at once is the
	// assembly's own ambiguity error.
	Overrides *CompositionOverrides
}

// CompositionOverrides is the code-override layer of the composition
// configuration: the highest of the five sources. A host builds one in code
// -- the selections and parameters its composition needs regardless of what
// any file or environment says -- and Puts it into the registry before the
// loader runs; the loader merges it last, so every key it carries wins.
//
// The layer's key order is preserved, so a host that needs a specific plan
// order for tied components states it here (the project layers are orderless
// and plan in sorted order).
type CompositionOverrides struct {
	// Config is the composition tree, built with
	// pkgcore.ComponentConfig.With so its key order survives: the root keys
	// are deployment, strict and components, and components holds one entry
	// per selected component.
	Config pkgcore.ComponentConfig
}

// Load runs the loader against reg: the configuration targets, the
// composition configuration and the bootstrap material source, each
// published into the registry's by-type context. It must run before
// ComponentRegistry.Prepare; Assemble is the caller that guarantees the
// order.
func Load(ctx context.Context, reg *pkgcore.ComponentRegistry, spec LoadSpec) error {
	if reg == nil {
		return errors.New("app: Load needs a component registry to publish into")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !isNonNilPointer(spec.Host) {
		return errors.New("app: LoadSpec.Host must be the host's non-nil configuration target (a pointer to a struct)")
	}
	if spec.Overrides != nil {
		reg.Put(*spec.Overrides)
	}

	loader := pkgconfig.New(spec.Options...)
	if err := loader.Load(spec.Host); err != nil {
		return fmt.Errorf("app: load the host configuration: %w", err)
	}

	composition, err := loadComposition(loader, reg, spec.Args)
	if err != nil {
		return err
	}

	reg.Put(spec.Host)
	reg.Put(composition)

	material, err := resolveBootstrapMaterial(loader, reg)
	if err != nil {
		return err
	}
	reg.Put(material)

	// The component configuration resolver, published before Prepare so the
	// five-source merge reaches every schema-carrying component's New. A
	// resolver already in the by-type context is a host's own and stays the
	// one in use: publishing a second one of the same type would make every
	// read of it ambiguous.
	_, resolverPresent, resolverErr := pkgcore.GetOptional[pkgcore.ComponentConfigResolver](reg)
	if resolverErr != nil {
		return fmt.Errorf("app: the component configuration resolver: %w", resolverErr)
	}
	if !resolverPresent {
		reg.Put(newComponentConfigResolver(loader, reg))
	}
	return nil
}

// resolveBootstrapMaterial resolves every registered component's declared key
// material on loader's own source chain and publishes the results as the
// assembly's by-key-path material source. The declarations are read straight
// off the components -- there is no host struct field behind a declared key,
// so nothing binds it and nothing can fail to bind -- and a declaration whose
// value no source supplies simply resolves to nothing, which leaves the
// consumer that needs it to report the missing material itself. A schema's
// derive field and a BootstrapKeys declaration resolve identically, at the
// same key path on the same chain, so a component's Prepare callback reads
// key material from this source whether the declaring component still spells
// it on the seat or declares it as its own configuration field.
//
// What does fail here, before anything is constructed, is a declaration set
// that cannot be resolved as one schema: a malformed key path, a format
// outside the closed set, a Sensitive key with no Description, or two
// components declaring one key path differently. Every problem names the
// stage, the declaring component, the reason and the remedy.
func resolveBootstrapMaterial(loader *pkgconfig.Loader, reg *pkgcore.ComponentRegistry) (*pkgcore.BootstrapMaterial, error) {
	decls, problems := declaredBootstrapKeys(reg)
	if len(problems) > 0 {
		return nil, fmt.Errorf(
			"app: the bootstrap key declarations of the registered components must be resolvable (stage prepare): %w",
			errors.Join(problems...))
	}

	values, err := loader.ResolveDeclarations(decls)
	if err != nil {
		return nil, fmt.Errorf("app: resolve the declared bootstrap keys (stage prepare): %w", err)
	}

	entries := make([]pkgcore.BootstrapMaterialEntry, 0, len(decls))
	for _, decl := range decls {
		value, resolved := values[decl.Key]
		if !resolved {
			continue
		}
		entries = append(entries, pkgcore.BootstrapMaterialEntry{KeyPath: decl.Key, Value: value})
	}
	return pkgcore.NewBootstrapMaterial(entries), nil
}

// declaredFormats is the closed set of declaration formats the engine accepts,
// spelled with the resolver's own constants. It guards the declarations of
// components whose descriptors never passed the legacy seat's validation
// (validateBootstrapKey), so a typo fails the assembly naming the component
// rather than the resolver.
var declaredFormats = map[string]struct{}{
	pkgconfig.FormatString: {},
	pkgconfig.FormatInt:    {},
	pkgconfig.FormatBool:   {},
	pkgconfig.FormatHexKey: {},
}

// declaredBootstrapKeys collects every registered component's declared key
// material, in registration order, as the resolver's declaration list,
// validating each declaration on the way. A component declares its
// process-start key material two ways, and both are gathered here: the
// BootstrapKeys seat (a host's own keys, which no module in this repository
// declares any longer) and the derive fields of its ConfigSchema (the
// modules' keys, resolved at the final key path the component's namespace
// declares -- the same path the engine's configuration resolver resolves the
// field at, so the published material and the component's own configuration
// carry one value). A key path two components declare identically -- the
// shape the transition bridge's wrappers produce, since a wrapper carries
// its module descriptor's own declarations -- is one declaration and is
// collapsed; one declared differently is a conflict, because one key path has
// one resolution per assembly and which component's declaration won would be
// undecidable. The returned problems are the assembly's own, ready to be
// joined into the stage-prepare refusal.
func declaredBootstrapKeys(reg *pkgcore.ComponentRegistry) ([]pkgconfig.Declaration, []error) {
	var decls []pkgconfig.Declaration
	var problems []error
	owner := make(map[string]string)                 // key path -> first declaring component name
	content := make(map[string]pkgcore.BootstrapKey) // key path -> first declaration

	fold := func(component string, key pkgcore.BootstrapKey) {
		if _, err := pkgcore.BootstrapKeyPurpose(key.Key); err != nil {
			problems = append(problems, fmt.Errorf(
				"%s declares an invalid bootstrap key path for its %q key: %w; give the declaration a non-empty dotted path with no empty segment",
				component, key.Key, err))
			return
		}
		if _, known := declaredFormats[key.Format]; !known {
			problems = append(problems, fmt.Errorf(
				"%s declares bootstrap key %q with format %q, want one of %q, %q, %q or %q; correct the component's declaration",
				component, key.Key, key.Format,
				pkgconfig.FormatString, pkgconfig.FormatInt, pkgconfig.FormatBool, pkgconfig.FormatHexKey))
			return
		}
		if key.Sensitive && key.Description == "" {
			problems = append(problems, fmt.Errorf(
				"%s declares bootstrap key %q as Sensitive but carries no Description; a secret key's contract cannot be left unwritten",
				component, key.Key))
			return
		}
		if first, seen := owner[key.Key]; seen {
			if content[key.Key] == key {
				return // one declaration, carried twice
			}
			problems = append(problems, fmt.Errorf(
				"%s declares bootstrap key %q differently from %s; one key path has one declaration",
				component, key.Key, first))
			return
		}
		owner[key.Key] = component
		content[key.Key] = key
		decls = append(decls, pkgconfig.Declaration{Key: key.Key, Format: key.Format})
	}

	for _, c := range pkgcore.RegisteredComponents(reg) {
		component := fmt.Sprintf("component %q", c.Name)
		for _, key := range c.BootstrapKeys {
			fold(component, key)
		}
		declared, err := schemaBootstrapKeys(reg, c)
		if err != nil {
			problems = append(problems, fmt.Errorf(
				"%s declares a configuration schema whose key material cannot be read: %w", component, err))
			continue
		}
		for _, key := range declared {
			fold(component, key)
		}
	}
	return decls, problems
}

// schemaBootstrapKeys returns the process-start key material a component's
// ConfigSchema declares: one entry per derive-tagged field, at the field's
// final key path -- the namespace prefix the component's registration
// declares, then the field's local key path. The format is the key-material
// shape the derive option promises, so a schema-derived declaration and a
// BootstrapKeys declaration of one path fold onto each other; everything the
// documentation side carries (the field's description and default) stays on
// the schema, where DescribeComponentSchema reads it.
func schemaBootstrapKeys(reg *pkgcore.ComponentRegistry, c pkgcore.Component) ([]pkgcore.BootstrapKey, error) {
	if c.ConfigSchema == nil {
		return nil, nil
	}
	fields, err := pkgconfig.Describe(c.ConfigSchema)
	if err != nil {
		return nil, err
	}
	prefix, err := componentConfigPrefix(reg, c.Name)
	if err != nil {
		return nil, err
	}
	var keys []pkgcore.BootstrapKey
	for _, field := range fields {
		if !field.Derive {
			continue
		}
		keys = append(keys, pkgcore.BootstrapKey{
			Key:    strings.ToLower(prefix + field.Key),
			Format: pkgconfig.FormatHexKey,
		})
	}
	return keys, nil
}

// isNonNilPointer reports whether v is a non-nil pointer.
func isNonNilPointer(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.Kind() == reflect.Pointer && !rv.IsNil()
}

// processArgs returns the process's own command-line arguments, the default
// source of the composition layer's component flags.
func processArgs() []string {
	return os.Args[1:]
}
