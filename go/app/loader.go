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
//  1. It loads the host's configuration target and the platform key-material
//     target through one pkgcore/config Loader -- the same options, the same
//     sources, one pass each -- so a value can never resolve differently for
//     the two halves.
//  2. It resolves the composition configuration from its five sources
//     (builtin defaults, project file, environment, command line and the
//     host's code override) and publishes it, with the host's configuration
//     targets, into the registry.
//  3. It resolves every registered component's declared BootstrapKeys on the
//     same loader chain, verifies that each declared key binds the host's
//     configuration targets, and publishes the results as the assembly's
//     by-purpose BootstrapMaterial source. A declared key that binds nothing
//     fails the load, naming the stage, the declaring component, the reason
//     and the remedy -- before any component is constructed.
//
// The loader is engine-provided and runs unconditionally: no composition can
// deselect it, because nothing can be chosen before it has run.

// LoadSpec names what the loader loads: the host's bootstrap configuration
// target, the platform key material's target, and the pkgcore/config options
// the load runs with. It is the configuration half of an assembly that the
// engine, not the host's option set, carries -- the new-world spelling of
// what ConfigSpec wires for the transition adapters.
type LoadSpec struct {
	// Host is the host's bootstrap configuration target: a non-nil pointer
	// to the struct the loader fills with the host's own keys. Required.
	Host any

	// Platform is the platform key material's target, typically
	// `*PlatformConfig` or a value of that type embedded unskipped in the
	// host's own target. Optional: nil loads and verifies the host target
	// alone, which is the shape of a host that declares every bootstrap key
	// path on its own target.
	Platform *PlatformConfig

	// Options are the loader options the load runs with -- the same set
	// ConfigOption names (ConfigFile, ConfigArgs, ConfigEnvPrefix,
	// ConfigRootKey, ConfigRootKeyEnv, ConfigKeyDerivation). They are
	// required: the engine ships no implicit source configuration.
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
	if spec.Platform != nil {
		if err := loader.Load(spec.Platform); err != nil {
			return fmt.Errorf("app: load the platform key material: %w", err)
		}
	}

	composition, err := loadComposition(loader, reg, spec.Args)
	if err != nil {
		return err
	}

	reg.Put(spec.Host)
	if spec.Platform != nil && !sameTarget(spec.Host, spec.Platform) {
		reg.Put(spec.Platform)
	}
	reg.Put(composition)

	material, err := resolveBootstrapMaterial(reg, spec)
	if err != nil {
		return err
	}
	reg.Put(material)
	return nil
}

// resolveBootstrapMaterial resolves every registered component's declared
// bootstrap keys and proves each one binds the host's configuration targets.
// The binding check is the loader's own reading of the declaration's
// contract: a declared key path is a value the loader must be able to
// resolve, so one that maps onto no field of the host target or of the
// platform target is a key whose contract is silently unreachable -- the
// component's wiring reads material no source can ever supply -- and it
// fails the load, naming the stage, the declaring component, the reason and
// the remedy.
func resolveBootstrapMaterial(reg *pkgcore.ComponentRegistry, spec LoadSpec) (*pkgcore.BootstrapMaterial, error) {
	var entries []pkgcore.BootstrapMaterialEntry
	var problems []error
	for _, c := range pkgcore.RegisteredComponents(reg) {
		for _, key := range c.BootstrapKeys {
			component := fmt.Sprintf("component %q", c.Name)
			if _, err := pkgcore.BootstrapKeyPurpose(key.Key); err != nil {
				problems = append(problems, fmt.Errorf(
					"%s declares an invalid bootstrap key path for its %q key: %w; give the declaration a non-empty dotted path with no empty segment",
					component, key.Key, err))
				continue
			}
			value, bound := lookupDeclaredKey(spec, key.Key)
			if !bound {
				problems = append(problems, fmt.Errorf(
					"%s declares bootstrap key %q, which maps onto no field of the host configuration target or of the platform key target; add a field for that key path to the host's configuration target (or embed the platform key-material declaration that carries it)",
					component, key.Key))
				continue
			}
			entries = append(entries, pkgcore.BootstrapMaterialEntry{KeyPath: key.Key, Value: value})
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf(
			"app: the configuration targets must bind every bootstrap key the registered components declared (stage prepare): %w",
			errors.Join(problems...))
	}
	return pkgcore.NewBootstrapMaterial(entries), nil
}

// lookupDeclaredKey reads the resolved value of one declared key path from
// the host target, then the platform target: Verify proves the path is bound
// and Lookup reads what it resolved to.
func lookupDeclaredKey(spec LoadSpec, keyPath string) (any, bool) {
	if pkgconfig.Verify(spec.Host, []string{keyPath}) == nil {
		return pkgconfig.Lookup(spec.Host, keyPath)
	}
	if spec.Platform != nil && pkgconfig.Verify(spec.Platform, []string{keyPath}) == nil {
		return pkgconfig.Lookup(spec.Platform, keyPath)
	}
	return nil, false
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

// sameTarget reports whether two configuration targets are the same pointer,
// the shape a host passing one value as both its own target and the platform
// target would have (in which case the loader publishes it once).
func sameTarget(a, b any) bool {
	ra, rb := reflect.ValueOf(a), reflect.ValueOf(b)
	return ra.Kind() == reflect.Pointer && rb.Kind() == reflect.Pointer && ra.Pointer() == rb.Pointer()
}

// processArgs returns the process's own command-line arguments, the default
// source of the composition layer's component flags.
func processArgs() []string {
	return os.Args[1:]
}
