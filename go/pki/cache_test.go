package pki

import (
	"testing"
	"time"
)

// --- get/put/invalidate ------------------------------------------------------

func TestKeySetCache_GetMissesOnAnUnknownPurpose(t *testing.T) {
	c := newKeySetCache(time.Minute)
	t.Cleanup(c.close)

	if _, ok := c.get("authn.access_token", time.Now()); ok {
		t.Errorf("get(never put) = ok, want a miss")
	}
}

func TestKeySetCache_PutThenGetHits(t *testing.T) {
	c := newKeySetCache(time.Minute)
	t.Cleanup(c.close)

	now := time.Now()
	active := &SigningKey{ID: "kid-1"}
	verifiable := []SigningKey{{ID: "kid-1"}}
	c.put("authn.access_token", active, verifiable, now)

	entry, ok := c.get("authn.access_token", now)
	if !ok {
		t.Fatalf("get after put = miss, want a hit")
	}
	if entry.active != active {
		t.Errorf("entry.active = %v, want the exact pointer put in", entry.active)
	}
	if len(entry.verifiable) != 1 || entry.verifiable[0].ID != "kid-1" {
		t.Errorf("entry.verifiable = %v, want the slice put in", entry.verifiable)
	}
}

// TestKeySetCache_GetExpiresAfterTTL proves an entry older than ttl is a
// miss regardless of whether it was ever invalidated -- the anti-loss net
// behind event invalidation this file's own doc comment describes.
func TestKeySetCache_GetExpiresAfterTTL(t *testing.T) {
	c := newKeySetCache(time.Minute)
	t.Cleanup(c.close)

	now := time.Now()
	c.put("authn.access_token", nil, nil, now)

	if _, ok := c.get("authn.access_token", now.Add(59*time.Second)); !ok {
		t.Errorf("get just under the TTL = miss, want a hit")
	}
	if _, ok := c.get("authn.access_token", now.Add(60*time.Second)); ok {
		t.Errorf("get at/past the TTL = hit, want a miss")
	}
}

func TestKeySetCache_Invalidate_DropsTheEntry(t *testing.T) {
	c := newKeySetCache(time.Minute)
	t.Cleanup(c.close)

	now := time.Now()
	c.put("authn.access_token", nil, nil, now)
	c.invalidate("authn.access_token")

	if _, ok := c.get("authn.access_token", now); ok {
		t.Errorf("get after invalidate = hit, want a miss")
	}
}

// TestKeySetCache_Invalidate_OnlyAffectsItsOwnPurpose proves invalidating
// one purpose leaves every other purpose's entry untouched -- a rotation on
// one purpose must not force every other purpose to re-read the database.
func TestKeySetCache_Invalidate_OnlyAffectsItsOwnPurpose(t *testing.T) {
	c := newKeySetCache(time.Minute)
	t.Cleanup(c.close)

	now := time.Now()
	c.put("authn.access_token", nil, nil, now)
	c.put("tenant.jwt_signing", nil, nil, now)
	c.invalidate("authn.access_token")

	if _, ok := c.get("authn.access_token", now); ok {
		t.Errorf("get(authn.access_token) after its own invalidate = hit, want a miss")
	}
	if _, ok := c.get("tenant.jwt_signing", now); !ok {
		t.Errorf("get(tenant.jwt_signing) after a DIFFERENT purpose's invalidate = miss, want a hit")
	}
}

// --- ttl<=0 disables the cache ------------------------------------------------

// TestKeySetCache_TTLDisabledNeverServesAHit proves ttl<=0 -- the setting a
// test wanting to observe every write immediately uses (Service's own doc
// comment on NewService) -- makes every get a miss even right after a put.
func TestKeySetCache_TTLDisabledNeverServesAHit(t *testing.T) {
	c := newKeySetCache(0)
	t.Cleanup(c.close)

	now := time.Now()
	c.put("authn.access_token", &SigningKey{ID: "kid-1"}, nil, now)

	if _, ok := c.get("authn.access_token", now); ok {
		t.Errorf("get on a ttl<=0 cache = hit, want a permanent miss")
	}
	if got := c.len(); got != 0 {
		t.Errorf("a ttl<=0 cache stored %d entries, want put() to be a no-op", got)
	}
}

// TestKeySetCache_TTLDisabledStartsNoJanitor proves close() on a disabled
// cache is safe (its own doc comment says "unless disabled"); a nil stop
// channel must not be closed or waited on.
func TestKeySetCache_TTLDisabledStartsNoJanitor(t *testing.T) {
	c := newKeySetCache(0)
	c.close() // must not panic or block
	c.close() // idempotent, even on a cache that never started a janitor
}

// --- janitor sweep ------------------------------------------------------------

// TestKeySetCache_Sweep_RemovesOnlyExpiredEntries proves sweep is a pure
// memory-bound cleanup: an unexpired entry survives a sweep that removes an
// expired sibling.
func TestKeySetCache_Sweep_RemovesOnlyExpiredEntries(t *testing.T) {
	c := newKeySetCache(time.Minute)
	t.Cleanup(c.close)

	now := time.Now()
	c.put("expired", nil, nil, now)
	c.put("fresh", nil, nil, now.Add(80*time.Second))

	c.sweep(now.Add(90 * time.Second))

	if got := c.len(); got != 1 {
		t.Fatalf("len() after sweep = %d, want 1 (only the fresh entry survives)", got)
	}
	if _, ok := c.get("fresh", now.Add(90*time.Second)); !ok {
		t.Errorf("the fresh entry did not survive the sweep")
	}
}

// TestKeySetCache_Close_IsIdempotentAndStopsTheJanitor proves close() can
// be called more than once without panicking or blocking forever, and that
// it actually waits for the janitor goroutine to exit (done is closed).
func TestKeySetCache_Close_IsIdempotentAndStopsTheJanitor(t *testing.T) {
	c := newKeySetCache(10 * time.Millisecond)
	c.close()
	c.close()

	select {
	case <-c.done:
		// the janitor goroutine has exited, as close() promises.
	default:
		t.Errorf("c.done is not closed after close() returned")
	}
}
