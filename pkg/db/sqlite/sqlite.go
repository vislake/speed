// Package sqlite is the SQLite implementation of the database capability pkg/db
// declares.
//
// It binds the SQLite driver, names the configuration namespace this engine's
// input items hang under, and delivers the capability exclusively. The
// connection, plugin, encryption and migration logic are pkg/db's, shared with
// every other implementation; what is dialect-specific stops at the driver and
// the namespace.
//
// It supplies no cross-process migration mutex, and this is where it differs
// from the PostgreSQL subpackage beyond the driver. A mutex orders replicas that
// reach the first migration at once, and several processes sharing one SQLite
// file is not a deployment shape this engine is offered in: a mutex here would
// guard against a risk the supported assemblies cannot reach. Leaving it out
// also keeps migration-lock-timeout out of this implementation's input items,
// so a host that sets it is told the key is not one this implementation accepts,
// rather than believing it changed something.
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
// A dialect has admission conditions to meet before it may carry migrations, and
// all of them are properties of the engine and its driver rather than of this
// code: it must put DDL inside a transaction, or a migration's execution and its
// record stop being atomic; and it must run a migration file holding several
// statements without pkg/db splitting the text, or the same migration set would
// stop working on a change of engine. The dialect-neutral suite holds both on
// every engine it runs on. The third condition — a cross-process mutex — is for
// dialects that appear in multi-replica deployments, which this one does not.
//
// The driver is the pure-Go one, so building a host on this engine needs no C
// toolchain and works with CGO_ENABLED=0.
package sqlite

import (
	sqlitedriver "github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/db"
)

const (
	// ModuleName is this implementation's name in the registry: its identity
	// in the startup diagnostics and in an enablement lookup.
	ModuleName = "db.sqlite"
	// ConfigNamespace is where this implementation's input items are mounted
	// in the config data. It is the module name, and it is disjoint from
	// every other implementation's by construction.
	ConfigNamespace = ModuleName
)

// Dialect is the engine this subpackage delivers. Its value is also the name
// of the subdirectory a migration set keeps this engine's files in.
const Dialect = db.SQLite

// Dialector binds the SQLite driver to a connection locator, for pkg/db to open
// the handle with.
//
// The driver appears here and nowhere in the capability's surface, which is
// what lets a host move between engines without touching a line of the code
// that uses the database.
func Dialector(dsn string) gorm.Dialector { return sqlitedriver.Open(dsn) }

// init registers this module with the process registry, so a host that imports
// the package has this engine available as soon as its configuration names it.
func init() { core.ProcessRegistry.Register(Module()) }

// Module is this module's descriptor, exported for the registries that do not
// inherit the process-level registrations.
//
// It delivers Database exclusively, which is what makes a host that configures
// this engine and another one fail the startup instead of running on whichever
// of them resolution happened to leave standing — with the dependants' tables
// split across two databases and nobody told.
//
// No migration mutex is passed, and the timeout that would bound waiting for one
// with it: this engine does not appear in multi-replica deployments.
func Module() core.Module {
	return db.NewModule(db.Spec{
		ModuleName:      ModuleName,
		ConfigNamespace: ConfigNamespace,
		Dialect:         Dialect,
		Dialector:       Dialector,
	})
}
