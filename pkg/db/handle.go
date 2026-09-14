package db

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/config"
)

// database is an implementation's product: the connection this package opened,
// with the capability's four queries answered from it.
//
// It keeps the Spec and the section it was assembled from, so that a second
// connection a caller builds with Open is put together the same way the
// delivered one was — same engine, same pool shape. Everything on it is fixed
// at construction and the handle underneath is safe for concurrent use, so the
// product carries no lock of its own.
type database struct {
	spec   Spec
	cfg    lockingConfig
	handle *gorm.DB
	// plugins is what the assembly declared, in the order it installs them.
	// The product keeps the list rather than reading the registry again so
	// that a connection built with Open carries the same set as the
	// delivered handle: the declarations are a property of the assembly,
	// and an install that consulted anything about the run would be free
	// to deliver two handles that disagree.
	plugins []pluginInstall
	// digest computes the blind index of one plaintext, under the subkey
	// derived from the configured root key. It is nil on a product
	// assembled without that derivation — there is no subkey to compute
	// with — and BlindIndex refuses rather than answering.
	digest func(plaintext string) string
}

// The product answers the capability the way the interface declares it, so a
// drift between them is caught where it is written rather than at the startup
// of every host that imports the implementation.
var _ Database = (*database)(nil)

// newDatabase reads this implementation's section, opens the connection it
// names, installs the declared plugins on it, and assembles the product.
//
// It takes a config.Reader rather than the registry, so the descriptor's
// callback does nothing but resolve and delegate; the plugins arrive collected,
// because the collection is the descriptor's to make and this function's to
// install.
func (s Spec) newDatabase(ctx context.Context, reader config.Reader, plugins []pluginInstall) (*database, error) {
	cfg, err := s.read(reader)
	if err != nil {
		return nil, err
	}
	handle, err := openHandle(ctx, s, cfg)
	if err != nil {
		return nil, err
	}
	// The blind index is answered from the product rather than from the
	// connection, so it needs the subkey on its own. The same derivation
	// assembled the connection, over the same section, so the two agree by
	// construction instead of by a value travelling between them — and a
	// root key this package cannot use has already failed the connection,
	// so nothing is refused here that was not refused before it.
	digest, err := digestFor(s.ConfigNamespace, cfg.Config)
	if err != nil {
		return nil, errors.Join(err, Close(handle))
	}
	// The plugins go on here, before the product exists: a handle without
	// them never reaches a caller, so nobody can hold the capability and
	// issue a statement the declarations were meant to reach.
	if err := installPlugins(handle, plugins); err != nil {
		// The handle does not leave this function on that path, so its
		// pool is released here — no caller could release it instead.
		return nil, errors.Join(err, Close(handle))
	}
	return &database{
		spec:    s,
		cfg:     cfg,
		handle:  handle,
		plugins: plugins,
		digest:  digest,
	}, nil
}

// DB returns the handle the module delivers.
func (d *database) DB() *gorm.DB { return d.handle }

// Dialect reports the engine behind the handle.
func (d *database) Dialect() Dialect { return d.spec.Dialect }

// BlindIndex returns the digest of a plaintext under this product's blind
// index subkey.
//
// Without that subkey there is no digest to return, and the two answers that
// suggest themselves are both silent: an empty digest, or one computed under a
// key nobody chose. Either way the caller writes the column, the write reports
// no error, and the only thing that ever disagrees is an equality query that
// finds no row — a long way from the write that produced it. So this refuses
// instead, and the gap surfaces at the call rather than in the data.
func (d *database) BlindIndex(plaintext string) string {
	if d.digest == nil {
		panic("db: BlindIndex was called on a database assembled without the key " +
			"derivation it works under")
	}
	return d.digest(plaintext)
}

// Open establishes another connection, to the database the locator names.
//
// The connection is assembled the way the delivered one is — same engine, same
// pool parameters, and the same declared plugins installed before it is
// returned. A caller that moved a query to another database would otherwise
// lose whatever the plugins do to it, and losing tenant filtering or auditing
// is not something the query's result shows. Each handle is installed into
// separately, so GORM calls every plugin's Initialize once more on this one;
// the instances are the declared ones, and the declaration is what this module
// installs.
//
// It is the caller's: this package keeps no reference to it, does not apply
// migrations to it, and does not close it. The caller closes it with the
// package-level Close in its own Close stage, and leaking it leaks connections
// that nothing here can release on its behalf.
func (d *database) Open(ctx context.Context, dsn string) (*gorm.DB, error) {
	cfg := d.cfg
	cfg.DSN = dsn
	handle, err := openHandle(ctx, d.spec, cfg)
	if err != nil {
		return nil, err
	}
	if err := installPlugins(handle, d.plugins); err != nil {
		return nil, errors.Join(err, Close(handle))
	}
	return handle, nil
}
