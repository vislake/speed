package migrations

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"

	"github.com/vislake/speed/examples/reference-app/internal/testutil/dbschema"
)

// legacyBootDDL is the attestation layer's schema as an earlier boot of this
// app created it: the CREATE ... IF NOT EXISTS statements executed
// imperatively at startup, before the table became this migration set. It
// is frozen here as the upgrade fixture -- a database some deployment's
// earlier release leaves behind -- and the pins below hold the set to
// producing exactly the same schema over it.
const legacyBootDDL = `
CREATE TABLE IF NOT EXISTS smilesim_attestations (
	object_id      VARCHAR(64)  NOT NULL PRIMARY KEY,
	tenant_id      VARCHAR(64)  NOT NULL,
	certificate_id VARCHAR(36)  NOT NULL,
	message        VARCHAR(512) NOT NULL,
	signature      VARCHAR(256) NOT NULL,
	created_at     TIMESTAMP    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_smilesim_attestations_tenant_created ON smilesim_attestations (tenant_id, created_at)`

// TestUpgrade_OverLegacyBootSchema pins the set's two upgrade contracts at
// once, over the real registry:
//
//   - a database whose table was created by the app's earlier imperative
//     boot DDL -- no ledger rows at all -- migrates cleanly (the statements
//     keep IF NOT EXISTS, so they are no-ops there) and the ledger then
//     records the file;
//   - the resulting schema is identical, table and index SQL alike, to the
//     one the same set produces from zero, so the migration set is a
//     faithful replacement for the imperative boot path.
func TestUpgrade_OverLegacyBootSchema(t *testing.T) {
	legacy := dbtest.NewSQLite(t)
	if err := legacy.Exec(legacyBootDDL).Error; err != nil {
		t.Fatalf("create the legacy boot schema: %v", err)
	}

	dbtest.Migrate(t, legacy, dbkit.DialectSQLite, dbtest.Migration{Module: "attestation", FS: FS})

	fresh := dbtest.NewSQLite(t)
	dbtest.Migrate(t, fresh, dbkit.DialectSQLite, dbtest.Migration{Module: "attestation", FS: FS})

	dbschema.AssertSameSchema(t,
		dbschema.SchemaSnapshot(t, legacy),
		dbschema.SchemaSnapshot(t, fresh),
		"the schema after migrating a legacy-boot database vs. the schema the same set creates from zero",
	)

	// The set's file is now a ledger row on the legacy database: the boot
	// after this one is a pure ledger skip, the equivalent of the former
	// imperatively re-run CREATE statements being no-ops.
	var ledgerRows int64
	if err := legacy.Raw("SELECT count(*) FROM schema_migrations WHERE module = 'attestation'").Scan(&ledgerRows).Error; err != nil {
		t.Fatalf("count the ledger rows: %v", err)
	}
	if ledgerRows != 1 {
		t.Fatalf("schema_migrations holds %d attestation rows after the upgrade, want 1 (0001)", ledgerRows)
	}
}
