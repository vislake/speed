package config

import (
	"sync"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// cacheKey addresses one row of the configs table in the process-local
// cache: the row is uniquely identified by (key, scope, tenant_id) -- its
// primary key -- so the cache is keyed exactly like the table. tenant is
// empty for ScopeSystem rows.
type cacheKey struct {
	key    string
	scope  Scope
	tenant pkgcore.TenantID
}

// cacheEntry is one cached row: the canonical value plus the row's
// updated_at, which the poller's watermark comparison needs.
type cacheEntry struct {
	canonical string
	updatedAt time.Time
}

// valueCache is the process-local, read-through cache of configs rows,
// implementing the design's dynamic-config rule (docs/internal/11-cross-
// cutting.md) that hot-path reads go through a process-local cache
// invalidated on change, never a per-read database query. Only rows that
// exist are cached -- a read
// that finds no row at a scope simply does not populate an entry, so a
// later Set that creates the row can only leave a stale cache behind if it
// forgets to invalidate (which the Set path and the event subscriber both
// do; see service.go).
//
// A cache entry holds the row's canonical value in the clear. That is
// deliberate: for Sensitive items the canonical value is the decrypted
// plaintext, and the cache is process memory behind the service's own
// access path -- the at-rest encryption guarantee concerns what reaches
// the table, not what the owning process holds in RAM. What the cache
// never holds is irrelevant to it: nothing here is ever logged, traced or
// served publicly, which is where redaction actually applies (see
// events.go's redactIf and http.go's public endpoint).
type valueCache struct {
	mu      sync.RWMutex
	entries map[cacheKey]cacheEntry

	// gen counts every mutation of entries -- put, a successful
	// putIfUnchanged, invalidate and invalidateAll alike. A read-through
	// backfill captures it (via the generation method) before its store
	// read and refuses to land (putIfUnchanged) once it has moved: a
	// mutation in between means the row may have changed since the read
	// began, so the backfill could plant a value the writer already
	// superseded. See putIfUnchanged and (*Service).resolveRow. A wrap at
	// 2^64 mutations is not guarded against: reaching it would take longer
	// than the cache's lifetime on any real schedule of config writes, and
	// a wrap would only drop a backfill, never serve a stale one.
	gen uint64
}

// newValueCache returns an empty cache.
func newValueCache() *valueCache {
	return &valueCache{entries: make(map[cacheKey]cacheEntry)}
}

// get returns the cached canonical value for the exact row keyed by
// (key, scope, tenant). The boolean reports whether the row was cached at
// all; only rows that exist are ever cached, so a miss means the caller
// must consult the store.
func (c *valueCache) get(key string, scope Scope, tenant pkgcore.TenantID) (cacheEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.entries[cacheKey{key: key, scope: scope, tenant: tenant}]
	return entry, ok
}

// generation returns the cache's mutation generation (see the valueCache
// struct's field comment). A caller about to read the store -- a
// read-through backfill -- captures it first, then passes the captured
// value to putIfUnchanged, which lands the backfill only if no mutation
// happened while the read was in flight.
func (c *valueCache) generation() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gen
}

// put caches the canonical value of one row. put is the writer's own path:
// (*Service).Set stores the value it just wrote, so it always lands. Like
// every mutation it advances generation, which is what makes a concurrent
// read-through backfill that captured the older generation drop instead of
// overwriting this fresh value (see putIfUnchanged).
func (c *valueCache) put(key string, scope Scope, tenant pkgcore.TenantID, canonical string, updatedAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[cacheKey{key: key, scope: scope, tenant: tenant}] = cacheEntry{canonical: canonical, updatedAt: updatedAt}
	c.gen++
}

// putIfUnchanged is the read-through backfill path: it caches the row only
// when no cache mutation happened since the caller captured gen (via
// generation, before its store read). A mutation in that window means the
// row may have changed since the read began -- a concurrent Set's own put
// and its invalidate, a poller sweep, a remote config.item.changed -- so
// the backfill is dropped: landing it could overwrite the newer value, or
// refill a slot the writer just invalidated, with a value the writer
// already superseded, leaving the cache stale until the next invalidation
// of that key or the periodic full reconciliation. Dropping costs one
// store read on the next access, never a stale serve. On success it
// advances generation exactly like put, so a later backfill from an older
// capture drops too.
func (c *valueCache) putIfUnchanged(key string, scope Scope, tenant pkgcore.TenantID, canonical string, updatedAt time.Time, captured uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != captured {
		return
	}
	c.entries[cacheKey{key: key, scope: scope, tenant: tenant}] = cacheEntry{canonical: canonical, updatedAt: updatedAt}
	c.gen++
}

// invalidate drops the cached row for one exact (key, scope, tenant). It is
// the invalidation half of every "value changed" path: the local Set, the
// config.item.changed subscriber and the poller all converge on it, so a
// stale entry cannot survive whichever of the three noticed the change
// first. Generation advances whether or not an entry was present to drop:
// the call itself reports a change to the row, and an in-flight
// read-through backfill of the pre-change value must not land after it.
func (c *valueCache) invalidate(key string, scope Scope, tenant pkgcore.TenantID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, cacheKey{key: key, scope: scope, tenant: tenant})
	c.gen++
}

// invalidateAll drops every cached entry at once. It is Refresh's periodic
// full-reconciliation primitive (see fullReconcileEvery in service.go): a
// row whose UpdatedAt landed behind an already-advanced watermark can
// never be selected by the incremental changedSince sweep again, however
// many cycles run, so the only way to bound how long its cache entry can
// stay stale is to evict it -- along with everything else -- on a fixed
// schedule that does not depend on the watermark at all. The next read of
// any key falls through to the store and observes its true current value.
// The one generation advance covers the whole wipe, so an in-flight
// backfill from before the reconciliation is dropped rather than
// repopulating the fresh cache with a row the wipe just retired.
func (c *valueCache) invalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[cacheKey]cacheEntry)
	c.gen++
}

// watch is one registered Watch callback. Watches are keyed by
// configuration key and fire on every config.item.changed event for that
// key, whatever scope or tenant the change happened at (see
// (*Service).Watch and onItemChanged for the exact delivery contract).
type watch struct {
	fn func(Value)
}

// watchers is the process-local Watch registry. It is safe for concurrent
// use: Set's event fan-out may fire callbacks while another goroutine
// registers a new watcher.
type watchers struct {
	mu    sync.RWMutex
	byKey map[string][]watch
}

// add registers fn for key.
func (w *watchers) add(key string, fn func(Value)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.byKey[key] = append(w.byKey[key], watch{fn: fn})
}

// fire delivers value to every callback registered for key, in
// registration order. Callbacks run on the caller's goroutine (the
// publishing Set's, for the in-memory bus); each is invoked with the
// watch mutex released, so a callback may itself register watchers or call
// back into the Service. A callback's behavior is deliberately not
// contained: the bus treats subscriber errors as delivery failures, and a
// panicking callback would take the publisher down with it -- so fire
// recovers panics and drops the callback, the same robustness the
// synchronous in-memory bus gives every other subscriber. The dropped
// callback's value change is still served correctly: the cache was
// invalidated before watches fired, so the next read re-reads the store.
func (w *watchers) fire(key string, value Value) {
	w.mu.RLock()
	registered := w.byKey[key]
	w.mu.RUnlock()
	for _, entry := range registered {
		func() {
			defer func() { _ = recover() }()
			entry.fn(value)
		}()
	}
}
