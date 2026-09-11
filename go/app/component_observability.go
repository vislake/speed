package app

import (
	"context"
	"fmt"

	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// component_observability.go carries the engine's observability component:
// the standard component that initializes the process's telemetry. It is
// registered into the global component set when go/app initializes -- every
// binary that imports the engine carries it -- and the loader's builtin
// composition defaults select it, so it participates in every assembly
// unless a higher configuration layer deselects it.
//
// The component's Prepare runs first among the components' Prepare callbacks
// (the engine registers it before any other component), which is where OTel
// gets initialized from the configuration the same loader loaded; its Close
// shuts the providers down and flushes, last in the reverse-order close
// because it is planned first. The engine never initializes observability
// itself: the whole telemetry lifecycle is this component's.

// The engine's observability component self-registers at package
// initialization, exactly as any component package does.
func init() {
	pkgcore.MustRegister(observabilityComponent())
}

// observabilityConfig is the component's configuration schema, resolved
// through the same five-source chain as every other component's block.
type observabilityConfig struct {
	// ServiceName becomes the service.name resource attribute. Empty leaves
	// go/observability's own documented default.
	ServiceName string `json:"service_name"`
	// OTLPEndpoint is the "host:port" OTLP/gRPC collector target traces and
	// metrics are pushed to; empty keeps the local exporters.
	OTLPEndpoint string `json:"otlp_endpoint"`
}

// observabilityRuntime is the bridge Prepare publishes: the shutdown
// function obs.Init returned. The instance does not exist yet when Prepare
// runs, so the runtime travels through the registry's by-type context; New
// reads it back and hands its own instance type the shutdown function, so
// Close shuts down exactly the providers this assembly's Prepare
// initialized (and the bridge value stays independently readable).
type observabilityRuntime struct {
	shutdown func(context.Context) error
}

// observabilityInstance is the component's product: the runtime's shutdown
// function, carried in the instance's own type so the product put at
// construction does not collide with the bridge value Prepare published.
type observabilityInstance struct {
	shutdown func(context.Context) error
}

// observabilityComponent returns the descriptor. It takes no dependencies
// and carries no assets. It declares MultiReplicaSafe: each replica exports
// its own spans and metrics, sharing no state with any sibling, so several
// replicas running it split nothing -- and telemetry stays optional by
// nature, so a composition that deselects the component still assembles.
func observabilityComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         "observability",
		ConfigSchema: (*observabilityConfig)(nil),
		Capabilities: pkgcore.MultiReplicaSafe,
		Prepare:      prepareObservability,
		New:          newObservability,
		Close:        closeObservability,
	}
}

// prepareObservability initializes OTel from the component's resolved
// configuration block -- read through OwnComponentConfig, so the reading
// follows whichever name the host selected this component under: a host
// that registers a renamed copy of the descriptor gets the copy's block,
// where a literal components.observability lookup would silently miss it.
// Both option halves are omitted when their value is empty, exactly as
// go/observability documents an unset value: an empty ServiceName leaves
// the package's default, an empty OTLPEndpoint the local exporters.
func prepareObservability(ctx context.Context, reg *pkgcore.ComponentRegistry) error {
	block, err := pkgcore.OwnComponentConfig(reg)
	if err != nil {
		return fmt.Errorf("app: read the observability component's own configuration block: %w", err)
	}
	var cfg observabilityConfig
	if decodeErr := block.Decode(&cfg); decodeErr != nil {
		return fmt.Errorf("app: the observability component configuration: %w", decodeErr)
	}
	var opts []obs.Option
	if cfg.ServiceName != "" {
		opts = append(opts, obs.WithServiceName(cfg.ServiceName))
	}
	if cfg.OTLPEndpoint != "" {
		opts = append(opts, obs.WithOTLPEndpoint(cfg.OTLPEndpoint))
	}
	shutdown, err := obs.Init(ctx, opts...)
	if err != nil {
		return fmt.Errorf("app: init observability: %w", err)
	}
	reg.Put(&observabilityRuntime{shutdown: shutdown})
	return nil
}

// newObservability returns the instance carrying the shutdown function
// Prepare's runtime published.
func newObservability(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
	runtime, err := pkgcore.Get[*observabilityRuntime](reg)
	if err != nil {
		return nil, fmt.Errorf("app: the observability runtime is missing; its Prepare callback publishes it before construction: %w", err)
	}
	return &observabilityInstance{shutdown: runtime.shutdown}, nil
}

// closeObservability shuts the providers down and flushes what they
// buffered. The flush runs on a context detached from the shutdown's own
// cancellation, so a cancelled drain context cannot cut the final export
// short; the exporters' bounds are go/observability's own.
func closeObservability(ctx context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
	observability, ok := instance.(*observabilityInstance)
	if !ok {
		return fmt.Errorf("app: the observability component holds an instance of type %T", instance)
	}
	if observability.shutdown == nil {
		return nil
	}
	return observability.shutdown(context.WithoutCancel(ctx))
}
