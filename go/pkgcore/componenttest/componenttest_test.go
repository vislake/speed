package componenttest

import (
	"context"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

type fixtureSchema struct {
	Host string `json:"host"`
}

type fixtureToken struct{}

type fixtureIface interface{ marker() }

// validComponent returns a descriptor that satisfies every contract check.
func validComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         "thingmaker",
		Module:       "thingmaker",
		ConfigSchema: (*fixtureSchema)(nil),
		Requires:     []pkgcore.Requirement{{Token: (*fixtureToken)(nil)}},
		Provides:     []any{(*fixtureIface)(nil)},
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &fixtureToken{}, nil
		},
	}
}

func TestWellFormedAcceptsAValidDescriptor(t *testing.T) {
	if err := WellFormed(validComponent()); err != nil {
		t.Fatalf("WellFormed(valid) = %v, want nil", err)
	}
	AssertWellFormed(t, validComponent())

	// A multi-implementation component extends its module's name, and one
	// with no module at all is a free-standing step.
	extended := validComponent()
	extended.Name = "thingmaker.local"
	if err := WellFormed(extended); err != nil {
		t.Errorf("WellFormed(%q) = %v, want nil", extended.Name, err)
	}
	free := validComponent()
	free.Name = "seed.demo"
	free.Module = ""
	if err := WellFormed(free); err != nil {
		t.Errorf("WellFormed(%q) = %v, want nil", free.Name, err)
	}

	// A hyphenated module name is expressible, so a component of such a
	// module can carry the module's own name ("ai-gateway" is the repo's
	// own hyphenated one).
	hyphenated := validComponent()
	hyphenated.Name = "ai-gateway"
	hyphenated.Module = "ai-gateway"
	if err := WellFormed(hyphenated); err != nil {
		t.Errorf("WellFormed(%q) = %v, want nil", hyphenated.Name, err)
	}
}

func TestWellFormedRejects(t *testing.T) {
	cases := []struct {
		name string
		make func() pkgcore.Component
		want []string
	}{
		{
			name: "empty name",
			make: func() pkgcore.Component {
				c := validComponent()
				c.Name = ""
				return c
			},
			want: []string{"no name"},
		},
		{
			name: "name convention",
			make: func() pkgcore.Component {
				c := validComponent()
				c.Name = "ThingMaker"
				return c
			},
			want: []string{"naming convention"},
		},
		{
			name: "name does not reflect the module",
			make: func() pkgcore.Component {
				c := validComponent()
				c.Name = "kv.redis"
				return c
			},
			want: []string{`module "thingmaker"`, "named after it"},
		},
		{
			name: "missing New",
			make: func() pkgcore.Component {
				c := validComponent()
				c.New = nil
				return c
			},
			want: []string{"no New callback"},
		},
		{
			name: "schema not a pointer",
			make: func() pkgcore.Component {
				c := validComponent()
				c.ConfigSchema = fixtureSchema{}
				return c
			},
			want: []string{"ConfigSchema", "pointer to a config struct"},
		},
		{
			name: "schema pointer to non-struct",
			make: func() pkgcore.Component {
				c := validComponent()
				c.ConfigSchema = (*string)(nil)
				return c
			},
			want: []string{"ConfigSchema"},
		},
		{
			name: "untyped requirement token",
			make: func() pkgcore.Component {
				c := validComponent()
				c.Requires = []pkgcore.Requirement{{Token: nil}}
				return c
			},
			want: []string{"Requires entry 0", "untyped nil"},
		},
		{
			name: "non-pointer Provides entry",
			make: func() pkgcore.Component {
				c := validComponent()
				c.Provides = []any{fixtureSchema{}}
				return c
			},
			want: []string{"Provides entry 0", "not a contract token"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := WellFormed(tc.make())
			if err == nil {
				t.Fatal("WellFormed = nil, want a contract violation")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not carry %q", err, want)
				}
			}
		})
	}
}

func TestWellFormedReportsEveryProblem(t *testing.T) {
	c := validComponent()
	c.Name = "Bad Name"
	c.New = nil

	err := WellFormed(c)
	if err == nil {
		t.Fatal("WellFormed = nil, want a contract violation")
	}
	for _, want := range []string{"naming convention", "no New callback"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q; every problem should be reported together", err, want)
		}
	}
}
