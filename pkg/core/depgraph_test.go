package core

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func needs(tokens ...Token) []Requirement {
	out := make([]Requirement, 0, len(tokens))
	for _, tok := range tokens {
		out = append(out, Requirement{Token: tok})
	}
	return out
}

func allEnabled(mods ...Module) map[string]Enablement {
	final := make(map[string]Enablement, len(mods))
	for _, m := range mods {
		final[m.Name] = Enablement{State: StateEnabled}
	}
	return final
}

func mustPlan(t *testing.T, mods ...Module) []string {
	t.Helper()
	order, err := planConstruction(mods, mods, allEnabled(mods...))
	if err != nil {
		t.Fatalf("planning failed: %v", err)
	}
	return order
}

func indexOf(t *testing.T, order []string, name string) int {
	t.Helper()
	i := slices.Index(order, name)
	if i < 0 {
		t.Fatalf("module %q is missing from the order %v", name, order)
	}
	return i
}

func TestMissingProviderNoDeclarer(t *testing.T) {
	consumer := Module{Name: "consumer", Requires: needs((*cacheCap)(nil))}

	_, err := planConstruction([]Module{consumer}, []Module{consumer}, allEnabled(consumer))
	if !errors.Is(err, ErrMissingProvider) {
		t.Fatalf("planning returned %v, want ErrMissingProvider", err)
	}
	if !strings.Contains(err.Error(), "no module delivers") {
		t.Errorf("error %q does not read as the no-declarer case", err)
	}
	if !strings.Contains(err.Error(), "Import") {
		t.Errorf("error %q does not state a fixing action", err)
	}
}

// TestMissingProviderTextCarriesDisableReason pins that a capability lost to
// resolution reports why its provider is not running, not merely that nobody
// provides it.
func TestMissingProviderTextCarriesDisableReason(t *testing.T) {
	provider := Module{Name: "provider", Provides: provides((*cacheCap)(nil))}
	consumer := Module{Name: "consumer", Requires: needs((*cacheCap)(nil))}
	final := map[string]Enablement{
		"consumer": {State: StateEnabled},
		"provider": {State: StateDisabled, Reason: "no connection address configured"},
	}

	_, err := planConstruction([]Module{consumer}, []Module{consumer, provider}, final)
	if !errors.Is(err, ErrMissingProvider) {
		t.Fatalf("planning returned %v, want ErrMissingProvider", err)
	}
	for _, want := range []string{"provider", "no connection address configured"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestOptionalMissingIsNotAnError(t *testing.T) {
	consumer := Module{
		Name:     "consumer",
		Requires: []Requirement{{Token: (*cacheCap)(nil), Optional: true}},
	}

	order, err := planConstruction([]Module{consumer}, []Module{consumer}, allEnabled(consumer))
	if err != nil {
		t.Fatalf("an absent optional dependency failed the planning: %v", err)
	}
	if !slices.Equal(order, []string{"consumer"}) {
		t.Fatalf("order is %v, want just the consumer", order)
	}
}

func TestOptionalPresentStillOrders(t *testing.T) {
	provider := Module{Name: "zzz-provider", Provides: provides((*cacheCap)(nil))}
	consumer := Module{
		Name:     "aaa-consumer",
		Requires: []Requirement{{Token: (*cacheCap)(nil), Optional: true}},
	}

	order := mustPlan(t, consumer, provider)
	if indexOf(t, order, "zzz-provider") > indexOf(t, order, "aaa-consumer") {
		t.Fatalf("order is %v, want the present optional provider first", order)
	}
}

func TestCycleDetectedNamingModules(t *testing.T) {
	a := Module{Name: "a", Requires: needs((*storeCap)(nil)), Provides: provides((*cacheCap)(nil))}
	b := Module{Name: "b", Requires: needs((*cacheCap)(nil)), Provides: provides((*storeCap)(nil))}

	_, err := planConstruction([]Module{a, b}, []Module{a, b}, allEnabled(a, b))
	if !errors.Is(err, ErrDependencyCycle) {
		t.Fatalf("planning returned %v, want ErrDependencyCycle", err)
	}
	for _, name := range []string{"a", "b"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
	if !strings.Contains(err.Error(), "Init") {
		t.Errorf("error %q does not point at the way out", err)
	}
}

func TestSelfCycle(t *testing.T) {
	m := Module{
		Name:     "self",
		Requires: needs((*cacheCap)(nil)),
		Provides: provides((*cacheCap)(nil)),
	}

	_, err := planConstruction([]Module{m}, []Module{m}, allEnabled(m))
	if !errors.Is(err, ErrDependencyCycle) {
		t.Fatalf("planning returned %v, want ErrDependencyCycle", err)
	}
	if !strings.Contains(err.Error(), "self") {
		t.Errorf("error %q does not name the module", err)
	}
}

func TestConsumerFollowsAllProviders(t *testing.T) {
	one := Module{Name: "one", Provides: provides((*storeCap)(nil))}
	two := Module{Name: "two", Provides: provides((*storeCap)(nil))}
	consumer := Module{Name: "a-consumer", Requires: needs((*storeCap)(nil))}

	order := mustPlan(t, consumer, one, two)
	at := indexOf(t, order, "a-consumer")
	if indexOf(t, order, "one") > at || indexOf(t, order, "two") > at {
		t.Fatalf("order is %v, want the consumer after both providers", order)
	}
}

// TestConfigModuleOrdersFirst pins the implicit dependency: nobody declares
// it, and everyone is constructed after it.
func TestConfigModuleOrdersFirst(t *testing.T) {
	cfg := Module{Name: configModuleName, Provides: provides((*readerCap)(nil))}
	a := Module{Name: "aaa", Provides: provides((*cacheCap)(nil))}
	b := Module{Name: "bbb", Requires: needs((*cacheCap)(nil))}

	order := mustPlan(t, a, b, cfg)
	if order[0] != configModuleName {
		t.Fatalf("order is %v, want config first", order)
	}
}

func TestOrderStableAcrossRegistrationOrders(t *testing.T) {
	cfg := Module{Name: configModuleName}
	a := Module{Name: "alpha", Provides: provides((*cacheCap)(nil))}
	b := Module{Name: "bravo", Requires: needs((*cacheCap)(nil)), Provides: provides((*storeCap)(nil))}
	c := Module{Name: "charlie", Requires: needs((*storeCap)(nil))}
	// delta shares bravo's tier: nothing orders the two, so the tie is
	// broken by name and must not follow registration order.
	d := Module{Name: "delta", Requires: needs((*cacheCap)(nil))}

	want := mustPlan(t, cfg, a, b, c, d)
	for _, permuted := range [][]Module{
		{d, c, b, a, cfg},
		{b, cfg, d, a, c},
		{c, a, cfg, d, b},
	} {
		got := mustPlan(t, permuted...)
		if !slices.Equal(got, want) {
			t.Fatalf("registration order changed the construction order: got %v, want %v", got, want)
		}
	}
	// Each step takes the smallest name among those whose dependencies are
	// already placed, so delta lands after charlie became ready.
	if !slices.Equal(want, []string{configModuleName, "alpha", "bravo", "charlie", "delta"}) {
		t.Fatalf("order is %v, want ties broken by name at each step", want)
	}
}
