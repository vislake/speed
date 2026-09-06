package integration

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// APIKeyRepository is this module's tenant-scoped data-access type for
// APIKey.
//
// It embeds *dbkit.Repository[APIKey] (Create / FindByID / Update / Delete
// / List promoted unchanged, per the backend coding standard's "business
// repositories embed Repository[T], never hold a raw *gorm.DB" rule) for
// round 1's own query shapes -- "one key by id, inside the tenant" and
// "every key of the tenant". Delete is never called on this table: a key's
// end of life is RevokedAt, set through Update, never a row removal.
//
// This round adds db (the same connection the embedded Repository[APIKey]
// was built on) plus three extra query shapes on top of it --
// createWithHashIndex, tenantForHash, byHash -- the identical construction
// go/org's Repository and go/sharing's ShareRepository both use for their
// own hand-written queries: composed on the same *gorm.DB layer 1 already
// protects (dbkit.WithTenantSession, so the tenant-scope GORM plugin still
// injects "WHERE tenant_id = ?" for a TenantScoped destination), never
// db.Table / db.Model / db.Raw. See model.go's apiKeyHashIndex doc comment
// for why Service.Authenticate needs these three at all.
type APIKeyRepository struct {
	*dbkit.Repository[APIKey]

	db *gorm.DB
}

// NewAPIKeyRepository returns an APIKeyRepository backed by db.
func NewAPIKeyRepository(db *gorm.DB) *APIKeyRepository {
	return &APIKeyRepository{Repository: dbkit.NewRepository[APIKey](db), db: db}
}

// createWithHashIndex inserts key and its apiKeyHashIndex row in one
// database transaction, so a key is never left reachable by its owner (an
// authenticated tenant caller, via List/Rotate/Revoke) while being
// permanently unreachable by Service.Authenticate -- the identical
// reasoning (*sharing.ShareRepository).createWithTokenIndex documents for
// its own analogous pair. Service.Create calls this instead of the embedded
// Repository[APIKey].Create.
//
// The two writes share dbkit.WithTenantSession's single transaction rather
// than two separate calls: the tenant-scope GORM plugin still forces key's
// tenant_id on the first Create exactly as Repository[APIKey].Create itself
// relies on (it does not touch apiKeyHashIndex at all, since that type
// implements no dbkit.TenantScoped -- see its own doc comment), and a
// failure on either write rolls back both, never leaving one committed
// without the other.
func (r *APIKeyRepository) createWithHashIndex(ctx context.Context, key *APIKey) error {
	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		return err
	}
	return dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		if err := tx.Create(key).Error; err != nil {
			return err
		}
		idx := &apiKeyHashIndex{Hash: key.Hash, TenantID: string(tenant)}
		return tx.Create(idx).Error
	})
}

// tenantForHash resolves the tenant a raw key's hash belongs to, with NO
// tenant predicate anywhere in the query -- the one deliberately narrow
// exception to this module's "every query is tenant-scoped" rule, mirroring
// (*sharing.ShareRepository).tenantForTokenHash's identical mechanism and
// doc comment almost word for word. This is not a general cross-tenant
// query capability: it reads apiKeyHashIndex, a table that was never
// tenant-scoped to begin with, through an ordinary, unfiltered query --
// dbkit's tenant-scope GORM plugin never engages here at all, because the
// plugin only ever acts on a model implementing dbkit.TenantScoped, and
// apiKeyHashIndex deliberately does not.
//
// Service.Authenticate is this method's only caller: it resolves the tenant
// here, attaches it to ctx with pkgcore.WithTenant, and continues to byHash
// for the ordinary tenant-scoped read. An unrecognized hash here returns
// ErrAuthenticationFailed, the same sentinel byHash returns for a
// recognized-but-refused (revoked or expired) key, so a caller cannot
// distinguish "no such key anywhere" from "no such key in the tenant it
// otherwise resolved to" -- the identical no-enumeration property
// tenantForTokenHash's own doc comment records for its own timing profile.
func (r *APIKeyRepository) tenantForHash(ctx context.Context, hash string) (pkgcore.TenantID, error) {
	var idx apiKeyHashIndex
	err := r.db.WithContext(ctx).Where("hash = ?", hash).First(&idx).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return "", ErrAuthenticationFailed
	case err != nil:
		return "", ErrInternal.WithCause(err)
	}
	return pkgcore.TenantID(idx.TenantID), nil
}

// touchLastUsed updates exactly one column -- last_used_at -- of the key
// named by keyID, in the tenant of ctx, leaving every other column
// untouched. It is the write Service.Authenticate's recordLastUsed performs
// after a successful authentication (authenticate.go), and the
// single-column scope is the whole point: the alternative, a full-row
// Update of the already-read row, races a concurrent Service.Revoke -- the
// authenticating side read the row while it was still live, and a full-row
// save of that stale copy would write its nil RevokedAt back over the
// revocation the other call just committed. An UPDATE whose SET clause
// names only last_used_at cannot undo a revocation, whichever way the race
// resolves (see recordLastUsed's own doc comment for the full argument).
//
// The tenant filter comes from dbkit's tenant-scope plugin (the statement
// runs inside WithTenantSession against the TenantScoped APIKey model), so
// this can never touch another tenant's row -- the identical construction
// byHash and createWithHashIndex already use.
func (r *APIKeyRepository) touchLastUsed(ctx context.Context, keyID string, at time.Time) error {
	return dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Model(&APIKey{}).Where("id = ?", keyID).Update("last_used_at", at).Error
	})
}

// byHash returns the caller tenant's key whose stored hash is hash, or
// ErrAuthenticationFailed -- mirroring
// (*sharing.ShareRepository).byTokenHash's identical construction. The
// tenant scoping is the security property that matters here: a hash minted
// under another tenant simply does not match under this tenant's scope, so
// nothing about its existence can be learned from this call alone.
// Service.Authenticate reports the plain ErrAuthenticationFailed (never a
// code naming "not found", "revoked" or "expired") specifically so a caller
// cannot distinguish this outcome from any other refusal reason.
func (r *APIKeyRepository) byHash(ctx context.Context, hash string) (*APIKey, error) {
	var key APIKey
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("hash = ?", hash).First(&key).Error
	})
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil, ErrAuthenticationFailed
	case err != nil:
		return nil, ErrInternal.WithCause(err)
	}
	return &key, nil
}
