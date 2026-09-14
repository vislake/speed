// Package postgres is the PostgreSQL implementation of the database capability
// pkg/db declares.
//
// It binds the PostgreSQL driver, names the configuration namespace this
// engine's input items hang under, and supplies the cross-process migration
// mutex its dialect owes the migration run. The connection, plugin, encryption
// and migration logic are pkg/db's, shared with every other implementation;
// what is dialect-specific stops at the driver, the mutex and the namespace.
//
// The namespace is this engine's alone, and that is load-bearing rather than
// tidy. The configuration manifest is collected from every registered module,
// this run's disabled ones included, so two implementations declaring input
// items on one path conflict as soon as a host imports both — and that conflict
// is raised while the manifest is being collected, before exclusivity is
// resolved, so the resolution that would have stood one of them down never
// runs. Importing several implementations and configuring one is the intended
// shape, and separate namespaces are what make it work.
//
// A dialect has three admission conditions to meet before it may carry
// migrations, and all three are properties of the engine and its driver rather
// than of this code: it must put DDL inside a transaction, or a migration's
// execution and its record stop being atomic; it must run a migration file
// holding several statements without pkg/db splitting the text, or the same
// migration set would stop working on a change of engine; and, appearing as it
// does in multi-replica deployments, it must offer a cross-process mutex. The
// dialect-neutral suite holds the first two on every engine it runs on, and the
// cases around NewMigrationLock hold the third.
package postgres

import (
	"time"

	pgdriver "gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/db"
)

const (
	// ModuleName is this implementation's name in the registry: its identity
	// in the startup diagnostics and in an enablement lookup.
	ModuleName = "db.postgres"
	// ConfigNamespace is where this implementation's input items are mounted
	// in the config data. It is the module name, and it is disjoint from
	// every other implementation's by construction.
	ConfigNamespace = ModuleName
)

// Dialect is the engine this subpackage delivers. Its value is also the name
// of the subdirectory a migration set keeps this engine's files in.
const Dialect = db.Postgres

const (
	// MigrationLockTimeoutKey is the input item bounding the wait for the
	// migration mutex, relative to ConfigNamespace. Only an implementation
	// that offers a mutex declares it; one that needs none has nothing to
	// wait for and would be offering a dial that does nothing.
	MigrationLockTimeoutKey = "migration-lock-timeout"
	// DefaultMigrationLockTimeout is how long a replica waits for another
	// replica to finish applying migrations before it gives up.
	DefaultMigrationLockTimeout = 5 * time.Minute
)

// Dialector binds the PostgreSQL driver to a connection locator, for pkg/db to
// open the handle with.
//
// The driver appears here and nowhere in the capability's surface, which is
// what lets a host move between engines without touching a line of the code
// that uses the database.
func Dialector(dsn string) gorm.Dialector { return pgdriver.Open(dsn) }
