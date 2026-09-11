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
// component descriptor (component.go) declares this one exported value, and
// its component_test.go pins the descriptor's declaration to it, so the
// bits a host reads off this package and the assembly validates cannot
// drift apart.
const Capabilities pkgcore.Capability = pkgcore.MultiReplicaSafe | pkgcore.SurvivesRestart

// closableKVStore is the value the "kv.postgres" component's New returns:
// the store itself (whose promoted methods satisfy pkgcore.KVStore) plus the
// Close() error method that releases the pool that New built, and the
// component's own Close callback releases it through this method. A host
// that calls NewKVStore itself gets the bare *Store and keeps owning its
// pool, exactly as that constructor's own doc comment promises; only the
// component-built value carries this closer.
type closableKVStore struct {
	pkgcore.KVStore
	closePool func()
}

// Close releases the pool the component's New built.
func (s *closableKVStore) Close() error {
	if s.closePool != nil {
		s.closePool()
	}
	return nil
}

// newPool validates and builds the pool for the "kv.postgres" settings the
// component (component.go) resolves: its typed configuration carries the
// dsn and its New passes its own context down to pgxpool.New. The required
// key and the pool construction live here; nothing is dialed here, per
// pgxpool.New's own laziness.
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
