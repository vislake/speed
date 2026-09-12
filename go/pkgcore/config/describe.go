package config

// describe.go carries the read-only reflection projection of a configuration
// struct: the field list a component's ConfigSchema declares, in the
// vocabulary of the config struct tag (the resolution options the loader
// itself reads, plus the documentation-only options the component contract
// adds). It describes a declaration for its consumers -- a tool rendering
// --help or a generated reference, an engine mapping the declaration onto
// the sources it resolves -- and resolves nothing itself: no source is
// read, and no value is written anywhere.
//
// The projection speaks the component contract's key vocabulary, which is
// also the vocabulary a configuration block is decoded by: a field's key is
// its json tag name, or its lowercased Go name when it carries no json tag,
// joined with KeyDelimiter through nested structs. That is deliberately not
// the key spelling the loader's own reflection path (Load) derives for a
// target struct, which ignores json tags; a struct the loader is pointed at
// directly and a struct used as a component's ConfigSchema are two
// different declarations, and each is read by the rules its own contract
// states.

import (
	"fmt"
	"reflect"
	"strings"
)

// FieldSummary is one field of a configuration struct as the declaration
// vocabulary reads it: the field's local key path (the spelling a
// component's configuration block, a derived key path prefix added by the
// caller, addresses it by), how the field resolves, and the documentation
// markers. The summary is a projection of the declaration alone -- it never
// says what any source supplied.
type FieldSummary struct {
	// Key is the field's local dotted key path: its json tag name or
	// lowercased Go name, joined with dots through nested structs. The
	// namespace prefix a component's fields resolve under is the caller's
	// to add (pkgcore.Component.ConfigNamespace), because this package can
	// not know the component a schema is registered on.
	Key string
	// Name is the leaf's Go field name, for error text.
	Name string
	// Type is the field's Go type. A derive-tagged field is necessarily
	// typed []byte; TypeName renders the operator-facing spelling.
	Type reflect.Type
	// Expose reports that the field participates in flag and environment
	// resolution, which the expose option declares and the derive option
	// implies. A field without it is supplied by the configuration block
	// alone.
	Expose bool
	// Derive reports that the field is one unit of []byte key material:
	// supplied by an explicit source, the root-key derivation over its key
	// path, or a declared default, and never left zero by a source that
	// spells nothing.
	Derive bool
	// Required reports that a required value must reach the field.
	Required bool
	// Sensitive reports that the field's value must not be printed by
	// help or reference output.
	Sensitive bool
	// Skip reports that the field carries the skip option ("-"): no source
	// resolves it -- neither a flag, nor an environment variable, nor the
	// configuration block -- and the component supplies it outside
	// construction. A resolver must keep such a field out of every source,
	// the configuration block included; a renderer must not list it as
	// configurable. The skip option stands alone, so a skipped field
	// carries no other option.
	Skip bool
	// Env is the exact environment variable name the env option pins the
	// field to. Empty means the name is derived from the key path by the
	// loader's own spelling rule.
	Env string
	// Group is the documentation grouping the group option declares.
	Group string
}

// TypeName renders the field's type for documentation: the operator-facing
// "[]byte" spelling for key material rather than reflect's "[]uint8"
// rendering, and the type's own string otherwise.
func (f FieldSummary) TypeName() string {
	if f.Type == nil {
		return ""
	}
	if f.Type == byteSliceType {
		return "[]byte"
	}
	return f.Type.String()
}

// Describe returns one FieldSummary per resolvable field of target, in
// declaration order: exported fields a source can address, descending into
// nested structs and stopping at a scalar struct a single value is supplied
// to (time.Time and anything that parses itself from text, the loader's own
// leaf rule). A field that no block key can name (a json "-" field, which
// Decode skips) is not described at all; a field carrying the skip option
// is described with Skip set, so a caller can keep its key out of every
// source. A target that is nil describes as an empty list; anything but a
// nil pointer to a struct fails with ErrInvalidTarget, and a malformed or
// unknown tag option fails the description naming the field.
//
// The returned summaries are a read-only projection: they describe the
// declaration, they never validate it into an assembly, and they never
// consult a source.
func Describe(target any) ([]FieldSummary, error) {
	if target == nil {
		return nil, nil
	}
	t := reflect.TypeOf(target)
	if t.Kind() != reflect.Pointer || t.Elem().Kind() != reflect.Struct {
		return nil, fmt.Errorf("%w: Describe needs a pointer to a configuration struct such as (*mailerConfig)(nil), got %s", ErrInvalidTarget, t)
	}
	var fields []FieldSummary
	if err := describeFields(t.Elem(), "", &fields, map[reflect.Type]bool{}); err != nil {
		return nil, err
	}
	return fields, nil
}

// describeFields appends one summary per resolvable leaf of t. prefix is the
// dotted local path already consumed, so a nested field's Key is the path a
// source addresses it by. The visiting set breaks recursive types.
func describeFields(t reflect.Type, prefix string, out *[]FieldSummary, visiting map[reflect.Type]bool) error {
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
		key, addressable := describeFieldKey(sf)
		if !addressable {
			continue
		}
		options, err := parseFieldTag(sf)
		if err != nil {
			return err
		}

		local := joinFieldPath(prefix, key)
		if options.skip {
			// The field resolves from no source, so its subtree belongs to
			// no source either: a container carrying the skip option is
			// described as one skipped field rather than descended into.
			*out = append(*out, FieldSummary{Key: local, Name: sf.Name, Type: sf.Type, Skip: true})
			continue
		}
		if inner, nested := structType(sf.Type); nested {
			if options.declares() {
				return fmt.Errorf("%w: field %s is a nested struct, and the struct tag options it carries (%q) belong on the fields of %s: a container field resolves no single value",
					ErrInvalidTarget, sf.Name, sf.Tag.Get(TagName), inner)
			}
			if err := describeFields(inner, local, out, visiting); err != nil {
				return err
			}
			continue
		}

		*out = append(*out, FieldSummary{
			Key:       local,
			Name:      sf.Name,
			Type:      sf.Type,
			Expose:    options.expose,
			Derive:    options.derive,
			Required:  options.required,
			Sensitive: options.sensitive,
			Env:       options.env,
			Group:     options.group,
		})
	}
	return nil
}

// joinFieldPath appends one key segment to the dotted local path already
// consumed, so a nested field's Key is its full local path.
func joinFieldPath(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + KeyDelimiter + key
}

// describeFieldKey returns the key segment a field is addressed by: its json
// tag name when it carries one, its lowercased Go name otherwise -- the key
// spelling ComponentConfig.Decode matches, so a summary's Key is the address
// a configuration block carries the value at. It reports false for a field
// no block key can name: a json "-" field, which Decode skips, is outside
// every source.
func describeFieldKey(sf reflect.StructField) (string, bool) {
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

// fieldTagOptions is one field's parsed config struct tag, in the
// declaration vocabulary this projection reports.
type fieldTagOptions struct {
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
func (o fieldTagOptions) declares() bool {
	return o.expose || o.derive || o.required || o.sensitive || o.env != "" || o.group != ""
}

// parseFieldTag reads a field's config struct tag in the declaration
// vocabulary: expose, derive, required, sensitive, env=NAME, group=NAME and
// "-" (skip). The options are a closed set -- an unknown option, a malformed
// or repeated value-carrying option, a derive option on a field that is not
// exactly []byte, and the skip option beside any other option are all
// refused here, naming the field, because a declaration with a meaningless
// option describes something no source could ever resolve.
//
// This is the companion of the loader's own parseTag, not a replacement:
// parseTag reads the tag vocabulary of a loader target struct, whose fields
// carry no json tags and whose readers are the five sources themselves,
// while this reader serves the component configuration contract, whose
// fields are addressed by json tag names and whose full option set includes
// the documentation-only markers.
func parseFieldTag(sf reflect.StructField) (fieldTagOptions, error) {
	tag, ok := sf.Tag.Lookup(TagName)
	if !ok {
		return fieldTagOptions{}, nil
	}

	var options fieldTagOptions
	for _, raw := range strings.Split(tag, tagOptionSeparator) {
		opt := strings.TrimSpace(raw)
		switch {
		case opt == "":
		case opt == tagSkip:
			options.skip = true
		case opt == "expose":
			options.expose = true
		case opt == "derive":
			if options.derive {
				return fieldTagOptions{}, fmt.Errorf("%w: field %s repeats the %q tag option; it takes no value and appears at most once", ErrInvalidTarget, sf.Name, opt)
			}
			if sf.Type != byteSliceType {
				return fieldTagOptions{}, fmt.Errorf("%w: field %s carries the %q tag option but is typed %s, and only a []byte field can hold key material", ErrInvalidTarget, sf.Name, opt, sf.Type)
			}
			options.derive = true
			// Key material always resolves through the full chain -- an
			// explicit flag/environment/file value, the derivation, or a
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
			return fieldTagOptions{}, fmt.Errorf("%w: field %s has a malformed or repeated %q tag option; it needs exactly one variable name, as in env=NAME", ErrInvalidTarget, sf.Name, opt)
		case strings.HasPrefix(opt, "group="):
			if name := strings.TrimPrefix(opt, "group="); name != "" && options.group == "" {
				options.group = name
				break
			}
			return fieldTagOptions{}, fmt.Errorf("%w: field %s has a malformed or repeated %q tag option; it needs exactly one group name, as in group=NAME", ErrInvalidTarget, sf.Name, opt)
		default:
			return fieldTagOptions{}, fmt.Errorf("%w: field %s has unknown config tag option %q", ErrInvalidTarget, sf.Name, opt)
		}
	}
	if options.skip && options.declares() {
		return fieldTagOptions{}, fmt.Errorf("%w: field %s carries the %q tag option beside other options; a field resolved by no source cannot also declare how a source resolves it", ErrInvalidTarget, sf.Name, tagSkip)
	}
	return options, nil
}
