package db_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/vislake/speed/pkg/core"
	"github.com/vislake/speed/pkg/db"
)

// TestEveryFailurePathKeepsTheChain is the executable form of the rule every
// wrap in this module follows: an error goes on with %w, so the sentinel stays
// reachable with errors.Is and the error from the layer below stays a node of
// the tree.
//
// The text is not the observation. A %v leaves the message all but identical and
// takes both out of the chain, which is why every path below is built for real
// and what is read is the tree under the error that came back: the class that
// matches, the classes that must not, and the error from below.
//
// The four classes are the whole of the sentinel table, and each is reached the
// way a host reaches it — through the lifecycle, from a module that was really
// assembled and really failed.
func TestEveryFailurePathKeepsTheChain(t *testing.T) {
	t.Run("connect", func(t *testing.T) {
		locator := unopenableLocator(t)
		_, err := startHost(t, sqliteSpec(), fmt.Sprintf(`{"db":{"testhost":{"dsn":%q}}}`, locator))
		assertReportsOnly(t, err, "ErrConnectFailed")
		assertHoldsNodeReading(t, err, driverRefusal(t, locator))
	})

	t.Run("plugin", func(t *testing.T) {
		refusal := &pluginRefusal{reason: "the audit sink is not reachable"}
		_, err := startHost(t, sqliteSpec(), hostConfig(t),
			pluginModule("auditing", refusingPlugin{err: refusal}))
		assertReportsOnly(t, err, "ErrPluginFailed")

		// The plugin's own error is reached by type rather than by text, so
		// this half holds whichever words the plugin chose.
		var recovered *pluginRefusal
		if !errors.As(err, &recovered) {
			t.Fatalf("the tree under %v does not hold the error the plugin reported, so what the "+
				"plugin said did not travel: a %%v keeps its text and drops the value", err)
		}
		if recovered != refusal {
			t.Errorf("the tree holds a %T other than the one the plugin reported", recovered)
		}
	})

	t.Run("migration", func(t *testing.T) {
		const broken = "CREATE TABLE widgets (id INTEGER"
		_, err := startHost(t, sqliteSpec(), hostConfig(t), core.Module{
			Name:      "broken",
			Resources: []any{migrationSet(db.SQLite, map[string]string{"0001_widgets.sql": broken})},
		})
		assertReportsOnly(t, err, "ErrMigrationFailed")
		assertHoldsNodeReading(t, err, statementRefusal(t, broken))
	})

	t.Run("migration mutex", func(t *testing.T) {
		// The mutex is the one seam a run takes from its implementation, and
		// the genuine timeout needs a second replica holding it — that path is
		// pinned against a real server in pkg/db/postgres. Here the mutex is
		// this case's own: it waits out the configured allowance and reports
		// the timeout, with an error of its own underneath.
		holder := errors.New("the other replica had not finished applying its migrations")
		spec := sqliteSpec()
		spec.NewMigrationLock = func(allowance time.Duration) db.MigrationLock {
			return &refusingLock{allowance: allowance, holder: holder}
		}
		spec.DefaultMigrationLockTimeout = 50 * time.Millisecond

		_, err := startHost(t, spec, hostConfig(t), core.Module{
			Name: "declaring",
			Resources: []any{migrationSet(db.SQLite, map[string]string{
				"0001_widgets.sql": "CREATE TABLE widgets (id INTEGER)",
			})},
		})
		// A timeout is its own class: it means the other replica has to be
		// looked at, and reporting it as a migration that would not apply sends
		// the reader through migration files that are sound.
		assertReportsOnly(t, err, "ErrMigrationLockTimeout")
		if !errors.Is(err, holder) {
			t.Errorf("the tree under %v does not hold the error the mutex reported, so what the "+
				"mutex said did not travel", err)
		}
	})
}

// assertReportsOnly pins that err reports exactly one class of failure, and
// which one — named as the design's table names it, so what the case checks is
// the table rather than a list written out a second time here. A host decides
// what to do from the class, so an error matching two of them leaves it with two
// responses and no way to choose between them.
func assertReportsOnly(t *testing.T, err error, name string) {
	t.Helper()
	want, known := sentinels[name]
	if !known {
		t.Fatalf("%q is not one of the classes the sentinel table holds", name)
	}
	if err == nil {
		t.Fatal("the startup reported no error, and this case needs it to have failed")
	}
	if !errors.Is(err, want) {
		t.Fatalf("the startup reported %v, which does not carry %s (%v)", err, name, want)
	}
	for otherName, other := range sentinels {
		if otherName == name {
			continue
		}
		if errors.Is(err, other) {
			t.Errorf("the error reports %s and carries %s as well, and the two call for different "+
				"responses: %v", name, otherName, err)
		}
	}
}

// assertHoldsNodeReading pins that some error in the tree under err reads
// exactly as cause does.
//
// It compares text because a driver reports a fresh error for every call: the
// same mistake made twice produces two values that no errors.Is can pair. What a
// node reading as the driver's error establishes is that the error itself
// travelled — a %v in place of a %w keeps the text and puts no such node there.
func assertHoldsNodeReading(t *testing.T, err, cause error) {
	t.Helper()
	want := cause.Error()
	var walked []string
	for _, node := range errorTree(err) {
		if node.Error() == want {
			return
		}
		walked = append(walked, node.Error())
	}
	t.Errorf("no error in the tree reads as the error from the layer below it does (%q), so that "+
		"error did not travel: a %%v in place of a %%w keeps the text and drops the node. The tree "+
		"under %v reads: %q", want, err, walked)
}

// errorTree lists every error reachable from err, through a single Unwrap and
// through the slice an errors.Join reports.
//
// A node is walked once, so an error a tree holds twice is listed twice rather
// than walked away with. The dedup is by value and not by text: a join of one
// error reads exactly as that error does, and a text key would take the child
// for the parent and stop there. The guard is on the type's comparability,
// because an error carrying a slice or a map cannot be a map key.
func errorTree(err error) []error {
	var tree []error
	seen := map[error]bool{}
	var walk func(error)
	walk = func(node error) {
		if node == nil {
			return
		}
		if reflect.ValueOf(node).Type().Comparable() {
			if seen[node] {
				return
			}
			seen[node] = true
		}
		tree = append(tree, node)
		for _, child := range childrenOf(node) {
			walk(child)
		}
	}
	walk(err)
	return tree
}

// childrenOf reports what one error unwraps to, in both shapes an error in this
// module's trees takes: the single error a wrap reports and the slice a join
// reports.
func childrenOf(node error) []error {
	if joined, ok := any(node).(interface{ Unwrap() []error }); ok {
		return joined.Unwrap()
	}
	if wrapped, ok := any(node).(interface{ Unwrap() error }); ok {
		if child := wrapped.Unwrap(); child != nil {
			return []error{child}
		}
	}
	return nil
}

// driverRefusal produces the driver's own error for a locator, by asking for a
// connection the way an attempt reaches it: open the session, and if that
// answered, ask the database whether it is there.
func driverRefusal(t *testing.T, locator string) error {
	t.Helper()
	session, err := gorm.Open(sqlite.Open(locator), &gorm.Config{})
	if err != nil {
		return err
	}
	pool, err := session.DB()
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()
	refusal := pool.PingContext(t.Context())
	if refusal == nil {
		t.Fatal("the driver served a locator this case needs it to refuse, so the failure under " +
			"test is not the driver's refusal")
	}
	return refusal
}

// statementRefusal produces the driver's own error for a statement, on a
// connection of the case's own and through the same call a migration goes
// through.
func statementRefusal(t *testing.T, statement string) error {
	t.Helper()
	session, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "refusal.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("opening a connection to produce the driver's own error: %v", err)
	}
	pool, err := session.DB()
	if err != nil {
		t.Fatalf("reaching the connection pool behind it: %v", err)
	}
	defer func() { _ = pool.Close() }()
	refusal := session.Exec(statement).Error
	if refusal == nil {
		t.Fatal("the driver accepted a statement this case needs it to refuse")
	}
	return refusal
}

// pluginRefusal is what a plugin reports when it will not install. It has a type
// of its own so that reaching it is observed with errors.As rather than by
// reading the text, which a %v would leave in place.
type pluginRefusal struct{ reason string }

func (e *pluginRefusal) Error() string { return "the audit plugin will not install: " + e.reason }

// refusingPlugin is a declared plugin whose installation fails.
type refusingPlugin struct{ err error }

func (p refusingPlugin) Name() string { return "auditing" }

func (p refusingPlugin) Initialize(*gorm.DB) error { return p.err }

// refusingLock is a migration mutex held by another replica for the whole
// allowance the run configured, which it then reports together with an error of
// its own.
type refusingLock struct {
	// allowance is the configured bound on the wait, handed to the mutex the
	// way an implementation receives it.
	allowance time.Duration
	// holder is what the mutex puts underneath the sentinel, standing for what
	// a real one says about the replica it waited for.
	holder error
}

func (l *refusingLock) Acquire(ctx context.Context, _ *sql.Conn) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("db_test: waiting for the migration mutex was cut short: %w", ctx.Err())
	case <-time.After(l.allowance):
	}
	return fmt.Errorf("%w: another replica held the migration mutex for the whole %s, and %s.%s "+
		"says to give up: %w", db.ErrMigrationLockTimeout, l.allowance, hostNamespace,
		db.MigrationLockTimeoutKey, l.holder)
}

// Release gives the mutex up. The run never calls it on this path — it releases
// only a mutex it took — and its own half is pinned in pkg/db/postgres.
func (l *refusingLock) Release(context.Context, *sql.Conn) error { return nil }
