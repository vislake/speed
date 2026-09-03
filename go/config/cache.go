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

// put caches the canonical value of one row.
func (c *valueCache) put(key string, scope Scope, tenant pkgcore.TenantID, canonical string, updatedAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[cacheKey{key: key, scope: scope, tenant: tenant}] = cacheEntry{canonical: canonical, updatedAt: updatedAt}
}

// invalidate drops the cached row for one exact (key, scope, tenant). It is
// the invalidation half of every "value changed" path: the local Set, the
// config.item.changed subscriber and the poller all converge on it, so a
// stale entry cannot survive whichever of the three noticed the change
// first.
func (c *valueCache) invalidate(key string, scope Scope, tenant pkgcore.TenantID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, cacheKey{key: key, scope: scope, tenant: tenant})
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
