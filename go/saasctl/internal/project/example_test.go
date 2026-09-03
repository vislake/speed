package project

import (
	"fmt"
	"os"
	"path/filepath"
)

// ExampleRead shows the context a consumer project's go.mod yields: the
// module path saasctl new stamped at materialization, the app name derived
// from it, and the project's speed requires, in go.mod order, with
// third-party requires and replace directives left out of the record.
func ExampleRead() {
	dir, err := os.MkdirTemp("", "project-example")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "go.mod")
	err = os.WriteFile(path, []byte(`module example.com/smile/cli-app

go 1.25.0

require (
	github.com/vislake/speed/go/pkgcore v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/config v0.0.0-00010101000000-000000000000
	github.com/vislake/speed/go/authn v0.0.0-00010101000000-000000000000
	golang.org/x/mod v0.27.0 // indirect
)

replace github.com/vislake/speed/go/config => /some/checkout/go/config
`), 0o644)
	if err != nil {
		fmt.Println(err)
		return
	}
	ctx, err := Read(path)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println("module:", ctx.ModPath)
	fmt.Println("app name:", ctx.AppName)
	fmt.Println("speed requires:", ctx.Requires)
	// Output:
	// module: example.com/smile/cli-app
	// app name: cli-app
	// speed requires: [github.com/vislake/speed/go/pkgcore github.com/vislake/speed/go/config github.com/vislake/speed/go/authn]
}
