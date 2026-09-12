package pkgcore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/vislake/speed/go/pkgcore/i18n"
	"github.com/vislake/speed/go/pkgcore/internal/componentfixtures/locales"
	"github.com/vislake/speed/go/pkgcore/internal/componentfixtures/localesmismatch"
	"github.com/vislake/speed/go/pkgcore/internal/componentfixtures/migrations"
)

// Assembly fixture contract types.
type (
	asmTokenA struct{}
	asmTokenB struct{}
	asmTokenC struct{}
	// asmSchema is the strict-decode target of the assembly fixtures that
	// take configuration.
	asmSchema struct {
		Host string `json:"host"`
		Port int    `json:"port"`
	}
)

// plainComponent returns a minimal component producing product.
func plainComponent(name string, product any) Component {
	return Component{
		Name: name,
		New:  func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return product, nil },
	}
}

// providerComponent returns a component that provides the given tokens.
func providerComponent(name string, product any, provides ...any) Component {
	c := plainComponent(name, product)
	c.Provides = provides
	return c
}

// prepareAssembly registers comps, puts a composition selecting entries in
// order, and runs Prepare.
func prepareAssembly(t *testing.T, comps []Component, entries ...configEntry) (*ComponentRegistry, error) {
	t.Helper()
	reg := newTestRegistry(t, comps...)
	reg.Put(testComposition(entries...))
	return reg, reg.Prepare(context.Background())
}

func TestPrepareRequiresCompositionConfiguration(t *testing.T) {
	reg := NewComponentRegistry()
	err := reg.Prepare(context.Background())
	if !errors.Is(err, ErrMissingRequirement) {
		t.Fatalf("Prepare without a composition configuration = %v, want ErrMissingRequirement", err)
	}
	if !strings.Contains(err.Error(), "composition configuration") || !strings.Contains(err.Error(), "stage prepare") {
		t.Errorf("error %q does not name the missing configuration and the stage", err)
	}
}

func TestPrepareExpandsSelection(t *testing.T) {
	a := plainComponent("asm.expand.a", &asmTokenA{})
	b := plainComponent("asm.expand.b", &asmTokenB{})
	off := plainComponent("asm.expand.off", &asmTokenC{})
	c := plainComponent("asm.expand.c", &compTokenA{})
	c.ConfigSchema = (*asmSchema)(nil)

	reg, err := prepareAssembly(t, []Component{a, b, off, c},
		configEntry{key: "asm.expand.c", value: map[string]any{"host": "h", "port": "8080"}},
		configEntry{key: "asm.expand.a", value: nil},
		configEntry{key: "asm.expand.off", value: false},
		configEntry{key: "asm.expand.b", value: ComponentConfig{}},
	)
	if err != nil {
		t.Fatalf("Prepare = %v, want nil", err)
	}
	assertPlanOrder(t, reg, []string{"asm.expand.c", "asm.expand.a", "asm.expand.b"})

	p, ok := reg.planned("asm.expand.c")
	if !ok {
		t.Fatal("asm.expand.c is not planned")
	}
	var decoded asmSchema
	if err := p.cfg.Decode(&decoded); err != nil {
		t.Fatalf("planned config of asm.expand.c does not decode: %v", err)
	}
	if decoded.Host != "h" || decoded.Port != 8080 {
		t.Errorf("planned config = %+v, want host=h port=8080", decoded)
	}
}

func TestPrepareRejectsSelectionValues(t *testing.T) {
	a := plainComponent("asm.value.a", &asmTokenA{})
	_, err := prepareAssembly(t, []Component{a}, configEntry{key: "asm.value.a", value: true})
	if err == nil || !strings.Contains(err.Error(), "false to deselect") {
		t.Fatalf("selection value true = %v, want a selection-value error", err)
	}
	_, err = prepareAssembly(t, []Component{a}, configEntry{key: "asm.value.a", value: "nope"})
	if err == nil || !strings.Contains(err.Error(), "got string") {
		t.Fatalf("selection value string = %v, want a selection-value error naming the type", err)
	}
}

func TestPrepareUnknownComponentNames(t *testing.T) {
	known := plainComponent("asm.known.authn", &asmTokenA{})

	t.Run("did you mean", func(t *testing.T) {
		_, err := prepareAssembly(t, []Component{known}, configEntry{key: "asm.known.authnn", value: nil})
		if !errors.Is(err, ErrUnknownComponent) {
			t.Fatalf("Prepare = %v, want ErrUnknownComponent", err)
		}
		for _, want := range []string{"stage prepare", `"asm.known.authnn"`, "is not registered", `did you mean "asm.known.authn"?`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	t.Run("no close match lists the registered names", func(t *testing.T) {
		_, err := prepareAssembly(t, []Component{known}, configEntry{key: "zzz.completely.different", value: nil})
		if !errors.Is(err, ErrUnknownComponent) {
			t.Fatalf("Prepare = %v, want ErrUnknownComponent", err)
		}
		if !strings.Contains(err.Error(), "registered components:") || !strings.Contains(err.Error(), "asm.known.authn") {
			t.Errorf("error %q does not list the registered components", err)
		}
	})
}

func TestPrepareValidatesConfigKeys(t *testing.T) {
	var planned asmSchema
	withSchema := plainComponent("asm.schema.holder", &planned)
	withSchema.ConfigSchema = (*asmSchema)(nil)

	_, err := prepareAssembly(t, []Component{withSchema},
		configEntry{key: "asm.schema.holder", value: map[string]any{"host": "h", "prot": 2525}},
	)
	if !errors.Is(err, ErrUnknownConfigKey) {
		t.Fatalf("Prepare = %v, want ErrUnknownConfigKey", err)
	}
	for _, want := range []string{"stage prepare", `"asm.schema.holder"`, `"prot"`, "accepted: host, port"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}

	noSchema := plainComponent("asm.schema.none", &asmTokenA{})
	_, err = prepareAssembly(t, []Component{noSchema},
		configEntry{key: "asm.schema.none", value: map[string]any{"anything": 1}},
	)
	if !errors.Is(err, ErrUnknownConfigKey) {
		t.Fatalf("Prepare = %v, want ErrUnknownConfigKey for a schema-less component with a block", err)
	}
	if !strings.Contains(err.Error(), "declares no configuration") {
		t.Errorf("error %q does not explain the component takes no configuration", err)
	}
}

func TestPrepareResolvesDependenciesAndOrdersThePlan(t *testing.T) {
	provider := providerComponent("asm.dep.p", &asmTokenA{}, (*asmTokenA)(nil))
	consumer := Component{
		Name:     "asm.dep.c",
		Requires: []Requirement{{Token: (*asmTokenA)(nil)}},
		New:      func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &asmTokenB{}, nil },
	}

	// Config order names the consumer first; the plan must order the
	// provider first.
	reg, err := prepareAssembly(t, []Component{provider, consumer},
		configEntry{key: "asm.dep.c", value: nil},
		configEntry{key: "asm.dep.p", value: nil},
	)
	if err != nil {
		t.Fatalf("Prepare = %v, want nil", err)
	}
	assertPlanOrder(t, reg, []string{"asm.dep.p", "asm.dep.c"})
}

func TestPrepareAutoPullsUniqueProviders(t *testing.T) {
	provider := providerComponent("asm.pull.p", &asmTokenA{}, (*asmTokenA)(nil))
	consumer := Component{
		Name:     "asm.pull.c",
		Requires: []Requirement{{Token: (*asmTokenA)(nil)}},
		New:      func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &asmTokenB{}, nil },
	}

	reg, err := prepareAssembly(t, []Component{provider, consumer}, configEntry{key: "asm.pull.c", value: nil})
	if err != nil {
		t.Fatalf("Prepare = %v, want nil", err)
	}
	assertPlanOrder(t, reg, []string{"asm.pull.p", "asm.pull.c"})
	p, ok := reg.planned("asm.pull.p")
	if !ok || !p.auto {
		t.Errorf("auto-pulled provider not marked auto (planned=%v auto=%v)", ok, p.auto)
	}

	// strict disables auto-pull.
	reg2 := newTestRegistry(t, provider, consumer)
	reg2.Put(testComposition(configEntry{key: "asm.pull.c", value: nil}).With("strict", true))
	err = reg2.Prepare(context.Background())
	if !errors.Is(err, ErrMissingRequirement) || !strings.Contains(err.Error(), "strict mode disables auto-pull") {
		t.Fatalf("strict Prepare = %v, want ErrMissingRequirement naming strict", err)
	}

	// A provider the composition explicitly deselected is not pulled back.
	reg3 := newTestRegistry(t, provider, consumer)
	reg3.Put(testComposition(
		configEntry{key: "asm.pull.c", value: nil},
		configEntry{key: "asm.pull.p", value: false},
	))
	err = reg3.Prepare(context.Background())
	if !errors.Is(err, ErrMissingRequirement) || !strings.Contains(err.Error(), "no registered component provides it") {
		t.Fatalf("Prepare with a deselected provider = %v, want ErrMissingRequirement", err)
	}
}

func TestPrepareAmbiguity(t *testing.T) {
	t.Run("two selected providers", func(t *testing.T) {
		p1 := providerComponent("asm.amb.p1", &asmTokenA{}, (*asmTokenA)(nil))
		p2 := providerComponent("asm.amb.p2", &asmTokenA{}, (*asmTokenA)(nil))
		consumer := Component{
			Name:     "asm.amb.c",
			Requires: []Requirement{{Token: (*asmTokenA)(nil)}},
			New:      func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &asmTokenB{}, nil },
		}

		_, err := prepareAssembly(t, []Component{p1, p2, consumer},
			configEntry{key: "asm.amb.p1", value: nil},
			configEntry{key: "asm.amb.p2", value: nil},
			configEntry{key: "asm.amb.c", value: nil},
		)
		if !errors.Is(err, ErrAmbiguousProvider) {
			t.Fatalf("Prepare = %v, want ErrAmbiguousProvider", err)
		}
		for _, want := range []string{"stage prepare", "provided by multiple selected components", "asm.amb.p1", "asm.amb.p2", "deselect all but one"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	t.Run("multiple auto-pull candidates", func(t *testing.T) {
		p1 := providerComponent("asm.amb.q1", &asmTokenA{}, (*asmTokenA)(nil))
		p2 := providerComponent("asm.amb.q2", &asmTokenA{}, (*asmTokenA)(nil))
		consumer := Component{
			Name:     "asm.amb.q",
			Requires: []Requirement{{Token: (*asmTokenA)(nil)}},
			New:      func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &asmTokenB{}, nil },
		}

		_, err := prepareAssembly(t, []Component{p1, p2, consumer}, configEntry{key: "asm.amb.q", value: nil})
		if !errors.Is(err, ErrAmbiguousProvider) {
			t.Fatalf("Prepare = %v, want ErrAmbiguousProvider", err)
		}
		for _, want := range []string{"could provide it", "asm.amb.q1", "asm.amb.q2", "select exactly one"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})
}

// TestPrepareRejectsDuplicateDeclaredDeliveries pins the whole-selection
// form of the single-value ambiguity rule: two selected components whose
// declared deliveries address one another are refused at plan time even
// when no requirement anchors the token -- the shape whose later by-type
// readings (Get, GetOptional, the nil-for-absent sugars) would all fail or
// silently answer absent while two deliveries sit in the context.
func TestPrepareRejectsDuplicateDeclaredDeliveries(t *testing.T) {
	t.Run("no requirement anchors the token", func(t *testing.T) {
		p1 := providerComponent("asm.dup.p1", &asmTokenA{}, (*asmTokenA)(nil))
		p2 := providerComponent("asm.dup.p2", &asmTokenA{}, (*asmTokenA)(nil))

		_, err := prepareAssembly(t, []Component{p1, p2},
			configEntry{key: "asm.dup.p1", value: nil},
			configEntry{key: "asm.dup.p2", value: nil},
		)
		if !errors.Is(err, ErrAmbiguousProvider) {
			t.Fatalf("Prepare = %v, want ErrAmbiguousProvider", err)
		}
		for _, want := range []string{"stage prepare", "asmTokenA", "asm.dup.p1", "asm.dup.p2", "deselect all but one"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	t.Run("disjoint declared tokens stay legal", func(t *testing.T) {
		p1 := providerComponent("asm.dup.q1", &asmTokenA{}, (*asmTokenA)(nil))
		p2 := providerComponent("asm.dup.q2", &asmTokenB{}, (*asmTokenB)(nil))
		if _, err := prepareAssembly(t, []Component{p1, p2},
			configEntry{key: "asm.dup.q1", value: nil},
			configEntry{key: "asm.dup.q2", value: nil},
		); err != nil {
			t.Fatalf("Prepare = %v, want nil for two components delivering distinct tokens", err)
		}
	})

	t.Run("an undeclared duplicate stays outside the plan check", func(t *testing.T) {
		// Both components put the same product type but declare nothing:
		// the plan reads no delivery for it, so the selection stays legal --
		// the boundary of a declarations-driven check.
		p1 := plainComponent("asm.dup.u1", &asmTokenA{})
		p2 := plainComponent("asm.dup.u2", &asmTokenA{})
		if _, err := prepareAssembly(t, []Component{p1, p2},
			configEntry{key: "asm.dup.u1", value: nil},
			configEntry{key: "asm.dup.u2", value: nil},
		); err != nil {
			t.Fatalf("Prepare = %v, want nil for undeclared deliveries", err)
		}
	})
}

// memberComponent returns a component contributing catalog members of the
// given tokens; sink, when non-nil, records the component's construction
// order.
func memberComponent(name string, product any, sink *[]string, members ...any) Component {
	return Component{
		Name:           name,
		ProvidesMember: members,
		New: func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) {
			if sink != nil {
				*sink = append(*sink, name)
			}
			return product, nil
		},
	}
}

// catalogConsumer returns a component consuming token as a catalog with the
// given lower bound; sink, when non-nil, records its construction order.
func catalogConsumer(name string, token any, min int, sink *[]string) Component {
	return Component{
		Name:     name,
		Requires: []Requirement{{Token: token, Catalog: true, MinMembers: min}},
		New: func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) {
			if sink != nil {
				*sink = append(*sink, name)
			}
			return &asmTokenC{}, nil
		},
	}
}

// TestPrepareCatalogRequirements pins the catalog delivery contract at plan
// time: the dependency edge reaches every selected member (so members order
// before the consumer even when the composition lists the consumer first),
// MinMembers bounds the member count, an empty catalog is legal at the zero
// bound, and a catalog never auto-pulls even when exactly one registered
// candidate exists.
func TestPrepareCatalogRequirements(t *testing.T) {
	t.Run("members order before the consumer and leave the by-type context alone", func(t *testing.T) {
		var constructed []string
		v1, v2 := &asmTokenA{}, &asmTokenA{}
		m1 := memberComponent("asm.cat.m1", v1, &constructed, (*asmTokenA)(nil))
		m2 := memberComponent("asm.cat.m2", v2, &constructed, (*asmTokenA)(nil))
		consumer := catalogConsumer("asm.cat.c", (*asmTokenA)(nil), 0, &constructed)

		reg, err := prepareAssembly(t, []Component{m1, m2, consumer},
			configEntry{key: "asm.cat.c", value: nil},
			configEntry{key: "asm.cat.m1", value: nil},
			configEntry{key: "asm.cat.m2", value: nil},
		)
		if err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
		assertPlanOrder(t, reg, []string{"asm.cat.m1", "asm.cat.m2", "asm.cat.c"})

		if err := reg.Construct(context.Background()); err != nil {
			t.Fatalf("Construct = %v, want nil", err)
		}
		t.Cleanup(func() { _ = reg.Close(context.Background()) })
		if want := []string{"asm.cat.m1", "asm.cat.m2", "asm.cat.c"}; !reflect.DeepEqual(constructed, want) {
			t.Errorf("construction order = %v, want %v", constructed, want)
		}

		members := Members[*asmTokenA](reg)
		if len(members) != 2 || members[0].Name != "asm.cat.m1" || members[0].Value != v1 || members[1].Name != "asm.cat.m2" || members[1].Value != v2 {
			t.Errorf("Members[*asmTokenA] = %+v, want m1 and m2 in dependency order with their own products", members)
		}

		if _, err := Get[*asmTokenA](reg); !errors.Is(err, ErrMissingRequirement) {
			t.Errorf("Get[*asmTokenA] = %v, want ErrMissingRequirement: a member product is not put", err)
		}
		if _, ok, err := GetOptional[*asmTokenA](reg); ok || err != nil {
			t.Errorf("GetOptional[*asmTokenA] = (ok=%v, %v), want absent", ok, err)
		}
	})

	t.Run("a member product is read under the interface its token names", func(t *testing.T) {
		m1 := memberComponent("asm.cat.i1", compSpreadImpl{}, nil, (*compSpreader)(nil))
		consumer := catalogConsumer("asm.cat.ic", (*compSpreader)(nil), 1, nil)

		reg, err := prepareAssembly(t, []Component{m1, consumer},
			configEntry{key: "asm.cat.ic", value: nil},
			configEntry{key: "asm.cat.i1", value: nil},
		)
		if err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
		if err := reg.Construct(context.Background()); err != nil {
			t.Fatalf("Construct = %v, want nil", err)
		}
		t.Cleanup(func() { _ = reg.Close(context.Background()) })

		members := Members[compSpreader](reg)
		if len(members) != 1 || members[0].Name != "asm.cat.i1" || members[0].Value.spread() != "spread" {
			t.Errorf("Members[compSpreader] = %+v, want the implementing member", members)
		}
	})

	t.Run("a shortfall fails naming the token, the count and the bound", func(t *testing.T) {
		m1 := memberComponent("asm.cat.s1", &asmTokenA{}, nil, (*asmTokenA)(nil))
		consumer := catalogConsumer("asm.cat.sc", (*asmTokenA)(nil), 2, nil)

		_, err := prepareAssembly(t, []Component{m1, consumer},
			configEntry{key: "asm.cat.sc", value: nil},
			configEntry{key: "asm.cat.s1", value: nil},
		)
		if !errors.Is(err, ErrMissingRequirement) {
			t.Fatalf("Prepare = %v, want ErrMissingRequirement", err)
		}
		for _, want := range []string{"stage prepare", "asmTokenA", "1 selected member(s)", "minimum of 2"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	t.Run("an empty catalog is legal at the zero bound", func(t *testing.T) {
		consumer := catalogConsumer("asm.cat.ec", (*asmTokenA)(nil), 0, nil)
		reg, err := prepareAssembly(t, []Component{consumer}, configEntry{key: "asm.cat.ec", value: nil})
		if err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
		assertPlanOrder(t, reg, []string{"asm.cat.ec"})
		if err := reg.Construct(context.Background()); err != nil {
			t.Fatalf("Construct = %v, want nil", err)
		}
		t.Cleanup(func() { _ = reg.Close(context.Background()) })
		if members := Members[*asmTokenA](reg); len(members) != 0 {
			t.Errorf("Members[*asmTokenA] = %+v, want an empty catalog", members)
		}
	})

	t.Run("a catalog never auto-pulls its unique registered candidate", func(t *testing.T) {
		candidate := memberComponent("asm.cat.p1", &asmTokenA{}, nil, (*asmTokenA)(nil))
		consumer := catalogConsumer("asm.cat.pc", (*asmTokenA)(nil), 0, nil)

		reg, err := prepareAssembly(t, []Component{candidate, consumer}, configEntry{key: "asm.cat.pc", value: nil})
		if err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
		assertPlanOrder(t, reg, []string{"asm.cat.pc"})
		if p, ok := reg.planned("asm.cat.p1"); ok {
			t.Errorf("the catalog's registered candidate joined the plan (auto=%v); a member joins only by explicit selection", p.auto)
		}
	})

	t.Run("an optional catalog accepts the zero bound", func(t *testing.T) {
		consumer := Component{
			Name:     "asm.cat.oc",
			Requires: []Requirement{{Token: (*asmTokenA)(nil), Catalog: true, Optional: true}},
			New:      func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &asmTokenC{}, nil },
		}
		if _, err := prepareAssembly(t, []Component{consumer}, configEntry{key: "asm.cat.oc", value: nil}); err != nil {
			t.Fatalf("Prepare = %v, want nil for an optional catalog", err)
		}
	})
}

// TestPrepareRejectsConflictingDeliveryKinds pins the cross-component rule:
// one token cannot be a bound delivery and a catalog membership at once, so
// a selection holding both is refused at plan time naming both components.
func TestPrepareRejectsConflictingDeliveryKinds(t *testing.T) {
	bound := providerComponent("asm.kind.bound", &asmTokenA{}, (*asmTokenA)(nil))
	member := memberComponent("asm.kind.member", &asmTokenA{}, nil, (*asmTokenA)(nil))

	_, err := prepareAssembly(t, []Component{bound, member},
		configEntry{key: "asm.kind.bound", value: nil},
		configEntry{key: "asm.kind.member", value: nil},
	)
	if !errors.Is(err, ErrInvalidComponent) {
		t.Fatalf("Prepare = %v, want ErrInvalidComponent", err)
	}
	for _, want := range []string{"stage prepare", "asmTokenA", "bound delivery", "catalog member", `"asm.kind.bound"`, `"asm.kind.member"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}

	// Two members of the same token are legal; only the mixed pair is not.
	other := memberComponent("asm.kind.member2", &asmTokenA{}, nil, (*asmTokenA)(nil))
	if _, err := prepareAssembly(t, []Component{member, other},
		configEntry{key: "asm.kind.member", value: nil},
		configEntry{key: "asm.kind.member2", value: nil},
	); err != nil {
		t.Fatalf("Prepare = %v, want nil for two members of one catalog", err)
	}
}

func TestPrepareOptionalRequirements(t *testing.T) {
	optionalConsumer := func(name string) Component {
		return Component{
			Name:     name,
			Requires: []Requirement{{Token: (*asmTokenA)(nil), Optional: true}},
			New:      func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &asmTokenC{}, nil },
		}
	}

	t.Run("missing optional dependency is fine and not pulled", func(t *testing.T) {
		provider := providerComponent("asm.opt.p", &asmTokenA{}, (*asmTokenA)(nil))
		reg, err := prepareAssembly(t, []Component{provider, optionalConsumer("asm.opt.c")},
			configEntry{key: "asm.opt.c", value: nil},
		)
		if err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
		assertPlanOrder(t, reg, []string{"asm.opt.c"})
	})

	t.Run("an optional dependency on a selected provider still orders", func(t *testing.T) {
		provider := providerComponent("asm.opt.p2", &asmTokenA{}, (*asmTokenA)(nil))
		reg, err := prepareAssembly(t, []Component{provider, optionalConsumer("asm.opt.c2")},
			configEntry{key: "asm.opt.c2", value: nil},
			configEntry{key: "asm.opt.p2", value: nil},
		)
		if err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
		assertPlanOrder(t, reg, []string{"asm.opt.p2", "asm.opt.c2"})
	})
}

func TestPrepareDependencyCycle(t *testing.T) {
	a := Component{
		Name:     "asm.cycle.a",
		Requires: []Requirement{{Token: (*asmTokenB)(nil)}},
		Provides: []any{(*asmTokenA)(nil)},
		New:      func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &asmTokenA{}, nil },
	}
	b := Component{
		Name:     "asm.cycle.b",
		Requires: []Requirement{{Token: (*asmTokenA)(nil)}},
		Provides: []any{(*asmTokenB)(nil)},
		New:      func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &asmTokenB{}, nil },
	}

	_, err := prepareAssembly(t, []Component{a, b},
		configEntry{key: "asm.cycle.a", value: nil},
		configEntry{key: "asm.cycle.b", value: nil},
	)
	if !errors.Is(err, ErrDependencyCycle) {
		t.Fatalf("Prepare = %v, want ErrDependencyCycle", err)
	}
	for _, want := range []string{"stage prepare", "asm.cycle.a -> asm.cycle.b -> asm.cycle.a", "break the cycle"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
}

func TestPrepareCapabilityValidation(t *testing.T) {
	plain := plainComponent("asm.cap.plain", &asmTokenA{})
	strong := plainComponent("asm.cap.strong", &asmTokenB{})
	strong.Capabilities = MultiReplicaSafe

	t.Run("missing capability fails the assembly", func(t *testing.T) {
		reg := newTestRegistry(t, plain)
		reg.Put(testComposition(configEntry{key: "asm.cap.plain", value: nil}).With("deployment", "distributed"))
		err := reg.Prepare(context.Background())
		if !errors.Is(err, ErrCapabilityUnsatisfied) {
			t.Fatalf("Prepare = %v, want ErrCapabilityUnsatisfied", err)
		}
		for _, want := range []string{"stage prepare", `"asm.cap.plain"`, "MultiReplicaSafe", `"distributed"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	t.Run("declared capability passes", func(t *testing.T) {
		reg := newTestRegistry(t, strong)
		reg.Put(testComposition(configEntry{key: "asm.cap.strong", value: nil}).With("deployment", "distributed"))
		if err := reg.Prepare(context.Background()); err != nil {
			t.Fatalf("Prepare = %v, want nil (declared capabilities satisfy distributed)", err)
		}
	})

	t.Run("standalone requires nothing", func(t *testing.T) {
		if _, err := prepareAssembly(t, []Component{plain}, configEntry{key: "asm.cap.plain", value: nil}); err != nil {
			t.Fatalf("standalone Prepare = %v, want nil", err)
		}
	})
}

func TestValidateMigrations(t *testing.T) {
	cases := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{
			name: "well formed",
			fsys: fstest.MapFS{
				"postgres/0001_a.sql": &fstest.MapFile{Data: []byte("select 1;")},
				"sqlite/0001_a.sql":   &fstest.MapFile{Data: []byte("select 1;")},
			},
		},
		{
			name: "unknown dialect directory",
			fsys: fstest.MapFS{"widgets/0001_a.sql": &fstest.MapFile{Data: []byte("select 1;")}},
			want: `dialect directory "widgets" is not supported`,
		},
		{
			name: "unnumbered file",
			fsys: fstest.MapFS{"postgres/init.sql": &fstest.MapFile{Data: []byte("select 1;")}},
			want: "want a numbered .sql file",
		},
		{
			name: "non-sql file",
			fsys: fstest.MapFS{"postgres/notes.md": &fstest.MapFile{Data: []byte("hi")}},
			want: "want a numbered .sql file",
		},
		{
			name: "file at the root",
			fsys: fstest.MapFS{"README.md": &fstest.MapFile{Data: []byte("hi")}},
			want: "is not a dialect directory",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMigrations(tc.fsys)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("validateMigrations = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("validateMigrations = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

func TestPrepareAssetValidation(t *testing.T) {
	t.Run("bad migration shape through a component", func(t *testing.T) {
		c := plainComponent("asm.asset.badmig", &asmTokenA{})
		c.Module = "asm.asset.badmig"
		c.Migrations = locales.FS // a locale pair: files at the FS root
		_, err := prepareAssembly(t, []Component{c}, configEntry{key: "asm.asset.badmig", value: nil})
		if !errors.Is(err, ErrInvalidAsset) {
			t.Fatalf("Prepare = %v, want ErrInvalidAsset", err)
		}
		for _, want := range []string{"stage prepare", `"asm.asset.badmig"`, "migration set", "is not a dialect directory"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	t.Run("openapi fragments", func(t *testing.T) {
		badUTF8 := plainComponent("asm.asset.utf8", &asmTokenA{})
		badUTF8.OpenAPISpec = []byte{0xff, 0xfe, 0xfd}
		_, err := prepareAssembly(t, []Component{badUTF8}, configEntry{key: "asm.asset.utf8", value: nil})
		if !errors.Is(err, ErrInvalidAsset) || !strings.Contains(err.Error(), "not valid UTF-8") {
			t.Fatalf("Prepare with a binary fragment = %v, want ErrInvalidAsset", err)
		}

		notOpenAPI := plainComponent("asm.asset.notopenapi", &asmTokenA{})
		notOpenAPI.OpenAPISpec = []byte("this is not an OpenAPI document\n")
		_, err = prepareAssembly(t, []Component{notOpenAPI}, configEntry{key: "asm.asset.notopenapi", value: nil})
		if !errors.Is(err, ErrInvalidAsset) || !strings.Contains(err.Error(), "no top-level openapi:") {
			t.Fatalf("Prepare with a non-OpenAPI fragment = %v, want ErrInvalidAsset", err)
		}

		jsonFragment := plainComponent("asm.asset.json", &asmTokenA{})
		jsonFragment.OpenAPISpec = []byte(`{"openapi":"3.0.3"}`)
		if _, err := prepareAssembly(t, []Component{jsonFragment}, configEntry{key: "asm.asset.json", value: nil}); err != nil {
			t.Fatalf("Prepare with a JSON fragment = %v, want nil", err)
		}
	})

	t.Run("locale parity mismatch", func(t *testing.T) {
		c := plainComponent("locmismatch", &asmTokenA{})
		c.Locales = localesmismatch.FS
		_, err := prepareAssembly(t, []Component{c}, configEntry{key: "locmismatch", value: nil})
		if !errors.Is(err, ErrInvalidAsset) {
			t.Fatalf("Prepare = %v, want ErrInvalidAsset", err)
		}
		if !errors.Is(err, i18n.ErrParityMismatch) {
			t.Errorf("error %q does not wrap the parity mismatch", err)
		}
		if !strings.Contains(err.Error(), "locale resources") {
			t.Errorf("error %q does not name the locale resources", err)
		}
	})

	t.Run("well formed assets pass", func(t *testing.T) {
		// The locale fixture's ids carry the "locgood." prefix, and the
		// catalog's message-id contract requires a locale-shipping
		// component's name to be the prefix, so the carrier is named
		// "locgood" -- a dot-free name, which the catalog's current
		// no-dot rule for message-shipping names requires until the
		// catalog itself grows component-name support.
		c := plainComponent("locgood", &asmTokenA{})
		c.Module = "locgood"
		c.Migrations = migrations.FS
		c.Locales = locales.FS
		c.OpenAPISpec = []byte("openapi: 3.0.3\ninfo:\n  title: fixture\n")
		if _, err := prepareAssembly(t, []Component{c}, configEntry{key: "locgood", value: nil}); err != nil {
			t.Fatalf("Prepare with well-formed assets = %v, want nil", err)
		}
	})

	t.Run("migration carrier without a module", func(t *testing.T) {
		c := plainComponent("asm.asset.nomodule", &asmTokenA{})
		c.Migrations = migrations.FS
		_, err := prepareAssembly(t, []Component{c}, configEntry{key: "asm.asset.nomodule", value: nil})
		if !errors.Is(err, ErrInvalidAsset) {
			t.Fatalf("Prepare = %v, want ErrInvalidAsset", err)
		}
		for _, want := range []string{"stage prepare", `"asm.asset.nomodule"`, "carries a migration set but declares no module"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	t.Run("one ledger key admits one migration set", func(t *testing.T) {
		// Two members of one module both carrying a set: the ledger keys
		// both by "signer", so the second could never record or apply.
		local := plainComponent("signer.local", &asmTokenA{})
		local.Module = "signer"
		local.Migrations = migrations.FS
		vault := plainComponent("signer.vault", &asmTokenB{})
		vault.Module = "signer"
		vault.Migrations = migrations.FS
		_, err := prepareAssembly(t, []Component{local, vault},
			configEntry{key: "signer.local", value: nil},
			configEntry{key: "signer.vault", value: nil},
		)
		if !errors.Is(err, ErrInvalidAsset) {
			t.Fatalf("Prepare = %v, want ErrInvalidAsset", err)
		}
		for _, want := range []string{"stage prepare", `"signer.local"`, `"signer.vault"`, `"signer"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}

		// One carrier and one migration-free member of the same module is
		// the directory-style shape the ledger does admit.
		carrier := plainComponent("signer.local", &asmTokenA{})
		carrier.Module = "signer"
		carrier.Migrations = migrations.FS
		member := plainComponent("signer.vault", &asmTokenB{})
		member.Module = "signer"
		if _, err := prepareAssembly(t, []Component{carrier, member},
			configEntry{key: "signer.local", value: nil},
			configEntry{key: "signer.vault", value: nil},
		); err != nil {
			t.Fatalf("Prepare with one carrier and one migration-free member = %v, want nil", err)
		}
	})

	t.Run("a renamed copy validates under its module prefix", func(t *testing.T) {
		// The carrier is a host-named copy of a descriptor implementing
		// module "locgood", carrying the module's own resources: the id
		// prefix is the MODULE's, so the copy validates exactly as the
		// module's own descriptor would -- the shape a host override uses.
		c := plainComponent("host.locgood.copy", &asmTokenA{})
		c.Module = "locgood"
		c.Locales = locales.FS
		if _, err := prepareAssembly(t, []Component{c}, configEntry{key: "host.locgood.copy", value: nil}); err != nil {
			t.Fatalf("Prepare with a renamed copy carrying its module's locales = %v, want nil", err)
		}
	})
}

func TestPreparePlanDeterminism(t *testing.T) {
	build := func() []Component {
		return []Component{
			plainComponent("asm.det.c", &asmTokenA{}),
			plainComponent("asm.det.a", &asmTokenB{}),
			plainComponent("asm.det.b", &asmTokenC{}),
		}
	}
	composition := func() ComponentConfig {
		return testComposition(
			configEntry{key: "asm.det.c", value: nil},
			configEntry{key: "asm.det.a", value: nil},
			configEntry{key: "asm.det.b", value: nil},
		)
	}

	var first []string
	for i := 0; i < 5; i++ {
		reg := newTestRegistry(t, build()...)
		reg.Put(composition())
		if err := reg.Prepare(context.Background()); err != nil {
			t.Fatalf("Prepare = %v", err)
		}
		var order []string
		for _, p := range reg.plannedComponents() {
			order = append(order, p.component.Name)
		}
		if i == 0 {
			first = order
			if want := []string{"asm.det.c", "asm.det.a", "asm.det.b"}; !reflect.DeepEqual(order, want) {
				t.Errorf("plan order = %v, want config order %v", order, want)
			}
			continue
		}
		if !reflect.DeepEqual(order, first) {
			t.Fatalf("plan order run %d = %v, want the same as run 0 (%v)", i, order, first)
		}
	}
}

func TestErrorCatalogFourElements(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name      string
		sentinel  error
		stage     string
		component string
		cause     string
		remedy    string
		run       func(t *testing.T) error
	}{
		{
			name:      "ErrUnknownComponent",
			sentinel:  ErrUnknownComponent,
			stage:     "stage prepare",
			component: "asm.catalog.authnn",
			cause:     "is not registered",
			remedy:    "did you mean",
			run: func(t *testing.T) error {
				_, err := prepareAssembly(t, []Component{plainComponent("asm.catalog.authnx", &asmTokenA{})},
					configEntry{key: "asm.catalog.authnn", value: nil})
				return err
			},
		},
		{
			name:      "ErrMissingRequirement",
			sentinel:  ErrMissingRequirement,
			stage:     "stage prepare",
			component: `"asm.catalog.consumer"`,
			cause:     "requires pkgcore.asmTokenA",
			remedy:    "no registered component provides it",
			run: func(t *testing.T) error {
				consumer := Component{
					Name:     "asm.catalog.consumer",
					Requires: []Requirement{{Token: (*asmTokenA)(nil)}},
					New:      func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &asmTokenB{}, nil },
				}
				_, err := prepareAssembly(t, []Component{consumer}, configEntry{key: "asm.catalog.consumer", value: nil})
				return err
			},
		},
		{
			name:      "ErrAmbiguousProvider",
			sentinel:  ErrAmbiguousProvider,
			stage:     "stage prepare",
			component: "asm.catalog.mailer1",
			cause:     "provided by multiple selected components",
			remedy:    "deselect all but one",
			run: func(t *testing.T) error {
				m1 := providerComponent("asm.catalog.mailer1", &asmTokenA{}, (*asmTokenA)(nil))
				m2 := providerComponent("asm.catalog.mailer2", &asmTokenA{}, (*asmTokenA)(nil))
				consumer := Component{
					Name:     "asm.catalog.sender",
					Requires: []Requirement{{Token: (*asmTokenA)(nil)}},
					New:      func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &asmTokenB{}, nil },
				}
				_, err := prepareAssembly(t, []Component{m1, m2, consumer},
					configEntry{key: "asm.catalog.mailer1", value: nil},
					configEntry{key: "asm.catalog.mailer2", value: nil},
					configEntry{key: "asm.catalog.sender", value: nil},
				)
				return err
			},
		},
		{
			name:      "ErrUnknownConfigKey",
			sentinel:  ErrUnknownConfigKey,
			stage:     "stage prepare",
			component: `"asm.catalog.keyholder"`,
			cause:     `config key "prot" is not declared`,
			remedy:    "accepted: host, port",
			run: func(t *testing.T) error {
				c := plainComponent("asm.catalog.keyholder", &asmTokenA{})
				c.ConfigSchema = (*asmSchema)(nil)
				_, err := prepareAssembly(t, []Component{c},
					configEntry{key: "asm.catalog.keyholder", value: map[string]any{"prot": 1}})
				return err
			},
		},
		{
			name:      "ErrComponentFailed",
			sentinel:  ErrComponentFailed,
			stage:     "stage verify",
			component: `"asm.catalog.broken"`,
			cause:     "verify failed",
			remedy:    "rolled back: asm.catalog.broken",
			run: func(t *testing.T) error {
				c := plainComponent("asm.catalog.broken", &asmTokenA{})
				c.Verify = func(context.Context, *ComponentRegistry, any) error { return errors.New("verify failed") }
				c.Close = func(context.Context, *ComponentRegistry, any) error { return nil }
				reg, err := prepareAssembly(t, []Component{c}, configEntry{key: "asm.catalog.broken", value: nil})
				if err != nil {
					return err
				}
				if err := reg.Construct(ctx); err != nil {
					return err
				}
				return reg.Verify(ctx)
			},
		},
		{
			name:      "ErrDuplicateComponent",
			sentinel:  ErrDuplicateComponent,
			stage:     "stage registration",
			component: `"asm.catalog.dup"`,
			cause:     "already registered",
			remedy:    "unique",
			run: func(t *testing.T) error {
				reg := newTestRegistry(t, plainComponent("asm.catalog.dup", &asmTokenA{}))
				return reg.Register(plainComponent("asm.catalog.dup", &asmTokenB{}))
			},
		},
		{
			name:      "ErrDependencyCycle",
			sentinel:  ErrDependencyCycle,
			stage:     "stage prepare",
			component: "asm.catalog.cyc.a",
			cause:     "->",
			remedy:    "break the cycle",
			run: func(t *testing.T) error {
				a := Component{
					Name:     "asm.catalog.cyc.a",
					Requires: []Requirement{{Token: (*asmTokenB)(nil)}},
					Provides: []any{(*asmTokenA)(nil)},
					New:      func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &asmTokenA{}, nil },
				}
				b := Component{
					Name:     "asm.catalog.cyc.b",
					Requires: []Requirement{{Token: (*asmTokenA)(nil)}},
					Provides: []any{(*asmTokenB)(nil)},
					New:      func(context.Context, *ComponentRegistry, ComponentConfig) (any, error) { return &asmTokenB{}, nil },
				}
				_, err := prepareAssembly(t, []Component{a, b},
					configEntry{key: "asm.catalog.cyc.a", value: nil},
					configEntry{key: "asm.catalog.cyc.b", value: nil},
				)
				return err
			},
		},
		{
			name:      "ErrCapabilityUnsatisfied",
			sentinel:  ErrCapabilityUnsatisfied,
			stage:     "stage prepare",
			component: `"asm.catalog.weak"`,
			cause:     "MultiReplicaSafe",
			remedy:    `deployment mode "distributed"`,
			run: func(t *testing.T) error {
				weak := plainComponent("asm.catalog.weak", &asmTokenA{})
				reg := newTestRegistry(t, weak)
				reg.Put(testComposition(configEntry{key: "asm.catalog.weak", value: nil}).With("deployment", "distributed"))
				return reg.Prepare(ctx)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(t)
			if err == nil {
				t.Fatal("scenario produced no error")
			}
			if !errors.Is(err, tc.sentinel) {
				t.Fatalf("error %v does not match sentinel %v", err, tc.sentinel)
			}
			for element, want := range map[string]string{
				"stage":     tc.stage,
				"component": tc.component,
				"cause":     tc.cause,
				"remedy":    tc.remedy,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s element missing: error %q does not carry %q", element, err, want)
				}
			}
		})
	}
}

// resolverCall records one ComponentConfigResolver invocation.
type resolverCall struct {
	componentName string
	schema        any
	fileConfig    ComponentConfig
}

// fakeResolver is the ComponentConfigResolver test double: it records every
// call and answers through fn.
type fakeResolver struct {
	calls []resolverCall
	fn    func(componentName string, schema any, fileConfig ComponentConfig) (ComponentConfig, error)
}

func (f *fakeResolver) ResolveComponentConfig(componentName string, schema any, fileConfig ComponentConfig) (ComponentConfig, error) {
	f.calls = append(f.calls, resolverCall{componentName: componentName, schema: schema, fileConfig: fileConfig})
	return f.fn(componentName, schema, fileConfig)
}

// schemaComponent returns a component with the given schema and product.
func schemaComponent(name string, schema any, product any) Component {
	c := plainComponent(name, product)
	c.ConfigSchema = schema
	return c
}

func TestPrepareResolvesComponentConfigurations(t *testing.T) {
	ctx := context.Background()
	a := schemaComponent("asm.resolve.a", (*asmSchema)(nil), &asmTokenA{})
	b := schemaComponent("asm.resolve.b", (*asmSchema)(nil), &asmTokenB{})
	noSchema := plainComponent("asm.resolve.c", &compTokenA{})
	off := schemaComponent("asm.resolve.off", (*asmSchema)(nil), &compTokenB{})

	resolver := &fakeResolver{fn: func(_ string, _ any, file ComponentConfig) (ComponentConfig, error) {
		return file.With("port", "7070"), nil
	}}
	reg := newTestRegistry(t, a, b, noSchema, off)
	reg.Put(resolver)
	reg.Put(testComposition(
		configEntry{key: "asm.resolve.a", value: map[string]any{"host": "h"}},
		configEntry{key: "asm.resolve.b", value: nil},
		configEntry{key: "asm.resolve.c", value: nil},
		configEntry{key: "asm.resolve.off", value: false},
	))
	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare = %v, want nil", err)
	}

	if len(resolver.calls) != 2 {
		t.Fatalf("resolver called %d times (%v), want one call per selected component with a schema", len(resolver.calls), resolver.calls)
	}
	if resolver.calls[0].componentName != "asm.resolve.a" || resolver.calls[1].componentName != "asm.resolve.b" {
		t.Errorf("resolver calls = %q, %q, want the selection order", resolver.calls[0].componentName, resolver.calls[1].componentName)
	}
	if resolver.calls[0].schema != a.ConfigSchema {
		t.Errorf("resolver schema = %v, want the component's own ConfigSchema", resolver.calls[0].schema)
	}
	if host, ok := resolver.calls[0].fileConfig.Get("host"); !ok || host != "h" {
		t.Errorf("resolver file block = %v, want the composition's block", resolver.calls[0].fileConfig.Keys())
	}

	p, ok := reg.planned("asm.resolve.a")
	if !ok {
		t.Fatal("asm.resolve.a is not planned")
	}
	var decoded asmSchema
	if err := p.cfg.Decode(&decoded); err != nil {
		t.Fatalf("planned config does not decode: %v", err)
	}
	if decoded.Host != "h" || decoded.Port != 7070 {
		t.Errorf("planned config = %+v, want host=h port=7070 (the merged block)", decoded)
	}
}

func TestPrepareResolvesAutoPulledComponentConfiguration(t *testing.T) {
	ctx := context.Background()
	consumer := plainComponent("asm.resolve.consumer", &asmTokenA{})
	consumer.Requires = []Requirement{{Token: (*asmTokenB)(nil)}}
	provider := schemaComponent("asm.resolve.provider", (*asmSchema)(nil), &asmTokenB{})
	provider.Provides = []any{(*asmTokenB)(nil)}

	resolver := &fakeResolver{fn: func(_ string, _ any, file ComponentConfig) (ComponentConfig, error) {
		return file, nil
	}}
	reg := newTestRegistry(t, consumer, provider)
	reg.Put(resolver)
	reg.Put(testComposition(configEntry{key: "asm.resolve.consumer", value: nil}))
	if err := reg.Prepare(ctx); err != nil {
		t.Fatalf("Prepare = %v, want nil", err)
	}

	// The consumer declares no schema, so its (empty) block is not consulted;
	// the provider is auto-pulled into the selection and resolved like any
	// other selected component.
	if len(resolver.calls) != 1 || resolver.calls[0].componentName != "asm.resolve.provider" {
		t.Fatalf("resolver calls = %v, want exactly the auto-pulled provider resolved", resolver.calls)
	}
}

func TestPrepareWithoutResolverKeepsFileBlock(t *testing.T) {
	c := schemaComponent("asm.naive.a", (*asmSchema)(nil), &asmTokenA{})
	reg, err := prepareAssembly(t, []Component{c}, configEntry{key: "asm.naive.a", value: map[string]any{"host": "h"}})
	if err != nil {
		t.Fatalf("Prepare = %v, want nil", err)
	}

	p, ok := reg.planned("asm.naive.a")
	if !ok {
		t.Fatal("asm.naive.a is not planned")
	}
	if _, carried := p.cfg.Get("port"); carried {
		t.Error("the planned config carries a port value, want the file block alone with no resolver in the registry")
	}
}

func TestPrepareResolverErrorFailsAssembly(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("the flag parse failed")
	c := schemaComponent("asm.resolve.broken", (*asmSchema)(nil), &asmTokenA{})
	resolver := &fakeResolver{fn: func(string, any, ComponentConfig) (ComponentConfig, error) {
		return ComponentConfig{}, boom
	}}
	reg := newTestRegistry(t, c)
	reg.Put(resolver)
	reg.Put(testComposition(configEntry{key: "asm.resolve.broken", value: nil}))

	err := reg.Prepare(ctx)
	if !errors.Is(err, boom) {
		t.Fatalf("Prepare = %v, want the resolver's error", err)
	}
	if !strings.Contains(err.Error(), `"asm.resolve.broken"`) || !strings.Contains(err.Error(), "stage prepare") {
		t.Errorf("error %q does not name the component and the stage", err)
	}
}

// requiredSchemaFixture declares a top-level and a nested required field.
type requiredSchemaFixture struct {
	Host     string `json:"host"`
	Port     int    `json:"port" config:"required"`
	Password struct {
		Memory uint32 `json:"memory" config:"required"`
	} `json:"password"`
}

func TestPrepareRequiredConfigValue(t *testing.T) {
	ctx := context.Background()
	newRegistry := func(t *testing.T, resolver *fakeResolver, block map[string]any) *ComponentRegistry {
		t.Helper()
		c := schemaComponent("asm.required.a", (*requiredSchemaFixture)(nil), &asmTokenA{})
		reg := newTestRegistry(t, c)
		if resolver != nil {
			reg.Put(resolver)
		}
		reg.Put(testComposition(configEntry{key: "asm.required.a", value: block}))
		return reg
	}

	t.Run("missing from the file block", func(t *testing.T) {
		reg := newRegistry(t, nil, map[string]any{"host": "h"})
		err := reg.Prepare(ctx)
		if !errors.Is(err, ErrMissingConfigValue) {
			t.Fatalf("Prepare = %v, want ErrMissingConfigValue", err)
		}
		for _, want := range []string{`"asm.required.a"`, `"port"`, `"password.memory"`, "required"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	t.Run("supplied by the file block", func(t *testing.T) {
		reg := newRegistry(t, nil, map[string]any{
			"port":     8080,
			"password": map[string]any{"memory": 256},
		})
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
	})

	t.Run("supplied by the resolver", func(t *testing.T) {
		resolver := &fakeResolver{fn: func(_ string, _ any, file ComponentConfig) (ComponentConfig, error) {
			return file.
				With("port", 8080).
				With("password", NewComponentConfig(map[string]any{"memory": 256})), nil
		}}
		reg := newRegistry(t, resolver, map[string]any{"host": "h"})
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
	})

	t.Run("resolver result without the key still fails", func(t *testing.T) {
		resolver := &fakeResolver{fn: func(_ string, _ any, file ComponentConfig) (ComponentConfig, error) {
			return file, nil
		}}
		reg := newRegistry(t, resolver, map[string]any{"host": "h"})
		if err := reg.Prepare(ctx); !errors.Is(err, ErrMissingConfigValue) {
			t.Fatalf("Prepare = %v, want ErrMissingConfigValue", err)
		}
	})

	t.Run("an explicit zero counts as supplied", func(t *testing.T) {
		// The required declaration is judged by presence, not by zero value:
		// a source that states the field supplies it, whatever it states.
		reg := newRegistry(t, nil, map[string]any{
			"port":     0,
			"password": map[string]any{"memory": 0},
		})
		if err := reg.Prepare(ctx); err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
	})
}

// sensitiveSchemas are the documentation pairings the sensitive check judges.
type (
	sensitiveUndocumented struct {
		Token string `json:"token" config:"sensitive"`
	}
	sensitiveEmptyDoc struct {
		Token string `json:"token" config:"sensitive"`
	}
	sensitiveOtherDoc struct {
		Token string `json:"token" config:"sensitive"`
	}
	sensitiveDocumented struct {
		Token string `json:"token" config:"sensitive"`
	}
)

func (sensitiveEmptyDoc) ConfigDocs() map[string]FieldDoc {
	return map[string]FieldDoc{"token": {Default: "no description"}}
}

func (sensitiveOtherDoc) ConfigDocs() map[string]FieldDoc {
	return map[string]FieldDoc{"other": {Description: "a field nobody marked sensitive"}}
}

func (sensitiveDocumented) ConfigDocs() map[string]FieldDoc {
	// Keyed by the Go field name: the lookup folds it onto the json spelling.
	return map[string]FieldDoc{"Token": {Description: "the token this fixture seals"}}
}

func TestPrepareSensitiveFieldNeedsDocs(t *testing.T) {
	prepare := func(schema any) error {
		c := schemaComponent("asm.sensitive.a", schema, &asmTokenA{})
		_, err := prepareAssembly(t, []Component{c}, configEntry{key: "asm.sensitive.a", value: nil})
		return err
	}

	for name, schema := range map[string]any{
		"no Documented implementation":      (*sensitiveUndocumented)(nil),
		"a doc entry without a description": (*sensitiveEmptyDoc)(nil),
		"a doc entry for another field":     (*sensitiveOtherDoc)(nil),
	} {
		t.Run(name, func(t *testing.T) {
			err := prepare(schema)
			if !errors.Is(err, ErrInvalidComponent) {
				t.Fatalf("Prepare = %v, want ErrInvalidComponent", err)
			}
			for _, want := range []string{`"asm.sensitive.a"`, `"token"`, "sensitive"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not carry %q", err, want)
				}
			}
		})
	}

	t.Run("a documented sensitive field passes", func(t *testing.T) {
		if err := prepare((*sensitiveDocumented)(nil)); err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
	})
}

// conflictSchemas are the key-path shapes the conflict check judges: the
// exposed variants hold a key path a source resolves, the plain ones are
// supplied by their own component's configuration block alone.
type (
	exposedTokenSchema struct {
		Token string `json:"token" config:"expose"`
	}
	exposedMaterialSchema struct {
		Material []byte `json:"pii_cipher_key" config:"expose"`
	}
	exposedHostSchema struct {
		Host string `json:"host" config:"expose"`
	}
	flatTokenSchema struct {
		Token string `json:"token"`
	}
	flatMaterialSchema struct {
		Material []byte `json:"pii_cipher_key"`
	}
	skippedTokenSchema struct {
		Token string `json:"token" config:"-"`
	}
)

func TestPrepareRejectsConflictingConfigKeyPaths(t *testing.T) {
	bareComponent := func(name string, schema any) Component {
		return schemaComponent(name, schema, &asmTokenA{})
	}
	prepare := func(comps ...Component) error {
		t.Helper()
		entries := make([]configEntry, 0, len(comps))
		for _, c := range comps {
			entries = append(entries, configEntry{key: c.Name, value: nil})
		}
		_, err := prepareAssembly(t, comps, entries...)
		return err
	}

	t.Run("two exposed bare fields at one path", func(t *testing.T) {
		err := prepare(
			bareComponent("asm.conflict.x", (*exposedTokenSchema)(nil)),
			bareComponent("asm.conflict.y", (*exposedTokenSchema)(nil)),
		)
		if !errors.Is(err, ErrConfigKeyConflict) {
			t.Fatalf("Prepare = %v, want ErrConfigKeyConflict", err)
		}
		for _, want := range []string{`"asm.conflict.x"`, `"asm.conflict.y"`, `"token"`, "stage prepare"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	t.Run("an exposed field against a BootstrapKeys declaration", func(t *testing.T) {
		declaring := schemaComponent("asm.conflict.declaring", (*asmSchema)(nil), &asmTokenB{})
		declaring.BootstrapKeys = []BootstrapKey{{Key: "pii_cipher_key"}}
		err := prepare(
			bareComponent("asm.conflict.flat", (*exposedMaterialSchema)(nil)),
			declaring,
		)
		if !errors.Is(err, ErrConfigKeyConflict) {
			t.Fatalf("Prepare = %v, want ErrConfigKeyConflict", err)
		}
		for _, want := range []string{`"asm.conflict.flat"`, `"asm.conflict.declaring"`, `"pii_cipher_key"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})

	t.Run("a custom namespace against another component's namespace", func(t *testing.T) {
		custom := schemaComponent("asm.conflict.custom", (*exposedHostSchema)(nil), &asmTokenA{})
		custom.ConfigNamespace = "asm.conflict.other"
		other := schemaComponent("asm.conflict.other", (*exposedHostSchema)(nil), &asmTokenB{})
		other.ConfigNamespace = "asm.conflict.other"
		err := prepare(custom, other)
		if !errors.Is(err, ErrConfigKeyConflict) {
			t.Fatalf("Prepare = %v, want ErrConfigKeyConflict", err)
		}
		if !strings.Contains(err.Error(), `"asm.conflict.other.host"`) {
			t.Errorf("error %q does not name the conflicting key path", err)
		}
	})

	t.Run("a bare field under a namespace it cannot collide with passes", func(t *testing.T) {
		declared := schemaComponent("asm.conflict.declared", (*exposedHostSchema)(nil), &asmTokenA{})
		declared.ConfigNamespace = "asm.conflict.other"
		if err := prepare(
			bareComponent("asm.conflict.bare", (*exposedTokenSchema)(nil)),
			declared,
		); err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
	})

	t.Run("same field name under distinct namespaces passes", func(t *testing.T) {
		one := schemaComponent("asm.conflict.one", (*exposedHostSchema)(nil), &asmTokenA{})
		one.ConfigNamespace = "asm.conflict.one"
		two := schemaComponent("asm.conflict.two", (*exposedHostSchema)(nil), &asmTokenB{})
		two.ConfigNamespace = "asm.conflict.two"
		if err := prepare(one, two); err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
	})

	t.Run("two block-only bare fields at one path coexist", func(t *testing.T) {
		// A field no source opens resolves from its own component's block
		// alone: the two values never share an address, so same bare names
		// are not a conflict.
		if err := prepare(
			bareComponent("asm.conflict.block.x", (*flatTokenSchema)(nil)),
			bareComponent("asm.conflict.block.y", (*flatTokenSchema)(nil)),
		); err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
	})

	t.Run("a block-only field beside a BootstrapKeys declaration coexists", func(t *testing.T) {
		declaring := schemaComponent("asm.conflict.declaring", (*asmSchema)(nil), &asmTokenB{})
		declaring.BootstrapKeys = []BootstrapKey{{Key: "pii_cipher_key"}}
		if err := prepare(
			bareComponent("asm.conflict.block", (*flatMaterialSchema)(nil)),
			declaring,
		); err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
	})

	t.Run("a skipped field claims no key path", func(t *testing.T) {
		if err := prepare(
			bareComponent("asm.conflict.skipped", (*skippedTokenSchema)(nil)),
			bareComponent("asm.conflict.claimed", (*exposedTokenSchema)(nil)),
		); err != nil {
			t.Fatalf("Prepare = %v, want nil", err)
		}
	})
}
