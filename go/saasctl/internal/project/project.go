// Package project reads the go.mod context the db and config command
// groups share: a consumer project's module path, the app name saasctl new
// derived from the target directory at materialization (the go.mod module
// path, whose final element is the name the __APP_NAME__ token stood for),
// and the project's github.com/vislake/speed/go/* requires.
//
// The go.mod is parsed through golang.org/x/mod/modfile -- the Go team's
// own parser, the same dependency internal/upgrade uses -- so every form
// the toolchain itself writes (block and single-line requires, comments,
// replace directives) reads back exactly as the toolchain maintains it.
package project

import (
	"fmt"
	"os"
	"path"
	"strings"

	"golang.org/x/mod/modfile"
)

// modulePrefix is the import-path prefix shared by every module this
// repository releases. A require whose path starts with it is a speed
// module. The same constant exists in internal/upgrade (upgrade's own
// rewrite engine, which predates this package); the two copies are
// deliberately not unified into one shared definition because upgrade's
// files are not part of this block's edit scope, and each package's copy
// sits next to the code that uses it.
const modulePrefix = "github.com/vislake/speed/go/"

// Context is the go.mod-derived context of one consumer project.
type Context struct {
	// ModPath is the module path the go.mod declares.
	ModPath string

	// AppName is the final element of ModPath: the project's own name, the
	// token saasctl new substituted __APP_NAME__ with at materialization.
	// It names the project's SQLite file by default and prefixes the
	// bootstrap-configuration error messages, exactly as the generated
	// cmd/server names them.
	AppName string

	// Requires lists every required speed module path, in go.mod order.
	// The empty slice means the go.mod carries no speed requires at all.
	Requires []string
}

// Read parses the go.mod at path and returns its Context. A file that does
// not parse -- or whose module line is missing or empty -- is an error: a
// go.mod is the contract between saasctl and the project it maintains, and
// a file the Go toolchain itself would refuse is refused here too.
//
// The path is the command line's go.mod argument: the operator runs this
// tool on a checkout they own, and reading the file they name is the
// command's whole purpose, so gosec's file-inclusion rule has nothing to
// guard against here.
func Read(modFile string) (Context, error) {
	//nolint:gosec // G304: the path is the operator-supplied go.mod argument
	data, err := os.ReadFile(modFile)
	if err != nil {
		return Context{}, fmt.Errorf("read %s: %w", modFile, err)
	}
	f, err := modfile.Parse(modFile, data, nil)
	if err != nil {
		return Context{}, fmt.Errorf("parse go.mod: %w", err)
	}
	modPath := ""
	if f.Module != nil {
		modPath = f.Module.Mod.Path
	}
	if modPath == "" {
		return Context{}, fmt.Errorf("parse go.mod: %s declares no module path", modFile)
	}
	ctx := Context{
		ModPath: modPath,
		AppName: path.Base(modPath),
	}
	for _, req := range f.Require {
		if strings.HasPrefix(req.Mod.Path, modulePrefix) {
			ctx.Requires = append(ctx.Requires, req.Mod.Path)
		}
	}
	return ctx, nil
}
