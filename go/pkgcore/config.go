package pkgcore

import (
	"encoding"
	"errors"
	"fmt"
	"maps"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// config.go carries the structured configuration value the component
// assembly is driven by: one ComponentConfig type that spans two levels. The
// whole composition configuration -- the deployment key, the strict key and
// the components block -- is a ComponentConfig, and the block each component
// receives as the cfg parameter of its New callback is the components.<name>
// subtree of it, the same type again.
//
// pkgcore never reads a file or the environment to build one: a host or an
// engine-owned loader assembles the raw values (from files, environment
// variables, flags and code overrides) and hands the result in. This file is
// the value and its strict decoding, not a source of values.

// ErrUnknownConfigKey is returned when strict decoding meets a key the
// target does not declare -- a component configuration block carrying a key
// its ConfigSchema does not name, a migration or composition block carrying
// an unknown top-level key, or a nested struct entry that matches no field.
// The error names the key, the component (or the composition configuration)
// and the full list of accepted keys at that level, because the remedy for a
// misspelled key is knowing the spelled ones.
var ErrUnknownConfigKey = errors.New("pkgcore: unknown config key")

// ComponentConfig is a tree of raw configuration values addressed by key:
// strings, numbers, bools, time.Duration, nested maps, slices, or a nested
// ComponentConfig for a subtree that carries its own key order.
//
// It is a value, cheap to copy, and immutable from the outside: With derives
// a new value instead of mutating one, so a config two callers share never
// changes under either of them.
type ComponentConfig struct {
	// entries holds the key-value pairs in key order: the sorted order
	// NewComponentConfig establishes, or the call order With appends in.
	// The order is what makes assembly planning deterministic, so it is
	// preserved here rather than re-derived from the map.
	entries []configEntry
	// index maps each key to its position in entries.
	index map[string]int
}

// configEntry is one key of a ComponentConfig.
type configEntry struct {
	key   string
	value any
}

// NewComponentConfig returns a ComponentConfig holding raw's entries in
// sorted key order. A Go map carries no writing order, so the sorted order
// is the one deterministic order this constructor can promise; a caller that
// needs a specific order (a loader replaying the order keys were written in
// a file, a test pinning tie-breaking) builds the value with With instead,
// which appends in call order.
//
// Values are stored as given. A raw map nested inside raw stays a plain map
// (its keys have no order); a nested ComponentConfig keeps its own order.
func NewComponentConfig(raw map[string]any) ComponentConfig {
	c := ComponentConfig{index: make(map[string]int, len(raw))}
	for _, key := range slices.Sorted(maps.Keys(raw)) {
		c.index[key] = len(c.entries)
		c.entries = append(c.entries, configEntry{key: key, value: raw[key]})
	}
	return c
}

// With returns a copy of c with key set to value. An existing key keeps its
// position in the order and takes the new value; a new key is appended after
// the entries already present. c itself is not modified, so a value derived
// with With shares nothing mutable with its base.
func (c ComponentConfig) With(key string, value any) ComponentConfig {
	next := ComponentConfig{
		entries: slices.Clone(c.entries),
		index:   maps.Clone(c.index),
	}
	if next.index == nil {
		next.index = make(map[string]int)
	}
	if i, exists := next.index[key]; exists {
		next.entries[i] = configEntry{key: key, value: value}
		return next
	}
	next.index[key] = len(next.entries)
	next.entries = append(next.entries, configEntry{key: key, value: value})
	return next
}

// Keys returns the config's keys in order.
func (c ComponentConfig) Keys() []string {
	keys := make([]string, 0, len(c.entries))
	for _, e := range c.entries {
		keys = append(keys, e.key)
	}
	return keys
}

// Len returns the number of entries in the config.
func (c ComponentConfig) Len() int { return len(c.entries) }

// lookup returns the raw value stored under key.
func (c ComponentConfig) lookup(key string) (any, bool) {
	i, exists := c.index[key]
	if !exists {
		return nil, false
	}
	return c.entries[i].value, true
}

// Get returns the raw value stored under key, exactly as it was given -- a
// scalar, a nested map, a nested ComponentConfig, or nothing. It is the raw
// reading, where Value is the converting one: a caller that merges or
// re-shapes configuration trees works on the values themselves and must not
// have a nested subtree bent into a scalar's shape along the way.
func (c ComponentConfig) Get(key string) (any, bool) {
	return c.lookup(key)
}

// asMap converts a raw value that is meant to be a mapping -- a plain
// map[string]any or a nested ComponentConfig -- into a ComponentConfig,
// reporting whether the value is a mapping at all.
func asMap(raw any) (ComponentConfig, bool) {
	switch v := raw.(type) {
	case ComponentConfig:
		return v, true
	case map[string]any:
		return NewComponentConfig(v), true
	default:
		return ComponentConfig{}, false
	}
}

// Value returns the value stored under key, converted to T by the same rules
// Decode applies to a struct field: exact and assignable types are taken as
// they are, numbers convert between numeric kinds with range checks, text
// converts to bool, numeric or time.Duration values, and a mapping converts
// to a struct or a map. A key that is not present, and a value that does not
// fit T, are both errors.
func Value[T any](c ComponentConfig, key string) (T, error) {
	var zero T
	raw, ok := c.lookup(key)
	if !ok {
		return zero, fmt.Errorf("pkgcore: config key %q is not present; the config carries %s", key, joinNames(c.Keys()))
	}
	dst := reflect.New(reflect.TypeFor[T]()).Elem()
	if err := assignConfigValue(dst, raw, key); err != nil {
		return zero, err
	}
	value, ok := dst.Interface().(T)
	if !ok {
		return zero, fmt.Errorf("pkgcore: config key %q: the value of type %s did not convert to %s", key, reflect.TypeOf(dst.Interface()), reflect.TypeFor[T]())
	}
	return value, nil
}

// Decode populates target from the config, strictly: target must be a
// non-nil pointer to a struct, every key must name a field of it (matched by
// json tag name when the field carries one, by field name otherwise, case-
// insensitively), and every value must fit its field. A key that names no
// field fails the decode with an error wrapping ErrUnknownConfigKey that
// lists the accepted keys; keys the config does not carry leave their fields
// at their zero value.
//
// Decode is the decode a component's ConfigSchema goes through: the assembly
// calls cfg.Decode on a fresh instance of the schema before anything is
// constructed, so a misspelled key fails the Prepare stage naming the
// component and the accepted keys, never a half-configured component at
// runtime.
func (c ComponentConfig) Decode(target any) error {
	rv := reflect.ValueOf(target)
	if !rv.IsValid() || rv.Kind() != reflect.Pointer || rv.IsNil() || rv.Elem().Kind() != reflect.Struct {
		return fmt.Errorf("pkgcore: Decode requires a non-nil pointer to a struct, got %T", target)
	}
	return decodeStruct(rv.Elem(), c, "")
}

// decodeStruct populates the struct value dst from c. path is the dotted key
// path already consumed, used to spell nested errors.
func decodeStruct(dst reflect.Value, c ComponentConfig, path string) error {
	fields := describeStruct(dst.Type())

	var unknown []string
	for _, e := range c.entries {
		field, ok := fields.match(e.key)
		if !ok {
			unknown = append(unknown, joinPath(path, e.key))
			continue
		}
		if err := assignConfigValue(dst.Field(field.index), e.value, joinPath(path, e.key)); err != nil {
			return err
		}
	}
	if len(unknown) > 0 {
		return unknownKeysError(unknown, fields.accepted())
	}
	return nil
}

// fieldMatch is one decodable field of a struct: the field's index within
// its struct and the key that addresses it.
type fieldMatch struct {
	index int
	key   string
}

// structFields is the decodable field set of one struct type, in declaration
// order.
type structFields []fieldMatch

// describeStruct returns the fields of t a config key can address, in
// declaration order: exported fields without a json "-" tag, keyed by their
// json tag name when present and by the field name otherwise.
func describeStruct(t reflect.Type) structFields {
	fields := make(structFields, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		sf := t.Field(i)
		if !sf.IsExported() {
			continue
		}
		key := sf.Name
		if tag, ok := sf.Tag.Lookup("json"); ok {
			name, _, _ := strings.Cut(tag, ",")
			if name == "-" {
				continue
			}
			if name != "" {
				key = name
			}
		}
		fields = append(fields, fieldMatch{index: i, key: key})
	}
	return fields
}

// match returns the field addressed by key, matching case-insensitively.
func (f structFields) match(key string) (fieldMatch, bool) {
	for _, field := range f {
		if strings.EqualFold(field.key, key) {
			return field, true
		}
	}
	return fieldMatch{}, false
}

// accepted renders the field keys in declaration order, the list an
// unknown-key error hands the reader.
func (f structFields) accepted() []string {
	keys := make([]string, 0, len(f))
	for _, field := range f {
		keys = append(keys, field.key)
	}
	return keys
}

// unknownKeysError renders the strict-decode failure for keys that matched no
// field of the struct they were read against: every unknown key, then the
// accepted keys of that level. The caller adds the context that names the
// stage and the component (or the composition configuration) the level
// belongs to.
func unknownKeysError(unknown, accepted []string) error {
	if len(unknown) == 1 {
		return fmt.Errorf("%w: config key %s is not declared (accepted: %s)",
			ErrUnknownConfigKey, quoteAll(unknown), joinNames(accepted))
	}
	return fmt.Errorf("%w: config keys %s are not declared (accepted: %s)",
		ErrUnknownConfigKey, quoteAll(unknown), joinNames(accepted))
}

// assignConfigValue assigns raw to dst, reporting a value that cannot be
// represented in dst's type with the key path it was read at.
func assignConfigValue(dst reflect.Value, raw any, path string) error {
	if raw == nil {
		dst.SetZero()
		return nil
	}

	rawValue := reflect.ValueOf(raw)
	// The fast path: a value whose type fits the target as it stands is
	// taken verbatim, which is also what an any-typed field receives. A
	// nested ComponentConfig is a mapping like any other: it is not
	// assignable to a struct- or map-typed field in general, so it falls
	// through to the kind switch below, whose mapping branches admit it
	// through asMap exactly as they admit a plain map[string]any -- a block
	// nested as a ComponentConfig and one nested as a raw map decode
	// identically.
	if rawValue.Type().AssignableTo(dst.Type()) {
		dst.Set(rawValue)
		return nil
	}

	switch dst.Kind() {
	case reflect.Pointer:
		dst.Set(reflect.New(dst.Type().Elem()))
		return assignConfigValue(dst.Elem(), raw, path)
	case reflect.Interface:
		if rawValue.Type().Implements(dst.Type()) {
			dst.Set(rawValue)
			return nil
		}
		return fmt.Errorf("pkgcore: config key %q: value of type %s does not implement %s", path, rawValue.Type(), dst.Type())
	case reflect.Struct:
		return assignStruct(dst, raw, rawValue, path)
	case reflect.Map:
		return assignMapping(dst, raw, path)
	case reflect.Slice:
		return assignSlice(dst, rawValue, path)
	case reflect.String:
		return assignString(dst, rawValue, path)
	case reflect.Bool:
		return assignBool(dst, rawValue, path)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return assignInt(dst, rawValue, path)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return assignUint(dst, rawValue, path)
	case reflect.Float32, reflect.Float64:
		return assignFloat(dst, rawValue, path)
	default:
		return fmt.Errorf("pkgcore: config key %q: value of type %s cannot be assigned to %s", path, rawValue.Type(), dst.Type())
	}
}

// durationType is the standard duration shape a config value converts into.
var durationType = reflect.TypeOf(time.Duration(0))

// The integer ranges' exact float64 boundaries, for the float-to-integer
// conversions: a float outside a range must be refused BEFORE the conversion,
// because Go leaves an out-of-range float-to-integer conversion
// implementation-defined -- a range check run after the conversion inspects a
// value the conversion already replaced. 2^63 and 2^64 are powers of two, so
// these constants are exact and the comparisons are exact.
const (
	minInt64Float  = float64(-1 << 63) // exactly -2^63, the minimum int64
	maxInt64Float  = float64(1 << 63)  // exactly 2^63, one past the maximum int64
	maxUint64Float = float64(1 << 64)  // exactly 2^64, one past the maximum uint64
)

// assignStruct assigns raw to a struct field: a mapping decodes into the
// struct's fields strictly, a string parses through encoding.TextUnmarshaler
// when the struct implements it (time.Time does), and anything else is
// refused.
func assignStruct(dst reflect.Value, raw any, rawValue reflect.Value, path string) error {
	if config, isMapping := asMap(raw); isMapping {
		return decodeStruct(dst, config, path)
	}
	if text, isText := raw.(string); isText {
		if u, ok := dst.Addr().Interface().(encoding.TextUnmarshaler); ok {
			if err := u.UnmarshalText([]byte(text)); err != nil {
				return fmt.Errorf("pkgcore: config key %q: %w", path, err)
			}
			return nil
		}
	}
	return fmt.Errorf("pkgcore: config key %q: value of type %s cannot be assigned to %s", path, rawValue.Type(), dst.Type())
}

// assignMapping assigns a mapping to a map field, converting every value to
// the map's element type.
func assignMapping(dst reflect.Value, raw any, path string) error {
	config, isMapping := asMap(raw)
	if !isMapping {
		return fmt.Errorf("pkgcore: config key %q: value of type %s is not a mapping", path, typeName(reflect.TypeOf(raw)))
	}
	if dst.Kind() != reflect.Map {
		return fmt.Errorf("pkgcore: config key %q: a mapping cannot be assigned to %s", path, dst.Type())
	}
	if dst.Type().Key().Kind() != reflect.String {
		return fmt.Errorf("pkgcore: config key %q: %s has a non-string key type", path, dst.Type())
	}
	if dst.IsNil() {
		dst.Set(reflect.MakeMap(dst.Type()))
	}
	for _, e := range config.entries {
		elem := reflect.New(dst.Type().Elem()).Elem()
		if err := assignConfigValue(elem, e.value, joinPath(path, e.key)); err != nil {
			return err
		}
		dst.SetMapIndex(reflect.ValueOf(e.key), elem)
	}
	return nil
}

// assignSlice assigns a list to a slice field, converting every element to
// the slice's element type.
func assignSlice(dst reflect.Value, rawValue reflect.Value, path string) error {
	if rawValue.Kind() != reflect.Slice && rawValue.Kind() != reflect.Array {
		return fmt.Errorf("pkgcore: config key %q: value of type %s is not a list", path, rawValue.Type())
	}
	out := reflect.MakeSlice(dst.Type(), rawValue.Len(), rawValue.Len())
	for i := 0; i < rawValue.Len(); i++ {
		if err := assignConfigValue(out.Index(i), rawValue.Index(i).Interface(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
			return err
		}
	}
	dst.Set(out)
	return nil
}

// assignString assigns text to a string field, converting a named string
// type along the way.
func assignString(dst reflect.Value, rawValue reflect.Value, path string) error {
	if rawValue.Kind() != reflect.String {
		return fmt.Errorf("pkgcore: config key %q: value of type %s cannot be assigned to %s", path, rawValue.Type(), dst.Type())
	}
	dst.SetString(rawValue.String())
	return nil
}

// assignBool assigns a bool field, reading text through strconv.ParseBool.
func assignBool(dst reflect.Value, rawValue reflect.Value, path string) error {
	switch rawValue.Kind() {
	case reflect.Bool:
		dst.SetBool(rawValue.Bool())
		return nil
	case reflect.String:
		parsed, err := strconv.ParseBool(rawValue.String())
		if err != nil {
			return fmt.Errorf("pkgcore: config key %q: value %q is not a valid bool", path, rawValue.String())
		}
		dst.SetBool(parsed)
		return nil
	default:
		return fmt.Errorf("pkgcore: config key %q: value of type %s cannot be assigned to %s", path, rawValue.Type(), dst.Type())
	}
}

// assignInt assigns an integer field: an integer or integral float of any
// width converts with a range check, a time.Duration field also accepts Go
// duration text ("5m"), and every other integer-like field accepts Go
// integer literal text (strconv.ParseInt base 0: 0x10, 0o17, 1_0).
func assignInt(dst reflect.Value, rawValue reflect.Value, path string) error {
	switch rawValue.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value := rawValue.Int()
		if dst.OverflowInt(value) {
			return overflowError(path, rawValue.Type(), dst.Type())
		}
		dst.SetInt(value)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value := rawValue.Uint()
		if value > math.MaxInt64 || dst.OverflowInt(int64(value)) {
			return overflowError(path, rawValue.Type(), dst.Type())
		}
		dst.SetInt(int64(value))
		return nil
	case reflect.Float32, reflect.Float64:
		value := rawValue.Float()
		if value != math.Trunc(value) {
			return fmt.Errorf("pkgcore: config key %q: value %v is not a whole number", path, value)
		}
		if value < minInt64Float || value >= maxInt64Float {
			return overflowError(path, rawValue.Type(), dst.Type())
		}
		if dst.OverflowInt(int64(value)) {
			return overflowError(path, rawValue.Type(), dst.Type())
		}
		dst.SetInt(int64(value))
		return nil
	case reflect.String:
		text := rawValue.String()
		if dst.Type() == durationType {
			parsed, err := time.ParseDuration(text)
			if err != nil {
				return fmt.Errorf("pkgcore: config key %q: value %q is not a valid duration", path, text)
			}
			dst.SetInt(int64(parsed))
			return nil
		}
		parsed, err := strconv.ParseInt(text, 0, dst.Type().Bits())
		if err != nil {
			return fmt.Errorf("pkgcore: config key %q: value %q is not a valid integer", path, text)
		}
		dst.SetInt(parsed)
		return nil
	default:
		return fmt.Errorf("pkgcore: config key %q: value of type %s cannot be assigned to %s", path, rawValue.Type(), dst.Type())
	}
}

// assignUint mirrors assignInt for unsigned integer fields.
func assignUint(dst reflect.Value, rawValue reflect.Value, path string) error {
	switch rawValue.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value := rawValue.Uint()
		if dst.OverflowUint(value) {
			return overflowError(path, rawValue.Type(), dst.Type())
		}
		dst.SetUint(value)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value := rawValue.Int()
		if value < 0 || dst.OverflowUint(uint64(value)) {
			return overflowError(path, rawValue.Type(), dst.Type())
		}
		dst.SetUint(uint64(value))
		return nil
	case reflect.Float32, reflect.Float64:
		value := rawValue.Float()
		if value != math.Trunc(value) || value < 0 {
			return fmt.Errorf("pkgcore: config key %q: value %v is not a whole non-negative number", path, value)
		}
		if value >= maxUint64Float {
			return overflowError(path, rawValue.Type(), dst.Type())
		}
		if dst.OverflowUint(uint64(value)) {
			return overflowError(path, rawValue.Type(), dst.Type())
		}
		dst.SetUint(uint64(value))
		return nil
	case reflect.String:
		text := rawValue.String()
		parsed, err := strconv.ParseUint(text, 0, dst.Type().Bits())
		if err != nil {
			return fmt.Errorf("pkgcore: config key %q: value %q is not a valid unsigned integer", path, text)
		}
		dst.SetUint(parsed)
		return nil
	default:
		return fmt.Errorf("pkgcore: config key %q: value of type %s cannot be assigned to %s", path, rawValue.Type(), dst.Type())
	}
}

// assignFloat assigns a floating-point field from a float, an integer or
// text.
func assignFloat(dst reflect.Value, rawValue reflect.Value, path string) error {
	switch rawValue.Kind() {
	case reflect.Float32, reflect.Float64:
		value := rawValue.Float()
		if dst.OverflowFloat(value) {
			return overflowError(path, rawValue.Type(), dst.Type())
		}
		dst.SetFloat(value)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		dst.SetFloat(float64(rawValue.Int()))
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		dst.SetFloat(float64(rawValue.Uint()))
		return nil
	case reflect.String:
		text := rawValue.String()
		parsed, err := strconv.ParseFloat(text, dst.Type().Bits())
		if err != nil {
			return fmt.Errorf("pkgcore: config key %q: value %q is not a valid number", path, text)
		}
		dst.SetFloat(parsed)
		return nil
	default:
		return fmt.Errorf("pkgcore: config key %q: value of type %s cannot be assigned to %s", path, rawValue.Type(), dst.Type())
	}
}

// overflowError renders a value that does not fit its field's type.
func overflowError(path string, from, to reflect.Type) error {
	return fmt.Errorf("pkgcore: config key %q: value of type %s does not fit %s", path, from, to)
}

// joinPath appends key to a dotted key path.
func joinPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// quoteAll renders each name in quotes, joined for error text.
func quoteAll(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, strconv.Quote(name))
	}
	return strings.Join(quoted, ", ")
}

// compositionKeys are the keys the composition configuration declares, the
// accepted set of its strict decode.
var compositionKeys = structFields{
	{index: 0, key: "components"},
	{index: 1, key: "deployment"},
	{index: 2, key: "strict"},
}

// composition is the parsed shape of the whole composition configuration.
type composition struct {
	// mode is the deployment mode the assembly validates capabilities
	// against; DeploymentModeStandalone when the config carries no
	// deployment key.
	mode DeploymentMode
	// strict is the strict flag: true disables auto-pull, so the selected
	// set is exactly the set the configuration names.
	strict bool
	// components is the components block, with its key order preserved.
	components ComponentConfig
}

// parseComposition strictly decodes c as a composition configuration: only
// the deployment, strict and components keys are accepted, deployment must
// be a valid mode name, strict must be a bool, and components must be a
// mapping.
func parseComposition(c ComponentConfig) (composition, error) {
	out := composition{mode: DeploymentModeStandalone}

	var unknown []string
	for _, e := range c.entries {
		switch e.key {
		case "deployment":
			text, ok := e.value.(string)
			if !ok {
				return composition{}, fmt.Errorf("pkgcore: composition configuration (stage prepare): the deployment value must be a mode name string, got %s", typeName(reflect.TypeOf(e.value)))
			}
			mode, err := ParseDeploymentMode(text)
			if err != nil {
				return composition{}, fmt.Errorf("pkgcore: composition configuration (stage prepare): %w", err)
			}
			out.mode = mode
		case "strict":
			strict, ok := e.value.(bool)
			if !ok {
				return composition{}, fmt.Errorf("pkgcore: composition configuration (stage prepare): the strict value must be a bool, got %s", typeName(reflect.TypeOf(e.value)))
			}
			out.strict = strict
		case "components":
			config, isMapping := asMap(e.value)
			if !isMapping {
				return composition{}, fmt.Errorf("pkgcore: composition configuration (stage prepare): the components value must be a mapping of component names to selection values, got %s", typeName(reflect.TypeOf(e.value)))
			}
			out.components = config
		default:
			unknown = append(unknown, e.key)
		}
	}
	if len(unknown) > 0 {
		return composition{}, fmt.Errorf("pkgcore: composition configuration (stage prepare): %w",
			unknownKeysError(unknown, compositionKeys.accepted()))
	}
	return out, nil
}
