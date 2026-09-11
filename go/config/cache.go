package config

import (
	"context"
	"fmt"
	"sync"
	"time"

	obs "github.com/vislake/speed/go/observability"
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

// cacheEntry is one cached answer for an exact row triple: the canonical
// value plus the row's updated_at for a row that exists, or an absence
// sentinel (missing == true; canonical and updatedAt are zero) for a triple
// a store read confirmed has no row. Serving cached absence is what keeps
// repeated reads of unset keys off the store (see the valueCache type's doc
// comment).
type cacheEntry struct {
	canonical string
	updatedAt time.Time
	missing   bool
}

// valueCache is the process-local, read-through cache of configs rows:
// hot-path reads go through it and are invalidated on change, never a
// per-read database query. The cache holds
// two shapes for one exact (key, scope, tenant) triple: the row itself when
// one exists, and an absence sentinel (cacheEntry.missing) when a store
// read confirmed that no row exists there. Absence is cached because an
// unset key is a normal steady state, not an error path -- every tenant
// read of a flag or public item without an override row falls through the
// tenant and system tiers to the platform default, which is exactly the
// pre-auth login-page answer -- so a no-row read that populated nothing
// would make every one of those requests pay two database queries each.
//
// A sentinel can only be wrong if a row appears at its triple, and every
// path that creates a row clears the way for the fresh answer on the same
// mutation: Set's own put lands at the exact triple (overwriting the
// sentinel), the config.item.changed subscriber invalidates the triple on
// a remote change, and the poller invalidates any row its sweep finds
// appeared meanwhile, the periodic full reconciliation evicting entries
// wholesale on top of that (see service.go). A sentinel is planted under
// the same mutation-generation guard as a row backfill (putMissing), so a
// no-row read that raced a concurrent write never caches the absence past
// the write that superseded it. Its memory is bounded by the frozen
// schema: keys come from the Attach-time declarations, never from callers,
// so no request can grow the cache with keys of its own.
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
	// putIfUnchanged or putMissing, invalidate and invalidateAll alike. A
	// read-through backfill captures it (via the generation method) before
	// its store read and refuses to land (putIfUnchanged/putMissing) once
	// it has moved: a mutation in between means the row may have changed
	// since the read began, so the backfill could plant a value -- or an
	// absence -- the writer already superseded. See putIfUnchanged,
	// putMissing and (*Service).resolveRow. A wrap at 2^64 mutations is not
	// guarded against: reaching it would take longer than the cache's
	// lifetime on any real schedule of config writes, and a wrap would only
	// drop a backfill, never serve a stale one.
	gen uint64
}

// newValueCache returns an empty cache.
func newValueCache() *valueCache {
	return &valueCache{entries: make(map[cacheKey]cacheEntry)}
}

// get returns the cached answer for the exact triple keyed by (key, scope,
// tenant). The boolean reports whether the triple was cached at all; a
// cached answer is either the row itself or a confirmed-absence sentinel
// (entry.missing), which resolveRow distinguishes. A miss means the caller
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
// (*Service).Set stores the value it just wrote, so it always lands -- and
// its landing at the exact triple is what supersedes a cached absence
// sentinel there: the writer is the answer to that absence, so the sentinel
// cannot outlive the row's creation. Like every mutation it advances
// generation, which is what makes a concurrent read-through backfill that
// captured the older generation drop instead of overwriting this fresh
// value (see putIfUnchanged).
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

// putMissing caches the confirmed absence of a row for one exact (key,
// scope, tenant) triple -- the read-through path for a store read that
// found no row (see (*Service).resolveRow). It lands only when no cache
// mutation happened since the caller captured gen, the identical guard
// putIfUnchanged applies to a row backfill: a mutation in the window means
// the absence may already be stale -- a concurrent Set's own put of the row
// it just wrote, its invalidate, a poller sweep, a remote
// config.item.changed -- so the sentinel is dropped rather than planted
// past the write that superseded it. Dropping costs one store read on the
// next access, never a stale absence. On success it advances generation
// exactly like every other mutation, so a later backfill -- row or absence
// alike -- from an older capture drops too.
func (c *valueCache) putMissing(key string, scope Scope, tenant pkgcore.TenantID, captured uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != captured {
		return
	}
	c.entries[cacheKey{key: key, scope: scope, tenant: tenant}] = cacheEntry{missing: true}
	c.gen++
}

// invalidate drops the cached answer -- a row or an absence sentinel alike
// -- for one exact (key, scope, tenant). It is the invalidation half of
// every "value changed" path: the local Set, the config.item.changed
// subscriber and the poller all converge on it, so a stale entry cannot
// survive whichever of the three noticed the change first. Generation
// advances whether or not an entry was present to drop:
// the call itself reports a change to the row, and an in-flight
// read-through backfill of the superseded value must not land after it.
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
// back into the Service. A panicking callback is contained the same way
// the in-process bus contains a panicking subscriber: the panic is
// recovered, logged as an Error naming the key, the callback's position in
// registration order and the panic value, and the callbacks registered
// after it still run -- so a buggy host watcher can never tear through a
// Set that may be running on a goroutine with no recover of its own. The
// log goes through the delivery context's logger (obs.FromContext), the
// same logger the Set or event delivery that triggered the fire is running
// under, so the record carries that change's tenant and trace correlation;
// a delivery context carrying no logger falls back to the process default,
// never to silence. The dropped callback's value change is still served
// correctly: the cache was invalidated before watches fired, so the next
// read re-reads the store.
func (w *watchers) fire(ctx context.Context, key string, value Value) {
	w.mu.RLock()
	registered := w.byKey[key]
	w.mu.RUnlock()
	for i, entry := range registered {
		func() {
			defer func() {
				if r := recover(); r != nil {
					obs.FromContext(ctx).Error("config: watcher callback panicked; recovered so the key's remaining watchers still run",
						"item", key,
						"watcher", i,
						"panic", fmt.Sprintf("%v", r),
					)
				}
			}()
			entry.fn(value)
		}()
	}
}
