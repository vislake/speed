package app

import (
	"fmt"
	"sort"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
	pkgconfig "github.com/vislake/speed/go/pkgcore/config"
)

// loader_composition.go carries the loader's composition-configuration
// pipeline: the five sources -- builtin defaults, the project file, the
// environment, the command line and the code override -- merged lowest to
// highest into the one pkgcore.ComponentConfig the assembly plans from.
//
// # Spelling
//
// The whole composition tree travels under the "composition" envelope key: a
// config file spells it as a top-level composition: section, the environment
// as APP_COMPOSITION__DEPLOYMENT and APP_COMPOSITION__COMPONENTS__… (the
// loader's own derived spelling), the command line as
// --composition.deployment=… / --composition.components.<name>.<key>=…
// Component names appear in files and the environment with their dots
// spelled as single underscores (mailer.smtp reads mailer_smtp), because the
// double underscore already marks one level of nesting; on the command line
// the dots stay literal and the name is matched against the registered set
// by longest prefix. A selection value spelled "false" (or "true") in a
// text source reads as its boolean.

// compositionSection is the loader target the project sources (config file,
// environment, command line) resolve the composition into. Deployment and
// Strict carry the static pair; Components carries the dynamic block -- a
// map-typed leaf, so every key nested underneath it belongs to it.
type compositionSection struct {
	Deployment string
	Strict     bool
	Components map[string]any
}

// compositionTarget is the envelope around compositionSection: the tree
// loads under the "composition" key, which is what makes a config file
// section, an APP_COMPOSITION__… variable and a --composition.… flag address
// the same tree.
type compositionTarget struct {
	Composition compositionSection
}

// compositionFlagPrefix is the flag name space the component selections use.
const compositionFlagPrefix = "composition.components."

// loadComposition resolves the five sources into the composition value the
// assembly plans from. loader has already resolved the project sources (the
// config file, the environment and the static flags) into target.
func loadComposition(loader *pkgconfig.Loader, reg *pkgcore.ComponentRegistry, args []string) (pkgcore.ComponentConfig, error) {
	var target compositionTarget
	if err := loader.Load(&target); err != nil {
		return pkgcore.ComponentConfig{}, fmt.Errorf("app: load the composition configuration: %w", err)
	}

	tree := builtinComposition()
	project, err := projectComposition(target, reg)
	if err != nil {
		return pkgcore.ComponentConfig{}, err
	}
	tree = mergeComposition(tree, project)

	flags, err := flagComposition(args, reg)
	if err != nil {
		return pkgcore.ComponentConfig{}, err
	}
	tree = mergeComposition(tree, flags)

	override, present, err := compositionOverrides(reg)
	if err != nil {
		return pkgcore.ComponentConfig{}, err
	}
	if present {
		tree = mergeComposition(tree, override)
	}
	return tree, nil
}

// builtinComposition is the lowest layer: the values compiled into the
// binary. The deployment defaults to standalone (a single-process
// composition excludes no implementation) and the engine's observability
// component participates by default; a host deselects it by naming it false
// in any higher layer.
func builtinComposition() pkgcore.ComponentConfig {
	components := pkgcore.ComponentConfig{}.With("observability", nil)
	return pkgcore.ComponentConfig{}.
		With("deployment", string(pkgcore.DeploymentModeStandalone)).
		With("components", components)
}

// projectComposition converts the loader-resolved project layer -- config
// file, environment and the static command-line flags, already merged by the
// loader's own precedence (flag over environment over file) -- into the next
// layer. An unset deployment or strict value carries no entry, so the
// builtin default underneath stands.
func projectComposition(target compositionTarget, reg *pkgcore.ComponentRegistry) (pkgcore.ComponentConfig, error) {
	tree := pkgcore.ComponentConfig{}
	if target.Composition.Deployment != "" {
		tree = tree.With("deployment", target.Composition.Deployment)
	}
	if target.Composition.Strict {
		tree = tree.With("strict", true)
	}
	if len(target.Composition.Components) > 0 {
		tree = tree.With("components", componentBlock(target.Composition.Components, reg))
	}
	return tree, nil
}

// componentBlock converts a raw components map into the ordered block form:
// every name is resolved against the registered set (a name whose dots are
// spelled as underscores in a text source maps back), every selection value
// is normalized, and the block is ordered by resolved name so a tree built
// from unordered sources still plans deterministically. A name that resolves
// to nothing is passed through unchanged: the assembly's own planning stage
// reports unknown selections, with the registered set and a suggestion, in
// one place.
func componentBlock(raw map[string]any, reg *pkgcore.ComponentRegistry) pkgcore.ComponentConfig {
	resolved := make(map[string]any, len(raw))
	names := make([]string, 0, len(raw))
	for key, value := range raw {
		name := resolveComponentName(key, reg)
		if _, exists := resolved[name]; !exists {
			names = append(names, name)
		}
		resolved[name] = normalizeSelection(value)
	}
	sort.Strings(names)

	block := pkgcore.ComponentConfig{}
	for _, name := range names {
		block = block.With(name, toComponentConfig(resolved[name]))
	}
	return block
}

// resolveComponentName maps one selection key onto a registered component
// name: the key as spelled, or -- when that names nothing and the key is
// text-source spelling -- with each underscore segment rejoined by dots.
// A key resolving to neither is returned unchanged.
func resolveComponentName(key string, reg *pkgcore.ComponentRegistry) string {
	if componentRegistered(reg, key) {
		return key
	}
	if candidate := strings.ReplaceAll(key, "_", "."); componentRegistered(reg, candidate) {
		return candidate
	}
	return key
}

// componentRegistered reports whether name is registered in reg.
func componentRegistered(reg *pkgcore.ComponentRegistry, name string) bool {
	for _, c := range pkgcore.RegisteredComponents(reg) {
		if c.Name == name {
			return true
		}
	}
	return false
}

// registeredComponentNames returns the registered names in registration
// order, for error text.
func registeredComponentNames(reg *pkgcore.ComponentRegistry) []string {
	components := pkgcore.RegisteredComponents(reg)
	names := make([]string, 0, len(components))
	for _, c := range components {
		names = append(names, c.Name)
	}
	return names
}

// normalizeSelection normalizes a selection value that a text source carried
// as its own spelling: the exact strings "false" and "true" read as their
// boolean. Every other value -- nil, a mapping, any other text -- is kept as
// it arrived, because a component block's own keys are typed by the
// component's schema, not here.
func normalizeSelection(value any) any {
	if text, isText := value.(string); isText {
		switch text {
		case "false":
			return false
		case "true":
			return true
		}
	}
	return value
}

// toComponentConfig converts a raw nested map into an ordered
// ComponentConfig; every other value is kept as it arrived.
func toComponentConfig(value any) any {
	if raw, isMap := value.(map[string]any); isMap {
		return pkgcore.NewComponentConfig(raw)
	}
	return value
}

// flagComposition reads the component selections off the command line:
// --composition.components.<name>.<key>=<value> flags, plus the bare
// --composition.components.<name>=false deselection. args is the argument
// list in os.Args[1:] form; a nil args reads the process's own arguments.
// The component name keeps its literal dots and is matched against the
// registered set by longest prefix, so mailer.smtp wins over a registered
// mailer for --composition.components.mailer.smtp.host.
func flagComposition(args []string, reg *pkgcore.ComponentRegistry) (pkgcore.ComponentConfig, error) {
	if args == nil {
		args = processArgs()
	}
	block := pkgcore.ComponentConfig{}
	for i := 0; i < len(args); i++ {
		flagName := strings.TrimLeft(args[i], "-")
		if flagName == args[i] || flagName == "" {
			continue // a positional argument, or a bare "-" / "--".
		}
		value, hasValue := "", false
		if eq := strings.Index(flagName, "="); eq >= 0 {
			flagName, value, hasValue = flagName[:eq], flagName[eq+1:], true
		}
		flagName = strings.ToLower(flagName)
		if !strings.HasPrefix(flagName, compositionFlagPrefix) {
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				return pkgcore.ComponentConfig{}, fmt.Errorf("app: the flag --%s needs a value", flagName)
			}
			value, hasValue = args[i+1], true
			i++
		}

		rest := flagName[len(compositionFlagPrefix):]
		name, keyPath, err := splitFlagComponent(rest, reg)
		if err != nil {
			return pkgcore.ComponentConfig{}, err
		}
		if keyPath == "" {
			block = block.With(name, normalizeSelection(value))
			continue
		}
		existing, _ := configGet(block, name)
		block = block.With(name, setNested(asComponentConfig(existing), keyPath, value))
	}
	return pkgcore.ComponentConfig{}.With("components", block), nil
}

// splitFlagComponent splits the remainder of a composition flag name into
// the registered component name (the longest registered name the remainder
// starts with, at a segment boundary) and the dotted key path underneath it.
func splitFlagComponent(rest string, reg *pkgcore.ComponentRegistry) (string, string, error) {
	best := ""
	for _, c := range pkgcore.RegisteredComponents(reg) {
		if rest != c.Name && !strings.HasPrefix(rest, c.Name+".") {
			continue
		}
		if len(c.Name) > len(best) {
			best = c.Name
		}
	}
	if best == "" {
		return "", "", fmt.Errorf("app: the composition flag names component %q, which is not registered; registered components: %s",
			rest, strings.Join(registeredComponentNames(reg), ", "))
	}
	keyPath := strings.TrimPrefix(strings.TrimPrefix(rest, best), ".")
	return best, keyPath, nil
}

// setNested sets value at the dotted key path under config, creating nested
// ComponentConfigs along the way.
func setNested(config pkgcore.ComponentConfig, path string, value any) pkgcore.ComponentConfig {
	segment, rest, nested := strings.Cut(path, ".")
	if !nested {
		return config.With(segment, value)
	}
	child, _ := configGet(config, segment)
	return config.With(segment, setNested(asComponentConfig(child), rest, value))
}

// asComponentConfig views a raw value as a ComponentConfig: a nested
// ComponentConfig is one already, a raw map converts (sorted), and anything
// else -- absent, or a non-mapping value a flag then layers over -- starts
// empty.
func asComponentConfig(value any) pkgcore.ComponentConfig {
	switch v := value.(type) {
	case pkgcore.ComponentConfig:
		return v
	case map[string]any:
		return pkgcore.NewComponentConfig(v)
	default:
		return pkgcore.ComponentConfig{}
	}
}

// mergeComposition merges higher over lower, per key: two mappings merge
// recursively (so a component block from one source extends rather than
// replaces the same component's block from another), every other pair takes
// the higher value. A key keeps the position its lower layer gave it; a key
// the higher layer adds lands after the keys already present, so the plan's
// tie-breaking order is stable across source combinations.
func mergeComposition(lower, higher pkgcore.ComponentConfig) pkgcore.ComponentConfig {
	out := lower
	for _, key := range higher.Keys() {
		high, ok := configGet(higher, key)
		if !ok {
			continue
		}
		low, exists := configGet(out, key)
		if exists && isMapping(low) && isMapping(high) {
			out = out.With(key, mergeComposition(asComponentConfig(low), asComponentConfig(high)))
			continue
		}
		out = out.With(key, high)
	}
	return out
}

// isMapping reports whether a raw value is a mapping: a ComponentConfig or a
// plain map.
func isMapping(value any) bool {
	switch value.(type) {
	case pkgcore.ComponentConfig, map[string]any:
		return true
	default:
		return false
	}
}

// configGet reads a raw value out of a ComponentConfig, reporting whether
// the key is present.
func configGet(c pkgcore.ComponentConfig, key string) (any, bool) {
	return c.Get(key)
}

// compositionOverrides reads the code-override layer the host may have Put
// into the registry before the loader runs. Its one spelling is
// CompositionOverrides; more than one such value fails (an assembly has one
// override layer), and none is a legitimate assembly with no code
// overrides.
func compositionOverrides(reg *pkgcore.ComponentRegistry) (pkgcore.ComponentConfig, bool, error) {
	override, ok, err := pkgcore.GetOptional[CompositionOverrides](reg)
	if err != nil {
		return pkgcore.ComponentConfig{}, false, fmt.Errorf("app: the composition configuration's code-override layer: %w", err)
	}
	if !ok {
		return pkgcore.ComponentConfig{}, false, nil
	}
	return override.Config, true, nil
}
