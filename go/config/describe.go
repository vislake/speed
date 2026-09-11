package config

import "sort"

// ConfigItemDescriptor is a read-only, exported view of one frozen schema
// entry (schemaItem, unexported) -- a ConfigItem as declared by its owning
// module, or a FeatureFlag folded into the same shape. It exists so an
// external tool (a Markdown configuration-reference generator, an admin
// console) can render the full configuration item listing -- which is
// generated from the config schema, never hand-written -- without needing
// to import schema.go's own unexported machinery, which no external
// package ever could.
//
// Every field mirrors schemaItem's own doc comments exactly; see that
// type for the full field-by-field rationale. Min/Max/Default are left in
// their stored canonical-string form (schemaItem's own encoding) rather
// than decoded back into a typed Go value: the decoding rule depends on
// Type, which a generic renderer has no business special-casing, and the
// canonical string is exactly what a human-readable reference needs to
// display anyway. The one deliberate divergence from the schema entry is
// Default: Describe applies redactIf to a Sensitive item's Default at
// assembly time (see that field's own comment), so the exported view
// carries the redactedMarker -- never the plaintext -- exactly as the
// event bus does.
type ConfigItemDescriptor struct {
	// Key is the configuration key, shared between items and flags.
	Key string

	// Type is the closed-set Type the value is stored and served as.
	Type string

	// Sensitive marks secrets: encrypted at rest, redacted on the bus and
	// never served publicly.
	Sensitive bool

	// Public marks values the unauthenticated public endpoint serves.
	Public bool

	// Description and Group mirror the declaration.
	Description string
	Group       string

	// Min and Max are the declared bounds in canonical form, nil when the
	// declaration carried none. Only int and duration items can declare
	// them.
	Min *string
	Max *string

	// HasDefault reports whether the declaration provided a Default. When
	// false, Default is the empty string and carries no meaning -- a key
	// with no row at any reachable scope has no value to serve at all
	// (ErrItemUnset).
	HasDefault bool

	// Default is the Default in canonical form, meaningful only when
	// HasDefault is true. For a feature flag this is always present. For a
	// Sensitive item the field carries the redactedMarker instead of the
	// declared plaintext (see redactIf): this descriptor is the exported
	// view boundary -- the admin console and the configuration-reference
	// generator both consume it -- and a secret's default has no more
	// business crossing it than crossing the event bus.
	Default string

	// IsFeatureFlag marks entries folded in from the FeatureFlag
	// registrar rather than declared as a plain ConfigItem.
	IsFeatureFlag bool

	// FlagDependsOn holds a feature flag's DependsOn keys (empty for a
	// plain ConfigItem) -- the dependency graph pkgcore.ValidateFeatureGraph
	// already proved acyclic and fully resolved when the assembly's Init
	// stage closed.
	FlagDependsOn []string
}

// Describe returns a read-only snapshot of every configuration item and
// feature flag the frozen schema carries, sorted by Key so a generated
// reference's diff is stable across regenerations run against an
// unchanged schema.
//
// A pure read accessor: it takes no lock beyond what the schema's own
// immutability-after-Attach guarantee already provides (the schema is
// built once, by Attach, and never mutated again -- see Service's own doc
// comment), allocates a fresh copy of every slice field so a caller
// mutating the result can never reach back into the schema itself, and
// changes no Register/Attach behavior whatsoever. s is always fully
// attached by the time a caller holds one: NewModule's Module.Attach is
// the only place a *Service is ever constructed, and it always populates
// schema before returning one (module.go's Attach) -- there is no
// partially-constructed *Service for this method to guard against.
//
// A Sensitive item's Default is redacted at this boundary, not at each
// consumer's: the descriptor is a document a caller renders and shares
// (RenderMarkdown's generated reference, an admin console's item listing),
// so the plaintext default goes through redactIf exactly as a Set's event
// values do on the bus -- see events.go for the marker semantics.
// RenderMarkdown's own renderDefault still applies redactIf a second time
// for hand-built descriptor slices; the marker is idempotent, so the
// double application is harmless.
func (s *Service) Describe() []ConfigItemDescriptor {
	out := make([]ConfigItemDescriptor, 0, len(s.schema.Load().items))
	for _, item := range s.schema.Load().items {
		out = append(out, ConfigItemDescriptor{
			Key:           item.key,
			Type:          item.typ,
			Sensitive:     item.sensitive,
			Public:        item.public,
			Description:   item.description,
			Group:         item.group,
			Min:           canonicalPtrCopy(item.minCanonical),
			Max:           canonicalPtrCopy(item.maxCanonical),
			HasDefault:    item.hasDefault,
			Default:       redactIf(item.sensitive, item.defaultCanonical),
			IsFeatureFlag: item.isFlag,
			FlagDependsOn: append([]string(nil), item.flagDeps...),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// canonicalPtrCopy returns a fresh *string carrying the same value as p, or
// nil when p is nil -- so a caller of Describe can never mutate the
// schema's own minCanonical/maxCanonical string through the pointer it
// gets back.
func canonicalPtrCopy(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
