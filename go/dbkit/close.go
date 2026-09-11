package dbkit

import (
	"errors"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// Close releases db's underlying database handle: it resolves the
// *sql.DB Open tucked inside the *gorm.DB and closes it, so every pooled
// connection is released and a later use of db fails instead of silently
// opening new ones. It is the one sanctioned way to end a *gorm.DB's life,
// the counterpart of Open being the one sanctioned way to obtain one:
// business modules never reach for db.DB() and close the handle by hand,
// exactly as they never call gorm.Open by hand.
//
// A caller closes its connection once, when the component or process that
// owns it shuts down -- for a component-registered connection, the db
// component's own Close callback (see component.go) is the caller. A nil db
// is returned as a plain error, mirroring Apply's own non-nil requirement;
// a handle whose underlying *sql.DB cannot be resolved or fails to close is
// returned as an *apperr.Error (apperr.Internal, code dbkit.close_failed,
// carrying the cause) so a shutdown path can report it through the same
// error surface as every other dbkit failure.
func Close(db *gorm.DB) error {
	if db == nil {
		return errors.New("dbkit: Close requires a non-nil *gorm.DB")
	}

	sqlDB, err := db.DB()
	if err != nil {
		return apperr.Internal("dbkit.close_failed").WithCause(err)
	}
	if err := sqlDB.Close(); err != nil {
		return apperr.Internal("dbkit.close_failed").WithCause(err)
	}
	return nil
}
