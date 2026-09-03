package config

import (
	"fmt"
	"strconv"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// Actor identifies who changed a configuration value. It is a string type
// so the audit trail and the configs row's updated_by column carry exactly
// the same value the caller handed over -- no wrapping, no reformatting. A
// real deployment fills it from the authenticated principal (a user or
// system identifier); authn does not exist yet, so this milestone's
// callers pass whatever identifier their own context provides. An empty
// Actor is rejected by Set (ErrActorRequired): every write must be
// attributable to someone.
type Actor string

// Value is what a read of a configuration key returns and what a write
// carries. For a read, Data holds the effective value decoded to the Go
// kind of the key's declared Type -- string, int64, bool or
// time.Duration -- resolved from the narrowest scope the context entitles
// the caller to read down to the schema default, and Scope names the tier
// the value was actually resolved at (ScopeTenant, ScopeSystem, or the
// zero Scope when it came from the schema default). For a write, only Data
// is consulted; the target tier is Set's own scope parameter, never a
// property of the value.
//
// Redacted is true when the key is Sensitive and the value could not be
// served in the clear: a Watch delivery of a Sensitive change carries
// Data == nil and Redacted == true. Direct Get/GetTyped reads of a
// Sensitive key decrypt and return the actual value (that is the point of
// the read path); Redacted exists for the paths where the value must not
// travel -- the bus, the public endpoint -- so a caller can recognize the
// shape instead of mistaking nil for an unset item.
type Value struct {
	Data     any
	Scope    Scope
	Redacted bool
}

// Values are stored in the configs table as canonical strings: the string
// itself for a string item, "true"/"false" for a bool, decimal for an int,
// time.Duration.String() for a duration. The canonical form is what the
// table column holds (after encryption, for Sensitive items), what an
// ItemChangedEvent carries as OldValue/NewValue, and what change detection
// compares, so two Go values that canonicalize identically are the same
// configuration value no matter how they were written. Reads decode the
// canonical form back into the Go value of the item's declared Type.

// canonicalizeValue renders v as the canonical string the itemType column
// stores. It accepts exactly the Go kinds ConfigItem.Type names -- the same
// kinds pkgcore's declaration validation accepts for Default -- so a value
// that passes here round-trips through the table and back to the same
// semantic value. The error wraps pkgcore.ErrInvalidConfigItem only when the
// itemType itself is unknown, which Attach-time validation makes impossible;
// the returned error for a wrong-kind value is a plain descriptive error the
// caller wraps as ErrInvalidValue, and it never echoes the value itself,
// because the item may be Sensitive.
func canonicalizeValue(itemType string, v any) (string, error) {
	switch itemType {
	case "string":
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("want a string value")
		}
		return s, nil
	case "bool":
		b, ok := v.(bool)
		if !ok {
			return "", fmt.Errorf("want a bool value")
		}
		return strconv.FormatBool(b), nil
	case "int":
		switch n := v.(type) {
		case int:
			return strconv.FormatInt(int64(n), 10), nil
		case int64:
			return strconv.FormatInt(n, 10), nil
		default:
			return "", fmt.Errorf("want an int or int64 value")
		}
	case "duration":
		d, ok := v.(time.Duration)
		if !ok {
			return "", fmt.Errorf("want a time.Duration value")
		}
		return d.String(), nil
	default:
		return "", fmt.Errorf("%w: unknown config item type %q", pkgcore.ErrInvalidConfigItem, itemType)
	}
}

// decodeValue reverses canonicalizeValue, returning the Go value the item's
// declared Type serves to callers: string, int64, bool or time.Duration. An
// int canonical form always decodes to int64 -- int item values are served
// as int64 regardless of whether the schema declared the default as int or
// int64 -- which is why GetTyped[int] reports ErrTypedValueMismatch and
// GetTyped[int64] is the sanctioned read.
func decodeValue(itemType, canonical string) (any, error) {
	switch itemType {
	case "string":
		return canonical, nil
	case "bool":
		b, err := strconv.ParseBool(canonical)
		if err != nil {
			return nil, fmt.Errorf("config: stored value %q is not a bool", canonical)
		}
		return b, nil
	case "int":
		n, err := strconv.ParseInt(canonical, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("config: stored value %q is not an int", canonical)
		}
		return n, nil
	case "duration":
		d, err := time.ParseDuration(canonical)
		if err != nil {
			return nil, fmt.Errorf("config: stored value %q is not a duration", canonical)
		}
		return d, nil
	default:
		return nil, fmt.Errorf("%w: unknown config item type %q", pkgcore.ErrInvalidConfigItem, itemType)
	}
}

// rangeViolation reports the first declared bound v falls outside of, or
// nil when v is inside every declared bound. Bounds only exist for int and
// duration items (pkgcore's declaration validation guarantees it), both of
// which canonicalize to values comparable in the int64 domain. The error
// names the bound, never the value, because the item may be Sensitive.
func rangeViolation(itemType, canonical string, min, max *string) error {
	if min == nil && max == nil {
		return nil
	}
	at := func(action, step string, bound *string, violates func(int64) bool) error {
		limit, err := boundInt64(itemType, *bound)
		if err != nil {
			return fmt.Errorf("declared %s %q is not comparable", step, *bound)
		}
		if violates(limit) {
			return fmt.Errorf("value %s its declared %s %v", action, step, *bound)
		}
		return nil
	}
	n, err := boundInt64(itemType, canonical)
	if err != nil {
		return fmt.Errorf("value cannot be compared against its declared bounds")
	}
	if min != nil {
		if err := at("falls below", "Min", min, func(lo int64) bool { return n < lo }); err != nil {
			return err
		}
	}
	if max != nil {
		if err := at("exceeds", "Max", max, func(hi int64) bool { return n > hi }); err != nil {
			return err
		}
	}
	return nil
}

// boundInt64 maps one canonical value of an int or duration item into the
// int64 domain every range comparison happens in. Duration canonical forms
// ("1m30s") parse as durations, int forms as integers.
func boundInt64(itemType, canonical string) (int64, error) {
	if itemType == "duration" {
		d, err := time.ParseDuration(canonical)
		return int64(d), err
	}
	return strconv.ParseInt(canonical, 10, 64)
}
