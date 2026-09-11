package pkgcore_test

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
)

// ExampleGlobalComponents enumerates the package-level global registration:
// a component registered here is visible to every later snapshot and to
// every registry instance created afterwards, which is how a roster check
// proves a binary contains the components its imports declare.
func ExampleGlobalComponents() {
	err := pkgcore.Register(pkgcore.Component{
		Name: "example.roster",
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &struct{}{}, nil
		},
	})
	if err != nil {
		fmt.Println("register:", err)
		return
	}

	for _, c := range pkgcore.GlobalComponents() {
		if c.Name == "example.roster" {
			fmt.Println("found:", c.Name)
		}
	}

	// Output:
	// found: example.roster
}
