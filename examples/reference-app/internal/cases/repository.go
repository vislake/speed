package cases

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// Repository is cases' tenant-scoped data-access type. It embeds
// dbkit.Repository[caseRecord] instead of holding a *gorm.DB directly
// (the multi-tenant isolation discipline) --
// Create, FindByID, Update, Delete, List and HardDelete are all promoted
// from the embedded base unchanged -- and carries the child table's own
// dbkit.Repository[casePhotoRecord] as the named field photos below. The
// child repository is deliberately a named field rather than a second
// anonymous embedding: two anonymously embedded dbkit.Repository types with
// different type parameters would give every promoted method name (Create,
// FindByID, ...) two candidates and make each call ambiguous, so only the
// cases repository is embedded and the photos repository is reached as
// r.photos.Create and friends (this package's two tables are two models;
// each gets the generic repository that model's discipline requires, never
// a shared raw handle).
//
// Two cautions about the promoted surface, both deliberate and both
// documented rather than hidden:
//
//   - Update and Delete on the cases side touch ONLY the cases row. The
//     surface ships no update or delete path at all (see the package doc
//     comment's "Known limitations" section), so nothing in this app calls
//     them; a case delete must remove its case_photos rows in the same
//     transaction -- the aggregate shape this file's own
//     createCaseWithPhotos already demonstrates -- and should hide or
//     override the promoted Delete rather than leaving the orphan-prone
//     surface open.
//   - Delete on the photos side is a physical row delete, which is fine:
//     a case_photos row is a pure reference with no soft-delete lifecycle
//     story of its own.
//
// The zero value is not ready to use; construct one with NewRepository.
type Repository struct {
	*dbkit.Repository[caseRecord]

	// photos is the child table's own repository, held as a named field
	// (never a second anonymous embedding -- see the struct's own doc
	// comment above for why), over the very same db (never a second pool).
	photos *dbkit.Repository[casePhotoRecord]

	// db is the *gorm.DB the embedded repositories were built over, kept
	// here for the WithTenantSession calls below. It is the very connection
	// the embedded repositories already use -- never a second pool --
	// exactly as notes' own Repository holds the db it was built over for
	// its own tenant-session queries (the precedent this shape follows).
	db *gorm.DB
}

// NewRepository returns a Repository backed by db. db is expected to come
// from dbkit.Open (directly, or through dbkit/dbtest in tests) -- see
// dbkit.Repository's own doc comment for why db is expected to come from
// Open specifically. It performs no I/O; the two tables and their three
// lookup indexes are this domain's migrations (internal/cases/migrations),
// which the assembly applies before anything can query -- the cases
// component carries the set and the database component applies it during
// the Verify stage. Tests apply the same set through dbtest.Migrate.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{
		Repository: dbkit.NewRepository[caseRecord](db),
		photos:     dbkit.NewRepository[casePhotoRecord](db),
		db:         db,
	}
}

// createCaseWithPhotos inserts the case row and all of its photo rows in
// ONE transaction -- the atomicity Service.Create's contract requires: a
// create request either fully lands (case plus every requested photo) or
// leaves nothing behind, never a half-created case missing photos the
// caller believes it attached. It is cases' own contribution to the
// embedded-repository shape, exactly the way notes' Repository adds its
// own tenant-session queries: Repository[T].Create commits each row on its
// own, and only one transaction can make the multi-row write atomic.
//
// The transaction runs inside dbkit.WithTenantSession, with the tenant
// half of every statement injected by dbkit's tenant-scoping plugin from
// the ctx tenant (go/dbkit/tenant_scope.go): the plugin's create callback
// populates tenant_id on each tenant-scoped model from the session's
// context, so neither the case row nor any photo row can land under a
// tenant other than ctx's -- the identical overwrite-the-caller guarantee
// Repository.Create itself carries. ctx must carry a tenant;
// WithTenantSession fails the whole call closed when it does not.
func (r *Repository) createCaseWithPhotos(ctx context.Context, record *caseRecord, photos []casePhotoRecord) error {
	return dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		if err := tx.Create(record).Error; err != nil {
			return err
		}
		for i := range photos {
			if err := tx.Create(&photos[i]).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// listByTenant returns every case belonging to ctx's tenant, newest
// first (created_at descending, with id as a stable tiebreak for two
// cases created within the same timestamp tick) -- the clinic-wide list
// Service.List answers. The query runs inside dbkit.WithTenantSession
// with the tenant half of the WHERE clause injected by the tenant-scope
// plugin from the ctx tenant -- never a post-query filter and never a
// hand-written tenant_id clause (see go/dbkit/tenant_scope.go) -- so a
// caller can only ever enumerate its own tenant's rows, exactly as
// smilesim's listByPhoto enumerates only its own tenant's. The list is
// deliberately NOT filtered by creator: everyone who treats a patient
// together in one practice must see the same cases, and only the tenant
// boundary hides one (the clinic-wide decision the package doc comment's
// "Shape decision" section records). CreatorUserID stays on the
// row as the recorded attribution of the create request; it is never a
// list key.
func (r *Repository) listByTenant(ctx context.Context) ([]caseRecord, error) {
	var records []caseRecord
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Order("created_at DESC, id DESC").
			Find(&records).Error
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}

// photoObjectTaken reports whether ctx's tenant already has a photo row
// referencing objectID -- Service.Create's pre-flight for the
// "already attached to another case of the same tenant" refusal. The
// lookup runs inside dbkit.WithTenantSession keyed on the object_id column
// with the tenant half of the WHERE clause injected by the tenant-scope
// plugin from the ctx tenant -- deliberately NOT the promoted
// Repository[casePhotoRecord].FindByID, whose id is the photo row's own
// uuid, not the referenced object's id -- so it answers only about ctx's
// own tenant, and an object another tenant references is invisible: the
// pre-flight can never leak another tenant's attachment.
func (r *Repository) photoObjectTaken(ctx context.Context, objectID string) (bool, error) {
	var record casePhotoRecord
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("object_id = ?", objectID).First(&record).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// listPhotosOf returns every photo row of ctx's tenant's case caseID, in
// attachment order (position ascending, with the row id as a stable
// tiebreak) -- the detail read Service.Get answers alongside the case row
// itself. Same WithTenantSession + plugin-injected tenant filter shape as
// listByTenant: a caller can only ever list its own tenant's photo rows,
// and a caseID belonging to another tenant answers an empty list rather
// than an error (the caller's Get would have already failed its own
// FindByID with not-found before this ever runs -- this method's contract
// stays the narrower one).
func (r *Repository) listPhotosOf(ctx context.Context, caseID string) ([]casePhotoRecord, error) {
	var records []casePhotoRecord
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("case_id = ?", caseID).
			Order("position ASC, id ASC").
			Find(&records).Error
	})
	if err != nil {
		return nil, err
	}
	return records, nil
}
