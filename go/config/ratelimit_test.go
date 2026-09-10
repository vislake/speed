package config

import (
	"context"
	"errors"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// Tests for ratelimit.go at the Service seam: the per-address check's
// fail-closed behavior when there is no limiter backend at all, and what
// the budget does with a request that carried no address. The wire half of
// the same contract -- the 429 a spent budget produces, its params, the one
// key shared by both endpoints and the caching headers on every answer --
// is pinned end to end over HTTP in http_test.go; the fallback
// clientIP takes for an address-less request is pinned there too, next to
// the helper it belongs to.

func TestCheckPreAuthIPLimit_FailsClosedWithoutALimiterBackend(t *testing.T) {
	// Only a hand-built, zero-value *pkgcore.Registry carries no KVStore
	// (pkgcore.NewRegistry requires one), so this is the Service a host's
	// wiring bug produces. The check must refuse rather than pass: it is
	// these endpoints' only throttle, so a missing backend that read as
	// "allow" would leave exactly the request volume it bounds ungoverned.
	svc := &Service{}
	err := svc.checkPreAuthIPLimit(context.Background(), "203.0.113.7")
	assertCode(t, err, ErrStorage)
	if !errors.Is(err, errNoKVStore) {
		t.Fatalf("checkPreAuthIPLimit with no KVStore carries cause %v, want the missing-store wiring error", err)
	}
}

func TestCheckPreAuthIPLimit_CountsAnEmptyAddressLikeAnyOther(t *testing.T) {
	// An address-less request -- RemoteAddr carrying nothing clientIP can
	// split -- is not exempt from the budget: every such caller shares one
	// counter, which is the documented consequence of supplying no better
	// identifier rather than a path that skips the check.
	svc := &Service{kv: pkgcore.NewMemoryKVStore()}
	ctx := context.Background()
	for i := 0; i < preAuthPerIPRate; i++ {
		if err := svc.checkPreAuthIPLimit(ctx, ""); err != nil {
			t.Fatalf("check %d/%d for an empty address = %v, want nil inside the budget", i+1, preAuthPerIPRate, err)
		}
	}

	err := svc.checkPreAuthIPLimit(ctx, "")
	assertCode(t, err, ErrRateLimited)
	assertParam(t, err, "dimension", "ip")
}
