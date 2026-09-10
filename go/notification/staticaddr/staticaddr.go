// Package staticaddr implements notification.UserAddressResolver over a
// fixed per-user address table.
//
// The table is the operator's own declaration of the addresses it has
// verified for those users. UserAddressResolver's contract requires every
// returned address to be the host's VERIFIED address for that user, and the
// notification module -- which never imports an identity store -- performs
// no consent or verification check on the user path, so the resolver is the
// entire "may we send to this address" gate. This package makes that
// obligation structural: the map handed to New is, by construction, the
// operator saying "these are verified addresses of mine".
//
// Because the declaration is static, this resolver serves exactly the
// deployment shapes where the addresses are statically held -- demos, a
// single-tenant install whose operator holds the accounts, tests. A host
// whose addresses change (users rebinding an email or phone, a verification
// flow updating them) must implement UserAddressResolver over its own
// address store instead: a fixed table cannot see such a change and would
// keep answering with a stale address, silently.
package staticaddr

import (
	"context"

	"github.com/vislake/speed/go/notification"
)

// New returns a notification.UserAddressResolver answering from a fixed
// per-user table, the shape of a host layer over its own identity store
// which the notification module deliberately never imports.
//
// The table is copied at construction: a later mutation of the caller's
// map never changes what the resolver answers, and the resolver is safe
// for concurrent use -- deliveries read it at send time, from whatever
// job worker goroutine runs the delivery. A user id the table does not
// name resolves to an empty UserAddresses and a nil error, the ordinary
// "no address on file" state the delivery job settles as a per-channel
// skip (see UserAddressResolver.Resolve's contract), never as an error.
func New(addresses map[string]notification.UserAddresses) notification.UserAddressResolver {
	copied := make(map[string]notification.UserAddresses, len(addresses))
	for userID, addrs := range addresses {
		copied[userID] = addrs
	}
	return staticResolver(copied)
}

// staticResolver is the resolver New returns: the construction-time copy
// of the operator's table, read-only thereafter.
type staticResolver map[string]notification.UserAddresses

// Resolve implements notification.UserAddressResolver. The map read is
// safe without synchronization because nothing ever writes to the copy
// after New built it.
func (r staticResolver) Resolve(_ context.Context, userID string) (notification.UserAddresses, error) {
	return r[userID], nil
}

// compile-time check that staticResolver satisfies the seam.
var _ notification.UserAddressResolver = staticResolver(nil)
