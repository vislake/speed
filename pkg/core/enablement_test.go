package core

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

func exclusive(tok Token) Provision { return Provision{Token: tok, Exclusive: true} }

func shared(tok Token) Provision { return Provision{Token: tok} }

func stated(name string, state EnablementState, reason string, provs ...Provision) stance {
	return stance{
		module: Module{Name: name, Provides: provs},
		state:  Enablement{State: state, Reason: reason},
	}
}

func mustResolve(t *testing.T, stances ...stance) map[string]Enablement {
	t.Helper()
	final, err := resolveEnablement(stances)
	if err != nil {
		t.Fatalf("resolution failed: %v", err)
	}
	return final
}

func assertEnabled(t *testing.T, final map[string]Enablement, names ...string) {
	t.Helper()
	for _, name := range names {
		if got := final[name]; got.State != StateEnabled {
			t.Errorf("module %q ended as %+v, want enabled", name, got)
		}
	}
}

func assertDisabled(t *testing.T, final map[string]Enablement, name, reasonFragment string) {
	t.Helper()
	got := final[name]
	if got.State != StateDisabled {
		t.Fatalf("module %q ended as %+v, want disabled", name, got)
	}
	if !strings.Contains(got.Reason, reasonFragment) {
		t.Fatalf("module %q was disabled with reason %q, which does not mention %q", name, got.Reason, reasonFragment)
	}
}

func TestBothExclusiveAutoViolates(t *testing.T) {
	_, err := resolveEnablement([]stance{
		stated("memory", StateAuto, "", exclusive((*cacheCap)(nil))),
		stated("remote", StateAuto, "", exclusive((*cacheCap)(nil))),
	})
	if !errors.Is(err, ErrExclusiveViolated) {
		t.Fatalf("resolution returned %v, want ErrExclusiveViolated", err)
	}
	for _, name := range []string{"memory", "remote"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}

// TestExclusiveAutoStandsDownForExplicitProvider is the default-implementation
// case: configure the remote one and it wins, leave it unconfigured and the
// in-memory one runs, with nobody maintaining a module list.
func TestExclusiveAutoStandsDownForExplicitProvider(t *testing.T) {
	final := mustResolve(t,
		stated("memory", StateAuto, "", exclusive((*cacheCap)(nil))),
		stated("remote", StateEnabled, "", exclusive((*cacheCap)(nil))),
	)
	assertDisabled(t, final, "memory", "already has an explicitly enabled provider")
	assertEnabled(t, final, "remote")
}

// TestPropagationLeavesNonExclusiveProvidersCoexisting pins that propagation
// only disables the exclusive default: mail states StateAuto and never claimed
// exclusivity, so a doomed exclusive default beside it must not drag it down.
func TestPropagationLeavesNonExclusiveProvidersCoexisting(t *testing.T) {
	final := mustResolve(t,
		stated("console", StateAuto, "", exclusive((*storeCap)(nil))),
		stated("mail", StateAuto, "", shared((*storeCap)(nil))),
		stated("sms", StateEnabled, "", shared((*storeCap)(nil))),
	)
	assertDisabled(t, final, "console", "already has an explicitly enabled provider")
	assertEnabled(t, final, "mail", "sms")
}

// TestNonExclusiveDefaultAgainstExclusiveExplicitFails pins the consequence
// the design spells out: the side that stands down must claim exclusivity
// itself, otherwise propagation has nobody to disable and adjudication fails.
func TestNonExclusiveDefaultAgainstExclusiveExplicitFails(t *testing.T) {
	_, err := resolveEnablement([]stance{
		stated("memory", StateAuto, "", shared((*cacheCap)(nil))),
		stated("remote", StateEnabled, "", exclusive((*cacheCap)(nil))),
	})
	if !errors.Is(err, ErrExclusiveViolated) {
		t.Fatalf("resolution returned %v, want ErrExclusiveViolated", err)
	}
}

func TestDisabledExclusiveProviderDoesNotConstrain(t *testing.T) {
	final := mustResolve(t,
		stated("exclusive-off", StateDisabled, "switched off", exclusive((*storeCap)(nil))),
		stated("mail", StateAuto, "", shared((*storeCap)(nil))),
		stated("sms", StateAuto, "", shared((*storeCap)(nil))),
	)
	assertEnabled(t, final, "mail", "sms")
	assertDisabled(t, final, "exclusive-off", "switched off")
}

func TestDisabledWithoutReason(t *testing.T) {
	_, err := resolveEnablement([]stance{
		stated("silent", StateDisabled, ""),
	})
	if !errors.Is(err, ErrMissingReason) {
		t.Fatalf("resolution returned %v, want ErrMissingReason", err)
	}
	if !strings.Contains(err.Error(), "silent") {
		t.Fatalf("error %q does not name the module", err)
	}
}

// TestAbsentPrepareIsAuto pins the zero value: a module without a Prepare
// callback states StateAuto, so it stands down like any other exclusive
// default.
func TestAbsentPrepareIsAuto(t *testing.T) {
	if (Enablement{}).State != StateAuto {
		t.Fatal("the zero Enablement is not StateAuto")
	}
	final := mustResolve(t,
		stance{module: Module{Name: "silent-default", Provides: []Provision{exclusive((*cacheCap)(nil))}}},
		stated("remote", StateEnabled, "", exclusive((*cacheCap)(nil))),
	)
	assertDisabled(t, final, "silent-default", "already has an explicitly enabled provider")
	assertEnabled(t, final, "remote")
}

// TestResolutionIndependentOfRegistrationOrder pins that the outcome comes
// from a snapshot: init only guarantees a partial order, so anything derived
// from registration order would change from build to build.
func TestResolutionIndependentOfRegistrationOrder(t *testing.T) {
	base := []stance{
		stated("console", StateAuto, "", exclusive((*storeCap)(nil))),
		stated("mail", StateEnabled, "", shared((*storeCap)(nil))),
		stated("sms", StateEnabled, "", shared((*storeCap)(nil))),
	}
	want := mustResolve(t, base...)

	orders := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, order := range orders {
		permuted := make([]stance, 0, len(base))
		for _, i := range order {
			permuted = append(permuted, base[i])
		}
		got, err := resolveEnablement(permuted)
		if err != nil {
			t.Fatalf("order %v: resolution failed: %v", order, err)
		}
		if !maps.Equal(got, want) {
			t.Fatalf("order %v produced %v, want %v", order, got, want)
		}
	}
}

// TestConfigModuleCountsAsEnabledDuringResolution pins that the config module
// takes part like any explicitly enabled provider: a second provider of what
// it delivers stands down rather than surfacing at lookup time.
func TestConfigModuleCountsAsEnabledDuringResolution(t *testing.T) {
	final := mustResolve(t,
		stated(configModuleName, StateEnabled, "", exclusive((*readerCap)(nil))),
		stated("second-reader", StateAuto, "", exclusive((*readerCap)(nil))),
	)
	assertEnabled(t, final, configModuleName)
	assertDisabled(t, final, "second-reader", "already has an explicitly enabled provider")
}

func TestModuleWithoutProvisionsOnlyUsesOwnState(t *testing.T) {
	final := mustResolve(t,
		stated("plain", StateAuto, ""),
		stated("off", StateDisabled, "not configured"),
		stated("remote", StateEnabled, "", exclusive((*cacheCap)(nil))),
	)
	assertEnabled(t, final, "plain", "remote")
	assertDisabled(t, final, "off", "not configured")
}

// TestExclusiveClaimLapsesWhenPropagationDisablesTheClaimant pins the snapshot
// semantics for a module that claims two capabilities: once propagation
// disables it over one of them, its claim on the other lapses with it.
func TestExclusiveClaimLapsesWhenPropagationDisablesTheClaimant(t *testing.T) {
	final := mustResolve(t,
		stated("x", StateAuto, "", exclusive((*cacheCap)(nil)), exclusive((*storeCap)(nil))),
		stated("y", StateEnabled, "", exclusive((*cacheCap)(nil))),
		stated("z", StateEnabled, "", shared((*storeCap)(nil))),
	)
	assertDisabled(t, final, "x", "already has an explicitly enabled provider")
	assertEnabled(t, final, "y", "z")
}

func TestResolutionCoversEveryStance(t *testing.T) {
	final := mustResolve(t,
		stated("a", StateAuto, ""),
		stated("b", StateEnabled, ""),
		stated("c", StateDisabled, "off"),
	)
	got := slices.Sorted(maps.Keys(final))
	if want := []string{"a", "b", "c"}; !slices.Equal(got, want) {
		t.Fatalf("resolution reported on %v, want %v", got, want)
	}
}

// TestExclusiveResolutionGroupsByTypeIdentity pins the resolution half of the
// same rule: an exclusive provider of superCacheCap and an exclusive provider
// of the capability it embeds are providers of two capabilities, so neither
// crowds the other out.
func TestExclusiveResolutionGroupsByTypeIdentity(t *testing.T) {
	final := mustResolve(t,
		stated("super", StateAuto, "", exclusive((*superCacheCap)(nil))),
		stated("plain", StateAuto, "", exclusive((*cacheCap)(nil))),
	)
	assertEnabled(t, final, "super", "plain")
}
