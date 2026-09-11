package jobs

// component.go registers the "queue.standalone" component with pkgcore's
// global component registration: the descriptor a composition configuration
// selects as the "queue" module's implementation. The queue the component
// builds is the module's one implementation (jobs.Queue) plus this
// package's own lifecycle: Start wires the assembled declarations onto the
// queue and then starts the worker pool and the periodic-task scheduler,
// Stop signals both without waiting, and Close drains.

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
)

// queueStandaloneConfig is the "queue.standalone" component's configuration
// schema, one field per key a composition block may carry; every key is
// optional and an unset one keeps the matching construction default.
//
//	worker                    bool          start this replica's worker pool
//	                                        and scheduler (default true;
//	                                        false is the worker-disabled
//	                                        replica of a multi-replica
//	                                        composition, which still wires
//	                                        the queue so Enqueue finds its
//	                                        table)
//	worker_count              int           jobs executed concurrently
//	tenant_concurrency_limit  int           jobs one tenant runs at once
//	poll_interval             duration      dispatcher tick cadence
//	job_timeout               duration      per-attempt timeout default
//	backoff_base/backoff_max  duration      retry backoff bounds
//	schedule_interval         duration      periodic-task scheduler tick
type queueStandaloneConfig struct {
	Worker                 *bool         `json:"worker"`
	WorkerCount            int           `json:"worker_count"`
	TenantConcurrencyLimit int           `json:"tenant_concurrency_limit"`
	PollInterval           time.Duration `json:"poll_interval"`
	JobTimeout             time.Duration `json:"job_timeout"`
	BackoffBase            time.Duration `json:"backoff_base"`
	BackoffMax             time.Duration `json:"backoff_max"`
	ScheduleInterval       time.Duration `json:"schedule_interval"`
}

// standaloneQueueInstance is the product of "queue.standalone": the queue
// itself (whose promoted methods satisfy Queue, the module's contract) plus
// the two runtime decisions Start makes but New cannot act on -- whether
// this replica runs a worker pool, and the scheduler's tick interval.
type standaloneQueueInstance struct {
	*StandaloneQueue
	runWorker        bool
	scheduleInterval time.Duration
	scheduler        *Scheduler
}

// queueStandalone validates cfg into the component's construction options:
// the queue's own Option values plus the two runtime knobs. A key that is
// set but cannot be honoured (a count below 1, a duration at or below zero)
// is refused by the option constructors' own coded panics -- the same
// refuse-at-option-time rule every Option in this package follows -- which
// a component author would see at assembly time.
func queueStandaloneOptions(c queueStandaloneConfig) ([]Option, bool, time.Duration) {
	var opts []Option
	if c.WorkerCount != 0 {
		opts = append(opts, WithWorkerCount(c.WorkerCount))
	}
	if c.TenantConcurrencyLimit != 0 {
		opts = append(opts, WithTenantConcurrencyLimit(c.TenantConcurrencyLimit))
	}
	if c.PollInterval != 0 {
		opts = append(opts, WithPollInterval(c.PollInterval))
	}
	if c.JobTimeout != 0 {
		opts = append(opts, WithJobTimeout(c.JobTimeout))
	}
	if c.BackoffBase != 0 || c.BackoffMax != 0 {
		base, max := c.BackoffBase, c.BackoffMax
		if base == 0 {
			base = DefaultBackoffBase
		}
		if max == 0 {
			max = DefaultBackoffMax
		}
		opts = append(opts, WithBackoff(base, max))
	}
	runWorker := c.Worker == nil || *c.Worker
	return opts, runWorker, c.ScheduleInterval
}

// queueStandaloneComponent is the component descriptor for
// "queue.standalone": the database-backed queue and worker pool the
// standalone deployment mode runs, declaring the exported capabilities of a
// shared-database implementation -- MultiReplicaSafe (the jobs live in the
// database every replica shares, and the single-writer registration fails
// closed rather than splitting work between two live writers, with a
// crashed writer's registration taken over once it goes stale) and
// SurvivesRestart (a restarted replica loses nothing the queue holds; the
// rows are the database's, and interrupted claims are recovered on the next
// Start).
//
// Requires: the database (its *gorm.DB product) is mandatory; the event bus
// is optional -- with one selected the queue publishes its
// jobs.job.terminal events on it, with none selected the queue runs without
// publishing. Provides (*Queue)(nil) so consumers declare the portable
// contract.
var queueStandaloneComponent = pkgcore.Component{
	Name:         "queue.standalone",
	Module:       "queue",
	Provides:     []any{(*Queue)(nil)},
	Capabilities: pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart,
	ConfigSchema: (*queueStandaloneConfig)(nil),
	Requires: []pkgcore.Requirement{
		{Token: (*gorm.DB)(nil)},
		{Token: (*pkgcore.EventBus)(nil), Optional: true},
		{Token: (*TenantLister)(nil), Optional: true},
	},
	New: func(_ context.Context, reg *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
		var c queueStandaloneConfig
		if err := cfg.Decode(&c); err != nil {
			return nil, err
		}
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		opts, runWorker, scheduleInterval := queueStandaloneOptions(c)
		// The bus is read here, at construction: a selected bus component
		// is ordered before this one by the optional requirement above, so
		// its product exists. With no bus selected or put, the queue
		// publishes nothing, exactly the WithEventBus-omitted behavior.
		bus, hasBus, err := pkgcore.GetOptional[pkgcore.EventBus](reg)
		if err != nil {
			return nil, err
		}
		if hasBus {
			opts = append(opts, WithEventBus(bus))
		}
		return &standaloneQueueInstance{
			StandaloneQueue:  NewStandaloneQueue(db, opts...),
			runWorker:        runWorker,
			scheduleInterval: scheduleInterval,
		}, nil
	},
	Start: func(ctx context.Context, reg *pkgcore.ComponentRegistry, instance any) error {
		q, ok := instance.(*standaloneQueueInstance)
		if !ok {
			return fmt.Errorf("jobs: queue.standalone: unexpected instance type %T", instance)
		}

		// Wire first: the Start stage runs after every Init callback, so the
		// Jobs seat holds every module's declared handlers and the queue's
		// own tables are created -- a worker-disabled replica still wires,
		// so Enqueue finds its table and no declared handler is refused as
		// unregistered. Start stays conditional on the worker flag.
		if err := Wire(ctx, q.StandaloneQueue, reg.Jobs); err != nil {
			return err
		}
		if !q.runWorker {
			return nil
		}
		if err := q.Start(ctx); err != nil {
			return err
		}

		schedulerOpts := []SchedulerOption{WithSchedules(reg.Schedules)}
		// The lister is read now, at use time: a host publishes its tenant
		// universe during its own Init (it depends on runtime services), so
		// it exists by Start. Without one, a PerTenant declaration makes
		// Scheduler.Start refuse with ErrTenantListerRequired rather than
		// silently sweeping nothing.
		lister, hasLister, err := pkgcore.GetOptional[TenantLister](reg)
		if err != nil {
			return err
		}
		if hasLister {
			schedulerOpts = append(schedulerOpts, WithTenantLister(lister))
		}
		if q.scheduleInterval > 0 {
			schedulerOpts = append(schedulerOpts, WithInterval(q.scheduleInterval))
		}
		scheduler := NewScheduler(q.StandaloneQueue, schedulerOpts...)
		// context.Background(), never ctx, per Scheduler.Start's own doc
		// comment: the enqueues must keep running until the Stop callback
		// stops the scheduler, not be cut short by whatever cancels the
		// assembly context.
		if err := scheduler.Start(context.Background()); err != nil {
			return err
		}
		q.scheduler = scheduler
		return nil
	},
	Stop: func(_ context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
		q, ok := instance.(*standaloneQueueInstance)
		if !ok {
			return fmt.Errorf("jobs: queue.standalone: unexpected instance type %T", instance)
		}
		// The scheduler stops first -- it enqueues into the queue, so it
		// must not race the queue's own wind-down -- and its Stop is the
		// one bounded wait here: it returns once an in-flight tick's
		// enqueues have finished, a few row inserts, never a drain of job
		// execution. The queue's own Stop is the non-blocking signal; the
		// drain is Close's.
		if q.scheduler != nil {
			q.scheduler.Stop()
		}
		q.Stop()
		return nil
	},
	Close: func(ctx context.Context, _ *pkgcore.ComponentRegistry, instance any) error {
		q, ok := instance.(*standaloneQueueInstance)
		if !ok {
			return fmt.Errorf("jobs: queue.standalone: unexpected instance type %T", instance)
		}
		return q.Close(ctx)
	},
}

func init() { pkgcore.MustRegister(queueStandaloneComponent) }
