package metering

import (
	"context"
	"embed"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/go/metering/locales"
	"github.com/vislake/speed/go/metering/migrations"
)

// moduleName is metering's pkgcore.Module.Name(), and the key
// dbkit.MigrationRegistry.Register builds its dependency graph on.
const moduleName = "metering"

// The configuration keys metering contributes. See AGENTS.md's "Overage
// thresholds: declared config schema, Go-level values" section for why
// ConfigDefaultOverageThreshold is declared here but not read live by any
// of this round's code paths.
const (
	// ConfigPeriodBucketSize is the calendar bucket real-time counters and
	// usage-summary rows reset on: PeriodBucketDaily or
	// PeriodBucketMonthly.
	ConfigPeriodBucketSize = "metering.period_bucket_size"
	// ConfigDefaultOverageThreshold is the default per-period usage
	// quantity, applied to any feature with no threshold of its own,
	// above which metering publishes EventOverageThresholdCrossed. A
	// value of 0 (the pkgcore.ConfigItem.Type "int" zero value) means "no
	// default threshold" in exactly the way OverageThresholds.Default
	// being nil does -- see this item's Description.
	ConfigDefaultOverageThreshold = "metering.default_overage_threshold"
)

// configItemDecls is the catalog entry for each config item, declared in
// Register.
var configItemDecls = []pkgcore.ConfigItem{
	{
		Key:         ConfigPeriodBucketSize,
		Type:        "string",
		Default:     defaultPeriodBucket,
		Description: "The calendar bucket real-time usage counters and usage-summary rows reset on: \"daily\" or \"monthly\".",
		Group:       "metering",
	},
	{
		Key:         ConfigDefaultOverageThreshold,
		Type:        "int",
		Default:     0,
		Description: "The default per-period usage quantity, applied to any feature with no threshold of its own, above which metering publishes an overage-threshold-crossed event. Zero means no default threshold.",
		Group:       "metering",
		Min:         0,
	},
}

// Module implements pkgcore.Module for go/metering.
//
// # Wiring
//
// A host constructs one with NewModule and hands it to Kernel.Bootstrap.
// Constructing a Module performs no I/O: db is opened and migrated by the
// host before Register is ever called, exactly like every other module in
// this codebase. Register wires the pkgcore.EventBus onto the Aggregator
// (the one seam metering's own runtime needs beyond db) and declares this
// module's config schema and published event; it performs no I/O of its
// own.
//
// # Starting the background pipelines
//
// NewModule builds every piece (SummaryRepository, Aggregator,
// AnalyticsRecorder, Dispatcher) but starts nothing -- constructing a
// Module must not start goroutines any more than it may perform I/O,
// mirroring go/jobs.StandaloneQueue's identical New/Start split. A host
// calls Start(ctx) once Bootstrap has returned, and Stop() to shut both
// background loops down cleanly.
type Module struct {
	db *gorm.DB

	summaries  *SummaryRepository
	aggregator *Aggregator
	analytics  *AnalyticsRecorder
	dispatcher *Dispatcher
}

// Option configures a Module at construction time.
type Option func(*Module)

// WithPeriodBucket overrides the default period bucket (PeriodBucketMonthly)
// real-time counters and usage-summary rows reset on.
func WithPeriodBucket(bucket string) Option {
	return func(m *Module) { m.aggregator.bucket = bucket }
}

// WithOverageThresholds configures the per-feature (and default) overage
// thresholds Aggregator checks after every Ingest. Without this option, no
// threshold applies to any feature and EventOverageThresholdCrossed is
// never published.
func WithOverageThresholds(t OverageThresholds) Option {
	return func(m *Module) { m.aggregator.thresholds = t }
}

// WithAnalyticsBufferSize overrides AnalyticsRecorder's channel capacity
// (default defaultAnalyticsBufferSize). n <= 0 is ignored.
func WithAnalyticsBufferSize(n int) Option {
	return func(m *Module) {
		if n > 0 {
			m.analytics.events = make(chan UsageEvent, n)
		}
	}
}

// WithDispatchInterval overrides how often Dispatcher polls for pending
// outbox rows (default defaultDispatchInterval). d <= 0 is ignored.
func WithDispatchInterval(d time.Duration) Option {
	return func(m *Module) {
		if d > 0 {
			m.dispatcher.interval = d
		}
	}
}

// WithDispatchBatchSize overrides how many outbox rows Dispatcher claims
// per poll cycle (default defaultDispatchBatchSize). n <= 0 is ignored.
func WithDispatchBatchSize(n int) Option {
	return func(m *Module) {
		if n > 0 {
			m.dispatcher.batchSize = n
		}
	}
}

// NewModule returns a Module whose tables live in db. Constructing a
// Module performs no I/O: opening and migrating db is the host's
// responsibility, done before Bootstrap ever calls Register.
func NewModule(db *gorm.DB, opts ...Option) *Module {
	summaries := NewSummaryRepository(db)
	aggregator := NewAggregator(summaries)
	m := &Module{
		db:         db,
		summaries:  summaries,
		aggregator: aggregator,
		analytics:  NewAnalyticsRecorder(aggregator),
		dispatcher: NewDispatcher(db, aggregator),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Summaries returns the module's SummaryRepository.
func (m *Module) Summaries() *SummaryRepository { return m.summaries }

// Aggregator returns the module's Aggregator, the in-process aggregation
// backend both reliability tiers feed.
func (m *Module) Aggregator() *Aggregator { return m.aggregator }

// Recorder returns the module's analytics-grade Recorder. A business
// module wanting the billing-grade tier calls the package-level Enqueue
// function directly instead -- see Enqueue's doc comment for why it is
// not behind this interface.
func (m *Module) Recorder() Recorder { return m.analytics }

// Dispatcher returns the module's Dispatcher, the billing-grade tier's
// delivery loop.
func (m *Module) Dispatcher() *Dispatcher { return m.dispatcher }

// Start begins both background pipelines: AnalyticsRecorder's flush loop
// and Dispatcher's poll loop. It must be called after Bootstrap has
// returned (Register itself performs no I/O and starts nothing). Safe to
// call at most once; a second call is a no-op for each loop, per
// AnalyticsRecorder.Start's and Dispatcher.Start's own sync.Once contract.
func (m *Module) Start(ctx context.Context) {
	m.analytics.Start(ctx)
	m.dispatcher.Start(ctx)
}

// Stop signals both background pipelines to exit and waits for them to do
// so. Safe to call before Start, or more than once.
func (m *Module) Stop() {
	m.analytics.Stop()
	m.dispatcher.Stop()
}

// Name implements pkgcore.Module.
func (m *Module) Name() string { return moduleName }

// DependsOn implements pkgcore.Module: nothing. metering sits above
// authn/rbac/org in docs/internal/01-architecture.md's graph, but this
// round wires no cross-module event subscription and no consumer of any
// other module's declarations, so DependsOn -- which enumerates only
// modules in the bootstrap set metering itself requires -- returns nil,
// the same answer go/pki's identical-shaped Module gives for the same
// reason.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements pkgcore.Module.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements pkgcore.Module: the descriptions of metering's error
// codes, in both supported languages with identical id sets.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements pkgcore.Module. metering has no HTTP surface this
// round -- it is a Go-level Recorder/Enqueue API business modules call
// in-process, not a service other code reaches over HTTP -- so this
// returns nil, the same "no fragment yet" answer go/config's and go/pki's
// Module give.
func (m *Module) OpenAPISpec() []byte { return nil }

// Register implements pkgcore.Module. Per the interface's own contract it
// only declares and wires -- no database call, no outbound call, nothing
// that touches m.db. It declares metering's round-1 configuration schema
// and its one published event, and attaches the registry's EventBus onto
// Aggregator so a later Ingest call can publish
// EventOverageThresholdCrossed. It declares no permission and no audit
// action: this round has no HTTP surface for rbac to gate, and no
// operation privileged enough to warrant an audit trail entry -- see
// AGENTS.md's Known limitations rather than inventing either
// speculatively.
func (m *Module) Register(reg *pkgcore.Registry) error {
	if err := reg.Config.Add(configItemDecls...); err != nil {
		return err
	}
	if err := reg.Events.Publishes(overageEventDecl); err != nil {
		return err
	}
	m.aggregator.bus = reg.EventBus()
	return nil
}

// compile-time check that *Module satisfies pkgcore.Module.
var _ pkgcore.Module = (*Module)(nil)
