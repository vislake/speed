// Fixture for tools/semgrep_rules/raw-gorm-bypass.yml.
// Planted violations: every pattern shape must fire on this file.
// This file is NOT shipped code -- it proves the rule fires.
package fixture

import "gorm.io/gorm"

type Account struct {
	gorm.Model
	Name string
}

func bypassing(db *gorm.DB, accountID string) {
	var account Account
	db.Table("accounts").Where("id = ?", accountID).First(&account) // fires: .Table
	db.Model(&Account{}).Update("name", "someone")                  // fires: .Model
	var count int64
	db.Raw("SELECT COUNT(*) FROM accounts WHERE name = ?", "x").Scan(&count) // fires: .Raw
	tx := db.Begin()
	tx.Model(&Account{}).Where("id = ?", accountID).Delete(&account) // fires: .Model on tx
}
