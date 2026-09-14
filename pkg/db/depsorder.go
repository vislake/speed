package db

import (
	"cmp"
	"maps"
	"reflect"
	"slices"

	"github.com/vislake/speed/pkg/core"
)

// Everything this module takes from the registry is ordered by the declaring
// module's place in the dependency order, and the ordering lives here alone.
// Migration sets and plugins are both consumed in that order, and two copies of
// it would drift apart on the tie-break between modules at the same place and
// on what a cycle falls back to — the same registry, two answers, neither of
// them wrong on its own.

// inDependencyOrder sorts module names by where the module falls in the
// dependency order.
//
// Ties go to the module name. Modules at the same place have no dependency
// between them, so nothing else would decide, and a stable order is what makes
// one build's result the next build's: without it, two runs of the same
// assembly could install two plugins in either order.
func inDependencyOrder(rank map[string]int, names []string) {
	slices.SortFunc(names, func(a, b string) int {
		if order := cmp.Compare(rank[a], rank[b]); order != 0 {
			return order
		}
		return cmp.Compare(a, b)
	})
}

// dependencyRank places every registered module in dependency order and reports
// each one's position.
//
// The order comes from the Requires declarations the modules already carry: a
// module whose tables reference another module's tables declares a dependency
// on that module's capability, and gets its migrations applied after it. There
// is no second ordering mechanism, and without that declaration there is no
// order between the two.
//
// core plans construction from the same relation but does not export the
// result, so the topological sort is repeated here. Ties go to the module name,
// so the same set of modules yields the same order between builds.
func dependencyRank(reg *core.Registry) map[string]int {
	modules := reg.Modules()
	names := make([]string, 0, len(modules))
	byName := make(map[string]core.Module, len(modules))
	providers := make(map[reflect.Type][]string)
	for _, m := range modules {
		names = append(names, m.Name)
		byName[m.Name] = m
		for _, provision := range m.Provides {
			if ct := capabilityOf(provision.Token); ct != nil {
				providers[ct] = append(providers[ct], m.Name)
			}
		}
	}
	slices.Sort(names)

	edges := make(map[string]map[string]bool, len(names))
	indegree := make(map[string]int, len(names))
	for _, name := range names {
		edges[name] = make(map[string]bool)
		indegree[name] = 0
	}
	for _, name := range names {
		for _, requirement := range byName[name].Requires {
			ct := capabilityOf(requirement.Token)
			if ct == nil {
				continue
			}
			for _, provider := range providers[ct] {
				if provider == name || edges[provider][name] {
					continue
				}
				edges[provider][name] = true
				indegree[name]++
			}
		}
	}

	ready := make([]string, 0, len(names))
	for _, name := range names {
		if indegree[name] == 0 {
			ready = append(ready, name)
		}
	}
	rank := make(map[string]int, len(names))
	placed := make(map[string]bool, len(names))
	for len(ready) > 0 {
		slices.Sort(ready)
		name := ready[0]
		ready = ready[1:]
		rank[name] = len(rank)
		placed[name] = true
		for _, to := range slices.Sorted(maps.Keys(edges[name])) {
			indegree[to]--
			if indegree[to] == 0 {
				ready = append(ready, to)
			}
		}
	}
	// A cycle cannot reach this point through the lifecycle — core refuses
	// to construct one — but a registry assembled by hand can hold one, and
	// ordering is not the place to fail a startup over it. Whatever is left
	// keeps its name order behind everything that was placed.
	for _, name := range names {
		if !placed[name] {
			rank[name] = len(rank)
		}
	}
	return rank
}

// capabilityOf gives the interface type a token designates, or nil for a token
// core would have refused at registration.
func capabilityOf(token core.Token) reflect.Type {
	rt := reflect.TypeOf(token)
	if rt == nil || rt.Kind() != reflect.Pointer {
		return nil
	}
	return rt.Elem()
}
