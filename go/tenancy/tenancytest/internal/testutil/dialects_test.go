package testutil

import "testing"

// TestDialects_PositionalContractIsSQLiteThenPostgres pins the ordering
// clause of Dialects's doc comment: index 0 is always SQLite and index 1 is
// always PostgreSQL, and every caller in this module addresses an entry
// positionally rather than by name — so a swapped or re-labelled row would
// silently redirect every caller's database choice.
//
// Only the labels are inspected here. The entries' NewDB constructors are
// deliberately not invoked: index 1's is dbtest.NewPostgres, which starts a
// real, disposable PostgreSQL container on every call — exactly the cost a
// plain (non-integration-tagged) _test.go file must never pay, per
// Dialects's own doc comment.
func TestDialects_PositionalContractIsSQLiteThenPostgres(t *testing.T) {
	d := Dialects()
	if len(d) != 2 {
		t.Fatalf("Dialects() returned %d entries, want 2", len(d))
	}
	if d[0].Name != "sqlite" {
		t.Errorf("Dialects()[0].Name = %q, want %q — callers address index 0 as sqlite", d[0].Name, "sqlite")
	}
	if d[1].Name != "postgres" {
		t.Errorf("Dialects()[1].Name = %q, want %q — callers address index 1 as postgres", d[1].Name, "postgres")
	}
}
