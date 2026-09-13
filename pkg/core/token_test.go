package core

import (
	"bytes"
	"strings"
	"testing"
)

// cacheCap and the fixtures below back the white-box suites in this package.
type cacheCap interface{ cacheID() string }

type storeCap interface{ storeID() string }

type readerCap interface{ readerID() string }

type product struct{ id string }

func (p *product) cacheID() string  { return p.id }
func (p *product) storeID() string  { return p.id }
func (p *product) readerID() string { return p.id }

// schemaRes and the two below stand in for resource declarations: core stores
// them as given and never interprets them.
type schemaRes struct{ Namespace string }

type specRes struct{ Path string }

type namedRes interface{ resourceName() string }

func (s schemaRes) resourceName() string { return "schema:" + s.Namespace }
func (s specRes) resourceName() string   { return "spec:" + s.Path }

// resNames is a named type over []string. A bare []string is assignable to it
// yet not assertable to it, which is the gap the two match rules turn on.
type resNames []string

type otherRes struct{ Label string }

func (o otherRes) resourceName() string { return "other:" + o.Label }

func assertPanics(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("expected a panic mentioning %q, got none", want)
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, want) {
			t.Fatalf("panic message %q does not mention %q", msg, want)
		}
	}()
	fn()
}

func TestCapabilityTokenRejectsNonPointer(t *testing.T) {
	assertPanics(t, "must be a typed nil pointer", func() {
		capabilityType(42, "test")
	})
	assertPanics(t, "untyped nil", func() {
		capabilityType(cacheCap(nil), "test")
	})
}

func TestCapabilityTokenRejectsNonNilPointer(t *testing.T) {
	assertPanics(t, "must be a nil pointer", func() {
		capabilityType(&product{}, "test")
	})
}

func TestCapabilityTokenRejectsConcreteElem(t *testing.T) {
	assertPanics(t, "must point to an interface", func() {
		capabilityType((*bytes.Buffer)(nil), "test")
	})
	assertPanics(t, "must point to an interface", func() {
		capabilityType((*product)(nil), "test")
	})
}

func TestCapabilityTokenAcceptsInterfaceElem(t *testing.T) {
	got := capabilityType((*cacheCap)(nil), "test")
	if got.Name() != "cacheCap" {
		t.Fatalf("capabilityType returned %s, want cacheCap", got)
	}
}

func TestResourceTokenAcceptsConcreteElem(t *testing.T) {
	got := resourceType((*schemaRes)(nil), "test")
	if got.Name() != "schemaRes" {
		t.Fatalf("resourceType returned %s, want schemaRes", got)
	}
	// A resource token is still a typed nil pointer; only the "must be an
	// interface" rule is lifted.
	assertPanics(t, "must be a typed nil pointer", func() {
		resourceType(schemaRes{}, "test")
	})
	assertPanics(t, "must be a nil pointer", func() {
		resourceType(&schemaRes{}, "test")
	})
}
