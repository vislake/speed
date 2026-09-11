package app

// This file pins the boot's message-catalog sharing: each of the four host
// steps that derive an assembly view reads the same catalog build, the
// merge input is the plan's frozen locale assets, and concurrent views
// share one instance.

import (
	"context"
	"sync"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/i18n"

	"github.com/vislake/speed/examples/reference-app/internal/app/testdata/hostcatalog/alpha"
	"github.com/vislake/speed/examples/reference-app/internal/app/testdata/hostcatalog/beta"
)

// localeCarrierFixture builds the smallest assembly a view can be derived
// over: a registry carrying one locale-carrying component plus the two seam
// values viewFromComponents reads, planned (Prepare ran), so the locale
// assets are in the plan the catalog merge reads.
func localeCarrierFixture(t *testing.T) (*serverBuild, *pkgcore.ComponentRegistry) {
	t.Helper()
	b := newServerBuild(ServerConfig{Port: "8080"})
	reg := pkgcore.NewComponentRegistry()
	carrier := pkgcore.Component{
		Name:         "reference-app.alpha-carrier",
		Module:       "alpha",
		Capabilities: pkgcore.MultiReplicaSafe,
		Locales:      alpha.FS,
		New:          newHostStep,
	}
	if err := reg.Register(carrier); err != nil {
		t.Fatalf("register %q: %v", carrier.Name, err)
	}
	reg.Put(pkgcore.ComponentConfig{}.With("strict", true).
		With("components", pkgcore.ComponentConfig{}.With(carrier.Name, nil)))
	reg.Put(pkgcore.NewMemoryEventBus())
	reg.Put(pkgcore.NewMemoryKVStore())
	if err := reg.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() over the locale-carrier fixture: %v", err)
	}
	return b, reg
}

// TestViewFromComponents_SharesOneCatalogBuild pins the catalog's sharing
// contract: the host steps derive their views one after another through
// viewFromComponents, and every view carries the boot's single catalog
// build, so the merge runs once per boot however many steps read a view.
func TestViewFromComponents_SharesOneCatalogBuild(t *testing.T) {
	b, reg := localeCarrierFixture(t)

	first, err := b.viewFromComponents(reg)
	if err != nil {
		t.Fatalf("first view: %v", err)
	}
	if _, err := first.catalog.Lookup(i18n.LocaleENUS, "alpha.greeting", nil); err != nil {
		t.Fatalf("lookup alpha.greeting over the first view's catalog: %v", err)
	}

	// The four view-carrying steps (post-bootstrap, post-attach, the
	// application component and pre-serve) all read through this one
	// derivation.
	for step := 2; step <= 4; step++ {
		next, err := b.viewFromComponents(reg)
		if err != nil {
			t.Fatalf("view %d: %v", step, err)
		}
		if next.catalog != first.catalog {
			t.Fatalf("view %d derived a catalog at %p, want the shared build at %p", step, next.catalog, first.catalog)
		}
	}
}

// TestViewFromComponents_CatalogInputIsFrozenByThePlan pins the premise the
// shared build rests on: the merge reads the plan's locale assets, which
// Prepare writes once, so a component registered after Prepare contributes
// nothing to a later merge and a later view still carries the same build.
// If the plan's locale assets ever stop being frozen at Prepare -- a
// merge input that grows after Prepare -- this test fails and the
// sharing contract needs re-deriving.
func TestViewFromComponents_CatalogInputIsFrozenByThePlan(t *testing.T) {
	b, reg := localeCarrierFixture(t)

	first, err := b.viewFromComponents(reg)
	if err != nil {
		t.Fatalf("first view: %v", err)
	}

	latecomer := pkgcore.Component{
		Name:         "reference-app.beta-carrier",
		Module:       "beta",
		Capabilities: pkgcore.MultiReplicaSafe,
		Locales:      beta.FS,
		New:          newHostStep,
	}
	if regErr := reg.Register(latecomer); regErr != nil {
		t.Fatalf("register %q after Prepare: %v", latecomer.Name, regErr)
	}

	// The merge input itself -- checked without the cache -- still omits
	// the late component's resources.
	fresh, err := hostCatalog(pkgcore.Assets(reg))
	if err != nil {
		t.Fatalf("hostCatalog() over the post-registration assets: %v", err)
	}
	if _, lookupErr := fresh.Lookup(i18n.LocaleENUS, "beta.notice", nil); lookupErr == nil {
		t.Fatal("a post-Prepare registration entered the catalog merge's input; the shared build's frozen-input premise no longer holds")
	}

	second, err := b.viewFromComponents(reg)
	if err != nil {
		t.Fatalf("second view: %v", err)
	}
	if second.catalog != first.catalog {
		t.Fatalf("the second view derived a catalog at %p, want the shared build at %p", second.catalog, first.catalog)
	}
}

// TestViewFromComponents_CatalogBuildIsConcurrencySafe pins the sharing
// contract under concurrent derivation: whatever order the views are
// derived in, every one of them receives the boot's single catalog build.
func TestViewFromComponents_CatalogBuildIsConcurrencySafe(t *testing.T) {
	b, reg := localeCarrierFixture(t)

	const views = 8
	start := make(chan struct{})
	catalogs := make([]*i18n.Catalog, views)
	errs := make([]error, views)
	var wg sync.WaitGroup
	for i := 0; i < views; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			view, err := b.viewFromComponents(reg)
			catalogs[i], errs[i] = view.catalog, err
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < views; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent view %d: %v", i, errs[i])
		}
		if catalogs[i] != catalogs[0] {
			t.Fatalf("concurrent view %d derived a catalog at %p, want the shared build at %p", i, catalogs[i], catalogs[0])
		}
	}
}
