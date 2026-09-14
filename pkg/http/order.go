package http

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/vislake/speed/pkg/core"
)

// layer is one middleware as an endpoint recorded it: the registration itself,
// and the capabilities it stands for on the chain.
//
// provides is what an After or a Before constraint resolves against. A
// constraint names a capability, and the layers standing for that capability
// are the ones this layer is placed against; a layer that stands for nothing
// is still ordered, it is just never the target of anyone's constraint. The
// ordering reads the field and nothing about where it came from.
type layer struct {
	mw       Middleware
	provides []core.Token
}

// unlandedConstraint is one After or Before that placed nothing: after the
// edge to the declaring layer itself was dropped, no layer was left for it to
// point at, so it drew no edge and the declared position is not what the layer
// gets.
//
// It is not a failure: a capability absent from the assembly is a legal
// configuration, and the layer runs unconstrained by design. It is reported
// because nothing else about it is observable, and because the ways of
// arriving here are the same shape in the graph — the module that would stand
// for the capability is not in this process, or it is in it and left its
// Provides empty — so without the report none of them produces any signal at
// all.
type unlandedConstraint struct {
	// endpoint and layer say where the declaration was made.
	endpoint string
	layer    string
	// field is "After" or "Before" and at is the position in it, which is
	// what locates the one declaration among several on the same layer.
	field string
	at    int
	// capability is the type the token designates, rendered with its
	// package path, so the reader can go and look at who was meant to
	// stand for it.
	capability string
	// cause is why the constraint placed nothing. The two causes have
	// different fixing actions, so a report that does not tell them apart
	// sends half its readers after a module that is not the problem.
	cause unlandedCause
}

// unlandedCause is why a constraint placed nothing on the chain. The two are
// told apart because the fixing action differs: one asks who should have been
// in the assembly and was not, the other asks the declaring layer to correct
// what it said about itself.
type unlandedCause string

const (
	// causeNoProvider is a capability no layer on this endpoint stands for:
	// the module that would stand for it is not in this process, or it is in
	// it and left its Provides empty. The graph cannot tell those two apart.
	causeNoProvider unlandedCause = "no layer on this endpoint stands for the capability"
	// causeSelfOnly is a capability whose only representative is the layer
	// declaring the constraint. The self edge is dropped, so the constraint
	// places nothing; nobody is absent, and the declaration is what is wrong.
	// A capability another layer stands for as well is not this case: that
	// part of the claim holds, so the constraint did what it said.
	causeSelfOnly unlandedCause = "the declaring layer is the only layer standing for the capability"
)

// orderLayers returns an endpoint's middleware from outermost to innermost,
// together with the constraints that landed on nothing. The layers arrive in
// registration order, and that order is the final tie-breaker, so the caller
// appends as it registers and does not sort.
//
// After and Before give the graph its edges: After C places this layer inside
// the middleware standing for C, so an edge runs from each provider of C to
// this layer, the provider coming out first and landing further out. Before C
// is the same edge reversed. A constraint a provider stands beside places the
// layer; one that does not places nothing, and comes back in the second return
// value for the caller to write out with the reason, because nothing else
// about it is observable. That is not a failure — a capability absent from the
// assembly is a legal configuration — but a constraint nobody placed is a
// position the declaring layer believes it has and does not.
//
// Order decides only within the ready set — among the layers whose
// constraints are already satisfied at that point — so it can never select a
// position a constraint forbids, and the two ways of declaring a position
// cannot contradict each other. A smaller Order goes further out. Equal Order
// falls back to registration order, which makes (Order, registration) a total
// order and the result unique.
//
// The graph is built per endpoint, per assembly, and thrown away with the
// call. It costs O(V+E) in the number of layers and the constraints that hold
// between them, once per endpoint at startup.
func orderLayers(endpoint string, layers []layer) ([]layer, []unlandedConstraint, error) {
	providers := providersByCapability(endpoint, layers)

	var unlanded []unlandedConstraint
	// note records a constraint that placed nothing, with the reason. The
	// declaring layer's own standing is what the answer turns on: the edge
	// to it is dropped, so a layer that is the only one standing for the
	// capability it names places nothing either, and a layer another
	// provider stands beside still draws that edge.
	note := func(self int, name, field string, at int, key reflect.Type) {
		cause, unplaced := unplacedCause(providers[key], self)
		if !unplaced {
			return
		}
		unlanded = append(unlanded, unlandedConstraint{
			endpoint:   endpoint,
			layer:      name,
			field:      field,
			at:         at,
			capability: typeName(key),
			cause:      cause,
		})
	}

	edges := make([]map[int]bool, len(layers))
	indegree := make([]int, len(layers))
	for i := range layers {
		edges[i] = make(map[int]bool)
	}
	// A layer that declares a constraint on a capability it delivers itself
	// names itself among the providers. Drawing that edge would be a self
	// loop, which no order can satisfy, and the cycle it forms is an
	// artefact of the edge rather than a contradiction between two
	// declarations; the same edge can also be drawn twice, by one layer's
	// After and the other's Before, and counting it twice in the indegree
	// leaves a layer that never becomes ready and reports a cycle that is
	// not there.
	addEdge := func(from, to int) {
		if from == to || edges[from][to] {
			return
		}
		edges[from][to] = true
		indegree[to]++
	}
	for i, l := range layers {
		for j, token := range l.mw.After {
			key := capabilityKey(token, declarationSite(endpoint, l.mw.Name, "After", j))
			note(i, l.mw.Name, "After", j, key)
			for _, p := range providers[key] {
				addEdge(p, i)
			}
		}
		for j, token := range l.mw.Before {
			key := capabilityKey(token, declarationSite(endpoint, l.mw.Name, "Before", j))
			note(i, l.mw.Name, "Before", j, key)
			for _, p := range providers[key] {
				addEdge(i, p)
			}
		}
	}

	// Kahn's algorithm. The ready set is re-sorted each round rather than
	// kept as a heap: it is sorted by (Order, registration) on every round
	// anyway, and the round count is the number of layers on one endpoint.
	ready := make([]int, 0, len(layers))
	for i := range layers {
		if indegree[i] == 0 {
			ready = append(ready, i)
		}
	}
	order := make([]int, 0, len(layers))
	for len(ready) > 0 {
		slices.SortFunc(ready, func(a, b int) int {
			if c := cmp.Compare(layers[a].mw.Order, layers[b].mw.Order); c != 0 {
				return c
			}
			return cmp.Compare(a, b)
		})
		at := ready[0]
		ready = ready[1:]
		order = append(order, at)
		for _, to := range sortedTargets(edges[at]) {
			indegree[to]--
			if indegree[to] == 0 {
				ready = append(ready, to)
			}
		}
	}
	if len(order) != len(layers) {
		// No chain comes out of a cyclic graph, and the unlanded list
		// describes an assembly that is about to be abandoned; the
		// failure is the only thing worth reading here.
		return nil, nil, cycleError(endpoint, layers, order, edges)
	}

	out := make([]layer, 0, len(order))
	for _, at := range order {
		out = append(out, layers[at])
	}
	return out, unlanded, nil
}

// unplacedCause reports whether a constraint placed nothing, and why. The
// declaring layer is skipped when its own edge is dropped, so what decides it
// is whether any other layer stands for the capability: if one does, the
// constraint binds to that one and placed the layer; if none does, the
// constraint placed nothing, and the capability having no representative at
// all is a different fixing action from the declaring layer being its only
// representative.
func unplacedCause(providers []int, self int) (unlandedCause, bool) {
	selfOnly := false
	for _, p := range providers {
		if p != self {
			return "", false
		}
		selfOnly = true
	}
	if selfOnly {
		return causeSelfOnly, true
	}
	return causeNoProvider, true
}

// providersByCapability indexes the layers by the capabilities they stand for.
// Two tokens designating one capability land on one key, whoever wrote them,
// which is what lets a constraint reach a layer registered by a module it
// never names.
func providersByCapability(endpoint string, layers []layer) map[reflect.Type][]int {
	providers := make(map[reflect.Type][]int)
	for i, l := range layers {
		for j, token := range l.provides {
			key := capabilityKey(token, declarationSite(endpoint, l.mw.Name, "Provides", j))
			providers[key] = append(providers[key], i)
		}
	}
	return providers
}

// cycleError names the layers on a cycle along with their own declarations.
// Naming the members alone would leave the reader to hunt for the constraints
// that tied them together, and dropping one of those constraints is the whole
// of the fixing action.
//
// The walk starts from the lowest registration index still unresolved and
// follows successors in the same order, so one graph yields one text.
func cycleError(endpoint string, layers []layer, ordered []int, edges []map[int]bool) error {
	left := make(map[int]bool, len(layers))
	for i := range layers {
		left[i] = true
	}
	for _, at := range ordered {
		delete(left, at)
	}

	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := make([]int, len(layers))
	var stack, cycle []int

	var visit func(at int) bool
	visit = func(at int) bool {
		color[at] = grey
		stack = append(stack, at)
		for _, to := range sortedTargets(edges[at]) {
			if !left[to] {
				continue
			}
			switch color[to] {
			case grey:
				cycle = slices.Clone(stack[slices.Index(stack, to):])
				return true
			case white:
				if visit(to) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[at] = black
		return false
	}
	for _, at := range sortedTargets(left) {
		if color[at] == white && visit(at) {
			break
		}
	}
	if len(cycle) == 0 {
		// Unreachable through the walk above: a Kahn run that stalls
		// leaves at least one cycle in what it did not resolve. Reporting
		// the whole remainder keeps the message from going empty if that
		// ever stops holding.
		cycle = sortedTargets(left)
	}

	described := make([]string, 0, len(cycle)+1)
	for _, at := range cycle {
		described = append(described, describeLayer(endpoint, layers[at]))
	}
	described = append(described, fmt.Sprintf("%q", layers[cycle[0]].mw.Name))

	return fmt.Errorf("%w: endpoint %q: %s. After and Before are hard constraints, so they have "+
		"to form an acyclic graph: drop one of the constraints on this cycle, or state the "+
		"position with Order, which only picks among the positions the remaining constraints "+
		"already allow",
		ErrMiddlewareCycle, endpoint, strings.Join(described, " -> "))
}

// describeLayer renders one layer of a cycle: its name, and the constraints it
// declared for itself.
func describeLayer(endpoint string, l layer) string {
	var parts []string
	if names := capabilityNames(endpoint, l, "After", l.mw.After); len(names) > 0 {
		parts = append(parts, "After "+strings.Join(names, ", "))
	}
	if names := capabilityNames(endpoint, l, "Before", l.mw.Before); len(names) > 0 {
		parts = append(parts, "Before "+strings.Join(names, ", "))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%q (declares no constraint of its own)", l.mw.Name)
	}
	return fmt.Sprintf("%q (%s)", l.mw.Name, strings.Join(parts, "; "))
}

// capabilityNames renders the capabilities a field designates, keeping the
// package path, so the reader can find the declaration the text asks them to
// drop.
func capabilityNames(endpoint string, l layer, field string, tokens []core.Token) []string {
	names := make([]string, 0, len(tokens))
	for j, token := range tokens {
		names = append(names, typeName(capabilityKey(token, declarationSite(endpoint, l.mw.Name, field, j))))
	}
	return names
}

// declarationSite locates a token for the panic an illegal one raises: which
// endpoint, which layer, which field, which position in it.
func declarationSite(endpoint, name, field string, at int) string {
	return fmt.Sprintf("endpoint %q middleware %q %s[%d]", endpoint, name, field, at)
}

// sortedTargets renders the keys of an edge set in ascending order, which is
// what keeps a walk over the graph independent of Go's map iteration order.
func sortedTargets[V any](m map[int]V) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
