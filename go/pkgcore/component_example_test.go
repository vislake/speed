package pkgcore_test

import (
	"context"
	"errors"
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

// ExampleComponentCapabilities shows the independent capability read: the
// declaration a selected component carries is read on its own, so a caller
// that must compare a capability before wiring the product gets it without
// Build growing a second return value. The read follows the assembly plan,
// so an unselected name reports the selection gap.
func ExampleComponentCapabilities() {
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(pkgcore.Component{
		Name:         "example.queue",
		Module:       "example",
		Capabilities: pkgcore.MultiReplicaSafe,
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &struct{}{}, nil
		},
	}); err != nil {
		fmt.Println("register:", err)
		return
	}
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{"example.queue": nil},
	}))
	if err := reg.Prepare(context.Background()); err != nil {
		fmt.Println("prepare:", err)
		return
	}

	caps, err := pkgcore.ComponentCapabilities(reg, "example.queue")
	fmt.Println(caps, err)
	fmt.Println(caps.Has(pkgcore.MultiReplicaSafe), caps.Has(pkgcore.SurvivesRestart))

	_, err = pkgcore.ComponentCapabilities(reg, "example.unselected")
	fmt.Println(errors.Is(err, pkgcore.ErrUnknownComponent))

	// Output:
	// MultiReplicaSafe <nil>
	// true false
	// true
}
