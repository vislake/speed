package config

import (
	"testing"
	"time"
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
	// Invalidation of a row that was never cached is a no-op.
	c.invalidate("brand.site_name", ScopeSystem, "")
}

func TestWatchers_FireInRegistrationOrder(t *testing.T) {
	w := &watchers{byKey: make(map[string][]watch)}
	var got []string
	w.add("brand.site_name", func(v Value) { got = append(got, "first:"+v.Data.(string)) })
	w.add("brand.site_name", func(v Value) { got = append(got, "second:"+v.Data.(string)) })
	w.add("support.reply_email", func(v Value) { got = append(got, "other") })

	w.fire("brand.site_name", Value{Data: "Studio A", Scope: ScopeTenant})

	if len(got) != 2 || got[0] != "first:Studio A" || got[1] != "second:Studio A" {
		t.Fatalf("watchers fired out of order or for the wrong key: %v", got)
	}
}

func TestWatchers_FireSkipsKeysWithoutWatchers(t *testing.T) {
	w := &watchers{byKey: make(map[string][]watch)}
	w.fire("brand.site_name", Value{Data: "Studio A"}) // must not panic
}

func TestWatchers_ContainPanicOfOneCallback(t *testing.T) {
	w := &watchers{byKey: make(map[string][]watch)}
	w.add("brand.site_name", func(Value) { panic("first callback blew up") })
	fired := false
	w.add("brand.site_name", func(v Value) { fired = v.Data == "Studio A" })

	w.fire("brand.site_name", Value{Data: "Studio A"})

	if !fired {
		t.Fatal("a panicking watcher must not prevent later watchers from firing")
	}
}

func TestWatchers_DuplicateRegistrationFiresTwice(t *testing.T) {
	w := &watchers{byKey: make(map[string][]watch)}
	calls := 0
	fn := func(Value) { calls++ }
	w.add("brand.site_name", fn)
	w.add("brand.site_name", fn)

	w.fire("brand.site_name", Value{})

	if calls != 2 {
		t.Fatalf("a doubly registered watcher fired %d times, want 2", calls)
	}
}
