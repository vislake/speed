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
// / List promoted unchanged) for the module's own query shapes -- "one key
// by id, inside the tenant" and "every key of the tenant". Delete is never
// called on this table: a key's end of life is RevokedAt, set through
// Update, never a row removal.
//
// It adds db (the same connection the embedded Repository[APIKey] was
// built on) plus three extra query shapes on top of it --
// createWithHashIndex, tenantForHash, byHash -- the identical construction
// go/org's Repository and go/sharing's ShareRepository both use for their
// own hand-written queries: composed on the same *gorm.DB the tenant-scope
// machinery protects (dbkit.WithTenantSession, so the tenant-scope GORM
// plugin still injects "WHERE tenant_id = ?" for a TenantScoped
// destination), never db.Table / db.Model / db.Raw. See model.go's
// apiKeyHashIndex doc comment for why Service.Authenticate needs these
// three at all.
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

// ListExpired returns every API key of the tenant in ctx whose ExpiresAt is
// at or before now -- the read the expiry-sweep task
// (Service.SweepExpiredAPIKeys, apikey_sweep.go) exists to serve, answered
// by the idx_integration_api_keys_tenant_expires_at index the schema has
// carried for exactly this query (see migrations/{sqlite,postgres}/
// 0001_create_integration_api_keys.sql's comment on that index). Revoked
// and live keys alike are returned once their expiry has passed: a revoked
// key is as dead as an expired one, and neither will ever be used again,
// which is precisely why both are swept (see SweepExpiredAPIKeys' own doc
// comment for the row-lifecycle reasoning).
func (r *APIKeyRepository) ListExpired(ctx context.Context, now time.Time) ([]APIKey, error) {
	var keys []APIKey
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("expires_at <= ?", now).Find(&keys).Error
	})
	return keys, err
}

// deleteWithHashIndex removes key and its apiKeyHashIndex row in one
// database transaction -- the exact inverse of createWithHashIndex, and the
// one delete path this table has (a key's ordinary end of life is the
// RevokedAt mark; only the expiry sweep physically removes rows, and it
// removes them complete: an APIKey row whose hash-index row survived would
// keep its tenant resolvable by Authenticate forever while returning
// nothing to authenticate -- a permanently dead end that is harmless today
// but accumulates rows the sweep exists to reclaim). Service.
// SweepExpiredAPIKeys calls this per row after ListExpired.
//
// The write targets both rows by their own keys inside the same
// WithTenantSession transaction: the APIKey delete runs under the
// tenant-scope plugin like every other write against that TenantScoped
// model, and the apiKeyHashIndex delete (that model implements no
// dbkit.TenantScoped -- see its own doc comment) is matched by hash alone,
// which is globally unique. A failure rolls back both, so a sweep that
// fails midway leaves the remaining rows for its next run exactly as it
// found them -- never half a pair.
func (r *APIKeyRepository) deleteWithHashIndex(ctx context.Context, key *APIKey) error {
	return dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		if err := tx.Where("id = ?", key.ID).Delete(&APIKey{}).Error; err != nil {
			return err
		}
		return tx.Where("hash = ?", key.Hash).Delete(&apiKeyHashIndex{}).Error
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

// touchLastUsed writes exactly one caller-chosen column -- last_used_at --
// of the key named by keyID, in the tenant of ctx. It is the write
// Service.Authenticate's recordLastUsed performs after a successful
// authentication (authenticate.go), and the single-column scope is the
// whole point: the alternative, a full-row Update of the already-read row,
// races a concurrent Service.Revoke -- the authenticating side read the row
// while it was still live, and a full-row save of that stale copy would
// write its nil RevokedAt back over the revocation the other call just
// committed. A write that carries no revocation state cannot undo a
// revocation, whichever way the race resolves (see recordLastUsed's own doc
// comment for the full argument). UpdatedAt is additionally refreshed by
// gorm's auto-update-time machinery -- see the mechanism note below.
//
// The write is expressed as tx.Where(...).Updates(&APIKey{...}), with gorm
// resolving the target table from the struct passed to Updates rather than
// from an explicit db.Model / db.Table / db.Raw call -- the three bypass
// entry points tools/semgrep_rules/raw-gorm-bypass.yml flags as Repository
// workarounds, and exactly the construction go/sharing's tryRecordView and
// go/org's acceptIfPending document for their own guarded writes. LastUsedAt
// is a *time.Time, and the non-nil &at pointer is what keeps the column in
// the SET clause: gorm's struct-based Updates silently omits a zero-valued
// field, and a nil pointer would be exactly that. UpdatedAt rides along
// through gorm's auto-update-time machinery (an incidental bookkeeping
// touch, never something the revocation race relies on -- the row's guard
// columns are untouched, which is the whole point).
//
// The tenant filter comes from dbkit's tenant-scope plugin (the statement
// runs inside WithTenantSession against the TenantScoped APIKey model, which
// gorm resolves from the Updates payload when no Model is set), so this can
// never touch another tenant's row -- the identical construction byHash and
// createWithHashIndex already use.
func (r *APIKeyRepository) touchLastUsed(ctx context.Context, keyID string, at time.Time) error {
	return dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.
			Where("id = ?", keyID).
			Updates(&APIKey{LastUsedAt: &at}).Error
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
