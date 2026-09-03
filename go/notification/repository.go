package notification

import (
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// Repository is the inbox's data path: the only sanctioned way to read and
// write in_app_messages rows.
//
// It is a named type embedding dbkit.Repository[InboxMessage] rather than
// the generic base itself, so the module's consumers and its documentation
// can name the concrete thing they hold -- the same reason org's repository
// and the reference app's notes repository declare their own named types.
// Everything Repository can do is promoted unchanged from the embedded
// base: Create, FindByID, Update, Delete and List, each carrying dbkit's
// tenant-isolation guarantees (the tenant comes from the context, never
// from the caller; a row of another tenant is indistinguishable from a row
// that does not exist, and both report dbkit's record-not-found code).
//
// The module's own future consumers -- the delivery subscriber and the HTTP
// handler of this round's later blocks -- construct the repository the same
// way this package's tests do, over the one *gorm.DB the host opened and
// migrated.
type Repository struct {
	*dbkit.Repository[InboxMessage]
}

// NewRepository returns a Repository over db. db must already carry
// dbkit's tenant-isolation plugin -- the *gorm.DB dbkit.Open returns, which
// every host uses -- and the in_app_messages migration must already have
// been applied; the repository performs no I/O of its own at construction.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{Repository: dbkit.NewRepository[InboxMessage](db)}
}
