package rbac

import (
	"sync"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// A stale authorization cache is a security failure, not a performance
// one: it keeps answering "yes" after a revoke. The cache below is
// therefore built around invalidation first and speed second. The
// performance half of the same requirement is
// docs/internal/05-identity-and-access.md's: decisions must be served from
// a policy cache rather than loaded in full on every request.
//
// Three mechanisms keep it honest, in decreasing order of how much is
// riding on each:
//
//  1. Event invalidation. Every assign, revoke and role change publishes on
//     the pkgcore.EventBus, and the Service's subscriber drops the affected
//     entries. In the standalone deployment mode the in-memory bus delivers
//     synchronously inside the writing call, so a local revoke is visible
//     before it returns. In the distributed mode the Redis Streams bus
//     carries it to every replica.
//  2. TTL expiry. An entry older than the TTL is a miss regardless of
//     events, so a dropped or undelivered event costs at most one TTL of
//     staleness rather than an unbounded amount. This is the anti-loss net
//     behind mechanism 1, the same role go/config's poller plays there.
//  3. A janitor goroutine that sweeps expired entries, so a process that
//     evaluates many subjects once each does not retain their grants
//     forever. It is a memory bound, not a correctness mechanism -- (2)
//     already makes an expired entry unusable before the janitor runs.

// grantKey addresses one cached decision set: a subject is a
// (tenant, user) pair, and so is its grant set.
type grantKey struct {
	tenant pkgcore.TenantID
	user   string
}

// grantEntry is one subject's effective grants plus when they were read.
//
// The grants map is treated as IMMUTABLE once stored: it is built inside
// loadGrants, handed to put, and only ever read afterwards. Nothing
// mutates a stored map, which is what lets readers share it across
// goroutines without holding the cache lock while they evaluate.
type grantEntry struct {
	grants   map[string]permissionGrant
	loadedAt time.Time
}

// permissionGrant records where one permission is granted for one subject:
// at the tenant root, over specific organization nodes, or both.
//
// It stores node IDs, never resolved paths. Resolution happens per
// decision through the host's SubtreeResolver, so a member who moves in
// the organization tree changes scope immediately -- the requirement
// docs/internal/16-verification.md pins: a member's permissions must
// follow immediately when they move within the tree. Caching paths here
// would reintroduce exactly the staleness that requirement forbids, one
// layer further in than the binding row does.
type permissionGrant struct {
	// tenantWide is true when at least one binding grants this permission
	// at the tenant root.
	tenantWide bool

	// nodeIDs holds the organization nodes the permission is granted over,
	// de-duplicated. It is empty when every grant is tenant-wide.
	nodeIDs []string
}

// grantCache is the process-local decision cache. The zero value is not
// usable; build one with newGrantCache.
type grantCache struct {
	// mu guards entries. It is a RWMutex because the read path (every
	// authorization decision) vastly outnumbers the write path (a grant
	// change, or the first read of a subject).
	mu      sync.RWMutex
	entries map[grantKey]grantEntry

	// ttl is the anti-loss expiry from DefaultCacheTTL or WithCacheTTL.
	ttl time.Duration

	// stopOnce, stop and done coordinate the janitor's shutdown. stop is
	// nil when no janitor was started (ttl <= 0, which tests use to get a
	// cache with no background goroutine).
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// newGrantCache returns a cache whose entries expire after ttl and starts
// its janitor. A ttl of zero or less yields a cache that never serves a
// hit and starts no goroutine -- the "disabled" setting, useful in tests
// that want every decision to read through to the database.
func newGrantCache(ttl time.Duration) *grantCache {
	c := &grantCache{
		entries: make(map[grantKey]grantEntry),
		ttl:     ttl,
	}
	c.startJanitor()
	return c
}

// get returns the cached grants for a subject when they are present and
// not yet expired. now is passed in rather than read from the clock so the
// expiry boundary is testable without sleeping.
func (c *grantCache) get(key grantKey, now time.Time) (map[string]permissionGrant, bool) {
	if c.ttl <= 0 {
		return nil, false
	}

	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if now.Sub(entry.loadedAt) >= c.ttl {
		return nil, false
	}
	return entry.grants, true
}

// put stores a subject's grants. The caller must not mutate grants
// afterwards; see grantEntry.
func (c *grantCache) put(key grantKey, grants map[string]permissionGrant, now time.Time) {
	if c.ttl <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = grantEntry{grants: grants, loadedAt: now}
}

// invalidate drops one subject's entry. It is what an assign or a revoke
// triggers: only that subject's grants changed.
func (c *grantCache) invalidate(key grantKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// invalidateTenant drops every entry belonging to one tenant. It is what a
// role change triggers: the set of permissions a role carries changed, and
// the cache stores grants already flattened through their roles, so every
// subject in that tenant may be affected and there is no index from role
// back to subject that would let this be narrower.
//
// The scan is O(entries in the process) and runs on a role change, which
// is an administrative action rather than a request-path one.
func (c *grantCache) invalidateTenant(tenant pkgcore.TenantID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if key.tenant == tenant {
			delete(c.entries, key)
		}
	}
}

// len reports how many entries are held, expired ones included. Only the
// janitor's tests need it.
func (c *grantCache) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// sweep removes every entry that has expired as of now.
func (c *grantCache) sweep(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key, entry := range c.entries {
		if now.Sub(entry.loadedAt) >= c.ttl {
			delete(c.entries, key)
		}
	}
}

// startJanitor launches the sweep goroutine, unless the cache is disabled.
func (c *grantCache) startJanitor() {
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

// close stops the janitor and waits for it to exit. It is idempotent, and
// the cache stays correct afterwards: without a janitor entries are still
// expired by get, they are simply no longer reclaimed.
func (c *grantCache) close() {
	if c.stop == nil {
		return
	}
	c.stopOnce.Do(func() { close(c.stop) })
	<-c.done
}
