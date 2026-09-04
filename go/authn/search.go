package authn

import (
	"context"
)

// defaultSearchLimit and maxSearchLimit bound a SearchUsers call: the
// default page size when UserSearchQuery.Limit is zero, and the hard
// ceiling a caller cannot exceed no matter what it asks for. A platform
// operator's search is meant to find one specific person, not to page
// through the whole users table.
const (
	defaultSearchLimit = 20
	maxSearchLimit     = 200
)

// UserSearchQuery is SearchUsers' input: exactly one of Email, Phone or
// DisplayNamePrefix must be set, in that precedence order when more than
// one is given (Email first, then Phone, then DisplayNamePrefix) -- this is
// a single-criterion lookup, not a composed filter, because the three
// criteria answer different questions ("this exact person" for the first
// two, "people whose name starts with..." for the third) and combining them
// with an implicit AND or OR would make the result depend on an
// unstated choice.
type UserSearchQuery struct {
	// Email, when set, matches exactly one account by its email blind
	// index -- the same canonical-form exact match UserRepository.FindByEmail
	// performs.
	Email string

	// Phone, when set, matches exactly one account by its phone blind
	// index -- the same canonical-form (E.164) exact match
	// UserRepository.FindByPhone performs.
	Phone string

	// DisplayNamePrefix, when set, matches every account whose DisplayName
	// starts with it, case-insensitively. This is the one criterion that
	// can return more than one row, which is why Limit exists.
	DisplayNamePrefix string

	// Limit bounds how many rows DisplayNamePrefix's prefix search returns.
	// Zero uses defaultSearchLimit; anything above maxSearchLimit is
	// clamped to it. Ignored by the Email and Phone paths, which can never
	// return more than one row.
	Limit int
}

// SearchUsers is authn's platform-operator search entry point: unlike
// FindByID/FindByEmail/FindByPhone, which answer "does this one identifier
// resolve to an account" for ordinary business code that already knows
// which tenant it is asking about, SearchUsers answers "which account (if
// any) does this identifier or name fragment belong to" with no tenant in
// scope at all -- because users is identity data, not tenant data (a
// person can belong to several tenants), only a platform-wide search makes
// sense here in the first place.
//
// It is a pure, additive method: no existing signature in this module
// changes. authn.Service has no opinion on who may call it -- that is an
// authorization decision, and this module never imports rbac (root
// CLAUDE.md's module-boundary rule). The caller (go/admin's HTTP handler,
// in this round) is responsible for gating the operation on a platform
// permission such as admin:search_users before it ever reaches here.
//
// It returns ErrSearchCriteriaRequired when q names none of its three
// fields, rather than silently answering with every user in the platform.
func (s *Service) SearchUsers(ctx context.Context, q UserSearchQuery) ([]User, error) {
	return s.users.Search(ctx, q)
}
