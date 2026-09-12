package migrations

import (
	"testing"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"

	"github.com/vislake/speed/examples/reference-app/internal/testutil/dbschema"
)

// legacyBootDDL is the smile-simulation domain's schema as an earlier boot
// of this app created it: the CREATE ... IF NOT EXISTS statements the two
// stores executed imperatively at startup, before the tables became this
// migration set. It is frozen here as the upgrade fixture -- a database
// some deployment's earlier release leaves behind -- and the pins below
// hold the set to producing exactly the same schema over it.
const legacyBootDDL = `
CREATE TABLE IF NOT EXISTS smilesim_simulations (
	job_id          VARCHAR(64)  NOT NULL PRIMARY KEY,
	tenant_id       VARCHAR(64)  NOT NULL,
	photo_object_id VARCHAR(64)  NOT NULL,
	options_json    VARCHAR(256) NOT NULL,
	created_at      TIMESTAMP    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_smilesim_simulations_tenant_photo ON smilesim_simulations (tenant_id, photo_object_id);

CREATE TABLE IF NOT EXISTS smilesim_credit_reservations (
	job_id     VARCHAR(64)  NOT NULL PRIMARY KEY,
	tenant_id  VARCHAR(64)  NOT NULL,
	credit_key VARCHAR(128) NOT NULL,
	created_at TIMESTAMP    NOT NULL
)`

// TestUpgrade_OverLegacyBootSchema pins the set's two upgrade contracts at
// once, over the real registry:
//
//   - a database whose tables were created by the app's earlier imperative
//     boot DDL -- no ledger rows at all -- migrates cleanly (the statements
//     keep IF NOT EXISTS, so they are no-ops there) and the ledger then
//     records every file;
//   - the resulting schema is identical, table and index SQL alike, to the
//     one the same set produces from zero, so the migration set is a
//     faithful replacement for the imperative boot path.
func TestUpgrade_OverLegacyBootSchema(t *testing.T) {
	legacy := dbtest.NewSQLite(t)
	if err := legacy.Exec(legacyBootDDL).Error; err != nil {
		t.Fatalf("create the legacy boot schema: %v", err)
	}

	dbtest.Migrate(t, legacy, dbkit.DialectSQLite, dbtest.Migration{Module: "smilesim", FS: FS})

	fresh := dbtest.NewSQLite(t)
	dbtest.Migrate(t, fresh, dbkit.DialectSQLite, dbtest.Migration{Module: "smilesim", FS: FS})

	dbschema.AssertSameSchema(t,
		dbschema.SchemaSnapshot(t, legacy),
		dbschema.SchemaSnapshot(t, fresh),
		"the schema after migrating a legacy-boot database vs. the schema the same set creates from zero",
	)

	// Every file of the set is now a ledger row on the legacy database: the
	// boot after this one is a pure ledger skip, the equivalent of the
	// former imperatively re-run CREATE statements being no-ops.
	var ledgerRows int64
	if err := legacy.Raw("SELECT count(*) FROM schema_migrations WHERE module = 'smilesim'").Scan(&ledgerRows).Error; err != nil {
		t.Fatalf("count the ledger rows: %v", err)
	}
	if ledgerRows != 2 {
		t.Fatalf("schema_migrations holds %d smilesim rows after the upgrade, want 2 (0001 and 0002)", ledgerRows)
	}
}
