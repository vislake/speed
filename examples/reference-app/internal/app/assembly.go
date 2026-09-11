package app

// This file carries the host's single assembly view: one reading of the
// declaration seats, the resolved infrastructure seam values and the merged
// message catalog an assembly produced, whatever registry shape produced
// them. Two constructors build it -- viewFromModules over the module
// Registry a Kernel.Bootstrap returns, viewFromComponents over the component
// assembly's registry -- and every host step is written against the view, so
// one step body serves both drives.

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
	// modules is the module Registry the assembly bootstrapped, or nil on a
	// view built from a component registry. It is the module world's own
	// surface: the typed Attach calls and the bootstrap-binding
	// verification read it, and both retire with the module-to-component
	// bridge.
	modules *pkgcore.Registry
}

// viewFromModules returns the view over the module Registry a
// Kernel.Bootstrap returned: its declaration seats (re-pointed at the
// component assembly's seats by the bridge, so reads and writes travel the
// same registrars), its resolved seams and its merged catalog.
func viewFromModules(modules *pkgcore.Registry) assemblyView {
	return assemblyView{
		routes:    modules,
		retention: modules.Retention,
		jobs:      modules.Jobs,
		events:    modules.Events,
		schedules: modules.Schedules,
		bus:       modules.EventBus(),
		kv:        modules.KVStore(),
		catalog:   modules.Locales(),
		modules:   modules,
	}
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
// message catalog, the same mechanism Kernel.Bootstrap merges a module set's
// (i18n.Builder over each carrier's own name and locale files). The
// component's name is its locale id prefix: the builder rejects an asset
// whose message ids do not start with the name it is registered under, so a
// component whose embedded resources and name disagree fails here, naming
// the component rather than rendering a missing id later. Components that
// carry no locale resources are skipped -- a zero embed.FS contributes
// nothing, and it is the ordinary shape of a component that renders no
// message.
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
	return builder.Build(), nil
}
