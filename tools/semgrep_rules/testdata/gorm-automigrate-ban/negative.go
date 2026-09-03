// Fixture for tools/semgrep_rules/gorm-automigrate-ban.yml.
// Clean control: none of these patterns may fire.
package fixture

import (
	"context"
	"io/fs"

	"github.com/vislake/speed/go/dbkit"
)

// Versioned migrations through dbkit.MigrationRegistry are the
// sanctioned path; schema migration SQL ships as embed.FS files.
func migrate(ctx context.Context, reg *dbkit.MigrationRegistry, dir fs.FS) error {
	return reg.Migrate(ctx, "fixture", dir)
}
