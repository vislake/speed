package jobs

// standalone_queue_bench_test.go holds the benchmark suite for the
// StandaloneQueue's hot paths, per docs/internal/20-quality-and-security.md's
// "performance-benchmark regression detection" plan: benchmarks land with the
// module that owns the hotspot, and the nightly pipeline's future regression
// leg runs them. Nothing here needs an external service: every benchmark runs
// against a private temp-file SQLite database (benchSQLite below mirrors
// dbkit/dbtest.NewSQLite with a *testing.B, which that helper's *testing.T
// signature cannot accept) -- the same durable write path production jobs
// pay.
//
// Run: go test -bench=BenchmarkStandaloneQueue -benchmem .

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

// standaloneQueueBenchSink keeps the last benchmarked result reachable, so a
// compiler cannot prove the operation dead and elide it.
var standaloneQueueBenchSink any

// benchSQLite returns a private, per-benchmark temp-file SQLite database
// opened through dbkit.Open exactly the way dbkit/dbtest.NewSQLite opens one
// (dbkit's full wiring included); the file lives under b.TempDir() and is
// closed by b.Cleanup. dbtest's own helper takes *testing.T, which a
// *testing.B does not satisfy, so benchmarks carry this local twin.
func benchSQLite(b *testing.B) *gorm.DB {
	b.Helper()
	dsn := filepath.Join(b.TempDir(), "bench.sqlite")
	db, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     dsn,
	})
	if err != nil {
		b.Fatalf("open temp-file sqlite database: %v", err)
	}
	b.Cleanup(func() {
		sqlDB, dbErr := db.DB()
		if dbErr != nil {
			return
		}
		_ = sqlDB.Close()
	})
	return db
}

// benchLogCtx returns a context whose logger writes to io.Discard.
// StandaloneQueue.Enqueue emits one "job enqueued" info line per call; a
// benchmark enqueues thousands of jobs per second, and a real log sink would
// flood the output and pay actual I/O on every measured operation.
func benchLogCtx() context.Context {
	return obs.WithLogger(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// benchTenant is the tenant every benchmarked job belongs to.
const benchTenant = pkgcore.TenantID("bench-tenant")

// benchRoundTripType is the job type the round-trip benchmark's handler
// registers for.
const benchRoundTripType = "benchmark.roundtrip"

// benchDeliveryPayload is a realistic task payload: the serialized shape of a
// notification delivery task, the kind of job a host enqueues most often. The
// bytes are shared across iterations; each Enqueue call still builds its own
// Task value around them, as a real caller would.
var benchDeliveryPayload = []byte(`{"recipient":{"class":"user","id":"user-4f7a92e1"},"type":"clinic.appointment_reminder","params":{"appointment_id":"apt-1042","patient_case":"case-8891"}}`)

// benchTask returns the Task the benchmarks enqueue: typeName selects the
// handler, and the payload is the shared delivery shape above.
func benchTask(tenant pkgcore.TenantID, typeName string) Task {
	return Task{
		Type:     typeName,
		TenantID: tenant,
		Payload:  benchDeliveryPayload,
	}
}

// BenchmarkStandaloneQueueEnqueue measures the durable single-job enqueue
// path: task validation, id generation, the transactional insert with its
// idempotency-key unique-index check and the per-job log line -- everything
// StandaloneQueue.Enqueue does for one task. The queue is constructed but
// never started, so no dispatcher competes for the rows and the write path is
// measured alone; each iteration commits its own transaction, the durability
// cost a real producer pays per job. The table grows by one row per
// iteration, mirroring the indexed-table depth a live queue works at.
func BenchmarkStandaloneQueueEnqueue(b *testing.B) {
	q := NewStandaloneQueue(benchSQLite(b))
	if err := ensureJobsSchema(context.Background(), q.db); err != nil {
		b.Fatalf("ensureJobsSchema() error = %v", err)
	}
	ctx := benchLogCtx()

	b.ReportAllocs()
	b.ResetTimer()
	var last JobID
	for i := 0; i < b.N; i++ {
		id, err := q.Enqueue(ctx, benchTask(benchTenant, "bench.enqueue"))
		if err != nil {
			b.Fatalf("Enqueue() error = %v", err)
		}
		last = id
	}
	standaloneQueueBenchSink = last
}

// benchRoundTripHandler counts handled jobs and returns immediately -- the
// only work the round-trip benchmark needs from the executed side. The
// handler increments done before the queue settles the job's terminal status,
// so the benchmark first waits for the counter and then confirms the terminal
// row, measuring the job's full passage through the queue rather than the
// handler alone.
type benchRoundTripHandler struct {
	done *atomic.Uint64
}

func (h *benchRoundTripHandler) Type() string { return benchRoundTripType }

func (h *benchRoundTripHandler) Handle(_ context.Context, _ *Job, _ ProgressFn) (Result, error) {
	h.done.Add(1)
	return Result{}, nil
}

// BenchmarkStandaloneQueueJobRoundTrip measures the end-to-end latency of one
// job through a running queue: enqueue, the dispatcher's next claim tick,
// worker execution and the terminal-status write. The poll interval bounds
// the latency, so the benchmark runs two configurations -- the fast cadence
// the module's own tests use and the DefaultPollInterval a production
// standalone deployment runs at -- and the gap between the two answers is
// exactly the cost of the dispatcher's polling granularity. The wait between
// iterations polls the job's record, so each iteration measures a complete
// enqueue-to-terminal passage.
func BenchmarkStandaloneQueueJobRoundTrip(b *testing.B) {
	for _, tc := range []struct {
		name string
		poll time.Duration
	}{
		{name: "poll-15ms", poll: 15 * time.Millisecond},
		{name: "poll-default-200ms", poll: DefaultPollInterval},
	} {
		b.Run(tc.name, func(b *testing.B) {
			// The worker logs one "job succeeded" line per completed job
			// through the default slog logger (its context carries no
			// WithLogger), which would flood the output at any real
			// iteration count; route the default logger to io.Discard for
			// this benchmark's duration, restoring it on the way out.
			prevDefault := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
			b.Cleanup(func() { slog.SetDefault(prevDefault) })

			q := NewStandaloneQueue(benchSQLite(b), WithPollInterval(tc.poll))
			done := &atomic.Uint64{}
			if err := q.RegisterHandler(&benchRoundTripHandler{done: done}); err != nil {
				b.Fatalf("RegisterHandler() error = %v", err)
			}
			if err := q.Start(context.Background()); err != nil {
				b.Fatalf("Start() error = %v", err)
			}
			b.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := q.Close(ctx); err != nil {
					b.Errorf("Close() error = %v", err)
				}
			})

			ctx := pkgcore.WithTenant(benchLogCtx(), benchTenant)
			b.ReportAllocs()
			b.ResetTimer()
			var last *Job
			for i := 0; i < b.N; i++ {
				id, err := q.Enqueue(ctx, benchTask(benchTenant, benchRoundTripType))
				if err != nil {
					b.Fatalf("Enqueue() error = %v", err)
				}
				last = waitBenchTerminal(ctx, q, id, uint64(i+1), done, b)
			}
			standaloneQueueBenchSink = last
		})
	}
}

// waitBenchTerminal blocks until the handler has run want times and the
// job's record has reached a terminal status, failing the benchmark after a
// generous deadline instead of hanging a nightly run forever.
func waitBenchTerminal(ctx context.Context, q *StandaloneQueue, id JobID, want uint64, done *atomic.Uint64, b *testing.B) *Job {
	b.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if done.Load() >= want {
			for {
				job, err := q.Get(ctx, id)
				if err != nil {
					b.Fatalf("Get(%q) error = %v", id, err)
				}
				if job.Status.Terminal() {
					return job
				}
				if time.Now().After(deadline) {
					b.Fatalf("timed out waiting for job %q to reach a terminal status; last status = %s", id, job.Status)
				}
				time.Sleep(500 * time.Microsecond)
			}
		}
		if time.Now().After(deadline) {
			b.Fatalf("timed out waiting for the handler to run %d times (ran %d)", want, done.Load())
		}
		time.Sleep(500 * time.Microsecond)
	}
}
