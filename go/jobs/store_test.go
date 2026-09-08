package jobs

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/tenancy/tenancytest"
)

// newTestDB returns a *gorm.DB backed by a private, per-test temp-file
// SQLite database (dbkit/dbtest) with the jobs schema already applied. The
// package's own tests exercise SQLite only; the PostgreSQL dialect of the
// schema is covered by the integration tier.
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

// testWriterOwner is the writer-registration token store-level tests claim
// rows under, standing in for the per-queue token NewStandaloneQueue
// generates (newWriterOwner). Distinct per test is unnecessary: every test
// uses its own database.
const testWriterOwner = "test-owner"

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

// TestEnsureJobsSchema_UpgradesLegacyTableWithoutClaimedBy proves the
// in-place column add that keeps a jobs table created by a pre-claim_by
// release bootable: a table carrying the pre-existing shape (no
// claimed_by column) must come out of ensureJobsSchema with the column
// present and every pre-existing row intact and reset-table-compatible
// (claimed_by defaulting to the empty string the reset's WHERE clause
// treats as another writer's).
func TestEnsureJobsSchema_UpgradesLegacyTableWithoutClaimedBy(t *testing.T) {
	db := dbtest.NewSQLite(t)
	legacy := `CREATE TABLE ` + jobsTable + ` (
		id              VARCHAR(36) NOT NULL PRIMARY KEY,
		type            VARCHAR(255) NOT NULL,
		tenant_id       VARCHAR(64) NOT NULL,
		payload         BLOB,
		idempotency_key VARCHAR(255) NOT NULL DEFAULT '',
		status          VARCHAR(32) NOT NULL,
		priority        INTEGER NOT NULL,
		progress_pct    INTEGER NOT NULL DEFAULT 0,
		progress_msg    VARCHAR(1000) NOT NULL DEFAULT '',
		result          BLOB,
		error_message   VARCHAR(4000) NOT NULL DEFAULT '',
		attempts        INTEGER NOT NULL DEFAULT 0,
		max_retries     INTEGER NOT NULL,
		timeout_nanos   BIGINT NOT NULL,
		scheduled_at    TIMESTAMP NOT NULL,
		created_at      TIMESTAMP NOT NULL,
		updated_at      TIMESTAMP NOT NULL,
		started_at      TIMESTAMP,
		completed_at    TIMESTAMP
	)`
	if err := db.Exec(legacy).Error; err != nil {
		t.Fatalf("create legacy jobs table: %v", err)
	}
	// Inserted through an explicit legacy column list (no claimed_by --
	// gorm would add the column the model now carries).
	if err := db.Exec(`INSERT INTO `+jobsTable+` (id, type, tenant_id, status, priority, max_retries, timeout_nanos, scheduled_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"legacy-1", "legacy.type", "tenant-a", string(StatusPending), int(PriorityNormal), DefaultMaxRetries, int64(DefaultTimeout),
		time.Now(), time.Now(), time.Now()).Error; err != nil {
		t.Fatalf("insert into legacy table: %v", err)
	}

	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("ensureJobsSchema() over a legacy table error = %v", err)
	}
	// Idempotent over the upgraded table too.
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("second ensureJobsSchema() over the upgraded table error = %v", err)
	}

	got, err := findByID(context.Background(), db, JobID("legacy-1"))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.ClaimedBy != "" {
		t.Errorf("ClaimedBy of a pre-existing row = %q, want empty (the legacy default the reset's WHERE treats as another writer's)", got.ClaimedBy)
	}
	// A claim against the upgraded table works end to end.
	if _, err := claimOne(context.Background(), db, *got, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() against the upgraded table error = %v", err)
	}
}

// TestEnsureJobsSchema_UpgradesLegacyQueueWritersTableWithoutStaleAt is the
// queue_writers twin of the upgrade test above: a queue_writers table
// created by a release predating the stale_at column must come out of
// ensureJobsSchema with the column present, its pre-existing row intact and
// carrying NULL stale_at -- the marker acquireWriterRegistration reads as
// "written by a pre-column release" -- and the legacy-row takeover must
// then work against the upgraded table end to end.
func TestEnsureJobsSchema_UpgradesLegacyQueueWritersTableWithoutStaleAt(t *testing.T) {
	db := dbtest.NewSQLite(t)
	legacy := `CREATE TABLE ` + queueWritersTable + ` (
		id             INTEGER NOT NULL PRIMARY KEY,
		owner          VARCHAR(64) NOT NULL,
		last_heartbeat TIMESTAMP NOT NULL
	)`
	if err := db.Exec(legacy).Error; err != nil {
		t.Fatalf("create legacy queue_writers table: %v", err)
	}
	// A row exactly as a crashed pre-column writer left it.
	if err := db.Exec(`INSERT INTO `+queueWritersTable+` (id, owner, last_heartbeat) VALUES (1, 'legacy-writer', ?)`, time.Now().Add(-10*time.Second)).Error; err != nil {
		t.Fatalf("insert into legacy queue_writers table: %v", err)
	}

	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("ensureJobsSchema() over a legacy queue_writers table error = %v", err)
	}
	// Idempotent over the upgraded table too.
	if err := ensureJobsSchema(context.Background(), db); err != nil {
		t.Fatalf("second ensureJobsSchema() over the upgraded table error = %v", err)
	}

	got := struct {
		Owner    string     `gorm:"column:owner"`
		StaleAt  *time.Time `gorm:"column:stale_at"`
		LastBeat time.Time  `gorm:"column:last_heartbeat"`
	}{}
	if err := db.Raw(`SELECT owner, last_heartbeat, stale_at FROM ` + queueWritersTable + ` WHERE id = 1`).Scan(&got).Error; err != nil {
		t.Fatalf("read back upgraded queue_writers row: %v", err)
	}
	if got.Owner != "legacy-writer" {
		t.Errorf("owner = %q, want %q (the upgrade must leave the pre-existing row intact)", got.Owner, "legacy-writer")
	}
	if got.StaleAt != nil {
		t.Errorf("stale_at = %v, want nil (a pre-column row must keep the NULL marker, never a made-up stale moment)", got.StaleAt)
	}

	// The legacy-row takeover works against the upgraded table: refused
	// while the row's last heartbeat is younger than the conservative
	// window...
	if err := acquireWriterRegistration(context.Background(), db, testWriterOwner, time.Now(), 2*time.Second); !errors.Is(err, ErrQueueWriterActive) {
		t.Fatalf("acquire against the upgraded legacy row error = %v, want ErrQueueWriterActive", err)
	}
	// ...taken over once it is older (age the row the way time does).
	if err := db.Exec(`UPDATE `+queueWritersTable+` SET last_heartbeat = ? WHERE id = 1`, time.Now().Add(-2*legacyRegistrationStaleAfter)).Error; err != nil {
		t.Fatalf("age the legacy row: %v", err)
	}
	if err := acquireWriterRegistration(context.Background(), db, testWriterOwner, time.Now(), 2*time.Second); err != nil {
		t.Fatalf("acquire past the aged legacy row error = %v, want nil (the takeover is the crash recovery)", err)
	}
	// The taken-over row now carries a real stale moment: a live sibling is
	// refused again.
	if err := acquireWriterRegistration(context.Background(), db, "other-owner", time.Now(), 2*time.Second); !errors.Is(err, ErrQueueWriterActive) {
		t.Fatalf("acquire against the taken-over row error = %v, want ErrQueueWriterActive", err)
	}
}

// TestJobRecord_NotTenantScoped is the mandatory isolation-assertion suite
// for platform data: jobRecord must NOT be affected by dbkit's
// tenant-scoping plugin, since the dispatcher scans eligible Jobs across
// every tenant at once. See jobRecord's own doc comment for the full design
// rationale.
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
// SQLite serializes writers on one file even under the bounded busy_timeout
// every dbkit SQLite connection carries: what this test pins
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

func TestClaimOne_ClaimsUnderOwner_LeavesAttemptCountingToTheWorkerHandoff(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}

	claimed, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner)
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
	if got.ClaimedBy != testWriterOwner {
		t.Errorf("ClaimedBy after claim = %q, want %q (the claiming queue's writer token)", got.ClaimedBy, testWriterOwner)
	}
	// The claim is only the first half: counting the attempt and stamping
	// its StartedAt belong to the worker handoff (markAttemptStarted), so
	// Get() between the claim and the handoff reports a Job that is claimed
	// but has not started -- Attempts not yet including an attempt no
	// Handle has made, StartedAt still nil.
	if got.Attempts != 0 {
		t.Errorf("Attempts after claim = %d, want 0 (an attempt is counted at the worker handoff, not at the claim)", got.Attempts)
	}
	if got.StartedAt != nil {
		t.Errorf("StartedAt after claim = %v, want nil (the attempt has not started yet)", got.StartedAt)
	}
}

func TestMarkAttemptStarted_CountsAttemptAndStampsStartedAt(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	startedAt := time.Now()
	if err := markAttemptStarted(context.Background(), db, rec.ID, startedAt); err != nil {
		t.Fatalf("markAttemptStarted() error = %v", err)
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Attempts != 1 {
		t.Errorf("Attempts after handoff = %d, want 1", got.Attempts)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(startedAt) {
		t.Errorf("StartedAt after handoff = %v, want %v", got.StartedAt, startedAt)
	}

	// A second handoff is the next attempt: another increment, another
	// start stamp -- the row is claimed again between attempts.
	if _, err := claimOne(context.Background(), db, *got, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("second claimOne() error = %v", err)
	}
	secondStart := time.Now()
	if startErr := markAttemptStarted(context.Background(), db, rec.ID, secondStart); startErr != nil {
		t.Fatalf("second markAttemptStarted() error = %v", startErr)
	}
	got2, findErr := findByID(context.Background(), db, JobID(rec.ID))
	if findErr != nil {
		t.Fatalf("findByID() error = %v", findErr)
	}
	if got2.Attempts != 2 {
		t.Errorf("Attempts after second handoff = %d, want 2", got2.Attempts)
	}
}

func TestMarkAttemptStarted_NoOpWhenNoLongerRunning(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	// A concurrent Cancel settles the row between the claim and the worker
	// handoff -- the same race markAttemptStarted's status guard exists for:
	// the attempt still executes (Cancel lets a claimed Job run), but its
	// count must not land on a cancelled row.
	if err := markCancelled(context.Background(), db, rec.ID, time.Now()); err != nil {
		t.Fatalf("markCancelled() error = %v", err)
	}
	if err := markAttemptStarted(context.Background(), db, rec.ID, time.Now()); err != nil {
		t.Fatalf("markAttemptStarted() error = %v, want nil (no-op, not an error)", err)
	}
	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0 (the handoff write must not count an attempt on a row a Cancel already settled)", got.Attempts)
	}
	if got.Status != string(StatusCancelled) {
		t.Errorf("Status = %q, want %q", got.Status, StatusCancelled)
	}
}

func TestClaimOne_StaleSnapshotLosesTheRace(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	staleSnapshot := *rec // status: pending, as read before any claim.

	firstClaimed, err := claimOne(context.Background(), db, staleSnapshot, time.Now(), testWriterOwner)
	if err != nil {
		t.Fatalf("first claimOne() error = %v", err)
	}
	if !firstClaimed {
		t.Fatalf("first claimOne() = false, want true")
	}

	secondClaimed, err := claimOne(context.Background(), db, staleSnapshot, time.Now(), testWriterOwner)
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
	if got.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0 (claims never count attempts -- the worker handoff does -- so a lost claim has nothing to double-count)", got.Attempts)
	}
}

func TestUpdateProgress(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	// A progress report only ever comes from a running attempt, so the row
	// must be claimed first -- updateProgress's status = 'running' guard
	// no-ops a report aimed anywhere else, exactly like its terminal-write
	// siblings.
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	if err := updateProgress(context.Background(), db, testWriterOwner, rec.ID, 42, "working"); err != nil {
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

// TestUpdateProgress_AfterTerminalState_DoesNotTouchRow is the regression
// for the missing status guard: a progress report arriving after the row
// left StatusRunning (here: a concurrent Cancel settled it) must no-op --
// never land on the terminal-state row, overwrite its progress fields and
// push its updated_at with a stale "I am working" stamp. Fails on the
// unguarded write, which updates the cancelled row regardless.
func TestUpdateProgress_AfterTerminalState_DoesNotTouchRow(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	cancelAt := time.Now()
	if err := markCancelled(context.Background(), db, rec.ID, cancelAt); err != nil {
		t.Fatalf("markCancelled() error = %v", err)
	}
	before, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if before.Status != string(StatusCancelled) {
		t.Fatalf("Status = %q, want %q (setup: the row must be terminal before the late report)", before.Status, StatusCancelled)
	}

	// The late progress report from an attempt that raced the Cancel.
	if perr := updateProgress(context.Background(), db, testWriterOwner, rec.ID, 42, "still working"); perr != nil {
		t.Fatalf("updateProgress() error = %v (a no-op write is success, not an error)", perr)
	}

	after, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if after.Status != string(StatusCancelled) {
		t.Errorf("Status = %q, want %q (the report must not resurrect or retransition the row)", after.Status, StatusCancelled)
	}
	if after.ProgressPct != 0 || after.ProgressMsg != "" {
		t.Errorf("progress = (%d, %q), want (0, %q) (a report landing after a terminal state must not overwrite the row's progress fields)", after.ProgressPct, after.ProgressMsg, "")
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("UpdatedAt moved from %v to %v (a report landing after a terminal state must not push updated_at)", before.UpdatedAt, after.UpdatedAt)
	}
}

func TestCompleteSucceeded(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	moved, err := completeSucceeded(context.Background(), db, testWriterOwner, rec.ID, Result{Data: []byte("done")}, time.Now())
	if err != nil || !moved {
		t.Fatalf("completeSucceeded() = (%v, %v), want (true, nil)", moved, err)
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
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	if _, err := completeRetrying(context.Background(), db, testWriterOwner, rec.ID, "first attempt failed", time.Now(), time.Now()); err != nil {
		t.Fatalf("completeRetrying() error = %v", err)
	}

	retried, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	_, err = claimOne(context.Background(), db, *retried, time.Now(), testWriterOwner)
	if err != nil {
		t.Fatalf("second claimOne() error = %v", err)
	}
	if moved, succErr := completeSucceeded(context.Background(), db, testWriterOwner, rec.ID, Result{Data: []byte("done")}, time.Now()); succErr != nil || !moved {
		t.Fatalf("completeSucceeded() = (%v, %v), want (true, nil)", moved, succErr)
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
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	if err := markCancelled(context.Background(), db, rec.ID, time.Now()); err != nil {
		t.Fatalf("markCancelled() error = %v", err)
	}

	moved, err := completeSucceeded(context.Background(), db, testWriterOwner, rec.ID, Result{Data: []byte("done")}, time.Now())
	if err != nil {
		t.Fatalf("completeSucceeded() error = %v, want nil (no-op, not an error)", err)
	}
	if moved {
		t.Fatal("completeSucceeded() = true, want false: the row is StatusCancelled, not StatusRunning, so the write must report no transition")
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Status != string(StatusCancelled) {
		t.Errorf("Status = %q, want %q (a completion write must not overwrite a cancellation)", got.Status, StatusCancelled)
	}
}

// TestCompleteSucceeded_NoOpWhenRowClaimedByAnotherWriter is the ownership
// half of the completion guard: a row that is StatusRunning under ANOTHER
// writer's claim — the state a second writer's reset-and-re-claim of the
// first writer's mid-Handle row leaves behind — must not be settled by the
// first writer's completion write. A completion write whose WHERE named
// only id and status would let a stale execution's success land on the
// sibling's running row while the sibling is still inside Handle -- the
// double-execution harm the status-guarded no-op prevents -- and it must
// never be read as a concurrent Cancel (see worker.go's
// logDiscardedOutcome).
func TestCompleteSucceeded_NoOpWhenRowClaimedByAnotherWriter(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	// The row is running — but under a claim this completion does not own.
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), "other-owner"); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	moved, err := completeSucceeded(context.Background(), db, testWriterOwner, rec.ID, Result{Data: []byte("stale success")}, time.Now())
	if err != nil {
		t.Fatalf("completeSucceeded() error = %v, want nil (no-op, not an error)", err)
	}
	if moved {
		t.Fatal("completeSucceeded() = true, want false: the row runs under another writer's claim, so this writer's completion must report no transition")
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Status != string(StatusRunning) {
		t.Errorf("Status = %q, want %q (a stale completion must not settle a row another writer is executing)", got.Status, StatusRunning)
	}
	if got.ClaimedBy != "other-owner" {
		t.Errorf("ClaimedBy = %q, want %q (the row must stay under the executing writer's claim)", got.ClaimedBy, "other-owner")
	}
	if len(got.Result) != 0 {
		t.Errorf("Result = %q, want empty (a stale completion must not write its result into another writer's row)", got.Result)
	}
}

func TestCompleteRetrying(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	next := time.Now().Add(10 * time.Second).UTC().Truncate(time.Second)
	if moved, err := completeRetrying(context.Background(), db, testWriterOwner, rec.ID, "transient failure", next, time.Now()); err != nil || !moved {
		t.Fatalf("completeRetrying() = (%v, %v), want (true, nil)", moved, err)
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
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	moved, err := completeDeadLetter(context.Background(), db, testWriterOwner, rec.ID, "permanent failure", time.Now())
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
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	if err := markCancelled(context.Background(), db, rec.ID, time.Now()); err != nil {
		t.Fatalf("markCancelled() error = %v", err)
	}

	moved, err := completeDeadLetter(context.Background(), db, testWriterOwner, rec.ID, "permanent failure", time.Now())
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
	_, err = claimOne(context.Background(), db, *running, time.Now(), testWriterOwner)
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
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	if moved, err := completeSucceeded(context.Background(), db, testWriterOwner, rec.ID, Result{}, time.Now()); err != nil || !moved {
		t.Fatalf("completeSucceeded() = (%v, %v), want (true, nil)", moved, err)
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

	// A row a worker was genuinely mid-Handle on when the process died: the
	// handoff ran (markAttemptStarted), so the attempt is counted -- the
	// recovery must not grant an extra attempt beyond MaxRetries. Claimed
	// under the crashed writer's own token -- the fresh successor's reset
	// below runs under testWriterOwner, a token no row carries.
	running := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, running); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *running, time.Now(), "crashed-writer"); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	if err := markAttemptStarted(context.Background(), db, running.ID, time.Now()); err != nil {
		t.Fatalf("markAttemptStarted() error = %v", err)
	}

	// A row only CLAIMED when the process died -- the worker handoff never
	// ran, so no attempt was ever started and none may be counted by the
	// recovery (see claimOne's two-phase doc comment).
	claimedOnly := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, claimedOnly); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *claimedOnly, time.Now(), "crashed-writer"); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	// A row claimed by a DIFFERENT (already dead) writer -- the situation a
	// fresh Start after a crash always faces -- plus a row that never was
	// claimed at all: both must come out of the reset untouched except for
	// the intended recovery.
	pending := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, pending); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	otherWriter := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, otherWriter); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *otherWriter, time.Now(), "other-owner"); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	// The fresh writer's token: brand new, so no row can carry it -- every
	// StatusRunning row above was claimed by someone else (or nobody) and
	// is this Start's to recover.
	if err := resetInterruptedRecords(context.Background(), db, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("resetInterruptedRecords() error = %v", err)
	}

	gotRunning, err := findByID(context.Background(), db, JobID(running.ID))
	if err != nil {
		t.Fatalf("findByID(running) error = %v", err)
	}
	if gotRunning.Status != string(StatusPending) {
		t.Errorf("mid-Handle Status = %q, want %q (recovered after unclean exit)", gotRunning.Status, StatusPending)
	}
	if gotRunning.Attempts != 1 {
		t.Errorf("mid-Handle Attempts = %d, want 1 (the started attempt stays counted; recovery must not grant a free extra attempt)", gotRunning.Attempts)
	}

	gotClaimedOnly, err := findByID(context.Background(), db, JobID(claimedOnly.ID))
	if err != nil {
		t.Fatalf("findByID(claimedOnly) error = %v", err)
	}
	if gotClaimedOnly.Status != string(StatusPending) {
		t.Errorf("claimed-only Status = %q, want %q (recovered after unclean exit)", gotClaimedOnly.Status, StatusPending)
	}
	if gotClaimedOnly.Attempts != 0 {
		t.Errorf("claimed-only Attempts = %d, want 0 (an attempt whose Handle never ran must not be counted by the recovery)", gotClaimedOnly.Attempts)
	}

	gotOther, err := findByID(context.Background(), db, JobID(otherWriter.ID))
	if err != nil {
		t.Fatalf("findByID(otherWriter) error = %v", err)
	}
	if gotOther.Status != string(StatusPending) {
		t.Errorf("other-writer Status = %q, want %q (a dead writer's rows are this Start's to recover)", gotOther.Status, StatusPending)
	}

	gotPending, err := findByID(context.Background(), db, JobID(pending.ID))
	if err != nil {
		t.Fatalf("findByID(pending) error = %v", err)
	}
	if gotPending.Status != string(StatusPending) {
		t.Errorf("already-pending Status = %q, want unchanged %q", gotPending.Status, StatusPending)
	}
}

// TestResetInterruptedRecords_OwnFreshClaimIsLeftAlone pins the WHERE
// clause's defensive tail: a Running row already claimed under the
// resetting owner's own token is not this reset's business. Not reachable
// through StandaloneQueue.Start (a fresh token claims nothing before the
// reset runs -- see resetInterruptedRecords' own doc comment), but the
// clause exists exactly so a re-entrant Start over a live run could never
// reset the very rows it is executing.
func TestResetInterruptedRecords_OwnFreshClaimIsLeftAlone(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	if err := resetInterruptedRecords(context.Background(), db, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("resetInterruptedRecords() error = %v", err)
	}
	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Status != string(StatusRunning) {
		t.Errorf("Status = %q, want %q (the reset must never touch a row claimed under its own owner token)", got.Status, StatusRunning)
	}
}

// TestWriterRegistration_AcquireHeartbeatRelease covers the single-writer
// registration lifecycle (queue_writers table): a fresh acquire succeeds, a
// second acquire while the incumbent's registration is fresh is refused with
// ErrQueueWriterActive, the incumbent's heartbeat keeps its own row fresh —
// refreshing the stale moment the row carries — a beat that names the wrong
// owner reaches nothing, an acquire after the incumbent's own stale moment
// has passed steals the crashed incumbent's registration, and release hands
// the table back so the next acquire succeeds immediately. The judgment
// datum throughout is the stale moment the INCUMBENT authored (its own
// stale window applied to its own last beat), never the acquiring side's
// window: "zero stale window makes the incumbent stale immediately" — the
// semantics that would let a fast-cadence taker steal a live beating
// incumbent — is pinned absent here.
func TestWriterRegistration_AcquireHeartbeatRelease(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	const stale = 2 * time.Second
	now := time.Now()
	if err := acquireWriterRegistration(ctx, db, testWriterOwner, now, stale); err != nil {
		t.Fatalf("first acquireWriterRegistration() error = %v", err)
	}
	// A second acquire with a fresh registration: refused, coded.
	err := acquireWriterRegistration(ctx, db, "other-owner", now.Add(100*time.Millisecond), stale)
	if !errors.Is(err, ErrQueueWriterActive) {
		t.Fatalf("second acquireWriterRegistration() error = %v, want ErrQueueWriterActive", err)
	}
	if appErr, ok := apperr.As(err); !ok || appErr.Code != ErrQueueWriterActive.Code {
		t.Fatalf("second acquireWriterRegistration() error = %v, want code %q", err, ErrQueueWriterActive.Code)
	}

	// Heartbeat: only the registered owner's own beat reaches the row, and
	// it refreshes the row's stale moment (now + the owner's own window).
	ok, err := heartbeatWriterRegistration(ctx, db, "other-owner", now.Add(500*time.Millisecond), stale)
	if err != nil {
		t.Fatalf("heartbeatWriterRegistration(other) error = %v", err)
	}
	if ok {
		t.Fatal("heartbeatWriterRegistration(other) = true, want false (only the registered owner may refresh its own row)")
	}
	ok, err = heartbeatWriterRegistration(ctx, db, testWriterOwner, now.Add(500*time.Millisecond), stale)
	if err != nil {
		t.Fatalf("heartbeatWriterRegistration(owner) error = %v", err)
	}
	if !ok {
		t.Fatal("heartbeatWriterRegistration(owner) = false, want true")
	}

	// Still refused while the owner's authored stale moment (now + 500ms +
	// 2s = now + 2.5s, refreshed by the beat above) has not passed...
	err = acquireWriterRegistration(ctx, db, "successor", now.Add(1*time.Second), stale)
	if !errors.Is(err, ErrQueueWriterActive) {
		t.Fatalf("acquire against a beating owner error = %v, want ErrQueueWriterActive", err)
	}
	// ...and STILL refused just before that moment with a ZERO stale window
	// on the acquiring side: the taker's own window must never judge the
	// incumbent. A "zero stale window makes the incumbent stale
	// immediately" reading would let a fast-cadence taker steal a live
	// incumbent beating at its own slower cadence — the double-execution
	// hazard this test pins closed.
	err = acquireWriterRegistration(ctx, db, "successor", now.Add(2*time.Second+400*time.Millisecond), 0)
	if !errors.Is(err, ErrQueueWriterActive) {
		t.Fatalf("acquire before the incumbent's own stale moment error = %v, want ErrQueueWriterActive (the taker's own window must not judge the incumbent)", err)
	}
	// ...and stolen only once its own stale moment (now + 2.5s) has passed:
	// the crashed writer's registration is taken over, exactly what a
	// restart does.
	if err := acquireWriterRegistration(ctx, db, "successor", now.Add(3*time.Second), 0); err != nil {
		t.Fatalf("acquire past the incumbent's stale moment error = %v, want nil (the crashed incumbent's registration is stolen)", err)
	}

	// Release hands the table back: the successor (now the registered
	// owner) releases, and a fresh acquire succeeds immediately.
	if err := releaseWriterRegistration(ctx, db, "successor"); err != nil {
		t.Fatalf("releaseWriterRegistration(successor) error = %v", err)
	}
	if err := acquireWriterRegistration(ctx, db, "third", time.Now(), stale); err != nil {
		t.Fatalf("acquire after release error = %v, want nil (release is the graceful handover)", err)
	}
}

// TestWriterRegistration_LegacyRowWithoutStaleMoment pins the one judgment
// a self-authored stale moment cannot serve: a registration row written by
// a release that predated the stale_at column carries stale_at NULL — the
// owner cadence that would let anyone convert its liveness into a stale
// moment is unknowable (its writer's code is gone) — so the acquiring side
// judges such a row by the conservative fixed window
// (legacyRegistrationStaleAfter) applied to its last heartbeat instead:
// refused while the row's heartbeat is younger than the window, taken over
// once it is older. The row is seeded with NULL stale_at exactly as the
// schema migration leaves a pre-column row.
func TestWriterRegistration_LegacyRowWithoutStaleMoment(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	now := time.Now()
	seed := func(heartbeat time.Time) {
		if err := db.WithContext(ctx).Exec(`INSERT INTO `+queueWritersTable+` (id, owner, last_heartbeat, stale_at) VALUES (1, 'legacy-writer', ?, NULL)`, heartbeat).Error; err != nil {
			t.Fatalf("seed legacy queue_writers row: %v", err)
		}
	}
	remove := func() {
		if err := db.WithContext(ctx).Exec(`DELETE FROM ` + queueWritersTable + ` WHERE id = 1`).Error; err != nil {
			t.Fatalf("remove queue_writers row: %v", err)
		}
	}

	// A legacy row whose last heartbeat is fresh (its pre-column writer is
	// live, beating at a cadence this side cannot know): refused.
	seed(now.Add(-10 * time.Second))
	err := acquireWriterRegistration(ctx, db, testWriterOwner, now, 2*time.Second)
	if !errors.Is(err, ErrQueueWriterActive) {
		t.Fatalf("acquire against a fresh legacy row error = %v, want ErrQueueWriterActive", err)
	}
	remove()

	// A legacy row whose last heartbeat is older than the conservative
	// window (its pre-column writer is long dead): taken over.
	seed(now.Add(-2 * legacyRegistrationStaleAfter))
	if err := acquireWriterRegistration(ctx, db, testWriterOwner, now, 2*time.Second); err != nil {
		t.Fatalf("acquire past a dead legacy row's heartbeat error = %v, want nil (the takeover is the crash recovery)", err)
	}
	got := struct {
		Owner string `gorm:"column:owner"`
	}{}
	if err := db.WithContext(ctx).Raw(`SELECT owner FROM ` + queueWritersTable + ` WHERE id = 1`).Scan(&got).Error; err != nil {
		t.Fatalf("read queue_writers row: %v", err)
	}
	if got.Owner != testWriterOwner {
		t.Errorf("queue_writers owner = %q, want %q (the legacy row's takeover must write the new owner and a real stale moment)", got.Owner, testWriterOwner)
	}
}

func TestDeadLetterRecords(t *testing.T) {
	db := newTestDB(t)

	dead := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, dead); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *dead, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}
	if moved, err := completeDeadLetter(context.Background(), db, testWriterOwner, dead.ID, "boom", time.Now()); err != nil {
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

// The three tests below pin the write-side choke point that keeps the two
// descriptive text columns of the jobs table fed only values that fit
// their declared widths: error_message is VARCHAR(4000) and progress_msg
// is VARCHAR(1000), spelled identically in both of store.go's
// createJobsTableSQL statements. Those widths are character counts that
// PostgreSQL enforces (an overlong value makes the UPDATE fail with
// 22001, the task never reaches its terminal state, and its recovery
// needs a process restart that re-runs the same overlong failure) and
// SQLite never enforces -- which is exactly why these tests assert the
// CUT value read back after the write: the cut is application-layer
// behaviour, identical on both dialects, while the database's refusal
// (and the running-state wedge it leaves behind) exists only on
// PostgreSQL and is pinned by the integration leg
// (integration_test/postgres_overlong_message_test.go). On SQLite an
// uncut value would store happily, so the width assertion here would
// fail; and each test's structured-warning assertion fails if a cut ever
// stops warning.
func TestCompleteDeadLetter_OverlongCause_TruncatedToColumnWidth(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	prevDefault := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	// The recorded cause is unbounded handler error text (worker.go's
	// settleFailedAttempt stores cause.Error()): 1500 characters past the
	// column's 4000-character width.
	longCause := strings.Repeat("x", 5500)
	moved, err := completeDeadLetter(context.Background(), db, testWriterOwner, rec.ID, longCause, time.Now())
	if err != nil || !moved {
		t.Fatalf("completeDeadLetter() = (%v, %v), want (true, nil): the terminal transition must never be refused because the cause is overlong", moved, err)
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Status != string(StatusDeadLetter) {
		t.Errorf("Status = %q, want %q", got.Status, StatusDeadLetter)
	}
	if want := strings.Repeat("x", 4000); got.Error != want {
		t.Errorf("stored error_message is %d characters, want exactly %d (the declared VARCHAR(4000) width, head of the cause preserved)", utf8.RuneCountInString(got.Error), utf8.RuneCountInString(want))
	}
	out := buf.String()
	if !strings.Contains(out, "jobs: message changed to fit its column") ||
		!strings.Contains(out, "column=error_message") || !strings.Contains(out, "reason=column_width") {
		t.Errorf("missing the structured truncation warning naming the error_message column and the column_width reason:\n%s", out)
	}
}

func TestCompleteRetrying_OverlongCause_TruncatedToColumnWidth(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	prevDefault := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	next := time.Now().Add(10 * time.Second).UTC().Truncate(time.Second)
	longCause := strings.Repeat("x", 5500)
	moved, err := completeRetrying(context.Background(), db, testWriterOwner, rec.ID, longCause, next, time.Now())
	if err != nil || !moved {
		t.Fatalf("completeRetrying() = (%v, %v), want (true, nil): the retry transition must never be refused because the cause is overlong", moved, err)
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.Status != string(StatusRetrying) {
		t.Errorf("Status = %q, want %q", got.Status, StatusRetrying)
	}
	if want := strings.Repeat("x", 4000); got.Error != want {
		t.Errorf("stored error_message is %d characters, want exactly %d (the declared VARCHAR(4000) width, head of the cause preserved)", utf8.RuneCountInString(got.Error), utf8.RuneCountInString(want))
	}
	out := buf.String()
	if !strings.Contains(out, "jobs: message changed to fit its column") ||
		!strings.Contains(out, "column=error_message") || !strings.Contains(out, "reason=column_width") {
		t.Errorf("missing the structured truncation warning naming the error_message column and the column_width reason:\n%s", out)
	}
}

func TestUpdateProgress_OverlongMessage_TruncatedToColumnWidth(t *testing.T) {
	db := newTestDB(t)
	rec := fixtureRecord("tenant-a", "t")
	if _, err := insertRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("insertRecord() error = %v", err)
	}
	// Same running-row setup TestUpdateProgress requires: the write only
	// lands on a StatusRunning row.
	if _, err := claimOne(context.Background(), db, *rec, time.Now(), testWriterOwner); err != nil {
		t.Fatalf("claimOne() error = %v", err)
	}

	prevDefault := slog.Default()
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevDefault) })

	// 1500 multi-byte runes (4500 UTF-8 bytes): the cut must land on a
	// rune boundary, never split a character, and must fit the column's
	// 1000-character width however wide the bytes are.
	longMsg := strings.Repeat("é", 1500)
	if err := updateProgress(context.Background(), db, testWriterOwner, rec.ID, 42, longMsg); err != nil {
		t.Fatalf("updateProgress() error = %v", err)
	}

	got, err := findByID(context.Background(), db, JobID(rec.ID))
	if err != nil {
		t.Fatalf("findByID() error = %v", err)
	}
	if got.ProgressPct != 42 {
		t.Errorf("ProgressPct = %d, want 42", got.ProgressPct)
	}
	if want := strings.Repeat("é", 1000); got.ProgressMsg != want {
		t.Errorf("stored progress_msg is %d characters, want exactly %d (the declared VARCHAR(1000) width, head of the message preserved)", utf8.RuneCountInString(got.ProgressMsg), utf8.RuneCountInString(want))
	}
	out := buf.String()
	if !strings.Contains(out, "jobs: message changed to fit its column") ||
		!strings.Contains(out, "column=progress_msg") || !strings.Contains(out, "reason=column_width") {
		t.Errorf("missing the structured truncation warning naming the progress_msg column and the column_width reason:\n%s", out)
	}
}
