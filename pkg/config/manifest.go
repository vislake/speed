package config

import (
	"encoding"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// leafKind is how a leaf takes a value, which decides which origins can give
// it one and whether it is overridden as a whole.
type leafKind uint8

const (
	// kindScalar is a single assignable value.
	kindScalar leafKind = iota
	// kindStringList is a list of strings, the one container the environment
	// and the command line can give, comma separated and replaced as a whole.
	kindStringList
	// kindContainer is a map or a list of structs: the primary source gives
	// it, and gives it as a whole, because its keys are not in the manifest.
	kindContainer
)

// manifestItem is one input item: a leaf of a mounted struct together with the
// metadata its module declared for it. Nested structs only contribute path
// segments and are not items themselves.
type manifestItem struct {
	// module is the name of the module that declared the item.
	module string
	// path is the complete path in the config data: namespace, mount path
	// and field path.
	path string
	// key is the path relative to the namespace, which is how Items is keyed.
	key string
	// item is the declared metadata, taken as given.
	item Item
	// origins is item.Origins with the zero value resolved.
	origins Origin
	// envName is the variable this item reads, empty when it reads none.
	envName string
	// typ is the type of the field the value lands in.
	typ reflect.Type
	// kind classifies typ for the layers that cannot give every shape.
	kind leafKind
	// def is the field's current value in the prototype, which is this
	// item's default. Container values are copies, so the prototype is not
	// reachable through the manifest.
	def any
}

// expandSchema turns one module's declaration into its input items: it walks
// the mounted structs down to the leaves, derives the paths and the
// environment variable names, and checks what a single declaration can be
// wrong about on its own.
//
// prefix is the host's environment variable prefix. With no prefix, derived
// names do not apply; pinned ones still do, since they are not built from it.
func expandSchema(module string, s Schema, prefix string) ([]*manifestItem, error) {
	if d := pathDefect(s.Namespace); d != "" {
		return nil, fmt.Errorf("%w: module %q declares namespace %q: %s",
			ErrInvalidSchema, module, s.Namespace, d)
	}
	e := &expander{module: module, schema: s, prefix: prefix, keys: make(map[string]bool)}
	for i, m := range s.Mounts {
		if err := e.mount(i, m); err != nil {
			return nil, err
		}
	}
	if err := e.checkItemKeys(); err != nil {
		return nil, err
	}
	return e.items, nil
}

// expander carries the state of one module's expansion.
type expander struct {
	module string
	schema Schema
	prefix string
	items  []*manifestItem
	keys   map[string]bool // relative keys the expansion produced
}

func (e *expander) mount(index int, m Mount) error {
	if d := pathDefect(m.Path); d != "" {
		return fmt.Errorf("%w: module %q mounts at %q: %s", ErrInvalidSchema, e.module, m.Path, d)
	}
	v := reflect.ValueOf(m.Value)
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		v = v.Elem()
	}
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return fmt.Errorf("%w: module %q gives %T as mount %d, and a mount carries a struct "+
			"or a pointer to one: the fields are where the accepted keys and their defaults come from",
			ErrInvalidSchema, e.module, m.Value, index)
	}
	return e.walk(v, m.Path, []reflect.Type{v.Type()})
}

// walk expands one struct. rel is the path reached so far, relative to the
// namespace; chain is the struct types on the way in, which is what turns a
// self-referencing declaration into an error instead of a hang.
//
// An embedded field is inlined at rel rather than given a segment of its own,
// which is the embedding semantics of encoding/json. The path rules are
// borrowed from the json tag convention, and borrowing the embedding semantics
// along with them is what keeps a third-party struct from carrying two sets of
// expectations at once.
func (e *expander) walk(v reflect.Value, rel string, chain []reflect.Type) error {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" {
			// Unexported: no source can reach it. This holds for an embedded
			// unexported type too, whose exported fields encoding/json would
			// promote: skipping an unexported field is a rule of its own here
			// and takes precedence over the embedding semantics.
			continue
		}
		name, tagged, skip := fieldName(f)
		if skip {
			continue
		}
		fv := v.Field(i)
		inner, inline, hollow := inlineEmbedded(f, fv, tagged)
		if hollow {
			continue // an embedded nil struct pointer has no defaults to read
		}
		if inline {
			if err := e.descend(inner, rel, chain, f); err != nil {
				return err
			}
			continue
		}
		if d := segmentDefect(name); d != "" {
			return fmt.Errorf("%w: module %q derives key %q from field %s of %s: %s",
				ErrInvalidSchema, e.module, name, f.Name, typeName(t), d)
		}
		key := joinPath(rel, name)
		switch f.Type.Kind() {
		case reflect.Func, reflect.Chan, reflect.Interface:
			continue // no source can give these
		case reflect.Pointer:
			if f.Type.Elem().Kind() == reflect.Struct && !decodesFromText(f.Type) {
				if fv.IsNil() {
					continue // no defaults to read out of it
				}
				if err := e.descend(fv.Elem(), key, chain, f); err != nil {
					return err
				}
				continue
			}
		case reflect.Struct:
			if !decodesFromText(f.Type) {
				if err := e.descend(fv, key, chain, f); err != nil {
					return err
				}
				continue
			}
		}
		if err := e.leaf(key, fv, f.Type); err != nil {
			return err
		}
	}
	return nil
}

// descend expands a nested struct, refusing a type that is already on the way
// in. The check is on types rather than on a depth limit: a cycle is a defect
// for certain, while depth is not, and a limit picked out of the air would one
// day stop a legitimate declaration. The same type on two sibling branches is
// not a cycle.
func (e *expander) descend(v reflect.Value, key string, chain []reflect.Type, f reflect.StructField) error {
	st := v.Type()
	if slices.Contains(chain, st) {
		return fmt.Errorf("%w: module %q reaches %s again through field %s at %s, so expanding "+
			"the declaration does not terminate. A configuration tree has no back edges: "+
			"drop the field, or mirror the part of it that is meant to be configurable",
			ErrInvalidSchema, e.module, typeName(st), f.Name, pathLabel(key))
	}
	return e.walk(v, key, append(chain, st))
}

// leaf records one input item and checks the metadata the module gave it.
func (e *expander) leaf(key string, fv reflect.Value, ft reflect.Type) error {
	e.keys[key] = true
	item := e.schema.Items[key]
	kind := classify(ft)
	origins := resolveOrigins(item.Origins, kind)
	// The declared set, not the resolved one, decides this: an item that
	// declared nothing resolves to a set it never asked for, and killing it
	// here would take down every undeclared map field.
	if kind == kindContainer && item.Origins&(OriginEnv|OriginFlag) != 0 {
		return fmt.Errorf("%w: module %q gives %q the environment or the command line as an "+
			"origin, and it is %s. A map or a list of structs has no flat form for those layers "+
			"to give, so the declaration could never produce a value; such an item takes the "+
			"primary config source alone",
			ErrInvalidSchema, e.module, key, typeName(ft))
	}
	if utf8.RuneCountInString(item.FlagShort) > 1 {
		return fmt.Errorf("%w: module %q gives %q the command-line short name %q, and a short "+
			"name is a single character. Short names do not cluster, so the parser reads -%s as "+
			"one name rather than as several, and no declaration would ever match it",
			ErrInvalidSchema, e.module, key, item.FlagShort, item.FlagShort)
	}
	if origins.has(OriginFlag) && item.FlagName == "" {
		return fmt.Errorf("%w: module %q gives %q the command line as an origin but no FlagName. "+
			"Long names are never derived from the path, because the command line is a human "+
			"interface and the name should be chosen deliberately",
			ErrInvalidSchema, e.module, key)
	}
	if item.Sensitive && item.Description == "" {
		return fmt.Errorf("%w: module %q marks %q sensitive but gives no Description. The help "+
			"output does not echo a sensitive default, so without a description the reader has "+
			"no way to know what to supply",
			ErrInvalidSchema, e.module, key)
	}
	mi := &manifestItem{
		module:  e.module,
		path:    joinPath(e.schema.Namespace, key),
		key:     key,
		item:    item,
		origins: origins,
		typ:     ft,
		kind:    kind,
		def:     defaultValue(fv),
	}
	if origins.has(OriginEnv) {
		switch {
		case item.EnvName != "":
			mi.envName = item.EnvName
		case e.prefix != "":
			mi.envName = deriveEnvName(e.prefix, mi.path)
		}
	}
	e.items = append(e.items, mi)
	return nil
}

// checkItemKeys refuses an Items key that reached no leaf. Every input item
// lives in a carrier struct and therefore has a path, so a key that matches
// none is a misspelling, and leaving it alone would drop a Required, a
// FlagName or a Sensitive without a word.
func (e *expander) checkItemKeys() error {
	for _, key := range slices.Sorted(maps.Keys(e.schema.Items)) {
		if !e.keys[key] {
			return fmt.Errorf("%w: module %q declares Items key %q, which matches no field of "+
				"its mounts. Check the spelling and the mount path: the key is relative to the "+
				"namespace and includes the mount path",
				ErrInvalidSchema, e.module, key)
		}
	}
	return nil
}

// fieldName derives the path segment of a field: the config tag first, the
// json tag next, the field name in lower kebab case otherwise. A tag of "-"
// keeps the field out, which is what it means on the structs that carry one.
//
// tagged reports that the name came from a tag rather than from the field
// name, which is what tells an embedded field that is inlined from one that
// names a segment of its own.
func fieldName(f reflect.StructField) (name string, tagged, skip bool) {
	for _, key := range [...]string{"config", "json"} {
		tag, ok := f.Tag.Lookup(key)
		if !ok {
			continue
		}
		value, _, _ := strings.Cut(tag, ",")
		if value == "-" {
			return "", false, true
		}
		if value != "" {
			return value, true, false
		}
	}
	return camelToKebab(f.Name), false, false
}

// inlineEmbedded reports how an anonymous field takes part in a walk. An
// embedded struct without an explicit tag is inlined: its fields join the
// enclosing path without a segment of their own. An explicit config or json
// tag names a segment as it does on any other field, since a name written by
// hand was written to be used.
//
// inner is the struct to walk when inline is set; hollow marks an embedded nil
// struct pointer, which is skipped for the same reason a named one is: there
// are no defaults to read out of it. An embedded non-struct such as a defined
// integer type is neither, and keeps contributing a segment named after its
// type, which is what encoding/json does with it as well.
func inlineEmbedded(f reflect.StructField, fv reflect.Value, tagged bool) (inner reflect.Value, inline, hollow bool) {
	if !f.Anonymous || tagged || decodesFromText(f.Type) {
		return reflect.Value{}, false, false
	}
	switch {
	case f.Type.Kind() == reflect.Struct:
		return fv, true, false
	case f.Type.Kind() == reflect.Pointer && f.Type.Elem().Kind() == reflect.Struct:
		if fv.IsNil() {
			return reflect.Value{}, false, true
		}
		return fv.Elem(), true, false
	}
	return reflect.Value{}, false, false
}

// pathLabel renders a path for an error message, naming the root when the path
// is empty, which is where the fields of an inlined embedded struct sit.
func pathLabel(key string) string {
	if key == "" {
		return "the mount root"
	}
	return fmt.Sprintf("%q", key)
}

// resolveOrigins settles the origins of one leaf. The zero value stands for
// the ordinary combination, except on a container leaf: a map or a list of
// structs takes the primary config source alone, so resolving its zero value
// to include the environment would have it read a variable whose flat text it
// could never convert.
func resolveOrigins(declared Origin, kind leafKind) Origin {
	if declared == 0 && kind == kindContainer {
		return OriginPrimary
	}
	return declared.resolved()
}

// camelToKebab turns a Go field name into a config key: PoolSize becomes
// pool-size, HTTPPort becomes http-port.
func camelToKebab(name string) string {
	runes := []rune(name)
	var b strings.Builder
	for i, r := range runes {
		if !unicode.IsUpper(r) {
			b.WriteRune(r)
			continue
		}
		startsWord := i > 0 && !unicode.IsUpper(runes[i-1])
		endsAcronym := i > 0 && unicode.IsUpper(runes[i-1]) && i+1 < len(runes) && unicode.IsLower(runes[i+1])
		if startsWord || endsAcronym {
			b.WriteRune('-')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// deriveEnvName maps a path to an environment variable name: the prefix, an
// underscore, then the segments joined by double underscores with hyphens
// turned into single underscores, upper cased throughout.
//
//	cache.redis.max-conns  ->  MYAPP_CACHE__REDIS__MAX_CONNS
//
// Double underscores separate levels and single ones belong to key names,
// which is unambiguous only because a key segment cannot contain an
// underscore of its own.
func deriveEnvName(prefix, path string) string {
	segments := strings.Split(path, ".")
	for i, segment := range segments {
		segments[i] = strings.ReplaceAll(segment, "-", "_")
	}
	return strings.ToUpper(prefix + "_" + strings.Join(segments, "__"))
}

// pathDefect reports why a dotted path is not a legal one, or "" when it is.
// An empty path is legal: it means the namespace root.
func pathDefect(path string) string {
	if path == "" {
		return ""
	}
	for _, segment := range strings.Split(path, ".") {
		if d := segmentDefect(segment); d != "" {
			return d
		}
	}
	return ""
}

// segmentDefect reports why one path segment is not a legal config key
// segment, or "" when it is. Segments hold lowercase letters, digits and
// hyphens, which is what makes the environment variable mapping reversible:
// allow an underscore in a key and a.pool_size and a.pool-size derive the same
// variable name while being different paths.
func segmentDefect(segment string) string {
	if segment == "" {
		return "it has an empty segment, and every segment names something"
	}
	for _, r := range segment {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			continue
		}
		return fmt.Sprintf("segment %q contains %q, and a config key segment holds "+
			"lowercase letters, digits and hyphens only", segment, string(r))
	}
	return ""
}

// classify decides how a leaf takes its value.
func classify(t reflect.Type) leafKind {
	switch t.Kind() {
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.String {
			return kindStringList
		}
		return kindContainer
	case reflect.Map:
		return kindContainer
	}
	return kindScalar
}

// textUnmarshalerType is the interface a type uses to decode itself from a
// string.
var textUnmarshalerType = reflect.TypeFor[encoding.TextUnmarshaler]()

// decodesFromText reports whether a struct type takes its value from a string
// of its own. Such a type is a leaf rather than a level of the path: expanding
// time.Time would reach nothing but unexported fields and make the item
// disappear.
func decodesFromText(t reflect.Type) bool {
	if t.Implements(textUnmarshalerType) {
		return true
	}
	return t.Kind() != reflect.Pointer && reflect.PointerTo(t).Implements(textUnmarshalerType)
}

// defaultValue reads a field's current value out of the prototype. Maps and
// slices are copied, so that nothing reached through the manifest can write
// back into a prototype two registries may share.
func defaultValue(v reflect.Value) any {
	switch v.Kind() {
	case reflect.Map:
		if v.IsNil() {
			break
		}
		copied := reflect.MakeMapWithSize(v.Type(), v.Len())
		for iter := v.MapRange(); iter.Next(); {
			copied.SetMapIndex(iter.Key(), iter.Value())
		}
		return copied.Interface()
	case reflect.Slice:
		if v.IsNil() {
			break
		}
		copied := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		reflect.Copy(copied, v)
		return copied.Interface()
	}
	return v.Interface()
}

// joinPath appends a segment path to a prefix, either of which may be empty.
func joinPath(prefix, rest string) string {
	switch {
	case prefix == "":
		return rest
	case rest == "":
		return prefix
	}
	return prefix + "." + rest
}

// typeName renders a type for an error message, keeping the package path so
// the reader can find the declaration.
func typeName(t reflect.Type) string {
	if t.PkgPath() != "" && t.Name() != "" {
		return t.PkgPath() + "." + t.Name()
	}
	return t.String()
}
