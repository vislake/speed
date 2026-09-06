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
	// mu guards entries and slots. It is a RWMutex because the read path
	// (every authorization decision) vastly outnumbers the write path (a
	// grant change, or the first read of a subject).
	mu      sync.RWMutex
	entries map[grantKey]grantEntry

	// slots holds the fence state of database loads currently in flight:
	// one slot per subject, created by beginLoad and released by the
	// load's putIfCurrent or abortLoad. It is the per-subject fencing
	// token grantsFor uses to close the read-then-write race between a
	// database load and a concurrent revoke of THAT subject: see
	// fenceSlot, beginLoad and putIfCurrent below. Keying the fence by
	// subject rather than by the whole process is what lets an
	// invalidation of subject A leave subject B's in-flight load alone.
	slots map[grantKey]*fenceSlot

	// ttl is the anti-loss expiry from DefaultCacheTTL or WithCacheTTL.
	ttl time.Duration

	// stopOnce, stop and done coordinate the janitor's shutdown. stop is
	// nil when no janitor was started (ttl <= 0, which tests use to get a
	// cache with no background goroutine).
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// fenceSlot is one subject's fencing state while one or more database
// loads of that subject are in flight. A slot exists only between
// beginLoad and the matching putIfCurrent or abortLoad; when the last
// in-flight load of the subject ends, the slot is removed (putIfCurrent
// and abortLoad), which keeps the fence's memory bounded by the number of
// subjects loading right now -- a handful -- rather than by every subject
// the process has ever seen. The cache's janitor bounds entries the same
// way.
type fenceSlot struct {
	// gen counts the invalidations that have touched this subject since
	// the slot was created. beginLoad returns it as the load's fence
	// value, and putIfCurrent stores only while it is unchanged: if an
	// invalidation of THIS subject lands between beginLoad and
	// putIfCurrent, gen moves on and the load's result -- read before the
	// invalidation -- is discarded instead of resurrecting what the
	// invalidation just dropped. Invalidations of any OTHER subject never
	// touch this slot, which is the whole point of the per-subject fence.
	gen uint64

	// inflight counts the loads that captured gen and have not yet stored
	// or aborted. It is what lets the slot outlive one load when several
	// concurrent loads of the same subject share it, and what lets the
	// last of them remove the slot.
	inflight int
}

// newGrantCache returns a cache whose entries expire after ttl and starts
// its janitor. A ttl of zero or less yields a cache that never serves a
// hit and starts no goroutine -- the "disabled" setting, useful in tests
// that want every decision to read through to the database.
func newGrantCache(ttl time.Duration) *grantCache {
	c := &grantCache{
		entries: make(map[grantKey]grantEntry),
		slots:   make(map[grantKey]*fenceSlot),
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

// put stores a subject's grants unconditionally. The caller must not
// mutate grants afterwards; see grantEntry.
//
// Only tests and put's own fenced counterpart below write through this
// path unconditionally; grantsFor's read-then-write load uses
// putIfCurrent instead, precisely because an unconditional store is what
// lets a load that is in flight during a revoke resurrect the grant it
// raced -- see putIfCurrent's doc comment.
func (c *grantCache) put(key grantKey, grants map[string]permissionGrant, now time.Time) {
	if c.ttl <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[key] = grantEntry{grants: grants, loadedAt: now}
}

// beginLoad registers a database load of key as in flight and returns the
// fence value the load's eventual putIfCurrent must carry: this subject's
// own invalidation count at the moment the load started. grantsFor calls
// it BEFORE starting the database read, in place of the process-global
// generation counter the fence used to read: because the returned fence
// is per-subject, an invalidation of a DIFFERENT subject (an assign or
// revoke for someone else, in any tenant) cannot discard this load's
// result -- only an invalidation of this subject itself, or of this
// subject's tenant, moves the fence.
//
// The disabled setting (ttl <= 0) registers nothing and returns 0: with
// no cache there is nothing to fence, and the matching putIfCurrent and
// abortLoad no-op.
func (c *grantCache) beginLoad(key grantKey) uint64 {
	if c.ttl <= 0 {
		return 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	slot, ok := c.slots[key]
	if !ok {
		slot = &fenceSlot{}
		c.slots[key] = slot
	}
	slot.inflight++
	return slot.gen
}

// putIfCurrent stores a subject's grants, but only if no invalidation
// touching THIS subject (invalidate of the subject, or invalidateTenant
// of its tenant) has been observed since gen was captured by beginLoad.
// The caller must not mutate grants afterwards; see grantEntry.
//
// This is what closes the race a plain get-miss/load/put would otherwise
// have: goroutine A takes a cache miss and starts a database read; while
// that read is in flight, a revoke of A commits its DELETE and calls
// invalidate(), which has nothing to drop yet because A has not written
// anything -- but it DOES move A's fence slot. When A's stale, pre-revoke
// read finally returns, its put is fenced by the gen it captured before
// the read started: since the slot's gen has moved on, the store is
// discarded instead of resurrecting the just-revoked permission for a
// full cache TTL. See grantsFor in service.go for the caller side of the
// fence, and beginLoad for why the fence is per-subject rather than
// process-wide.
//
// Whether the store lands or is discarded, this call releases the load's
// in-flight registration: the slot's inflight count drops, and the slot
// itself is removed once no load of this subject remains.
func (c *grantCache) putIfCurrent(key grantKey, grants map[string]permissionGrant, now time.Time, gen uint64) {
	if c.ttl <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	slot, ok := c.slots[key]
	if ok {
		slot.inflight--
		if slot.inflight <= 0 {
			delete(c.slots, key)
		}
		if slot.gen != gen {
			// Something invalidated this subject (or its tenant) after the
			// load that produced grants started reading. That load's result
			// may already be stale, so it must not be cached; the next read
			// will load fresh instead.
			return
		}
	}
	// A missing slot means no beginLoad preceded this call (only direct
	// callers can arrange that; grantsFor always pairs beginLoad with
	// putIfCurrent): there is no fence to compare, so the store is
	// unconditional, exactly like put.
	c.entries[key] = grantEntry{grants: grants, loadedAt: now}
}

// abortLoad releases the in-flight registration beginLoad made when the
// load ends in an error and nothing will be stored. Without it, a load
// that failed would leave its subject's slot pinned forever -- the fence's
// memory bound is exactly "slots live only while a load is in flight", and
// a failure is the one path that would otherwise never release its slot.
func (c *grantCache) abortLoad(key grantKey) {
	if c.ttl <= 0 {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if slot, ok := c.slots[key]; ok {
		slot.inflight--
		if slot.inflight <= 0 {
			delete(c.slots, key)
		}
	}
}

// invalidate drops one subject's entry and fences any in-flight load of
// that subject. It is what an assign or a revoke triggers: only that
// subject's grants changed, so no other subject's entry -- and no other
// subject's in-flight load -- is disturbed.
func (c *grantCache) invalidate(key grantKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
	if slot, ok := c.slots[key]; ok {
		slot.gen++
	}
}

// invalidateTenant drops every entry belonging to one tenant and fences
// every in-flight load of a subject in that tenant. It is what a role
// change triggers: the set of permissions a role carries changed, and the
// cache stores grants already flattened through their roles, so every
// subject in that tenant may be affected and there is no index from role
// back to subject that would let this be narrower.
//
// The scan is O(entries + in-flight loads in the process) and runs on a
// role change, which is an administrative action rather than a
// request-path one.
func (c *grantCache) invalidateTenant(tenant pkgcore.TenantID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if key.tenant == tenant {
			delete(c.entries, key)
		}
	}
	// An in-flight load of a subject in this tenant must be fenced even
	// when the subject has no cached entry yet (its load has not stored):
	// the load read the role's permissions before the change, so its
	// result may be stale and must not be cached. The slots scan covers
	// exactly those loads -- the entries scan alone would miss a subject
	// whose load is in flight but whose entry does not exist yet.
	for key, slot := range c.slots {
		if key.tenant == tenant {
			slot.gen++
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

// inflight reports how many subjects currently have a database load
// registered by beginLoad and not yet released by putIfCurrent or
// abortLoad. Only the fence's own tests need it, to prove a slot never
// outlives the load that created it (the fence's memory bound).
func (c *grantCache) inflight() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.slots)
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
