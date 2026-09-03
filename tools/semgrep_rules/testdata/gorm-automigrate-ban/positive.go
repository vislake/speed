// Fixture for tools/semgrep_rules/gorm-automigrate-ban.yml.
// Planted violation: must fire on this file.
// This file is NOT shipped code -- it proves the rule fires.
package fixture

import "gorm.io/gorm"

type Invoice struct {
	gorm.Model
	AmountCents int64
}

func setUpSchema(db *gorm.DB) error {
	// The banned shape: production schema drift through AutoMigrate
	// instead of versioned Atlas-generated SQL.
	return db.AutoMigrate(&Invoice{})
}
