package demoseed_test

// Runnable documentation for the demoseed public API. Every example here is
// compiled and executed by `go test`, so an API change that invalidates the
// documented usage fails the build instead of silently rotting.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/authn/demoseed"
)

// Example seeds one demo account through a composed handler, the way a
// host's boot does: declare the demo domain once, then register each demo
// account and read back the user id authn assigned.
//
// This is demonstration wiring. Production code must not import this
// package: the handler below stands in for a host's composed stack, whose
// real register route would carry the password policy, the rate limiter and
// the users table a demo is supposed to exercise.
func Example() {
	// In a real host this handler is the composed server -- the same one
	// browsers reach -- so the register POST below traverses the real
	// register route with its middleware and validation intact.
	mux := http.NewServeMux()
	mux.HandleFunc(http.MethodPost+" /api/v1/authn/register", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"user-7"}`))
	})

	// The lookup answers the pre-registration existence question; in a host
	// this is authn.Service's SearchUsers, passed as a method value
	// (authnService.SearchUsers).
	lookup := func(_ context.Context, _ authn.UserSearchQuery) ([]authn.User, error) {
		return nil, nil
	}

	seeder, err := demoseed.NewSeeder(mux, lookup, "example.com")
	if err != nil {
		fmt.Println("seeder:", err)
		return
	}

	userID, alreadyExists, err := seeder.Register(context.Background(), "demo-owner@example.com", "demo password")
	if err != nil {
		fmt.Println("register:", err)
		return
	}
	fmt.Printf("registered %s (already there: %t)\n", userID, alreadyExists)

	// An account outside the declared demo domain is refused before
	// anything is sent -- the property that keeps the package out of
	// production provisioning paths.
	_, _, err = seeder.Register(context.Background(), "ops@acme.com", "demo password")
	fmt.Println("outside the demo domain:", err)

	// Output:
	// registered user-7 (already there: false)
	// outside the demo domain: demoseed: "ops@acme.com" is outside the declared demo domain "example.com"; this package seeds demo accounts only
}
