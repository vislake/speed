package notification

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// tablePlatformBlacklist is the platform_blacklist table name, shared by the
// model's TableName and by the migration's own header comments.
const tablePlatformBlacklist = "platform_blacklist"

// Platform-blacklist reasons, the closed vocabulary of the
// platform_blacklist.reason column.
//
// The two reasons mirror the two ways an address proves it cannot receive
// messages: a recipient who marks a message as spam (complaint), and a
// transport that reports the address is permanently undeliverable
// (hard_bounce). The writers that produce these records -- the complaint
// webhook and the delivery job's hard-failure leg -- belong to later rounds
// (the delivery job of this round marks the tenant's own contact bounced
// without touching the platform list; see contact.go's MarkBounced), and
// AGENTS.md records the deferral. In this round the table exists so the
// platform-level record of a bad address has a home and an isolation proof
// before any writer needs it.
const (
	BlacklistReasonComplaint  = "complaint"
	BlacklistReasonHardBounce = "hard_bounce"
)

// PlatformBlacklist is one platform-level record that an address may not be
// messaged, whatever tenant would send to it.
//
// # Data domain
//
// Platform data (docs/internal/04-data-and-tenancy.md's data-domain table):
// an address that spams or hard-bounces is bad for every tenant of the
// platform, not just the tenant that sent to it, so the record must be
// visible to a query that scans across tenants (the future writer and the
// deliverability checks that read it). PlatformBlacklist therefore
// deliberately does NOT implement dbkit.TenantScoped -- no GetTenantID, no
// embedded dbkit.TenantModel -- exactly as jobs' jobRecord and dbkit/audit's
// AuditEvent do not, for the reasons their doc comments spell out (a
// TenantScoped model makes dbkit's isolation plugin inject WHERE tenant_id
// into every query, which is precisely the filter this domain must never
// have). Its isolation is proven by tenancytest.AssertNotTenantScoped.
//
// TenantID is nevertheless a real column, written by the future writers
// with the tenant whose send produced the record, and unenforced: it exists
// so an operator can see where a blacklisted address was last encountered,
// the same treatment jobs and audit give their own real tenant columns.
// The schema defaults it to the empty-string sentinel (the audit
// convention -- platform-level rows are never NULL); reads never filter on
// it.
//
// # Shape
//
// Channel is ChannelEmail or ChannelSMS, AddressIndex the blind index of
// the canonical address form -- the only form of the address ever stored:
// the plaintext never reaches this table, exactly as it never reaches
// verified_contacts' queryable columns (see contact.go). The UNIQUE
// (channel, address_index) index the migration creates is the platform-wide
// dedupe: one address can be blacklisted at most once per channel, because
// a second complaint or bounce adds nothing a first record did not say.
type PlatformBlacklist struct {
	// ID is an application-generated UUID, never a database-generated one.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantID is the owning tenant of the send that produced the record,
	// unenforced and never filtered on -- see the doc comment above.
	TenantID string `gorm:"column:tenant_id;size:64;not null"`

	// Channel is ChannelEmail or ChannelSMS.
	Channel string `gorm:"column:channel;size:16;not null"`

	// AddressIndex is the blind index of the blacklisted address; see the
	// doc comment above.
	AddressIndex string `gorm:"column:address_index;size:64;not null"`

	// Reason is BlacklistReasonComplaint or BlacklistReasonHardBounce.
	Reason string `gorm:"column:reason;size:16;not null"`

	// CreatedAt and UpdatedAt are populated by gorm's autoCreateTime /
	// autoUpdateTime, never by a database default (SQLite has no NOW()).
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime"`
}

// TableName names the platform_blacklist table.
func (PlatformBlacklist) TableName() string { return tablePlatformBlacklist }

// PlatformBlacklistRepository is the platform_blacklist data path.
//
// It is deliberately NOT built on dbkit.Repository[T], whose generic
// constraint requires dbkit.TenantScoped and therefore cannot compile
// against PlatformBlacklist even by accident -- the compile-time guarantee
// that this platform-domain table never acquires tenant scoping. It queries
// the plain *gorm.DB dbkit.Open returns directly, the documented pattern
// for identity and platform data (see go/dbkit/AGENTS.md's "Known
// limitations"), and never reaches for db.Table, db.Model or db.Raw.
type PlatformBlacklistRepository struct {
	db *gorm.DB
}

// NewPlatformBlacklistRepository returns a PlatformBlacklistRepository
// backed by db. db is expected to come from dbkit.Open, already migrated
// with this module's Migrations().
func NewPlatformBlacklistRepository(db *gorm.DB) *PlatformBlacklistRepository {
	return &PlatformBlacklistRepository{db: db}
}

// IsBlacklisted reports whether one channel-address pair is on the
// platform blacklist.
//
// The query reads across tenants by design -- a platform-level record must
// be visible to every tenant's deliverability check -- so it never goes
// through dbkit.WithTenantSession and never carries a tenant filter; the
// answer is the same whatever tenant's context is passed. An absent row is
// (false, nil); a database failure surfaces raw for the caller to wrap.
func (r *PlatformBlacklistRepository) IsBlacklisted(ctx context.Context, channel, addressIndex string) (bool, error) {
	var row PlatformBlacklist
	err := r.db.WithContext(ctx).
		Where("channel = ? AND address_index = ?", channel, addressIndex).
		First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
