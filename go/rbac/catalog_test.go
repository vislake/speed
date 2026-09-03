package rbac

import (
	"reflect"
	"testing"
)

func TestNewCatalog_Has(t *testing.T) {
	c := newCatalog([]string{"notes:read", "notes:write", "rbac:manage"})
	for _, perm := range []string{"notes:read", "notes:write", "rbac:manage"} {
		if !c.Has(perm) {
			t.Fatalf("Has(%q) = false on a catalog that was built with it", perm)
		}
	}
	for _, perm := range []string{"notes:delete", "notes", "", "NOTES:READ"} {
		if c.Has(perm) {
			t.Fatalf("Has(%q) = true on a catalog that was not built with it", perm)
		}
	}
}

func TestNewCatalog_Empty_DeniesEverything(t *testing.T) {
	// A host whose modules declared no permissions must answer false for
	// every question, not panic and not accept anything: fail closed.
	c := newCatalog(nil)
	if c.Has("anything") {
		t.Fatal("an empty catalog claimed to know a permission")
	}
	if got := c.permissions(); len(got) != 0 {
		t.Fatalf("an empty catalog returned %v, want no permissions", got)
	}
}

func TestNewCatalog_DropsTheEmptyString(t *testing.T) {
	// "" is not a permission. Keeping it would let a grant of "" pass the
	// catalog check that exists precisely to reject undeclared strings.
	c := newCatalog([]string{"", "notes:read"})
	if c.Has("") {
		t.Fatal("the empty string was accepted as a declared permission")
	}
	if got, want := c.permissions(), []string{"notes:read"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("permissions() = %v, want %v", got, want)
	}
}

func TestNewCatalog_CollapsesDuplicates(t *testing.T) {
	c := newCatalog([]string{"notes:read", "notes:read"})
	if got, want := c.permissions(), []string{"notes:read"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("permissions() = %v, want %v", got, want)
	}
}

func TestCatalog_Permissions_SortedAndCopied(t *testing.T) {
	c := newCatalog([]string{"rbac:manage", "notes:write", "notes:read"})

	want := []string{"notes:read", "notes:write", "rbac:manage"}
	if got := c.permissions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("permissions() = %v, want it sorted as %v", got, want)
	}

	// The frozen snapshot must not be mutable through the returned slice:
	// this set decides whether a grant is legal.
	mutated := c.permissions()
	mutated[0] = "tampered"
	if got := c.permissions(); !reflect.DeepEqual(got, want) {
		t.Fatalf("permissions() = %v after a caller mutated an earlier result, want %v", got, want)
	}
}
