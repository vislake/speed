package app

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	pkgconfig "github.com/vislake/speed/go/pkgcore/config"
)

// component_config.go implements the engine's half of the component
// configuration contract: the pkgcore.ComponentConfigResolver that upgrades
// a selected component's configuration block into the five-source-merged
// value its New receives. It resolves through the same pkgcore/config
// Loader the loader stage already built (loader.go), so a component field
// and a declared bootstrap key can never resolve differently for one
// assembly, and adding the engine's resolver changes nothing for a
// component whose schema declares no source-opening option.
//
// # Layering
//
// The block the composition carried is the base: every key it holds reaches
// the component unless the schema says otherwise, so a field with no tag
// keeps the plain block-supplied behaviour. Over that base, each field the
// schema marks expose (the derive option implies it) is resolved at its own
// key path through the loader's five-source chain -- command-line flag over
// environment variable over the optional config file over the root-key
// derivation over the declared defaults table -- and whatever the chain
// supplies is written over the block. A field carrying the skip option
// ("-") resolves from no source at all, the block included, so its key
// (and, for a container, its whole subtree) is dropped from the base. A
// field whose value no source supplies keeps the block's value, and a
// derive-tagged field's resolved key material -- explicit value, derivation
// or declared default -- is written into the block as the bytes it is, so
// the assembly's required check sees the field as supplied.
//
// # Key paths and their spellings
//
// A field's key path is the component's namespace prefix (pkgcore's
// Component.ConfigNamespace, mirrored by componentConfigPrefix) followed by
// the field's local key path. The spellings follow the loader's own rules:
// the flag is --<key path>, the environment variable is the pinned name
// (config "env=NAME") or the loader's derived spelling of the key path
// under its prefix, and a config file entry sits at the key path itself.

// componentConfigResolver is the engine's pkgcore.ComponentConfigResolver.
// It carries the loader every other configuration read of the assembly runs
// on, and the registry, whose registered descriptors name the namespace
// prefix each component's schema fields resolve under.
type componentConfigResolver struct {
	loader *pkgconfig.Loader
	reg    *pkgcore.ComponentRegistry
}

var _ pkgcore.ComponentConfigResolver = (*componentConfigResolver)(nil)

// newComponentConfigResolver returns the resolver the loader stage publishes
// into reg before the Prepare stage runs.
func newComponentConfigResolver(loader *pkgconfig.Loader, reg *pkgcore.ComponentRegistry) *componentConfigResolver {
	return &componentConfigResolver{loader: loader, reg: reg}
}

// ResolveComponentConfig implements pkgcore.ComponentConfigResolver.
func (r *componentConfigResolver) ResolveComponentConfig(componentName string, schema any, fileConfig pkgcore.ComponentConfig) (pkgcore.ComponentConfig, error) {
	fields, err := pkgconfig.Describe(schema)
	if err != nil {
		return pkgcore.ComponentConfig{}, err
	}
	prefix, err := componentConfigPrefix(r.reg, componentName)
	if err != nil {
		return pkgcore.ComponentConfig{}, err
	}

	merged := fileConfig
	var declarations []pkgconfig.Declaration
	for _, field := range fields {
		switch {
		case field.Skip:
			merged = removeConfigKey(merged, field.Key)
		case field.Expose:
			key := strings.ToLower(prefix + field.Key)
			declarations = append(declarations, pkgconfig.Declaration{
				Key:    key,
				Format: declarationFormat(field),
				Env:    field.Env,
			})
		}
	}
	if len(declarations) == 0 {
		return merged, nil
	}

	values, err := r.loader.ResolveDeclarations(declarations)
	if err != nil {
		return pkgcore.ComponentConfig{}, err
	}
	for _, field := range fields {
		if field.Skip || !field.Expose {
			continue
		}
		value, supplied := values[strings.ToLower(prefix+field.Key)]
		if !supplied {
			continue
		}
		merged = setConfigKey(merged, field.Key, value)
	}
	return merged, nil
}

// declarationFormat maps a schema field onto the declaration format whose
// conversion rules its explicit values take: key material resolves as the
// hexkey declaration it is, a bool or a signed integer field takes the
// conversion its own kind has (the same strconv rules the strict decode
// applies, reported at the loader instead), and every other type is carried
// verbatim for the decode to convert, exactly as a block-supplied value is.
// A time.Duration is deliberately not an integer here: a text source
// spells it with Go's duration syntax, which the decode parses and the
// integer conversion does not.
func declarationFormat(field pkgconfig.FieldSummary) string {
	switch {
	case field.Derive:
		return pkgconfig.FormatHexKey
	case field.Type == nil:
		return pkgconfig.FormatString
	case field.Type.Kind() == reflect.Bool:
		return pkgconfig.FormatBool
	case field.Type != durationFieldType && isSignedIntKind(field.Type.Kind()):
		return pkgconfig.FormatInt
	default:
		return pkgconfig.FormatString
	}
}

// durationFieldType is the integer-like type a text source spells as a
// duration, so it is carried verbatim rather than parsed as an integer.
var durationFieldType = reflect.TypeOf(time.Duration(0))

// isSignedIntKind reports whether a kind is one of Go's signed integer
// kinds.
func isSignedIntKind(kind reflect.Kind) bool {
	switch kind {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	default:
		return false
	}
}

// componentConfigPrefix returns the namespace prefix componentName's schema
// fields resolve under. It mirrors pkgcore's own key-path rule
// (config_schema.go's configKeyPrefix, unexported, and restated here because
// this module reaches pkgcore only through its exported surface): the
// default "components.<name>.", nothing at all for NoConfigNamespace, and
// the declared namespace -- normalized to end in a dot -- otherwise. The
// assembly's own one-key-path-per-source check runs the same rule over the
// same descriptors, so a drift here would surface as an unresolved field.
//
// The namespace is read off the registry's descriptors rather than the
// process-wide registration, so a component a host registered on this
// registry alone resolves under the namespace it declares.
func componentConfigPrefix(reg *pkgcore.ComponentRegistry, componentName string) (string, error) {
	for _, c := range pkgcore.RegisteredComponents(reg) {
		if c.Name != componentName {
			continue
		}
		switch namespace := c.ConfigNamespace; namespace {
		case "":
			return "components." + componentName + ".", nil
		case pkgcore.NoConfigNamespace:
			return "", nil
		default:
			if !strings.HasSuffix(namespace, ".") {
				namespace += "."
			}
			return namespace, nil
		}
	}
	return "", fmt.Errorf("component %q is not registered in this assembly; a schema's key path has no namespace to resolve under", componentName)
}

// setConfigKey returns tree with value set at the local dotted key path. An
// entry already present at a segment -- whatever its case, the way config
// keys match -- keeps its spelling and its position; a segment the tree does
// not carry is appended as a nested mapping. A segment carrying a
// non-mapping value is replaced by the mapping the schema's shape demands.
func setConfigKey(tree pkgcore.ComponentConfig, path string, value any) pkgcore.ComponentConfig {
	segment, rest, nested := strings.Cut(path, ".")
	key, _ := foldConfigKey(tree, segment)
	if !nested {
		return tree.With(key, value)
	}
	child, _ := tree.Get(key)
	return tree.With(key, setConfigKey(asComponentConfig(child), rest, value))
}

// removeConfigKey returns tree without the entry at the local dotted key
// path, matched segment by segment the way config keys match. A parent left
// empty by the removal -- a mapping the tree carried only for the removed
// key -- is dropped with it, so a skipped container's subtree leaves no
// trace in the block.
func removeConfigKey(tree pkgcore.ComponentConfig, path string) pkgcore.ComponentConfig {
	segment, rest, nested := strings.Cut(path, ".")
	key, found := foldConfigKey(tree, segment)
	if !found {
		return tree
	}
	if !nested {
		return dropConfigKey(tree, key)
	}
	raw, _ := tree.Get(key)
	if !isMapping(raw) {
		return tree
	}
	trimmed := removeConfigKey(asComponentConfig(raw), rest)
	if trimmed.Len() == 0 {
		return dropConfigKey(tree, key)
	}
	return tree.With(key, trimmed)
}

// dropConfigKey returns tree without the entry under key. A ComponentConfig
// derives new values rather than mutating one, so a removal rebuilds the
// tree from the entries around the dropped key.
func dropConfigKey(tree pkgcore.ComponentConfig, key string) pkgcore.ComponentConfig {
	next := pkgcore.ComponentConfig{}
	for _, k := range tree.Keys() {
		if k == key {
			continue
		}
		value, _ := tree.Get(k)
		next = next.With(k, value)
	}
	return next
}

// foldConfigKey resolves a key path segment against a tree's own keys,
// matching case-insensitively the way config keys resolve, and reports
// whether the tree carries it. The returned key is the tree's spelling of
// the segment, or the segment itself when the tree carries neither.
func foldConfigKey(tree pkgcore.ComponentConfig, segment string) (string, bool) {
	if _, ok := tree.Get(segment); ok {
		return segment, true
	}
	for _, key := range tree.Keys() {
		if strings.EqualFold(key, segment) {
			return key, true
		}
	}
	return segment, false
}
