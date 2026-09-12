package pkgcore

import (
	"encoding"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// config_schema.go carries the declaration half of the component
// configuration contract: the vocabulary a component's ConfigSchema struct
// declares its fields with (the config struct tag), the optional namespace
// prefix the fields resolve under (Component.ConfigNamespace), and the
// documentation surface (Documented, FieldDoc, DescribeComponentSchema) that
// keeps a field's operator-facing description beside the field instead of
// inside the tag string. The resolution half -- turning a component's file
// block into the five-source-merged value its New receives -- is the
// ComponentConfigResolver seam (config_resolver.go); the assembly-side
// wiring, the required-value check and the one-key-path-per-source
// validation live in component_assembly.go.
// docs/internal/32-component-config-contract.md is the design authority.
//
// One vocabulary, one address. Every tag option except "-" talks about the
// field's one key path: the address the field already occupies in the
// component's configuration block -- its json tag name, or its lowercased
// Go field name when it carries no json tag -- prefixed by the component's
// namespace when it declares one, and bare otherwise. An option never gives
// a field a second spelling, so a value is addressable exactly one way
// whether it arrives from a flag, an environment variable, a config file or
// a derivation.

// ErrConfigKeyConflict reports a configuration key path two sources of one
// assembly both produce: two selected components' source-addressed
// ConfigSchema fields (an expose/derive field each), or such a field and a
// BootstrapKeys declaration, spaced by the namespaces their descriptors
// declare. The assembly refuses it at the Prepare stage, naming the key path
// and both sources, because a single address two declarations share would
// otherwise silently resolve one of them from the other's value. A field no
// source opens (no expose option) resolves from its own component's
// configuration block alone, so it holds no shareable address and cannot
// conflict.
var ErrConfigKeyConflict = errors.New("pkgcore: conflicting configuration key path")

// ErrMissingConfigValue reports a field a ConfigSchema declares with the
// required tag option for which no configuration source supplied a value.
// The assembly refuses it at the Prepare stage -- after the component's
// configuration block has been resolved, before anything is constructed --
// naming the component and the key path, so a component's New never has to
// discover its own zero value at runtime.
var ErrMissingConfigValue = errors.New("pkgcore: required configuration value is missing")

// FieldDoc is one ConfigSchema field's human-readable documentation: the
// operator-facing text a rendered reference or a --help listing shows beside
// the field's key path. The values are statements, not runnable Go values --
// the authoritative default is whatever stands at the end of the resolution
// chain, never a second default spelled here.
type FieldDoc struct {
	// Description states what the field configures and why it exists. A
	// field marked sensitive must carry one: an undocumented secret is a
	// value no operator can be told how to supply or rotate.
	Description string
	// Default states in operator terms what happens when no source supplies
	// the field, for example "documented non-secret development default".
	// Empty means no fallback is documented.
	Default string
	// Example is the suggested value a rendered reference shows for the
	// field; empty means the recommendation is to leave it unset.
	Example string
}

// Documented is the optional interface a ConfigSchema's target type
// implements to document its fields. ConfigDocs keys each entry by the
// field's local key path -- the dotted path without the component's
// namespace prefix, matched case-insensitively, the same spelling
// FieldDescriptor.Key carries minus the prefix -- and the assembly requires
// an entry with a non-empty Description for every field the schema marks
// sensitive (ErrInvalidComponent, naming the field, otherwise).
//
// The interface is deliberately separate from the schema's tag vocabulary:
// prose belongs in Go where it is readable, compilable and greppable, never
// escaped into a struct tag.
type Documented interface {
	ConfigDocs() map[string]FieldDoc
}

// FieldDescriptor is one field of a component's ConfigSchema as
// documentation reads it: the field's final key path (namespace prefix
// included), how the field resolves, and its FieldDoc. The command-line
// --help rendering and the generated configuration reference both collect
// fields through DescribeComponentSchema, so the two renderings cannot
// drift from the declaration the assembly actually enforces.
type FieldDescriptor struct {
	// Key is the field's final dotted key path: the namespace prefix the
	// component declares, then the field's local path (its json tag name, or
	// its lowercased Go name, joined with dots through nested structs). It
	// is the same path a flag, an environment variable, a config file entry
	// or a derivation addresses the field by.
	Key string
	// Type is the field's Go type, rendered as a string (for example
	// "string", "[]byte", "time.Duration").
	Type string
	// Expose reports that the field participates in flag/environment
	// resolution, which the expose tag option declares and the derive option
	// implies. A field without it is supplied by the configuration block
	// alone.
	Expose bool
	// Derive reports that the field is one unit of key material: a []byte
	// field whose value comes from an explicit source, the root-key
	// derivation over its key path, or its declared default.
	Derive bool
	// Required reports that the assembly fails when no source supplies the
	// field.
	Required bool
	// Sensitive reports that the field's value must not be printed by
	// help/reference output.
	Sensitive bool
	// Env is the exact environment variable name the env tag option pins the
	// field to. Empty means the name is derived from the key path by the
	// loader's own spelling rule.
	Env string
	// Group is the documentation grouping the group tag option declares.
	Group string
	// Doc is the field's ConfigDocs entry, zero-valued when the schema
	// documents none.
	Doc FieldDoc
}

// DescribeComponentSchema expands a component's ConfigSchema into one
// FieldDescriptor per resolvable field, in declaration order, with the
// field's final key path and its ConfigDocs entry layered on. It is the one
// collection the command-line --help rendering and the configuration
// reference generator share.
//
// componentName names the component whose schema is described: the
// namespace prefix is the ConfigNamespace the component's own registration
// declares (its effect on the rendered key paths is exactly its effect on
// resolution), and a name no registered component carries declares none --
// its fields describe at their bare local paths, the way an empty
// ConfigNamespace resolves them. A schema that is nil or declares no fields
// describes as an empty list.
//
// The returned descriptors are a read-only projection: they describe the
// declaration, they never validate it into an assembly (the Prepare stage
// does that, with the same analyzer).
func DescribeComponentSchema(componentName string, schema any) ([]FieldDescriptor, error) {
	namespace := ""
	if registered, ok := globalComponent(componentName); ok {
		namespace = registered.ConfigNamespace
	}
	prefix, err := configKeyPrefix(componentName, namespace)
	if err != nil {
		return nil, err
	}
	fields, err := analyzeConfigSchema(schema)
	if err != nil {
		return nil, fmt.Errorf("pkgcore: component %q: %w", componentName, err)
	}

	docs := configDocs(schema)
	descriptors := make([]FieldDescriptor, 0, len(fields))
	for _, f := range fields {
		descriptor := FieldDescriptor{
			Key:       prefix + f.key,
			Type:      renderSchemaType(f.typ),
			Expose:    f.expose,
			Derive:    f.derive,
			Required:  f.required,
			Sensitive: f.sensitive,
			Env:       f.env,
			Group:     f.group,
		}
		if doc, ok := configDocFor(docs, f.key); ok {
			descriptor.Doc = doc
		}
		descriptors = append(descriptors, descriptor)
	}
	return descriptors, nil
}

// renderSchemaType renders a schema field's Go type for documentation: the
// operator-facing "[]byte" spelling for key material rather than reflect's
// "[]uint8" rendering.
func renderSchemaType(t reflect.Type) string {
	if t == keyMaterialType {
		return "[]byte"
	}
	return t.String()
}

// configKeyPrefix returns the namespace prefix a component's schema fields
// resolve under: no prefix at all when namespace is empty -- the fields
// resolve at their bare local paths -- and the namespace itself, with a
// trailing dot added when it lacks one, otherwise. A namespace carrying an
// empty segment (a leading dot, or two dots in a row) is refused: it would
// produce unusable key paths.
func configKeyPrefix(componentName, namespace string) (string, error) {
	if namespace == "" {
		return "", nil
	}
	prefix := namespace
	if !strings.HasSuffix(prefix, ".") {
		prefix += "."
	}
	if strings.HasPrefix(prefix, ".") || strings.Contains(prefix, "..") {
		return "", fmt.Errorf("%w: component %q declares ConfigNamespace %q, whose normalized form %q carries an empty segment", ErrInvalidComponent, componentName, namespace, prefix)
	}
	return prefix, nil
}

// schemaField is one resolvable leaf of a ConfigSchema: a field some source
// can supply a value for, either directly or as a nested key of the block.
type schemaField struct {
	// key is the field's local dotted key path, without the namespace
	// prefix: the json tag name or lowercased Go name, joined with dots
	// through nested structs.
	key string
	// name is the leaf's Go field name, for error text.
	name      string
	typ       reflect.Type
	expose    bool
	derive    bool
	required  bool
	sensitive bool
	env       string
	group     string
}

// analyzeConfigSchema flattens a ConfigSchema into its resolvable leaves, in
// declaration order: exported fields a source can address, descending into
// nested structs (their leaves carry their own declarations; a container
// field resolves no single value, so tag options on one are refused), and
// stopping at scalar structs a single value is supplied to (time.Time and
// anything that parses itself from text, mirroring the loader's own leaf
// decision). Nil describes nothing, and is not an error: a component that
// takes no configuration declares no schema.
func analyzeConfigSchema(schema any) ([]schemaField, error) {
	if schema == nil {
		return nil, nil
	}
	t := reflect.TypeOf(schema)
	if t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w: ConfigSchema is %s, want a pointer to a config struct such as (*mailerConfig)(nil)", ErrInvalidComponent, t)
	}
	var fields []schemaField
	if err := collectSchemaFields(t.Elem(), "", &fields, map[reflect.Type]bool{}); err != nil {
		return nil, err
	}
	return fields, nil
}

// collectSchemaFields appends one schemaField per resolvable leaf of t.
// prefix is the dotted local path already consumed, so nested fields carry
// the path their value is addressed by.
func collectSchemaFields(t reflect.Type, prefix string, out *[]schemaField, visiting map[reflect.Type]bool) error {
	if visiting[t] {
		return nil
	}
	visiting[t] = true
	defer delete(visiting, t)

	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		key, addressable := schemaFieldKey(sf)
		if !addressable {
			continue
		}
		options, err := parseSchemaTag(sf)
		if err != nil {
			return err
		}
		if options.skip {
			continue
		}

		local := joinPath(prefix, key)
		if inner, nested := schemaStructType(sf.Type); nested {
			if options.declares() {
				return fmt.Errorf("%w: field %s is a nested struct, and the struct tag options it carries (%q) belong on the fields of %s: a container field resolves no single value", ErrInvalidComponent, sf.Name, sf.Tag.Get("config"), inner)
			}
			if err := collectSchemaFields(inner, local, out, visiting); err != nil {
				return err
			}
			continue
		}

		*out = append(*out, schemaField{
			key:       local,
			name:      sf.Name,
			typ:       sf.Type,
			expose:    options.expose,
			derive:    options.derive,
			required:  options.required,
			sensitive: options.sensitive,
			env:       options.env,
			group:     options.group,
		})
	}
	return nil
}

// schemaFieldKey returns the key segment a field is addressed by: its json
// tag name when it carries one, its lowercased Go name otherwise -- exactly
// the key ComponentConfig.Decode matches, so a field's resolved key path and
// its configuration-block address are the same address. It reports false for
// a field no block key can name: a json "-" field, which Decode skips, is
// outside every source.
func schemaFieldKey(sf reflect.StructField) (string, bool) {
	if tag, ok := sf.Tag.Lookup("json"); ok {
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" {
			return "", false
		}
		if name != "" {
			return name, true
		}
	}
	return strings.ToLower(sf.Name), true
}

// schemaTagOptions is one field's parsed config struct tag.
type schemaTagOptions struct {
	skip      bool
	expose    bool
	derive    bool
	required  bool
	sensitive bool
	env       string
	group     string
}

// declares reports whether the options carry any declaration besides the
// skip option.
func (o schemaTagOptions) declares() bool {
	return o.expose || o.derive || o.required || o.sensitive || o.env != "" || o.group != ""
}

// parseSchemaTag reads a field's config struct tag. The options are a closed
// vocabulary: expose, derive, required, sensitive, env=NAME, group=NAME and
// "-" (skip). An unknown option, a malformed or repeated value-carrying
// option, a derive option on a field that is not exactly []byte, and the
// skip option beside any other option are all refused here, naming the
// field, because a schema with a meaningless declaration must fail its
// assembly rather than silently ignore half of what it says.
func parseSchemaTag(sf reflect.StructField) (schemaTagOptions, error) {
	tag, ok := sf.Tag.Lookup("config")
	if !ok {
		return schemaTagOptions{}, nil
	}

	var options schemaTagOptions
	for _, raw := range strings.Split(tag, ",") {
		opt := strings.TrimSpace(raw)
		switch {
		case opt == "":
		case opt == "-":
			options.skip = true
		case opt == "expose":
			options.expose = true
		case opt == "derive":
			if options.derive {
				return schemaTagOptions{}, fmt.Errorf("%w: field %s repeats the %q tag option; it takes no value and appears at most once", ErrInvalidComponent, sf.Name, opt)
			}
			if sf.Type != keyMaterialType {
				return schemaTagOptions{}, fmt.Errorf("%w: field %s carries the %q tag option but is typed %s, and only a []byte field can hold key material", ErrInvalidComponent, sf.Name, opt, sf.Type)
			}
			options.derive = true
			// Key material always resolves through the full chain -- an
			// explicit flag/environment value, the derivation, or the
			// declared default -- so the option implies expose.
			options.expose = true
		case opt == "required":
			options.required = true
		case opt == "sensitive":
			options.sensitive = true
		case strings.HasPrefix(opt, "env="):
			if name := strings.TrimPrefix(opt, "env="); name != "" && options.env == "" {
				options.env = name
				break
			}
			return schemaTagOptions{}, fmt.Errorf("%w: field %s has a malformed or repeated %q tag option; it needs exactly one variable name, as in env=NAME", ErrInvalidComponent, sf.Name, opt)
		case strings.HasPrefix(opt, "group="):
			if name := strings.TrimPrefix(opt, "group="); name != "" && options.group == "" {
				options.group = name
				break
			}
			return schemaTagOptions{}, fmt.Errorf("%w: field %s has a malformed or repeated %q tag option; it needs exactly one group name, as in group=NAME", ErrInvalidComponent, sf.Name, opt)
		default:
			return schemaTagOptions{}, fmt.Errorf("%w: field %s has unknown config tag option %q", ErrInvalidComponent, sf.Name, opt)
		}
	}
	if options.skip && options.declares() {
		return schemaTagOptions{}, fmt.Errorf("%w: field %s carries the %q tag option beside other options; a field resolved by no source cannot also declare how a source resolves it", ErrInvalidComponent, sf.Name, "-")
	}
	return options, nil
}

// keyMaterialType is the field type a derive-tagged declaration must carry:
// one unit of 32-byte key material.
var keyMaterialType = reflect.TypeOf([]byte(nil))

// The types the scalar-struct leaf rule reads.
var (
	timeTimeType        = reflect.TypeOf(time.Time{})
	textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
)

// schemaStructType reports whether a field is a struct the schema descends
// into, returning the struct type. Pointers are followed, and a scalar
// struct -- time.Time, or anything that parses itself from text -- is a leaf
// a single value is supplied to, not a container.
func schemaStructType(t reflect.Type) (reflect.Type, bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || scalarStructType(t) {
		return nil, false
	}
	return t, true
}

// scalarStructType reports whether a struct type is supplied from a single
// value rather than from nested keys, mirroring the loader's own rule.
func scalarStructType(t reflect.Type) bool {
	return t == timeTimeType ||
		t.Implements(textUnmarshalerType) ||
		reflect.PointerTo(t).Implements(textUnmarshalerType)
}

// configDocs returns the ConfigDocs() map of a schema, and nil when the
// schema (or its target type) implements no Documented. The schema value a
// descriptor registers is a typed nil pointer, on which a value-receiver
// method would panic, so the call routes through a fresh zero value of the
// struct type.
func configDocs(schema any) map[string]FieldDoc {
	if schema == nil {
		return nil
	}
	documented, ok := schema.(Documented)
	if !ok {
		return nil
	}
	if v := reflect.ValueOf(schema); v.Kind() == reflect.Pointer && v.IsNil() {
		documented, ok = reflect.New(v.Type().Elem()).Interface().(Documented)
		if !ok {
			return nil
		}
	}
	return documented.ConfigDocs()
}

// configDocFor finds the ConfigDocs entry for a field's local key path,
// matching case-insensitively the way Decode matches keys, so a doc written
// against the Go field name and one written against the json tag spelling
// both land.
func configDocFor(docs map[string]FieldDoc, key string) (FieldDoc, bool) {
	if doc, ok := docs[key]; ok {
		return doc, true
	}
	for documentedKey, doc := range docs {
		if strings.EqualFold(documentedKey, key) {
			return doc, true
		}
	}
	return FieldDoc{}, false
}
