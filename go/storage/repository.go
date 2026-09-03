package storage

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/vislake/speed/go/dbkit"
)

// ObjectRepository is storage's tenant-scoped data-access type for Object.
//
// It embeds *dbkit.Repository[Object], so Create, FindByID, Update, Delete
// and List are promoted unchanged, with isolation layers 2 and 3 (the
// fail-closed tenant check and the RLS session wiring) already applied. On
// top of that it adds the query shapes Repository[T]'s deliberately minimal
// surface cannot express: a keyset-cursor page ordered by (created_at, id)
// -- the ordering the objects listing promises -- plus the lifecycle
// queries the deletion and sweep services drive (markDeleting's guarded
// state flip, deleteObjectRows' single-transaction row removal, and the
// state/expiry listings a sweep run walks).
//
// The queries are written the way go/dbkit/AGENTS.md's "Known limitations"
// prescribes as option 1: build the query on the same *gorm.DB layer 1
// already protects, against a TenantScoped destination, so the GORM
// isolation plugin still injects WHERE tenant_id = ? even though
// Repository[T]'s own re-verification does not run for that call -- and run
// it inside dbkit.WithTenantSession, so isolation layer 3 (the PostgreSQL
// RLS session variable) is set for it exactly as it is for every promoted
// method. What this file must never do, and does not:
//
//   - hand-write a tenant_id filter. There is no "tenant_id = ?" string in
//     this package; the plugin supplies it.
//   - reach for db.Table, db.Model or db.Raw. Those bypass the plugin
//     entirely.
type ObjectRepository struct {
	*dbkit.Repository[Object]

	// db is the same connection the embedded Repository[Object] was built
	// on, kept only so the queries above can be composed on it. Every use
	// routes through WithTenantSession and a TenantScoped destination;
	// nothing in this file issues an unprotected statement.
	db *gorm.DB
}

// NewObjectRepository returns an ObjectRepository backed by db. db is
// expected to come from dbkit.Open, already migrated with this module's
// Migrations() -- see dbkit.Repository's own doc comment for why Open
// specifically.
func NewObjectRepository(db *gorm.DB) *ObjectRepository {
	return &ObjectRepository{Repository: dbkit.NewRepository[Object](db), db: db}
}

// listPage returns up to limit objects of the caller's tenant ordered by
// (created_at DESC, id DESC) -- newest first, ties broken by id so the
// order is total and stable across engines -- starting after the row named
// by beforeID. Pass an empty beforeID for the first page.
//
// The page is a keyset cursor, not an offset: the caller echoes the last
// row's id as the next page's beforeID, so a page inserted between two
// fetches shifts nothing. Cross-tenant cursor semantics are inherited from
// the promoted FindByID the before-row lookup runs through: a beforeID that
// names no object of the caller's tenant -- including one that exists under
// another tenant -- reports dbkit.ErrRecordNotFound, indistinguishable from
// a cursor that never existed, so a caller can never learn that an id it
// does not own exists at all.
func (r *ObjectRepository) listPage(ctx context.Context, limit int, beforeID string) ([]Object, error) {
	return r.listPageState(ctx, "", limit, beforeID)
}

// listPageState is listPage restricted to one object state. Pass the state
// whose rows the listing is about (ObjectService lists completed objects,
// for instance, so uploading and deleting rows stay invisible through it);
// an empty state disables the restriction entirely, which is listPage's own
// unfiltered contract.
//
// The state filter reaches the keyset cursor too: the row a beforeID names
// must itself be in the requested state, or the cursor reports
// dbkit.ErrRecordNotFound exactly as it would for a cursor that never
// existed. A cursor row that left the state since the caller's last page --
// completed and then deleted by a concurrent sweep, say -- therefore reads
// as "no such row in this listing", never as a page silently resumed from
// the wrong place.
func (r *ObjectRepository) listPageState(ctx context.Context, state string, limit int, beforeID string) ([]Object, error) {
	var objects []Object
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		query := tx.Order("created_at DESC, id DESC").Limit(limit)
		if state != "" {
			query = query.Where("state = ?", state)
		}
		if beforeID != "" {
			before, err := r.findByIDIn(tx, beforeID)
			if err != nil {
				return err
			}
			if state != "" && before.State != state {
				return dbkit.ErrRecordNotFound.WithParam("id", beforeID)
			}
			query = query.Where(
				"(created_at < ?) OR (created_at = ? AND id < ?)",
				before.CreatedAt, before.CreatedAt, before.ID,
			)
		}
		return query.Find(&objects).Error
	})
	if err != nil {
		if hasCode(err, dbkit.ErrRecordNotFound.Code) {
			return nil, err
		}
		return nil, ErrInternal.WithCause(err)
	}
	return objects, nil
}

// findByIDIn resolves one object of the caller's tenant inside the
// transaction tx. It is the promoted FindByID's WHERE shape -- tenant_id is
// supplied by the isolation plugin from the context, never written here --
// returning dbkit.ErrRecordNotFound for a row that does not exist or
// belongs to another tenant.
func (r *ObjectRepository) findByIDIn(tx *gorm.DB, id string) (*Object, error) {
	var object Object
	err := tx.Where("id = ?", id).First(&object).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, dbkit.ErrRecordNotFound.WithParam("id", id)
	}
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return &object, nil
}

// markDeleting advances one completed object of the caller's tenant into
// ObjectStateDeleting -- the first step of the deletion protocol, the step
// that makes a delete resumable: once the row reads "deleting", every later
// step of the protocol can be re-run safely because the object's readers
// already see nothing.
//
// The flip is guarded so it only fires from the completed state, using the
// same WHERE id + SELECT * + Save shape the promoted Repository[T].Update
// uses: the UPDATE carries state = deleting and matches at most the row the
// read just loaded, so a concurrent protocol run cannot flip a row this
// call never saw. A row already in deleting is returned as-is -- the state
// machine's "resume, don't re-mark" rule, which makes a second DeleteObject
// racing an in-flight one converge on the same protocol instead of erroring
// -- and a row in uploading is returned untouched for the caller to refuse
// (only the sweep reclaims uploading rows). A row that is gone by the time
// the guarded write lands reports dbkit.ErrRecordNotFound, exactly like a
// read of a row that never existed.
//
// All of this runs in one transaction: the read, the guarded flip, and the
// re-read that disambiguates a zero-row write are atomic against concurrent
// deleters, so no interleaving can observe a row half-marked.
func (r *ObjectRepository) markDeleting(ctx context.Context, objectID string) (*Object, error) {
	var row *Object
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		found, err := r.findByIDIn(tx, objectID)
		if err != nil {
			return err
		}
		if found.State != ObjectStateCompleted {
			row = found
			return nil
		}
		found.State = ObjectStateDeleting
		res := tx.Where("id = ?", objectID).Select("*").Save(found)
		if res.Error != nil {
			return ErrInternal.WithCause(res.Error)
		}
		if res.RowsAffected == 0 {
			// A concurrent protocol run deleted the row between the read
			// and the write. Re-read to learn the truth: the row is gone
			// (not-found, the caller's delete converges on "already gone")
			// or another writer won the window and the re-read's state
			// answers for it.
			row, err = r.findByIDIn(tx, objectID)
			return err
		}
		row = found
		return nil
	})
	if err != nil {
		if hasCode(err, dbkit.ErrRecordNotFound.Code) {
			return nil, err
		}
		return nil, ErrInternal.WithCause(err)
	}
	return row, nil
}

// finalizeUpload is the upload lifecycle's single state-changing write: it
// moves one row from ObjectStateUploading to ObjectStateCompleted carrying
// the finalized metadata the pipeline derived. The write is conditional --
// only a row that is still uploading AND whose upload window is still open
// at the moment of the write can be finalized:
//
//	WHERE id = ? AND state = 'uploading' AND upload_expires_at > ?
//
// Both conditions are load-bearing, and both exist because this write can
// land long after the caller's entry checks ran. The deadline condition
// makes the upload window a hard one: the completion pipeline checks the
// window when it starts, but only a write-time check keeps a pipeline that
// straddles its own window end from committing after it -- the module's
// rule is that a declaration whose window lapsed is reclaimed as
// never-arriving, and the commit is where that must hold. The state
// condition is the delete protocol's other half: the expiry sweep reclaims
// expired uploads from an unlocked listing, and a completion racing the
// reclaim either commits before the row removal (the reclaim then removes
// the completed row along with it -- legitimate, because a row this write
// refused can never complete after the reclaim listed it) or writes zero
// rows after it (refused here, never a silent success for a row that is
// gone).
//
// The returned bool is whether the write committed. It is false for all
// three refusal shapes -- the row vanished, it is no longer uploading, or
// its window closed -- and the caller re-reads the row to tell them apart.
// Real failures are wrapped in ErrInternal; a zero-row conditional write is
// not an error.
func (r *ObjectRepository) finalizeUpload(ctx context.Context, row *Object, now time.Time) (bool, error) {
	done := false
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.Where(
			"id = ? AND state = ? AND upload_expires_at > ?",
			row.ID, ObjectStateUploading, now,
		).Select("*").Save(row)
		done = res.RowsAffected > 0
		return res.Error
	})
	if err != nil {
		return false, ErrInternal.WithCause(err)
	}
	return done, nil
}

// deleteObjectRows removes one object's rows -- the object row itself and
// then its derivative rows -- in a single transaction, the protocol's
// commit point: before it, every earlier step (mark, byte deletion) is
// resumable by re-running the protocol; after it, no row references the
// deleted bytes anywhere. Deleting zero rows is not an error: the caller
// uses this after its own state checks, and a row another protocol run
// already removed simply reads as nothing left to do.
//
// The returned bool is whether this call removed the object row itself.
// It answers the protocol's one remaining question -- "did the deletion
// commit here?" -- because only the run whose row removal commits may
// announce the deletion: two protocol runs racing over one object both
// survive to this step, but exactly one of them sees a row to remove, so
// exactly one deleted event is published. A run that removed nothing (the
// row vanished between its mark and here) announces nothing.
//
// The object row is deleted before the derivative rows on purpose: the
// object row is the lock insertDerivativeIfAbsent's object-state gate
// holds, and reaching for it first serializes this transaction against
// every concurrent gate-holding insert at the transaction's first
// statement. Deleting the derivative rows first would leave a window no
// lock can close -- a derivative insert that committed while this
// transaction waited on the object row would land after this
// transaction's own derivative-row DELETE had already run, and would
// survive it: a ghost row naming bytes the protocol is about to remove,
// whose object row is gone and that nothing ever walks again. With the
// object row first, the derivative-row DELETE is this transaction's last
// statement, so an insert the gate admitted commits before it (the gate
// holds the object row this transaction is waiting on) and is removed by
// it, and an insert that arrives after this transaction commits is
// refused by the gate.
//
// The two Deletes run through the isolation plugin like every statement in
// this file: WHERE tenant_id = ? is injected from the context, so the
// object row and each derivative row can only be removed by their own
// tenant.
func (r *ObjectRepository) deleteObjectRows(ctx context.Context, objectID string) (removed bool, err error) {
	err = dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		res := tx.Where("id = ?", objectID).Delete(&Object{})
		if res.Error != nil {
			return res.Error
		}
		removed = res.RowsAffected > 0
		delRes := tx.Where("object_id = ?", objectID).Delete(&ObjectDerivative{})
		if delRes.Error != nil {
			return delRes.Error
		}
		return nil
	})
	if err != nil {
		return false, ErrInternal.WithCause(err)
	}
	return removed, nil
}

// listStateRows returns every object row of the caller's tenant in one
// state, oldest first. It is the sweep's resume listing: rows in
// ObjectStateDeleting are in-flight deletions whose protocol was
// interrupted (a crash, a failed store write), and a sweep run re-runs the
// protocol's remaining steps over the whole list.
//
// The listing is deliberately unpaged: a sweep task is tenant-scoped and
// its rows are consumed -- each listed deleting row is either completed or
// left for the next scheduled run when its protocol fails again -- so a
// full list is bounded by the tenant's in-flight deletions, not by its
// lifetime upload volume. Paging a state listing is future work if a
// tenant's backlog ever grows past one task's worth.
func (r *ObjectRepository) listStateRows(ctx context.Context, state string) ([]Object, error) {
	var rows []Object
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("state = ?", state).
			Order("created_at ASC, id ASC").
			Find(&rows).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return rows, nil
}

// listExpiredUploads returns every ObjectStateUploading row of the caller's
// tenant whose upload window closed before the time passed in -- the rows
// the sweep reclaims: their upload never finished, their bytes (if any)
// are never becoming readable, and both are removed. A row whose window
// closes exactly at the sweep's now is not listed: the upload gate refuses
// completions strictly after the window, so the sweep reclaims strictly
// past it and the two never race on the same instant.
//
// Same deliberate shape as listStateRows: unpaged, tenant-scoped, consumed
// by the run that reads it.
func (r *ObjectRepository) listExpiredUploads(ctx context.Context, before time.Time) ([]Object, error) {
	var rows []Object
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("state = ? AND upload_expires_at < ?", ObjectStateUploading, before).
			Order("created_at ASC, id ASC").
			Find(&rows).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return rows, nil
}

// listExpiredCompleted returns every ObjectStateCompleted row of the
// caller's tenant whose retention deadline passed before the time passed
// in. NULL expires_at rows -- objects that never expire -- are excluded
// explicitly, never matched by a NULL comparison: "no deadline" means
// "never", and only rows with a deadline that has passed are listed.
//
// Same deliberate shape as listStateRows: unpaged, tenant-scoped, consumed
// by the run that reads it.
func (r *ObjectRepository) listExpiredCompleted(ctx context.Context, before time.Time) ([]Object, error) {
	var rows []Object
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where(
			"state = ? AND expires_at IS NOT NULL AND expires_at < ?",
			ObjectStateCompleted, before,
		).Order("created_at ASC, id ASC").Find(&rows).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return rows, nil
}

// DerivativeRepository is storage's tenant-scoped data-access type for
// ObjectDerivative. It embeds *dbkit.Repository[ObjectDerivative] and adds
// the two query shapes the derive and delete pipelines need on top of the
// promoted methods: listByObject, the "everything one object has" listing
// the delete protocol walks before removing bytes, and
// insertDerivativeIfAbsent, the idempotent write that makes re-running a
// derive a no-op, its insert additionally gated on the object still being
// completed so the delete/derive race closes at the row level.
type DerivativeRepository struct {
	*dbkit.Repository[ObjectDerivative]

	// db is the same connection the embedded Repository[ObjectDerivative]
	// was built on, kept only so the queries above can be composed on it.
	// Every use routes through WithTenantSession and a TenantScoped
	// destination; nothing in this file issues an unprotected statement.
	db *gorm.DB
}

// NewDerivativeRepository returns a DerivativeRepository backed by db. db
// is expected to come from dbkit.Open, already migrated with this module's
// Migrations().
func NewDerivativeRepository(db *gorm.DB) *DerivativeRepository {
	return &DerivativeRepository{
		Repository: dbkit.NewRepository[ObjectDerivative](db),
		db:         db,
	}
}

// listByObject returns every derivative of one object of the caller's
// tenant, oldest first with the id tiebreak making the order total and
// stable across engines. The delete protocol walks the list to remove each
// derivative's bytes before the rows go; the order makes that byte removal
// deterministic across runs.
func (r *DerivativeRepository) listByObject(ctx context.Context, objectID string) ([]ObjectDerivative, error) {
	var rows []ObjectDerivative
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("object_id = ?", objectID).
			Order("created_at ASC, id ASC").
			Find(&rows).Error
	})
	if err != nil {
		return nil, ErrInternal.WithCause(err)
	}
	return rows, nil
}

// insertDerivativeIfAbsent inserts row unless a derivative of the same
// (object, kind) already exists, in one transaction so the check and the
// insert cannot interleave. The existence check and the UNIQUE
// (tenant_id, object_id, kind) index the migrations declare are the two
// layers of the derive pipeline's idempotent skip: a re-run after a crash
// between the byte write and the row insert converges here on "already
// derived", never on a duplicate row. (The index is the backstop, not the
// primary mechanism -- the pipeline never parses a dialect's unique-
// violation text -- and the jobs layer's idempotency key already collapses
// concurrent derives of one object into one task.)
//
// The insert is additionally gated on the object the row names: a
// derivative row may only be committed while the object's own row still
// exists and reads completed at the moment of the insert, checked inside
// this same transaction as a locked read of the object row (clause.Locking
// -- a genuine FOR UPDATE on PostgreSQL; the SQLite driver has no row
// locks and drops the clause, its single-writer serialization answering a
// concurrent writer with a busy error the caller's retry converges). The
// returned bool is whether the gate refused: true when the object's row is
// gone or not completed -- the delete protocol won the race, and the
// caller drops the bytes it just wrote and converges on nil, never
// committing a derivative row no object row will ever walk -- false when
// the row was inserted or already existed.
//
// The gate precedes the existence check on purpose: the refused caller
// discards its just-written bytes, and that discard is only safe while no
// row can still reference them. Gate first makes refused imply the object
// is not completed -- its rows belong to a delete protocol that is
// removing, or has removed, the bytes it walks -- so the discard never
// deletes content a live row names. An existence check that ran first
// could answer "already derived" for an object the delete protocol was
// mid-way through, leaving the caller's just-written bytes orphaned once
// the protocol's own byte removal had already run.
func (r *DerivativeRepository) insertDerivativeIfAbsent(ctx context.Context, row ObjectDerivative) (bool, error) {
	refused := false
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		// The object-state gate: only a completed object whose row still
		// exists may gain a derivative. The lock is what closes the
		// delete/derive race -- a delete that reaches deleteObjectRows
		// blocks on this row until this transaction commits, and its
		// derivative-row removal runs after this insert in that case;
		// a delete that commits first leaves no completed row to find.
		var object Object
		gateErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND state = ?", row.ObjectID, ObjectStateCompleted).
			First(&object).Error
		if errors.Is(gateErr, gorm.ErrRecordNotFound) {
			refused = true
			return nil
		}
		if gateErr != nil {
			return gateErr
		}
		var existing ObjectDerivative
		findErr := tx.Where("object_id = ? AND kind = ?", row.ObjectID, row.Kind).
			First(&existing).Error
		if findErr == nil {
			// Already derived -- the idempotent skip.
			return nil
		}
		if !errors.Is(findErr, gorm.ErrRecordNotFound) {
			return findErr
		}
		if createErr := tx.Create(&row).Error; createErr != nil {
			return createErr
		}
		return nil
	})
	if err != nil {
		return false, ErrInternal.WithCause(err)
	}
	return refused, nil
}
