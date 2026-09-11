package config

// declaration.go carries the declaration-driven resolution entry:
// resolving a list of declared bootstrap keys without a Go struct behind
// them. It is the path a declared key takes when the declaring module is
// its only owner -- no host keeps an independent, deployment-specific
// spelling of the key on a loader target of its own -- which is the shape
// of the platform key material every host inherits rather than spells out.
// A key a host does own keeps the reflection path: the host's target
// struct is then the one source of the key's type and default, and Load
// resolves it there (see the package comment's key-material section).
//
// The declaration path runs the same pipeline as the reflection path: the
// five-source chain (flag, environment, file, root-key derivation,
// declared defaults), the empty-value rule, the text conversion rules and
// the key material's three-stage order are the same functions, so a value
// resolves identically either way. What differs is the schema's source
// (declarations instead of struct tags), the output (a map addressed by
// key path instead of struct fields) and the name of the lowest-priority
// source in error text: a declaration has no struct whose values could
// stand, so the tail of the consulted-source list names the declared
// defaults table instead.

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
)

// Declaration is one bootstrap key the declaration-driven resolver must
// produce a value for: the dotted key path and the format naming the
// value's shape. It is the minimal spelling of a declaring component's
// bootstrap key that crosses into this package -- this package sits on the
// dependency floor and must not import the pkgcore root package -- and it
// mirrors the declaring side's own fields: the key path is the flag name,
// the base of the environment variable's derived spelling and the identity
// of the key-material derivation, and the format is what selects the
// value's shape.
type Declaration struct {
	// Key is the dotted key path, the same naming surface Load resolves,
	// for example "authn.pii_cipher_key". It must be lowercase, non-empty
	// and free of empty segments.
	Key string

	// Format names the value's shape, one of the Format constants:
	// FormatString, FormatInt, FormatBool or FormatHexKey. The set is
	// closed; anything else is refused.
	Format string
}

// Formats a Declaration may name. The set mirrors the declaring side's own
// closed set of bootstrap key formats (pkgcore.BootstrapKey.Format, pinned
// equal in the root package's tests), so a declaration and this resolver
// never disagree about what a format means.
const (
	// FormatString names a text value, resolved and returned verbatim.
	FormatString = "string"

	// FormatInt names a value shaped like an int field: converted by
	// strconv.ParseInt's rules (Go's integer literal syntax, base 0) and
	// returned as an int.
	FormatInt = "int"

	// FormatBool names a value shaped like a bool field: converted by
	// strconv.ParseBool's rules and returned as a bool.
	FormatBool = "bool"

	// FormatHexKey names one unit of key material: 64 hexadecimal
	// characters spelling 32 bytes. It resolves through the key-material
	// three-stage order (an explicit value, the root-key derivation, the
	// declared defaults table) and is returned as []byte.
	FormatHexKey = "hexkey"
)

// declaredDefaultsSource names the lowest-priority source of a
// declaration-driven schema in error text: the stand-in for the target
// struct's own values, which a declaration has none of.
const declaredDefaultsSource = "the declared defaults table"

// declarationTypes maps each format to the Go type whose conversion rules
// the resolved value takes: the type a reflection-path field of that shape
// would carry. A hexkey declaration takes []byte, the type of the
// derive-tagged field the reflection path reserves for key material.
var declarationTypes = map[string]reflect.Type{
	FormatString: reflect.TypeOf(""),
	FormatInt:    reflect.TypeOf(0),
	FormatBool:   reflect.TypeOf(false),
	FormatHexKey: reflect.TypeOf([]byte(nil)),
}

// WithDevDefaults installs defaults as the declared defaults table: the
// lowest-priority source a declared key falls back to when neither an
// explicit source (flag, environment, config file) nor the root-key
// derivation supplies it. Keys are declared key paths
// ("authn.pii_cipher_key"), values the []byte material of a hexkey key.
//
// The table serves hexkey declarations alone -- it stands exactly where a
// struct default stands for a derive-tagged field -- and an entry naming a
// key declared with another format is a wiring error, refused by
// ResolveDeclarations. An entry naming a key no declaration carries is
// inert: one table may span a whole platform's key surface while a given
// binary carries only some of the declaring modules, so an unclaimed entry
// names nothing this assembly resolves rather than a contradiction.
//
// The option has no effect on Load: a struct-backed schema's
// lowest-priority source is the values its target already holds, never
// this table.
//
// The loader ships no defaults of its own; the table is the assembling
// party's documented development convenience (a host's recognizable
// non-secret dev keys), and a deployment that takes its key material from
// a secret store or a root key simply does not pass one -- a declared key
// with no source and no table entry resolves to nothing, and the consumer
// that needs it reports the missing material itself.
func WithDevDefaults(defaults map[string][]byte) Option {
	cloned := make(map[string][]byte, len(defaults))
	for key, value := range defaults {
		cloned[key] = slices.Clone(value)
	}
	return func(l *Loader) { l.devDefaults = cloned }
}

// ResolveDeclarations resolves a list of declared bootstrap keys against
// the loader's five-source chain and returns the result addressed by
// declared key path: the text of a string declaration, the parsed int or
// bool of an int or bool one, the 32 bytes of a hexkey one. A declared key
// no source supplied is absent from the map -- there is no struct holding
// a default to stand in, and a consumer that needs the key reports its own
// missing material.
//
// It is the entry for keys whose declaring module is their only owner. A
// key a host spells out on its own loader target struct keeps the Load
// path, where the struct's own values are what stand when no source
// supplies the key; a declaration here instead falls back to the loader's
// declared defaults table (WithDevDefaults), if one is installed.
//
// The chain, the empty-value rule, the text conversion rules and the
// key-material three-stage order are Load's own: the same functions run
// here, so a value resolves identically whether it arrives through a
// declaration or through a struct field. Failures wrap the same sentinels
// -- ErrInvalidValue, ErrInvalidRootKey, ErrSourceUnreadable -- and name
// the key and the sources consulted for it; the one difference is the last
// name in that list, which reads "the declared defaults table" where
// Load's reads "the default set on the target struct".
//
// The declaration set is refused before any source is read when it cannot
// be resolved as one schema: an empty key path, a key path with an empty
// segment or an uppercase letter (the loader's key universe is lowercase),
// a repeated key, an unknown Format, or a declared-defaults entry naming a
// declared key of another format.
func (l *Loader) ResolveDeclarations(decls []Declaration) (map[string]any, error) {
	s, err := describeDeclarations(decls)
	if err != nil {
		return nil, err
	}
	if err = l.checkDeclaredDefaults(s); err != nil {
		return nil, err
	}
	if err = l.resolveEnvNames(s); err != nil {
		return nil, err
	}

	values, origins, rootKey, err := l.resolveSources(s)
	if err != nil {
		return nil, err
	}

	materials, err := l.applyDeclaredKeyMaterial(s, values, origins, rootKey)
	if err != nil {
		return nil, err
	}
	for key, material := range materials {
		values[key] = material
	}
	return values, nil
}

// describeDeclarations validates a declaration list and flattens it into
// the schema the shared pipeline consumes: one field per declaration, its
// key the declared path, its type the Go type the format names, and the
// derive flag set for a hexkey declaration -- the shape describe() builds
// for a struct whose fields carried the matching tags.
func describeDeclarations(decls []Declaration) (*schema, error) {
	s := &schema{
		byKey:         make(map[string]*field, len(decls)),
		defaultSource: declaredDefaultsSource,
	}
	for _, decl := range decls {
		if err := checkDeclaredKeyPath(decl.Key); err != nil {
			return nil, err
		}
		typ, known := declarationTypes[decl.Format]
		if !known {
			return nil, fmt.Errorf("%w: declared key %q has format %q, want one of %q, %q, %q or %q",
				ErrInvalidTarget, decl.Key, decl.Format, FormatString, FormatInt, FormatBool, FormatHexKey)
		}
		if _, exists := s.byKey[decl.Key]; exists {
			return nil, fmt.Errorf("%w: declared key %q appears more than once; one key path has one declaration",
				ErrInvalidTarget, decl.Key)
		}
		derive := decl.Format == FormatHexKey
		s.fields = append(s.fields, field{key: decl.Key, typ: typ, derive: derive})
		if derive {
			s.derives++
		}
	}
	for i := range s.fields {
		s.byKey[s.fields[i].key] = &s.fields[i]
	}
	return s, nil
}

// checkDeclaredKeyPath refuses a declared key path the loader's key
// universe cannot carry: the loader lowercases every key it reads and
// matches dotted paths literally, so an empty path, an empty segment or an
// uppercase letter would silently resolve to nothing.
func checkDeclaredKeyPath(key string) error {
	if key == "" {
		return fmt.Errorf("%w: a declared key needs a non-empty dotted path", ErrInvalidTarget)
	}
	if key != strings.ToLower(key) {
		return fmt.Errorf("%w: declared key %q must be lowercase; the loader's key universe is lowercase",
			ErrInvalidTarget, key)
	}
	for _, segment := range strings.Split(key, KeyDelimiter) {
		if segment == "" {
			return fmt.Errorf("%w: declared key %q has an empty path segment", ErrInvalidTarget, key)
		}
	}
	return nil
}

// checkDeclaredDefaults refuses every table entry that contradicts a
// declaration: an entry naming a declared key that is not hexkey. Such an
// entry would either be silently ignored or invent a conversion rule the
// reflection path does not have, and either way the table would be
// promising something the key cannot take.
func (l *Loader) checkDeclaredDefaults(s *schema) error {
	for _, key := range slices.Sorted(maps.Keys(l.devDefaults)) {
		f, declared := s.byKey[key]
		if !declared || f.derive {
			continue
		}
		return fmt.Errorf("%w: the declared defaults table carries key %q, declared with format %q; the table serves hexkey declarations alone",
			ErrInvalidTarget, key, declarationFormat(f.typ))
	}
	return nil
}

// declarationFormat names the format a declaration type came from.
func declarationFormat(t reflect.Type) string {
	switch t {
	case declarationTypes[FormatString]:
		return FormatString
	case declarationTypes[FormatInt]:
		return FormatInt
	case declarationTypes[FormatBool]:
		return FormatBool
	default:
		return FormatHexKey
	}
}

// applyDeclaredKeyMaterial resolves every hexkey declaration's material in
// the key-material three-stage order -- an explicit value, the root-key
// derivation, the declared defaults table -- removes the keys from the
// text pipeline, and returns the material by key path. The table stands in
// the stage the reflection path leaves to the target struct's own value: a
// declaration whose table entry (or derivation, or explicit value) is
// absent is left out of the result entirely, which is what makes "declared
// but unresolved" a distinguishable state at the reading end.
func (l *Loader) applyDeclaredKeyMaterial(s *schema, values map[string]any, origins map[string]source, rootKey []byte) (map[string][]byte, error) {
	materials := make(map[string][]byte)
	for i := range s.fields {
		f := &s.fields[i]
		if !f.derive {
			continue
		}

		var material []byte
		if raw, supplied := values[f.key]; supplied {
			decoded, err := l.decodeDeclaredHex(s, f.key, raw, origins)
			if err != nil {
				return nil, err
			}
			material = decoded
		} else if rootKey != nil {
			derived, err := l.deriveDeclaredKey(s, f.key, rootKey)
			if err != nil {
				return nil, err
			}
			material = derived
		} else if table, ok := l.devDefaults[f.key]; ok {
			if len(table) != rootKeySize {
				return nil, fmt.Errorf("%w: the declared defaults table carries key %q as %d bytes, and a key-material default must be exactly %d bytes; sources checked: %s",
					ErrInvalidValue, f.key, len(table), rootKeySize, l.sourcesFor(s, f.key))
			}
			// The caller gets a copy, never the table's own buffer.
			material = slices.Clone(table)
		} else {
			continue
		}

		// Whether or not a source supplied this key, the text pipeline must
		// not see it: a hexkey value is bytes the caller reads from the
		// result, never text a decoder would parse.
		delete(values, f.key)
		materials[f.key] = material
	}
	return materials, nil
}
