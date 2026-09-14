package db

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/config"
	"github.com/vislake/speed/pkg/core"
)

// Spec is what an implementation subpackage hands this package to become a
// module. It is the whole of what an implementation contributes: a name, an
// engine, the namespace its input items hang under, the driver binding, and a
// cross-process migration mutex when its engine appears in multi-replica
// deployments.
//
// Everything else — the input item declaration, the stance on whether the
// module runs, the connection, the plugins, the encryption serializer, the
// migration run and the release of the pool — is this package's, shared by
// every implementation. That split is what keeps the implementations from
// drifting apart in the behaviour a host sees: an engine that put its own
// Prepare together would be one edit away from disabling itself on a different
// condition than its neighbour.
type Spec struct {
	// ModuleName is this implementation's name in the registry: its identity
	// in the startup diagnostics and in an enablement lookup.
	ModuleName string
	// ConfigNamespace is where this implementation's input items are mounted
	// in the config data. Implementations give it the module name, so a
	// diagnostic naming the module and an error naming a key read as the same
	// thing; each pins that for itself.
	ConfigNamespace string
	// Dialect is the engine behind the driver. It reaches dependants through
	// Database.Dialect, and it names the subdirectory of a migration set this
	// implementation reads.
	Dialect Dialect
	// Dialector binds this engine's driver to a connection locator. It is the
	// one place a third-party driver enters, which is what lets a host move
	// between engines without touching the code that uses the database.
	Dialector func(dsn string) gorm.Dialector
	// NewMigrationLock builds the cross-process mutex the migration run is
	// held under, bounded by the configured timeout. An implementation whose
	// engine does not appear in multi-replica deployments leaves it nil, and
	// the run proceeds without a mutex.
	//
	// Leaving it nil also keeps migration-lock-timeout out of this
	// implementation's input items: a dial that changes nothing is worse than
	// no dial, because setting it looks like it did something.
	NewMigrationLock func(timeout time.Duration) MigrationLock
	// DefaultMigrationLockTimeout is what migration-lock-timeout takes when
	// the configuration gives none. It is mandatory alongside
	// NewMigrationLock: the value travels into the mutex, and a zero one
	// would give up before it had waited at all.
	DefaultMigrationLockTimeout time.Duration
}

// NewModule builds an implementation's module descriptor.
//
// A defective Spec panics here rather than being reported: a subpackage fills
// it in at init, so the fault is in this repository's own code, shows up
// deterministically on the first run of any host that imports it, and leaves a
// host nothing to handle. It is the treatment core gives an illegal token, for
// the same reason.
func NewModule(spec Spec) core.Module {
	spec.check()
	return core.Module{
		Name: spec.ModuleName,
		// Exclusive, because two engines behind one capability is not a
		// configuration this module could serve: the dependants would each
		// take up whichever one resolution happened to leave standing, and
		// their tables would be split across two databases. Two configured
		// implementations therefore fail the startup instead.
		Provides:  []core.Provision{{Token: (*Database)(nil), Exclusive: true}},
		Resources: []any{spec.schema()},
		Prepare: func(_ context.Context, reg *core.Registry) (core.Enablement, error) {
			reader, err := spec.reader(reg, "whether this database runs")
			if err != nil {
				return core.Enablement{}, err
			}
			return spec.prepare(reader)
		},
		New: func(ctx context.Context, reg *core.Registry) (any, error) {
			reader, err := spec.reader(reg, "the connection locator")
			if err != nil {
				return nil, err
			}
			// The declarations are read before the connection is opened:
			// reading them is not a database action, so a defective one
			// fails the startup with nothing yet opened to release.
			plugins, err := collectPlugins(reg)
			if err != nil {
				return nil, err
			}
			return spec.newDatabase(ctx, reader, plugins)
		},
		Migrate: func(ctx context.Context, reg *core.Registry, instance any) error {
			reader, err := spec.reader(reg, "the bound on waiting for the migration mutex")
			if err != nil {
				return err
			}
			return spec.migrate(ctx, reg, reader, instance)
		},
		Close: func(_ context.Context, _ *core.Registry, instance any) error {
			return closeInstance(instance)
		},
	}
}

// check refuses a Spec that cannot produce a working module. Each condition is
// one this package would otherwise meet much later and much further from its
// cause: a missing driver binding as a nil call inside New, a mutex with no
// default timeout as a startup that gives up on the mutex before it has waited.
func (s Spec) check() {
	switch {
	case s.ModuleName == "":
		panic("db: a Spec carries the module name this implementation registers under")
	case s.ConfigNamespace == "":
		panic(fmt.Sprintf("db: module %q gives no configuration namespace, and its input items "+
			"have to hang under one of its own: implementations sharing a namespace conflict "+
			"as soon as a host imports both", s.ModuleName))
	case s.Dialect == "":
		panic(fmt.Sprintf("db: module %q gives no dialect, which is what names the subdirectory "+
			"its migrations are read from", s.ModuleName))
	case s.Dialector == nil:
		panic(fmt.Sprintf("db: module %q gives no dialector, and there is no other way to reach "+
			"its driver", s.ModuleName))
	case s.NewMigrationLock != nil && s.DefaultMigrationLockTimeout <= 0:
		panic(fmt.Sprintf("db: module %q supplies a migration mutex and no default for %s.%s, "+
			"so a run that configured none would give up on the mutex before waiting",
			s.ModuleName, s.ConfigNamespace, MigrationLockTimeoutKey))
	}
}

// reader takes up the configuration. what names the thing being read, so a
// host that assembled no config module is told which of this module's
// decisions it broke rather than that something was unavailable.
func (s Spec) reader(reg *core.Registry, what string) (config.Reader, error) {
	reader, err := core.Resolve[config.Reader](reg)
	if err != nil {
		return nil, fmt.Errorf("%s: %s is read from configuration, and no module delivers it: %w",
			s.ModuleName, what, err)
	}
	return reader, nil
}

// prepare states whether this implementation runs, judging by its own section.
//
// A configured locator states StateEnabled rather than StateAuto, and the
// difference is the whole point of the stance here. Resolution stands down an
// exclusive provider that states StateAuto, so with StateAuto a host that
// configured two engines would get one of them silently, with half its tables
// in a database nobody mentioned. Stating enabled makes that assembly fail.
//
// An absent locator disables the module with a reason instead of failing the
// startup: whether an assembly needs a database is the assembly's decision,
// and a host that does need one finds out through the dependency resolution of
// whatever module required the capability, with this reason attached.
func (s Spec) prepare(reader config.Reader) (core.Enablement, error) {
	cfg, err := s.read(reader)
	if err != nil {
		return core.Enablement{}, err
	}
	if strings.TrimSpace(cfg.DSN) == "" {
		return core.Enablement{
			State: core.StateDisabled,
			Reason: fmt.Sprintf("%s.%s gives no connection locator, so this assembly runs "+
				"without a %s database; set it to run on %s",
				s.ConfigNamespace, dsnKey, s.Dialect, s.Dialect),
		}, nil
	}
	return core.Enablement{State: core.StateEnabled}, nil
}

// migrate applies the declared migration sets to the delivered handle, under
// this implementation's mutex when it has one.
//
// The timeout is read here rather than carried over from New because the
// capability hands out a handle and a dialect, not the section it was built
// from, and re-reading a snapshot that cannot change costs nothing.
func (s Spec) migrate(ctx context.Context, reg *core.Registry, reader config.Reader, instance any) error {
	handle, ok := deliveredHandle(instance)
	if !ok {
		return fmt.Errorf("%w: module %q has no handle to apply migrations on, its product is "+
			"%T; every constructed module reaches this stage, and this one delivers the "+
			"database capability", ErrMigrationFailed, s.ModuleName, instance)
	}
	cfg, err := s.read(reader)
	if err != nil {
		return err
	}
	return applyMigrations(ctx, reg, handle, s.Dialect, s.lock(cfg))
}

// lock builds the mutex the migration run is held under, or reports none.
func (s Spec) lock(cfg lockingConfig) MigrationLock {
	if s.NewMigrationLock == nil {
		return nil
	}
	return s.NewMigrationLock(cfg.MigrationLockTimeout)
}

// closeInstance releases the connection pool behind a delivered product.
//
// Every shape a rollback can hand it is tolerated. A startup that failed
// part-way closes every module whatever stage it reached, so this runs on a
// module whose New never returned a product and on one that returned a nil
// product of its own type; and an instance of an unrelated type would be a
// defect in this package rather than something a host could act on. Reporting
// an error on any of them would bury the failure that started the rollback
// under one about the cleanup — and an unguarded type assertion would panic
// there and lose it outright.
func closeInstance(instance any) error {
	handle, ok := deliveredHandle(instance)
	if !ok {
		return nil
	}
	return Close(handle)
}

// deliveredHandle takes the handle out of a product of this module, and
// reports whether there was one to take.
//
// The nil test is by reflection because a typed nil pointer in an interface is
// not the nil interface: a product that came back as a nil pointer of its own
// type satisfies the assertion and panics on the method call.
func deliveredHandle(instance any) (*gorm.DB, bool) {
	product, ok := instance.(Database)
	if !ok {
		return nil, false
	}
	if v := reflect.ValueOf(product); v.Kind() == reflect.Pointer && v.IsNil() {
		return nil, false
	}
	return product.DB(), true
}
