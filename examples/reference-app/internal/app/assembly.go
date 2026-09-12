package app

// This file carries the host's single assembly view: one reading of the
// declaration seats, the resolved infrastructure seam values and the boot's
// merged message catalog. Every host step is written against the view, so
// one step body serves every step component.

import (
	"embed"
	"fmt"

	speedchain "github.com/vislake/speed/go/app/chain"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/i18n"
)

// assemblyView is what the host's steps read: the declaration seats a step
// registers onto, the resolved seam values it hands to services, and the
// merged message catalog. The two registry shapes answer the same seat
// interfaces (RouteRegistrar, RetentionRegistrar, JobHandlerRegistrar,
// EventRegistrar, PeriodicTaskRegistrar), so building a view is a type
// adaptation, never a second declaration path.
type assemblyView struct {
	// routes is the registry reading the middleware chain derives its route
	// partition and its outermost middleware layer from (chain.Standard's
	// RouteSource: the mounted routes plus the registry's Middleware seat).
	routes speedchain.RouteSource
	// retention is the retention seat: where a step registers the
	// participants a compliance sweep drives.
	retention pkgcore.RetentionRegistrar
	// jobs is the job-handler seat the queue is wired from.
	jobs pkgcore.JobHandlerRegistrar
	// events is the event seat: where subscriptions are installed, on the
	// bus the same assembly resolved for publishing.
	events pkgcore.EventRegistrar
	// schedules is the periodic-task seat the scheduler runs over.
	schedules pkgcore.PeriodicTaskRegistrar
	// bus is the assembled event bus.
	bus pkgcore.EventBus
	// kv is the assembled key-value store.
	kv pkgcore.KVStore
	// catalog is the merged message catalog.
	catalog *i18n.Catalog
}

// viewFromComponents returns the view over a component assembly's registry:
// its declaration seats, the EventBus and KVStore values the assembly put,
// and the boot's shared message catalog (mergedCatalog). A registry missing
// either seam value fails here, naming the seam, rather than surfacing a
// nil store somewhere inside a step.
func (b *serverBuild) viewFromComponents(reg *pkgcore.ComponentRegistry) (assemblyView, error) {
	bus, err := pkgcore.Get[pkgcore.EventBus](reg)
	if err != nil {
		return assemblyView{}, fmt.Errorf("reference-app: read the assembled event bus: %w", err)
	}
	kv, err := pkgcore.Get[pkgcore.KVStore](reg)
	if err != nil {
		return assemblyView{}, fmt.Errorf("reference-app: read the assembled key-value store: %w", err)
	}
	catalog, err := b.mergedCatalog(reg)
	if err != nil {
		return assemblyView{}, err
	}
	return assemblyView{
		routes:    reg,
		retention: reg.Retention,
		jobs:      reg.Jobs,
		events:    reg.Events,
		schedules: reg.Schedules,
		bus:       bus,
		kv:        kv,
		catalog:   catalog,
	}, nil
}

// mergedCatalog returns the boot's one merged message catalog. Its input --
// the plan's locale assets -- is frozen when Prepare writes the plan (Assets
// reads the plan, and a registration after Prepare never enters it), so
// every view this build derives reads the same catalog: the first view
// builds it, every later view shares the instance, and the instance is
// retired with the build itself -- one serverBuild serves one boot.
func (b *serverBuild) mergedCatalog(reg *pkgcore.ComponentRegistry) (*i18n.Catalog, error) {
	b.catalogOnce.Do(func() {
		b.catalog, b.catalogErr = hostCatalog(pkgcore.Assets(reg))
	})
	return b.catalog, b.catalogErr
}

// hostCatalog merges the selected components' locale resources into the
// message catalog, the same mechanism a module set's locales merge under
// (i18n.Builder over each carrier's own module and locale files). The id
// prefix is the MODULE the component implements (falling back to the
// component's name for an assembly-time step that implements none): the
// message ids belong to the module's contract, so an override component --
// which carries a host name but the module's own resources -- merges under
// the module's name exactly as the module's own descriptor would, and the
// builder's prefix check holds for both. A carrier whose embedded resources
// and module disagree fails here, naming the component rather than
// rendering a missing id later. Components that carry no locale resources
// are skipped: a zero embed.FS contributes nothing, and it is the ordinary
// shape of a component that renders no message.
func hostCatalog(assets []pkgcore.Asset) (*i18n.Catalog, error) {
	var zeroLocales embed.FS
	builder := i18n.NewBuilder()
	for _, asset := range assets {
		if asset.Locales == zeroLocales {
			continue
		}
		prefix := asset.Module
		if prefix == "" {
			prefix = asset.Name
		}
		if err := builder.AddModule(prefix, asset.Locales); err != nil {
			return nil, fmt.Errorf("reference-app: component %q has invalid locale resources: %w", asset.Name, err)
		}
	}
	return builder.Build(), nil
}
