package storage

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// ObjectRepository is storage's tenant-scoped data-access type for Object.
//
// It embeds *dbkit.Repository[Object], so Create, FindByID, Update, Delete
// and List are promoted unchanged, with isolation layers 2 and 3 (the
// fail-closed tenant check and the RLS session wiring) already applied. On
// top of that it adds the one query shape Repository[T]'s deliberately
// minimal surface cannot express -- a keyset-cursor page ordered by
// (created_at, id), the ordering the objects listing promises.
//
// The cursor page is written the way go/dbkit/AGENTS.md's "Known
// limitations" prescribes as option 1: build the query on the same
// *gorm.DB layer 1 already protects, against a TenantScoped destination, so
// the GORM isolation plugin still injects WHERE tenant_id = ? even though
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
	// on, kept only so the cursor page above can be composed on it. Every
	// use routes through WithTenantSession and a TenantScoped destination;
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

// DerivativeRepository is storage's tenant-scoped data-access type for
// ObjectDerivative. It embeds *dbkit.Repository[ObjectDerivative] and adds
// no query of its own this round: the derive pipeline writes one
// derivative per (object, kind) through the promoted Create, the unique
// index uq_object_derivatives_object_kind is the idempotent-skip backstop,
// and the delete cascade reads and removes derivatives through the promoted
// methods. The empty surface is deliberate -- nothing in this file issues
// an unprotected statement either.
type DerivativeRepository struct {
	*dbkit.Repository[ObjectDerivative]
}

// NewDerivativeRepository returns a DerivativeRepository backed by db. db
// is expected to come from dbkit.Open, already migrated with this module's
// Migrations().
func NewDerivativeRepository(db *gorm.DB) *DerivativeRepository {
	return &DerivativeRepository{Repository: dbkit.NewRepository[ObjectDerivative](db)}
}
