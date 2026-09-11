package app

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// DatabaseSpec declares the database stage 2 opens. Every value is host
// policy: which dialect, which DSN, and whether dbkit's automatic
// write-capture plugin publishes on an audit bus.
type DatabaseSpec struct {
	// Dialect selects the SQL dialect dbkit opens the database with, and the
	// dialect whose migration set every module's Apply reads.
	Dialect dbkit.Dialect

	// DSN is the driver-specific data source name. It often carries
	// credentials, so it is never logged and never appears in a returned
	// error.
	DSN string

	// AuditBus, when non-nil, installs dbkit's automatic write-capture
	// plugin: every write against an Auditable model publishes a
	// WriteCapturedEvent on this bus. The host owns the bus's lifecycle; a
	// composition that captures writes must hand the kernel the very same
	// bus (WithKernelOptions' pkgcore.WithEventBus), or the captured events
	// publish where the audit module's subscriptions never see them.
	AuditBus pkgcore.EventBus

	// AuditModels restricts the capture scope to the listed model types; nil
	// captures every Auditable model written through this connection.
	AuditModels []any
}

// WithDatabase declares the database the engine opens and migrates in stage
// 2, and is required.
func WithDatabase(spec DatabaseSpec) Option {
	return func(c *engineConfig) {
		s := spec
		c.databaseSpec = &s
	}
}

// WithPreDB registers a callback the infrastructure stage runs after the
// platform cipher is built and before the database is opened. It exists for
// the registrations that must precede dbkit.Open: GORM resolves a model's
// named serializer while it parses the schema, so a module's encrypted-column
// serializers and blind indexers have to be in place before the connection
// that will parse those models exists. The callback receives the platform
// cipher built from the config.cipher_key material, and the assembly's
// resolved bootstrap material, so the ciphers and indexers of keys a
// component declares -- authn's PII cipher, pki's local-key cipher -- are
// built from the very resolutions every other consumer reads.
func WithPreDB(fn func(ctx context.Context, deps PreDBDeps) error) Option {
	return func(c *engineConfig) { c.preDB = fn }
}

// PreDBDeps is what a WithPreDB callback receives: the platform cipher and
// the assembly's resolved bootstrap material, both ready before the database
// opens.
type PreDBDeps struct {
	// Cipher is the platform cipher built from the config.cipher_key
	// material.
	Cipher *dbkit.Cipher

	// Material is the assembly's resolved bootstrap material: every
	// registered component's declared keys, addressed by declared key path
	// (Material returns the []byte of a hexkey declaration).
	Material *pkgcore.BootstrapMaterial
}

// ModuleDeps is what a WithModules callback receives: the opened database,
// the platform cipher and the resolved bootstrap material, all ready for
// module construction.
type ModuleDeps struct {
	// DB is the database dbkit.Open returned, already carrying the tenant
	// scoping and soft-delete plugins and any write-capture wiring the
	// DatabaseSpec declared.
	DB *gorm.DB

	// Cipher is the platform cipher built from the config.cipher_key
	// material, the same instance WithPreDB received.
	Cipher *dbkit.Cipher

	// Material is the assembly's resolved bootstrap material, the same
	// source WithPreDB received: the declared keys' values by key path.
	Material *pkgcore.BootstrapMaterial
}

// WithModules registers the callback stage 3 runs to construct the module
// set. The callback's returned order is the order the kernel registers the
// modules in, so a host whose composition requires one module's Register to
// precede another's states that order here (the kernel still sorts by
// DependsOn on top of it); a nil callback composes no modules.
//
// The callback runs after the database is open and before anything is
// bootstrapped, so it may hold onto the modules it builds -- a host keeps
// them in its own closure state for the later hooks (a verifier handed to the
// middleware chain, a queue handed to the worker).
func WithModules(fn func(ctx context.Context, deps ModuleDeps) ([]pkgcore.Module, error)) Option {
	return func(c *engineConfig) { c.modules = fn }
}

// applyMigrations registers every module's migration set and applies it to
// db for the declared dialect. A module shipping no migrations of its own
// (an empty FS) registers and applies as a no-op, so the whole module set can
// be handed over without the host classifying which modules migrate.
func applyMigrations(ctx context.Context, db *gorm.DB, dialect dbkit.Dialect, modules []pkgcore.Module) error {
	registry := dbkit.NewMigrationRegistry()
	for _, m := range modules {
		if m == nil {
			return fmt.Errorf("app: register module migrations: %w", dbkit.ErrNilModule)
		}
		if err := registry.Register(m); err != nil {
			return fmt.Errorf("app: register the migrations of module %q: %w", m.Name(), err)
		}
	}
	if err := registry.Apply(ctx, db, dialect); err != nil {
		return fmt.Errorf("app: apply migrations: %w", err)
	}
	return nil
}
