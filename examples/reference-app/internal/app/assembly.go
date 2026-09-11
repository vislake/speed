package app

// This file carries the host's single assembly view: one reading of the
// declaration seats, the resolved infrastructure seam values and the merged
// message catalog an assembly produced. Every host step is written against
// the view, so one step body serves every step component.

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
	// routes is the mounted-route reading the middleware chain derives its
	// partition from (chain.Standard's RouteSource).
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
// and the catalog merged from the selected components' locale resources. A
// registry missing either seam value fails here, naming the seam, rather
// than surfacing a nil store somewhere inside a step.
func viewFromComponents(reg *pkgcore.ComponentRegistry) (assemblyView, error) {
	bus, err := pkgcore.Get[pkgcore.EventBus](reg)
	if err != nil {
		return assemblyView{}, fmt.Errorf("reference-app: read the assembled event bus: %w", err)
	}
	kv, err := pkgcore.Get[pkgcore.KVStore](reg)
	if err != nil {
		return assemblyView{}, fmt.Errorf("reference-app: read the assembled key-value store: %w", err)
	}
	catalog, err := hostCatalog(pkgcore.Assets(reg))
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

// hostCatalog merges the selected components' locale resources into the
// message catalog, the same mechanism a module set's locales merge under
// (i18n.Builder over each carrier's own name and locale files), plus the
// resources of the modules whose descriptors this host overrides
// (host_wiring.go's overriddenModuleLocales). The component name is the
// locale id prefix -- the builder rejects an asset whose message ids do not
// start with the name it is registered under, so an override component
// cannot carry its module's locales (it carries a host name) and those
// resources merge here under the module's own name -- and a component whose
// embedded resources and name disagree fails here, naming the component
// rather than rendering a missing id later. Components that carry no locale
// resources are skipped: a zero embed.FS contributes nothing, and it is the
// ordinary shape of a component that renders no message.
func hostCatalog(assets []pkgcore.Asset) (*i18n.Catalog, error) {
	var zeroLocales embed.FS
	builder := i18n.NewBuilder()
	for _, asset := range assets {
		if asset.Locales == zeroLocales {
			continue
		}
		if err := builder.AddModule(asset.Name, asset.Locales); err != nil {
			return nil, fmt.Errorf("reference-app: component %q has invalid locale resources: %w", asset.Name, err)
		}
	}
	for _, overridden := range overriddenModuleLocales {
		if err := builder.AddModule(overridden.name, overridden.fs); err != nil {
			return nil, fmt.Errorf("reference-app: overridden module %q has invalid locale resources: %w", overridden.name, err)
		}
	}
	return builder.Build(), nil
}
