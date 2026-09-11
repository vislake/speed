package notes

// component.go registers the "notes" component with pkgcore's global
// component registration: the descriptor a composition configuration selects
// as this app-local module's implementation. The component builds the same
// *Module every other caller builds through NewModule.

import (
	"context"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/notes/locales"
	"github.com/vislake/speed/examples/reference-app/internal/notes/migrations"
)

// notesComponent is the component descriptor for "notes". It declares
// MultiReplicaSafe: the module's state is its rows in the shared database
// and nothing it holds lives in one process alone.
//
// Requires the database as its one mandatory product; the creator resolver
// is optional, matching the module's own construction contract -- an
// unwired resolver fails every create closed (ErrSubjectUnresolved) rather
// than refusing to boot.
//
// Init is deliberately not declared: declaration (the module's Register
// call) is made today by the host's bootstrap path, not by this descriptor.
var notesComponent = pkgcore.Component{
	Name:         "notes",
	Module:       "notes",
	Provides:     []any{(*Module)(nil)},
	Capabilities: pkgcore.MultiReplicaSafe,
	Requires: []pkgcore.Requirement{
		{Token: (*gorm.DB)(nil)},
		{Token: (*SubjectResolver)(nil), Optional: true},
	},
	Migrations:  migrations.FS,
	Locales:     locales.FS,
	OpenAPISpec: openAPISpecYAML,
	New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		db, err := pkgcore.Get[*gorm.DB](reg)
		if err != nil {
			return nil, err
		}
		var opts []Option
		if subject, err := pkgcore.Get[SubjectResolver](reg); err == nil {
			opts = append(opts, WithSubjectResolver(subject))
		}
		return NewModule(db, opts...), nil
	},
}

func init() { pkgcore.MustRegister(notesComponent) }
