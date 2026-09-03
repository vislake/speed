// Fixture for tools/semgrep_rules/handwritten-tenant-id-filter.yml.
// Planted violations: every pattern shape must fire on this file.
// This file is NOT shipped code -- it proves the rule fires.
package fixture

import "gorm.io/gorm"

func handWritten(db *gorm.DB, tenantID string, ids []string) {
	var count int64
	db.Where("tenant_id = ?", tenantID).Count(&count)                 // fires: Where
	db.Model(&Record{}).Where("status = ? AND tenant_id = ?", "ok", tenantID) // fires: mid-clause
	db.Or("tenant_id = ?", tenantID).Find(&[]Record{})                // fires: Or
	db.Not("tenant_id = ?", tenantID).Find(&[]Record{})               // fires: Not
	db.Having("tenant_id = ?", tenantID).Find(&[]Record{})            // fires: Having
	db.Where("tenant_id   =   ?", tenantID).First(&Record{})          // fires: loose spacing
}
