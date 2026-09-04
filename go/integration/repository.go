package integration

import (
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// APIKeyRepository is this module's tenant-scoped data-access type for
// APIKey.
//
// It embeds *dbkit.Repository[APIKey] (Create / FindByID / Update / Delete
// / List promoted unchanged, per the backend coding standard's "business
// repositories embed Repository[T], never hold a raw *gorm.DB" rule) and
// adds nothing else: round 1's query shapes -- "one key by id, inside the
// tenant" and "every key of the tenant" -- are exactly what the promoted
// FindByID and List already answer, so there is no extra query surface to
// write here. Delete is never called on this table: a key's end of life is
// RevokedAt, set through Update, never a row removal.
type APIKeyRepository struct {
	*dbkit.Repository[APIKey]
}

// NewAPIKeyRepository returns an APIKeyRepository backed by db.
func NewAPIKeyRepository(db *gorm.DB) *APIKeyRepository {
	return &APIKeyRepository{Repository: dbkit.NewRepository[APIKey](db)}
}
