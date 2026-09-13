package config

import (
	"encoding"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// listSeparator is how the environment and the command line give a list of
// strings. It carries no escape: an element holding a comma cannot be written
// this way and has to come from the primary config source. An escape would be
// a quoting rule only this project knows, which is the very thing the flat
// form is meant to avoid.
const listSeparator = ","

var durationType = reflect.TypeFor[time.Duration]()

// coerce converts a value a source gave into the type of the field it lands
// in. Every layer goes through here, so one rule set decides what a value may
// look like whichever origin produced it.
func coerce(path string, raw any, t reflect.Type) (any, error) {
	out := reflect.New(t).Elem()
	if err := assign(out, raw); err != nil {
		return nil, fmt.Errorf("%w: %q takes %s, and %s", ErrTypeMismatch, path, typeName(t), err)
	}
	return out.Interface(), nil
}

// coerceText converts the string form the environment and the command line
// give. They carry text and nothing else, so the container rule applies here
// rather than in coerce: a map or a list of structs has no flat form, while a
// list of strings has the conventional comma-separated one.
func coerceText(path, text string, item *manifestItem) (any, error) {
	switch item.kind {
	case kindStringList:
		return coerceList(path, text, item.typ)
	case kindContainer:
		return nil, fmt.Errorf("%w: %q is %s, and a map or a list of structs takes its value "+
			"from the primary config source alone. A flat form for them would be a syntax only "+
			"this project knows; change the value where it is written",
			ErrTypeMismatch, path, typeName(item.typ))
	}
	return coerce(path, text, item.typ)
}

// coerceList splits a comma-separated list of strings and replaces the value
// as a whole rather than appending to it: the layers override each other, and
// appending would make a value depend on which lower layer it landed on.
func coerceList(path, text string, t reflect.Type) (any, error) {
	var parts []string
	if text != "" {
		parts = strings.Split(text, listSeparator)
	}
	out := reflect.MakeSlice(reflect.SliceOf(t.Elem()), len(parts), len(parts))
	for i, part := range parts {
		out.Index(i).SetString(part)
	}
	if t.Kind() != reflect.Array {
		return out.Interface(), nil
	}
	if out.Len() != t.Len() {
		return nil, fmt.Errorf("%w: %q takes %s, and the command line or the environment gave "+
			"%d element(s)", ErrTypeMismatch, path, typeName(t), out.Len())
	}
	fixed := reflect.New(t).Elem()
	reflect.Copy(fixed, out)
	return fixed.Interface(), nil
}

// assign writes raw into dst, converting as it goes. It reports what is wrong
// without a sentinel; the caller names the path and wraps.
func assign(dst reflect.Value, raw any) error {
	if raw == nil {
		return fmt.Errorf("the source gave no value")
	}
	t := dst.Type()
	rv := reflect.ValueOf(raw)

	// A value that already has the target type is taken as it is, except for
	// the two reference kinds: sharing a map or a slice would let one holder
	// of a value write into another's.
	if rv.Type() == t && t.Kind() != reflect.Map && t.Kind() != reflect.Slice {
		dst.Set(rv)
		return nil
	}
	if t.Kind() == reflect.Interface && rv.Type().AssignableTo(t) {
		dst.Set(rv)
		return nil
	}
	if t == durationType {
		return assignDuration(dst, raw)
	}
	if t.Kind() == reflect.Pointer {
		return assignPointer(dst, raw)
	}
	if dst.CanAddr() {
		if u, ok := dst.Addr().Interface().(encoding.TextUnmarshaler); ok {
			return assignText(u, t, raw)
		}
	}

	switch t.Kind() {
	case reflect.Bool:
		return assignBool(dst, raw, rv)
	case reflect.String:
		s, ok := raw.(string)
		if !ok {
			return fmt.Errorf("the source gave %s", describe(raw))
		}
		dst.SetString(s)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return assignInt(dst, raw, rv)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return assignUint(dst, raw, rv)
	case reflect.Float32, reflect.Float64:
		return assignFloat(dst, raw, rv)
	case reflect.Slice, reflect.Array:
		return assignList(dst, rv)
	case reflect.Map:
		return assignMap(dst, rv)
	case reflect.Struct:
		return assignStruct(dst, raw)
	}
	return fmt.Errorf("no source can give a value of this kind")
}

// assignDuration keeps time.Duration on the string form alone. Its underlying
// type is an int64 count of nanoseconds, so accepting a bare 300 would make
// the value five minutes to a reader and three hundred nanoseconds to the
// machine. Duration does not decode itself from text, so this rule has
// nowhere else to live.
func assignDuration(dst reflect.Value, raw any) error {
	s, ok := raw.(string)
	if !ok {
		return fmt.Errorf("the source gave %s, and a duration is written as a string "+
			"time.ParseDuration accepts, such as \"5m\": a bare number would read as five "+
			"minutes and mean three hundred nanoseconds", describe(raw))
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is not a duration: it is written as in \"5m\", \"1h30m\" or \"200ms\"", s)
	}
	dst.SetInt(int64(d))
	return nil
}

// assignPointer allocates the pointee and writes into it. A source giving a
// value for a pointer field is asking for one to exist.
func assignPointer(dst reflect.Value, raw any) error {
	held := reflect.New(dst.Type().Elem())
	if u, ok := held.Interface().(encoding.TextUnmarshaler); ok {
		if err := assignText(u, dst.Type(), raw); err != nil {
			return err
		}
		dst.Set(held)
		return nil
	}
	if err := assign(held.Elem(), raw); err != nil {
		return err
	}
	dst.Set(held)
	return nil
}

// assignText hands the value to a type that decodes itself from a string.
// time.Time and the address types travel this way, so this mechanism needs no
// rule of its own for any of them.
func assignText(u encoding.TextUnmarshaler, t reflect.Type, raw any) error {
	s, ok := raw.(string)
	if !ok {
		return fmt.Errorf("%s reads itself from a string while the source gave %s",
			typeName(t), describe(raw))
	}
	if err := u.UnmarshalText([]byte(s)); err != nil {
		return fmt.Errorf("%s rejected %q: %v", typeName(t), s, err)
	}
	return nil
}

func assignBool(dst reflect.Value, raw any, rv reflect.Value) error {
	if rv.Kind() == reflect.Bool {
		dst.SetBool(rv.Bool())
		return nil
	}
	s, ok := raw.(string)
	if !ok {
		return fmt.Errorf("the source gave %s", describe(raw))
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return fmt.Errorf("%q is not a boolean: write true or false", s)
	}
	dst.SetBool(b)
	return nil
}

func assignInt(dst reflect.Value, raw any, rv reflect.Value) error {
	switch {
	case isSigned(rv.Kind()):
		return setInt(dst, rv.Int())
	case isUnsigned(rv.Kind()):
		u := rv.Uint()
		if u > math.MaxInt64 {
			return fmt.Errorf("%d does not fit", u)
		}
		return setInt(dst, int64(u))
	case isFloat(rv.Kind()):
		n, err := exactInt(rv.Float())
		if err != nil {
			return err
		}
		return setInt(dst, n)
	case rv.Kind() == reflect.String:
		n, err := strconv.ParseInt(rv.String(), 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not a whole number", rv.String())
		}
		return setInt(dst, n)
	}
	return fmt.Errorf("the source gave %s", describe(raw))
}

func assignUint(dst reflect.Value, raw any, rv reflect.Value) error {
	switch {
	case isSigned(rv.Kind()):
		n := rv.Int()
		if n < 0 {
			return fmt.Errorf("%d is negative", n)
		}
		return setUint(dst, uint64(n))
	case isUnsigned(rv.Kind()):
		return setUint(dst, rv.Uint())
	case isFloat(rv.Kind()):
		n, err := exactInt(rv.Float())
		if err != nil {
			return err
		}
		if n < 0 {
			return fmt.Errorf("%d is negative", n)
		}
		return setUint(dst, uint64(n))
	case rv.Kind() == reflect.String:
		u, err := strconv.ParseUint(rv.String(), 10, 64)
		if err != nil {
			return fmt.Errorf("%q is not a whole number that is zero or above", rv.String())
		}
		return setUint(dst, u)
	}
	return fmt.Errorf("the source gave %s", describe(raw))
}

func assignFloat(dst reflect.Value, raw any, rv reflect.Value) error {
	switch {
	case isSigned(rv.Kind()):
		dst.SetFloat(float64(rv.Int()))
		return nil
	case isUnsigned(rv.Kind()):
		dst.SetFloat(float64(rv.Uint()))
		return nil
	case isFloat(rv.Kind()):
		f := rv.Float()
		if dst.OverflowFloat(f) {
			return fmt.Errorf("%v does not fit", f)
		}
		dst.SetFloat(f)
		return nil
	case rv.Kind() == reflect.String:
		f, err := strconv.ParseFloat(rv.String(), 64)
		if err != nil {
			return fmt.Errorf("%q is not a number", rv.String())
		}
		if dst.OverflowFloat(f) {
			return fmt.Errorf("%v does not fit", f)
		}
		dst.SetFloat(f)
		return nil
	}
	return fmt.Errorf("the source gave %s", describe(raw))
}

// exactInt turns a floating point value into a whole number, refusing a
// fractional part and a magnitude that no longer survives the trip. JSON has
// no integer type, so a 10 written in a config file arrives as a float and
// belongs in an int field; 10.5 does not, and truncating it silently would
// change what the file says.
func exactInt(f float64) (int64, error) {
	n := int64(f)
	if float64(n) != f {
		return 0, fmt.Errorf("%v is not a whole number this field can hold", f)
	}
	return n, nil
}

func setInt(dst reflect.Value, n int64) error {
	if dst.OverflowInt(n) {
		return fmt.Errorf("%d does not fit", n)
	}
	dst.SetInt(n)
	return nil
}

func setUint(dst reflect.Value, u uint64) error {
	if dst.OverflowUint(u) {
		return fmt.Errorf("%d does not fit", u)
	}
	dst.SetUint(u)
	return nil
}

// assignList builds a fresh list element by element. Lists arrive from the
// primary config source as a list of values of their own, so each element goes
// through the same conversion as any other value.
func assignList(dst reflect.Value, rv reflect.Value) error {
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return fmt.Errorf("the source gave %s", describe(rv.Interface()))
	}
	t := dst.Type()
	if t.Kind() == reflect.Array {
		if rv.Len() != t.Len() {
			return fmt.Errorf("the source gave %d element(s) and the field holds %d", rv.Len(), t.Len())
		}
		for i := range rv.Len() {
			if err := assign(dst.Index(i), rv.Index(i).Interface()); err != nil {
				return fmt.Errorf("element %d: %w", i, err)
			}
		}
		return nil
	}
	out := reflect.MakeSlice(t, rv.Len(), rv.Len())
	for i := range rv.Len() {
		if err := assign(out.Index(i), rv.Index(i).Interface()); err != nil {
			return fmt.Errorf("element %d: %w", i, err)
		}
	}
	dst.Set(out)
	return nil
}

// assignMap builds a fresh map entry by entry. A map is a leaf that the
// primary source replaces as a whole, so its keys never reach the manifest and
// are not checked against it.
func assignMap(dst reflect.Value, rv reflect.Value) error {
	if rv.Kind() != reflect.Map {
		return fmt.Errorf("the source gave %s", describe(rv.Interface()))
	}
	t := dst.Type()
	out := reflect.MakeMapWithSize(t, rv.Len())
	for iter := rv.MapRange(); iter.Next(); {
		key := reflect.New(t.Key()).Elem()
		if err := assign(key, iter.Key().Interface()); err != nil {
			return fmt.Errorf("key %v: %w", iter.Key().Interface(), err)
		}
		value := reflect.New(t.Elem()).Elem()
		if err := assign(value, iter.Value().Interface()); err != nil {
			return fmt.Errorf("key %v: %w", iter.Key().Interface(), err)
		}
		out.SetMapIndex(key, value)
	}
	dst.Set(out)
	return nil
}

// assignStruct fills a struct that arrived as a section of the primary source.
// Only a struct inside a container reaches here: a declared struct field is
// expanded into input items of its own and never converted as a whole. Its
// field names are derived by the same rule as a path segment, and a key
// matching no field is left alone, since the keys inside a container do not
// take part in the unknown-key check.
func assignStruct(dst reflect.Value, raw any) error {
	section, ok := raw.(map[string]any)
	if !ok {
		return fmt.Errorf("the source gave %s", describe(raw))
	}
	t := dst.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name, skip := fieldName(f)
		if skip {
			continue
		}
		value, given := section[name]
		if !given {
			continue
		}
		if err := assign(dst.Field(i), value); err != nil {
			return fmt.Errorf("key %q: %w", name, err)
		}
	}
	return nil
}

func isSigned(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

func isUnsigned(k reflect.Kind) bool {
	switch k {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}

func isFloat(k reflect.Kind) bool {
	return k == reflect.Float32 || k == reflect.Float64
}

// describe renders what a source gave, so the reader sees the shape rather
// than a Go type name they never wrote.
func describe(raw any) string {
	rv := reflect.ValueOf(raw)
	if !rv.IsValid() {
		return "nothing"
	}
	switch {
	case rv.Kind() == reflect.String:
		return fmt.Sprintf("the string %q", rv.String())
	case rv.Kind() == reflect.Bool:
		return fmt.Sprintf("the boolean %v", rv.Bool())
	case isSigned(rv.Kind()) || isUnsigned(rv.Kind()) || isFloat(rv.Kind()):
		return fmt.Sprintf("the number %v", raw)
	case rv.Kind() == reflect.Map:
		return "a section"
	case rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array:
		return "a list"
	}
	return fmt.Sprintf("a value of type %s", typeName(rv.Type()))
}

// originNames renders an origin set for an error message.
func originNames(o Origin) string {
	var names []string
	if o.has(OriginPrimary) {
		names = append(names, "the primary config source")
	}
	if o.has(OriginEnv) {
		names = append(names, "the environment")
	}
	if o.has(OriginFlag) {
		names = append(names, "the command line")
	}
	if len(names) == 0 {
		return "no source at all"
	}
	return strings.Join(names, ", ")
}
