package demo

// component.go registers the "demo" component with pkgcore's global
// component registration: the descriptor a composition configuration selects
// as this app-local module's implementation. The component is stateless --
// it builds the same parameterless *Module every other caller builds
// through NewModule.

import (
	"context"

	"github.com/vislake/speed/go/pkgcore"

	"github.com/vislake/speed/examples/reference-app/internal/demo/locales"
)

// demoComponent is the component descriptor for "demo": the app-local
// notification-type carrier. It declares MultiReplicaSafe -- it holds no
// state at all, so several replicas carrying the same declarations split
// nothing -- and requires nothing, exactly as the module's own empty
// DependsOn states.
//
// Init is deliberately not declared: declaration (the module's Register
// call) is made today by the host's bootstrap path, not by this descriptor.
var demoComponent = pkgcore.Component{
	Name:         "demo",
	Module:       "demo",
	Provides:     []any{(*Module)(nil)},
	Capabilities: pkgcore.MultiReplicaSafe,
	Locales:      locales.FS,
	New: func(_ context.Context, _ *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
		return NewModule(), nil
	},
}

func init() { pkgcore.MustRegister(demoComponent) }
