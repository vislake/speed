package config

import (
	"fmt"
	"maps"
	"slices"
)

// layer is the source a leaf's current value came from. Keeping it is what
// lets a required item tell "nobody gave it a value" from "somebody gave it
// the zero value": the carrier struct always has a value, so the final value
// on its own carries no such answer.
type layer uint8

const (
	// layerDefault is the carrier struct's field value. It does not satisfy
	// a required item.
	layerDefault layer = iota
	// layerPrimary is the file or remote config centre the locator points at.
	layerPrimary
	// layerEnv is the environment.
	layerEnv
	// layerFlag is the command line.
	layerFlag
)

func (l layer) String() string {
	switch l {
	case layerDefault:
		return "the declared default"
	case layerPrimary:
		return "the primary config source"
	case layerEnv:
		return "the environment"
	case layerFlag:
		return "the command line"
	}
	return "an unknown layer"
}

// leafValue is one leaf's value together with the layer that put it there.
// The value already has the type of the field it lands in: conversion happens
// as a layer is applied, so a value that cannot convert fails the startup
// where the source that gave it is still known.
type leafValue struct {
	value any
	layer layer
}

// data is the config data of a run: every declared leaf with the value the
// topmost layer that gave one produced. It is not the content of the primary
// config source, which is only one of the layers and may be absent entirely.
type data struct {
	values map[string]leafValue
}

// newData lays the base: every declared leaf takes the current value of its
// field in the carrier struct. Decode reads from here and never goes back to
// the prototype, which is how a caller passing a zero struct still gets the
// declared defaults.
func newData(m *manifest) *data {
	d := &data{values: make(map[string]leafValue, len(m.items))}
	for _, item := range m.items {
		d.values[item.path] = leafValue{value: item.def, layer: layerDefault}
	}
	return d
}

// set records a value a layer gave.
func (d *data) set(item *manifestItem, value any, l layer) {
	d.values[item.path] = leafValue{value: value, layer: l}
}

// given reports whether any layer above the defaults gave the path a value,
// which is exactly the question a required item asks.
func (d *data) given(path string) bool {
	held, ok := d.values[path]
	return ok && held.layer != layerDefault
}

// applyPrimary overlays the content of the primary config source. Declared
// leaves are overridden one by one rather than whole subtrees: the key set of
// a section comes from the declarations, each leaf is an input item of its
// own, and a source giving cache.addr must not knock cache.ttl back to its
// zero value.
func (d *data) applyPrimary(m *manifest, content map[string]any) error {
	return d.applyPrimarySection(m, "", content)
}

func (d *data) applyPrimarySection(m *manifest, prefix string, section map[string]any) error {
	for _, key := range slices.Sorted(maps.Keys(section)) {
		path := joinPath(prefix, key)
		raw := section[key]

		if item, declared := m.byPath[path]; declared {
			if !item.origins.has(OriginPrimary) {
				return fmt.Errorf("%w: the primary config source gives %q, which module %q "+
					"declares but does not accept from the primary source: it reads %s. Letting "+
					"it through would swallow a value written on purpose",
					ErrUnknownKey, path, item.module, originNames(item.origins))
			}
			value, err := coerce(path, raw, item.typ)
			if err != nil {
				return err
			}
			d.set(item, value, layerPrimary)
			continue
		}

		if _, interior := m.interior[path]; interior {
			child, isSection := raw.(map[string]any)
			if !isSection {
				return fmt.Errorf("%w: the primary config source gives a value at %q, which is a "+
					"section holding input items rather than an input item of its own",
					ErrUnknownKey, path)
			}
			if err := d.applyPrimarySection(m, path, child); err != nil {
				return err
			}
			continue
		}

		return fmt.Errorf("%w: the primary config source gives %q, which no module declares. "+
			"Check the spelling, and check that the module owning it is imported: the manifest "+
			"is collected from the modules this binary carries",
			ErrUnknownKey, path)
	}
	return nil
}
