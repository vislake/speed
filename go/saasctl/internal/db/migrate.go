package db

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/dbkit"

	// Blank-imported for its init side effect: registers dbkit.DialectSQLite
	// so dbkit.Open below has a driver to build from -- saasctl's db migrate
	// command runs only against a project's standalone SQLite database.
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/org"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pki"
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
that ships its own migrations (authn, config, org, pki and rbac), and apply
their sqlite migration files through dbkit's MigrationRegistry -- one
transaction per module, in dependency order, each file recorded in
schema_migrations as it lands, so a re-run applies only what is not yet
recorded.

With SPEED_DB_PATH unset, the database defaults to <app name>.db, and this
command resolves that default next to the [go.mod] argument -- the
directory the generated app is documented to run from, where its default
database lands -- so a go.mod argument naming another directory's project
migrates that project's own database whichever directory the command is
invoked from. A set SPEED_DB_PATH always wins and is used exactly as the
app would use it.

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
// migrations, in alphabetical order -- the order the command's reports
// name module counts in, kept stable so the universe and its report lines
// never reorder on a whim. Alphabetical is not an apply order: dbkit's
// MigrationRegistry topologically sorts the registered modules by
// DependsOn and applies one transaction per module, with this list's
// registration order breaking ties among modules that declare no
// dependency relationship. A module that ships no migration files of its
// own (pkgcore, dbkit, tenancy, observability, ratelimit, jobs, ...) is
// deliberately not here: a consumer project that requires only those
// applies its schema through its app's own startup Apply, which composes
// whatever modules the app wires.
// authnPKIName and pkiName are migrationUniverse's two entries whose
// construct functions cannot be fully independent: authn's constructor
// demands a KeySource, and pki's own *Module is what satisfies it (see
// buildAuthnAndPKI's own doc comment for why the shared instance matters).
// Declared as constants purely so the two special-cased branches in
// buildSelectedModules below and their table entries cannot drift apart on
// a rename.
const (
	authnModuleName = "authn"
	pkiModuleName   = "pki"
)

var migrationUniverse = []migrationModule{
	{
		name:    authnModuleName,
		modPath: modulePrefix + "authn",
		// construct is unused for this entry -- see buildSelectedModules,
		// which special-cases authn and pki together so the two share
		// exactly one *pki.Module instance rather than each constructing
		// (and each trying to register under the SAME "pki" name) its own.
		construct: nil,
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
		name:    pkiModuleName,
		modPath: modulePrefix + "pki",
		// construct is unused for this entry too -- see buildAuthnAndPKI.
		// pki is not part of saasctl's --with selection set -- it follows
		// authn silently (docs/internal/22-pki.md's section on where pki
		// sits in saasctl's module selection set) -- but its own tables (pki_signing_keys and
		// friends) still need migrating for any project that requires it,
		// exactly like every other migration-shipping module here, so the
		// CLI-then-boot and boot-only paths agree (a fresh app boot after
		// `db migrate` must not find any module's schema still unapplied).
		construct: nil,
	},
	{
		name:    "rbac",
		modPath: modulePrefix + "rbac",
		construct: func(db *gorm.DB) (pkgcore.Module, error) {
			return rbac.NewModule(db), nil
		},
	},
}

// buildAuthnAndPKI constructs authn's Module and pki's Module together,
// sharing exactly one *pki.Module instance between them: authn's
// constructor demands a KeySource, pki's own *Module.Service() satisfies
// it, and pki's own migrations are still exactly the same *pki.Module's
// Migrations() -- there is no second, independent pki instance to
// register, which matters because dbkit.MigrationRegistry.Register keys
// on Name(), and two DIFFERENT *pki.Module values would both report "pki"
// and collide.
//
// This also simplified the file relative to before pki existed: the old
// authn-only construct function fabricated a throwaway authn.KeySet (a
// name, a real Ed25519 keypair, and its own error handling) purely to
// satisfy authn.NewModule's requirement. pki.NewModule(db) needs none of
// that -- it performs no I/O either, and neither module's own serializer
// needs registering here, because migration application is raw versioned
// SQL, never AutoMigrate, so it never parses either module's GORM model
// structs (no token is ever issued or stored, and no signing key is ever
// generated, by this command).
func buildAuthnAndPKI(db *gorm.DB) (authnModule pkgcore.Module, pkiModule pkgcore.Module, err error) {
	pm := pki.NewModule(db)
	blindIndexKey := make([]byte, 32)
	if _, readErr := rand.Read(blindIndexKey); readErr != nil {
		return nil, nil, readErr
	}
	am, err := authn.NewModule(db, authn.WithKeySource(pm.Service()), authn.WithBlindIndexKey(blindIndexKey))
	if err != nil {
		return nil, nil, err
	}
	return am, pm, nil
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
		return "", fmt.Errorf("none of the required speed modules ships its own migrations (requires: %s): db migrate applies the migrations of the authn, config, org, pki and rbac modules; a project that requires only other speed modules applies its schema through its app's own startup Apply, not through saasctl", strings.Join(proj.Requires, ", "))
	}

	// The deployment mode and database path resolve from the same
	// environment surface the generated app boots from -- appconfig.Load
	// is the app's configFromEnv twin, errors and all.
	cfg, err := appconfig.Load(proj.AppName, os.LookupEnv)
	if err != nil {
		return "", err
	}
	if cfg.DeploymentMode != pkgcore.DeploymentModeStandalone {
		return "", fmt.Errorf("db migrate applies the standalone deployment mode's SQLite schema; %s resolves to %q, and the distributed mode's schema lives in PostgreSQL, which this command never touches", appconfig.DeploymentModeEnv, cfg.DeploymentMode)
	}
	// The app's default database path (<app name>.db, resolved when
	// SPEED_DB_PATH is unset) is relative to the app's working directory;
	// this command anchors it to the directory of the go.mod argument it
	// was handed instead -- the directory the app is documented to run
	// from, where its default database actually lands -- so a go.mod
	// argument pointing at another directory's project migrates that
	// project's own database rather than silently creating one in the
	// caller's working directory. An explicit SPEED_DB_PATH always wins
	// and is used exactly as the app would use it.
	dbPath := cfg.SQLitePath
	if !cfg.SQLitePathFromEnv {
		dbPath = filepath.Join(filepath.Dir(modPath), dbPath)
	}

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

	// pkiRequired is whether the project's own go.mod actually requires
	// go/pki -- distinct from authn's unconditional NEED for a KeySource
	// value to construct at all. buildAuthnAndPKI always builds a
	// *pki.Module to hand authn's constructor a working Service(), since
	// that costs nothing (pki.NewModule performs no I/O), but this command
	// registers -- and therefore migrates -- pki's own tables only when the
	// project genuinely declared that dependency, the same
	// requires-driven selection every other entry in migrationUniverse
	// gets. A project wiring authn with its own, non-pki-backed KeySource
	// (a real possibility -- pki is the saasctl-generated default, not a
	// hard requirement of authn itself) must not have this command create
	// pki_signing_keys and friends in its database uninvited.
	pkiRequired := slices.ContainsFunc(selected, func(m migrationModule) bool { return m.name == pkiModuleName })

	// pkiRegistered tracks whether pki's *Module has already been
	// registered -- either alongside authn (buildAuthnAndPKI, the ordinary
	// case: migrationUniverse's declared order puts "authn" before "pki",
	// so the authn case always runs first when both are selected) or, for
	// a project that somehow requires pki without authn, on its own. Either
	// way pki must never be registered twice: dbkit.MigrationRegistry.Register
	// keys on Name(), and a second "pki" registration is a collision, not a
	// no-op.
	registry := dbkit.NewMigrationRegistry()
	pkiRegistered := false
	for _, mod := range selected {
		switch mod.name {
		case authnModuleName:
			am, pm, buildErr := buildAuthnAndPKI(gdb)
			if buildErr != nil {
				return "", fmt.Errorf("construct the %s and %s modules: %w", authnModuleName, pkiModuleName, buildErr)
			}
			if regErr := registry.Register(am); regErr != nil {
				return "", fmt.Errorf("register the %s module: %w", authnModuleName, regErr)
			}
			if pkiRequired && !pkiRegistered {
				if regErr := registry.Register(pm); regErr != nil {
					return "", fmt.Errorf("register the %s module: %w", pkiModuleName, regErr)
				}
				pkiRegistered = true
			}
		case pkiModuleName:
			if pkiRegistered {
				continue
			}
			if regErr := registry.Register(pki.NewModule(gdb)); regErr != nil {
				return "", fmt.Errorf("register the %s module: %w", pkiModuleName, regErr)
			}
			pkiRegistered = true
		default:
			m, modErr := mod.construct(gdb)
			if modErr != nil {
				return "", fmt.Errorf("construct the %s module: %w", mod.name, modErr)
			}
			if regErr := registry.Register(m); regErr != nil {
				return "", fmt.Errorf("register the %s module: %w", mod.name, regErr)
			}
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
