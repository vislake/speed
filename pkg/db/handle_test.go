package db_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/config"
	jsonformat "github.com/vislake/speed/pkg/config/format/json"
	filesource "github.com/vislake/speed/pkg/config/source/file"
	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/db"
)

// hostNamespace is the namespace the cases below configure. It is not either
// shipped implementation's, so a case about the delivery this package owns does
// not read as a statement about which engine is behind it.
const hostNamespace = "db.testhost"

// sqliteSpec is an implementation of the shape every subpackage has, bound to
// the pure-Go SQLite driver — the one engine that needs no container.
//
// The driver is the real one rather than a stand-in, and the implementation
// subpackage is not imported: its init registers a module in the process
// registry, which would take part in every case in this binary.
func sqliteSpec() db.Spec {
	return db.Spec{
		ModuleName:      hostNamespace,
		ConfigNamespace: hostNamespace,
		Dialect:         db.SQLite,
		Dialector:       sqlite.Open,
	}
}

// hostConfig is the primary configuration source one case runs on: a locator
// for a database of its own, and whatever else the case configures beside it.
func hostConfig(t *testing.T, extra ...string) string {
	t.Helper()
	body := fmt.Sprintf(`{"db":{"testhost":{"dsn":%q`, filepath.Join(t.TempDir(), "host.db"))
	for _, item := range extra {
		body += "," + item
	}
	return body + "}}}"
}

// unopenableLocator is a locator the driver refuses: the directory it names
// does not exist.
func unopenableLocator(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "no-such-directory", "db.sqlite")
}

// poolOf reaches the connection pool behind a session.
func poolOf(t *testing.T, handle *gorm.DB) *sql.DB {
	t.Helper()
	pool, err := handle.DB()
	if err != nil {
		t.Fatalf("reaching the connection pool: %v", err)
	}
	return pool
}

// probe is a module that takes up the database capability the way a dependant
// does, and holds the lifecycle open while a case looks at what it took up.
//
// It takes the capability in its own New, which is where a dependant's first
// use of it sits — a capability that arrived without something installed on its
// handle is caught there and nowhere later. It then reports reaching Init, and
// waits: Init runs after Migrate, so a host whose Init has been reached has its
// migrations applied and its pool open, and a case that closed the pool while a
// migration was in flight would be interrupting a statement rather than
// observing a delivery.
type probe struct {
	delivered chan db.Database
	migrated  chan struct{}
	gate      chan struct{}
}

func newProbe() *probe {
	return &probe{
		delivered: make(chan db.Database, 1),
		migrated:  make(chan struct{}),
		gate:      make(chan struct{}),
	}
}

func (p *probe) module() core.Module {
	return core.Module{
		Name:     "probe",
		Requires: []core.Requirement{{Token: (*db.Database)(nil)}},
		New: func(_ context.Context, reg *core.Registry) (any, error) {
			instance, err := core.Resolve[db.Database](reg)
			if err != nil {
				return nil, err
			}
			p.delivered <- instance
			return nil, nil
		},
		Init: func(context.Context, *core.Registry, any) error {
			close(p.migrated)
			<-p.gate
			return nil
		},
	}
}

// host is one running assembly.
type host struct {
	// Instance is the capability as a dependant takes it up: what the module
	// the Spec describes delivered.
	Instance db.Database

	migrated chan struct{}
	gate     chan struct{}
	cancel   context.CancelFunc
	done     chan error
	once     sync.Once
}

// finish lets the lifecycle run to its end and waits for it to return, which is
// also what releases the connection pool the module delivered. It is
// idempotent, so a case may finish the host itself and leave the cleanup that
// already covers it in place.
func (h *host) finish(t *testing.T) {
	t.Helper()
	h.once.Do(func() {
		close(h.gate)
		h.cancel()
		if err := <-h.done; err != nil {
			t.Errorf("the host shut down with an error: %v", err)
		}
	})
}

// startHost assembles a real host around one implementation and runs it until
// the database capability has been delivered.
//
// Everything about it is real: the configuration loader over a JSON primary
// source, the registry, the lifecycle driver, and the probe above. Going
// through all of it is the point — what these cases observe is the contract a
// dependant meets, and a callback called in isolation would not show whether
// the stages were wired to each other at all.
//
// A host that could not start reports the error the run returned, which is what
// a process would print and exit on. A host that started is finished by the
// cleanup this leaves behind.
//
// Anything in extra is registered beside the implementation, which is how a
// case assembles the module a declaration comes from — a plugin, or a migration
// set — without this helper having to grow a parameter per declaration kind.
func startHost(t *testing.T, spec db.Spec, body string, extra ...core.Module) (*host, error) {
	t.Helper()
	locator := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(locator, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the primary configuration source: %v", err)
	}

	reg := core.New()
	reg.Register(config.Module())
	reg.Register(jsonformat.Module())
	reg.Register(filesource.Module())
	reg.Register(core.Module{
		Name:      "host",
		Resources: []any{config.HostIdentity{Prefix: "SPEEDDBTEST", DefaultLocator: "file://" + locator}},
	})
	reg.Register(db.NewModule(spec))
	for _, module := range extra {
		reg.Register(module)
	}
	p := newProbe()
	reg.Register(p.module())

	// The loader reads os.Args unconditionally and a test binary carries
	// arguments of its own, so they are taken away for the call. The cases
	// here therefore do not run in parallel.
	previous := os.Args
	os.Args = []string{"db.test"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reg.Run(ctx) }()

	h := &host{migrated: p.migrated, gate: p.gate, cancel: cancel, done: done}
	select {
	case err := <-done:
		os.Args = previous
		cancel()
		return nil, err
	case h.Instance = <-p.delivered:
		os.Args = previous
	}
	// The capability is in hand, but the run has Migrate ahead of it, and
	// returning here would let a case cancel the context under a migration
	// that is still in flight.
	select {
	case err := <-done:
		cancel()
		return nil, err
	case <-p.migrated:
	}
	t.Cleanup(func() { h.finish(t) })
	return h, nil
}

// TestPoolParametersReachTheHandle pins that the connection a module delivers
// is sized by the configuration the host wrote.
//
// The value travels the whole way — configuration file, loader, carrier,
// delivery — so a module that read the section and opened with something else
// is caught here and not by a case that handed the carrier in directly.
func TestPoolParametersReachTheHandle(t *testing.T) {
	h, err := startHost(t, sqliteSpec(), hostConfig(t, `"max-open-conns":7`))
	if err != nil {
		t.Fatalf("starting a host on a configured database: %v", err)
	}
	if got := poolOf(t, h.Instance.DB()).Stats().MaxOpenConnections; got != 7 {
		t.Errorf("the delivered handle opens at most %d connections, want the configured 7", got)
	}
}

// TestTheDeliveredInstanceReportsItsEngine pins the one thing a dependant
// branching on native SQL needs: which engine is behind the handle it took up.
func TestTheDeliveredInstanceReportsItsEngine(t *testing.T) {
	h, err := startHost(t, sqliteSpec(), hostConfig(t))
	if err != nil {
		t.Fatalf("starting a host on a configured database: %v", err)
	}
	if got := h.Instance.Dialect(); got != db.SQLite {
		t.Errorf("the delivered instance reports %q, want %q", got, db.SQLite)
	}
}

// TestStartupOnAnUnreachableDatabaseIsErrConnectFailed pins that a locator the
// driver refuses fails the startup with the sentinel the design names for it,
// rather than somewhere later or as a missing capability.
func TestStartupOnAnUnreachableDatabaseIsErrConnectFailed(t *testing.T) {
	_, err := startHost(t, sqliteSpec(),
		fmt.Sprintf(`{"db":{"testhost":{"dsn":%q}}}`, unopenableLocator(t)))
	if !errors.Is(err, db.ErrConnectFailed) {
		t.Fatalf("starting a host on a locator the driver refuses gave %v, want ErrConnectFailed", err)
	}
}

// TestOpenFailureIsErrConnectFailed pins the same sentinel on the other side of
// the delivery. A connection a caller builds at run time fails for the same
// reasons a startup one does, and the fixing action is the same, so it is the
// same class rather than a second sentinel.
func TestOpenFailureIsErrConnectFailed(t *testing.T) {
	h, err := startHost(t, sqliteSpec(), hostConfig(t))
	if err != nil {
		t.Fatalf("starting a host on a configured database: %v", err)
	}
	if _, err := h.Instance.Open(t.Context(), unopenableLocator(t)); !errors.Is(err, db.ErrConnectFailed) {
		t.Fatalf("building a connection to a locator the driver refuses gave %v, want "+
			"ErrConnectFailed", err)
	}
}

// TestOpenSharesPoolParameters pins what a second connection keeps from the
// delivered one, and what it must not keep.
//
// The shape is the same, because the pool parameters describe this module's
// connections rather than the configured database. The pool is not: a handle
// that shared one would close the module's connections along with the caller's,
// and the caller closing its own would take the delivered handle down with it.
func TestOpenSharesPoolParameters(t *testing.T) {
	h, err := startHost(t, sqliteSpec(), hostConfig(t, `"max-open-conns":7`))
	if err != nil {
		t.Fatalf("starting a host on a configured database: %v", err)
	}
	opened, err := h.Instance.Open(t.Context(), filepath.Join(t.TempDir(), "second.db"))
	if err != nil {
		t.Fatalf("building a second connection: %v", err)
	}
	deliveredPool, openedPool := poolOf(t, h.Instance.DB()), poolOf(t, opened)
	t.Cleanup(func() {
		if err := openedPool.Close(); err != nil {
			t.Errorf("releasing the second connection's pool: %v", err)
		}
	})

	if deliveredPool == openedPool {
		t.Error("the second connection came back on the delivered handle's own pool, so closing " +
			"it would close the module's connections as well")
	}
	if got, want := openedPool.Stats().MaxOpenConnections, deliveredPool.Stats().MaxOpenConnections; got != want {
		t.Errorf("the second connection opens at most %d connections and the delivered one %d, "+
			"want the pool shape of the configuration on both", got, want)
	}
	if err := openedPool.PingContext(t.Context()); err != nil {
		t.Errorf("the second connection does not answer: %v", err)
	}
}

// TestCloseIsReentrant pins both halves of the package-level Close at once: the
// first call really releases the pool, and a second one on the same handle is
// not an error.
//
// Only the pair together says anything. A Close that reported no error without
// releasing anything passes the second half alone, and every caller gives the
// close to a defer that runs whether or not the handle arrived — so a Close
// that failed on an already closed pool would turn a failed startup, which is
// the error worth reading, into a complaint about the cleanup.
func TestCloseIsReentrant(t *testing.T) {
	h, err := startHost(t, sqliteSpec(), hostConfig(t))
	if err != nil {
		t.Fatalf("starting a host on a configured database: %v", err)
	}
	handle := h.Instance.DB()
	pool := poolOf(t, handle)

	if err := db.Close(handle); err != nil {
		t.Fatalf("closing a live handle: %v", err)
	}
	if err := pool.PingContext(t.Context()); err == nil {
		t.Error("the pool still answers after it was closed, so the first close released nothing")
	}
	if err := db.Close(handle); err != nil {
		t.Errorf("closing a handle whose pool is already closed: %v", err)
	}
}
