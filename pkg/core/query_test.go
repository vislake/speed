package core

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// seed registers a module and, when instance is non-nil, records it as that
// module's product. The lifecycle driver does this in New; these suites are
// white-box so that queries can be pinned before the driver exists.
func seed(t *testing.T, r *Registry, m Module, instance any) {
	t.Helper()
	r.Register(m)
	if instance != nil {
		r.instances[m.Name] = instance
	}
}

func provides(tokens ...Token) []Provision {
	out := make([]Provision, 0, len(tokens))
	for _, tok := range tokens {
		out = append(out, Provision{Token: tok})
	}
	return out
}

// TestResolveIgnoresUndeclaredImplementation pins the declaration as the sole
// authority: a product that happens to implement a capability its module never
// declared is invisible.
func TestResolveIgnoresUndeclaredImplementation(t *testing.T) {
	reg := New()
	seed(t, reg, Module{Name: "undeclared"}, &product{id: "undeclared"})

	_, err := reg.Resolve((*cacheCap)(nil))
	if !errors.Is(err, ErrMissingProvider) {
		t.Fatalf("Resolve returned %v, want ErrMissingProvider", err)
	}
}

func TestResolveAmbiguous(t *testing.T) {
	reg := New()
	seed(t, reg, Module{Name: "one", Provides: provides((*cacheCap)(nil))}, &product{id: "one"})
	seed(t, reg, Module{Name: "two", Provides: provides((*cacheCap)(nil))}, &product{id: "two"})

	_, err := reg.Resolve((*cacheCap)(nil))
	if !errors.Is(err, ErrAmbiguousProvider) {
		t.Fatalf("Resolve returned %v, want ErrAmbiguousProvider", err)
	}
	for _, name := range []string{"one", "two"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name provider %s", err, name)
		}
	}
}

// TestResolveDoesNotFilterByRequires pins that Requires orders construction
// and validates it, and does not limit what a module can see: a dependency of
// a dependency never appears in one's own declarations.
func TestResolveDoesNotFilterByRequires(t *testing.T) {
	reg := New()
	seed(t, reg, Module{Name: "c"}, &product{id: "c"})
	seed(t, reg, Module{Name: "d", Provides: provides((*storeCap)(nil))}, &product{id: "d"})

	got, err := reg.Resolve((*storeCap)(nil))
	if err != nil {
		t.Fatalf("Resolve failed for an undeclared but constructed capability: %v", err)
	}
	if got.(storeCap).storeID() != "d" {
		t.Fatalf("Resolve returned %v, want d's product", got)
	}
}

// TestResolveNamesLaterConstructedProvider separates "constructed later" from
// "nobody delivers it": the fixing actions differ.
func TestResolveNamesLaterConstructedProvider(t *testing.T) {
	reg := New()
	seed(t, reg, Module{Name: "later", Provides: provides((*cacheCap)(nil))}, nil)

	_, err := reg.Resolve((*cacheCap)(nil))
	if !errors.Is(err, ErrMissingProvider) {
		t.Fatalf("Resolve returned %v, want ErrMissingProvider", err)
	}
	for _, want := range []string{"later", "Requires", "Init"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	other := New()
	_, absent := other.Resolve((*cacheCap)(nil))
	if !errors.Is(absent, ErrMissingProvider) {
		t.Fatalf("Resolve returned %v, want ErrMissingProvider", absent)
	}
	if strings.Contains(absent.Error(), "Requires") {
		t.Errorf("the no-declarer message %q reads like the constructed-later one", absent)
	}
}

func TestResolveAllSortedByModuleName(t *testing.T) {
	reg := New()
	for _, name := range []string{"zeta", "alpha", "mid"} {
		seed(t, reg, Module{Name: name, Provides: provides((*cacheCap)(nil))}, &product{id: name})
	}

	var got []string
	for _, v := range reg.ResolveAll((*cacheCap)(nil)) {
		got = append(got, v.(cacheCap).cacheID())
	}
	if want := []string{"alpha", "mid", "zeta"}; !slices.Equal(got, want) {
		t.Fatalf("ResolveAll returned %v, want %v", got, want)
	}
}

func TestResolveAllEmptyReturnsEmptySlice(t *testing.T) {
	got := New().ResolveAll((*cacheCap)(nil))
	if got == nil {
		t.Fatal("ResolveAll returned nil, want an empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("ResolveAll returned %v, want empty", got)
	}
}

func TestResourcesMatchByAssignabilityInterface(t *testing.T) {
	reg := New()
	seed(t, reg, Module{Name: "a", Resources: []any{schemaRes{Namespace: "cache"}}}, nil)
	seed(t, reg, Module{Name: "b", Resources: []any{specRes{Path: "/v1"}}}, nil)
	seed(t, reg, Module{Name: "c", Resources: []any{otherRes{Label: "x"}}}, nil)

	got := reg.Resources((*namedRes)(nil))
	if len(got) != 3 {
		t.Fatalf("Resources collected %d declarations, want 3", len(got))
	}
}

func TestResourcesMatchByAssignabilityConcrete(t *testing.T) {
	reg := New()
	seed(t, reg, Module{Name: "a", Resources: []any{schemaRes{Namespace: "cache"}}}, nil)
	seed(t, reg, Module{Name: "b", Resources: []any{specRes{Path: "/v1"}}}, nil)

	got := reg.Resources((*schemaRes)(nil))
	if len(got) != 1 {
		t.Fatalf("Resources collected %d declarations, want only the schema one", len(got))
	}
	if got[0].Value.(schemaRes).Namespace != "cache" {
		t.Fatalf("Resources returned %v, want the cache schema", got[0].Value)
	}
}

func TestResourcesCarryModuleName(t *testing.T) {
	reg := New()
	seed(t, reg, Module{Name: "declarer", Resources: []any{schemaRes{Namespace: "cache"}}}, nil)

	got := reg.Resources((*schemaRes)(nil))
	if len(got) != 1 || got[0].Module != "declarer" {
		t.Fatalf("Resources returned %v, want it tagged with declarer", got)
	}
}

func TestResourcesSortedByModuleName(t *testing.T) {
	reg := New()
	for _, name := range []string{"zeta", "alpha", "mid"} {
		seed(t, reg, Module{Name: name, Resources: []any{schemaRes{Namespace: name}}}, nil)
	}

	var got []string
	for _, res := range reg.Resources((*schemaRes)(nil)) {
		got = append(got, res.Module)
	}
	if want := []string{"alpha", "mid", "zeta"}; !slices.Equal(got, want) {
		t.Fatalf("Resources returned %v, want %v", got, want)
	}
}

// TestResourcesKeepsMultipleFromOneModule pins what ADR-module-resources
// leaves to the consumer: the mechanism itself does not cap the count.
func TestResourcesKeepsMultipleFromOneModule(t *testing.T) {
	reg := New()
	seed(t, reg, Module{
		Name:      "twice",
		Resources: []any{schemaRes{Namespace: "first"}, schemaRes{Namespace: "second"}},
	}, nil)

	got := reg.Resources((*schemaRes)(nil))
	if len(got) != 2 {
		t.Fatalf("Resources returned %d declarations, want both", len(got))
	}
	if got[0].Value.(schemaRes).Namespace != "first" || got[1].Value.(schemaRes).Namespace != "second" {
		t.Fatalf("Resources reordered a module's own declarations: %v", got)
	}
}

// TestResourcesConcreteTargetRequiresTypeIdentity pins the concrete half of
// the match rule. A []string is assignable to a named type over []string but
// not assertable to it, so selecting by assignability hands the generic
// wrapper a value its assertion cannot take.
func TestResourcesConcreteTargetRequiresTypeIdentity(t *testing.T) {
	reg := New()
	seed(t, reg, Module{Name: "underlying", Resources: []any{[]string{"a", "b"}}}, nil)

	if got := reg.Resources((*resNames)(nil)); len(got) != 0 {
		t.Fatalf("Resources collected %v for a target the values are not, want none", got)
	}
	if got := Resources[resNames](reg); len(got) != 0 {
		t.Fatalf("Resources[resNames] collected %v, want none", got)
	}
}

// TestResourcesConcreteTargetRejectsChannelDirection covers the second shape
// of the same gap: a bidirectional channel is assignable to a receive-only
// channel type, and again not assertable to it.
func TestResourcesConcreteTargetRejectsChannelDirection(t *testing.T) {
	reg := New()
	seed(t, reg, Module{Name: "signals", Resources: []any{make(chan int)}}, nil)

	if got := reg.Resources((*<-chan int)(nil)); len(got) != 0 {
		t.Fatalf("Resources collected %v for a direction the values do not have, want none", got)
	}
	if got := Resources[<-chan int](reg); len(got) != 0 {
		t.Fatalf("Resources[<-chan int] collected %v, want none", got)
	}
}

// TestResourcesInterfaceTargetKeepsAssignability pins that tightening the
// concrete half leaves the interface half alone: implementations of the target
// still match, whatever their own type, and a non-implementor still does not.
func TestResourcesInterfaceTargetKeepsAssignability(t *testing.T) {
	reg := New()
	seed(t, reg, Module{Name: "a", Resources: []any{schemaRes{Namespace: "cache"}}}, nil)
	seed(t, reg, Module{Name: "b", Resources: []any{specRes{Path: "/v1"}}}, nil)
	seed(t, reg, Module{Name: "c", Resources: []any{[]string{"not a namedRes"}}}, nil)

	got := Resources[namedRes](reg)
	if len(got) != 2 {
		t.Fatalf("Resources[namedRes] collected %v, want both implementations", got)
	}
	if got[0].Value.resourceName() != "schema:cache" || got[1].Value.resourceName() != "spec:/v1" {
		t.Fatalf("Resources[namedRes] returned %v, want the schema and the spec", got)
	}
}

func TestGenericResolveAllPanicsOnNonInterface(t *testing.T) {
	reg := New()
	assertPanics(t, "must point to an interface", func() {
		ResolveAll[int](reg)
	})
}

func TestGenericResourcesAllowsConcreteType(t *testing.T) {
	reg := New()
	seed(t, reg, Module{Name: "a", Resources: []any{schemaRes{Namespace: "cache"}}}, nil)

	got := Resources[schemaRes](reg)
	if len(got) != 1 || got[0].Value.Namespace != "cache" {
		t.Fatalf("Resources[schemaRes] returned %v, want the cache schema", got)
	}
}

func TestGenericResolveReturnsTypedInstance(t *testing.T) {
	reg := New()
	seed(t, reg, Module{Name: "a", Provides: provides((*cacheCap)(nil))}, &product{id: "a"})

	got, err := Resolve[cacheCap](reg)
	if err != nil {
		t.Fatalf("Resolve[cacheCap] failed: %v", err)
	}
	if got.cacheID() != "a" {
		t.Fatalf("Resolve[cacheCap] returned %v, want a's product", got)
	}
}

func TestEnablementMissingName(t *testing.T) {
	reg := New()
	reg.enablement = map[string]Enablement{"known": {State: StateEnabled}}

	if _, ok := reg.Enablement("unknown"); ok {
		t.Fatal("Enablement reported a name that was never registered")
	}
}

func TestEnablementReportsDisabledWithReason(t *testing.T) {
	reg := New()
	reg.enablement = map[string]Enablement{
		"off": {State: StateDisabled, Reason: "no connection address configured"},
	}

	got, ok := reg.Enablement("off")
	if !ok {
		t.Fatal("Enablement missed a module it holds a verdict for")
	}
	if got.State != StateDisabled || got.Reason != "no connection address configured" {
		t.Fatalf("Enablement returned %+v, want the disabled verdict with its reason", got)
	}
}

// TestEnablementBeforeResolutionReportsNotReady pins that the query promises
// the final verdict, and before resolution there is none to give.
func TestEnablementBeforeResolutionReportsNotReady(t *testing.T) {
	reg := New()
	reg.Register(Module{Name: "pending"})

	if _, ok := reg.Enablement("pending"); ok {
		t.Fatal("Enablement answered before resolution had run")
	}
}

// TestResolveMatchesCapabilityByTypeIdentity pins how one capability token
// matches another: by the identity of the element type, not by assignability.
// A module delivering superCacheCap delivers one capability, and a request for
// the capability it embeds does not see it -- matching by assignability would
// put that module in two groups at once, and exclusive resolution could then
// not name the providers of a capability at all.
func TestResolveMatchesCapabilityByTypeIdentity(t *testing.T) {
	reg := New()
	seed(t, reg, Module{Name: "super", Provides: provides((*superCacheCap)(nil))}, &product{id: "super"})

	if _, err := reg.Resolve((*cacheCap)(nil)); !errors.Is(err, ErrMissingProvider) {
		t.Fatalf("resolving the embedded capability returned %v, want ErrMissingProvider", err)
	}
	got, err := reg.Resolve((*superCacheCap)(nil))
	if err != nil {
		t.Fatalf("resolving the declared capability failed: %v", err)
	}
	if got.(*product).id != "super" {
		t.Fatalf("the declared capability resolved to %#v, want the module's own product", got)
	}
}
