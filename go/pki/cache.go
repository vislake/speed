package pki

import (
	"sync"
	"time"
)

// The sign/verify path is this module's highest-frequency call -- every
// token issuance and every token verification reaches Service.ActiveSigner
// or Service.VerificationKeys -- so round 1's "query the database on every
// call" behaviour (go/pki/AGENTS.md's "no caching" known limitation) is
// exactly what this cache exists to fix, following the SAME pattern
// go/rbac's decision cache uses (go/rbac/cache.go), for the same reason: a
// stale cache here does not merely slow a request, it can hand back a key
// that has since been retired or fail to see one that has since been
// activated.
//
// Three mechanisms keep it honest, in decreasing order of how much is
// riding on each:
//
//  1. Event invalidation. Every staged/activated/retired transition
//     publishes on the pkgcore.EventBus, and this module's own subscriber
//     drops the affected purpose's entry. In the standalone deployment mode
//     the in-memory bus delivers synchronously inside the writing call, so
//     a local rotation is visible before the call that triggered it
//     returns; in the distributed mode the bus carries it to every replica.
//  2. TTL expiry. An entry older than the TTL is a miss regardless of
//     events, so a dropped or undelivered event costs at most one TTL of
//     staleness -- docs/internal/22-pki.md's own "caching" section calls
//     this out by name: caching still keeps a fallback poll behind it, to
//     cover a missed event.
//  3. A janitor goroutine that sweeps expired entries, so a process that
//     evaluates many purposes once each does not retain them forever. Pure
//     memory bound, not a correctness mechanism: (2) already makes an
//     expired entry unusable before the janitor runs.

// keySetEntry is one purpose's cached key set: the row ActiveSigner should
// use (nil if the purpose currently has none) and the rows VerificationKeys
// should use, plus when this snapshot was read.
//
// Both slices/pointers are treated as IMMUTABLE once stored, the same
// convention grantEntry.grants documents in go/rbac/cache.go: built once
// inside loadKeySet, handed to put, never mutated afterwards, which is what
// lets readers share them across goroutines without holding the cache lock
// while they use them.
type keySetEntry struct {
	active     *SigningKey
	verifiable []SigningKey
	loadedAt   time.Time
}

// keySetCache is the process-local cache of each purpose's current key set.
// The zero value is not usable; build one with newKeySetCache.
type keySetCache struct {
	mu      sync.RWMutex
	entries map[string]keySetEntry

	// ttl is the anti-loss expiry. A cache built with ttl<=0 never serves a
	// hit and starts no janitor -- the "disabled" setting, matching
	// grantCache's identical zero-TTL convention.
	ttl time.Duration

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// newKeySetCache returns a cache whose entries expire after ttl and starts
// its janitor.
func newKeySetCache(ttl time.Duration) *keySetCache {
	c := &keySetCache{
		entries: make(map[string]keySetEntry),
		ttl:     ttl,
	}
	c.startJanitor()
	return c
}

// get returns the cached entry for purpose when present and not yet
// expired. now is passed in rather than read from the clock so the expiry
// boundary is testable without sleeping.
func (c *keySetCache) get(purpose string, now time.Time) (keySetEntry, bool) {
	if c.ttl <= 0 {
		return keySetEntry{}, false
	}
	c.mu.RLock()
	entry, ok := c.entries[purpose]
	c.mu.RUnlock()
	if !ok {
		return keySetEntry{}, false
	}
	if now.Sub(entry.loadedAt) >= c.ttl {
		return keySetEntry{}, false
	}
	return entry, true
}

// put stores purpose's entry unconditionally. The caller must not mutate
// active or verifiable afterwards.
func (c *keySetCache) put(purpose string, active *SigningKey, verifiable []SigningKey, now time.Time) {
	if c.ttl <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[purpose] = keySetEntry{active: active, verifiable: verifiable, loadedAt: now}
}

// invalidate drops one purpose's entry -- what a staged, activated or
// retired transition triggers, whether written locally or observed through
// the event bus from another replica.
func (c *keySetCache) invalidate(purpose string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, purpose)
}

// len reports how many entries are held, expired ones included. Only tests
// need it.
func (c *keySetCache) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// sweep removes every entry that has expired as of now.
func (c *keySetCache) sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for purpose, entry := range c.entries {
		if now.Sub(entry.loadedAt) >= c.ttl {
			delete(c.entries, purpose)
		}
	}
}

// startJanitor launches the sweep goroutine, unless the cache is disabled.
func (c *keySetCache) startJanitor() {
	if c.ttl <= 0 {
		return
	}
	c.stop = make(chan struct{})
	c.done = make(chan struct{})
	go func() {
		defer close(c.done)
		ticker := time.NewTicker(c.ttl)
		defer ticker.Stop()
		for {
			select {
			case <-c.stop:
				return
			case now := <-ticker.C:
				c.sweep(now)
			}
		}
	}()
}

// close stops the janitor and waits for it to exit. Idempotent.
func (c *keySetCache) close() {
	if c.stop == nil {
		return
	}
	c.stopOnce.Do(func() { close(c.stop) })
	<-c.done
}
