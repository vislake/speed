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

func TestGrantCache_PutIfCurrent_SucceedsWhenFenceUnchanged(t *testing.T) {
	// The ordinary case: nothing invalidated this subject between beginLoad
	// and putIfCurrent, so the load's result is stored exactly like put
	// would.
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	key := grantKey{tenant: "tenant-a", user: "user-1"}
	gen := c.beginLoad(key) // captured "before the database read"
	now := time.Now()
	c.putIfCurrent(key, grantsOf("notes:read"), now, gen)

	got, ok := c.get(key, now)
	if !ok {
		t.Fatal("putIfCurrent with an unchanged fence reported a miss, want a hit")
	}
	if _, granted := got["notes:read"]; !granted {
		t.Fatalf("cached grants = %v, want notes:read", got)
	}
	// The stored load released its in-flight registration: a slot never
	// outlives the load that created it (the fence's memory bound).
	if c.inflight() != 0 {
		t.Fatalf("%d slots remain after a load stored, want 0", c.inflight())
	}
}

func TestGrantCache_PutIfCurrent_DiscardsAfterInterveningInvalidate(t *testing.T) {
	// The fenced race this method exists to close (see its doc comment and
	// the review finding on service.go's grantsFor): a load captures the
	// fence, then something invalidates the SAME subject before the load's
	// result is written back. The stale result must not resurrect what the
	// invalidation just dropped.
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	key := grantKey{tenant: "tenant-a", user: "user-1"}
	gen := c.beginLoad(key) // captured "before the database read"

	c.invalidate(key) // the concurrent revoke, landing mid-load

	now := time.Now()
	c.putIfCurrent(key, grantsOf("notes:read"), now, gen) // the stale load finally writes

	if _, ok := c.get(key, now); ok {
		t.Fatal("putIfCurrent stored a load whose fence was stale, resurrecting a revoked grant")
	}
	if c.inflight() != 0 {
		t.Fatalf("%d slots remain after a discarded load, want 0", c.inflight())
	}
}

func TestGrantCache_PutIfCurrent_DiscardsAfterInterveningInvalidateTenant(t *testing.T) {
	// The tenant-wide counterpart: a role's permissions narrowed and
	// invalidateTenant ran while a load for a subject in that tenant was in
	// flight. onRoleChanged's invalidation must not be "un-invalidated" by
	// that load's late write either -- the load read the role's permissions
	// before the change.
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	key := grantKey{tenant: "tenant-a", user: "user-1"}
	gen := c.beginLoad(key)

	c.invalidateTenant("tenant-a")

	now := time.Now()
	c.putIfCurrent(key, grantsOf("notes:read"), now, gen)

	if _, ok := c.get(key, now); ok {
		t.Fatal("putIfCurrent stored a load whose fence predated a tenant-wide invalidation")
	}
}

// TestGrantCache_PutIfCurrent_FenceIsPerSubject_OtherSubjectsInvalidateSparesThisLoad
// pins the review finding on the cache's fence: it used to be keyed on a
// process-global generation counter, so ANY invalidation -- any tenant,
// any subject -- discarded every unrelated subject's in-flight load. In
// the distributed mode every replica hears every platform-wide authz
// change, so under write load the cache would almost never fill and every
// check became a full database read. The fence is per-subject now: an
// invalidation of subject A must not discard subject B's load, while A's
// own invalidation must still fence A's own stale load.
func TestGrantCache_PutIfCurrent_FenceIsPerSubject_OtherSubjectsInvalidateSparesThisLoad(t *testing.T) {
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	keyA := grantKey{tenant: "tenant-a", user: "user-1"}
	keyB := grantKey{tenant: "tenant-a", user: "user-2"}

	genA := c.beginLoad(keyA) // A's database load starts
	genB := c.beginLoad(keyB) // B's database load starts
	c.invalidate(keyA)        // a revoke for A lands while both loads are in flight

	now := time.Now()
	c.putIfCurrent(keyB, grantsOf("notes:read"), now, genB) // B's fresh, post-revoke read writes back
	if _, ok := c.get(keyB, now); !ok {
		t.Fatal("B's in-flight load was discarded by an invalidation of A: nothing about B changed, so B's fresh read must land")
	}

	// And the fence must still hold for A itself: A's own pre-revoke read
	// is stale and must not resurrect the revoked grant.
	c.putIfCurrent(keyA, grantsOf("notes:read"), now, genA)
	if _, ok := c.get(keyA, now); ok {
		t.Fatal("A's stale load was cached despite A's own revoke landing mid-load")
	}
}

// TestGrantCache_PutIfCurrent_FenceIsPerTenant_OtherTenantsInvalidateSparesThisLoad
// is the tenant-wide mirror of the per-subject regression above: a role
// change in tenant B must not discard an in-flight load of a tenant-A
// subject, while a role change in B's own tenant must fence B's own
// in-flight load -- even one whose subject has no cached entry yet.
func TestGrantCache_PutIfCurrent_FenceIsPerTenant_OtherTenantsInvalidateSparesThisLoad(t *testing.T) {
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	keyA := grantKey{tenant: "tenant-a", user: "user-1"}
	keyB := grantKey{tenant: "tenant-b", user: "user-1"}

	genA := c.beginLoad(keyA)
	genB := c.beginLoad(keyB)      // B's load: no cached entry exists for it yet
	c.invalidateTenant("tenant-b") // a role change in B, landing mid-load

	now := time.Now()
	c.putIfCurrent(keyA, grantsOf("notes:read"), now, genA)
	if _, ok := c.get(keyA, now); !ok {
		t.Fatal("a tenant-b role change discarded tenant-a's in-flight load")
	}
	c.putIfCurrent(keyB, grantsOf("notes:read"), now, genB)
	if _, ok := c.get(keyB, now); ok {
		t.Fatal("a tenant-b role change failed to fence tenant-b's own in-flight load, even though it had no cached entry yet")
	}
}

// TestGrantCache_PutIfCurrent_TwoConcurrentLoadsOfOneSubject_BothFencedByOneInvalidate
// pins the slot bookkeeping: two loads of the SAME subject share one fence
// slot, so a single intervening invalidation fences both, and the slot is
// released only once both loads have landed.
func TestGrantCache_PutIfCurrent_TwoConcurrentLoadsOfOneSubject_BothFencedByOneInvalidate(t *testing.T) {
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	key := grantKey{tenant: "tenant-a", user: "user-1"}
	genFirst := c.beginLoad(key)
	genSecond := c.beginLoad(key)
	if genFirst != genSecond {
		t.Fatalf("two loads of one subject captured different fences (%d, %d), want the same", genFirst, genSecond)
	}
	c.invalidate(key) // one revoke, in flight while both loads run

	now := time.Now()
	c.putIfCurrent(key, grantsOf("notes:read"), now, genSecond)
	c.putIfCurrent(key, grantsOf("notes:read"), now, genFirst)
	if _, ok := c.get(key, now); ok {
		t.Fatal("a fenced load of the subject was cached despite the intervening revoke")
	}
	if c.inflight() != 0 {
		t.Fatalf("%d slots remain after both loads released, want 0", c.inflight())
	}

	// A fresh load after the dust settles captures a fresh fence and
	// stores normally.
	gen := c.beginLoad(key)
	c.putIfCurrent(key, grantsOf("notes:read"), now, gen)
	if _, ok := c.get(key, now); !ok {
		t.Fatal("a fresh post-revoke load did not store")
	}
}

// TestGrantCache_AbortLoad_ReleasesTheSlot pins the error half of the
// fence's memory bound: a load that fails between beginLoad and any store
// must release its registration, or its subject's slot would stay pinned
// forever.
func TestGrantCache_AbortLoad_ReleasesTheSlot(t *testing.T) {
	c := newGrantCache(time.Minute)
	t.Cleanup(c.close)

	key := grantKey{tenant: "tenant-a", user: "user-1"}
	c.beginLoad(key)
	if c.inflight() != 1 {
		t.Fatalf("%d slots during an in-flight load, want 1", c.inflight())
	}

	c.abortLoad(key) // the load failed; nothing will be stored
	if c.inflight() != 0 {
		t.Fatalf("%d slots remain after an aborted load, want 0 (the fence's memory bound)", c.inflight())
	}

	// A later load of the same subject is unaffected by the aborted one --
	// including a fenced one that aborts again.
	c.beginLoad(key)
	c.invalidate(key)
	c.abortLoad(key) // the fenced load failed too; the slot must still release
	if c.inflight() != 0 {
		t.Fatalf("%d slots remain after a fenced load aborted, want 0", c.inflight())
	}
	gen := c.beginLoad(key)
	c.putIfCurrent(key, grantsOf("notes:read"), time.Now(), gen)
	if _, ok := c.get(key, time.Now()); !ok {
		t.Fatal("a fresh load after the aborted ones did not store")
	}
}

func TestGrantCache_ConcurrentUse_IsRaceFree(t *testing.T) {
	// The decision cache is the module's one concurrency hot spot (backend
	// coding standard §13: caches require -race tests). Readers, writers,
	// both invalidation paths and both halves of the fenced write path run
	// together against the same entries and slots.
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
				switch worker % 6 {
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
				case 3:
					// The fenced write path grantsFor actually uses.
					gen := c.beginLoad(key)
					c.putIfCurrent(key, grantsOf("notes:read"), time.Now(), gen)
				case 4:
					// The load failed: nothing is stored, but the slot must
					// still release.
					c.beginLoad(key)
					c.abortLoad(key)
				default:
					c.invalidateTenant("tenant-a")
				}
			}
		}(i)
	}
	wg.Wait()
}
