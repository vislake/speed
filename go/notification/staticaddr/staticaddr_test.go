package staticaddr_test

import (
	"context"
	"sync"
	"testing"

	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/notification/staticaddr"
)

// TestNew_ResolvesTheTableRows pins the resolver's whole answering contract
// against UserAddressResolver's own: a user id the table names resolves to
// exactly the addresses the table holds, and one it does not name resolves
// to an empty UserAddresses with a nil error -- absence of addresses is not
// an error, it is the ordinary state the delivery job settles as a
// per-channel skip.
func TestNew_ResolvesTheTableRows(t *testing.T) {
	resolver := staticaddr.New(map[string]notification.UserAddresses{
		"user-7": {Email: "demo@example.com", Phone: "+8613800138000"},
		"user-8": {Phone: "+8613800138001"},
	})
	ctx := context.Background()

	got, err := resolver.Resolve(ctx, "user-7")
	if err != nil {
		t.Fatalf("Resolve(user-7): %v", err)
	}
	want := notification.UserAddresses{Email: "demo@example.com", Phone: "+8613800138000"}
	if got != want {
		t.Errorf("Resolve(user-7) = %+v, want %+v", got, want)
	}

	got, err = resolver.Resolve(ctx, "user-8")
	if err != nil {
		t.Fatalf("Resolve(user-8): %v", err)
	}
	if got != (notification.UserAddresses{Phone: "+8613800138001"}) {
		t.Errorf("Resolve(user-8) = %+v, want the phone-only entry", got)
	}

	got, err = resolver.Resolve(ctx, "user-does-not-exist")
	if err != nil {
		t.Fatalf("Resolve(unknown): %v", err)
	}
	if got != (notification.UserAddresses{}) {
		t.Errorf("Resolve(unknown) = %+v, want empty UserAddresses", got)
	}
}

// TestNew_CopiesTheTableAtConstruction pins the copy semantics New's doc
// comment promises: mutating the caller's map after New -- changing an
// entry, deleting one, adding one -- never changes what the resolver
// answers, so a caller that keeps using its own map cannot race a
// delivery's read of the resolver.
func TestNew_CopiesTheTableAtConstruction(t *testing.T) {
	table := map[string]notification.UserAddresses{
		"user-7": {Email: "demo@example.com"},
		"user-8": {Email: "other@example.com"},
	}
	resolver := staticaddr.New(table)
	ctx := context.Background()

	// Mutate the caller's map in every direction after construction.
	table["user-7"] = notification.UserAddresses{Email: "changed@example.com"}
	delete(table, "user-8")
	table["user-9"] = notification.UserAddresses{Email: "late@example.com"}

	got, err := resolver.Resolve(ctx, "user-7")
	if err != nil {
		t.Fatalf("Resolve(user-7): %v", err)
	}
	if got.Email != "demo@example.com" {
		t.Errorf("Resolve(user-7).Email = %q, want the construction-time %q", got.Email, "demo@example.com")
	}

	got, err = resolver.Resolve(ctx, "user-8")
	if err != nil {
		t.Fatalf("Resolve(user-8): %v", err)
	}
	if got.Email != "other@example.com" {
		t.Errorf("Resolve(user-8).Email = %q, want the construction-time %q (a deleted table row must keep resolving)", got.Email, "other@example.com")
	}

	got, err = resolver.Resolve(ctx, "user-9")
	if err != nil {
		t.Fatalf("Resolve(user-9): %v", err)
	}
	if got != (notification.UserAddresses{}) {
		t.Errorf("Resolve(user-9) = %+v, want empty (a row added after construction must not be visible)", got)
	}
}

// TestNew_NilTableAnswersNoAddresses pins the degenerate input: New(nil)
// builds a resolver that answers every user with the ordinary empty,
// no-error result rather than failing construction or panicking on a read.
func TestNew_NilTableAnswersNoAddresses(t *testing.T) {
	resolver := staticaddr.New(nil)
	got, err := resolver.Resolve(context.Background(), "user-7")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != (notification.UserAddresses{}) {
		t.Errorf("Resolve = %+v, want empty UserAddresses", got)
	}
}

// TestNew_ConcurrentReads pins the concurrency half of New's contract: the
// resolver is read concurrently -- deliveries resolve at send time from
// whatever worker goroutine runs the job -- and answers every reader
// consistently. Run under -race, the concurrent goroutines below are the
// standing proof there is no data race against the construction-time copy.
func TestNew_ConcurrentReads(t *testing.T) {
	const (
		readers           = 32
		resolvesPerReader = 200
	)
	resolver := staticaddr.New(map[string]notification.UserAddresses{
		"user-7": {Email: "demo@example.com", Phone: "+8613800138000"},
	})
	ctx := context.Background()
	want := notification.UserAddresses{Email: "demo@example.com", Phone: "+8613800138000"}

	var wg sync.WaitGroup
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < resolvesPerReader; i++ {
				got, err := resolver.Resolve(ctx, "user-7")
				if err != nil || got != want {
					t.Errorf("concurrent Resolve = (%+v, %v), want (%+v, nil)", got, err, want)
					return
				}
				if missing, err := resolver.Resolve(ctx, "user-missing"); err != nil || missing != (notification.UserAddresses{}) {
					t.Errorf("concurrent Resolve(missing) = (%+v, %v), want (empty, nil)", missing, err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
