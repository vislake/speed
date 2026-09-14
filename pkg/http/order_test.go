package http

import (
	"errors"
	nethttp "net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/vislake/speed/pkg/core"
)

// The capabilities the fixtures below order themselves against. What they
// stand for does not matter to the ordering: this module builds the graph out
// of the tokens and never learns what any layer does.
type (
	capRecovery interface{ recovery() }
	capAuth     interface{ auth() }
	capTenant   interface{ tenant() }
	capQuota    interface{ quota() }
)

// passThrough is a layer body. The ordering never calls it; a registration
// carries one, so the fixtures do too.
func passThrough(h nethttp.Handler) nethttp.Handler { return h }

// registered builds one layer with its own delivery list supplied directly.
// That list is what the constraints of the other layers resolve against, and
// the registration surface has no way to fill it: Middleware declares no
// capability and Use carries no registrant identity. These fixtures therefore
// reach a state Use cannot produce, and what they pin is the ordering itself,
// not what a registrant can express through the surface today.
func registered(name string, order int, provides, after, before []core.Token) layer {
	return layer{
		mw: Middleware{
			Name:   name,
			After:  after,
			Before: before,
			Order:  order,
			Wrap:   passThrough,
		},
		provides: provides,
	}
}

// tokens is shorthand for a constraint or delivery list.
func tokens(list ...core.Token) []core.Token { return list }

// layerNames renders a solved chain outermost first, which is the order
// orderLayers returns.
func layerNames(layers []layer) []string {
	out := make([]string, 0, len(layers))
	for _, l := range layers {
		out = append(out, l.mw.Name)
	}
	return out
}

func solve(t *testing.T, layers []layer) []layer {
	t.Helper()
	out, err := orderLayers("public", layers)
	if err != nil {
		t.Fatalf("ordering %v failed: %v", layerNames(layers), err)
	}
	return out
}

// TestOrderDecidesWithinReadySet pins where Order applies: inside the ready
// set, and nowhere else.
//
// P is registered first and carries the largest Order, R the middle one, and Q
// the smallest but sits behind a constraint on P. Sorting by Order first and
// running a stable topological pass over the result — the shape that reads as
// if it honoured both — yields Q, R, P: Q's constraint is satisfied by the
// time the pass reaches it, so nothing pulls it back. Choosing within the
// ready set yields R, P, Q, because Q is not in the ready set at all until P
// has been taken out.
func TestOrderDecidesWithinReadySet(t *testing.T) {
	layers := []layer{
		registered("p", 5, tokens((*capRecovery)(nil)), nil, nil),
		registered("q", 0, nil, tokens((*capRecovery)(nil)), nil),
		registered("r", 3, nil, nil, nil),
	}

	got := layerNames(solve(t, layers))
	want := []string{"r", "p", "q"}
	if !slices.Equal(got, want) {
		t.Errorf("chain is %v, want %v: Order picks within the ready set, so q cannot be "+
			"reached before p leaves it", got, want)
	}
}

// TestConstraintBeatsSmallerOrder pins that a constraint outranks Order. The
// recovery layer asks for the outermost position with the smallest Order in
// the endpoint and is registered first, and still lands inside the layer it
// declared itself to be after.
func TestConstraintBeatsSmallerOrder(t *testing.T) {
	layers := []layer{
		registered("recover", -100, nil, tokens((*capAuth)(nil)), nil),
		registered("auth", 0, tokens((*capAuth)(nil)), nil, nil),
	}

	got := layerNames(solve(t, layers))
	want := []string{"auth", "recover"}
	if !slices.Equal(got, want) {
		t.Errorf("chain is %v, want %v: After is a hard constraint, and neither a smaller "+
			"Order nor an earlier registration may place recover outside auth", got, want)
	}
}

// TestBeforeEdgeReversesDirection pins that Before draws the same edge as
// After with its ends swapped. Both halves hold the layers, their Order values
// and their registration order fixed, so the pair coming out reversed can only
// be the field that changed.
//
// In the Before half the outcome also contradicts Order and registration order
// at once, which is what makes it a statement about the edge rather than about
// a tie-break.
func TestBeforeEdgeReversesDirection(t *testing.T) {
	after := []layer{
		registered("auth", 0, tokens((*capAuth)(nil)), nil, nil),
		registered("peer", 10, nil, tokens((*capAuth)(nil)), nil),
	}
	before := []layer{
		registered("auth", 0, tokens((*capAuth)(nil)), nil, nil),
		registered("peer", 10, nil, nil, tokens((*capAuth)(nil))),
	}

	gotAfter := layerNames(solve(t, after))
	if want := []string{"auth", "peer"}; !slices.Equal(gotAfter, want) {
		t.Errorf("with After the chain is %v, want %v", gotAfter, want)
	}
	gotBefore := layerNames(solve(t, before))
	if want := []string{"peer", "auth"}; !slices.Equal(gotBefore, want) {
		t.Errorf("with Before the chain is %v, want %v: Before places the layer outside the "+
			"capability's providers, which is the After edge reversed", gotBefore, want)
	}
}

// TestEqualOrderKeepsRegistrationOrder pins the final tie-break: layers with
// equal Order and no constraint between them stay in the order they were
// registered in.
//
// What is pinned here is this endpoint's own record — the order in which it
// received three Use calls, which is the order it appends them in. The
// fixture is three registrations a single module makes in a row, because that
// is the sequence this module answers for. How two different modules' Init
// callbacks are ordered against each other is not asserted anywhere here.
//
// The names are registered out of alphabetical order, so an implementation
// that reached for the name as a tie-break instead would come out different.
func TestEqualOrderKeepsRegistrationOrder(t *testing.T) {
	layers := []layer{
		registered("gamma", 7, nil, nil, nil),
		registered("alpha", 7, nil, nil, nil),
		registered("beta", 7, nil, nil, nil),
	}

	got := layerNames(solve(t, layers))
	want := []string{"gamma", "alpha", "beta"}
	if !slices.Equal(got, want) {
		t.Errorf("chain is %v, want %v: equal Order falls back to the order the endpoint "+
			"recorded the registrations in", got, want)
	}
}

// TestMissingProviderDropsConstraint pins the cost the design accepts: a
// constraint naming a capability nothing on this endpoint delivers disappears,
// and the assembly carries on with no failure and no diagnostic. The layer
// that declared itself inside recovery is ordered by its Order alone, which
// here puts it where its constraint would never have allowed it to be if a
// recovery layer had been present.
func TestMissingProviderDropsConstraint(t *testing.T) {
	layers := []layer{
		registered("trace", 5, nil, tokens((*capRecovery)(nil)), nil),
		registered("metrics", 1, nil, nil, nil),
	}

	out, err := orderLayers("public", layers)
	if err != nil {
		t.Fatalf("a constraint with no provider must not fail the assembly, got: %v", err)
	}
	got := layerNames(out)
	want := []string{"metrics", "trace"}
	if !slices.Equal(got, want) {
		t.Errorf("chain is %v, want %v: with no provider for the capability the constraint "+
			"draws no edge, leaving Order to decide", got, want)
	}
}

// TestSelfConstraintIsNotACycle pins that a layer naming a capability it
// delivers itself is not ordered against itself. A module that both provides
// recovery and declares itself inside recovery — the shape a module registering
// two layers falls into — names itself among the providers, and the self loop
// that edge would make is unsatisfiable by construction rather than a
// contradiction between two declarations.
func TestSelfConstraintIsNotACycle(t *testing.T) {
	layers := []layer{
		registered("recover", 0,
			tokens((*capRecovery)(nil)),
			tokens((*capRecovery)(nil)),
			tokens((*capRecovery)(nil))),
		registered("trace", 1, nil, nil, nil),
	}

	got := layerNames(solve(t, layers))
	want := []string{"recover", "trace"}
	if !slices.Equal(got, want) {
		t.Errorf("chain is %v, want %v", got, want)
	}
}

// TestDuplicateEdgeIsCountedOnce pins that one relation declared from both
// ends is one edge. The After on one layer and the Before on the other say the
// same thing, and counting the relation twice in the indegree leaves the inner
// layer one decrement short of ready: the solve stalls with every constraint
// satisfiable and reports a cycle that does not exist.
func TestDuplicateEdgeIsCountedOnce(t *testing.T) {
	layers := []layer{
		registered("inner", 0, tokens((*capAuth)(nil)), tokens((*capTenant)(nil)), nil),
		registered("outer", 0, tokens((*capTenant)(nil)), nil, tokens((*capAuth)(nil))),
	}

	out, err := orderLayers("public", layers)
	if err != nil {
		t.Fatalf("one relation declared from both ends must solve, got: %v", err)
	}
	got := layerNames(out)
	want := []string{"outer", "inner"}
	if !slices.Equal(got, want) {
		t.Errorf("chain is %v, want %v", got, want)
	}
}

// TestCycleNamesEveryMemberAndTheirDeclarations pins what the failure has to
// carry. Asserting that the solve failed would pass just as well against an
// implementation whose message is empty, and an empty message leaves the
// reader with three modules that never name each other and no way to see which
// declarations tied them together.
//
// The layer off the cycle is in the fixture for the other half of the claim:
// the text names the members of the cycle, not every layer on the endpoint.
func TestCycleNamesEveryMemberAndTheirDeclarations(t *testing.T) {
	layers := []layer{
		registered("auth", 0, tokens((*capAuth)(nil)), tokens((*capTenant)(nil)), nil),
		registered("tenant", 0, tokens((*capTenant)(nil)), tokens((*capQuota)(nil)), nil),
		registered("quota", 0, tokens((*capQuota)(nil)), tokens((*capAuth)(nil)), nil),
		registered("offcycle", 0, nil, nil, nil),
	}

	out, err := orderLayers("public", layers)
	if !errors.Is(err, ErrMiddlewareCycle) {
		t.Fatalf("error is %v, want one wrapping ErrMiddlewareCycle", err)
	}
	if out != nil {
		t.Errorf("a failed solve returned a chain of %v, want none: a partial order is not a "+
			"chain anyone may assemble", layerNames(out))
	}

	text := err.Error()
	for _, name := range []string{"auth", "tenant", "quota"} {
		if !strings.Contains(text, `"`+name+`"`) {
			t.Errorf("the cycle report does not name %q, so the reader cannot tell who is on "+
				"the cycle: %s", name, text)
		}
	}
	for _, token := range []core.Token{(*capAuth)(nil), (*capTenant)(nil), (*capQuota)(nil)} {
		declared := typeName(reflect.TypeOf(token).Elem())
		if !strings.Contains(text, declared) {
			t.Errorf("the cycle report does not carry the declaration %s, which is the one the "+
				"reader has to drop: %s", declared, text)
		}
	}
	if strings.Contains(text, "offcycle") {
		t.Errorf("the cycle report names offcycle, which is not on the cycle: %s", text)
	}
}

// TestCycleReportIsDeterministic pins that one graph yields one text. The walk
// that picks the cycle out of the unresolved remainder runs over maps, and
// taking their keys as they come would hand the reader a different set of
// members, or the same set in a different rotation, from one run to the next.
func TestCycleReportIsDeterministic(t *testing.T) {
	layers := []layer{
		registered("auth", 0, tokens((*capAuth)(nil)), tokens((*capTenant)(nil)), nil),
		registered("tenant", 0, tokens((*capTenant)(nil)), tokens((*capQuota)(nil)), nil),
		registered("quota", 0, tokens((*capQuota)(nil)), tokens((*capAuth)(nil)), nil),
		registered("offcycle", 0, nil, nil, nil),
	}

	_, err := orderLayers("public", layers)
	if err == nil {
		t.Fatal("the fixture no longer forms a cycle, so nothing is being pinned")
	}
	first := err.Error()
	for i := range 100 {
		_, again := orderLayers("public", layers)
		if again == nil {
			t.Fatalf("run %d solved a cyclic graph", i)
		}
		if again.Error() != first {
			t.Fatalf("run %d reported\n\t%s\nand the first run reported\n\t%s", i, again.Error(), first)
		}
	}
}

// TestIllegalConstraintTokenPanicsNamingTheDeclaration pins that an illegal
// capability token panics where it was written. The rules themselves are
// pinned with capabilityKey; what is pinned here is that the text locates the
// declaration — which endpoint, which layer, which field and which position in
// it — because the panic carries no other trace back to the call site.
func TestIllegalConstraintTokenPanicsNamingTheDeclaration(t *testing.T) {
	defer func() {
		raised := recover()
		if raised == nil {
			t.Fatal("an untyped nil in After was accepted, so the layer is ordered against a " +
				"capability that designates nothing")
		}
		text, ok := raised.(string)
		if !ok {
			t.Fatalf("panicked with %T, want the string the capability rules raise", raised)
		}
		for _, want := range []string{`endpoint "public"`, `middleware "trace"`, "After[0]"} {
			if !strings.Contains(text, want) {
				t.Errorf("the panic does not carry %s, so the reader cannot find the "+
					"declaration: %s", want, text)
			}
		}
	}()

	_, _ = orderLayers("public", []layer{
		registered("trace", 0, nil, tokens(nil), nil),
	})
}

// TestNoLayersIsNotAFailure pins the endpoint that nobody registered a
// middleware on: it assembles, with the routing engine as the whole chain.
func TestNoLayersIsNotAFailure(t *testing.T) {
	out, err := orderLayers("public", nil)
	if err != nil {
		t.Fatalf("an endpoint with no middleware must solve, got: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("solving no layers produced %v", layerNames(out))
	}
}
