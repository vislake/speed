package config

import (
	"errors"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// Tests for values.go's canonical-form machinery: the canonicalizeValue /
// decodeValue round trip every stored value goes through, the bounds
// comparison rangeViolation performs, and boundInt64's mapping of both
// comparable types into one int64 domain.

func TestCanonicalizeValue_RoundTripsPerType(t *testing.T) {
	tests := []struct {
		name        string
		itemType    string
		value       any
		want        string
		wantDecoded any
	}{
		{name: "string", itemType: "string", value: "hello", want: "hello", wantDecoded: "hello"},
		{name: "bool true", itemType: "bool", value: true, want: "true", wantDecoded: true},
		{name: "bool false", itemType: "bool", value: false, want: "false", wantDecoded: false},
		{name: "int from int", itemType: "int", value: int(42), want: "42", wantDecoded: int64(42)},
		{name: "int from int64", itemType: "int", value: int64(42), want: "42", wantDecoded: int64(42)},
		{name: "duration", itemType: "duration", value: 90 * time.Second, want: "1m30s", wantDecoded: 90 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			canonical, err := canonicalizeValue(tc.itemType, tc.value)
			if err != nil {
				t.Fatalf("canonicalizeValue returned an error: %v", err)
			}
			if canonical != tc.want {
				t.Fatalf("canonicalizeValue(%v) = %q, want %q", tc.value, canonical, tc.want)
			}
			decoded, err := decodeValue(tc.itemType, canonical)
			if err != nil {
				t.Fatalf("decodeValue returned an error: %v", err)
			}
			if decoded != tc.wantDecoded {
				t.Fatalf("decodeValue(%q) = %#v (%T), want %#v (%T)", canonical, decoded, decoded, tc.wantDecoded, tc.wantDecoded)
			}
		})
	}
}

func TestCanonicalizeValue_RejectsWrongGoKinds(t *testing.T) {
	tests := []struct {
		name     string
		itemType string
		value    any
	}{
		{name: "int for string", itemType: "string", value: 5},
		{name: "string for bool", itemType: "bool", value: "true"},
		{name: "bool for int", itemType: "int", value: true},
		{name: "string for duration", itemType: "duration", value: "1m30s"},
		{name: "float for int", itemType: "int", value: 1.5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if canonical, err := canonicalizeValue(tc.itemType, tc.value); err == nil {
				t.Fatalf("canonicalizeValue(%v) = %q, want an error", tc.value, canonical)
			}
		})
	}
}

func TestCanonicalizeValue_RejectsUnknownItemType(t *testing.T) {
	_, err := canonicalizeValue("decimal", 1)
	if !errors.Is(err, pkgcore.ErrInvalidConfigItem) {
		t.Fatalf("want an error wrapping pkgcore.ErrInvalidConfigItem, got %v", err)
	}
	_, err = decodeValue("decimal", "1")
	if !errors.Is(err, pkgcore.ErrInvalidConfigItem) {
		t.Fatalf("decodeValue: want an error wrapping pkgcore.ErrInvalidConfigItem, got %v", err)
	}
}

func TestDecodeValue_RejectsStoredCorruption(t *testing.T) {
	if _, err := decodeValue("int", "not-a-number"); err == nil {
		t.Fatal("decodeValue of a non-numeric int canonical form must fail")
	}
	if _, err := decodeValue("bool", "maybe"); err == nil {
		t.Fatal("decodeValue of a non-bool canonical form must fail")
	}
	if _, err := decodeValue("duration", "forty winks"); err == nil {
		t.Fatal("decodeValue of a non-duration canonical form must fail")
	}
}

func TestBoundInt64_MapsBothComparableTypes(t *testing.T) {
	n, err := boundInt64("int", "42")
	if err != nil || n != 42 {
		t.Fatalf("boundInt64(int, 42) = %d, %v", n, err)
	}
	d, err := boundInt64("duration", "1m30s")
	if err != nil || d != int64(90*time.Second) {
		t.Fatalf("boundInt64(duration, 1m30s) = %d, %v", d, err)
	}
	if _, err := boundInt64("duration", "nope"); err == nil {
		t.Fatal("boundInt64 must fail on an unparseable duration")
	}
}

func TestRangeViolation_AppliesDeclaredBounds(t *testing.T) {
	lo, hi := "1", "10"
	tests := []struct {
		name      string
		itemType  string
		canonical string
		min, max  *string
		wantErr   bool
	}{
		{name: "no bounds", itemType: "int", canonical: "42", wantErr: false},
		{name: "within both", itemType: "int", canonical: "5", min: &lo, max: &hi, wantErr: false},
		{name: "equal to lower bound", itemType: "int", canonical: "1", min: &lo, max: &hi, wantErr: false},
		{name: "equal to upper bound", itemType: "int", canonical: "10", min: &lo, max: &hi, wantErr: false},
		{name: "below min", itemType: "int", canonical: "0", min: &lo, max: &hi, wantErr: true},
		{name: "above max", itemType: "int", canonical: "11", min: &lo, max: &hi, wantErr: true},
		{name: "min only", itemType: "int", canonical: "0", min: &lo, wantErr: true},
		{name: "max only", itemType: "int", canonical: "11", max: &hi, wantErr: true},
		{name: "duration below min", itemType: "duration", canonical: "500ms", min: &lo, wantErr: true},
		{name: "duration above max", itemType: "duration", canonical: "1m31s", max: &hi, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := rangeViolation(tc.itemType, tc.canonical, tc.min, tc.max)
			if tc.wantErr && err == nil {
				t.Fatal("rangeViolation reported no violation, want one")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("rangeViolation returned an unexpected error: %v", err)
			}
		})
	}
}
