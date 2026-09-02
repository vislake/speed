package ratelimit_test

// Runnable documentation for ratelimit's public API, mirroring
// go/pkgcore/example_test.go's and go/dbkit/example_test.go's own
// convention: every example here is compiled and executed by `go test`, so
// a change to ratelimit's public API that breaks the documented usage fails
// the build instead of only rotting in prose.

import (
	"context"
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
func ExampleLimiter_multipleDimensions() {
	ctx := context.Background()
	limiter := ratelimit.New(pkgcore.NewMemoryKVStore())

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

	allowed, err := checkLogin("ada@example.com", "203.0.113.7")
	fmt.Println(allowed, err)

	// Output:
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

	// Output:
	// true
}
