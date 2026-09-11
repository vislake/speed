package postgres

// construction.go carries this package's construction helpers: the exported
// capability declaration and the shared constructors the "kv.postgres" component
// (component.go) builds from -- kept beside the implementation they adapt,
// the file-locality this package always had.
import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vislake/speed/go/pkgcore"
)

// Capabilities is what "kv.postgres" declares about itself: many replicas
// may share one PostgreSQL database, and the entries the store writes to
// its table outlive any one process -- exactly what the two bits mean. The
// built-in registration below and the Registration factory a host wraps a
// self-built pool in both declare this one exported value, so the
// declaration a host reads off this package and the one assembly validates
// cannot drift apart. A host injecting a hand-built store with
// the kv value's configuration block passes it as the injection's capability argument.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

// closableKVStore is the value "kv.postgres"'s registration returns: the
// store itself (whose promoted methods satisfy pkgcore.KVStore) plus the
// Close() error method that releases the pool the registration built, per
// the Registration-level resource-ownership contract. A host that calls
// NewKVStore itself gets the bare *Store and keeps owning its pool, exactly
// as that constructor's own doc comment promises; only the preset-built
// value carries the registration's closer.
type closableKVStore struct {
	pkgcore.KVStore
	closePool func()
}

// Close releases the pool the registration built.
func (s *closableKVStore) Close() error {
	if s.closePool != nil {
		s.closePool()
	}
	return nil
}

// newPool validates and builds the pool for the "kv.postgres" settings both
// configuration channels resolve to: the flat pkgcore.Config adapter above
// (which carries no context of its own -- Registration.New has none --
// hence its background context) and the "kv.postgres" component
// (component.go), whose typed configuration carries the same field and
// whose New passes its own context down. The required key and the pool
// construction live here, so the two channels cannot drift on them; nothing
// is dialed here, per pgxpool.New's own laziness.
func newPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, fmt.Errorf(`missing required config key "dsn"`)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("build connection pool: %w", err)
	}
	return pool, nil
}
