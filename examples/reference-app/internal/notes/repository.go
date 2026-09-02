package notes

import (
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// Repository is notes' tenant-scoped data-access type. It embeds
// dbkit.Repository[Note] instead of holding a *gorm.DB directly (root
// CLAUDE.md's multi-tenant isolation rule; backend coding standard §3.2) --
// Create, FindByID, Update, Delete and List are all promoted from the
// embedded base unchanged. It exists as notes' own named type, rather than
// every caller using *dbkit.Repository[Note] directly, so a query specific
// to notes that Repository[T]'s deliberately minimal surface cannot express
// (see dbkit/AGENTS.md's "Known limitations") has somewhere to live later
// without changing any caller's import.
type Repository struct {
	*dbkit.Repository[Note]
}

// NewRepository returns a Repository backed by db. db is expected to come
// from dbkit.Open, already migrated with this module's own Migrations()
// (see Module.Migrations and cmd/server's wiring for the exact sequence) --
// see dbkit.Repository's own doc comment for why db is expected to come
// from Open specifically.
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{Repository: dbkit.NewRepository[Note](db)}
}
