package ratelimit_test

// Runnable documentation for ratelimit's public API, mirroring
// go/pkgcore/example_test.go's and go/dbkit/example_test.go's own
// convention: every example here is compiled and executed by `go test`, so
// a change to ratelimit's public API that breaks the documented usage fails
// the build instead of only rotting in prose.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/ratelimit"
)

// Example shows the core usage pattern: a Limiter backed by
// pkgcore.NewMemoryKVStore() -- the standalone deployment mode's KVStore,
// and the same store unit tests use -- hit up to its configured Rate, then
// denied once that Rate is exceeded within Per. Per is long relative to how
// fast this example runs, so all three hits land in the same window and
// Remaining decreases by exactly one hit at a time.
func Example() {
	ctx := context.Background()
	limiter := ratelimit.New(pkgcore.NewMemoryKVStore())
	limit := ratelimit.Limit{Rate: 2, Per: time.Minute}

	for i := 1; i <= 3; i++ {
		decision, err := limiter.Allow(ctx, "login:user-42", limit)
		if err != nil {
			fmt.Println("allow:", err)
			return
		}
		fmt.Printf("hit %d: allowed=%t remaining=%d\n", i, decision.Allowed, decision.Remaining)
	}

	// Output:
	// hit 1: allowed=true remaining=1
	// hit 2: allowed=true remaining=0
	// hit 3: allowed=false remaining=0
}

// ExampleLimiter_multipleDimensions shows how a caller composes several
// independent dimensions on top of Allow, the way authn's own brute-force
// guard combines IP with an endpoint-specific second dimension (account,
// email, or phone number): one Allow call per dimension, any single denial
// denying the whole request. Limiter deliberately has no built-in notion of
// combining dimensions -- see Limiter's own doc comment -- so this
// composition lives entirely in the caller.
//
// The account dimension is keyed by the account's blind index, never by the
// account's plaintext email or phone number. The key string a caller passes
// lands verbatim in the underlying KVStore -- a Redis key in the distributed
// deployment mode -- so a plaintext identifier in one is PII stored outside
// the database, unencrypted, for as long as the window's keys live. authn
// guards its login endpoint exactly this way: the service computes the
// identifier's blind index (a keyed HMAC-SHA256 over the normalized value,
// via dbkit.NewBlindIndexer) and passes only that to its rate guard (see
// go/authn/service.go's login and go/authn/ratelimit.go's CheckLogin).
// ratelimit itself imports nothing PII- or tenancy-shaped, so this example
// stands in a plain hex SHA-256 digest of the demo email as the
// blind-index-shaped value -- what matters for the limiter is that the value
// in the key is opaque and never the identifier itself; the keyed derivation
// is the caller's own.
func ExampleLimiter_multipleDimensions() {
	ctx := context.Background()
	limiter := ratelimit.New(pkgcore.NewMemoryKVStore())

	// The account dimension's value: an opaque digest derived from the
	// identifier (here, the demo email), never the identifier itself.
	account := fmt.Sprintf("%x", sha256.Sum256([]byte("ada@example.com")))

	checkLogin := func(account, ip string) (bool, error) {
		byAccount, err := limiter.Allow(ctx, "login:account:"+account, ratelimit.Limit{Rate: 5, Per: time.Minute})
		if err != nil {
			return false, err
		}
		byIP, err := limiter.Allow(ctx, "login:ip:"+ip, ratelimit.Limit{Rate: 20, Per: time.Minute})
		if err != nil {
			return false, err
		}
		return byAccount.Allowed && byIP.Allowed, nil
	}

	// The dimension key this example builds carries the digest, never the
	// email: "ada@example.com" appears nowhere in it. (Allow appends the
	// current window's index to this base key -- windowKey -- before
	// touching the store.)
	fmt.Println("account key:", "login:account:"+account)
	allowed, err := checkLogin(account, "203.0.113.7")
	fmt.Println(allowed, err)

	// Output:
	// account key: login:account:b5fc85e55755f9e0d030a10ab4429b6b2944855f9a0d60077fe832becbc41d72
	// true <nil>
}

// ExampleErrInvalidLimit shows Allow's validation of Limit: a Rate or Per
// that is zero or negative fails loudly with an error wrapping
// ErrInvalidLimit, rather than being silently treated as always-allow or
// always-deny -- a security-relevant primitive must not guess.
func ExampleErrInvalidLimit() {
	ctx := context.Background()
	limiter := ratelimit.New(pkgcore.NewMemoryKVStore())

	_, err := limiter.Allow(ctx, "any-key", ratelimit.Limit{Rate: 0, Per: time.Minute})
	fmt.Println(errors.Is(err, ratelimit.ErrInvalidLimit))

	// A Rate of 1 is refused too, with its own coded error that still wraps
	// the same sentinel: whatever Per it is paired with, "once per Per"
	// cannot be honoured by this limiter (see ErrRateOneUnsupported), so it
	// fails loudly instead of silently delivering "allowed once ever, then
	// denied forever".
	_, err = limiter.Allow(ctx, "any-key", ratelimit.Limit{Rate: 1, Per: time.Minute})
	fmt.Println(errors.Is(err, ratelimit.ErrInvalidLimit), errors.Is(err, ratelimit.ErrRateOneUnsupported))

	// Output:
	// true
	// true true
}

// ExampleRetryAfterSeconds shows the whole-second conversion every consumer
// of a denied Decision performs: a sub-second window tail -- the ordinary
// end of an exhausted window -- reports 1, never the 0 a truncating
// conversion would emit (Retry-After: 0 means "retry immediately"), and a
// remainder that has already elapsed reports 0, never a negative count.
func ExampleRetryAfterSeconds() {
	for _, remaining := range []time.Duration{
		900 * time.Millisecond,
		-3 * time.Second,
	} {
		fmt.Println(ratelimit.RetryAfterSeconds(remaining))
	}

	// Output:
	// 1
	// 0
}
