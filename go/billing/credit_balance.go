package billing

import (
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// billingCreditBalancesTable names the shared billing_credit_balances table.
const billingCreditBalancesTable = "billing_credit_balances"

// CreditBalance is one tenant's current credit position: how many credits
// are freely spendable (Available) and how many are provisionally set
// aside by an in-flight PreDeduct that has not yet been confirmed or
// refunded (Reserved). The two never overlap -- a credit is in exactly one
// of the two buckets, or already permanently spent (removed from both,
// once PreDeduct's reservation is Confirmed).
//
// This row is never written directly: every mutation goes through
// CreditService, whose PreDeduct/Confirm/Refund/Grant/Expire methods use a
// database-arbitrated compare-and-swap UPDATE (never a read-modify-write)
// so concurrent operations against the same tenant's balance cannot both
// succeed when only one of them fits -- see CreditService's own doc
// comment for the full mechanism and CreditBalanceRepository's for why a
// row always exists once first touched (an implicit zero-balance row is
// created under the same CAS discipline, never left to a plain
// FindOrCreate race).
//
// ID is the row's own primary key, set equal to the owning tenant's id --
// there is exactly one CreditBalance row per tenant, so a second,
// independent id would only be an opportunity for the wrong one to be
// looked up by mistake. This is the same "make the natural key the
// primary key" choice go/metering's UsageSummary.ID makes for its own
// (tenant, feature, period) uniqueness, adapted to CreditBalance's simpler
// (tenant) uniqueness.
type CreditBalance struct {
	dbkit.TenantModel

	// ID equals the owning tenant's id (see the type's own doc comment).
	ID string `gorm:"column:id;primaryKey;size:64"`

	// Available is the freely spendable credit count.
	Available int64 `gorm:"column:available;not null"`

	// Reserved is the credit count set aside by a not-yet-resolved
	// PreDeduct.
	Reserved int64 `gorm:"column:reserved;not null"`

	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime;not null"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime;not null"`
}

// TableName pins CreditBalance to the billing_credit_balances table.
func (CreditBalance) TableName() string { return billingCreditBalancesTable }

// CreditBalanceRepository is the tenant-scoped accessor for
// billing_credit_balances. CreditBalance is tenant data, so this embeds
// dbkit.Repository[CreditBalance] and inherits all three tenant-isolation
// layers. CreditService is the only caller: every real mutation goes
// through its compare-and-swap methods (CreditBalanceRepository's own
// promoted Create/Update are used only to materialize the tenant's first,
// zero-valued row -- see CreditService.ensureBalance).
type CreditBalanceRepository struct {
	*dbkit.Repository[CreditBalance]
}

// NewCreditBalanceRepository returns a CreditBalanceRepository over db. db
// is expected to come from dbkit.Open with this module's migrations
// applied.
func NewCreditBalanceRepository(db *gorm.DB) *CreditBalanceRepository {
	return &CreditBalanceRepository{Repository: dbkit.NewRepository[CreditBalance](db)}
}
