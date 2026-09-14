package postgres

import (
	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/db"
)

// init registers this module with the process registry, so a host that imports
// the package has PostgreSQL assembled from its configuration.
func init() { core.ProcessRegistry.Register(Module()) }

// Module is this module's descriptor, exported for the registries that do not
// inherit the process-level registrations.
//
// It is db.NewModule applied to spec below, and that is the whole of this
// subpackage's part in the lifecycle. The four phase callbacks, the capability,
// its exclusivity and the input item declaration are the root package's, so
// that this engine and its neighbour cannot drift apart in the behaviour a host
// sees; what is decided here is what is dialect-specific, which is also what
// spec carries.
func Module() core.Module { return db.NewModule(spec()) }

// spec is this engine as the root package takes it up.
//
// None of the fields is optional. The mutex is mandatory here because this
// engine does appear in multi-replica deployments, and its default timeout is
// mandatory beside it: db.NewModule refuses a spec that supplies one without
// the other, since a run given no timeout would give up on the mutex before it
// had waited.
func spec() db.Spec {
	return db.Spec{
		ModuleName:                  ModuleName,
		ConfigNamespace:             ConfigNamespace,
		Dialect:                     Dialect,
		Dialector:                   Dialector,
		NewMigrationLock:            NewMigrationLock,
		DefaultMigrationLockTimeout: DefaultMigrationLockTimeout,
	}
}
