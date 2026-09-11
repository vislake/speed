package pkgcore_test

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
)

// exampleGreeterConfig is the greeter component's configuration schema.
type exampleGreeterConfig struct {
	Greeting string `json:"greeting"`
}

// ExampleNewComponentRegistry assembles a two-component application and
// drives it through the stage sequence: the clock component is selected with
// a bare entry (defaults), the greeter with a configuration block, and both
// are constructed with the products reachable through the by-type context.
func ExampleNewComponentRegistry() {
	ctx := context.Background()
	type clock struct{}
	type greeter struct{ greeting string }

	reg := pkgcore.NewComponentRegistry()
	components := []pkgcore.Component{
		{
			Name: "clock",
			New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
				return &clock{}, nil
			},
		},
		{
			Name:         "greeter",
			Module:       "greeter",
			ConfigSchema: (*exampleGreeterConfig)(nil),
			New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
				var c exampleGreeterConfig
				if err := cfg.Decode(&c); err != nil {
					return nil, err
				}
				return &greeter{greeting: c.Greeting}, nil
			},
		},
	}
	for _, c := range components {
		if err := reg.Register(c); err != nil {
			fmt.Println("register:", err)
			return
		}
	}

	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"clock":   nil,
			"greeter": map[string]any{"greeting": "hi"},
		},
	}))

	stages := []struct {
		name string
		run  func(context.Context) error
	}{
		{"prepare", reg.Prepare},
		{"construct", reg.Construct},
		{"verify", reg.Verify},
		{"init", reg.Init},
		{"start", reg.Start},
	}
	for _, stage := range stages {
		if err := stage.run(ctx); err != nil {
			fmt.Printf("%s: %v\n", stage.name, err)
			return
		}
	}

	built, err := pkgcore.Get[*greeter](reg)
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	fmt.Println("greeting:", built.greeting)
	fmt.Println("members:", pkgcore.MemberNames(reg, "greeter"))

	if err := reg.Stop(ctx); err != nil {
		fmt.Println("stop:", err)
	}
	if err := reg.Close(ctx); err != nil {
		fmt.Println("close:", err)
	}

	// Output:
	// greeting: hi
	// members: [greeter]
}
