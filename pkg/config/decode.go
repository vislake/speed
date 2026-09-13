package config

import (
	"fmt"
	"reflect"
	"slices"
)

// reader hands each module the values it declared. It holds the manifest and
// the config data of one run and nothing else: configuration is a snapshot
// taken at startup, so there is nothing to refresh and nothing to guard.
type reader struct {
	manifest *manifest
	data     *data
}

var _ Reader = (*reader)(nil)

// Decode writes the values under path into target.
//
// The walk is over the target rather than over the manifest, which is what
// lets two modules share a namespace: each one decodes the keys its own struct
// names and never sees the other's. Defaults arrive through the config data's
// base layer, so a caller passing a zero struct still gets them, and the
// prototype the module declared is never consulted again.
//
// A path the caller never declared is not an error: target keeps what the
// caller put in it. A required item that no source gave a value for is
// ErrMissingRequired, raised here rather than in a global sweep, because the
// manifest holds the items of modules this run will not enable, and failing
// on theirs would break the very stance that lets a default implementation
// stand down.
func (r *reader) Decode(path string, target any) error {
	v := reflect.ValueOf(target)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return fmt.Errorf("%w: Decode was given %T for %s, and it writes into a non-nil pointer "+
			"to a struct. Pass the address of the carrier struct declared for this path",
			ErrInvalidSchema, target, pathLabel(path))
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return fmt.Errorf("%w: Decode was given %T for %s, which points at %s, and it writes "+
			"into a struct. Pass the address of the carrier struct declared for this path",
			ErrInvalidSchema, target, pathLabel(path), v.Kind())
	}
	return r.decodeStruct(v, path, []reflect.Type{v.Type()})
}

// decodeStruct walks one struct down to its leaves, deriving the same paths
// the declaration did: what the manifest expanded and what Decode looks up
// have to be the same names, or a module would declare one key and read
// another. Embedded fields are therefore inlined here exactly as the expansion
// inlines them; the two walks are separate implementations of one rule, and a
// change made to only one of them declares one key and reads another without
// saying so.
func (r *reader) decodeStruct(v reflect.Value, prefix string, chain []reflect.Type) error {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue
		}
		name, tagged, skip := fieldName(f)
		if skip {
			continue
		}
		fv := v.Field(i)
		inner, inline, hollow := inlineEmbedded(f, fv, tagged)
		if hollow {
			continue
		}
		if inline {
			if err := r.descend(inner, prefix, chain, f); err != nil {
				return err
			}
			continue
		}
		key := joinPath(prefix, name)
		switch f.Type.Kind() {
		case reflect.Func, reflect.Chan, reflect.Interface:
			continue
		case reflect.Pointer:
			if f.Type.Elem().Kind() == reflect.Struct && !decodesFromText(f.Type) {
				if fv.IsNil() {
					continue
				}
				if err := r.descend(fv.Elem(), key, chain, f); err != nil {
					return err
				}
				continue
			}
		case reflect.Struct:
			if !decodesFromText(f.Type) {
				if err := r.descend(fv, key, chain, f); err != nil {
					return err
				}
				continue
			}
		}
		if err := r.leaf(key, fv); err != nil {
			return err
		}
	}
	return nil
}

// descend walks into a nested struct, refusing a type already on the way in.
// A declared mount cannot hold a cycle, collection rejects those; a target the
// caller assembled for itself can, and stopping is better than not returning.
// It is the same defect collection names, so it carries the same sentinel:
// the sentinel table is closed and every startup failure is in it.
func (r *reader) descend(v reflect.Value, key string, chain []reflect.Type, f reflect.StructField) error {
	st := v.Type()
	if slices.Contains(chain, st) {
		return fmt.Errorf("%w: decoding %s reaches %s again through field %s, so walking the "+
			"target does not terminate. A configuration carrier has no back edges: drop the "+
			"field, or mirror the part of it that is meant to be configurable",
			ErrInvalidSchema, pathLabel(key), typeName(st), f.Name)
	}
	return r.decodeStruct(v, key, append(chain, st))
}

// leaf writes one value into the target field and enforces the required rule
// on it.
func (r *reader) leaf(path string, fv reflect.Value) error {
	if item, declared := r.manifest.byPath[path]; declared && item.item.Required && !r.data.given(path) {
		return fmt.Errorf("%w: %q is required and neither the primary config source, the "+
			"environment nor the command line gave it a value. The default in the carrier struct "+
			"does not count: a value nobody gave and a value that happens to be the zero one "+
			"would otherwise be the same thing",
			ErrMissingRequired, path)
	}
	held, present := r.data.values[path]
	if !present || !fv.CanSet() {
		return nil
	}
	if err := assign(fv, held.value); err != nil {
		return fmt.Errorf("%w: %q takes %s, and %w", ErrTypeMismatch, path, typeName(fv.Type()), err)
	}
	return nil
}
