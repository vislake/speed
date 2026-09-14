package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// openHandle opens one connection with an implementation's driver, sizes its
// pool from the configuration, and checks that the database answers.
//
// Every connection this package hands out is opened here — the one the module
// delivers, and each one a caller builds with Open — so one place decides how
// a pool is sized, what a failed connection reports, and what a handle carries
// before anyone can issue a statement on it. The dialector is the only part
// that differs between engines.
//
// The connectivity check is bound to ctx, which is the ctx the stage received:
// a host that wants a bound on how long a startup may wait for the database
// sets it on the context it hands Run, because the driver's own timeouts are
// written in the locator rather than declared here as input items.
//
// A failure reports ErrConnectFailed whatever the step — opening the driver,
// reaching the pool behind the session, or the connectivity check — because
// the host's response is the same for all three: look at the locator and at
// the service behind it. The text names the dialect and the step that failed,
// and never the locator: it carries credentials, and this error reaches the
// startup diagnostics.
func openHandle(ctx context.Context, spec Spec, cfg lockingConfig) (*gorm.DB, error) {
	session, err := gorm.Open(spec.Dialector(cfg.DSN), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("%w: opening a %s connection failed: %w",
			ErrConnectFailed, spec.Dialect, err)
	}
	pool, err := session.DB()
	if err != nil {
		return nil, fmt.Errorf("%w: the %s session came back without a connection pool to size: %w",
			ErrConnectFailed, spec.Dialect, err)
	}
	// The three parameters travel together: database/sql reads 0 as no limit
	// on the open connections and as a connection that is never replaced, so
	// a handle that lost one of them does not fail, it opens as many
	// connections as the load asks for and keeps each one for good.
	pool.SetMaxOpenConns(cfg.MaxOpenConns)
	pool.SetMaxIdleConns(cfg.MaxIdleConns)
	pool.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	if err := pool.PingContext(ctx); err != nil {
		return nil, errors.Join(
			fmt.Errorf("%w: the %s database did not answer the connectivity check: %w",
				ErrConnectFailed, spec.Dialect, err),
			closeUnusablePool(pool))
	}
	// The encryption support is installed here rather than by the callers,
	// because it is the section that decides it: the key material comes out
	// of the same configuration the pool parameters do, and both callers of
	// this function have only a section to hand. What the registry decides
	// instead — the declared plugins — is installed by the callers, which
	// are the sites that hold it.
	if err := installEncryption(session, spec.ConfigNamespace, cfg.Config); err != nil {
		return nil, errors.Join(err, Close(session))
	}
	return session, nil
}

// closeUnusablePool releases a pool whose connection never answered, so the
// failed attempt leaves nothing behind. The handle never leaves openHandle on
// that path, so no caller could release it instead.
func closeUnusablePool(pool *sql.DB) error {
	if err := pool.Close(); err != nil {
		return fmt.Errorf("db: closing the pool of a connection that never answered failed: %w", err)
	}
	return nil
}
