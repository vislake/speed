package migrations

import (
	"embed"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
)

// upTo0006 embeds exactly the metering migration files 0001..0006 -- the
// schema revision 0007_enforce_outbox_retry_after_not_null.sql leaves
// behind -- in both dialect directories, so a test can stage a genuine
// 0006-era database (real registry, real files, real ledger rows) and
// then upgrade it with the full set. The pattern's [1-6] bracket is
// deliberate: the subset must freeze at the 0006 snapshot and must not
// silently grow when a later migration file lands. The same staged shape
// go/org/migrations/upgrade_test.go uses for its own single-root index
// repair.
//
//go:embed sqlite/000[1-6]_*.sql postgres/000[1-6]_*.sql
var upTo0006 embed.FS

// stageRetryAfterUpgrade drives the 0007 upgrade-path proof on one real
// database: apply the 0006-era subset (the same module name, so the full
// set's re-run is a genuine ledger-skip upgrade of it), seed rows on the
// nullable schema -- including a NULL-retry_after row, a write only the
// pre-0007 schema accepts -- then upgrade by re-applying the module's full
// set, and pin that 0007 backfilled the NULL row to its created_at, made
// the column NOT NULL (a fresh NULL write is refused), preserved the
// concrete row untouched, and kept the unique index working (a duplicate
// idempotency key is still refused -- the sqlite/ copy's table rebuild
// recreates the indexes, and this is where a dropped index would show).
func stageRetryAfterUpgrade(t *testing.T, db *gorm.DB, dialect dbkit.Dialect) {
	t.Helper()
	now := time.Now()
	createdAt := now.Add(-time.Minute)
	scheduledAt := now.Add(-2 * time.Hour)

	// Stage 1: a genuine 0006-era database.
	dbtest.Migrate(t, db, dialect, dbtest.Migration{Module: "metering", FS: upTo0006})

	insertRow := `INSERT INTO metering_outbox_records
		(id, tenant_id, feature, quantity, idempotency_key, occurred_at, metadata, status, attempts, last_error, retry_after, created_at, delivered_at)
		VALUES (?, ?, 'f', 1, ?, ?, '', 'pending', 0, '', ?, ?, NULL)`

	// A row whose retry_after is NULL: the state module-produced databases
	// never write (0005's backfill converted the pre-0005 rows) but the
	// 0006-era schema still permits -- the exact hole migration 0007
	// closes. On the 0006-era schema this insert succeeds (pinned here);
	// the upgrade must convert it before the constraint lands.
	if err := db.Exec(insertRow, "legacy-null", "tenant-a", "idem-legacy-null", now, nil, createdAt).Error; err != nil {
		t.Fatalf("insert a NULL-retry_after row on the 0006-era schema: %v (the pre-0007 schema must accept this write -- that is the gap the migration closes)", err)
	}
	// A module-written row: retry_after concrete, distinct from created_at
	// (a failed row's schedule). The upgrade's backfill must not touch it.
	if err := db.Exec(insertRow, "scheduled", "tenant-a", "idem-scheduled", now, scheduledAt, createdAt).Error; err != nil {
		t.Fatalf("insert the scheduled row on the 0006-era schema: %v", err)
	}

	// Stage 2: the upgrade. Applying the FULL set re-runs the registry
	// against a database whose ledger already records 0001..0006 (same
	// module name, same filenames), so it executes exactly the new file --
	// 0007_enforce_outbox_retry_after_not_null.sql.
	dbtest.Migrate(t, db, dialect, dbtest.Migration{Module: "metering", FS: FS})

	var ledgerRows int64
	if err := db.Raw("SELECT count(*) FROM schema_migrations WHERE module = 'metering'").Scan(&ledgerRows).Error; err != nil {
		t.Fatalf("count the ledger rows: %v", err)
	}
	if ledgerRows != 7 {
		t.Fatalf("schema_migrations holds %d metering rows after the upgrade, want 7 (0001..0007)", ledgerRows)
	}

	type rowTimes struct {
		RetryAfter *time.Time
		CreatedAt  time.Time
	}
	readTimes := func(id string) rowTimes {
		t.Helper()
		var got rowTimes
		if err := db.Raw("SELECT retry_after, created_at FROM metering_outbox_records WHERE id = ?", id).Scan(&got).Error; err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		return got
	}

	// The NULL row was backfilled to its own created_at -- 0005's own
	// idempotent backfill statement, re-run in-file, so the NOT NULL that
	// follows it can never fail on a 0006-era database. Both values are
	// read from the database in one scan, so the comparison is
	// zone-consistent on either dialect.
	legacy := readTimes("legacy-null")
	if legacy.RetryAfter == nil {
		t.Fatal("legacy-null retry_after is still NULL after the upgrade, want it backfilled to created_at")
	}
	if !legacy.RetryAfter.Equal(legacy.CreatedAt) {
		t.Errorf("legacy-null retry_after = %v after the upgrade, want its created_at %v (0007's backfill must convert the NULL row before the constraint lands)", legacy.RetryAfter, legacy.CreatedAt)
	}
	// The concrete schedule was not clobbered by the backfill. The
	// comparison runs as a database-side equality probe binding the
	// original Go value, so each driver's own timestamp encoding decides
	// both sides -- a Go-side comparison of a driver-decoded time against
	// the original local-time value would be zone-fragile across dialects.
	var scheduledOK int64
	if err := db.Raw("SELECT count(*) FROM metering_outbox_records WHERE id = 'scheduled' AND retry_after = ?", scheduledAt).Scan(&scheduledOK).Error; err != nil {
		t.Fatalf("probe the scheduled row: %v", err)
	}
	if scheduledOK != 1 {
		t.Errorf("scheduled retry_after no longer equals its seeded %v after the upgrade (the backfill must only fill NULLs, never rewrite a concrete schedule)", scheduledAt)
	}

	// NOT NULL is now enforced: the same NULL-retry_after write the
	// 0006-era schema accepted is refused.
	if err := db.Exec(insertRow, "refused-null", "tenant-a", "idem-refused-null", now, nil, createdAt).Error; err == nil {
		t.Fatal("inserting a NULL-retry_after row after the upgrade succeeded, want a NOT NULL constraint refusal")
	} else if msg := strings.ToLower(strings.ReplaceAll(err.Error(), "-", " ")); !strings.Contains(msg, "not null") {
		t.Fatalf("NULL-retry_after insert error after the upgrade = %v, want a NOT NULL constraint refusal", err)
	}

	// The unique index survived the upgrade (on SQLite the table rebuild
	// drops it with the old table, so a working refusal proves 0007
	// recreated it): a duplicate (tenant_id, idempotency_key) is refused.
	dupErr := db.Exec(insertRow, "dup-key", "tenant-a", "idem-legacy-null", now, scheduledAt, createdAt).Error
	assertUniqueViolation(t, dupErr, "inserting a duplicate (tenant_id, idempotency_key) after the upgrade")

	// Exactly the two seeded rows landed -- the two refused inserts added
	// nothing.
	var rowCount int64
	if err := db.Raw("SELECT count(*) FROM metering_outbox_records").Scan(&rowCount).Error; err != nil {
		t.Fatalf("count the rows: %v", err)
	}
	if rowCount != 2 {
		t.Fatalf("metering_outbox_records holds %d rows after the upgrade, want 2 (both refused inserts must have left no row)", rowCount)
	}
}

// assertUniqueViolation fails t unless err is (or looks like) a
// unique-index violation, tolerating an untranslated dialect error message
// in case gorm's error translation did not engage for a raw Exec.
func assertUniqueViolation(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: statement succeeded, want a unique-constraint violation", what)
	}
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "unique") && !strings.Contains(msg, "duplicate") {
		t.Fatalf("%s: error = %v, want a unique-constraint violation", what, err)
	}
}

// TestMigrations_0007_UpgradeFromA0006EraDatabase is the 0007
// upgrade-path proof on SQLite: a database migrated through 0006 -- the
// schema state that predates the NOT NULL enforcement -- gets
// 0007_enforce_outbox_retry_after_not_null.sql applied by the ordinary
// re-run of the migration registry, its NULL row backfilled, its column
// NOT NULL from then on, and its table (rebuilt on this dialect) fully
// intact.
func TestMigrations_0007_UpgradeFromA0006EraDatabase(t *testing.T) {
	stageRetryAfterUpgrade(t, dbtest.NewSQLite(t), dbkit.DialectSQLite)
}
