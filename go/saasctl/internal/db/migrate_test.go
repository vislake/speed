package db

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/saasctl/internal/appconfig"
)

// bootstrapEnvKeys lists the environment surface a generated project's
// bootstrap reads -- the five variables appconfig resolves, exported for
// the tests and examples that must clear or restore them all.
var bootstrapEnvKeys = []string{
	appconfig.DeploymentModeEnv,
	appconfig.PortEnv,
	appconfig.DBPathEnv,
	appconfig.ConfigKeyEnv,
	appconfig.OrgIndexKeyEnv,
}

// clearBootstrapEnv empties every bootstrap variable through t.Setenv, so
// a test starts from the same all-defaults state a fresh shell would be
// in. Empty counts as unset -- matching os.Getenv, the generated app's
// own view of the environment.
func clearBootstrapEnv(t *testing.T) {
	t.Helper()
	for _, key := range bootstrapEnvKeys {
		t.Setenv(key, "")
	}
}

// driveMigrate runs the migrate subcommand through the group's Run under
// a cleared bootstrap environment plus env, and returns its exit code and
// the two captured output streams.
func driveMigrate(t *testing.T, extraArgs []string, env map[string]string) (code int, stdout, stderr string) {
	t.Helper()
	clearBootstrapEnv(t)
	for key, value := range env {
		t.Setenv(key, value)
	}
	args := []string{"migrate"}
	args = append(args, extraArgs...)
	var out, errOut bytes.Buffer
	code = Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// fixture returns the absolute path of one go.mod fixture in testdata.
func fixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", name)
}

// openDB reopens the SQLite file at path through dbkit.Open, the same
// way the command itself opens it, so a test can inspect what a run left
// behind.
func openDB(t *testing.T, path string) *gorm.DB {
	t.Helper()
	gdb, err := dbkit.Open(context.Background(), dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: path})
	if err != nil {
		t.Fatalf("reopen %s: %v", path, err)
	}
	t.Cleanup(func() {
		if sqlDB, err := gdb.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return gdb
}

// ledgerCounts returns how many migration files schema_migrations records
// per module in the open database.
func ledgerCounts(t *testing.T, gdb *gorm.DB) map[string]int {
	t.Helper()
	ledger, err := readLedger(gdb)
	if err != nil {
		t.Fatalf("read the migration ledger: %v", err)
	}
	counts := make(map[string]int, len(ledger))
	for module, filenames := range ledger {
		counts[module] = len(filenames)
	}
	return counts
}

// fullUniverseLedger is the ledger a fully migrated database carries.
//
// The counts are drift guards against the modules' own migration sets:
// authn ships nine sqlite migration files (0001_create_users through
// 0009_create_user_recovery_codes), config one (0001_create_configs), org
// three (0001_create_org_nodes, 0002_create_memberships,
// 0003_create_org_invitations) and rbac one (0001_create_rbac) -- 14 in
// total, tables configs, users, org_nodes and rbac_roles among them. A
// module adding or removing a migration file fails the tests that pin
// these numbers, so the expectation here is updated deliberately when the
// module's migration set really changes, never silently.
var fullUniverseLedger = map[string]int{
	"authn":  9,
	"config": 1,
	"org":    3,
	"rbac":   1,
}

// TestMigrateFreshDatabaseAppliesTheWholeRequiredUniverse: a first run
// against a database that does not exist yet applies every migration file
// of every migration-shipping module the go.mod requires, reports exactly
// which files it applied, and leaves the real tables and the complete
// schema_migrations ledger behind.
func TestMigrateFreshDatabaseAppliesTheWholeRequiredUniverse(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cli-app.db")
	code, stdout, stderr := driveMigrate(t, []string{fixture(t, "full.mod")},
		map[string]string{appconfig.DBPathEnv: dbPath})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	want := fmt.Sprintf("Migrated %s: applied 14 migration files (authn 9, config 1, org 3, rbac 1)\n", dbPath)
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	gdb := openDB(t, dbPath)
	if got := ledgerCounts(t, gdb); !reflect.DeepEqual(got, fullUniverseLedger) {
		t.Errorf("ledger = %v, want %v", got, fullUniverseLedger)
	}
	for _, table := range []string{"schema_migrations", "configs", "users", "org_nodes", "rbac_roles"} {
		if !gdb.Migrator().HasTable(table) {
			t.Errorf("table %s does not exist after migrating", table)
		}
	}
}

// TestMigrateDefaultDatabaseAnchorsAtTheGoModArgument: with SPEED_DB_PATH
// unset, the database defaults to <app name>.db, resolved -- by this
// command -- next to the go.mod argument, not the caller's working
// directory: `saasctl db migrate /path/to/project/go.mod` migrates the
// project's own database whichever directory it is invoked from. The
// project's go.mod lives in one directory, the command runs from another,
// and the database file must land next to the go.mod -- where the app's
// default database lands when the app is run from its own directory --
// never in the caller's.
func TestMigrateDefaultDatabaseAnchorsAtTheGoModArgument(t *testing.T) {
	projectDir := t.TempDir()
	mod := filepath.Join(projectDir, "go.mod")
	full, err := os.ReadFile(filepath.Join("testdata", "full.mod"))
	if err != nil {
		t.Fatalf("read the full.mod fixture: %v", err)
	}
	if err := os.WriteFile(mod, full, 0o644); err != nil {
		t.Fatalf("write the project's go.mod: %v", err)
	}

	// The operator runs the command from a directory that is not the
	// project's, pointing at the project's go.mod by absolute path.
	callerDir := t.TempDir()
	t.Chdir(callerDir)
	code, stdout, stderr := driveMigrate(t, []string{mod}, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}

	// The report names the database next to the go.mod...
	want := fmt.Sprintf("Migrated %s: applied 14 migration files (authn 9, config 1, org 3, rbac 1)\n",
		filepath.Join(projectDir, "cli-app.db"))
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	// ...the database file really exists there with the full ledger...
	if got := ledgerCounts(t, openDB(t, filepath.Join(projectDir, "cli-app.db"))); !reflect.DeepEqual(got, fullUniverseLedger) {
		t.Errorf("ledger = %v, want %v", got, fullUniverseLedger)
	}
	// ...and nothing was created in the caller's working directory.
	if _, err := os.Stat(filepath.Join(callerDir, "cli-app.db")); !os.IsNotExist(err) {
		t.Errorf("database file exists in the caller's working directory; the default path must resolve next to the go.mod argument (stat err = %v)", err)
	}
}

// TestMigrateRerunOverTheSameDatabaseReportsUpToDate: a second run
// applies nothing -- the ledger already records every file, so the report
// says so, and the database is left byte-for-byte the same shape.
func TestMigrateRerunOverTheSameDatabaseReportsUpToDate(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cli-app.db")
	env := map[string]string{appconfig.DBPathEnv: dbPath}
	mod := fixture(t, "full.mod")
	code, _, stderr := driveMigrate(t, []string{mod}, env)
	if code != 0 {
		t.Fatalf("first run exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	code, stdout, stderr := driveMigrate(t, []string{mod}, env)
	if code != 0 {
		t.Fatalf("second run exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	want := fmt.Sprintf("%s is up to date: schema_migrations already records 14 migration files (authn 9, config 1, org 3, rbac 1)\n", dbPath)
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if got := ledgerCounts(t, openDB(t, dbPath)); !reflect.DeepEqual(got, fullUniverseLedger) {
		t.Errorf("ledger = %v, want %v", got, fullUniverseLedger)
	}
}

// TestMigrateAppliesOnlyWhatAGrowingGoModNewlyRequires: a project whose
// go.mod grows from config-only to the full universe migrates the first
// module on its first run and exactly the newly required modules on the
// second -- the ledger accumulates, never re-applies.
func TestMigrateAppliesOnlyWhatAGrowingGoModNewlyRequires(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cli-app.db")
	env := map[string]string{appconfig.DBPathEnv: dbPath}

	code, stdout, stderr := driveMigrate(t, []string{fixture(t, "config_only.mod")}, env)
	if code != 0 {
		t.Fatalf("config-only run exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	want := fmt.Sprintf("Migrated %s: applied 1 migration files (config 1)\n", dbPath)
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	code, stdout, stderr = driveMigrate(t, []string{fixture(t, "full.mod")}, env)
	if code != 0 {
		t.Fatalf("full-universe run exit code = %d, want 0; stderr:\n%s", code, stderr)
	}
	want = fmt.Sprintf("Migrated %s: applied 13 migration files (authn 9, org 3, rbac 1)\n", dbPath)
	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if got := ledgerCounts(t, openDB(t, dbPath)); !reflect.DeepEqual(got, fullUniverseLedger) {
		t.Errorf("ledger = %v, want %v", got, fullUniverseLedger)
	}
}

// TestMigrateGoModWithoutSpeedRequiresIsRefused: a go.mod that requires
// no speed module at all is probably not a generated project, and the
// command says exactly that, naming the file.
func TestMigrateGoModWithoutSpeedRequiresIsRefused(t *testing.T) {
	mod := fixture(t, "non_speed.mod")
	code, stdout, stderr := driveMigrate(t, []string{mod}, nil)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	want := fmt.Sprintf("saasctl db migrate: no github.com/vislake/speed/go/* requires found in %s; nothing to migrate\n", mod)
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// TestMigrateGoModRequiringOnlyNonMigrationModulesIsRefused: a go.mod
// that requires speed modules, but none that ships its own migrations, is
// a real project whose schema its app's startup Apply composes -- the
// command refuses it and points at that path, naming what it does apply.
func TestMigrateGoModRequiringOnlyNonMigrationModulesIsRefused(t *testing.T) {
	mod := fixture(t, "tenancy_only.mod")
	code, _, stderr := driveMigrate(t, []string{mod}, nil)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "saasctl db migrate: none of the required speed modules ships its own migrations (requires: github.com/vislake/speed/go/tenancy)") {
		t.Errorf("stderr = %q, want the requires list named", stderr)
	}
	if !strings.Contains(stderr, "applies its schema through its app's own startup Apply") {
		t.Errorf("stderr = %q, want the startup-Apply pointer", stderr)
	}
}

// TestMigrateDistributedModeIsRefused: migrating is a standalone-mode
// operation -- the distributed mode's schema lives in PostgreSQL, which
// the command never touches -- so SPEED_DEPLOYMENT_MODE=distributed is
// refused, naming the variable and the resolved mode.
func TestMigrateDistributedModeIsRefused(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cli-app.db")
	code, stdout, stderr := driveMigrate(t, []string{fixture(t, "full.mod")}, map[string]string{
		appconfig.DBPathEnv:         dbPath,
		appconfig.DeploymentModeEnv: "distributed",
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	want := fmt.Sprintf("saasctl db migrate: db migrate applies the standalone deployment mode's SQLite schema; %s resolves to %q, and the distributed mode's schema lives in PostgreSQL, which this command never touches\n",
		appconfig.DeploymentModeEnv, pkgcore.DeploymentModeDistributed)
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("database file exists after the refusal; a refused run must not create one (stat err = %v)", err)
	}
}

// TestMigrateMalformedDeploymentModeIsReportedVerbatim: a value that is
// not a deployment mode at all fails with pkgcore's own parse error --
// the same error the generated app's bootstrap would surface -- prefixed
// with the command name.
func TestMigrateMalformedDeploymentModeIsReportedVerbatim(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cli-app.db")
	code, _, stderr := driveMigrate(t, []string{fixture(t, "full.mod")}, map[string]string{
		appconfig.DBPathEnv:         dbPath,
		appconfig.DeploymentModeEnv: "banana",
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	want := fmt.Sprintf("saasctl db migrate: %v: %q (valid values are %q and %q)\n",
		pkgcore.ErrInvalidDeploymentMode, "banana", pkgcore.DeploymentModeStandalone, pkgcore.DeploymentModeDistributed)
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// TestMigrateMalformedConfigKeyNamesTheAppAndVariable: a malformed
// SPEED_CONFIG_KEY fails with the generated app's own error text -- app
// name and variable named, the required shape stated -- before the
// database is ever touched.
func TestMigrateMalformedConfigKeyNamesTheAppAndVariable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cli-app.db")
	code, _, stderr := driveMigrate(t, []string{fixture(t, "full.mod")}, map[string]string{
		appconfig.DBPathEnv:    dbPath,
		appconfig.ConfigKeyEnv: "abc",
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	want := "saasctl db migrate: cli-app: SPEED_CONFIG_KEY must hold 64 hex characters (a 32-byte key), got 3\n"
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("database file exists after the refusal; a refused run must not create one (stat err = %v)", err)
	}
}

// TestMigrateExistingFileWithoutALedgerIsRefused: an existing database
// file that carries no schema_migrations table cannot be migrated safely
// -- the command cannot know which files that database has already seen --
// so it is refused with repair guidance, never guessed at.
func TestMigrateExistingFileWithoutALedgerIsRefused(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cli-app.db")
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatalf("create the empty database file: %v", err)
	}
	code, _, stderr := driveMigrate(t, []string{fixture(t, "full.mod")},
		map[string]string{appconfig.DBPathEnv: dbPath})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "refusing to migrate an existing file with no migration ledger") {
		t.Errorf("stderr = %q, want the ledger-missing refusal", stderr)
	}
	if !strings.Contains(stderr, dbPath) || !strings.Contains(stderr, "schema_migrations") {
		t.Errorf("stderr = %q, want the file and the missing table named", stderr)
	}
	if !strings.Contains(stderr, "point "+appconfig.DBPathEnv+" at a fresh path") {
		t.Errorf("stderr = %q, want the repair guidance", stderr)
	}
}

// TestMigrateNonRegularDatabasePathIsRefused: a database path that exists
// but is not a regular file (a directory, say) is refused, naming the
// path and the variable to repoint.
func TestMigrateNonRegularDatabasePathIsRefused(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "cli-app.db")
	if err := os.Mkdir(dbPath, 0o755); err != nil {
		t.Fatalf("create the directory: %v", err)
	}
	code, _, stderr := driveMigrate(t, []string{fixture(t, "full.mod")},
		map[string]string{appconfig.DBPathEnv: dbPath})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	want := fmt.Sprintf("saasctl db migrate: %s exists and is not a regular file; point %s at the SQLite database path\n", dbPath, appconfig.DBPathEnv)
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}

// TestMigrateMissingGoModNamesThePath: a go.mod argument that does not
// exist fails with the read-prefixed error naming the path, like the
// sibling commands' file errors.
func TestMigrateMissingGoModNamesThePath(t *testing.T) {
	mod := filepath.Join(t.TempDir(), "no-such-go.mod")
	code, _, stderr := driveMigrate(t, []string{mod}, nil)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.HasPrefix(stderr, "saasctl db migrate: read "+mod+":") {
		t.Errorf("stderr = %q, want the read %s: prefix", stderr, mod)
	}
}

// TestMigrateTooManyGoModArgumentsIsAUsageError: migrate takes at most
// one go.mod path; two are a usage error -- the message plus the migrate
// usage on stderr, exit 2.
func TestMigrateTooManyGoModArgumentsIsAUsageError(t *testing.T) {
	code, stdout, stderr := driveMigrate(t, []string{"one.mod", "two.mod"}, nil)
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	want := "saasctl db migrate: expected at most one go.mod path, got 2\n\n" + migrateUsage
	if stderr != want {
		t.Errorf("stderr = %q, want %q", stderr, want)
	}
}
