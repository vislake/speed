package rbac

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

// grants is a one-permission grant set, the smallest thing worth caching.
func grantsOf(perm string) map[string]permissionGrant {
	return map[string]permissionGrant{perm: {tenantWide: true}}
}

func TestGrantCache_PutThenGet_IsAHit(t *testing.T) {
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	key := grantKey{tenant: "tenant-a", user: "user-1"}
	now := time.Now()
	c.put(key, grantsOf("notes:read"), now)

	got, ok := c.get(key, now)
	if !ok {
		t.Fatal("get after put reported a miss, want a hit")
	}
	if _, granted := got["notes:read"]; !granted {
		t.Fatalf("cached grants = %v, want notes:read", got)
	}
}

func TestGrantCache_StoresNodeIDsNotResolvedPaths(t *testing.T) {
	// What a node-scoped grant carries through the cache is the node ID.
	// Caching the resolved materialized path instead would go stale the
	// moment the node moved in the organization tree, which is exactly the
	// staleness docs/internal/16-verification.md forbids -- and it would
	// go stale one layer further in than the binding row does, where
	// nothing would report it.
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	key := grantKey{tenant: "tenant-a", user: "user-1"}
	now := time.Now()
	c.put(key, map[string]permissionGrant{
		"notes:read": {nodeIDs: []string{"node-7", "node-9"}},
	}, now)

	got, ok := c.get(key, now)
	if !ok {
		t.Fatal("get after put reported a miss")
	}
	grant := got["notes:read"]
	if grant.tenantWide {
		t.Fatal("a node-scoped grant came back tenant-wide")
	}
	if !reflect.DeepEqual(grant.nodeIDs, []string{"node-7", "node-9"}) {
		t.Fatalf("cached node ids = %v, want [node-7 node-9]", grant.nodeIDs)
	}
}

func TestGrantCache_Get_UnknownSubject_IsAMiss(t *testing.T) {
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	if _, ok := c.get(grantKey{tenant: "tenant-a", user: "nobody"}, time.Now()); ok {
		t.Fatal("get on an empty cache reported a hit")
	}
}

func TestGrantCache_Get_DifferentTenantSameUser_IsAMiss(t *testing.T) {
	// The same person in two tenants is the ordinary case
	// docs/internal/05-identity-and-access.md calls out: an administrator
	// in one tenant and an ordinary member in another. A cache keyed on
	// the user alone would serve tenant A's grants in tenant B, which is a
	// cross-tenant authorization leak rather than a cache bug.
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	now := time.Now()
	c.put(grantKey{tenant: "tenant-a", user: "user-1"}, grantsOf("notes:write"), now)

	if _, ok := c.get(grantKey{tenant: "tenant-b", user: "user-1"}, now); ok {
		t.Fatal("tenant-b read tenant-a's cached grants for the same user id")
	}
}

func TestGrantCache_Get_AfterTTL_IsAMiss(t *testing.T) {
	// The anti-loss net: an entry older than the TTL is unusable even
	// though no invalidation event ever arrived. Driven by an explicit
	// clock rather than a sleep, so the boundary is exact.
	const ttl = 30 * time.Second
	c := newGrantCache(ttl)
	t.Cleanup(c.close)

	key := grantKey{tenant: "tenant-a", user: "user-1"}
	loadedAt := time.Now()
	c.put(key, grantsOf("notes:read"), loadedAt)

	if _, ok := c.get(key, loadedAt.Add(ttl-time.Nanosecond)); !ok {
		t.Fatal("an entry one nanosecond short of the TTL was a miss, want a hit")
	}
	if _, ok := c.get(key, loadedAt.Add(ttl)); ok {
		t.Fatal("an entry exactly at the TTL was a hit, want a miss (the boundary is inclusive)")
	}
	if _, ok := c.get(key, loadedAt.Add(2*ttl)); ok {
		t.Fatal("a long-expired entry was a hit")
	}
}

func TestGrantCache_ZeroTTL_NeverHits_AndStartsNoJanitor(t *testing.T) {
	// The disabled setting: every decision reads through. It must also
	// start no goroutine, which close() proves by returning rather than
	// blocking on a done channel that nobody would ever close.
	c := newGrantCache(0)

	key := grantKey{tenant: "tenant-a", user: "user-1"}
	now := time.Now()
	c.put(key, grantsOf("notes:read"), now)
	if _, ok := c.get(key, now); ok {
		t.Fatal("a zero-TTL cache reported a hit")
	}
	if c.len() != 0 {
		t.Fatalf("a zero-TTL cache stored %d entries, want none", c.len())
	}
	c.close()
}

func TestGrantCache_Invalidate_DropsOnlyThatSubject(t *testing.T) {
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	now := time.Now()
	victim := grantKey{tenant: "tenant-a", user: "user-1"}
	bystander := grantKey{tenant: "tenant-a", user: "user-2"}
	c.put(victim, grantsOf("notes:read"), now)
	c.put(bystander, grantsOf("notes:read"), now)

	c.invalidate(victim)

	if _, ok := c.get(victim, now); ok {
		t.Fatal("the invalidated subject was still a hit")
	}
	if _, ok := c.get(bystander, now); !ok {
		t.Fatal("invalidating one subject dropped another subject's entry")
	}
}

func TestGrantCache_Invalidate_IsIdempotent(t *testing.T) {
	// Both the writing call and its own event subscriber invalidate, so
	// double invalidation is the normal path, not an edge case.
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	key := grantKey{tenant: "tenant-a", user: "user-1"}
	c.put(key, grantsOf("notes:read"), time.Now())
	c.invalidate(key)
	c.invalidate(key)

	if _, ok := c.get(key, time.Now()); ok {
		t.Fatal("the entry survived two invalidations")
	}
}

func TestGrantCache_InvalidateTenant_DropsThatTenantOnly(t *testing.T) {
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	now := time.Now()
	a1 := grantKey{tenant: "tenant-a", user: "user-1"}
	a2 := grantKey{tenant: "tenant-a", user: "user-2"}
	b1 := grantKey{tenant: "tenant-b", user: "user-1"}
	for _, key := range []grantKey{a1, a2, b1} {
		c.put(key, grantsOf("notes:read"), now)
	}

	c.invalidateTenant("tenant-a")

	for _, key := range []grantKey{a1, a2} {
		if _, ok := c.get(key, now); ok {
			t.Fatalf("%v survived its tenant's invalidation", key)
		}
	}
	if _, ok := c.get(b1, now); !ok {
		t.Fatal("invalidating tenant-a dropped tenant-b's entry")
	}
}

func TestGrantCache_Sweep_ReclaimsOnlyExpiredEntries(t *testing.T) {
	const ttl = time.Minute
	c := newGrantCache(ttl)
	t.Cleanup(c.close)

	base := time.Now()
	old := grantKey{tenant: "tenant-a", user: "old"}
	fresh := grantKey{tenant: "tenant-a", user: "fresh"}
	c.put(old, grantsOf("notes:read"), base)
	c.put(fresh, grantsOf("notes:read"), base.Add(ttl))

	c.sweep(base.Add(ttl))

	if c.len() != 1 {
		t.Fatalf("after the sweep the cache holds %d entries, want 1", c.len())
	}
	if _, ok := c.get(fresh, base.Add(ttl)); !ok {
		t.Fatal("the sweep reclaimed a live entry")
	}
}

func TestGrantCache_Close_StopsTheJanitor_AndIsIdempotent(t *testing.T) {
	// A short TTL so the janitor is genuinely running and ticking while
	// close() races it; close must still return rather than deadlock, and
	// a second close must not panic on an already-closed channel.
	c := newGrantCache(time.Millisecond)
	c.put(grantKey{tenant: "tenant-a", user: "user-1"}, grantsOf("notes:read"), time.Now())

	c.close()
	c.close()

	// Still correct after close: expiry is enforced by get, not by the
	// janitor, so a stale entry cannot be served just because nothing is
	// reclaiming it any more.
	if _, ok := c.get(grantKey{tenant: "tenant-a", user: "user-1"}, time.Now().Add(time.Hour)); ok {
		t.Fatal("an expired entry became a hit after close")
	}
}

func TestGrantCache_ConcurrentUse_IsRaceFree(t *testing.T) {
	// The decision cache is the module's one concurrency hot spot (backend
	// coding standard §13: caches require -race tests). Readers, writers
	// and both invalidation paths run together against the same entries.
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	const goroutines = 8
	const iterations = 200

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			key := grantKey{tenant: "tenant-a", user: "user-1"}
			for n := 0; n < iterations; n++ {
				switch worker % 4 {
				case 0:
					c.put(key, grantsOf("notes:read"), time.Now())
				case 1:
					if grants, ok := c.get(key, time.Now()); ok {
						// Read the shared map the way the evaluator does.
						// A writer that mutated a stored map instead of
						// replacing it would be caught here.
						_ = grants["notes:read"]
					}
				case 2:
					c.invalidate(key)
				default:
					c.invalidateTenant("tenant-a")
				}
			}
		}(i)
	}
	wg.Wait()
}
