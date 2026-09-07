package migrations

import (
	"context"
	"embed"
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/dbkit/dbtest"
	"github.com/vislake/speed/go/pkgcore"
)

// upTo0007 embeds exactly the org migration files 0001..0007 -- the schema
// revision 0007_single_root.sql alone leaves behind, and the revision whose
// single-root index 0008_single_root_live.sql repairs -- in both dialect
// directories, so a test can stage a genuine 0007-era database (real
// registry, real files, real ledger rows) and then upgrade it with the full
// set. The pattern's [1-7] bracket is deliberate: the subset must freeze at
// the 0007 snapshot and must not silently grow when a later migration file
// lands.
//
//go:embed sqlite/000[1-7]_*.sql postgres/000[1-7]_*.sql
var upTo0007 embed.FS

// migrationSetModule is the minimal pkgcore.Module a test needs to feed a
// chosen embed.FS to dbkit.MigrationRegistry, mirroring org's own
// internal/testutil migrationModule for the staged-upgrade tests here (the
// subset FS above is only constructible from inside this package, so the
// stub has to live beside it).
type migrationSetModule struct {
	name string
	fs   embed.FS
}

func (m migrationSetModule) Name() string                   { return m.name }
func (migrationSetModule) DependsOn() []string              { return nil }
func (m migrationSetModule) Migrations() embed.FS           { return m.fs }
func (migrationSetModule) Locales() embed.FS                { return embed.FS{} }
func (migrationSetModule) OpenAPISpec() []byte              { return nil }
func (migrationSetModule) Register(*pkgcore.Registry) error { return nil }

// stageSingleRootUpgrade drives the P1-org-11 upgrade-path proof on one
// real database: apply the 0007-era subset, pin that its single-root index
// still counts a soft-deleted root (the pre-0008 behavior the bug was), then
// upgrade by applying the FULL set through a second registry -- whose ledger
// skip is the real upgrade mechanism -- and pin that the narrowed index now
// lets a live root coexist with the soft-deleted one while still refusing
// two live roots.
func stageSingleRootUpgrade(t *testing.T, db *gorm.DB, dialect dbkit.Dialect) {
	t.Helper()
	ctx := context.Background()

	// Stage 1: a genuine 0007-era database.
	era := dbkit.NewMigrationRegistry()
	if err := era.Register(migrationSetModule{name: "org", fs: upTo0007}); err != nil {
		t.Fatalf("Register(0007-era set): %v", err)
	}
	if err := era.Apply(ctx, db, dialect); err != nil {
		t.Fatalf("apply the 0007-era migration set: %v", err)
	}

	insertRoot := `INSERT INTO org_nodes
		(id, tenant_id, parent_id, path, depth, name, kind, created_at, updated_at, deleted_at, deleted_by)
		VALUES (?, ?, '', ?, ?, ?, ?, ?, ?, ?, '')`
	now := time.Now()

	// The 0007-era index scopes on parent_id = '' alone: a soft-deleted root
	// row still occupies the tenant's single root slot, so a live root
	// cannot exist beside it -- the pre-fix state of the P1-org-11 bug,
	// pinned here so the upgrade's effect is visible in the same test.
	if err := db.Exec(insertRoot, "old-root", "tenant-a", "/old-root/", 0, "Old Root", "group", now, now, now).Error; err != nil {
		t.Fatalf("insert the soft-deleted root row on the 0007-era schema: %v", err)
	}
	eraLiveRootErr := db.Exec(insertRoot, "new-root", "tenant-a", "/new-root/", 0, "New Root", "group", now, now, nil).Error
	assertUniqueViolation(t, eraLiveRootErr, "inserting a live root beside a soft-deleted one on the 0007-era schema")

	// Stage 2: the upgrade. A second registry applies the FULL set; its
	// ledger already records 0001..0007 (same module name, same filenames),
	// so it executes exactly the new files -- 0008_single_root_live.sql and
	// every migration that has landed since (0009_memberships_user_lookup.sql
	// at the time of writing).
	full := dbkit.NewMigrationRegistry()
	if err := full.Register(migrationSetModule{name: "org", fs: FS}); err != nil {
		t.Fatalf("Register(full set): %v", err)
	}
	if err := full.Apply(ctx, db, dialect); err != nil {
		t.Fatalf("upgrade the 0007-era database with the full set: %v", err)
	}

	var ledgerRows int64
	if err := db.Raw("SELECT count(*) FROM schema_migrations WHERE module = 'org'").Scan(&ledgerRows).Error; err != nil {
		t.Fatalf("count the ledger rows: %v", err)
	}
	if ledgerRows != 9 {
		t.Fatalf("schema_migrations holds %d org rows after the upgrade, want 9 (0001..0009)", ledgerRows)
	}

	// The narrowed index (deleted_at IS NULL added to the predicate): the
	// same live-root insert that the 0007-era schema refused now succeeds --
	// the soft-deleted root's slot is free -- while a second LIVE root is
	// still refused.
	if err := db.Exec(insertRoot, "new-root", "tenant-a", "/new-root/", 0, "New Root", "group", now, now, nil).Error; err != nil {
		t.Fatalf("insert a live root beside the soft-deleted one after the upgrade: %v, want success -- 0008's deleted_at IS NULL predicate is not in effect", err)
	}
	secondLiveRootErr := db.Exec(insertRoot, "third-root", "tenant-a", "/third-root/", 0, "Third Root", "group", now, now, nil).Error
	assertUniqueViolation(t, secondLiveRootErr, "inserting a second LIVE root after the upgrade")
}

// assertUniqueViolation fails t unless err is (or looks like) a unique-index
// violation, tolerating an untranslated dialect error message in case gorm's
// error translation did not engage for a raw Exec.
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

// TestMigrations_0008_UpgradeFromA0007EraDatabase is the P1-org-11
// upgrade-path proof on SQLite: a database migrated through 0007 -- the
// state every environment that shipped the 0007 round is in today -- gets
// 0008_single_root_live.sql applied by the ordinary re-run of the migration
// registry, and ends with uq_org_nodes_single_root narrowed to live rows.
func TestMigrations_0008_UpgradeFromA0007EraDatabase(t *testing.T) {
	stageSingleRootUpgrade(t, dbtest.NewSQLite(t), dbkit.DialectSQLite)
}
