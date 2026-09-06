package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/vislake/speed/go/dbkit"
)

// TestWithRecursiveTriggers pins the DSN-merging logic on its own, for every
// DSN shape this package's callers are known to pass to dbkit.Open: a bare
// file path with no query string at all, a "file:" URI that already carries
// one query parameter, and a bare ":memory:" DSN — see
// go/dbkit/audit/repository_test.go, go/dbkit/audit/example_test.go and
// go/dbkit/open_test.go for the exact strings this mirrors.
func TestWithRecursiveTriggers(t *testing.T) {
	tests := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "bare_file_path_no_query",
			dsn:  "/tmp/some-dir/app.db",
			want: "/tmp/some-dir/app.db?_pragma=recursive_triggers(1)",
		},
		{
			name: "bare_memory_no_query",
			dsn:  ":memory:",
			want: ":memory:?_pragma=recursive_triggers(1)",
		},
		{
			name: "file_uri_with_existing_query",
			dsn:  "file::memory:?cache=shared",
			want: "file::memory:?cache=shared&_pragma=recursive_triggers(1)",
		},
		{
			name: "file_uri_with_mode_and_cache",
			dsn:  "file:audit_test_1?mode=memory&cache=shared",
			want: "file:audit_test_1?mode=memory&cache=shared&_pragma=recursive_triggers(1)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withRecursiveTriggers(tt.dsn); got != tt.want {
				t.Errorf("withRecursiveTriggers(%q) = %q, want %q", tt.dsn, got, tt.want)
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
