package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
)

// helpSchema is the schema the help-rendering tests describe: one field per
// face the rendering distinguishes -- an exposed field with a documentation
// group, a required field with a pinned environment variable, a derive-tagged
// sensitive key whose documentation cells must be masked, a plain
// block-only field, an exposed duration, and a skipped field that no
// listing may carry.
type helpSchema struct {
	Host      string        `json:"host" config:"expose,group=network"`
	Port      int           `json:"port" config:"expose,required,env=HELP_PORT"`
	CipherKey []byte        `json:"cipher_key" config:"derive,required,sensitive,group=security"`
	Plain     string        `json:"plain"`
	TTL       time.Duration `json:"ttl" config:"expose"`
	Internal  string        `json:"internal" config:"-"`
}

// ConfigDocs documents the schema's fields the way the assembly requires a
// sensitive field to: an entry with a description. The Default and Example
// on the sensitive field are the cells the rendering must replace with the
// redacted marker.
func (*helpSchema) ConfigDocs() map[string]pkgcore.FieldDoc {
	return map[string]pkgcore.FieldDoc{
		"host": {Description: "the address the helper binds", Default: "127.0.0.1:0", Example: "0.0.0.0:8080"},
		"cipher_key": {
			Description: "32 bytes of key material the helper seals with",
			Default:     "documented non-secret development default",
			Example:     "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",
		},
	}
}

// helpComponent returns a component carrying the fixture schema under name.
func helpComponent(name string) pkgcore.Component {
	return pkgcore.Component{
		Name:         name,
		ConfigSchema: (*helpSchema)(nil),
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return new(int), nil
		},
	}
}

// TestCollectComponentConfig_SchemaCarryingComponentsOnly: the collection
// carries one surface per component that declares resolvable fields -- the
// fixture schema's fields, with the skipped field absent (no source resolves
// it, so a listing must not present it as configurable) -- and contributes
// nothing for a component without a schema or one whose empty struct
// describes no field.
func TestCollectComponentConfig_SchemaCarryingComponentsOnly(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	for _, c := range []pkgcore.Component{
		helpComponent("cfghelp.helper"),
		{Name: "cfghelp.schemaless", New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return new(int), nil
		}},
		{
			Name:         "cfghelp.empty",
			ConfigSchema: (*struct{})(nil),
			New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
				return new(int), nil
			},
		},
	} {
		if err := reg.Register(c); err != nil {
			t.Fatalf("register %s: %v", c.Name, err)
		}
	}

	surfaces, err := CollectComponentConfig(pkgcore.RegisteredComponents(reg))
	if err != nil {
		t.Fatalf("CollectComponentConfig() error = %v, want nil", err)
	}

	var keys []string
	names := make(map[string]bool, len(surfaces))
	for _, s := range surfaces {
		names[s.Name] = true
		if s.Name == "cfghelp.helper" {
			for _, f := range s.Fields {
				keys = append(keys, f.Key)
			}
		}
	}
	if !names["cfghelp.helper"] {
		t.Fatalf("the fixture component is missing from the collected surface %v", surfaces)
	}
	if names["cfghelp.schemaless"] || names["cfghelp.empty"] {
		t.Errorf("components without resolvable fields appear in the surface: %v", names)
	}
	wantKeys := []string{
		"host",
		"port",
		"cipher_key",
		"plain",
		"ttl",
	}
	if len(keys) != len(wantKeys) {
		t.Fatalf("fixture keys = %v, want %v", keys, wantKeys)
	}
	for i, want := range wantKeys {
		if keys[i] != want {
			t.Errorf("fixture keys[%d] = %q, want %q", i, keys[i], want)
		}
	}
}

// TestCollectComponentConfig_NamespaceComesFromTheRegistration: a component
// registered with a ConfigNamespace renders its fields at the namespaced key
// paths -- the prefix pkgcore's shared collection applies -- so the help
// listing spells a field's key path exactly as the assembly resolves it
// (authn's platform keys are the live instance of this rule).
func TestCollectComponentConfig_NamespaceComesFromTheRegistration(t *testing.T) {
	const name = "cfghelp.namespaced"
	if err := pkgcore.Register(pkgcore.Component{
		Name:            name,
		ConfigNamespace: "cfghelp.ns",
		ConfigSchema:    (*helpSchema)(nil),
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return new(int), nil
		},
	}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}

	surfaces, err := CollectComponentConfig(pkgcore.RegisteredComponents(pkgcore.NewComponentRegistry()))
	if err != nil {
		t.Fatalf("CollectComponentConfig() error = %v, want nil", err)
	}
	var found bool
	for _, s := range surfaces {
		if s.Name != name {
			continue
		}
		found = true
		if got, want := s.Fields[0].Key, "cfghelp.ns.host"; got != want {
			t.Errorf("first key = %q, want the declared namespace's %q", got, want)
		}
	}
	if !found {
		t.Fatalf("the namespaced fixture is missing from the collected surface")
	}
}

// TestCollectComponentConfig_MalformedSchemaIsRefused: a schema the shared
// collection refuses fails the collection naming the component -- the
// surface must not silently drop a declaration the assembly itself would
// refuse at Prepare.
func TestCollectComponentConfig_MalformedSchemaIsRefused(t *testing.T) {
	type badSchema struct {
		Field string `json:"field" config:"frobnicate"`
	}
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(pkgcore.Component{
		Name:         "cfghelp.malformed",
		ConfigSchema: (*badSchema)(nil),
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return new(int), nil
		},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	_, err := CollectComponentConfig(pkgcore.RegisteredComponents(reg))
	if err == nil {
		t.Fatal("CollectComponentConfig() error = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "cfghelp.malformed") {
		t.Errorf("error %q does not name the declaring component", err)
	}
}

// TestCollectComponentConfig_EmptySetDescribesAsEmpty: a set carrying no
// schema-carrying component -- the empty global registration of a binary
// that imports none -- describes as an empty surface, not an error: the
// renderer's own line states the absence.
func TestCollectComponentConfig_EmptySetDescribesAsEmpty(t *testing.T) {
	surfaces, err := CollectComponentConfig(nil)
	if err != nil {
		t.Fatalf("CollectComponentConfig(nil) error = %v, want nil", err)
	}
	if len(surfaces) != 0 {
		t.Fatalf("CollectComponentConfig(nil) = %v, want no surfaces", surfaces)
	}
}

// TestRenderComponentConfigHelp_PinsTheDeclaredSurface renders the fixture
// surface and pins the whole output: the key path with its type and markers,
// the source line per resolution face, the flag and environment spellings,
// the documented cells, and -- the masking the contract owes an operator --
// the sensitive field's Default and Example replaced by the redacted marker
// while its Description still renders.
func TestRenderComponentConfigHelp_PinsTheDeclaredSurface(t *testing.T) {
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(helpComponent("cfghelp.helper")); err != nil {
		t.Fatalf("register: %v", err)
	}
	surfaces, err := CollectComponentConfig(pkgcore.RegisteredComponents(reg))
	if err != nil {
		t.Fatalf("CollectComponentConfig() error = %v, want nil", err)
	}
	var fixture []ComponentConfigSurface
	for _, s := range surfaces {
		if s.Name == "cfghelp.helper" {
			fixture = append(fixture, s)
		}
	}

	var out strings.Builder
	if err := RenderComponentConfigHelp(&out, fixture, "APP_"); err != nil {
		t.Fatalf("RenderComponentConfigHelp() error = %v, want nil", err)
	}

	want := `Component configuration surface (each field collected through
pkgcore.DescribeComponentSchema, the same collection the generated
configuration reference reads; a key path is the address its flag,
environment variable, config-file entry or derivation spells):

cfghelp.helper
  host  string  group=network
      source: flag > environment > config file > the configuration block's value
      flag: --host
      env: APP_HOST
      description: the address the helper binds
      default: 127.0.0.1:0
      example: 0.0.0.0:8080
  port  int  required
      source: flag > environment > config file > the configuration block's value
      flag: --port
      env: HELP_PORT (pinned by the declaration)
  cipher_key  []byte  derive required sensitive group=security
      source: flag > environment > config file > root-key derivation > declared default
      flag: --cipher_key
      env: APP_CIPHER_KEY
      description: 32 bytes of key material the helper seals with
      default: [redacted]
      example: [redacted]
  plain  string
      source: the component's configuration block only
  ttl  time.Duration
      source: flag > environment > config file > the configuration block's value
      flag: --ttl
      env: APP_TTL
`
	if out.String() != want {
		t.Errorf("rendered help differs from the pinned surface:\n--- got ---\n%s--- want ---\n%s", out.String(), want)
	}

	// The masking is a property of the sensitive field alone: the plain
	// field's cells above render verbatim, so a leak of the secret's
	// declared cells is the only failure mode left to assert.
	for _, leaked := range []string{"documented non-secret development default", "000102030405060708090a0b0c0d0e0f"} {
		if strings.Contains(out.String(), leaked) {
			t.Errorf("the rendering leaked a sensitive field's declared cell %q", leaked)
		}
	}
	if strings.Contains(out.String(), ".internal") {
		t.Errorf("the skipped field appears in the rendering:\n%s", out.String())
	}
}

// TestRenderComponentConfigHelp_DefaultPrefixWhenUnset: an empty prefix
// renders the loader's own default prefix in the derived environment names,
// so a caller that does not configure one cannot print a spelling the
// loader would not read.
func TestRenderComponentConfigHelp_DefaultPrefixWhenUnset(t *testing.T) {
	var out strings.Builder
	surfaces := []ComponentConfigSurface{{
		Name: "solo",
		Fields: []pkgcore.FieldDescriptor{{
			Key:    "addr",
			Type:   "string",
			Expose: true,
		}},
	}}
	if err := RenderComponentConfigHelp(&out, surfaces, ""); err != nil {
		t.Fatalf("RenderComponentConfigHelp() error = %v, want nil", err)
	}
	if want := "env: SPEED_ADDR\n"; !strings.Contains(out.String(), want) {
		t.Errorf("rendering lacks the loader's default-prefix spelling %q:\n%s", want, out.String())
	}
}

// TestRenderComponentConfigHelp_EmptySurface: a registry none of whose
// components declare a schema renders that absence as one honest line
// rather than an empty document.
func TestRenderComponentConfigHelp_EmptySurface(t *testing.T) {
	var out strings.Builder
	if err := RenderComponentConfigHelp(&out, nil, "APP_"); err != nil {
		t.Fatalf("RenderComponentConfigHelp() error = %v, want nil", err)
	}
	if want := "(no registered component declares a configuration schema)\n"; !strings.Contains(out.String(), want) {
		t.Errorf("rendering lacks %q:\n%s", want, out.String())
	}
}

// failingWriter refuses every write, the shape a closed pipe or a full disk
// produces.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("cfghelp: write refused")
}

// TestRenderComponentConfigHelp_WriteErrorPropagates: a failed write is
// reported, never swallowed -- the caller's exit code is the contract.
func TestRenderComponentConfigHelp_WriteErrorPropagates(t *testing.T) {
	err := RenderComponentConfigHelp(failingWriter{}, []ComponentConfigSurface{{
		Name:   "solo",
		Fields: []pkgcore.FieldDescriptor{{Key: "addr", Type: "string"}},
	}}, "APP_")
	if err == nil {
		t.Fatal("RenderComponentConfigHelp() error = nil, want the writer's error")
	}
}
