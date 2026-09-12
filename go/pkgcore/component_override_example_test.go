package pkgcore_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
)

// exampleTimezone and exampleBuzzer are the contract types the Override
// example's requirement tokens name.
type exampleTimezone struct{}
type exampleBuzzer struct{}

// ExampleLookupComponent reads a registered descriptor back by name: the
// reading a host wiring performs before deriving its own construction for a
// component, and the way any caller acts on a component's own declarations
// rather than on a selection plan.
func ExampleLookupComponent() {
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(pkgcore.Component{
		Name:   "example.clock",
		Module: "clock",
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &struct{}{}, nil
		},
	}); err != nil {
		fmt.Println("register:", err)
		return
	}

	component, ok := pkgcore.LookupComponent(reg, "example.clock")
	fmt.Println("found:", ok, "module:", component.Module)

	if _, ok := pkgcore.LookupComponent(reg, "example.clock.absent"); !ok {
		fmt.Println("absent reported as absent")
	}

	// Output:
	// found: true module: clock
	// absent reported as absent
}

// ExampleOverride derives a host's own construction for a component: the
// declaration surface stays the registered descriptor's own -- module,
// capabilities, assets and lifecycle callbacks included -- and only the
// name, the New callback and the appended extra requirements differ.
func ExampleOverride() {
	type clock struct{ source string }

	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(pkgcore.Component{
		Name:         "example.alarm-clock",
		Module:       "clock",
		Capabilities: pkgcore.MultiReplicaSafe,
		Requires:     []pkgcore.Requirement{{Token: (*exampleTimezone)(nil)}},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &clock{source: "system"}, nil
		},
	}); err != nil {
		fmt.Println("register:", err)
		return
	}

	construct := func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
		return &clock{source: "host"}, nil
	}
	host, err := pkgcore.Override(reg, "example.alarm-clock", "host.alarm-clock", construct,
		pkgcore.Requirement{Token: (*exampleBuzzer)(nil)},
	)
	if err != nil {
		fmt.Println("override:", err)
		return
	}
	fmt.Println("name:", host.Name)
	fmt.Println("module kept:", host.Module)
	fmt.Println("capabilities kept:", host.Capabilities == pkgcore.MultiReplicaSafe)
	fmt.Println("requires:", len(host.Requires))

	if _, err := pkgcore.Override(reg, "example.absent", "host.absent", construct); err != nil {
		fmt.Println("unregistered base named:", strings.Contains(err.Error(), `"example.absent"`))
	}

	// Output:
	// name: host.alarm-clock
	// module kept: clock
	// capabilities kept: true
	// requires: 2
	// unregistered base named: true
}
