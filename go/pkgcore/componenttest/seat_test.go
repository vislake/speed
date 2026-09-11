package componenttest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

// TestDuringInitOpensTheSeatsForTheDeclaration pins the helper's contract:
// the declaration runs inside a real Init stage, so its writes land, and the
// seats are closed again once it returns -- a later write is refused exactly
// as production refuses it.
func TestDuringInitOpensTheSeatsForTheDeclaration(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	DuringInit(t, reg, func(r *pkgcore.ComponentRegistry) error {
		if err := r.ConfigSeat().Add(pkgcore.ConfigItem{Key: "helped.item", Type: "string", Description: "d"}); err != nil {
			return err
		}
		return r.PermissionsSeat().Add("helped:read")
	})

	if items := reg.Config.Items(); len(items) != 1 || items[0].Key != "helped.item" {
		t.Fatalf("Config.Items() = %v, want the declaration made inside Init", items)
	}
	if perms := reg.Permissions.Permissions(); len(perms) != 1 || perms[0] != "helped:read" {
		t.Fatalf("Permissions() = %v, want the declaration made inside Init", perms)
	}

	if err := reg.Config.Add(pkgcore.ConfigItem{Key: "after.item", Type: "string", Description: "d"}); !errors.Is(err, pkgcore.ErrStageViolation) {
		t.Fatalf("a write after the helper returned = %v, want ErrStageViolation", err)
	}
}

// TestRunInitDrivesTheAssemblyAndReturnsTheProduct pins the minimal
// assembly: the product is returned, the values resolve as providers, and a
// requirement the values do not satisfy fails the Prepare stage in strict
// mode rather than auto-pulling a provider.
func TestRunInitDrivesTheAssemblyAndReturnsTheProduct(t *testing.T) {
	type greeting struct{ text string }
	type source struct{ text string }
	component := pkgcore.Component{
		Name:     "greeter",
		Provides: []any{(*greeting)(nil)},
		Requires: []pkgcore.Requirement{{Token: (*source)(nil)}},
		New: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ pkgcore.ComponentConfig) (any, error) {
			src, err := pkgcore.Get[*source](reg)
			if err != nil {
				return nil, err
			}
			return &greeting{text: src.text}, nil
		},
		Init: func(_ context.Context, reg *pkgcore.ComponentRegistry, _ any) error {
			return reg.ConfigSeat().Add(pkgcore.ConfigItem{Key: "greeter.item", Type: "string", Description: "d"})
		},
	}

	reg := pkgcore.NewComponentRegistry()
	if err := RunInit(t, reg, component, &source{text: "provided"}); err != nil {
		t.Fatalf("RunInit() = %v", err)
	}
	product, err := pkgcore.Get[*greeting](reg)
	if err != nil {
		t.Fatalf("the assembly's product: %v", err)
	}
	if product.text != "provided" {
		t.Fatalf("RunInit() product = %v, want the component's own product built over the given value", product)
	}
	if items := reg.Config.Items(); len(items) != 1 || items[0].Key != "greeter.item" {
		t.Fatalf("Config.Items() = %v, want the declaration the Init callback made", items)
	}

	unsatisfied := component
	unsatisfied.Name = "greeter2"
	unsatisfied.Requires = []pkgcore.Requirement{{Token: (*int)(nil)}}
	if err := RunInit(t, pkgcore.NewComponentRegistry(), unsatisfied); err == nil {
		t.Fatal("RunInit() with an unsatisfied requirement = nil, want a Prepare-stage refusal")
	} else if !errors.Is(err, pkgcore.ErrMissingRequirement) || !strings.Contains(err.Error(), "int") {
		t.Fatalf("refusal = %v, want a missing-requirement refusal naming the token", err)
	}
}
