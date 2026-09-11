package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/vislake/speed/go/pkgcore"
	pkgconfig "github.com/vislake/speed/go/pkgcore/config"
)

// loader.go carries the engine's loader: the bootstrap root of every
// assembly. It runs before the first stage of the seven-stage drive, because
// what it resolves is what the Prepare stage plans from and what every
// component's Prepare callback reads -- the assembly cannot even choose its
// components until the composition configuration exists.
//
// The loader does three things, in order:
//
//  1. It loads the host's configuration target through a pkgcore/config
//     Loader -- the same options and the same sources the declared bootstrap
//     keys resolve on -- so a host key and a declared key can never resolve
//     differently for one assembly.
//  2. It resolves the composition configuration from its five sources
//     (builtin defaults, project file, environment, command line and the
//     host's code override) and publishes it, with the host's configuration
//     target, into the registry.
//  3. It resolves every registered component's declared BootstrapKeys on the
//     same loader chain -- through the declaration-driven entry, with no
//     host struct field behind them -- and publishes the results as the
//     assembly's by-key-path BootstrapMaterial source. A declaration that is
//     not resolvable as one schema fails the load, naming the stage, the
//     declaring component, the reason and the remedy -- before any component
//     is constructed.
//
// The loader is engine-provided and runs unconditionally: no composition can
// deselect it, because nothing can be chosen before it has run.

// LoadSpec names what the loader loads: the host's bootstrap configuration
// target and the pkgcore/config options the load runs with. It is the
// configuration half of an assembly that the engine, not the host's option
// set, carries -- the new-world spelling of what ConfigSpec wires for the
// transition adapters. The declared bootstrap keys are not named here: the
// loader reads them off the registered components, and their values resolve
// on the same chain into the published BootstrapMaterial.
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
	return nil
}

// resolveBootstrapMaterial resolves every registered component's declared
// bootstrap keys on loader's own source chain and publishes the results as
// the assembly's by-key-path material source. The declarations are read
// straight off the components -- there is no host struct field behind a
// declared key, so nothing binds it and nothing can fail to bind -- and a
// declaration whose value no source supplies simply resolves to nothing,
// which leaves the consumer that needs it to report the missing material
// itself.
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

// declaredBootstrapKeys collects every registered component's BootstrapKeys,
// in registration order, as the resolver's declaration list, validating each
// declaration on the way. A key path two components declare identically --
// the shape the transition bridge's wrappers produce, since a wrapper carries
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

	for _, c := range pkgcore.RegisteredComponents(reg) {
		component := fmt.Sprintf("component %q", c.Name)
		for _, key := range c.BootstrapKeys {
			if _, err := pkgcore.BootstrapKeyPurpose(key.Key); err != nil {
				problems = append(problems, fmt.Errorf(
					"%s declares an invalid bootstrap key path for its %q key: %w; give the declaration a non-empty dotted path with no empty segment",
					component, key.Key, err))
				continue
			}
			if _, known := declaredFormats[key.Format]; !known {
				problems = append(problems, fmt.Errorf(
					"%s declares bootstrap key %q with format %q, want one of %q, %q, %q or %q; correct the component's declaration",
					component, key.Key, key.Format,
					pkgconfig.FormatString, pkgconfig.FormatInt, pkgconfig.FormatBool, pkgconfig.FormatHexKey))
				continue
			}
			if key.Sensitive && key.Description == "" {
				problems = append(problems, fmt.Errorf(
					"%s declares bootstrap key %q as Sensitive but carries no Description; a secret key's contract cannot be left unwritten",
					component, key.Key))
				continue
			}
			if first, seen := owner[key.Key]; seen {
				if content[key.Key] == key {
					continue // one declaration, carried twice
				}
				problems = append(problems, fmt.Errorf(
					"%s declares bootstrap key %q differently from %s; one key path has one declaration",
					component, key.Key, first))
				continue
			}
			owner[key.Key] = component
			content[key.Key] = key
			decls = append(decls, pkgconfig.Declaration{Key: key.Key, Format: key.Format})
		}
	}
	return decls, problems
}

// componentConfigOf reads one component's resolved configuration block out
// of the composition configuration published in reg: the components.<name>
// subtree. It reports present == false when the composition carries no block
// for name (the component then takes its schema's zero values), and is the
// reading a component's Prepare callback uses to see its own block -- the
// one callback that runs before any instance, and therefore before the
// per-component cfg parameter New receives exists.
func componentConfigOf(reg *pkgcore.ComponentRegistry, name string) (pkgcore.ComponentConfig, bool, error) {
	whole, err := pkgcore.Get[pkgcore.ComponentConfig](reg)
	if err != nil {
		return pkgcore.ComponentConfig{}, false, fmt.Errorf("app: the composition configuration: %w", err)
	}
	raw, ok := configGet(whole, "components")
	if !ok {
		return pkgcore.ComponentConfig{}, false, nil
	}
	block, ok := configGet(asComponentConfig(raw), name)
	if !ok {
		return pkgcore.ComponentConfig{}, false, nil
	}
	return asComponentConfig(block), true, nil
}

// decodeComponentConfig decodes one component's resolved block into target,
// strictly: an unknown key fails, listing the schema's accepted keys. It is
// a component's own reading of configuration during Prepare.
func decodeComponentConfig(reg *pkgcore.ComponentRegistry, name string, target any) error {
	block, _, err := componentConfigOf(reg, name)
	if err != nil {
		return err
	}
	if err := block.Decode(target); err != nil {
		return fmt.Errorf("app: component %q configuration: %w", name, err)
	}
	return nil
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
