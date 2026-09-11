package metering

// component.go carries metering's descriptor for the config-driven component
// assembly: the selection key a composition configuration names, the assets
// the module brings, the contract it consumes, and the callbacks that
// construct, declare, start and stop it. Its Init runs the module's one
// declaration entry point, Register, so the module's declarations reach the
// assembly's seats whether a host drives the module Registry or the assembly
// drives the descriptor -- the same declaration body either way.

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/metering/locales"
	"github.com/vislake/speed/go/metering/migrations"
)

// componentConfig is metering's configuration schema in the assembly: the
// construction-time knobs NewModule's options carry, as structured
// configuration. The runtime-tunable knobs (metering.period_bucket_size,
// metering.default_overage_threshold) stay on the runtime configuration seat
// and are deliberately not repeated here.
type componentConfig struct {
	PeriodBucket               string                   `json:"period_bucket"`
	OverageThresholds          *overageThresholdsConfig `json:"overage_thresholds"`
	AnalyticsBufferSize        int                      `json:"analytics_buffer_size"`
	DispatchInterval           time.Duration            `json:"dispatch_interval"`
	DispatchBatchSize          int                      `json:"dispatch_batch_size"`
	DispatchRetryDelay         time.Duration            `json:"dispatch_retry_delay"`
	DispatchEscalationAttempts int                      `json:"dispatch_escalation_attempts"`
	OutboxRetention            time.Duration            `json:"outbox_retention"`
}

// overageThresholdsConfig is the overage threshold block: the same shape
// OverageThresholds carries, under the assembly's structured configuration.
type overageThresholdsConfig struct {
	Default    *float64           `json:"default"`
	PerFeature map[string]float64 `json:"per_feature"`
}

// component returns metering's component descriptor: the value init
// registers, so a composition configuration can select the module and the
// assembly can construct it from the database product in the by-type
// context. Its Init runs the module's one declaration entry point, Register,
// inside the assembly's Init stage -- the one stage whose seats accept
// writes.
func component() pkgcore.Component {
	return pkgcore.Component{
		Name:   moduleName,
		Module: moduleName,
		Requires: []pkgcore.Requirement{
			// The database connection the module's tables live in, the
			// selected db component's product.
			{Token: (*gorm.DB)(nil)},
		},
		// The construction deliveries are the *Module plus the *Aggregator
		// it exposes -- the usage reading billing's UsageReader token
		// resolves against; NewModule builds the Aggregator eagerly, so New
		// puts it alongside its own returned product and the declaration
		// delivers in full.
		Provides: []any{(*Module)(nil), (*Aggregator)(nil)},
		// metering's state is its rows in the deployment's shared database
		// and the shared event bus, so several replicas may run it at once.
		Capabilities: pkgcore.MultiReplicaSafe,
		ConfigSchema: (*componentConfig)(nil),
		Migrations:   migrations.FS,
		Locales:      locales.FS,
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
			var c componentConfig
			if err := cfg.Decode(&c); err != nil {
				return nil, err
			}
			db, err := pkgcore.Get[*gorm.DB](reg)
			if err != nil {
				return nil, err
			}
			var opts []Option
			if c.PeriodBucket != "" {
				opts = append(opts, WithPeriodBucket(c.PeriodBucket))
			}
			if c.OverageThresholds != nil {
				opts = append(opts, WithOverageThresholds(OverageThresholds{
					Default:    c.OverageThresholds.Default,
					PerFeature: c.OverageThresholds.PerFeature,
				}))
			}
			if c.AnalyticsBufferSize != 0 {
				opts = append(opts, WithAnalyticsBufferSize(c.AnalyticsBufferSize))
			}
			if c.DispatchInterval != 0 {
				opts = append(opts, WithDispatchInterval(c.DispatchInterval))
			}
			if c.DispatchBatchSize != 0 {
				opts = append(opts, WithDispatchBatchSize(c.DispatchBatchSize))
			}
			if c.DispatchRetryDelay != 0 {
				opts = append(opts, WithDispatchRetryDelay(c.DispatchRetryDelay))
			}
			if c.DispatchEscalationAttempts != 0 {
				opts = append(opts, WithDispatchEscalationAttempts(c.DispatchEscalationAttempts))
			}
			if c.OutboxRetention != 0 {
				opts = append(opts, WithOutboxRetention(c.OutboxRetention))
			}
			m := NewModule(db, opts...)
			// The Aggregator is a construction value (NewModule builds it
			// eagerly), so it is one of this component's construction-time
			// deliveries: put it so the declared token resolves for every
			// consumer, exactly as the Module product does.
			reg.Put(m.Aggregator())
			return m, nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
			m, ok := instance.(*Module)
			if !ok {
				return fmt.Errorf("metering: component init got a %T instance, want *metering.Module", instance)
			}
			return m.Register(reg)
		},
		Start: func(ctx context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
			m, ok := instance.(*Module)
			if !ok {
				return fmt.Errorf("metering: component start got a %T instance, want *metering.Module", instance)
			}
			m.Start(ctx)
			return nil
		},
		Stop: func(_ context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
			m, ok := instance.(*Module)
			if !ok {
				return fmt.Errorf("metering: component stop got a %T instance, want *metering.Module", instance)
			}
			// metering's Stop signals both background pipelines and waits
			// for them to exit; it is idempotent and safe before Start, so
			// it carries the module's whole stop in the assembly's Stop
			// stage.
			m.Stop()
			return nil
		},
	}
}

func init() {
	pkgcore.MustRegister(component())
}
