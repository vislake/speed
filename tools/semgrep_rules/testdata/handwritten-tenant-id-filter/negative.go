// Fixture for tools/semgrep_rules/handwritten-tenant-id-filter.yml.
// Clean control: none of these patterns may fire.
package fixture

import "gorm.io/gorm"

func pluginStyle(db *gorm.DB, tenantID string) {
	var count int64
	// Filters on other columns are legitimate business conditions.
	db.Where("status = ?", "ok").Count(&count)
	db.Where("tenant_id IN ?", []string{tenantID}).Find(&[]Record{}) // IN form is not the hand-written equality
	db.Model(&Record{}).Where("id = ?", "rec-1").Delete(&Record{})
}
