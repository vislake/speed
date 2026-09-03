package db

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/rbac"
	"github.com/vislake/speed/go/saasctl/internal/appconfig"
	"github.com/vislake/speed/go/saasctl/internal/project"
)

// defaultModPath is the go.mod the migrate command reads when no path is
// given: the one in the working directory, mirroring the sibling
// commands' default.
const defaultModPath = "go.mod"

// migrateUsage is the migrate subcommand's help text.
const migrateUsage = `Usage: saasctl db migrate [go.mod]

Apply the SQL migrations of the speed modules a consumer project requires
to the project's SQLite database, exactly as the generated app's startup
Apply would: read the go.mod (the [go.mod] argument, defaulting to
./go.mod), resolve the project's bootstrap environment the way the
generated app resolves it (SPEED_DEPLOYMENT_MODE, SPEED_DB_PATH, ...),
construct every github.com/vislake/speed/go/* module the go.mod requires
that ships its own migrations (authn, config, org and rbac), and apply
their sqlite migration files through dbkit's MigrationRegistry -- one
transaction per module, in dependency order, each file recorded in
schema_migrations as it lands, so a re-run applies only what is not yet
recorded.

Migrating is a standalone-mode operation: the distributed mode's schema
lives in PostgreSQL, which this command never touches, so a deployment
mode other than standalone is refused.

Examples:

  saasctl db migrate
  saasctl db migrate /path/to/project/go.mod

Exit codes: 0 success or help, 2 usage error, 1 execution error.
`

// modulePrefix is the import-path prefix shared by every module this
// repository releases. It is duplicated from internal/project, which the
// migration universe's module paths are matched against; the duplication
// mirrors the one internal/project itself documents for internal/upgrade.
const modulePrefix = "github.com/vislake/speed/go/"

// A migrationModule is one entry of the migration universe: the module's
// short name -- the string its migration files are recorded under in
// dbkit's schema_migrations ledger -- and a constructor that builds the
// module over a freshly opened database.
type migrationModule struct {
	name    string
	modPath string
	// construct builds the module. Construction performs no I/O and never
	// touches the database: the module exists so the migration registry
	// can read Name, DependsOn and Migrations from it.
	construct func(db *gorm.DB) (pkgcore.Module, error)
}

// migrationUniverse lists every speed root module that ships its own SQL
// migrations, in alphabetical order -- the order the reference app's
// cmd/server/server.go registers them in, and the order the command's
// reports name module counts in. A module that ships no migration files
// of its own (pkgcore, dbkit, tenancy, observability, ratelimit, jobs,
// ...) is deliberately not here: a consumer project that requires only
// those applies its schema through its app's own startup Apply, which
// composes whatever modules the app wires.
var migrationUniverse = []migrationModule{
	{
		name:    "authn",
		modPath: modulePrefix + "authn",
		construct: func(db *gorm.DB) (pkgcore.Module, error) {
			// authn's constructor demands a signing key set and a
			// blind-index key up front. Their values are irrelevant to
			// migration application -- no token is ever issued or stored
			// here -- so the command supplies throwaway development-shaped
			// keys, the same shape the generated app's dev seed uses.
			key, err := authn.GenerateTokenKey("saasctl db migrate")
			if err != nil {
				return nil, err
			}
			keySet, err := authn.NewKeySet(key)
			if err != nil {
				return nil, err
			}
			blindIndexKey := make([]byte, 32)
			if _, err := rand.Read(blindIndexKey); err != nil {
				return nil, err
			}
			return authn.NewModule(db, authn.WithSigningKeys(keySet), authn.WithBlindIndexKey(blindIndexKey))
		},
	},
	{
		name:    "config",
		modPath: modulePrefix + "config",
		construct: func(db *gorm.DB) (pkgcore.Module, error) {
			return config.NewModule(db), nil
		},
	},
	{
		name:    "org",
		modPath: modulePrefix + "org",
		construct: func(db *gorm.DB) (pkgcore.Module, error) {
			return org.NewModule(db), nil
		},
	},
	{
		name:    "rbac",
		modPath: modulePrefix + "rbac",
		construct: func(db *gorm.DB) (pkgcore.Module, error) {
			return rbac.NewModule(db), nil
		},
	},
}

// selectMigrationModules intersects a project's speed requires with the
// migration universe, preserving the universe's alphabetical order.
func selectMigrationModules(requires []string) []migrationModule {
	selected := make([]migrationModule, 0, len(requires))
	for _, candidate := range migrationUniverse {
		if slices.Contains(requires, candidate.modPath) {
			selected = append(selected, candidate)
		}
	}
	return selected
}

// errLedgerMissing wraps the refusal to migrate an existing database file
// that carries no migration ledger.
var errLedgerMissing = errors.New("refusing to migrate an existing file with no migration ledger")

// schemaMigrationsTable is the name of dbkit's migration-bookkeeping
// table. The name is duplicated from dbkit (whose constant is
// unexported): the command reads the ledger through its own plain model.
const schemaMigrationsTable = "schema_migrations"

// ledgerRow is one row of dbkit's schema_migrations table, read back so
// the command can tell which migration files a run must still apply and
// which it just applied. It mirrors dbkit's internal schemaMigration
// model, columns and all.
type ledgerRow struct {
	Module   string `gorm:"column:module"`
	Filename string `gorm:"column:filename"`
}

// TableName pins ledgerRow to dbkit's schema_migrations table. Like every
// read in this command, the ledger is queried through a plain struct
// with a TableName method, never through Table/Model/Raw.
func (ledgerRow) TableName() string { return schemaMigrationsTable }

// readLedger returns the migration filenames schema_migrations already
// records, grouped by module. A table that does not exist surfaces as the
// driver's no-such-table error, which the caller classifies.
func readLedger(db *gorm.DB) (map[string][]string, error) {
	var rows []ledgerRow
	if err := db.Find(&rows).Error; err != nil {
		return nil, err
	}
	ledger := make(map[string][]string)
	for _, row := range rows {
		ledger[row.Module] = append(ledger[row.Module], row.Filename)
	}
	return ledger, nil
}

// A moduleCount is one module's migration-file count in a report.
type moduleCount struct {
	name  string
	count int
}

// formatCounts renders module counts as the parenthetical of a report
// line -- "authn 9, config 1, org 3, rbac 1" -- in the order collected
// (the migration universe's alphabetical order).
func formatCounts(counts []moduleCount) string {
	pairs := make([]string, len(counts))
	for i, c := range counts {
		pairs[i] = fmt.Sprintf("%s %d", c.name, c.count)
	}
	return strings.Join(pairs, ", ")
}

// runMigrate parses the migrate subcommand's arguments and runs it,
// returning the process exit code: 0 on success (one report line on
// stdout), 0 for -h (usage on stderr), 2 for a malformed invocation, 1
// for a failed execution.
func runMigrate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("migrate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		_, _ = fmt.Fprint(stderr, migrateUsage)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	modPath := defaultModPath
	if paths := flags.Args(); len(paths) > 1 {
		return usageError(stderr, fmt.Errorf("expected at most one go.mod path, got %d", len(paths)))
	} else if len(paths) == 1 {
		modPath = paths[0]
	}
	line, err := migrate(modPath)
	if err != nil {
		return reportError(stderr, err)
	}
	_, _ = fmt.Fprintln(stdout, line)
	return 0
}

// usageError reports a malformed invocation: the error plus the usage
// text on stderr, exit code 2.
func usageError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "saasctl db migrate: %v\n\n%s", err, migrateUsage)
	return 2
}

// reportError reports a failed execution: one line on stderr, exit code
// 1.
func reportError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "saasctl db migrate: %v\n", err)
	return 1
}

// migrate applies the SQL migrations of every migration-shipping speed
// module the project at modPath requires to the SQLite database the
// project's bootstrap environment names, and returns the one report line
// the command prints on success.
func migrate(modPath string) (string, error) {
	proj, err := project.Read(modPath)
	if err != nil {
		return "", err
	}
	selected := selectMigrationModules(proj.Requires)
	if len(selected) == 0 {
		// Two distinct empty states, two distinct refusals: a go.mod with
		// no speed requires at all is probably not a generated project,
		// while one that requires only non-migration-shipping modules is a
		// real project whose schema its app's own startup Apply composes.
		if len(proj.Requires) == 0 {
			return "", fmt.Errorf("no github.com/vislake/speed/go/* requires found in %s; nothing to migrate", modPath)
		}
		return "", fmt.Errorf("none of the required speed modules ships its own migrations (requires: %s): db migrate applies the migrations of the authn, config, org and rbac modules; a project that requires only other speed modules applies its schema through its app's own startup Apply, not through saasctl", strings.Join(proj.Requires, ", "))
	}

	// The database path and deployment mode resolve from the same
	// environment surface the generated app boots from -- appconfig.Load
	// is the app's configFromEnv twin, errors and all -- so the database
	// this command migrates is the database the app opens.
	cfg, err := appconfig.Load(proj.AppName, os.LookupEnv)
	if err != nil {
		return "", err
	}
	if cfg.DeploymentMode != pkgcore.DeploymentModeStandalone {
		return "", fmt.Errorf("db migrate applies the standalone deployment mode's SQLite schema; %s resolves to %q, and the distributed mode's schema lives in PostgreSQL, which this command never touches", appconfig.DeploymentModeEnv, cfg.DeploymentMode)
	}
	dbPath := cfg.SQLitePath

	// A file that does not exist yet is a fresh database, migrated from
	// nothing. A file that exists must be a regular file, and -- because a
	// re-run must apply only what is not yet recorded -- must already
	// carry dbkit's schema_migrations ledger: an existing file without
	// one is refused rather than guessed at.
	fresh := false
	info, err := os.Stat(dbPath)
	switch {
	case err == nil:
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("%s exists and is not a regular file; point %s at the SQLite database path", dbPath, appconfig.DBPathEnv)
		}
	case errors.Is(err, os.ErrNotExist):
		fresh = true
	default:
		return "", fmt.Errorf("stat %s: %w", dbPath, err)
	}

	ctx := context.Background()
	gdb, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dbPath})
	if err != nil {
		// dbkit never embeds the DSN in its errors; naming the path here
		// is what lets the operator see which file failed to open.
		return "", fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer func() {
		if sqlDB, closeErr := gdb.DB(); closeErr == nil {
			_ = sqlDB.Close()
		}
	}()

	var pre map[string][]string
	if !fresh {
		pre, err = readLedger(gdb)
		if err != nil {
			if strings.Contains(err.Error(), "no such table") {
				// SQLite's own no-such-table text: acceptable to match
				// literally, because this command is SQLite-only by
				// contract.
				return "", fmt.Errorf("%w: %s already exists but has no schema_migrations table, so db migrate cannot know which migrations that file has already seen; if it is not a database this command migrated before, remove it or point %s at a fresh path", errLedgerMissing, dbPath, appconfig.DBPathEnv)
			}
			return "", fmt.Errorf("read the migration ledger of %s: %w", dbPath, err)
		}
	}

	registry := dbkit.NewMigrationRegistry()
	for _, mod := range selected {
		m, modErr := mod.construct(gdb)
		if modErr != nil {
			return "", fmt.Errorf("construct the %s module: %w", mod.name, modErr)
		}
		if regErr := registry.Register(m); regErr != nil {
			return "", fmt.Errorf("register the %s module: %w", mod.name, regErr)
		}
	}
	if applyErr := registry.Apply(ctx, gdb, dbkit.DialectSQLite); applyErr != nil {
		return "", fmt.Errorf("apply %s's migrations: %w", dbPath, applyErr)
	}
	post, err := readLedger(gdb)
	if err != nil {
		return "", fmt.Errorf("read the migration ledger of %s: %w", dbPath, err)
	}

	// The report contrasts the ledger before and after the run: what was
	// newly applied (possibly nothing), against what the ledger already
	// recorded, per module, in the universe's alphabetical order.
	newApplied := 0
	recorded := 0
	var appliedCounts []moduleCount
	for _, mod := range selected {
		before := len(pre[mod.name])
		after := len(post[mod.name])
		recorded += after
		if delta := after - before; delta > 0 {
			newApplied += delta
			appliedCounts = append(appliedCounts, moduleCount{name: mod.name, count: delta})
		}
	}
	if newApplied > 0 {
		return fmt.Sprintf("Migrated %s: applied %d migration files (%s)", dbPath, newApplied, formatCounts(appliedCounts)), nil
	}
	recordedCounts := make([]moduleCount, 0, len(selected))
	for _, mod := range selected {
		recordedCounts = append(recordedCounts, moduleCount{name: mod.name, count: len(post[mod.name])})
	}
	return fmt.Sprintf("%s is up to date: schema_migrations already records %d migration files (%s)", dbPath, recorded, formatCounts(recordedCounts)), nil
}
