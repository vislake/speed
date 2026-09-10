package staticaddr_test

// Runnable documentation for the staticaddr package. The example is
// compiled and executed by `go test`, so an API change that invalidates the
// documented usage fails the build instead of silently rotting.

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/notification"
	"github.com/vislake/speed/go/notification/staticaddr"
)

// ExampleNew builds the minimal host wiring for
// notification.WithUserAddressResolver: a resolver over the operator's own
// verified-address table, then one resolution per user the table names and
// one for a user it does not -- the ordinary "no address on file" answer
// that makes the delivery job skip a channel rather than fail.
func ExampleNew() {
	resolver := staticaddr.New(map[string]notification.UserAddresses{
		"user-7": {Email: "demo@example.com", Phone: "+8613800138000"},
	})

	addresses, err := resolver.Resolve(context.Background(), "user-7")
	if err != nil {
		fmt.Println("resolve:", err)
		return
	}
	fmt.Println("user-7:", addresses.Email, addresses.Phone)

	// A user the table does not name resolves to no addresses and no error.
	missing, err := resolver.Resolve(context.Background(), "user-8")
	if err != nil {
		fmt.Println("resolve missing:", err)
		return
	}
	fmt.Println("user-8 has addresses:", missing != notification.UserAddresses{})

	// Output:
	// user-7: demo@example.com +8613800138000
	// user-8 has addresses: false
}
