package core_test

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/core/internal/regprobe"
)

// The capability fixtures for the black-box suites in this file and in
// run_test.go.
type Cache interface{ CacheID() string }

type Store interface{ StoreID() string }

type Greeter interface{ Greet() string }

type box struct{ id string }

func (b *box) CacheID() string { return b.id }
func (b *box) StoreID() string { return b.id }
func (b *box) Greet() string   { return "hello from " + b.id }

func recoverMessage(t *testing.T, fn func()) string {
	t.Helper()
	var msg string
	func() {
		defer func() {
			if r := recover(); r != nil {
				msg = fmt.Sprint(r)
			}
		}()
		fn()
	}()
	if msg == "" {
		t.Fatal("expected a panic, got none")
	}
	return msg
}

// TestRegisterDuplicateNamePanicsNamingBothPackagePaths pins the requirement
// that the panic locates both sides: the available set is decided by imports,
// so the other registrant may sit in a dependency rather than in the host's
// own code.
func TestRegisterDuplicateNamePanicsNamingBothPackagePaths(t *testing.T) {
	reg := core.New()
	regprobe.Register(reg, core.Module{Name: "duplicated"})

	msg := recoverMessage(t, func() {
		reg.Register(core.Module{Name: "duplicated"})
	})

	if !strings.Contains(msg, "duplicated") {
		t.Errorf("panic %q does not name the module", msg)
	}
	for _, want := range []string{
		"github.com/vislake/speed/pkg/core/internal/regprobe",
		"github.com/vislake/speed/pkg/core_test",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("panic %q does not name the registration site %s", msg, want)
		}
	}
}

func TestRegisterPanicsOnConcreteProvisionToken(t *testing.T) {
	reg := core.New()
	msg := recoverMessage(t, func() {
		reg.Register(core.Module{
			Name:     "concrete",
			Provides: []core.Provision{{Token: (*bytes.Buffer)(nil)}},
		})
	})
	if !strings.Contains(msg, "must point to an interface") {
		t.Fatalf("panic %q does not explain that a capability must be an interface", msg)
	}
	if _, ok := reg.Lookup("concrete"); ok {
		t.Error("a module with an illegal token must not be registered")
	}
}

func TestRegisterPanicsOnUntypedNilRequirementToken(t *testing.T) {
	reg := core.New()
	msg := recoverMessage(t, func() {
		reg.Register(core.Module{
			Name:     "untyped",
			Requires: []core.Requirement{{Token: Cache(nil)}},
		})
	})
	if !strings.Contains(msg, "untyped nil") {
		t.Fatalf("panic %q does not explain that the token is an untyped nil", msg)
	}
}

// TestNewRegistryDoesNotInheritProcessRegistrations pins the property that
// makes a hand-built registry the way to assemble a small group of modules.
func TestNewRegistryDoesNotInheritProcessRegistrations(t *testing.T) {
	// Registering only when absent keeps the test idempotent under -count>1:
	// the process registry outlives an individual test run.
	if _, ok := core.ProcessRegistry.Lookup("registry_test.process-probe"); !ok {
		core.ProcessRegistry.Register(core.Module{Name: "registry_test.process-probe"})
	}
	if _, ok := core.ProcessRegistry.Lookup("registry_test.process-probe"); !ok {
		t.Fatal("the process registry did not record the registration")
	}
	if _, ok := core.New().Lookup("registry_test.process-probe"); ok {
		t.Fatal("a registry from New inherited a process-level registration")
	}
}

func TestLookup(t *testing.T) {
	reg := core.New()
	reg.Register(core.Module{Name: "present", Provides: []core.Provision{{Token: (*Cache)(nil)}}})

	m, ok := reg.Lookup("present")
	if !ok {
		t.Fatal("Lookup missed a registered module")
	}
	if len(m.Provides) != 1 {
		t.Fatalf("Lookup returned %d provisions, want 1", len(m.Provides))
	}
	if _, ok := reg.Lookup("absent"); ok {
		t.Fatal("Lookup found a module that was never registered")
	}
}

// TestModulesReturnsEveryRegistration compares as a set: the design pins a
// name order on ResolveAll and Resources only, and Modules promises none.
func TestModulesReturnsEveryRegistration(t *testing.T) {
	reg := core.New()
	for _, name := range []string{"zeta", "alpha", "mid"} {
		reg.Register(core.Module{Name: name})
	}

	var got []string
	for _, m := range reg.Modules() {
		got = append(got, m.Name)
	}
	slices.Sort(got)
	if want := []string{"alpha", "mid", "zeta"}; !slices.Equal(got, want) {
		t.Fatalf("Modules returned %v, want the set %v", got, want)
	}
}
