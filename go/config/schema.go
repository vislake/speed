package config

import (
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
)

// itemTypeBool is the closed-set Type value every feature flag is served
// as: a flag is a bool configuration item whose schema default comes from
// the flag's Default (see buildSchema). The value is pkgcore's own
// "bool" -- duplicated here only because pkgcore's closed set is
// documented in prose, not exported as constants.
const itemTypeBool = "bool"

// schemaItem is one entry of the runtime schema snapshot: a ConfigItem as
// declared by its owning module, or a FeatureFlag folded into the same
// shape (a bool item defaulting to the flag's Default, marked isFlag). The
// snapshot is built once, at Attach, from what every module registered
// during Bootstrap -- see buildSchema and AGENTS.md's attach seam.
type schemaItem struct {
	// key is the configuration key, shared between items and flags.
	key string

	// typ is the closed-set Type the value is stored and served as.
	typ string

	// sensitive marks secrets: encrypted at rest, redacted on the bus and
	// never served publicly.
	sensitive bool

	// public marks values the unauthenticated public endpoint serves.
	public bool

	// description and group mirror the declaration; the admin console and
	// the generated configuration reference are downstream consumers.
	description string
	group       string

	// minCanonical and maxCanonical are the declared Min/Max bounds in
	// canonical form (nil when absent). Only int and duration items can
	// declare them (pkgcore's declaration validation guarantees it).
	minCanonical *string
	maxCanonical *string

	// hasDefault reports whether the declaration provided a Default. When
	// false, a key with no row at any reachable scope has no value to
	// serve (ErrItemUnset).
	hasDefault bool

	// defaultCanonical is the Default in canonical form. For a flag it is
	// always present and equals strconv.FormatBool(flag.Default).
	defaultCanonical string

	// isFlag marks entries folded in from the FeatureFlag registrar.
	isFlag bool

	// flagDeps holds a flag's DependsOn keys, unresolved here -- buildSchema
	// proved every one resolves to a registered flag and that the graph is
	// acyclic before the snapshot freezes (see buildSchema). Empty for
	// plain items.
	flagDeps []string
}

// schema is the frozen runtime schema snapshot a Service serves against:
// every key its host's modules declared, folded into one map. It is
// immutable after Attach, so reads need no locking to consult it.
type schema struct {
	// items indexes every entry by key.
	items map[string]*schemaItem
}

// lookup returns the schema entry for key. The boolean reports whether the
// key was declared at all; an undeclared key is ErrUnknownKey material at
// the Service layer.
func (s *schema) lookup(key string) (*schemaItem, bool) {
	item, ok := s.items[key]
	return item, ok
}

// buildSchema folds the registry's declared ConfigItems and FeatureFlags
// into one runtime snapshot. FeatureFlag declarations become bool schema
// items whose default is the flag's Default, sharing the key space with
// ConfigItems: a flag is, at the storage layer, exactly a bool
// configuration value -- the flag-specific semantics live in IsEnabled's
// dependency walk, not in the table.
//
// The fold fails when:
//
//   - the same key was declared both as a ConfigItem and as a FeatureFlag:
//     the two declarations would fight over one row in the configs table
//     (ErrSchemaConflict). A module that wants a flag with richer
//     metadata -- a Description or a range -- can serve it as a bool
//     ConfigItem instead; it then simply cannot take part in flag
//     dependency chains, which only FeatureFlag declarations express.
//
//   - a flag's DependsOn does not resolve to another declared flag: the
//     runtime "enabled" definition would reference a key with no bool
//     semantics (ErrFeatureFlagDependencyCycle is reserved for genuine
//     cycles; an unresolved dependency is folded into the same Attach
//     failure class below, wrapping ErrSchemaConflict, because by the time
//     Attach runs pkgcore.ValidateFeatureGraph has already rejected any
//     unresolved dependency at Bootstrap -- reaching Attach with one means
//     the registry was assembled outside Kernel.Bootstrap).
//
//   - the flags' dependency graph contains a cycle: pkgcore proves every
//     dependency resolves, but proving the graph is acyclic is this
//     module's job, because a cyclic "enabled" definition would never
//     stabilize (ErrFeatureFlagDependencyCycle).
//
// Registration-order validation in pkgcore guarantees each declaration is
// internally coherent (closed Type set, Default/Min/Max of the right Go
// kind, Sensitive and Public mutually exclusive), so buildSchema does not
// re-validate declarations; it only folds. Every error is decorated with
// the offending key so a host can see which module's declaration to fix.
func buildSchema(items []pkgcore.ConfigItem, flags []pkgcore.FeatureFlag) (*schema, error) {
	s := &schema{items: make(map[string]*schemaItem, len(items)+len(flags))}

	for _, declared := range items {
		if _, dup := s.items[declared.Key]; dup {
			return nil, ErrSchemaConflict.WithParam("key", declared.Key)
		}
		entry := &schemaItem{
			key:         declared.Key,
			typ:         declared.Type,
			sensitive:   declared.Sensitive,
			public:      declared.Public,
			description: declared.Description,
			group:       declared.Group,
		}
		if err := foldBounds(entry, declared); err != nil {
			return nil, err
		}
		if declared.Default != nil {
			canonical, err := canonicalizeValue(declared.Type, declared.Default)
			if err != nil {
				return nil, ErrSchemaConflict.WithParam("key", declared.Key).WithCause(err)
			}
			entry.hasDefault = true
			entry.defaultCanonical = canonical
		}
		s.items[entry.key] = entry
	}

	// Flags fold second so a flag that collides with an item key is caught
	// by the same duplicate check rather than silently winning.
	flagKeys := make(map[string]bool, len(flags))
	for _, flag := range flags {
		flagKeys[flag.Key] = true
		if _, dup := s.items[flag.Key]; dup {
			return nil, ErrSchemaConflict.WithParam("key", flag.Key)
		}
		canonical, err := canonicalizeValue(itemTypeBool, flag.Default)
		if err != nil {
			return nil, ErrSchemaConflict.WithParam("key", flag.Key).WithCause(err)
		}
		s.items[flag.Key] = &schemaItem{
			key:              flag.Key,
			typ:              itemTypeBool,
			description:      flag.Description,
			hasDefault:       true,
			defaultCanonical: canonical,
			isFlag:           true,
			flagDeps:         append([]string(nil), flag.DependsOn...),
		}
	}

	// Every flag dependency must resolve to a declared flag, and the
	// dependency graph must be acyclic. Resolution is checked against the
	// flag keys collected above (a dependency pointing at a plain item
	// would have no bool semantics to define "enabled" against).
	for _, item := range s.items {
		if !item.isFlag {
			continue
		}
		for _, dep := range item.flagDeps {
			if !flagKeys[dep] {
				return nil, ErrSchemaConflict.
					WithParam("key", item.key).
					WithParam("depends_on", dep).
					WithCause(fmt.Errorf("flag %q depends on %q, which is not a declared feature flag", item.key, dep))
			}
		}
	}
	if err := detectFlagCycles(s.items); err != nil {
		return nil, err
	}
	return s, nil
}

// foldBounds converts a declaration's Min/Max (any, holding an int, int64
// or time.Duration) into the canonical forms range checks compare against.
// pkgcore's declaration validation has already confined bounds to
// int/duration items and to the item's own Type's Go kinds, so a failure
// here means the registry was assembled behind pkgcore's back.
func foldBounds(entry *schemaItem, declared pkgcore.ConfigItem) error {
	bounds := []struct {
		name  string
		value any
		dst   **string
	}{
		{"Min", declared.Min, &entry.minCanonical},
		{"Max", declared.Max, &entry.maxCanonical},
	}
	for _, bound := range bounds {
		if bound.value == nil {
			continue
		}
		canonical, err := canonicalizeValue(declared.Type, bound.value)
		if err != nil {
			return ErrSchemaConflict.WithParam("key", declared.Key).WithParam("bound", bound.name).WithCause(err)
		}
		*bound.dst = &canonical
	}
	return nil
}

// detectFlagCycles rejects a flag dependency graph in which a flag depends
// on itself, directly or transitively. An "enabled" definition over a
// cycle would never stabilize: whether A is enabled would depend on
// whether A is enabled. pkgcore.ValidateFeatureGraph deliberately does not
// check this -- it only proves resolution -- so the acyclicity proof lives
// here, at Attach, where the runtime semantics are defined.
func detectFlagCycles(items map[string]*schemaItem) error {
	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := make(map[string]int, len(items))
	var walk func(key string, trail []string) error
	walk = func(key string, trail []string) error {
		switch state[key] {
		case done:
			return nil
		case visiting:
			cycle := append(append([]string(nil), trail...), key)
			return ErrFeatureFlagDependencyCycle.WithParam("cycle", cycle)
		}
		state[key] = visiting
		for _, dep := range items[key].flagDeps {
			if err := walk(dep, append(trail, key)); err != nil {
				return err
			}
		}
		state[key] = done
		return nil
	}
	for key := range items {
		if err := walk(key, nil); err != nil {
			return err
		}
	}
	return nil
}
