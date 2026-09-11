package pkgcore

import (
	"context"
	"testing"
)

// TestGlobalComponents_ReturnsTheRegisteredSetInOrder pins the enumeration's
// contract: the snapshot carries every registered component in registration
// order (a component registered after the snapshot is absent from it, a
// later snapshot holds it last), and the returned slice is a copy -- writing
// to it never touches the registration itself.
func TestGlobalComponents_ReturnsTheRegisteredSetInOrder(t *testing.T) {
	before := GlobalComponents()
	seen := make(map[string]bool, len(before))
	for _, c := range before {
		if c.Name == "" {
			t.Fatal("GlobalComponents() returned a component with an empty name")
		}
		seen[c.Name] = true
	}

	// A probe registered once per process: the same test binary run with
	// -count>1 executes this function again, and a duplicate registration
	// would be an error rather than a property of the enumeration.
	const probeName = "component.global_test.probe"
	if !seen[probeName] {
		probe := Component{
			Name: probeName,
			New: func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) {
				return &struct{}{}, nil
			},
		}
		if err := Register(probe); err != nil {
			t.Fatalf("Register(%q) error = %v", probeName, err)
		}
		before = GlobalComponents()
	}

	after := GlobalComponents()
	if len(after) != len(before) {
		t.Fatalf("GlobalComponents() returned %d components, want the %d registered so far", len(after), len(before))
	}
	if got := after[len(after)-1].Name; got != probeName {
		t.Errorf("the last component = %q, want the most recently registered %q", got, probeName)
	}

	// The snapshot is a copy: mutating it leaves the registration intact.
	first := after[0].Name
	after[0] = Component{}
	if got := GlobalComponents()[0].Name; got != first {
		t.Errorf("mutating a snapshot changed the registration: first component = %q, want %q", got, first)
	}
}
