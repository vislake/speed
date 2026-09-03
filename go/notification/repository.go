package notification

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// Repository is the inbox's data path: the only sanctioned way to read and
// write in_app_messages rows.
//
// It is a named type embedding dbkit.Repository[InboxMessage] rather than
// the generic base itself, so the module's consumers and its documentation
// can name the concrete thing they hold -- the same reason org's repository
// and the reference app's notes repository declare their own named types.
// Everything Repository can do is promoted unchanged from the embedded
// base: Create, FindByID, Update, Delete and List, each carrying dbkit's
// tenant-isolation guarantees (the tenant comes from the context, never
// from the caller; a row of another tenant is indistinguishable from a row
// that does not exist, and both report dbkit's record-not-found code).
// FindByDedupeKey below is this type's own query shape, expressed the same
// way PreferenceRepository expresses its two (see its doc comment).
type Repository struct {
	*dbkit.Repository[InboxMessage]

	// db is the same connection the embedded Repository was built on, kept
	// only so FindByDedupeKey can be composed on it. Every use routes
	// through WithTenantSession and a TenantScoped destination.
	db *gorm.DB
}

// NewRepository returns a Repository over db. db must already carry
// dbkit's tenant-isolation plugin -- the *gorm.DB dbkit.Open returns, which
// every host uses -- and the in_app_messages migration must already have
// been applied; the repository performs no I/O of its own at construction.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{
		Repository: dbkit.NewRepository[InboxMessage](db),
		db:         db,
	}
}

// FindByDedupeKey returns the inbox row whose dedupe_key is key, or
// (nil, nil) when no row carries it.
//
// This is the delivery path's redelivery probe: a retried job recomputes
// the same derived key (delivery.go's deriveDeliveryKey) and finds the row
// its first attempt wrote, turning a duplicate insert -- which the global
// UNIQUE index on dedupe_key would refuse -- into the "already delivered"
// answer that lets the retry converge without a second send.
//
// The tenant comes from ctx, and the query is written the way
// go/dbkit/AGENTS.md's "Known limitations" prescribes: built on the same
// *gorm.DB the embedded Repository was built on, against a TenantScoped
// destination, so the GORM isolation plugin still injects WHERE tenant_id
// even though Repository[T]'s own re-verification does not run for the
// call -- and run inside dbkit.WithTenantSession, so the PostgreSQL RLS
// session variable is set for it exactly as it is for every promoted
// method.
func (r *Repository) FindByDedupeKey(ctx context.Context, key string) (*InboxMessage, error) {
	var msg InboxMessage
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("dedupe_key = ?", key).First(&msg).Error
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &msg, nil
}
