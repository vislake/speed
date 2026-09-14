package db

import (
	"context"
	"slices"
	"testing"

	"github.com/vislake/speed/pkg/core"
)

// This file pins the one ordering both consumers read: the migration sets and
// the plugin declarations are collected from the same registry and placed by
// the same relation, so a case here covers both rather than either.

// dependencyProvider stands for what one module takes up from another. It is
// what the providing module constructs, so a provider has a product to deliver.
//
// The stand-in is probeCapability, declared in migrate_test.go: the relation
// that orders migrations is the same one that orders plugin installation, and
// both are pinned against one definition of it rather than two.
type dependencyProvider struct{}

func (dependencyProvider) probe() {}

// providerModule delivers that capability.
func providerModule(name string) core.Module {
	return core.Module{
		Name:     name,
		Provides: []core.Provision{{Token: (*probeCapability)(nil)}},
		New:      func(context.Context, *core.Registry) (any, error) { return dependencyProvider{}, nil },
	}
}

// dependantModule takes it up, which is the whole of the edge between the two.
func dependantModule(name string) core.Module {
	return core.Module{
		Name:     name,
		Requires: []core.Requirement{{Token: (*probeCapability)(nil)}},
	}
}

// TestTheOrderComesFromRequiresNotFromNames pins what decides the order: a
// module's dependency on another, not its name.
//
// The dependency runs against the names on purpose. "alpha" depends on "zeta",
// so zeta's migrations are applied first and zeta's plugins are installed
// first; ordered by name, alpha would come first and both would run the other
// way round.
func TestTheOrderComesFromRequiresNotFromNames(t *testing.T) {
	rank := dependencyRank(registryOf(dependantModule("alpha"), providerModule("zeta")))

	if rank["zeta"] >= rank["alpha"] {
		t.Errorf("zeta is ranked %d and alpha %d, want the module whose capability alpha took up "+
			"placed first", rank["zeta"], rank["alpha"])
	}

	names := []string{"alpha", "zeta"}
	inDependencyOrder(rank, names)
	if want := []string{"zeta", "alpha"}; !slices.Equal(names, want) {
		t.Errorf("the modules come out as %v, want %v", names, want)
	}
}

// TestModulesWithNoRelationBetweenThemKeepNameOrder pins the fallback, which is
// the whole of the order between two modules that declared nothing about each
// other.
//
// It is not decoration: install order is callback order in GORM, so without it
// the same assembly could install two plugins in either order on two runs, and
// two replicas of it could run their callbacks in different orders.
func TestModulesWithNoRelationBetweenThemKeepNameOrder(t *testing.T) {
	rank := map[string]int{"alpha": 0, "middle": 0, "zeta": 0}
	names := []string{"zeta", "middle", "alpha"}

	inDependencyOrder(rank, names)

	if want := []string{"alpha", "middle", "zeta"}; !slices.Equal(names, want) {
		t.Errorf("modules at the same place come out as %v, want the name order %v", names, want)
	}
}

// cycleCapability is the other half of the cycle below.
type cycleCapability interface{ cycle() }

// TestAModuleOnACycleIsPlacedBehindEverythingElse pins what the ordering does
// with a registry it cannot fully order.
//
// The lifecycle refuses to construct a cycle, but a registry assembled by hand
// can hold one, and the ordering is not the place to fail over it — so the
// modules on it have to land somewhere. Dropping them would be the quiet
// failure: their migrations would never be applied and their plugins never
// installed, and nothing in the run would say which or why.
func TestAModuleOnACycleIsPlacedBehindEverythingElse(t *testing.T) {
	alpha := core.Module{
		Name:     "alpha",
		Provides: []core.Provision{{Token: (*probeCapability)(nil)}},
		Requires: []core.Requirement{{Token: (*cycleCapability)(nil)}},
	}
	zeta := core.Module{
		Name:     "zeta",
		Provides: []core.Provision{{Token: (*cycleCapability)(nil)}},
		Requires: []core.Requirement{{Token: (*probeCapability)(nil)}},
	}

	rank := dependencyRank(registryOf(alpha, zeta))

	if len(rank) != 2 {
		t.Fatalf("the ordering placed %d of the 2 registered modules: %v. A module left out of it "+
			"keeps its migrations and its plugins out of the run without a word", len(rank), rank)
	}
	names := []string{"zeta", "alpha"}
	inDependencyOrder(rank, names)
	if want := []string{"alpha", "zeta"}; !slices.Equal(names, want) {
		t.Errorf("the modules on the cycle come out as %v, want the name order %v", names, want)
	}
}
