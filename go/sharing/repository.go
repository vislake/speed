package sharing

import (
	"context"
	"errors"
	"sync"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// ShareRepository is the tenant-scoped data-access type for Share. It
// embeds dbkit.Repository[Share] for the ordinary CRUD surface (Create,
// FindByID, Update, ...) and adds the extra query shapes Service needs on
// top, composed on the *gorm.DB the isolation plugin protects, inside
// dbkit.WithTenantSession -- no hand-written tenant predicate, no
// db.Table / db.Model / db.Raw, the identical construction rule
// go/org's InvitationRepository documents for its own extra methods.
type ShareRepository struct {
	*dbkit.Repository[Share]

	db *gorm.DB

	// serializeWrites reports whether this repository's guarded writes
	// (runGuardedWrite, concurrency.go) order themselves behind writeMu:
	// true only when db speaks SQLite, the one dialect whose single-writer
	// file lock this ordering exists to keep contention-free (concurrency.go's
	// own doc comment has the full reasoning). On PostgreSQL the field is
	// false and guarded writes run without the in-process mutex -- the
	// database's own row locks plus the bounded conflict retry are the
	// honest mechanism there, and a process-wide cross-tenant mutex would
	// only serialize unrelated tenants' writes for no benefit. Set once at
	// construction from the dialector name (db.Name()), never read per call.
	serializeWrites bool

	// writeMu is the mutex serializeWrites gates: it orders this
	// repository's own guarded writes behind one in-process lock on
	// SQLite, so the view-recording, revocation and sweep writers this
	// module itself spawns never contend with each other for the file
	// lock -- see concurrency.go's own doc comment for why that ordering
	// exists alongside the bounded conflict retry, and why the zero value
	// is ready to use.
	writeMu sync.Mutex
}

// NewShareRepository returns a ShareRepository backed by db.
func NewShareRepository(db *gorm.DB) *ShareRepository {
	// db may be nil on a Service constructed only for identity checks
	// (NewModule(nil) -- module_test.go's TestModule_Identity); the
	// dialect probe is guarded so construction still succeeds, and any
	// actual I/O on such a Service panics exactly as it always did.
	// (db.Name() is gorm's own Dialector.Name promoted through the
	// embedded dialector -- "sqlite" for the SQLite driver, "postgres"
	// for PostgreSQL.)
	serialize := db != nil && db.Name() == "sqlite"
	return &ShareRepository{
		Repository:      dbkit.NewRepository[Share](db),
		db:              db,
		serializeWrites: serialize,
	}
}

// byTokenHash returns the caller tenant's share whose stored hash is hash,
// or ErrNotAccessible.
//
// The tenant scoping is the security property that matters here, mirroring
// org.InvitationRepository.byTokenHash's identical reasoning: a token
// minted for another tenant simply does not match under this tenant's
// scope, so nothing about its existence can be learned from this call
// alone. Service.Access reports the plain ErrNotAccessible (rather than a
// code naming "not found") specifically so a caller cannot distinguish this
// outcome from a revoked, expired or view-exhausted share.
func (r *ShareRepository) byTokenHash(ctx context.Context, hash string) (*Share, error) {
	var share Share
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("token_hash = ?", hash).First(&share).Error
	})
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, ErrNotAccessible
	case err != nil:
		return nil, ErrInternal.WithCause(err)
	}
	return &share, nil
}

// tryRecordView attempts to record one granted access against share's own
// expectation of the row's current state: it succeeds only if, at the
// moment the UPDATE actually runs, the row is still exactly as share
// describes it -- same view count, still not revoked, still not expired as
// of now, and (if MaxViews is set) still under it -- and reports whether
// this call is the one that recorded the view.
//
// When the attempt IS the one that records the view, the caller's
// grantedEntry -- the access's granted log row, never nil on the
// Service.recordView path -- is inserted in the SAME transaction, so the
// count and its trail commit or roll back together: a granted access whose
// log row cannot be written rolls the count back with it instead of
// leaving the share exhausted by an access that failed (Service.Access's
// own doc comment). grantedEntry is written only by the winning attempt:
// a retried attempt whose WHERE clause no longer matches -- because a
// concurrent writer committed a view between attempts -- affects zero
// rows, reports won == false and inserts nothing, so no duplicate log row
// can survive a lost race.
//
// The count-and-trail atomicity above holds for every failure INSIDE the
// transaction; one failure sits OUTSIDE it, and this method answers that
// cell below rather than leaving it to the caller's re-read loop. dbkit's
// commit-time-failure cell (WithTenantSession's own doc comment and
// go/dbkit/AGENTS.md's "commit-time failure" entry) is the commit itself
// reported failed while the fn's writes actually stuck, durably and
// together: the retried attempt then finds its WHERE clause no longer
// matching -- the count it premised on is gone -- and must not read the
// zero rows as "a concurrent writer took the view". The one state that
// distinguishes "someone else committed" from "I committed" is the granted
// log row: only this logical access's own winning attempt could have
// inserted a row under grantedEntry.ID (minted once per logical access by
// Service.accessLogEntry and carried unchanged through every retry of that
// access), and the row commits in the same transaction as the increment it
// trails, so a row present under that id means this access's own view was
// already spent. The zero-row branch therefore probes for its own row
// before reporting the loss: a row found answers won == true -- the caller
// serves the access its committed attempt already paid for, no second view
// is spent and no duplicate row written -- while an absent row leaves the
// loss standing for the caller's re-read-and-retry loop to arbitrate
// against the row's honest current state. (The probe is a model-anchored
// First against the log table, so the tenant-scope plugin filters it like
// any other read of this module's own rows.)
//
// This is a compare-and-swap guard expressed entirely through a WHERE
// clause and a struct passed to Updates, deliberately NOT a raw SQL
// increment (view_count = view_count + 1) reached through .Model(...): the
// three bypass entry points named by the discipline table
// (tools/semgrep_rules/raw-gorm-bypass.yml) are .Table/.Model/.Raw, and
// this codebase's own established pattern for a guarded write inside
// dbkit.WithTenantSession -- org.InvitationRepository.acceptIfPending is
// the precedent -- passes a struct to Where/Updates so the table resolves
// from the struct's own TableName rather than an explicit .Model() call.
//
// Since the access route (handler.go) now reserves a limited share's view
// BEFORE its delivery begins (Share.ViewsReserved), this CAS carries a
// reservation clause alongside the liveness and ceiling ones: a LIVE
// reservation (one younger than viewReservationTimeout) refuses the
// increment -- a view the route is mid-delivery on cannot be spent by an
// in-process Access -- while a STALE one (a reservation older than
// viewReservationTimeout, presumed left behind by a serve that died
// without resolving it) does not: the stale-or-free disjunction below lets
// this write converge the interrupted reservation by spending the view it
// held, exactly as tryReserveView's own takeover clause converges one.
// Service.recordView's early exit (hasLiveReservation) is what keeps a
// live reservation from costing the retry loop its whole budget; this
// clause is the write-time enforcement behind that read-time refusal.
//
// Service.Access retries on a lost race (this call returning won == false
// while the row it re-reads is still live) rather than treating ordinary
// concurrency as "not accessible" -- see its own doc comment.
//
// The statement itself runs through runGuardedWrite (concurrency.go):
// on SQLite this repository's guarded writes are ordered behind one
// in-process mutex -- so a view-recording storm and a revoke racing it
// never contend with each other for the SQLite file lock at all -- while
// on PostgreSQL no in-process mutex is taken (the database's own row locks
// arbitrate; see serializeWrites's own doc comment); and each attempt's
// transaction retries up to txRetryBudget times on a transient,
// contention-only database failure (SQLITE_BUSY from a writer outside this
// module, or PostgreSQL's deadlock/serialization failure) rather than
// surfacing as a store error. A lost CAS race is NOT such a failure (it is
// a legitimate won == false), and a retried attempt whose WHERE clause no
// longer matches -- because the concurrent writer that caused the conflict
// committed a view between the attempts -- affects zero rows and reports
// won == false, never a double-count -- the one exception being the
// recognition cell the paragraph above describes, where the retried
// attempt's zero-row update finds its OWN granted row and answers
// won == true, also never a second increment -- so in the ordinary
// lost-race reading the retry changes nothing about how Service.recordView
// interprets this method's answer.
func (r *ShareRepository) tryRecordView(ctx context.Context, share *Share, now time.Time, grantedEntry *AccessLogEntry) (won bool, err error) {
	err = r.runGuardedWrite(ctx, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ?", share.ID).
			Where("view_count = ?", share.ViewCount).
			Where("revoked_at IS NULL").
			Where("expires_at > ?", now).
			Where("max_views IS NULL OR view_count < max_views").
			Where("views_reserved = 0 OR (views_reserved_at IS NOT NULL AND views_reserved_at <= ?)", now.Add(-viewReservationTimeout)).
			Updates(&Share{ViewCount: share.ViewCount + 1})
		if res.Error != nil {
			return res.Error
		}
		won = res.RowsAffected == 1
		if won && grantedEntry != nil {
			return tx.Create(grantedEntry).Error
		}
		// A zero-row UPDATE is normally a lost CAS race against a
		// concurrent writer -- report won == false and let the caller's
		// re-read-and-retry loop (Service.recordView) arbitrate. One cell
		// must not read as a loss: dbkit's commit-time-failure cell (see
		// the doc comment above), where an earlier attempt of THIS
		// logical access had its commit reported failed while the
		// increment and the granted row actually stuck. The row is the
		// one state that distinguishes "someone else committed" from "I
		// committed": only this access's own winning attempt could have
		// inserted a row under grantedEntry.ID, and the row commits in
		// the same transaction as the increment it trails, so a row
		// present under that id means this access's own view was already
		// spent -- refusing would burn it on a MaxViews=1 share with no
		// reclaim. The honest answer is won == true, and the caller
		// serves the access its committed attempt already paid for. An
		// absent row leaves the loss standing.
		if !won && grantedEntry != nil {
			var recorded AccessLogEntry
			if probeErr := tx.Where("id = ?", grantedEntry.ID).First(&recorded).Error; probeErr == nil {
				won = true
				return nil
			} else if !errors.Is(probeErr, gorm.ErrRecordNotFound) {
				return probeErr
			}
		}
		return nil
	})
	if err != nil {
		return false, ErrInternal.WithCause(err)
	}
	return won, nil
}

// tryReserveView takes a MaxViews-limited share's single in-flight view
// reservation for a serve that is about to begin: it sets
// views_reserved = 1 (with views_reserved_at = now) and reports whether
// this call is the one that took the reservation. It succeeds only if, at
// the moment the UPDATE runs, the share is still live -- not revoked, not
// expired, under its MaxViews ceiling -- and not already reserved by a
// live serve: the WHERE clause is the access route's in-use refusal and
// its ceiling check in one, so two concurrent fetches of a limited share
// can never both begin delivering (Service.reserveAccessView's doc comment
// has the full reserve-before-serve reasoning).
//
// A reservation that has OUTLIVED viewReservationTimeout does not refuse:
// it is presumed interrupted -- the serve that took it is gone without
// having resolved it (a resolved serve always confirms or refunds within
// the delivery it owns) -- and this call takes it over, refreshing
// views_reserved_at to now in the same write. That takeover is the
// "converged by the next access" half of an interrupted reservation's
// lifecycle; the expiry sweep's reservation arm (cleanup.go) is the other.
//
// The statement is a struct-based partial update (only views_reserved and
// views_reserved_at are written), so the tenant-scope GORM plugin engages
// exactly as it does for every other struct Updates call in this module,
// and the WHERE clause's stale-or-free disjunction is re-evaluated on
// every attempt of the guarded write's retry envelope (concurrency.go) --
// a retried attempt that lost the race affects zero rows and reports
// won == false, never two reservations. The refuser need not belong to
// another caller, either: on dbkit's commit-time-failure cell
// (WithTenantSession's own doc comment) a retried attempt can be losing to
// its OWN earlier attempt -- the reservation that attempt committed is
// still visible, still younger than viewReservationTimeout, and rightly
// refuses the retry exactly as a live reservation refuses any other
// contender. That self-reservation needs no behavior of its own here,
// because a reservation is never a consumption: the losing attempt serves
// nothing, so no view is spent, and the held reservation outlives
// viewReservationTimeout into staleness, where the next access's
// stale-or-free takeover (the "converged by the next access" half above)
// or the expiry sweep's reservation arm (cleanup.go) converges it -- the
// row state the takeover converges is the same whether the interrupted
// serve was this caller's own earlier attempt or another caller's.
func (r *ShareRepository) tryReserveView(ctx context.Context, share *Share, now time.Time) (won bool, err error) {
	err = r.runGuardedWrite(ctx, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ?", share.ID).
			Where("revoked_at IS NULL").
			Where("expires_at > ?", now).
			Where("max_views IS NOT NULL").
			Where("view_count < max_views").
			Where("views_reserved = 0 OR (views_reserved_at IS NOT NULL AND views_reserved_at <= ?)", now.Add(-viewReservationTimeout)).
			Updates(&Share{ViewsReserved: 1, ViewsReservedAt: &now})
		won = res.RowsAffected == 1
		return res.Error
	})
	if err != nil {
		return false, ErrInternal.WithCause(err)
	}
	return won, nil
}

// tryConfirmView settles a taken reservation into a spent view: it
// increments view_count, clears the reservation (views_reserved = 0,
// views_reserved_at = NULL) and -- when this call is the one that landed --
// inserts the caller's grantedEntry -- the serve's granted log row, never
// nil on the Service.confirmAccessView path -- in the SAME transaction,
// so the spend and its trail commit or roll back together (the identical
// count-and-trail atomicity tryRecordView documents for its own winning
// write). Service.confirmAccessView drives this after a serve's content
// was FULLY delivered: a delivered serve's reservation is never refunded,
// only confirmed -- see that method's doc comment for the two-directions
// reasoning this statement enforces at the database.
//
// The WHERE clause re-checks liveness and ceiling at confirm time, exactly
// as tryRecordView's own settle-time guards do: a share revoked or expired
// while its delivery was in flight refuses the confirm (the delivery is
// settled as denied and the reservation refunded by the caller instead,
// the same settle-time-liveness semantics ee20d37 established), and the
// ceiling check keeps the count from ever exceeding max_views even in the
// one corner where two serves can overlap -- a stale reservation taken
// over by a newer fetch while its original, still-alive serve later
// completes (see viewReservationTimeout's own doc comment for that
// recorded residual). The views_reserved = 1 predicate ties the spend to a
// reservation actually standing: a confirm whose reservation a concurrent
// writer already resolved affects zero rows and is settled as denied.
//
// This is a raw Exec for the same reasons tryIncrementView's own doc
// comment documents in full: the statement needs genuine server-side
// arithmetic (view_count = view_count + 1, which a struct-based partial
// update cannot express without first reading the count) and explicit
// clears to zero and NULL (which a struct-based partial update skips as
// zero values). Exec bypasses the tenant-scope plugin's callback chain, so
// the tenant_id predicate below is hand-written, exactly as
// tryIncrementView's is -- the isolation proof
// TestShareRepository_TryConfirmView_ScopedToOneTenant is the test this
// shape requires.
func (r *ShareRepository) tryConfirmView(ctx context.Context, share *Share, now time.Time, grantedEntry *AccessLogEntry) (won bool, err error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return false, err
	}
	err = r.runGuardedWrite(ctx, func(tx *gorm.DB) error {
		res := tx.Exec(
			`UPDATE `+tableShares+` `+
				`SET view_count = view_count + 1, views_reserved = 0, views_reserved_at = NULL, updated_at = ? `+
				`WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL AND expires_at > ? `+
				`AND views_reserved = 1 AND view_count < max_views`,
			now, share.ID, string(tenant), now)
		if res.Error != nil {
			return res.Error
		}
		won = res.RowsAffected == 1
		if won && grantedEntry != nil {
			return tx.Create(grantedEntry).Error
		}
		return nil
	})
	if err != nil {
		return false, ErrInternal.WithCause(err)
	}
	return won, nil
}

// tryRefundView releases a taken reservation back to the share without
// spending a view: it clears views_reserved and views_reserved_at, so a
// serve that failed before its content was delivered leaves the share
// exactly as it found it -- the delivery-failure arm of the
// reserve/confirm/refund shape (Service.refundAccessView). A call that
// finds no reservation standing (views_reserved = 0: already confirmed,
// already refunded, or cleared by a revoke) affects zero rows and reports
// won == false -- refunds are idempotent under the guarded write, exactly
// as the resolution arms of go/billing's CreditService are. No liveness
// predicate: a refund also clears a reservation on a row a concurrent
// revoke transitioned meanwhile, which is exactly the cleanup that row
// needs.
//
// Raw Exec for the identical reason tryConfirmView's is: the statement
// clears a column to zero and one to NULL, which a struct-based partial
// update cannot express; the hand-written tenant_id predicate below is the
// isolation mechanism (see tryConfirmView's doc comment), proven by
// TestShareRepository_TryRefundView_ScopedToOneTenant.
func (r *ShareRepository) tryRefundView(ctx context.Context, shareID string, now time.Time) (won bool, err error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return false, err
	}
	err = r.runGuardedWrite(ctx, func(tx *gorm.DB) error {
		res := tx.Exec(
			`UPDATE `+tableShares+` `+
				`SET views_reserved = 0, views_reserved_at = NULL, updated_at = ? `+
				`WHERE id = ? AND tenant_id = ? AND views_reserved = 1`,
			now, shareID, string(tenant))
		won = res.RowsAffected == 1
		return res.Error
	})
	if err != nil {
		return false, ErrInternal.WithCause(err)
	}
	return won, nil
}

// tryIncrementView records one granted access against share's row with a
// single atomic server-side increment, reporting whether the update was the
// one that landed. Service.recordView uses this for an UNLIMITED share
// (MaxViews nil) -- see recordView's own doc comment for why the
// compare-and-swap guard of tryRecordView is the wrong tool there.
//
// When the increment IS the one that lands, the caller's grantedEntry --
// the access's granted log row, never nil on the Service.recordView path
// -- is inserted in the SAME transaction, exactly as tryRecordView commits
// it alongside its own winning UPDATE: the count and its trail commit or
// roll back together (tryRecordView's own doc comment has the reasoning,
// which is identical here).
//
// # Why this is a raw Exec, not a struct-based Updates call
//
// The increment needs genuine server-side arithmetic ("view_count =
// view_count + 1"): GORM's struct-based partial update can only SET a
// column to a fixed value, and a fixed value derived from this caller's
// own read of ViewCount loses increments when two viewers race -- each
// writes its own read-plus-one and one granted view silently vanishes from
// the count. The three GORM entry points this codebase's raw-gorm-bypass
// semgrep rule flags as Repository workarounds are .Table/.Model/.Raw; a
// plain .Exec(sql, args...) is a different, narrower surface that rule does
// not (and, per its own "Residual gaps" note, deliberately cannot) catch
// -- the exact "raw SQL escape hatch" backend-coding-standards SKILL.md
// §3.2 sanctions for a genuine need like this one, PROVIDED the tenant is
// passed explicitly and the call carries an isolation test. The precedent
// is go/billing's applyBalanceDelta (credit_service.go), the identical
// shape: server-side arithmetic .Exec with the tenant bound into the WHERE
// clause by hand, because .Exec bypasses the ORM callback chain entirely
// and the tenant-scope plugin therefore does NOT auto-filter it -- the
// hand-written tenant_id predicate below is the isolation mechanism, not a
// bypass of an injected guard, and every call site runs inside
// dbkit.WithTenantSession, so on PostgreSQL the row-level-security GUC is
// engaged as a second, database-level backstop underneath it. The
// view-count isolation proof is TestShareRepository_TryIncrementView_ScopedToOneTenant.
//
// The WHERE clause doubles as the liveness guard: revocation or expiry
// between Service's own isLive check and this statement refuses the
// increment (RowsAffected 0), exactly as tryRecordView's own WHERE clauses
// refuse a no-longer-live row.
//
// The statement itself runs through runGuardedWrite (concurrency.go),
// the same ordered-and-retried path tryRecordView and markRevoked use: on
// SQLite this repository's own writers are ordered behind writeMu, and a
// transient, contention-only database failure (SQLITE_BUSY from a writer
// outside this module, or PostgreSQL's deadlock/serialization failure --
// the only conflicts left once PostgreSQL skips the in-process mutex
// entirely, per serializeWrites's own doc comment) retries the whole
// guarded increment from a fresh transaction up to txRetryBudget times
// rather than surfacing as a store error. Each retried attempt is the same
// atomic server-side increment, so a retry can never double-count.
func (r *ShareRepository) tryIncrementView(ctx context.Context, share *Share, now time.Time, grantedEntry *AccessLogEntry) (won bool, err error) {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return false, err
	}
	err = r.runGuardedWrite(ctx, func(tx *gorm.DB) error {
		res := tx.Exec(
			`UPDATE `+tableShares+` `+
				`SET view_count = view_count + 1, updated_at = ? `+
				`WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL AND expires_at > ?`,
			now, share.ID, string(tenant), now)
		if res.Error != nil {
			return res.Error
		}
		won = res.RowsAffected == 1
		if won && grantedEntry != nil {
			return tx.Create(grantedEntry).Error
		}
		return nil
	})
	if err != nil {
		return false, ErrInternal.WithCause(err)
	}
	return won, nil
}

// markRevoked sets share's RevokedAt to at, guarded so that only the first
// caller to reach the row transitions it: the WHERE revoked_at IS NULL
// predicate makes a second, concurrent revoke affect zero rows rather than
// writing over the first one. Service.Revoke and Service.Sweep both call
// this instead of the embedded Repository[Share].Update, whose full-row
// Save of a freshly-read Share would write every column back -- including a
// stale ViewCount that rolls back a view-count increment a concurrent
// granted access committed between the read and the write (see Service.Revoke's
// own doc comment). The struct payload means the tenant-scope plugin
// engages exactly as it does for every other struct Updates call in this
// module (only revoked_at and the auto-updated timestamps are written;
// TenantModel.TenantID is zero and therefore omitted from the SET clause).
//
// The statement itself runs through runGuardedWrite (concurrency.go),
// the same ordered-and-retried path tryRecordView and tryIncrementView
// use: a revoke racing this module's own concurrent view recording never
// contends with it for the SQLite file lock at all (writeMu orders the
// two), and a transient, contention-only database failure from a writer
// outside this module -- SQLite's SQLITE_BUSY, or PostgreSQL's deadlock/
// serialization failure -- retries the whole guarded UPDATE from a fresh
// transaction up to txRetryBudget times rather than surfacing as a store
// error. Only the first attempt to actually land transitions the row (the
// WHERE clause still guards that), so retries cannot double-announce a
// revocation.
func (r *ShareRepository) markRevoked(ctx context.Context, id string, at time.Time) (won bool, err error) {
	err = r.runGuardedWrite(ctx, func(tx *gorm.DB) error {
		res := tx.
			Where("id = ?", id).
			Where("revoked_at IS NULL").
			Updates(&Share{RevokedAt: &at})
		won = res.RowsAffected == 1
		return res.Error
	})
	if err != nil {
		return false, ErrInternal.WithCause(err)
	}
	return won, nil
}

// createWithTokenIndex inserts share and its shareTokenIndex row in one
// database transaction, so a share is never left reachable by its owner
// (an authenticated tenant caller) while being permanently unreachable by
// an anonymous visitor holding the same token -- the inconsistency a
// two-step, non-transactional write could otherwise leave behind
// indefinitely, since nothing else in this module ever repairs a missing
// index row. Service.Create calls this instead of the embedded
// Repository[Share].Create.
//
// The two writes share dbkit.WithTenantSession's single transaction rather
// than two separate calls: the tenant-scope GORM plugin still forces
// share's tenant_id on the first Create exactly as Repository[Share].Create
// itself relies on (it does not touch shareTokenIndex at all, since that
// type implements no dbkit.TenantScoped -- see its own doc comment), and a
// failure on either write rolls back both, never leaving one committed
// without the other.
func (r *ShareRepository) createWithTokenIndex(ctx context.Context, share *Share) error {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return err
	}
	return dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		if err := tx.Create(share).Error; err != nil {
			return err
		}
		idx := &shareTokenIndex{TokenHash: share.TokenHash, TenantID: string(tenant)}
		return tx.Create(idx).Error
	})
}

// tenantForTokenHash resolves the tenant a token hash belongs to, with NO
// tenant predicate anywhere in the query -- the one deliberately narrow
// exception to this module's "every query is tenant-scoped" rule, and the
// mechanism AGENTS.md's "Tenant resolution for an unauthenticated viewer"
// section chose to close round 1's documented gap: a genuinely
// unauthenticated visitor holds no tenant claim, so nothing about their
// request can scope this lookup by tenant before it runs -- that is
// precisely the property this method exists to establish, not violate.
//
// This is not a second byTokenHash and not a general cross-tenant query
// capability: it reads shareTokenIndex, a table that was never tenant-
// scoped to begin with (see that type's own doc comment for the full data-
// domain reasoning), through an ordinary, unfiltered query -- dbkit's
// tenant-scope GORM plugin never engages here at all, because the plugin
// only ever acts on a model implementing dbkit.TenantScoped, and
// shareTokenIndex deliberately does not. Nothing here reaches for raw SQL,
// pkgcore.WithSystemContext, or any other escape hatch around a tenant-
// scoped query -- there is no tenant-scoped query to escape, because this
// method touches a different, narrower table than byTokenHash does. It
// returns a tenant id and nothing else: no ResourceRef, no ShareID, no
// Share row at all, so a caller cannot use this method to learn anything
// about a share beyond which tenant a token's hash belongs to.
//
// Service.AccessPublic is this method's only caller: it resolves the
// tenant here, attaches it to ctx with pkgcore.WithTenant, and re-enters
// the ordinary tenant-scoped Service.Access unchanged -- Access itself
// still performs its own byTokenHash lookup, its own password check, and
// its own outward-identical-answer handling exactly as it always has. An
// unrecognized hash here returns ErrNotAccessible, the same sentinel
// byTokenHash returns for an unrecognized hash under a known tenant, so a
// caller cannot distinguish "no such token anywhere" from "no such token
// in the tenant it otherwise resolved to". The one timing property this
// method does NOT hide is documented at AccessPublic's own doc comment:
// an unrecognized token is answered after this single read, deliberately
// without the argon2id burn Access's recognized-token refusal paths pay
// (that burn on every unknown token was the scanner amplification the
// rules-reinforcement round removed), while a recognized-but-refused
// token pays one extra repository read (Access's own byTokenHash) plus
// its argon2id check on top -- a timing difference rule 5 was never
// written to cover, since it protects "which of these refusal reasons
// applied", not "does this token exist at all", which a valid token's own
// successful use already discloses to whoever holds it.
func (r *ShareRepository) tenantForTokenHash(ctx context.Context, hash string) (pkgcore.TenantID, error) {
	var idx shareTokenIndex
	err := r.db.WithContext(ctx).Where("token_hash = ?", hash).First(&idx).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return "", ErrNotAccessible
	case err != nil:
		return "", ErrInternal.WithCause(err)
	}
	return pkgcore.TenantID(idx.TenantID), nil
}

// listByTenant returns every share of the caller tenant, newest first and
// then by id so the order is total and stable -- the listing
// Service.List (the round-3 owner-facing HTTP surface's sharing_listShares
// operation) serves to a resource owner. Unlike listExpiredOrExhausted this
// is not filtered by liveness: a revoked or expired share still belongs to
// its tenant and an owner-facing listing must still be able to show it (its
// RevokedAt is exactly how the owner learns it is gone).
func (r *ShareRepository) listByTenant(ctx context.Context) ([]Share, error) {
	var out []Share
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Order("created_at DESC, id").Find(&out).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return out, nil
}

// listExpiredOrExhausted returns every live (not yet revoked) row of the
// caller tenant whose ExpiresAt has passed as of now, or whose MaxViews has
// been reached -- the expiry sweep's own listing (cleanup.go).
func (r *ShareRepository) listExpiredOrExhausted(ctx context.Context, now time.Time) ([]Share, error) {
	var out []Share
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("revoked_at IS NULL").
			Where("expires_at <= ? OR (max_views IS NOT NULL AND view_count >= max_views)", now).
			Order("id").
			Find(&out).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return out, nil
}

// listStaleReserved returns every row of the caller tenant that still
// holds a view reservation which has OUTLIVED viewReservationTimeout as of
// now -- the expiry sweep's reservation-arm listing (cleanup.go), whose
// sweep refunds each such reservation on the assumption that the serve
// that took it died without resolving it. Unlike listExpiredOrExhausted
// this listing deliberately carries no revoked_at filter: a reservation
// standing on a revoked or expired row is residue a refund is free to
// clear too (such a row can never serve again, so nothing depends on the
// marker), and the reservation-age predicate alone is what the refund arm
// acts on.
func (r *ShareRepository) listStaleReserved(ctx context.Context, now time.Time) ([]Share, error) {
	var out []Share
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("views_reserved = 1").
			Where("views_reserved_at IS NOT NULL").
			Where("views_reserved_at <= ?", now.Add(-viewReservationTimeout)).
			Order("id").
			Find(&out).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return out, nil
}

// AccessLogRepository is the tenant-scoped data-access type for
// AccessLogEntry. It embeds dbkit.Repository[AccessLogEntry] and adds the
// retried append (createWithRetry) plus the one read shape Service.ListAccessLog
// needs. Denied access rows are appended through createWithRetry here; a
// GRANTED access's row is appended inside ShareRepository's own guarded
// view-recording transaction (tryRecordView/tryIncrementView's grantedEntry
// insert) so the count and its trail commit together -- both writes go to
// the same append-only table the type's own doc comment describes.
type AccessLogRepository struct {
	*dbkit.Repository[AccessLogEntry]

	db *gorm.DB
}

// NewAccessLogRepository returns an AccessLogRepository backed by db.
func NewAccessLogRepository(db *gorm.DB) *AccessLogRepository {
	return &AccessLogRepository{Repository: dbkit.NewRepository[AccessLogEntry](db), db: db}
}

// createWithRetry appends one access log row, retrying the insert through
// the same withTxRetry envelope this module's guarded writes run under
// when the database reports a transient, contention-only failure
// (SQLITE_BUSY, a PostgreSQL deadlock/serialization conflict). A denied
// row carries no share state -- nothing to order against this module's
// other writers and nothing a failed insert leaves inconsistent -- so this
// is deliberately the retry alone, without ShareRepository's writeMu
// ordering: the retry absorbs momentary congestion, and a failure that
// outlasts it surfaces to Service.writeAccessLog as the ErrInternal rule 4
// demands (Service.Access's own doc comment).
func (r *AccessLogRepository) createWithRetry(ctx context.Context, entry *AccessLogEntry) error {
	return withTxRetry(func() error {
		return dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
			return tx.Create(entry).Error
		})
	})
}

// listByShare returns every access log row of the caller tenant recorded
// against shareID, newest first and then by id so the order is total and
// stable -- the exact listing Service.ListAccessLog serves to a resource
// owner.
func (r *AccessLogRepository) listByShare(ctx context.Context, shareID string) ([]AccessLogEntry, error) {
	var out []AccessLogEntry
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("share_id = ?", shareID).
			Order("occurred_at DESC, id").
			Find(&out).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return out, nil
}
