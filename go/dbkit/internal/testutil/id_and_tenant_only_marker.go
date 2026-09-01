package testutil

import "github.com/vislake/speed/go/pkgcore"

// IDAndTenantOnlyMarker is a TenantScoped model whose ONLY fields are its
// composite primary key (id, tenant_id) — no other column at all, unlike
// Widget (which also has Name and Value). It exists solely to reproduce and
// guard against a gorm-level edge case: gorm's Update callback computes its
// SET clause by excluding every primary-key column, so when every column of
// T IS the primary key, that computed SET clause is empty and gorm's own
// callback returns before executing any SQL, leaving RowsAffected at its
// zero value regardless of whether the row exists. See Repository's Update
// doc comment in repository.go for the full explanation and the fix this
// fixture guards, and dbkit's AGENTS.md for the public-facing summary.
//
// It is shared, like Widget, between dbkit's unit tier (repository_test.go,
// package dbkit) and its integration tier
// (integration_test/postgres_tenant_isolation_test.go, package dbkit_test)
// rather than defined twice — both sit under go/dbkit/..., so both can
// import this internal package.
type IDAndTenantOnlyMarker struct {
	ID       string `gorm:"primaryKey;size:26"`
	TenantID string `gorm:"primaryKey;size:26;not null"`
}

// GetTenantID returns the tenant IDAndTenantOnlyMarker belongs to, and
// satisfies dbkit's TenantScoped contract.
func (m IDAndTenantOnlyMarker) GetTenantID() pkgcore.TenantID {
	return pkgcore.TenantID(m.TenantID)
}

// IDAndTenantOnlyMarkerTableSQL is the DDL that creates the
// id_and_tenant_only_markers table backing IDAndTenantOnlyMarker.
//
// Unlike Widget, this fixture has no separate per-dialect migration files
// under migrations/{postgres,sqlite}/: its only columns are plain TEXT,
// which is identical, valid DDL under both SQLite and PostgreSQL, so one
// literal here is already the single source of truth for both dialects —
// callers on either tier just db.Exec it directly against their own
// connection instead of applying it through NewTestSQLite or a migration
// registry.
const IDAndTenantOnlyMarkerTableSQL = `CREATE TABLE id_and_tenant_only_markers (id TEXT NOT NULL, tenant_id TEXT NOT NULL, PRIMARY KEY (tenant_id, id))`
