package notification

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// PreferenceRepository is the preference matrix's data path: the only
// sanctioned way to read and write notification_preferences rows.
//
// It is a named type embedding dbkit.Repository[NotificationPreference],
// exactly as Repository is for inbox rows (see repository.go's doc comment
// for why), adding the two query shapes Repository[T]'s minimal surface
// cannot express: a lookup by (recipient, type) and a listing of one
// recipient's preferences.
//
// Both are written the way go/dbkit/AGENTS.md's "Known limitations"
// prescribes: built on the same *gorm.DB the embedded Repository was built
// on, against a TenantScoped destination, so the GORM isolation plugin still
// injects WHERE tenant_id = ? even though Repository[T]'s own
// re-verification does not run for the call -- and run inside
// dbkit.WithTenantSession, so the PostgreSQL RLS session variable is set for
// them exactly as it is for every promoted method. Nothing in this file
// hand-writes a tenant_id filter, and nothing reaches for db.Table,
// db.Model or db.Raw.
type PreferenceRepository struct {
	*dbkit.Repository[NotificationPreference]

	// db is the same connection the embedded Repository was built on, kept
	// only so the two query shapes above can be composed on it. Every use
	// routes through WithTenantSession and a TenantScoped destination.
	db *gorm.DB
}

// NewPreferenceRepository returns a PreferenceRepository backed by db. db is
// expected to come from dbkit.Open, already migrated with this module's
// Migrations() -- see dbkit.Repository's own doc comment for why Open
// specifically.
func NewPreferenceRepository(db *gorm.DB) *PreferenceRepository {
	return &PreferenceRepository{
		Repository: dbkit.NewRepository[NotificationPreference](db),
		db:         db,
	}
}

// ByUserAndType returns the recipient's stored preference for one
// notification type, or (nil, nil) when no row exists.
//
// The nil-and-nil return is deliberate, and it is this repository's most
// important contract: in this domain an absent row is a VALUE -- it means
// the type's DefaultChannels apply (see preference.go's doc comment) -- not
// an error. Every consumer treats the two cases through that lens: the
// preference service reads (nil, nil) as "no choice made" and never as a
// failure.
//
// The tenant comes from ctx; a row of another tenant -- even for the same
// recipient and type -- is indistinguishable from a row that does not exist.
func (r *PreferenceRepository) ByUserAndType(ctx context.Context, recipientUserID, typeKey string) (*NotificationPreference, error) {
	var pref NotificationPreference
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("recipient_user_id = ? AND type_key = ?", recipientUserID, typeKey).First(&pref).Error
	})
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &pref, nil
}

// ListByUser returns every preference the recipient has stored in the
// tenant of ctx, ordered by type_key so a consumer rendering the preference
// matrix gets a stable order without imposing one of its own.
func (r *PreferenceRepository) ListByUser(ctx context.Context, recipientUserID string) ([]NotificationPreference, error) {
	var prefs []NotificationPreference
	err := dbkit.WithTenantSession(ctx, r.db, func(tx *gorm.DB) error {
		return tx.Where("recipient_user_id = ?", recipientUserID).Order("type_key").Find(&prefs).Error
	})
	return prefs, err
}
