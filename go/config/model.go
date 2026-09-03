package config

import "time"

// row is one stored configuration value in the shared configs table.
//
// The table is platform data, not tenant data (docs/internal/04-data-and-
// tenancy.md lists system-level configuration among its platform-domain
// examples): rows are written and read through this module's service
// methods, which enforce the scope and system-context rules, so the model
// deliberately implements no dbkit.TenantScoped interface and is never
// touched through a dbkit.Repository[T]. The GORM tenant-isolation plugin
// only filters models that opt into tenancy, so a plain *gorm.DB carries no
// tenant filter for this table; tenancytest.AssertNotTenantScoped proves
// the point in this module's own tests.
//
// The primary key is (key, scope, tenant_id): one row per configuration key
// per scope, with tenant_id disambiguating the rows of the tenant tier.
// tenant_id is NOT NULL and holds the empty string on system-tier rows.
// Empty is a deliberate choice over NULL: NULLs are distinct in a
// PostgreSQL unique index, so two system rows for one key could coexist
// under NULL where the empty-string sentinel collapses them into the
// single row the primary key promises. No config row is ever deleted in
// this milestone (see
// go/config/AGENTS.md's known limitations), so the primary key needs no
// tombstone column.
//
// value holds the row's canonical string (see values.go) -- except on
// Sensitive items, where it holds base64(ciphertext) sealed by the host's
// dbkit.Cipher. Whether a row is encrypted is decided by the schema's
// Sensitive flag at read time, never by a marker on the row itself: a
// marker would let an attacker who can read the table tell which columns
// are worth stealing.
type row struct {
	Key      string `gorm:"primaryKey;size:100"`
	Scope    string `gorm:"primaryKey;size:16"`
	TenantID string `gorm:"primaryKey;size:64;column:tenant_id"`
	// Value is the canonical string, or the base64 ciphertext of it on a
	// Sensitive item. Column type TEXT on both dialects: configuration
	// values are small, and TEXT keeps the two migration files identical
	// in spirit.
	Value string `gorm:"type:TEXT"`
	// UpdatedBy is the Actor of the last Set, carried so the audit trail can
	// attribute a row's current state even if the change events that led to
	// it were lost.
	UpdatedBy string `gorm:"size:100;column:updated_by"`
	// UpdatedAt is the moment of the last Set. The poller (see service.go)
	// reads rows whose UpdatedAt is newer than its watermark, so precision
	// to the second is enough.
	UpdatedAt time.Time
}

// TableName names the shared configs table.
func (row) TableName() string { return "configs" }
