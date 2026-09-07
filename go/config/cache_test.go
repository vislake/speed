package config

import (
	"context"
	"log/slog"
	"testing"
	"time"

	obs "github.com/vislake/speed/go/observability"
)

// Tests for cache.go's two in-process structures: the read-through row
// cache (entries keyed by the exact row triple, invalidation by the same)
// and the Watch registry (callbacks keyed by configuration key, fired in
// registration order with panic containment).

func TestValueCache_PutGetRoundTrip(t *testing.T) {
	c := newValueCache()
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	c.put("brand.site_name", ScopeSystem, "", "Smile Studio", at)

	entry, ok := c.get("brand.site_name", ScopeSystem, "")
	if !ok {
		t.Fatal("the cached row was not found")
	}
	if entry.canonical != "Smile Studio" || !entry.updatedAt.Equal(at) {
		t.Fatalf("cached entry = %+v, want canonical %q at %v", entry, "Smile Studio", at)
	}
}

func TestValueCache_KeyedByExactRowTriple(t *testing.T) {
	c := newValueCache()
	c.put("brand.site_name", ScopeTenant, "tenant-a", "Studio A", time.Now())
	c.put("brand.site_name", ScopeTenant, "tenant-b", "Studio B", time.Now())
	c.put("brand.site_name", ScopeSystem, "", "Global Co", time.Now())

	if _, ok := c.get("brand.site_name", ScopeTenant, "tenant-a"); !ok {
		t.Fatal("tenant-a's row is missing")
	}
	// A miss must not be served from another scope's or tenant's entry: the
	// scope fallback lives in the Service, not in the cache.
	if _, ok := c.get("brand.site_name", ScopeTenant, "tenant-c"); ok {
		t.Fatal("an entry appeared for a tenant that has no row")
	}
	if _, ok := c.get("brand.site_name", ScopeTenant, ""); ok {
		t.Fatal("a tenant-tier get matched the system-tier entry")
	}
	entry, ok := c.get("brand.site_name", ScopeTenant, "tenant-b")
	if !ok || entry.canonical != "Studio B" {
		t.Fatalf("tenant-b's entry = %+v, want canonical %q", entry, "Studio B")
	}
}

func TestValueCache_InvalidateDropsOneRow(t *testing.T) {
	c := newValueCache()
	c.put("brand.site_name", ScopeTenant, "tenant-a", "Studio A", time.Now())
	c.put("support.reply_email", ScopeTenant, "tenant-a", "ops@example.com", time.Now())

	c.invalidate("brand.site_name", ScopeTenant, "tenant-a")
	if _, ok := c.get("brand.site_name", ScopeTenant, "tenant-a"); ok {
		t.Fatal("invalidate left the entry behind")
	}
	if _, ok := c.get("support.reply_email", ScopeTenant, "tenant-a"); !ok {
		t.Fatal("invalidate dropped a different row")
	}
	// Invalidation of a row that was never cached drops nothing from the
	// entries map (it still advances the mutation generation: the row
	// changed, and an in-flight backfill of the pre-change value must not
	// land).
	c.invalidate("brand.site_name", ScopeSystem, "")
}

// The putIfUnchanged tests pin the read-through backfill guard: a backfill
// captures the cache's mutation generation before its store read and must
// not land once any mutation -- a concurrent Set's put, its invalidate, a
// poller sweep -- has happened in between. The sequences below are the
// exact interleavings resolveRow's backfill races (service.go); each is
// driven deterministically at the cache seam rather than by timing.

func TestValueCache_PutIfUnchanged_BackfillLandsWhenNothingChanged(t *testing.T) {
	c := newValueCache()
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	// The quiet case: a reader missed, captured the generation, read the
	// row, and nothing mutated the cache while it read -- the backfill is
	// exactly what the cache should hold.
	gen := c.generation()
	c.putIfUnchanged("brand.site_name", ScopeTenant, "tenant-a", "Studio A", at, gen)

	entry, ok := c.get("brand.site_name", ScopeTenant, "tenant-a")
	if !ok || entry.canonical != "Studio A" {
		t.Fatalf("the quiet backfill did not land: %+v, %v", entry, ok)
	}
}

func TestValueCache_PutIfUnchanged_BackfillAcrossAWriteAndItsInvalidateIsDropped(t *testing.T) {
	c := newValueCache()
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	// A reader missed, captured the generation, and its store read (about
	// to return the pre-write row) is in flight. A concurrent Set lands in
	// the meantime: its own put, then its invalidate (the order Set and the
	// synchronous bus subscriber produce). The reader's backfill of the
	// pre-write value must be dropped -- landing it would refill a slot the
	// writer just invalidated with a value the writer already superseded.
	gen := c.generation()
	c.put("brand.site_name", ScopeTenant, "tenant-a", "Studio B", at)
	c.invalidate("brand.site_name", ScopeTenant, "tenant-a")
	c.putIfUnchanged("brand.site_name", ScopeTenant, "tenant-a", "Studio A", at.Add(-time.Second), gen)

	if _, ok := c.get("brand.site_name", ScopeTenant, "tenant-a"); ok {
		t.Fatal("a backfill that crossed a write's invalidate landed; the pre-write value is cached")
	}
}

func TestValueCache_PutIfUnchanged_BackfillCannotOverwriteTheWritersOwnPut(t *testing.T) {
	c := newValueCache()
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	// The writer's own put is a mutation too: a backfill captured before it
	// must not overwrite the fresh value even in the instant before the
	// writer's invalidate arrives (the window that exists on the
	// asynchronous distributed bus, where the writer's own event delivery
	// is not synchronous with its put).
	gen := c.generation()
	c.put("brand.site_name", ScopeTenant, "tenant-a", "Studio B", at)
	c.putIfUnchanged("brand.site_name", ScopeTenant, "tenant-a", "Studio A", at.Add(-time.Second), gen)

	entry, ok := c.get("brand.site_name", ScopeTenant, "tenant-a")
	if !ok || entry.canonical != "Studio B" {
		t.Fatalf("a stale backfill overwrote the writer's fresh put: %+v, %v", entry, ok)
	}
}

func TestValueCache_PutIfUnchanged_AnOlderCaptureCannotLandAfterANewerOne(t *testing.T) {
	c := newValueCache()
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	// Two readers miss the same row. The first captures, reads and lands
	// its backfill; the second -- whose store read began no later -- must
	// not overwrite it afterwards: the first's success advanced the
	// generation, so the second's older capture is refused.
	older := c.generation()
	first := c.generation()
	c.putIfUnchanged("brand.site_name", ScopeTenant, "tenant-a", "Studio A", at, first)
	c.putIfUnchanged("brand.site_name", ScopeTenant, "tenant-a", "Studio B", at, older)

	entry, ok := c.get("brand.site_name", ScopeTenant, "tenant-a")
	if !ok || entry.canonical != "Studio A" {
		t.Fatalf("an older-capture backfill overwrote a newer one: %+v, %v", entry, ok)
	}
}

func TestValueCache_InvalidateAll_DropsInFlightBackfillsOfTheWipedEra(t *testing.T) {
	c := newValueCache()
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	// The periodic full reconciliation (Refresh's invalidateAll) is a
	// mutation like any other: a backfill captured before the wipe must not
	// repopulate the fresh cache with a row the reconciliation just
	// retired.
	gen := c.generation()
	c.invalidateAll()
	c.putIfUnchanged("brand.site_name", ScopeTenant, "tenant-a", "Studio A", at, gen)

	if _, ok := c.get("brand.site_name", ScopeTenant, "tenant-a"); ok {
		t.Fatal("a backfill from before the full reconciliation landed after the wipe")
	}
}

func TestValueCache_PutMissing_StoresACachedAbsenceUntilARowReplacesIt(t *testing.T) {
	c := newValueCache()
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	// A no-row store answer is cached as an absence sentinel: get reports
	// the triple cached, with missing set -- repeated reads of an unset key
	// must not each consult the store.
	c.putMissing("brand.site_name", ScopeTenant, "tenant-a", c.generation())
	entry, ok := c.get("brand.site_name", ScopeTenant, "tenant-a")
	if !ok || !entry.missing {
		t.Fatalf("the cached absence is not served as missing: %+v, %v", entry, ok)
	}

	// A later Set that creates the row lands its own put at the same triple
	// and thereby replaces the sentinel: the cache must serve the row from
	// then on, never the stale absence.
	c.put("brand.site_name", ScopeTenant, "tenant-a", "Studio A", at)
	entry, ok = c.get("brand.site_name", ScopeTenant, "tenant-a")
	if !ok || entry.missing || entry.canonical != "Studio A" {
		t.Fatalf("a put did not supersede the cached absence: %+v, %v", entry, ok)
	}

	// The invalidation paths drop a sentinel exactly like a row entry: the
	// change subscriber's invalidate and the poller's full reconciliation
	// alike.
	c.putMissing("brand.site_name", ScopeTenant, "tenant-a", c.generation())
	c.invalidate("brand.site_name", ScopeTenant, "tenant-a")
	if _, ok := c.get("brand.site_name", ScopeTenant, "tenant-a"); ok {
		t.Fatal("invalidate left the cached absence behind")
	}
	c.putMissing("brand.site_name", ScopeTenant, "tenant-a", c.generation())
	c.invalidateAll()
	if _, ok := c.get("brand.site_name", ScopeTenant, "tenant-a"); ok {
		t.Fatal("invalidateAll left the cached absence behind")
	}
}

func TestValueCache_PutMissing_BackfillAcrossAWriteIsDropped(t *testing.T) {
	c := newValueCache()
	at := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

	// The mirror of putIfUnchanged's guard, driven at the cache seam: a
	// no-row reader captured the generation before its store read, and a
	// concurrent Set created the row (its own put) while the read was in
	// flight. The stale sentinel must not land over the writer's fresh
	// entry -- the absence would outlive the write that superseded it.
	gen := c.generation()
	c.put("brand.site_name", ScopeTenant, "tenant-a", "Studio A", at)
	c.putMissing("brand.site_name", ScopeTenant, "tenant-a", gen)

	entry, ok := c.get("brand.site_name", ScopeTenant, "tenant-a")
	if !ok || entry.missing {
		t.Fatalf("a stale absence backfill overwrote the writer's fresh put: %+v, %v", entry, ok)
	}
	if entry.canonical != "Studio A" {
		t.Fatalf("the writer's row = %+v, want canonical %q", entry, "Studio A")
	}
}

func TestWatchers_FireInRegistrationOrder(t *testing.T) {
	w := &watchers{byKey: make(map[string][]watch)}
	var got []string
	w.add("brand.site_name", func(v Value) { got = append(got, "first:"+v.Data.(string)) })
	w.add("brand.site_name", func(v Value) { got = append(got, "second:"+v.Data.(string)) })
	w.add("support.reply_email", func(v Value) { got = append(got, "other") })

	w.fire(context.Background(), "brand.site_name", Value{Data: "Studio A", Scope: ScopeTenant})

	if len(got) != 2 || got[0] != "first:Studio A" || got[1] != "second:Studio A" {
		t.Fatalf("watchers fired out of order or for the wrong key: %v", got)
	}
}

func TestWatchers_FireSkipsKeysWithoutWatchers(t *testing.T) {
	w := &watchers{byKey: make(map[string][]watch)}
	w.fire(context.Background(), "brand.site_name", Value{Data: "Studio A"}) // must not panic
}

func TestWatchers_ContainPanicOfOneCallback(t *testing.T) {
	w := &watchers{byKey: make(map[string][]watch)}
	logs := &capturedLogs{}
	ctx := obs.WithLogger(context.Background(), slog.New(logs))
	w.add("brand.site_name", func(Value) { panic("first callback blew up") })
	fired := false
	w.add("brand.site_name", func(v Value) { fired = v.Data == "Studio A" })

	w.fire(ctx, "brand.site_name", Value{Data: "Studio A"})

	if !fired {
		t.Fatal("a panicking watcher must not prevent later watchers from firing")
	}
	// The recovery must report itself: a panic silently dropped into the
	// blank identifier makes a buggy host callback disappear with no
	// diagnostics (the regression this log assertion pins).
	logs.errorAboutPanic(t, "brand.site_name", 0, "first callback blew up")
}

func TestWatchers_DuplicateRegistrationFiresTwice(t *testing.T) {
	w := &watchers{byKey: make(map[string][]watch)}
	calls := 0
	fn := func(Value) { calls++ }
	w.add("brand.site_name", fn)
	w.add("brand.site_name", fn)

	w.fire(context.Background(), "brand.site_name", Value{})

	if calls != 2 {
		t.Fatalf("a doubly registered watcher fired %d times, want 2", calls)
	}
}
