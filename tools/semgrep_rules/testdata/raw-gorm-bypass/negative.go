// Fixture for tools/semgrep_rules/raw-gorm-bypass.yml.
// Clean control: none of these patterns may fire.
package fixture

import (
	"context"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// Repository usage is the sanctioned path -- chaining and updates on the
// Repository receiver never touch the raw *gorm.DB surface.
type AccountRepository struct {
	*dbkit.Repository[Account]
}

func repoPath(ctx context.Context, repo *AccountRepository, id string) error {
	account, err := repo.FindByID(ctx, dbkit.ID(id))
	if err != nil {
		return err
	}
	return repo.Update(ctx, account, pkgcore.WithTenant(ctx, "tenant-a"))
}
