package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/vislake/speed/go/dbkit"
)

// TestWithDefaultPragmas pins the DSN-merging logic on its own, for every
// DSN shape this package's callers are known to pass to dbkit.Open: a bare
// file path with no query string at all, a "file:" URI that already carries
// one query parameter, and a bare ":memory:" DSN — see
// go/dbkit/audit/repository_test.go, go/dbkit/audit/example_test.go and
// go/dbkit/open_test.go for the exact strings this mirrors.
//
// The pragma fragment is order-sensitive at the driver end: glebarez/go-
// sqlite applies "_pragma" params in DSN order, last application winning, so
// the last expectation below pins that this package's own busy_timeout is
// appended AFTER a caller-supplied one and therefore deliberately overrides
// it — one fixed default per AGENTS.md's "SQLite busy timeout" section.
func TestWithDefaultPragmas(t *testing.T) {
	wantPragmas := "_pragma=recursive_triggers(1)&_pragma=busy_timeout(" + strconv.Itoa(defaultBusyTimeoutMS) + ")"
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "bare_file_path_no_query",
			dsn:  "/tmp/some-dir/app.db",
			want: "/tmp/some-dir/app.db?" + wantPragmas,
		},
		{
			name: "bare_memory_no_query",
			dsn:  ":memory:",
			want: ":memory:?" + wantPragmas,
		},
		{
			name: "file_uri_with_existing_query",
			dsn:  "file::memory:?cache=shared",
			want: "file::memory:?cache=shared&" + wantPragmas,
		},
		{
			name: "file_uri_with_mode_and_cache",
			dsn:  "file:audit_test_1?mode=memory&cache=shared",
			want: "file:audit_test_1?mode=memory&cache=shared&" + wantPragmas,
		},
		{
			name: "caller_supplied_busy_timeout_is_overridden",
			dsn:  "file:app.db?_pragma=busy_timeout(100)",
			want: "file:app.db?_pragma=busy_timeout(100)&" + wantPragmas,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withDefaultPragmas(tt.dsn); got != tt.want {
				t.Errorf("withDefaultPragmas(%q) = %q, want %q", tt.dsn, got, tt.want)
			}
		})
	}
}

// TestOpen_EnablesRecursiveTriggersOnEveryPooledConnection re-proves the
// actual effect withRecursiveTriggers exists for: PRAGMA recursive_triggers
// must read back ON on every physical connection dbkit.Open's pool can hand
// out, not merely on whichever connection happened to open first. It holds
// several sql.Conn handles open at once (never releasing one before
// acquiring the next), which forces database/sql's pool to establish that
// many distinct physical connections rather than serving every acquisition
// from one reused connection, and reads the pragma back on each one.
func TestOpen_EnablesRecursiveTriggersOnEveryPooledConnection(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "recursive-triggers-test.db")

	db, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: dsn})
	if err != nil {
		t.Fatalf("dbkit.Open: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("db.DB(): %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	const connections = 8
	conns := make([]*sql.Conn, connections)
	for i := range conns {
		conn, err := sqlDB.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn() #%d: %v", i, err)
		}
		conns[i] = conn
	}
	t.Cleanup(func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	if got := sqlDB.Stats().OpenConnections; got < 2 {
		t.Skip("pool reports fewer than 2 open connections -- cannot distinguish per-connection application from a one-time PRAGMA in this run alone")
	}

	for i, conn := range conns {
		var value int
		if err := conn.QueryRowContext(ctx, `PRAGMA recursive_triggers`).Scan(&value); err != nil {
			t.Fatalf("PRAGMA recursive_triggers on connection #%d: %v", i, err)
		}
		if value != 1 {
			t.Fatalf("PRAGMA recursive_triggers on connection #%d = %d, want 1 (ON)", i, value)
		}
	}
}
