package pkgcore

import (
	"fmt"
	"time"
)

// configItemTypes is the closed set of value types a ConfigItem.Type may
// name. The type decides how the item's Default, Min and Max are
// interpreted, and later how the dynamic-config module stores and serves the
// item's value, so an item declaring a type outside the set can never be
// read back coherently and is rejected at registration.
var configItemTypes = map[string]struct{}{
	"string":   {},
	"int":      {},
	"bool":     {},
	"duration": {},
}

// validateConfigItem checks that one item's fields describe a single
// coherent value. It is the declaration-time gate every registration goes
// through: a contradiction here would otherwise surface only later, as a
// runtime failure in the dynamic-config module or as an admin console form
// nobody can save. Errors wrap ErrInvalidConfigItem and name the key (when
// the item has one) and the contradiction, never the value of a Sensitive
// item.
//
// This guarantee is load-bearing for a CodeQL go/clear-text-logging alert
// traced through this function (go/authn's socialCredentialItems ->
// ConfigItem{Key: channel.secretKey, Sensitive: true} -> here -> up through
// Register/Bootstrap to a log call in the reference app's main.go, reviewed
// and confirmed a false positive): every error this function returns names
// only item.Key, a schema identifier, never item.Default or any other
// value-bearing field. Do not add a value to any error message here without
// re-auditing that alert.
func validateConfigItem(item ConfigItem) error {
	if item.Key == "" {
		return fmt.Errorf("%w: an item without a key cannot be registered", ErrInvalidConfigItem)
	}
	if _, ok := configItemTypes[item.Type]; !ok {
		return fmt.Errorf("%w: key %q has type %q, want one of string, int, bool or duration",
			ErrInvalidConfigItem, item.Key, item.Type)
	}
	// Checked here, before anything type-specific, so it guards every type
	// equally: a sensitive value is never served on a public endpoint, so a
	// declaration that asks for both can never be honored.
	if item.Sensitive && item.Public {
		return fmt.Errorf("%w: key %q is both Sensitive and Public; a sensitive value is never served on a public endpoint",
			ErrInvalidConfigItem, item.Key)
	}
	if err := validateConfigItemValue(item, "default", item.Default); err != nil {
		return err
	}

	if item.Type != "int" && item.Type != "duration" {
		if item.Min != nil || item.Max != nil {
			return fmt.Errorf("%w: key %q has type %q, whose values have no ordering; Min and Max are only defined for int and duration items",
				ErrInvalidConfigItem, item.Key, item.Type)
		}
		return nil
	}
	if err := validateConfigItemValue(item, "min", item.Min); err != nil {
		return err
	}
	if err := validateConfigItemValue(item, "max", item.Max); err != nil {
		return err
	}

	min, minSet := configItemBound(item.Min)
	max, maxSet := configItemBound(item.Max)
	if minSet && maxSet && min > max {
		return fmt.Errorf("%w: key %q declares Min %v above Max %v",
			ErrInvalidConfigItem, item.Key, item.Min, item.Max)
	}
	if item.Default == nil {
		return nil
	}
	def, _ := configItemBound(item.Default)
	if minSet && def < min {
		if item.Sensitive {
			return fmt.Errorf("%w: key %q default falls below its declared Min %v",
				ErrInvalidConfigItem, item.Key, item.Min)
		}
		return fmt.Errorf("%w: key %q default %v falls below its declared Min %v",
			ErrInvalidConfigItem, item.Key, item.Default, item.Min)
	}
	if maxSet && def > max {
		if item.Sensitive {
			return fmt.Errorf("%w: key %q default exceeds its declared Max %v",
				ErrInvalidConfigItem, item.Key, item.Max)
		}
		return fmt.Errorf("%w: key %q default %v exceeds its declared Max %v",
			ErrInvalidConfigItem, item.Key, item.Default, item.Max)
	}
	return nil
}

// validateConfigItemValue checks that v is a Go value of the kind item.Type
// requires. field names the part of the declaration being checked ("default",
// "min" or "max") for the error message. A nil v is a legal "not declared"
// and passes for every type.
func validateConfigItemValue(item ConfigItem, field string, v any) error {
	if v == nil {
		return nil
	}
	ok := false
	switch item.Type {
	case "string":
		_, ok = v.(string)
	case "bool":
		_, ok = v.(bool)
	case "int":
		_, ok = configItemBound(v)
	case "duration":
		_, ok = v.(time.Duration)
	}
	if !ok {
		return fmt.Errorf("%w: key %q %s has Go type %T, want %s",
			ErrInvalidConfigItem, item.Key, field, v, configItemKindName(item.Type))
	}
	return nil
}

// configItemKindName names the Go value kind a ConfigItem type requires, for
// error messages. itemType is validated before it reaches this function, so
// the fallback is unreachable.
func configItemKindName(itemType string) string {
	switch itemType {
	case "string":
		return "string"
	case "bool":
		return "bool"
	case "int":
		return "int or int64"
	case "duration":
		return "time.Duration"
	}
	return "a value of the declared type"
}

// configItemBound converts an int, int64 or time.Duration bound or default
// to the int64 domain every comparison happens in. A conversion from int is
// always lossless: int is at most 64 bits, so int64 holds any int value. A
// time.Duration has to be listed as its own case -- although its underlying
// type is int64, a type switch on the named type never matches the int64
// branch, and an unnoticed miss would silently skip every range comparison
// for duration items. The second result is false for every other type, nil
// included.
func configItemBound(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case time.Duration:
		return int64(n), true
	default:
		return 0, false
	}
}
