package sharing

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/tenancy/tenancytest"

	"github.com/vislake/speed/go/sharing/internal/testutil"
	"github.com/vislake/speed/go/sharing/migrations"
)

// newTestDB returns a fresh, per-call SQLite *gorm.DB with this module's
// migrations applied from zero.
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.NewSQLite(t, moduleName, migrations.FS)
}

func newTestShare(id string, now time.Time) *Share {
	expires := now.Add(24 * time.Hour)
	return &Share{
		ID:          id,
		ResourceRef: "storage:object-" + id,
		TokenHash:   hashShareToken("token-" + id),
		ExpiresAt:   &expires,
	}
}

// --- ShareRepository -------------------------------------------------

func TestShareRepository_AssertIsolated(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *Share {
		return newTestShare(uuid.NewString(), now)
	})
}

func TestShareRepository_ByTokenHash_FindsWithinTenant(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	share := newTestShare("share-1", now)
	if err := repo.Create(ctxA, share); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.byTokenHash(ctxA, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash(own tenant): %v", err)
	}
	if got.ID != share.ID {
		t.Errorf("byTokenHash(own tenant) = %+v, want ID %q", got, share.ID)
	}

	if _, otherErr := repo.byTokenHash(ctxB, share.TokenHash); !errors.Is(otherErr, ErrNotAccessible) {
		t.Errorf("byTokenHash(other tenant) error = %v, want ErrNotAccessible", otherErr)
	}
}

func TestShareRepository_ByTokenHash_UnknownHashReportsNotAccessible(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")
	if _, err := repo.byTokenHash(ctx, "does-not-exist"); !errors.Is(err, ErrNotAccessible) {
		t.Errorf("byTokenHash(unknown hash) error = %v, want ErrNotAccessible", err)
	}
}

func TestShareRepository_TryRecordView_GuardsOnCurrentState(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	share := newTestShare("share-1", now)
	one := 1
	share.MaxViews = &one
	if err := repo.Create(ctx, share); err != nil {
		t.Fatalf("Create: %v", err)
	}

	won, err := repo.tryRecordView(ctx, share, now, nil)
	if err != nil {
		t.Fatalf("tryRecordView (first): %v", err)
	}
	if !won {
		t.Fatalf("tryRecordView (first) = false, want true")
	}

	// Re-read so this copy's ViewCount matches the row's real state (1):
	// the equality guard on view_count would otherwise ALSO refuse a
	// second attempt, which would not isolate what this test wants to
	// prove -- that the max_views ceiling itself refuses it.
	fresh, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if fresh.ViewCount != 1 {
		t.Fatalf("fresh.ViewCount = %d, want 1", fresh.ViewCount)
	}

	won, err = repo.tryRecordView(ctx, fresh, now, nil)
	if err != nil {
		t.Fatalf("tryRecordView (second, exhausted): %v", err)
	}
	if won {
		t.Errorf("tryRecordView (second, exhausted) = true, want false -- max_views already reached")
	}
}

func TestShareRepository_TryRecordView_StaleViewCountLosesTheRace(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	share := newTestShare("share-1", now)
	if err := repo.Create(ctx, share); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate another caller having already recorded a view: the row's
	// real view_count is now 1, but this copy of share still says 0.
	other, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if racerWon, racerErr := repo.tryRecordView(ctx, other, now, nil); racerErr != nil || !racerWon {
		t.Fatalf("tryRecordView (racer): won=%v err=%v", racerWon, racerErr)
	}

	won, err := repo.tryRecordView(ctx, share, now, nil)
	if err != nil {
		t.Fatalf("tryRecordView (stale copy): %v", err)
	}
	if won {
		t.Errorf("tryRecordView (stale copy) = true, want false -- the row's view_count has moved on")
	}
}

// TestShareRepository_TryRecordView_RetriedAttemptRecognizesItsOwnRecordedAccess
// pins the idempotency half of tryRecordView's answer to dbkit's
// commit-time-failure cell (WithTenantSession's own doc comment): a
// WithTenantSession non-nil return does not prove the attempt committed
// nothing -- a commit reported failed can leave its writes durably present
// -- so a retried attempt must not read "my WHERE clause no longer
// matched" as "a concurrent writer took the view". The one state that
// distinguishes "someone else committed" from "I committed" is the granted
// log row: only one logical access's own winning attempt inserts a row
// carrying its grantedEntry.ID (minted once per Service.Access call and
// carried unchanged through every one of that call's retries), and that
// row commits in the same transaction as the increment it trails. A
// retried attempt that finds its own row already durably recorded is
// looking at its own earlier attempt's committed residue: the view was
// already consumed by THIS access, so the attempt must report won == true
// -- the caller then serves the content the access already paid for --
// rather than won == false, which would refuse the access and burn the
// view on a MaxViews=1 share with no mechanism to reclaim it.
//
// The first tryRecordView below models the misreported attempt's durable
// residue honestly: it commits the increment and the granted row for real.
// The database-level failure that would have reported that commit as
// failed cannot be manufactured deterministically from this module's side
// (it needs dbkit's own deferred-constraint or commit-time-lock machinery,
// against a schema this module deliberately keeps free of foreign keys) --
// but the residue it leaves is exactly the state this retried call sees,
// which is the state the recognition must answer for: without the
// recognition, this retried call would report won == false and the access
// would be refused twice over.
func TestShareRepository_TryRecordView_RetriedAttemptRecognizesItsOwnRecordedAccess(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	share := newTestShare("share-1", now)
	one := 1
	share.MaxViews = &one
	if err := repo.Create(ctx, share); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The logical access's one granted log row, carrying the ID every one
	// of its retried attempts shares (Service.accessLogEntry's shape).
	entry := &AccessLogEntry{
		ID:          "entry-share-1",
		TenantModel: dbkit.TenantModel{TenantID: "tenant-a"},
		ShareID:     share.ID,
		OccurredAt:  now,
		Outcome:     AccessOutcomeGranted,
	}

	// First attempt: wins the CAS and commits the count and its trail
	// together.
	won, err := repo.tryRecordView(ctx, share, now, entry)
	if err != nil {
		t.Fatalf("tryRecordView (first): %v", err)
	}
	if !won {
		t.Fatalf("tryRecordView (first) = false, want true")
	}

	// Retried attempt with the SAME stale premise (this copy still says
	// ViewCount 0) and the SAME granted row: the residue of the first
	// attempt is durably present, exactly as the commit-time-failure cell
	// leaves it for the attempt that follows.
	won, err = repo.tryRecordView(ctx, share, now, entry)
	if err != nil {
		t.Fatalf("tryRecordView (retried): %v", err)
	}
	if !won {
		t.Fatalf("tryRecordView (retried) = false, want true -- the retried attempt's own committed residue must be recognized as its access, not misread as someone else's win")
	}

	// The recognition must not have spent a second view or written a
	// duplicate row: the share still holds exactly the one view the first
	// attempt consumed, and the log holds exactly the one granted row.
	fresh, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if fresh.ViewCount != 1 {
		t.Errorf("ViewCount = %d after recognition, want 1 -- a recognized retry must not spend a second view", fresh.ViewCount)
	}
	logs, err := NewAccessLogRepository(repo.db).listByShare(ctx, share.ID)
	if err != nil {
		t.Fatalf("listByShare: %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("access log rows = %d, want 1 -- a recognized retry must not write a duplicate granted row", len(logs))
	}
}

// TestShareRepository_TryReserveView_TakesAndRefusesTheLiveReservation
// pins the reserve half of the access route's reserve/confirm/refund
// shape: a MaxViews-limited share takes exactly one in-flight reservation,
// and a second attempt while that reservation is still LIVE (younger than
// viewReservationTimeout) affects zero rows -- the write-time in-use
// refusal a concurrent second fetch races against. Nothing about the
// reservation spends a view: ViewCount stays put until a confirm.
func TestShareRepository_TryReserveView_TakesAndRefusesTheLiveReservation(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	share := newTestShare("share-1", now)
	one := 1
	share.MaxViews = &one
	if err := repo.Create(ctx, share); err != nil {
		t.Fatalf("Create: %v", err)
	}

	won, err := repo.tryReserveView(ctx, share, now)
	if err != nil {
		t.Fatalf("tryReserveView (first): %v", err)
	}
	if !won {
		t.Fatalf("tryReserveView (first) = false, want true")
	}

	fresh, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if fresh.ViewsReserved != 1 || fresh.ViewsReservedAt == nil {
		t.Fatalf("after the reservation: ViewsReserved = %d, ViewsReservedAt = %v, want 1 and a timestamp", fresh.ViewsReserved, fresh.ViewsReservedAt)
	}
	if fresh.ViewCount != 0 {
		t.Fatalf("ViewCount = %d after the reservation, want 0 -- reserving must not spend a view", fresh.ViewCount)
	}

	// A second reservation attempt while the first is still live loses --
	// the in-use refusal -- and changes nothing.
	won, err = repo.tryReserveView(ctx, fresh, now)
	if err != nil {
		t.Fatalf("tryReserveView (second, in use): %v", err)
	}
	if won {
		t.Fatalf("tryReserveView (second, in use) = true, want false")
	}
	after, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash (after the refused second): %v", err)
	}
	if after.ViewsReserved != 1 || after.ViewCount != 0 {
		t.Errorf("row after the refused second reservation = ViewsReserved %d / ViewCount %d, want 1 / 0", after.ViewsReserved, after.ViewCount)
	}
}

// TestShareRepository_TryReserveView_StaleReservationIsTakenOver pins the
// interrupted-reservation convergence: a reservation that has OUTLIVED
// viewReservationTimeout no longer refuses -- it is presumed left behind by
// a serve that died without resolving it, and a newer fetch takes it over,
// refreshing views_reserved_at in the same write. The takeover is what
// keeps a crashed serve's dead reservation from wedging the share's last
// view forever.
func TestShareRepository_TryReserveView_StaleReservationIsTakenOver(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	t0 := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	share := newTestShare("share-1", t0)
	one := 1
	share.MaxViews = &one
	if err := repo.Create(ctx, share); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if won, err := repo.tryReserveView(ctx, share, t0); err != nil || !won {
		t.Fatalf("tryReserveView (first): won=%v err=%v", won, err)
	}

	// Still live just before the timeout boundary: refused.
	if won, err := repo.tryReserveView(ctx, share, t0.Add(viewReservationTimeout-1*time.Minute)); err != nil || won {
		t.Fatalf("tryReserveView (pre-timeout) = won=%v err=%v, want won=false (still live)", won, err)
	}

	// Past the timeout: the stale reservation is taken over.
	takeoverAt := t0.Add(viewReservationTimeout + time.Minute)
	won, err := repo.tryReserveView(ctx, share, takeoverAt)
	if err != nil {
		t.Fatalf("tryReserveView (takeover): %v", err)
	}
	if !won {
		t.Fatalf("tryReserveView (takeover) = false, want true -- a stale reservation must be taken over")
	}
	fresh, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if fresh.ViewsReserved != 1 || fresh.ViewsReservedAt == nil {
		t.Fatalf("after the takeover: ViewsReserved = %d, ViewsReservedAt = %v, want 1 and a timestamp", fresh.ViewsReserved, fresh.ViewsReservedAt)
	}
	if !fresh.ViewsReservedAt.Equal(takeoverAt) {
		t.Errorf("ViewsReservedAt = %v after the takeover, want the takeover's own time %v -- the takeover refreshes the reservation", fresh.ViewsReservedAt, takeoverAt)
	}
}

// TestShareRepository_TryReserveView_RefusesOnNonReservableRows pins the
// guard's outer bounds: a reservation can only stand on a row that is
// still live, still under its MaxViews ceiling, and carrying a MaxViews at
// all -- a revoked, expired or exhausted share cannot be reserved (its
// refusals are the route's outward answer), and an UNLIMITED share has no
// finite allowance to draw a reservation from, so its rows never reserve.
func TestShareRepository_TryReserveView_RefusesOnNonReservableRows(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	revoked := newTestShare("revoked", now)
	one := 1
	revoked.MaxViews = &one
	if err := repo.Create(ctx, revoked); err != nil {
		t.Fatalf("Create(revoked): %v", err)
	}
	if won, err := repo.markRevoked(ctx, revoked.ID, now); err != nil || !won {
		t.Fatalf("markRevoked: won=%v err=%v", won, err)
	}

	exhausted := newTestShare("exhausted", now)
	exhausted.MaxViews = &one
	if err := repo.Create(ctx, exhausted); err != nil {
		t.Fatalf("Create(exhausted): %v", err)
	}
	freshExhausted, err := repo.byTokenHash(ctx, exhausted.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash(exhausted): %v", err)
	}
	if won, err := repo.tryRecordView(ctx, freshExhausted, now, nil); err != nil || !won {
		t.Fatalf("tryRecordView (exhausting the one view): won=%v err=%v", won, err)
	}

	unlimited := newTestShare("unlimited", now)
	if err := repo.Create(ctx, unlimited); err != nil {
		t.Fatalf("Create(unlimited): %v", err)
	}

	expired := newTestShare("expired", now)
	expired.MaxViews = &one
	if err := repo.Create(ctx, expired); err != nil {
		t.Fatalf("Create(expired): %v", err)
	}

	expiredLater := now.Add(25 * time.Hour) // newTestShare expires 24h after now
	cases := []struct {
		name string
		row  *Share
		at   time.Time
	}{
		{"revoked share", revoked, now},
		{"exhausted share", freshExhausted, now},
		{"share past its expiry", expired, expiredLater},
		{"unlimited share", unlimited, now},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row, err := repo.byTokenHash(ctx, c.row.TokenHash)
			if err != nil {
				t.Fatalf("byTokenHash: %v", err)
			}
			won, err := repo.tryReserveView(ctx, row, c.at)
			if err != nil {
				t.Fatalf("tryReserveView: %v", err)
			}
			if won {
				t.Errorf("tryReserveView on a %s = true, want false", c.name)
			}
		})
	}
}

// TestShareRepository_TryConfirmView_ConvertsTheReservationAndLogsWithIt
// pins the confirm half of the reserve/confirm/refund shape: a standing
// reservation is confirmed into one spent view -- view_count incremented,
// the reservation cleared -- with the serve's granted log row committed in
// the SAME transaction. Once confirmed, nothing remains to confirm: a
// second confirm (or a confirm with no reservation standing at all)
// affects zero rows.
func TestShareRepository_TryConfirmView_ConvertsTheReservationAndLogsWithIt(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	share := newTestShare("share-1", now)
	one := 1
	share.MaxViews = &one
	if err := repo.Create(ctx, share); err != nil {
		t.Fatalf("Create: %v", err)
	}
	fresh, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if won, reserveErr := repo.tryReserveView(ctx, fresh, now); reserveErr != nil || !won {
		t.Fatalf("tryReserveView: won=%v err=%v", won, reserveErr)
	}

	entry := &AccessLogEntry{
		ID:          uuid.NewString(),
		TenantModel: dbkit.TenantModel{TenantID: "tenant-a"},
		ShareID:     share.ID,
		OccurredAt:  now,
		Outcome:     AccessOutcomeGranted,
	}
	won, err := repo.tryConfirmView(ctx, fresh, now, entry)
	if err != nil {
		t.Fatalf("tryConfirmView: %v", err)
	}
	if !won {
		t.Fatalf("tryConfirmView = false, want true")
	}

	after, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if after.ViewCount != 1 {
		t.Errorf("ViewCount = %d after the confirm, want 1", after.ViewCount)
	}
	if after.ViewsReserved != 0 || after.ViewsReservedAt != nil {
		t.Errorf("reservation after the confirm = ViewsReserved %d / ViewsReservedAt %v, want 0 / nil", after.ViewsReserved, after.ViewsReservedAt)
	}

	// The granted row landed in the same transaction.
	var logged []AccessLogEntry
	if logErr := dbkit.WithTenantSession(ctx, repo.db, func(tx *gorm.DB) error {
		return tx.Where("share_id = ?", share.ID).Find(&logged).Error
	}); logErr != nil {
		t.Fatalf("access-log read: %v", logErr)
	}
	if len(logged) != 1 || logged[0].Outcome != AccessOutcomeGranted || logged[0].ID != entry.ID {
		t.Errorf("access log after the confirm = %+v, want exactly the serve's one granted row", logged)
	}

	// A second confirm -- no reservation standing -- affects zero rows.
	won, err = repo.tryConfirmView(ctx, after, now, nil)
	if err != nil {
		t.Fatalf("tryConfirmView (second): %v", err)
	}
	if won {
		t.Errorf("tryConfirmView (second, no reservation) = true, want false")
	}
}

// TestShareRepository_TryConfirmView_LostConfirmLeavesNoTrace pins the
// confirm's count-and-trail atomicity on the losing side: a confirm whose
// reservation was already resolved, or whose share ceased to be live while
// its delivery was in flight, affects zero rows, inserts nothing, and
// spends nothing -- the caller settles the delivered serve as denied
// instead (confirmAccessView's own doc comment).
func TestShareRepository_TryConfirmView_LostConfirmLeavesNoTrace(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	// A share whose delivery was interrupted -- no reservation ever taken
	// (say the serve failed before the reserve, or the reservation was
	// already refunded): a confirm finds nothing to confirm.
	noReservation := newTestShare("no-reservation", now)
	one := 1
	noReservation.MaxViews = &one
	if err := repo.Create(ctx, noReservation); err != nil {
		t.Fatalf("Create(no-reservation): %v", err)
	}
	freshNoReservation, err := repo.byTokenHash(ctx, noReservation.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash(no-reservation): %v", err)
	}
	if won, confirmErr := repo.tryConfirmView(ctx, freshNoReservation, now, nil); confirmErr != nil || won {
		t.Fatalf("tryConfirmView (no reservation) = won=%v err=%v, want won=false", won, confirmErr)
	}

	// A share revoked while its delivery was in flight: the reservation
	// stands but the confirm's liveness guard refuses it, exactly as
	// settle-time liveness refuses a no-longer-live post-delivery record
	// (tryConfirmView's own doc comment).
	revoked := newTestShare("revoked-mid-flight", now)
	revoked.MaxViews = &one
	if createErr := repo.Create(ctx, revoked); createErr != nil {
		t.Fatalf("Create(revoked-mid-flight): %v", createErr)
	}
	freshRevoked, err := repo.byTokenHash(ctx, revoked.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash(revoked-mid-flight): %v", err)
	}
	if won, reserveErr := repo.tryReserveView(ctx, freshRevoked, now); reserveErr != nil || !won {
		t.Fatalf("tryReserveView(revoked-mid-flight): won=%v err=%v", won, reserveErr)
	}
	if won, markErr := repo.markRevoked(ctx, revoked.ID, now); markErr != nil || !won {
		t.Fatalf("markRevoked: won=%v err=%v", won, markErr)
	}
	if won, confirmErr := repo.tryConfirmView(ctx, freshRevoked, now, nil); confirmErr != nil || won {
		t.Fatalf("tryConfirmView (revoked mid-flight) = won=%v err=%v, want won=false", won, confirmErr)
	}
	afterRevoked, err := repo.byTokenHash(ctx, revoked.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash(after revoked confirm): %v", err)
	}
	if afterRevoked.ViewCount != 0 {
		t.Errorf("ViewCount = %d after the refused confirm on the revoked share, want 0", afterRevoked.ViewCount)
	}

	// A share at its ceiling cannot be confirmed into a view it has no
	// room for -- the corner a stale-reservation takeover can create (two
	// overlapping serves racing one ceiling, viewReservationTimeout's own
	// doc comment).
	overCeiling := newTestShare("over-ceiling", now)
	overCeiling.MaxViews = &one
	if createErr := repo.Create(ctx, overCeiling); createErr != nil {
		t.Fatalf("Create(over-ceiling): %v", createErr)
	}
	freshOverCeiling, err := repo.byTokenHash(ctx, overCeiling.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash(over-ceiling): %v", err)
	}
	if won, err := repo.tryReserveView(ctx, freshOverCeiling, now); err != nil || !won {
		t.Fatalf("tryReserveView(over-ceiling): won=%v err=%v", won, err)
	}
	// A concurrent winner already spent the ceiling while this serve was in
	// flight.
	if err := repo.db.Exec("UPDATE "+tableShares+" SET view_count = 1 WHERE id = ?", overCeiling.ID).Error; err != nil {
		t.Fatalf("UPDATE view_count: %v", err)
	}
	if won, err := repo.tryConfirmView(ctx, freshOverCeiling, now, nil); err != nil || won {
		t.Fatalf("tryConfirmView (over ceiling) = won=%v err=%v, want won=false", won, err)
	}
}

// TestShareRepository_TryConfirmView_ScopedToOneTenant is the isolation
// proof tryConfirmView's raw-Exec shape requires (repository.go's own doc
// comment names it): the hand-written tenant_id predicate must scope the
// confirm to the caller's tenant exactly as the tenant-scope plugin would.
// Another tenant's confirm against the same share id affects zero rows and
// leaves the reservation standing.
func TestShareRepository_TryConfirmView_ScopedToOneTenant(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	share := newTestShare("share-1", now)
	one := 1
	share.MaxViews = &one
	if err := repo.Create(ctxA, share); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if won, err := repo.tryReserveView(ctxA, share, now); err != nil || !won {
		t.Fatalf("tryReserveView: won=%v err=%v", won, err)
	}

	won, err := repo.tryConfirmView(ctxB, share, now, nil)
	if err != nil {
		t.Fatalf("tryConfirmView(other tenant): %v", err)
	}
	if won {
		t.Fatalf("tryConfirmView(other tenant) = true, want false -- the confirm must not cross tenants")
	}

	fresh, err := repo.byTokenHash(ctxA, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if fresh.ViewCount != 0 || fresh.ViewsReserved != 1 {
		t.Errorf("row after the foreign-tenant confirm = ViewCount %d / ViewsReserved %d, want 0 / 1 -- nothing of the reservation was spent cross-tenant", fresh.ViewCount, fresh.ViewsReserved)
	}
}

// TestShareRepository_TryRefundView_ClearsOnlyAStandingReservation pins the
// refund arm: a standing reservation is released without spending a view
// (the failed-serve arm of the reserve/confirm/refund shape), and a refund
// that finds no reservation standing -- already confirmed, already
// refunded, never taken -- is an idempotent no-op reporting won == false,
// exactly as the resolution arms of go/billing's CreditService are.
func TestShareRepository_TryRefundView_ClearsOnlyAStandingReservation(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	share := newTestShare("share-1", now)
	one := 1
	share.MaxViews = &one
	if err := repo.Create(ctx, share); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// No reservation standing: the refund is an idempotent no-op.
	won, err := repo.tryRefundView(ctx, share.ID, now)
	if err != nil {
		t.Fatalf("tryRefundView (nothing standing): %v", err)
	}
	if won {
		t.Fatalf("tryRefundView (nothing standing) = true, want false")
	}

	// A standing reservation is released, spending nothing.
	reserveWon, reserveErr := repo.tryReserveView(ctx, share, now)
	if reserveErr != nil {
		t.Fatalf("tryReserveView: %v", reserveErr)
	}
	if !reserveWon {
		t.Fatalf("tryReserveView = false, want true")
	}
	won, err = repo.tryRefundView(ctx, share.ID, now)
	if err != nil {
		t.Fatalf("tryRefundView: %v", err)
	}
	if !won {
		t.Fatalf("tryRefundView = false, want true")
	}
	fresh, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if fresh.ViewsReserved != 0 || fresh.ViewsReservedAt != nil {
		t.Errorf("reservation after the refund = ViewsReserved %d / ViewsReservedAt %v, want 0 / nil", fresh.ViewsReserved, fresh.ViewsReservedAt)
	}
	if fresh.ViewCount != 0 {
		t.Errorf("ViewCount = %d after the refund, want 0 -- a refund must not spend a view", fresh.ViewCount)
	}

	// And the released share can be reserved again.
	won, err = repo.tryReserveView(ctx, fresh, now)
	if err != nil {
		t.Fatalf("tryReserveView (after refund): %v", err)
	}
	if !won {
		t.Errorf("tryReserveView (after refund) = false, want true -- the refunded share must be reservable again")
	}
}

// TestShareRepository_TryRefundView_ScopedToOneTenant is the isolation
// proof tryRefundView's raw-Exec shape requires: the hand-written tenant_id
// predicate must scope the refund to the caller's tenant. Another tenant's
// refund against the same share id affects zero rows and leaves the
// standing reservation in place.
func TestShareRepository_TryRefundView_ScopedToOneTenant(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	share := newTestShare("share-1", now)
	one := 1
	share.MaxViews = &one
	if err := repo.Create(ctxA, share); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if won, err := repo.tryReserveView(ctxA, share, now); err != nil || !won {
		t.Fatalf("tryReserveView: won=%v err=%v", won, err)
	}

	won, err := repo.tryRefundView(ctxB, share.ID, now)
	if err != nil {
		t.Fatalf("tryRefundView(other tenant): %v", err)
	}
	if won {
		t.Fatalf("tryRefundView(other tenant) = true, want false -- the refund must not cross tenants")
	}
	fresh, err := repo.byTokenHash(ctxA, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if fresh.ViewsReserved != 1 {
		t.Errorf("ViewsReserved = %d after the foreign-tenant refund attempt, want 1 -- the standing reservation must survive it", fresh.ViewsReserved)
	}
}

// TestShareRepository_TryRecordView_RefusedWhileTheReservationIsLive pins
// the reservation clause on the in-process Access path's CAS: a LIVE
// reservation (a route serve mid-delivery) refuses the record -- an
// in-process Access must not spend the view a delivery in flight holds --
// while a STALE reservation does not refuse: the CAS converges it by
// spending the view it held, the next-access half of an interrupted
// reservation's lifecycle.
func TestShareRepository_TryRecordView_RefusedWhileTheReservationIsLive(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	t0 := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	share := newTestShare("share-1", t0)
	one := 1
	share.MaxViews = &one
	if err := repo.Create(ctx, share); err != nil {
		t.Fatalf("Create: %v", err)
	}
	fresh, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if won, reserveErr := repo.tryReserveView(ctx, fresh, t0); reserveErr != nil || !won {
		t.Fatalf("tryReserveView: won=%v err=%v", won, reserveErr)
	}

	// Live reservation: the CAS refuses, spending nothing.
	won, err := repo.tryRecordView(ctx, fresh, t0, nil)
	if err != nil {
		t.Fatalf("tryRecordView (live reservation): %v", err)
	}
	if won {
		t.Fatalf("tryRecordView (live reservation) = true, want false")
	}
	afterLive, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash (live reservation): %v", err)
	}
	if afterLive.ViewCount != 0 {
		t.Fatalf("ViewCount = %d after the refused record against a live reservation, want 0", afterLive.ViewCount)
	}

	// Same reservation once STALE: the CAS converges it by spending.
	staleNow := t0.Add(viewReservationTimeout + time.Minute)
	afterStaleRead, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash (stale reservation): %v", err)
	}
	if afterStaleRead.hasLiveReservation(staleNow) {
		t.Fatalf("hasLiveReservation(%v) = true for a reservation from %v, want false", staleNow, t0)
	}
	won, err = repo.tryRecordView(ctx, afterStaleRead, staleNow, nil)
	if err != nil {
		t.Fatalf("tryRecordView (stale reservation): %v", err)
	}
	if !won {
		t.Fatalf("tryRecordView (stale reservation) = false, want true -- the stale reservation must be converged by the spend")
	}
	afterStale, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash (after stale convergence): %v", err)
	}
	if afterStale.ViewCount != 1 {
		t.Errorf("ViewCount = %d after the stale-reservation convergence, want 1", afterStale.ViewCount)
	}
}

// TestShareRepository_ListStaleReserved_ReportsOnlyAgedReservations pins
// the sweep's reservation-arm listing: exactly the rows still holding a
// reservation that has outlived viewReservationTimeout as of now are
// listed -- a live reservation is not, and neither is a row with no
// reservation at all.
func TestShareRepository_ListStaleReserved_ReportsOnlyAgedReservations(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	t0 := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	stale := newTestShare("stale", t0)
	one := 1
	stale.MaxViews = &one
	if err := repo.Create(ctx, stale); err != nil {
		t.Fatalf("Create(stale): %v", err)
	}
	if won, err := repo.tryReserveView(ctx, stale, t0); err != nil || !won {
		t.Fatalf("tryReserveView(stale): won=%v err=%v", won, err)
	}

	// The live reservation is taken just before the sweep's own instant, so
	// it is still younger than viewReservationTimeout at listing time.
	sweepNow := t0.Add(viewReservationTimeout + 2*time.Minute)
	live := newTestShare("live", t0)
	live.MaxViews = &one
	if err := repo.Create(ctx, live); err != nil {
		t.Fatalf("Create(live): %v", err)
	}
	if won, err := repo.tryReserveView(ctx, live, sweepNow.Add(-time.Minute)); err != nil || !won {
		t.Fatalf("tryReserveView(live): won=%v err=%v", won, err)
	}

	plain := newTestShare("plain", t0)
	if err := repo.Create(ctx, plain); err != nil {
		t.Fatalf("Create(plain): %v", err)
	}

	rows, err := repo.listStaleReserved(ctx, sweepNow)
	if err != nil {
		t.Fatalf("listStaleReserved: %v", err)
	}
	var ids []string
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	if len(rows) != 1 || rows[0].ID != stale.ID {
		t.Errorf("listStaleReserved = %v, want exactly the stale row %q", ids, stale.ID)
	}
}

func TestShareRepository_ListExpiredOrExhausted(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	live := newTestShare("live", now)
	if err := repo.Create(ctx, live); err != nil {
		t.Fatalf("Create(live): %v", err)
	}

	expired := newTestShare("expired", now)
	past := now.Add(-time.Hour)
	expired.ExpiresAt = &past
	if err := repo.Create(ctx, expired); err != nil {
		t.Fatalf("Create(expired): %v", err)
	}

	exhausted := newTestShare("exhausted", now)
	zero := 0
	exhausted.MaxViews = &zero
	if err := repo.Create(ctx, exhausted); err != nil {
		t.Fatalf("Create(exhausted): %v", err)
	}

	alreadyRevoked := newTestShare("already-revoked", now)
	alreadyRevoked.ExpiresAt = &past
	revokedAt := now
	alreadyRevoked.RevokedAt = &revokedAt
	if err := repo.Create(ctx, alreadyRevoked); err != nil {
		t.Fatalf("Create(alreadyRevoked): %v", err)
	}

	got, err := repo.listExpiredOrExhausted(ctx, now)
	if err != nil {
		t.Fatalf("listExpiredOrExhausted: %v", err)
	}
	ids := make(map[string]bool, len(got))
	for _, s := range got {
		ids[s.ID] = true
	}
	if !ids["expired"] {
		t.Errorf("listExpiredOrExhausted missed the expired share")
	}
	if !ids["exhausted"] {
		t.Errorf("listExpiredOrExhausted missed the view-exhausted share")
	}
	if ids["live"] {
		t.Errorf("listExpiredOrExhausted wrongly included the live share")
	}
	if ids["already-revoked"] {
		t.Errorf("listExpiredOrExhausted wrongly included an already-revoked share")
	}
}

// TestShareRepository_ListPage_NewestFirstAndTenantScoped is
// listPage's own proof (Service.List's backing method, the owner-facing
// sharing_listShares operation): it returns the caller tenant's shares,
// newest first, revoked and live alike --
// unlike listExpiredOrExhausted above, an owner-facing listing must still
// show a share that is gone, since RevokedAt on the returned row is exactly
// how the owner learns that. The page is big enough to hold everything, so
// this is the whole-tenancy read.
func TestShareRepository_ListPage_NewestFirstAndTenantScoped(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	// Explicit, distinct CreatedAt values (autoCreateTime only fills a zero
	// value) so ordering is asserted against a real difference, never
	// against however fast two inserts land on the test's own clock.
	oldest := newTestShare("oldest", now)
	oldest.CreatedAt = now.Add(-2 * time.Hour)
	if err := repo.Create(ctxA, oldest); err != nil {
		t.Fatalf("Create(oldest): %v", err)
	}

	revoked := newTestShare("revoked", now)
	revoked.CreatedAt = now.Add(-time.Hour)
	revokedAt := now
	revoked.RevokedAt = &revokedAt
	if err := repo.Create(ctxA, revoked); err != nil {
		t.Fatalf("Create(revoked): %v", err)
	}

	newest := newTestShare("newest", now)
	newest.CreatedAt = now
	if err := repo.Create(ctxA, newest); err != nil {
		t.Fatalf("Create(newest): %v", err)
	}

	otherTenant := newTestShare("other-tenant", now)
	if err := repo.Create(ctxB, otherTenant); err != nil {
		t.Fatalf("Create(otherTenant): %v", err)
	}

	got, err := repo.listPage(ctxA, 200, "")
	if err != nil {
		t.Fatalf("listPage: %v", err)
	}
	var ids []string
	for _, s := range got {
		ids = append(ids, s.ID)
	}
	want := []string{"newest", "revoked", "oldest"}
	if len(ids) != len(want) {
		t.Fatalf("listPage returned %v, want %v (other-tenant's share must never appear)", ids, want)
	}
	for i, id := range want {
		if ids[i] != id {
			t.Errorf("listPage[%d] = %q, want %q -- ordering must be newest first", i, ids[i], id)
		}
	}
}

// TestShareRepository_ListPage_PagesByKeysetCursor drives listPage's keyset
// mechanics: a page of limit rows, the next page starting after the last
// row's id, and a row inserted between two fetches shifting nothing -- the
// property an offset-based listing does not have. The order across pages
// is the total (created_at DESC, id DESC) order, and pages never overlap.
func TestShareRepository_ListPage_PagesByKeysetCursor(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	now := time.Now().UTC()

	// Five shares with distinct CreatedAt values: s1 oldest ... s5 newest.
	// Row ids are created so the id tie-break direction never matters here.
	for i := 1; i <= 5; i++ {
		share := newTestShare(fmt.Sprintf("s%d", i), now)
		share.CreatedAt = now.Add(time.Duration(i-6) * time.Hour)
		if err := repo.Create(ctxA, share); err != nil {
			t.Fatalf("Create(s%d): %v", i, err)
		}
	}

	ids := func(rows []Share) []string {
		var out []string
		for _, s := range rows {
			out = append(out, s.ID)
		}
		return out
	}

	// Page 1 of 2: newest first, exactly limit rows.
	page1, err := repo.listPage(ctxA, 2, "")
	if err != nil {
		t.Fatalf("listPage(page 1): %v", err)
	}
	want1 := []string{"s5", "s4"}
	if got := ids(page1); !slices.Equal(got, want1) {
		t.Fatalf("page 1 = %v, want %v", got, want1)
	}

	// Page 2 continues strictly after page 1's last row: no overlap, no gap.
	page2, err := repo.listPage(ctxA, 2, page1[len(page1)-1].ID)
	if err != nil {
		t.Fatalf("listPage(page 2): %v", err)
	}
	if got := ids(page2); !slices.Equal(got, []string{"s3", "s2"}) {
		t.Fatalf("page 2 = %v, want [s3 s2]", got)
	}

	// A share created between the two fetches -- newest of all, so it joins
	// page 1's front if page 1 were re-fetched -- must not shift page 2:
	// the keyset cursor names a position, not an offset.
	late := newTestShare("s6", now)
	late.CreatedAt = now.Add(time.Hour)
	if createErr := repo.Create(ctxA, late); createErr != nil {
		t.Fatalf("Create(s6, between pages): %v", createErr)
	}
	page2Again, err := repo.listPage(ctxA, 2, page1[len(page1)-1].ID)
	if err != nil {
		t.Fatalf("listPage(page 2 after an insert): %v", err)
	}
	if got := ids(page2Again); !slices.Equal(got, []string{"s3", "s2"}) {
		t.Fatalf("page 2 after an insert = %v, want [s3 s2] -- the keyset must shift nothing", got)
	}

	// The final page: the cursor walks to the end and the last page may be
	// short.
	page3, err := repo.listPage(ctxA, 2, page2[len(page2)-1].ID)
	if err != nil {
		t.Fatalf("listPage(page 3): %v", err)
	}
	if got := ids(page3); !slices.Equal(got, []string{"s1"}) {
		t.Fatalf("page 3 = %v, want [s1]", got)
	}
}

// TestShareRepository_ListPage_UnknownCursor_ReportsRecordNotFound pins the
// cursor probe's not-found answer: a beforeID naming no share of the caller
// tenant -- one that never existed, or one that exists under another tenant
// -- reports dbkit.ErrRecordNotFound with the id named, indistinguishable on
// purpose (a share has no deletion path, so "not in this tenant" and "never
// existed" are the same fact). Service.List maps that answer onto
// sharing.share_not_found for the HTTP surface.
func TestShareRepository_ListPage_UnknownCursor_ReportsRecordNotFound(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")
	now := time.Now().UTC()

	foreign := newTestShare("foreign", now)
	if err := repo.Create(ctxB, foreign); err != nil {
		t.Fatalf("Create(foreign): %v", err)
	}

	for _, tc := range []struct {
		name     string
		beforeID string
	}{
		{"a cursor that never existed", "no-such-share"},
		{"another tenant's share id", foreign.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := repo.listPage(ctxA, 10, tc.beforeID)
			if !dbkit.IsRecordNotFound(err) {
				t.Fatalf("listPage(beforeID %q) error = %v, want dbkit's not-found code", tc.beforeID, err)
			}
		})
	}
}

// --- shareTokenIndex / createWithTokenIndex / tenantForTokenHash --------

// TestShareTokenIndex_AssertNotTenantScoped proves shareTokenIndex is
// genuinely platform data, not tenant data: a row written with no tenant
// in context is visible under an arbitrary one, and a row written under one
// tenant is visible under another -- the exact opposite of AssertIsolated,
// and the correct property for a table dbkit's tenant-scope GORM plugin
// must never engage on (model.go's shareTokenIndex doc comment explains
// why).
func TestShareTokenIndex_AssertNotTenantScoped(t *testing.T) {
	db := newTestDB(t)
	i := 0
	tenancytest.AssertNotTenantScoped(t, db, shareTokenIndex{},
		func(tx *gorm.DB) error {
			i++
			return tx.Create(&shareTokenIndex{
				TokenHash: hashShareToken("probe-token-" + uuid.NewString()),
				TenantID:  "irrelevant",
			}).Error
		},
		func(tx *gorm.DB) (int64, error) {
			var n int64
			err := tx.Model(&shareTokenIndex{}).Count(&n).Error
			return n, err
		},
	)
}

func TestShareRepository_CreateWithTokenIndex_WritesBothRowsUnderTheSameTenant(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	share := newTestShare("share-1", now)
	if err := repo.createWithTokenIndex(ctx, share); err != nil {
		t.Fatalf("createWithTokenIndex: %v", err)
	}

	// The ordinary tenant-scoped lookup finds the Share row, under the
	// tenant createWithTokenIndex resolved from ctx.
	got, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if got.ID != share.ID {
		t.Errorf("byTokenHash = %+v, want ID %q", got, share.ID)
	}

	// The narrow, tenant-less lookup resolves the same tenant from the same
	// hash, with no tenant anywhere in ctx.
	tenant, err := repo.tenantForTokenHash(context.Background(), share.TokenHash)
	if err != nil {
		t.Fatalf("tenantForTokenHash: %v", err)
	}
	if tenant != "tenant-a" {
		t.Errorf("tenantForTokenHash = %q, want %q", tenant, "tenant-a")
	}
}

func TestShareRepository_TenantForTokenHash_UnknownHashReportsNotAccessible(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	if _, err := repo.tenantForTokenHash(context.Background(), "does-not-exist"); !errors.Is(err, ErrNotAccessible) {
		t.Errorf("tenantForTokenHash(unknown hash) error = %v, want ErrNotAccessible", err)
	}
}

// TestShareRepository_TenantForTokenHash_NeedsNoTenantInContext is the
// direct proof of this method's whole reason to exist: a genuinely
// anonymous caller -- context.Background(), nothing attached to it at all
// -- can still resolve a tenant from a token hash. A regression that
// silently made this method call pkgcore.MustTenantFromContext (or route
// through dbkit.WithTenantSession, which does the same) would fail this
// test with pkgcore.ErrNoTenant instead of a real answer.
func TestShareRepository_TenantForTokenHash_NeedsNoTenantInContext(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	share := newTestShare("share-1", now)
	if err := repo.createWithTokenIndex(pkgcore.WithTenant(context.Background(), "tenant-b"), share); err != nil {
		t.Fatalf("createWithTokenIndex: %v", err)
	}

	tenant, err := repo.tenantForTokenHash(context.Background(), share.TokenHash)
	if err != nil {
		t.Fatalf("tenantForTokenHash(no tenant in ctx) error = %v, want success", err)
	}
	if tenant != "tenant-b" {
		t.Errorf("tenantForTokenHash = %q, want %q", tenant, "tenant-b")
	}
}

// --- AccessLogRepository -----------------------------------------------

func newTestAccessLogEntry(id, shareID string, now time.Time) *AccessLogEntry {
	return &AccessLogEntry{
		ID:         id,
		ShareID:    shareID,
		OccurredAt: now,
		Outcome:    AccessOutcomeGranted,
	}
}

func TestAccessLogRepository_AssertIsolated(t *testing.T) {
	repo := NewAccessLogRepository(newTestDB(t))
	now := time.Now().UTC()
	i := 0
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *AccessLogEntry {
		i++
		return newTestAccessLogEntry(uuid.NewString(), "share-x", now.Add(time.Duration(i)*time.Second))
	})
}

func TestAccessLogRepository_ListByShare_ScopedToTenantAndShare(t *testing.T) {
	repo := NewAccessLogRepository(newTestDB(t))
	now := time.Now().UTC()
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	entry1 := newTestAccessLogEntry("log-1", "share-1", now)
	entry2 := newTestAccessLogEntry("log-2", "share-1", now.Add(time.Second))
	otherShare := newTestAccessLogEntry("log-3", "share-2", now)
	if err := repo.Create(ctxA, entry1); err != nil {
		t.Fatalf("Create(entry1): %v", err)
	}
	if err := repo.Create(ctxA, entry2); err != nil {
		t.Fatalf("Create(entry2): %v", err)
	}
	if err := repo.Create(ctxA, otherShare); err != nil {
		t.Fatalf("Create(otherShare): %v", err)
	}
	crossTenant := newTestAccessLogEntry("log-4", "share-1", now)
	if err := repo.Create(ctxB, crossTenant); err != nil {
		t.Fatalf("Create(crossTenant): %v", err)
	}

	got, err := repo.listByShare(ctxA, "share-1")
	if err != nil {
		t.Fatalf("listByShare: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("listByShare returned %d rows, want 2", len(got))
	}
	if got[0].ID != "log-2" || got[1].ID != "log-1" {
		t.Errorf("listByShare order = [%s, %s], want newest first [log-2, log-1]", got[0].ID, got[1].ID)
	}
}

// TestShareRepository_TryIncrementView_IncrementsAndGuardsLiveness pins
// the unlimited-share view-recording guard (Service.recordView's arm for a
// MaxViews-nil share): one call records one view, a second call on a fresh
// row records another, and once the row is revoked or expired the
// increment refuses (won == false) instead of recording a view against a
// no-longer-live share.
func TestShareRepository_TryIncrementView_IncrementsAndGuardsLiveness(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	share := newTestShare("share-1", now)
	if err := repo.Create(ctx, share); err != nil {
		t.Fatalf("Create: %v", err)
	}

	won, err := repo.tryIncrementView(ctx, share, now, nil)
	if err != nil {
		t.Fatalf("tryIncrementView (first): %v", err)
	}
	if !won {
		t.Fatalf("tryIncrementView (first) = false, want true")
	}

	fresh, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if fresh.ViewCount != 1 {
		t.Fatalf("fresh.ViewCount = %d, want 1", fresh.ViewCount)
	}

	won, err = repo.tryIncrementView(ctx, fresh, now, nil)
	if err != nil {
		t.Fatalf("tryIncrementView (second): %v", err)
	}
	if !won {
		t.Fatalf("tryIncrementView (second) = false, want true")
	}

	revokedAt := now
	revokeWon, err := repo.markRevoked(ctx, share.ID, revokedAt)
	if err != nil {
		t.Fatalf("markRevoked: %v", err)
	}
	if !revokeWon {
		t.Fatalf("markRevoked = false, want true")
	}

	// A revoked row refuses the increment: the WHERE clause's liveness
	// guard is what makes the atomic increment refuse, not a read-then-
	// decide race.
	won, err = repo.tryIncrementView(ctx, share, now, nil)
	if err != nil {
		t.Fatalf("tryIncrementView (revoked): %v", err)
	}
	if won {
		t.Errorf("tryIncrementView (revoked share) = true, want false")
	}
}

// TestShareRepository_TryIncrementView_ScopedToOneTenant is the isolation
// proof tryIncrementView's raw-Exec shape requires (repository.go's own
// doc comment names it): the hand-written tenant_id predicate -- present
// because .Exec bypasses the tenant-scope plugin's callback chain -- must
// scope the increment to the caller's tenant exactly as the plugin would.
// Another tenant's call against the same share id affects zero rows.
func TestShareRepository_TryIncrementView_ScopedToOneTenant(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctxA := pkgcore.WithTenant(context.Background(), "tenant-a")
	ctxB := pkgcore.WithTenant(context.Background(), "tenant-b")

	share := newTestShare("share-1", now)
	if err := repo.Create(ctxA, share); err != nil {
		t.Fatalf("Create: %v", err)
	}

	won, err := repo.tryIncrementView(ctxB, share, now, nil)
	if err != nil {
		t.Fatalf("tryIncrementView(other tenant): %v", err)
	}
	if won {
		t.Fatalf("tryIncrementView(other tenant) = true, want false -- the increment must not cross tenants")
	}

	fresh, err := repo.byTokenHash(ctxA, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if fresh.ViewCount != 0 {
		t.Errorf("ViewCount = %d after a foreign-tenant increment attempt, want 0", fresh.ViewCount)
	}
}

// TestShareRepository_MarkRevoked_WinsOnceThenIdempotent pins the guarded
// revocation's transition semantics: the first markRevoked wins (won ==
// true), a second call -- the sequential double-revoke shape Service.Revoke
// answers idempotently -- affects zero rows (won == false), and the mark
// touches nothing but revoked_at: a view recorded before the mark survives
// it.
func TestShareRepository_MarkRevoked_WinsOnceThenIdempotent(t *testing.T) {
	repo := NewShareRepository(newTestDB(t))
	now := time.Now().UTC()
	ctx := pkgcore.WithTenant(context.Background(), "tenant-a")

	share := newTestShare("share-1", now)
	if err := repo.Create(ctx, share); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if won, err := repo.tryIncrementView(ctx, share, now, nil); err != nil || !won {
		t.Fatalf("tryIncrementView: won=%v err=%v", won, err)
	}

	revokedAt := now
	won, err := repo.markRevoked(ctx, share.ID, revokedAt)
	if err != nil {
		t.Fatalf("markRevoked (first): %v", err)
	}
	if !won {
		t.Fatalf("markRevoked (first) = false, want true")
	}

	won, err = repo.markRevoked(ctx, share.ID, revokedAt.Add(time.Hour))
	if err != nil {
		t.Fatalf("markRevoked (second): %v", err)
	}
	if won {
		t.Errorf("markRevoked (second) = true, want false -- the second revoke must not re-transition the row")
	}

	fresh, err := repo.byTokenHash(ctx, share.TokenHash)
	if err != nil {
		t.Fatalf("byTokenHash: %v", err)
	}
	if fresh.ViewCount != 1 {
		t.Errorf("ViewCount = %d after markRevoked, want 1 -- the guarded revoke must not roll back a recorded view", fresh.ViewCount)
	}
	if fresh.RevokedAt == nil {
		t.Errorf("RevokedAt is nil after markRevoked, want set")
	}
}
