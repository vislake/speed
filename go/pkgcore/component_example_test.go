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

// exampleVendor is the catalog contract the ExampleMembers components
// deliver members of.
type exampleVendor interface{ VendorName() string }

// exampleVendorImpl implements exampleVendor.
type exampleVendorImpl struct{ name string }

func (v *exampleVendorImpl) VendorName() string { return v.name }

// ExampleMembers shows the catalog delivery and its reading: two components
// contributing members of one contract are selected together -- the
// selection a bound delivery would refuse as ambiguous -- and Members
// enumerates them by name, while Get stays blind to them because a member
// product is not put into the by-type context.
func ExampleMembers() {
	ctx := context.Background()

	reg := pkgcore.NewComponentRegistry()
	for _, vendor := range []struct{ component, name string }{
		{"chat.alpha", "Alpha"},
		{"chat.beta", "Beta"},
	} {
		impl := &exampleVendorImpl{name: vendor.name}
		if err := reg.Register(pkgcore.Component{
			Name:           vendor.component,
			Module:         "chat",
			ProvidesMember: []any{(*exampleVendor)(nil)},
			New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
				return impl, nil
			},
		}); err != nil {
			fmt.Println("register:", err)
			return
		}
	}
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{"chat.alpha": nil, "chat.beta": nil},
	}))
	for _, stage := range []struct {
		name string
		run  func(context.Context) error
	}{{"prepare", reg.Prepare}, {"construct", reg.Construct}} {
		if err := stage.run(ctx); err != nil {
			fmt.Println(stage.name+":", err)
			return
		}
	}
	defer func() { _ = reg.Close(ctx) }()

	for _, member := range pkgcore.Members[exampleVendor](reg) {
		fmt.Println(member.Name, "->", member.Value.VendorName())
	}
	_, err := pkgcore.Get[exampleVendor](reg)
	fmt.Println("get:", errors.Is(err, pkgcore.ErrMissingRequirement))

	// Output:
	// chat.alpha -> Alpha
	// chat.beta -> Beta
	// get: true
}
