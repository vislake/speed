package jobs

import (
	"context"
	"sync"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// newTestDB returns a *gorm.DB backed by a private, per-test temp-file
// SQLite database (dbkit/dbtest, the mandatory dual-dialect test helper —
// backend coding standard §13) with the jobs schema already applied. See
// AGENTS.md's Known limitations for why this package's own tests exercise
// SQLite only, never dbtest.NewPostgres.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := dbtest.NewSQLite(t)
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("ensureJobsSchema() error = %v", err)
	}
	return db
}

// fixtureRecordSeq gives every fixtureRecord a distinct id across a test
// binary run.
var fixtureRecordSeq int64

func nextFixtureID() string {
	fixtureRecordSeq++
	return time.Now().Format("20060102150405.000000000") + "-" + string(rune('a'+fixtureRecordSeq%26))
}

// fixtureRecord returns a minimally valid jobRecord: every NOT NULL column
// populated, ready to insert.
func fixtureRecord(tenant pkgcore.TenantID, jobType string) *jobRecord {
	now := time.Now().UTC().Truncate(time.Second)
	return &jobRecord{
		ID:           nextFixtureID(),
		Type:         jobType,
		TenantID:     string(tenant),
		Status:       string(StatusPending),
		Priority:     int(PriorityNormal),
		MaxRetries:   DefaultMaxRetries,
		TimeoutNanos: int64(DefaultTimeout),
		ScheduledAt:  now,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func TestEnsureJobsSchema_Idempotent(t *testing.T) {
	db := dbtest.NewSQLite(t)
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("first ensureJobsSchema() error = %v", err)
	}
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("second ensureJobsSchema() error = %v, want nil (CREATE TABLE/INDEX IF NOT EXISTS must be idempotent)", err)
	}
}

// TestJobRecord_NotTenantScoped is the mandatory isolation-assertion suite
// (root CLAUDE.md, backend coding standard §3.3) for platform data:
// jobRecord must NOT be affected by dbkit's tenant-scoping plugin, since
// the dispatcher scans eligible Jobs across every tenant at once. See
// jobRecord's own doc comment for the full design rationale.
func TestJobRecord_NotTenantScoped(t *testing.T) {
	db := newTestDB(t)

	createFn := func(session *gorm.DB) error {
		return session.Create(fixtureRecord("some-tenant", "probe.type")).Error
	}
	findFn := func(session *gorm.DB) (int64, error) {
		var n int64
		err := session.Model(&jobRecord{}).Count(&n).Error
		return n, err
	}

	tenancytest.AssertNotTenantScoped(t, db, jobRecord{}, createFn, findFn)
}

func TestInsertRecord_And_FindByID_Roundtrip(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "notes.export")
	rec.Payload = []byte(`{"note_id":"n-1"}`)

	id, err := insertRecord(context.Background(), db, rec)
	if err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if id != rec.ID {
		t.Fatalf("insertRecord() id = %q, want %q", id, rec.ID)
	}

	got, err := findByID(context.Background(), db, JobID(id))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Type != rec.Type || got.TenantID != rec.TenantID || string(got.Payload) != string(rec.Payload) {
		t.Errorf("findByID() = %+v, want a record matching %+v", got, rec)
	}
}

func TestFindByID_NotFound_ReturnsErrJobNotFound(t *testing.T) {
	db := newTestDB(t)
	_, err := findByID(context.Background(), db, JobID("does-not-exist"))
	if !isJobNotFound(err) {
		t.Errorf("findByID() error = %v, want ErrJobNotFound", err)
	}
}

func TestInsertRecord_IdempotencyKey_SecondCallReturnsFirstID(t *testing.T) {
	db := newTestDB(t)

	first := fixtureRecord("tenant-a", "billing.charge")
	first.IdempotencyKey = "charge-42"
	firstID, err := insertRecord(context.Background(), db, first)
	if err != nil {
		t.Fatalf("first insertRecord() error = %v", err)
	}

	second := fixtureRecord("tenant-a", "billing.charge")
	second.IdempotencyKey = "charge-42"
	secondID, err := insertRecord(context.Background(), db, second)
	if err != nil {
		t.Fatalf("second insertRecord() error = %v, want nil (idempotent no-op, not a conflict error)", err)
	}
	if secondID != firstID {
		t.Errorf("second insertRecord() id = %q, want the first call's id %q", secondID, firstID)
	}

	var count int64
	if err := db.Model(&jobRecord{}).
		Where("tenant_id = ? AND idempotency_key = ?", "tenant-a", "charge-42").
		Count(&count).Error; err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Errorf("rows for (tenant-a, charge-42) = %d, want 1 (second Enqueue must not create a new row)", count)
	}
}

func TestInsertRecord_IdempotencyKey_EmptyKeyNeverConflicts(t *testing.T) {
	db := newTestDB(t)
	for i := 0; i < 3; i++ {
		rec := fixtureRecord("tenant-a", "notes.export")
		// IdempotencyKey left empty.
		if _, err := insertRecord(context.Background(), db, rec); err != nil {
			t.Fatalf("insertRecord() call %d error = %v, want nil (no idempotency key must never collide)", i, err)
		}
	}
	var count int64
	if err := db.Model(&jobRecord{}).Where("tenant_id = ?", "tenant-a").Count(&count).Error; err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 3 {
		t.Errorf("rows = %d, want 3 independent Jobs", count)
	}
}

func TestInsertRecord_IdempotencyKey_DifferentTenantsDoNotCollide(t *testing.T) {
	db := newTestDB(t)

	a := fixtureRecord("tenant-a", "billing.charge")
	a.IdempotencyKey = "same-key"
	idA, err := insertRecord(context.Background(), db, a)
	if err != nil {
		t.Fatalf("insertRecord(tenant-a) error = %v", err)
	}

	b := fixtureRecord("tenant-b", "billing.charge")
	b.IdempotencyKey = "same-key"
	idB, err := insertRecord(context.Background(), db, b)
	if err != nil {
		t.Fatalf("insertRecord(tenant-b) error = %v", err)
	}

	if idA == idB {
		t.Errorf("insertRecord() for two different tenants using the same idempotency key returned the same id %q; idempotency must be scoped per tenant", idA)
	}
}

// TestInsertRecord_IdempotencyKey_ConcurrentEnqueue_ReturnsSameID proves the
// idempotency guarantee holds under genuine concurrent submission, not only
// when called twice sequentially: this is the scenario
// createJobsIdempotencySQL's own doc comment names as the reason Enqueue
// relies on a unique-index conflict rather than a check-then-insert
// sequence. Concurrency is kept modest (a handful of goroutines) because
// SQLite serializes writers on one file even under the bounded 5000 ms
// busy_timeout every dbkit SQLite connection carries (the driver's own
// default, declared explicitly by dbkit's dialect factory — see
// go/dbkit/AGENTS.md's "SQLite busy timeout" section): what this test pins
// is the idempotency property under genuine contention, not SQLite's writer
// scheduling, and a modest concurrency keeps the assertion about the
// unique-index conflict rather than about lock-wait convergence.
func TestInsertRecord_IdempotencyKey_ConcurrentEnqueue_ReturnsSameID(t *testing.T) {
	db := newTestDB(t)
	const concurrency = 6

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		ids     []string
		errs    []error
		release = make(chan struct{})
	)
	for i := 0; i < concurrency; i++ {
		rec := fixtureRecord("tenant-a", "billing.charge")
		rec.IdempotencyKey = "concurrent-charge"
		wg.Add(1)
		go func(rec *jobRecord) {
			defer wg.Done()
			<-release // released together, to maximize actual overlap
			id, err := insertRecord(context.Background(), db, rec)
			mu.Lock()
			ids = append(ids, id)
			errs = append(errs, err)
			mu.Unlock()
		}(rec)
	}
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("insertRecord() call %d error = %v, want nil", i, err)
		}
	}
	for i, id := range ids {
		if id != ids[0] {
			t.Errorf("insertRecord() call %d id = %q, want every concurrent call to agree on %q", i, id, ids[0])
		}
	}

	var count int64
	if err := db.Model(&jobRecord{}).
		Where("tenant_id = ? AND idempotency_key = ?", "tenant-a", "concurrent-charge").
		Count(&count).Error; err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if count != 1 {
		t.Errorf("rows for the concurrently-enqueued key = %d, want exactly 1", count)
	}
}

func TestClaimCandidates_OrdersByPriorityThenScheduledAt(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	low := fixtureRecord("tenant-a", "t")
	low.Priority = int(PriorityLow)
	low.ScheduledAt = now.Add(-time.Minute)

	highLater := fixtureRecord("tenant-a", "t")
	highLater.Priority = int(PriorityHigh)
	highLater.ScheduledAt = now.Add(-time.Second)

	highEarlier := fixtureRecord("tenant-a", "t")
	highEarlier.Priority = int(PriorityHigh)
	highEarlier.ScheduledAt = now.Add(-time.Minute)

	notYetDue := fixtureRecord("tenant-a", "t")
	notYetDue.Priority = int(PriorityHigh)
	notYetDue.ScheduledAt = now.Add(time.Hour)

	succeeded := fixtureRecord("tenant-a", "t")
	succeeded.Status = string(StatusSucceeded)
	succeeded.Priority = int(PriorityHigh)
	succeeded.ScheduledAt = now.Add(-time.Hour)

	retrying := fixtureRecord("tenant-a", "t")
	retrying.Status = string(StatusRetrying)
	retrying.Priority = int(PriorityNormal)
	retrying.ScheduledAt = now.Add(-time.Hour)

	for _, r := range []*jobRecord{low, highLater, highEarlier, notYetDue, succeeded, retrying} {
		if _, err := insertRecord(context.Background(), db, r); err != nil {
			t.Fatalf("insertRecord() error = %v", err)
		}
	}

	got, err := claimCandidates(context.Background(), db, now, claimBatchSize)
	if err != nil {
		t.Fatalf("claimCandidates() error = %v", err)
	}

	wantIDs := []string{highEarlier.ID, highLater.ID, retrying.ID, low.ID}
	if len(got) != len(wantIDs) {
		t.Fatalf("claimCandidates() returned %d rows, want %d: %v", len(got), len(wantIDs), got)
	}
	for i, w := range wantIDs {
		if got[i].ID != w {
			t.Errorf("claimCandidates()[%d].ID = %q, want %q (priority desc, then scheduled_at asc)", i, got[i].ID, w)
		}
	}
}

// TestClaimCandidates_FairShareRotationAcrossTenants_PriorityWithinTenantShare
// pins the cross-tenant ordering contract Priority's own doc comment (job.go)
// now states explicitly: tenant fair-share rotation takes precedence over
// Priority ACROSS tenants, while Priority orders the Jobs WITHIN one tenant's
// own share (and, at the same wave position, which tenant's head-of-line Job
// leads the wave).
//
// The single-tenant TestClaimCandidates_OrdersByPriorityThenScheduledAt above
// cannot see this distinction: every row shares one tenant_id, so the
// round-robin interleaving is a no-op there. This test's four rows are
// arranged so that every plausible naive ordering differs from the pinned
// one:
//
//	tenant-a: aHighOlder (High, t-2m -- its rank 1), aHighNewer (High, t-1m --
//	          its rank 2), aLowNewest (Low, t0 -- its rank 3)
//	tenant-b: bLowOldest  (Low, t-3m -- its rank 1, the oldest eligible row)
//
// Expected window: [aHighOlder, bLowOldest, aHighNewer, aLowNewest].
//
//   - tenant-b's head-of-line Job is claimed before tenant-a's SECOND Job --
//     and even before tenant-a's lowest-Priority Job that is the newest row
//     of all -- so rotation beats a flat "priority DESC across everything"
//     (which would yield [aHighOlder, aHighNewer, bLowOldest, aLowNewest])
//     and beats raw age (bLowOldest is oldest, yet trails aHighOlder).
//   - aHighOlder leads the wave over bLowOldest even though bLowOldest is
//     older: at the same wave position (tenant_rank), Priority breaks the
//     tie between different tenants' head-of-line rows.
//   - Within tenant-a's own share, Priority still orders its Jobs: both
//     High rows claim their turns before the Low row, equal-Priority rows in
//     ScheduledAt order (aHighOlder before aHighNewer).
func TestClaimCandidates_FairShareRotationAcrossTenants_PriorityWithinTenantShare(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)

	aHighOlder := fixtureRecord("tenant-a", "t")
	aHighOlder.Priority = int(PriorityHigh)
	aHighOlder.ScheduledAt = now.Add(-2 * time.Minute)

	aHighNewer := fixtureRecord("tenant-a", "t")
	aHighNewer.Priority = int(PriorityHigh)
	aHighNewer.ScheduledAt = now.Add(-time.Minute)

	aLowNewest := fixtureRecord("tenant-a", "t")
	aLowNewest.Priority = int(PriorityLow)
	aLowNewest.ScheduledAt = now

	bLowOldest := fixtureRecord("tenant-b", "t")
	bLowOldest.Priority = int(PriorityLow)
	bLowOldest.ScheduledAt = now.Add(-3 * time.Minute)

	for _, r := range []*jobRecord{aHighOlder, aHighNewer, aLowNewest, bLowOldest} {
		if _, err := insertRecord(context.Background(), db, r); err != nil {
			t.Fatalf("insertRecord() error = %v", err)
		}
	}

	got, err := claimCandidates(context.Background(), db, now, claimBatchSize)
	if err != nil {
		t.Fatalf("claimCandidates() error = %v", err)
	}

	wantIDs := []string{aHighOlder.ID, bLowOldest.ID, aHighNewer.ID, aLowNewest.ID}
	if len(got) != len(wantIDs) {
		t.Fatalf("claimCandidates() returned %d rows, want %d: %v", len(got), len(wantIDs), got)
	}
	for i, w := range wantIDs {
		if got[i].ID != w {
			t.Errorf("claimCandidates()[%d].ID = %q, want %q (tenant fair-share rotation first, then priority within a tenant's own share)", i, got[i].ID, w)
		}
	}
}

func TestClaimOne_ClaimsAndIncrementsAttempts(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}

	claimed, err := claimOne(context.Background(), db, *rec, time.Now())
	if err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	if !claimed {
		t.Fatalf("claimOne() = false, want true for a freshly inserted pending record")
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Status != string(StatusRunning) {
		t.Errorf("Status after claim = %q, want %q", got.Status, StatusRunning)
	}
	if got.Attempts != 1 {
		t.Errorf("Attempts after claim = %d, want 1", got.Attempts)
	}
	if got.StartedAt == nil {
		t.Error("StartedAt after claim = nil, want set")
	}
}

// TestClaimOne_StaleSnapshotLosesTheRace proves claimOne's WHERE ... AND
// status = ? guard: a second attempt to claim the SAME row using a
// snapshot taken before the first, now-stale, claim succeeded reports
// claimed = false with no error, rather than double-claiming or erroring.
// This is deliberately exercised with two sequential calls against one
// stale snapshot, rather than real goroutine concurrency, so the assertion
// is exact and immune to SQLite's writer serialization under genuine
// concurrent writers — the bounded busy_timeout on every dbkit SQLite
// connection (see go/dbkit/AGENTS.md's "SQLite busy timeout" section) makes
// contended writes wait rather than fail, but lock-wait scheduling is not
// what this test asserts (see
// TestInsertRecord_IdempotencyKey_ConcurrentEnqueue_ReturnsSameID's own doc
// comment for the same concern) — the two code paths exercise the exact
// same WHERE-clause guard either way, since claimOne itself has no notion
// of "concurrent" versus "stale sequential".
func TestClaimOne_StaleSnapshotLosesTheRace(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	staleSnapshot := *rec // status: pending, as read before any claim.

	firstClaimed, err := claimOne(context.Background(), db, staleSnapshot, time.Now())
	if err != nil {
		t.Fatalf("first claimOne() error = %v", err)
	}
	if !firstClaimed {
		t.Fatalf("first claimOne() = false, want true")
	}

	secondClaimed, err := claimOne(context.Background(), db, staleSnapshot, time.Now())
	if err != nil {
		t.Fatalf("second claimOne() error = %v, want nil (lost race is a no-op, not an error)", err)
	}
	if secondClaimed {
		t.Fatalf("second claimOne() = true, want false: the row was already claimed once, so a second claim against the same stale snapshot must lose")
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (the lost claim must not have incremented it a second time)", got.Attempts)
	}
}

func TestUpdateProgress(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}

	if err := updateProgress(context.Background(), db, rec.ID, 42, "working"); err != nil {
		t.Fatalf("updateProgress() error = %v", err)
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.ProgressPct != 42 || got.ProgressMsg != "working" {
		t.Errorf("progress = (%d, %q), want (42, %q)", got.ProgressPct, got.ProgressMsg, "working")
	}
}

func TestCompleteSucceeded(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now()); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	if err := completeSucceeded(context.Background(), db, rec.ID, Result{Data: []byte("done")}, time.Now()); err != nil {
		t.Fatalf("completeSucceeded() error = %v", err)
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Status != string(StatusSucceeded) {
		t.Errorf("Status = %q, want %q", got.Status, StatusSucceeded)
	}
	if string(got.Result) != "done" {
		t.Errorf("Result = %q, want %q", got.Result, "done")
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt = nil, want set")
	}
}

// TestCompleteSucceeded_ClearsStaleErrorFromEarlierRetry proves Job.Error's
// documented contract ("empty otherwise") actually holds: a Job that failed
// one or more earlier attempts (recording a message via completeRetrying)
// before eventually succeeding must not keep reporting that stale failure
// once it is StatusSucceeded.
func TestCompleteSucceeded_ClearsStaleErrorFromEarlierRetry(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now()); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	if err := completeRetrying(context.Background(), db, rec.ID, "first attempt failed", time.Now(), time.Now()); err != nil {
		t.Fatalf("completeRetrying() error = %v", err)
	}

	retried, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	_, err = claimOne(context.Background(), db, *retried, time.Now())
	if err != nil {
		t.Fatalf("second claimOne() error = %v", err)
	}
	err = completeSucceeded(context.Background(), db, rec.ID, Result{Data: []byte("done")}, time.Now())
	if err != nil {
		t.Fatalf("completeSucceeded() error = %v", err)
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want empty once Status is StatusSucceeded (stale failure from the earlier retry must be cleared)", got.Error)
	}
}

// TestCompleteSucceeded_NoOpWhenNotRunning proves the concurrent-Cancel
// guard Queue.Cancel's own doc comment describes: a completion write that
// targets a row no longer StatusRunning (because Cancel already moved it
// to StatusCancelled) must not resurrect it as StatusSucceeded.
func TestCompleteSucceeded_NoOpWhenNotRunning(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now()); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	if err := markCancelled(context.Background(), db, rec.ID, time.Now()); err != nil {
		t.Fatalf("markCancelled() error = %v", err)
	}

	if err := completeSucceeded(context.Background(), db, rec.ID, Result{Data: []byte("done")}, time.Now()); err != nil {
		t.Fatalf("completeSucceeded() error = %v, want nil (no-op, not an error)", err)
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Status != string(StatusCancelled) {
		t.Errorf("Status = %q, want %q (a completion write must not overwrite a cancellation)", got.Status, StatusCancelled)
	}
}

func TestCompleteRetrying(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now()); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	next := time.Now().Add(10 * time.Second).UTC().Truncate(time.Second)
	if err := completeRetrying(context.Background(), db, rec.ID, "transient failure", next, time.Now()); err != nil {
		t.Fatalf("completeRetrying() error = %v", err)
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Status != string(StatusRetrying) {
		t.Errorf("Status = %q, want %q", got.Status, StatusRetrying)
	}
	if got.Error != "transient failure" {
		t.Errorf("Error = %q, want %q", got.Error, "transient failure")
	}
	if !got.ScheduledAt.Equal(next) {
		t.Errorf("ScheduledAt = %v, want %v", got.ScheduledAt, next)
	}
}

func TestCompleteDeadLetter(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now()); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	moved, err := completeDeadLetter(context.Background(), db, rec.ID, "permanent failure", time.Now())
	if err != nil {
		t.Fatalf("completeDeadLetter() error = %v", err)
	}
	if !moved {
		t.Error("completeDeadLetter() moved = false, want true (the row was StatusRunning)")
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Status != string(StatusDeadLetter) {
		t.Errorf("Status = %q, want %q", got.Status, StatusDeadLetter)
	}
	if got.Error != "permanent failure" {
		t.Errorf("Error = %q, want %q", got.Error, "permanent failure")
	}
	if got.CompletedAt == nil {
		t.Error("CompletedAt = nil, want set")
	}
}

// TestCompleteDeadLetter_NoTransitionWhenAlreadyCancelled is the
// store-level half of the concurrent-Cancel race completeDeadLetter's
// (bool, error) report exists to surface: a row a Cancel already moved to
// StatusCancelled while the final attempt was still executing must report
// moved == false with a nil error (a no-op, exactly like
// completeSucceeded's own guard), must never overwrite StatusCancelled,
// and must not record its cause -- so execute (worker.go) can tell "this
// attempt really dead-lettered the Job" from "a Cancel already settled
// it" and skip OnFailure for the latter. See worker_test.go's
// TestExecute_FinalFailureAfterCancel_DoesNotRunOnFailure for the OnFailure
// half of the same race.
func TestCompleteDeadLetter_NoTransitionWhenAlreadyCancelled(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now()); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	if err := markCancelled(context.Background(), db, rec.ID, time.Now()); err != nil {
		t.Fatalf("markCancelled() error = %v", err)
	}

	moved, err := completeDeadLetter(context.Background(), db, rec.ID, "permanent failure", time.Now())
	if err != nil {
		t.Fatalf("completeDeadLetter() error = %v, want nil (no-op, not an error)", err)
	}
	if moved {
		t.Error("completeDeadLetter() moved = true, want false (a Cancel already moved the row out of StatusRunning)")
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Status != string(StatusCancelled) {
		t.Errorf("Status = %q, want %q (a dead-letter write must not overwrite a cancellation)", got.Status, StatusCancelled)
	}
	if got.Error != "" {
		t.Errorf("Error = %q, want empty (a no-op dead-letter write must not record its cause)", got.Error)
	}
}

func TestMarkCancelled_FromPendingAndRunning(t *testing.T) {
	db := newTestDB(t)

	pending := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, pending); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if err := markCancelled(context.Background(), db, pending.ID, time.Now()); err != nil {
		t.Fatalf("markCancelled(pending) error = %v", err)
	}
	got, err := findByID(context.Background(), db, JobID(pending.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Status != string(StatusCancelled) {
		t.Errorf("Status = %q, want %q", got.Status, StatusCancelled)
	}

	running := fixtureRecord("tenant-a", "t")
	_, err = insertRecord(context.Background(), db, running)
	if err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	_, err = claimOne(context.Background(), db, *running, time.Now())
	if err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	err = markCancelled(context.Background(), db, running.ID, time.Now())
	if err != nil {
		t.Fatalf("markCancelled(running) error = %v", err)
	}
	got, err = findByID(context.Background(), db, JobID(running.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Status != string(StatusCancelled) {
		t.Errorf("Status = %q, want %q", got.Status, StatusCancelled)
	}
}

func TestMarkCancelled_IdempotentOnAlreadyTerminal(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now()); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	if err := completeSucceeded(context.Background(), db, rec.ID, Result{}, time.Now()); err != nil {
		t.Fatalf("completeSucceeded() error = %v", err)
	}

	if err := markCancelled(context.Background(), db, rec.ID, time.Now()); err != nil {
		t.Fatalf("markCancelled(already succeeded) error = %v, want nil (idempotent no-op)", err)
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Status != string(StatusSucceeded) {
		t.Errorf("Status = %q, want %q (Cancel must not un-succeed a terminal Job)", got.Status, StatusSucceeded)
	}
}

func TestResetInterruptedRecords(t *testing.T) {
	db := newTestDB(t)

	running := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, running); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *running, time.Now()); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	pending := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, pending); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}

	if err := resetInterruptedRecords(context.Background(), db, time.Now()); err != nil {
		t.Fatalf("resetInterruptedRecords() error = %v", err)
	}

	gotRunning, err := findByID(context.Background(), db, JobID(running.ID))
	if err != nil {
		t.Fatalf("findByID(running) error = %v", err)
	}
	if gotRunning.Status != string(StatusPending) {
		t.Errorf("previously-running Status = %q, want %q (recovered after unclean exit)", gotRunning.Status, StatusPending)
	}
	if gotRunning.Attempts != 1 {
		t.Errorf("previously-running Attempts = %d, want 1 (recovery must not grant a free extra attempt)", gotRunning.Attempts)
	}

	gotPending, err := findByID(context.Background(), db, JobID(pending.ID))
	if err != nil {
		t.Fatalf("findByID(pending) error = %v", err)
	}
	if gotPending.Status != string(StatusPending) {
		t.Errorf("already-pending Status = %q, want unchanged %q", gotPending.Status, StatusPending)
	}
}

func TestDeadLetterRecords(t *testing.T) {
	db := newTestDB(t)

	dead := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, dead); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *dead, time.Now()); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	if moved, err := completeDeadLetter(context.Background(), db, dead.ID, "boom", time.Now()); err != nil {
		t.Fatalf("completeDeadLetter() error = %v", err)
	} else if !moved {
		t.Error("completeDeadLetter() moved = false, want true (the row was StatusRunning)")
	}

	alive := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, alive); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}

	got, err := deadLetterRecords(context.Background(), db)
	if err != nil {
		t.Fatalf("deadLetterRecords() error = %v", err)
	}
	if len(got) != 1 || got[0].ID != dead.ID {
		t.Errorf("deadLetterRecords() = %v, want exactly [%q]", got, dead.ID)
	}
}

func TestQueueDepthByTypeAndStatus(t *testing.T) {
	db := newTestDB(t)

	pendingA := fixtureRecord("tenant-a", "type-a")
	pendingA2 := fixtureRecord("tenant-b", "type-a")
	retryingA := fixtureRecord("tenant-a", "type-a")
	retryingA.Status = string(StatusRetrying)
	pendingB := fixtureRecord("tenant-a", "type-b")
	succeededA := fixtureRecord("tenant-a", "type-a")
	succeededA.Status = string(StatusSucceeded)

	for _, r := range []*jobRecord{pendingA, pendingA2, retryingA, pendingB, succeededA} {
		if _, err := insertRecord(context.Background(), db, r); err != nil {
			t.Fatalf("insertRecord() error = %v", err)
		}
	}

	counts, err := queueDepthByTypeAndStatus(context.Background(), db)
	if err != nil {
		t.Fatalf("queueDepthByTypeAndStatus() error = %v", err)
	}

	byKey := make(map[string]int64, len(counts))
	for _, c := range counts {
		byKey[c.Type+"|"+c.Status] = c.Count
	}

	if got := byKey["type-a|"+string(StatusPending)]; got != 2 {
		t.Errorf("type-a pending count = %d, want 2 (across both tenants -- this metric is not tenant-scoped)", got)
	}
	if got := byKey["type-a|"+string(StatusRetrying)]; got != 1 {
		t.Errorf("type-a retrying count = %d, want 1", got)
	}
	if got := byKey["type-b|"+string(StatusPending)]; got != 1 {
		t.Errorf("type-b pending count = %d, want 1", got)
	}
	if _, ok := byKey["type-a|"+string(StatusSucceeded)]; ok {
		t.Errorf("succeeded Jobs must not appear in the backlog-depth gauge, got an entry for type-a|succeeded")
	}
}
