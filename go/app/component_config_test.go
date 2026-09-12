package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/go/pkgcore"
	pkgconfig "github.com/vislake/speed/go/pkgcore/config"
)

// resolverName is the component name the resolver tests assemble: a
// dot-free name, so the flag, environment and file spellings of its key
// paths have one obvious form.
const resolverName = "cfgresolver"

// resolverSchema is the schema the resolver tests exercise: one field per
// resolution face the component contract declares.
type resolverSchema struct {
	Host      string        `json:"host" config:"expose"`
	Port      int           `json:"port" config:"expose"`
	CipherKey []byte        `json:"cipher_key" config:"derive"`
	Pinned    string        `json:"pinned" config:"expose,env=RESOLVER_PINNED_ADDR"`
	TTL       time.Duration `json:"ttl" config:"expose"`
	Debug     bool          `json:"debug" config:"expose"`
	Token     string        `json:"token" config:"expose,sensitive"`
	Plain     string        `json:"plain"`
	Internal  string        `json:"internal" config:"-"`
	Managed   struct {
		Address string `json:"address" config:"expose"`
	} `json:"managed" config:"-"`
}

// ConfigDocs documents the schema's sensitive field, the pairing the
// assembly requires.
func (resolverSchema) ConfigDocs() map[string]pkgcore.FieldDoc {
	return map[string]pkgcore.FieldDoc{
		"token": {Description: "the token the resolver test seals"},
	}
}

// resolverProduct is the fixture component's product: the configuration its
// New decoded, so a test reads what actually reached construction.
type resolverProduct struct{ cfg resolverSchema }

// resolverComponent returns the fixture component: its New decodes its
// configuration block exactly as every component's New does.
func resolverComponent(name string) pkgcore.Component {
	return pkgcore.Component{
		Name:         name,
		ConfigSchema: (*resolverSchema)(nil),
		New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
			var c resolverSchema
			if err := cfg.Decode(&c); err != nil {
				return nil, err
			}
			return &resolverProduct{cfg: c}, nil
		},
	}
}

// resolverAssembly describes one test assembly.
type resolverAssembly struct {
	// block is the component's configuration block as the composition
	// carries it; nil selects the component with no block at all.
	block map[string]any
	// args and environ are the process-level sources the loader reads.
	args    []string
	environ []string
	// options are extra loader options (a root key, a config file).
	options []ConfigOption
	// component overrides the fixture component, for the checks that judge
	// a different declaration. It must carry the fixture's name.
	component pkgcore.Component
}

// run drives the described assembly through the engine's own entry points
// (Load, Prepare, Construct) and returns the registry, or the failure the
// assembly reported.
func (a resolverAssembly) run(t *testing.T) (*pkgcore.ComponentRegistry, error) {
	t.Helper()

	// The injected environment is the whole environment these assemblies
	// read, so an ambient variable cannot leak into a case.
	loaderOptions := append(testConfigOptions(), ConfigArgs(a.args), pkgconfig.WithEnviron(a.environ))
	loaderOptions = append(loaderOptions, a.options...)

	var host testHostConfig
	spec := LoadSpec{Host: &host, Options: loaderOptions, Args: []string{}}

	component := a.component
	if component.Name == "" {
		component = resolverComponent(resolverName)
	}
	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(component); err != nil {
		t.Fatalf("register %s: %v", component.Name, err)
	}
	selection := any(nil)
	if a.block != nil {
		selection = pkgcore.NewComponentConfig(a.block)
	}
	reg.Put(CompositionOverrides{Config: pkgcore.ComponentConfig{}.With("components",
		pkgcore.ComponentConfig{}.With(component.Name, selection).With("observability", false))})

	if err := Assemble(context.Background(), reg, spec); err != nil {
		return nil, err
	}
	return reg, nil
}

// assemble runs the described assembly of the fixture component and returns
// its decoded configuration.
func (a resolverAssembly) assemble(t *testing.T) (resolverSchema, error) {
	t.Helper()

	reg, err := a.run(t)
	if err != nil {
		return resolverSchema{}, err
	}
	product, err := pkgcore.Get[*resolverProduct](reg)
	if err != nil {
		t.Fatalf("read the product: %v", err)
	}
	return product.cfg, nil
}

// resolverFile writes a config file carrying one host value at the
// component's own key path. The fixture component declares no namespace,
// so the key path is the bare local path, and the file spells it the way
// the loader reads any declared key.
func resolverFile(t *testing.T, host string) string {
	t.Helper()

	body := fmt.Sprintf("host: %s\n", host)
	path := filepath.Join(t.TempDir(), "resolver.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

// resolverDeriver is the derivation the assembled loader installs: a plain
// deterministic function, the same stand-in shape the loader's own tests
// use.
func resolverDeriver(rootKey []byte, keyPath string) ([]byte, error) {
	sum := sha256.Sum256(append(append([]byte{}, rootKey...), keyPath...))
	return sum[:], nil
}

// resolverRootKey is the 32-byte root key the derivation cases configure.
func resolverRootKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = 0x40 + byte(i)
	}
	return key
}

// resolverMaterial returns the material the fixture derivation produces for
// the component's cipher key.
func resolverMaterial(t *testing.T, keyPath string) []byte {
	t.Helper()
	material, err := resolverDeriver(resolverRootKey(), keyPath)
	if err != nil {
		t.Fatalf("derive %s: %v", keyPath, err)
	}
	return material
}

// TestResolverComponentConfig_FiveSourceLadder walks the priority chain one
// source at a time: the command-line flag outranks the environment variable,
// which outranks the config file, which outranks the composition block --
// every source spelling the field's own key path.
func TestResolverComponentConfig_FiveSourceLadder(t *testing.T) {
	t.Parallel()

	const flag = "--host=from-flag"
	env := "TEST_HOST=from-env"
	file := resolverFile(t, "from-file")
	block := map[string]any{"host": "from-block", "port": 8080}

	cases := []struct {
		name string
		a    resolverAssembly
		want string
	}{
		{
			name: "the flag outranks every other source",
			a:    resolverAssembly{block: block, args: []string{flag}, environ: []string{env}, options: []ConfigOption{ConfigFile(file)}},
			want: "from-flag",
		},
		{
			name: "the environment outranks the file and the block",
			a:    resolverAssembly{block: block, environ: []string{env}, options: []ConfigOption{ConfigFile(file)}},
			want: "from-env",
		},
		{
			name: "the file outranks the block",
			a:    resolverAssembly{block: block, options: []ConfigOption{ConfigFile(file)}},
			want: "from-file",
		},
		{
			name: "the environment outranks the block",
			a:    resolverAssembly{block: block, environ: []string{env}},
			want: "from-env",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := tt.a.assemble(t)
			if err != nil {
				t.Fatalf("assemble: %v", err)
			}
			if cfg.Host != tt.want {
				t.Errorf("Host = %q, want %q", cfg.Host, tt.want)
			}
			if cfg.Port != 8080 {
				t.Errorf("Port = %d, want the block's 8080", cfg.Port)
			}
		})
	}

	t.Run("the block alone stands with no other source set", func(t *testing.T) {
		t.Parallel()

		cfg, err := resolverAssembly{block: block}.assemble(t)
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		if cfg.Host != "from-block" {
			t.Errorf("Host = %q, want the block's value", cfg.Host)
		}
	})
}

// TestResolverComponentConfig_DeriveMaterialLadder walks the key-material
// tiers: an explicit value outranks the root-key derivation, the derivation
// outranks the declared defaults table, and the table stands when no root
// key is configured -- the same three-stage order the declaration path runs.
func TestResolverComponentConfig_DeriveMaterialLadder(t *testing.T) {
	t.Parallel()

	keyPath := "cipher_key"
	derived := resolverMaterial(t, keyPath)
	explicit := strings.Repeat("ab", 32)
	explicitBytes := slices.Repeat([]byte{0xab}, 32)
	table := map[string][]byte{keyPath: slices.Repeat([]byte{0x0d}, 32)}

	t.Run("an explicit value outranks the derivation", func(t *testing.T) {
		t.Parallel()

		cfg, err := resolverAssembly{
			environ: []string{"TEST_CIPHER_KEY=" + explicit},
			options: []ConfigOption{ConfigRootKey(resolverRootKey()), ConfigKeyDerivation(resolverDeriver)},
		}.assemble(t)
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		if !slices.Equal(cfg.CipherKey, explicitBytes) {
			t.Errorf("CipherKey = %x, want the explicit value", cfg.CipherKey)
		}
	})

	t.Run("the derivation outranks the declared default", func(t *testing.T) {
		t.Parallel()

		cfg, err := resolverAssembly{
			options: []ConfigOption{
				ConfigRootKey(resolverRootKey()),
				ConfigKeyDerivation(resolverDeriver),
				ConfigDevDefaults(table),
			},
		}.assemble(t)
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		if !slices.Equal(cfg.CipherKey, derived) {
			t.Errorf("CipherKey = %x, want the derivation's material %x", cfg.CipherKey, derived)
		}
	})

	t.Run("the declared default stands with no root key", func(t *testing.T) {
		t.Parallel()

		cfg, err := resolverAssembly{
			options: []ConfigOption{
				ConfigDevDefaults(table),
				ConfigKeyDerivation(resolverDeriver),
			},
		}.assemble(t)
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		if !slices.Equal(cfg.CipherKey, table[keyPath]) {
			t.Errorf("CipherKey = %x, want the declared default %x", cfg.CipherKey, table[keyPath])
		}
	})
}

// requiredResolverSchema declares the two required shapes the assembly's
// required check judges.
type requiredResolverSchema struct {
	Port      int    `json:"port" config:"expose,required"`
	CipherKey []byte `json:"cipher_key" config:"derive,required"`
}

// requiredResolverProduct is the required fixture's product.
type requiredResolverProduct struct{ cfg requiredResolverSchema }

// requiredResolverComponent returns the fixture component over the required
// schema.
func requiredResolverComponent() pkgcore.Component {
	return pkgcore.Component{
		Name:         resolverName,
		ConfigSchema: (*requiredResolverSchema)(nil),
		New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
			var c requiredResolverSchema
			if err := cfg.Decode(&c); err != nil {
				return nil, err
			}
			return &requiredResolverProduct{cfg: c}, nil
		},
	}
}

// TestResolverComponentConfig_DeriveMaterialSatisfiesRequired pins the
// contract point the assembly's required check rests on: a derive-tagged
// field's material enters the resolved block, so a required key-material
// field is satisfied by the derivation (and by an explicit zero counts as
// supplied) -- and a required value no source supplies still fails the
// assembly at the Prepare stage, the shape the root package's check reports.
func TestResolverComponentConfig_DeriveMaterialSatisfiesRequired(t *testing.T) {
	t.Parallel()

	keyPath := "cipher_key"

	t.Run("the derivation satisfies the required key", func(t *testing.T) {
		t.Parallel()

		reg, err := resolverAssembly{
			component: requiredResolverComponent(),
			block:     map[string]any{"port": 8080},
			options: []ConfigOption{
				ConfigRootKey(resolverRootKey()),
				ConfigKeyDerivation(resolverDeriver),
			},
		}.run(t)
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		product, err := pkgcore.Get[*requiredResolverProduct](reg)
		if err != nil {
			t.Fatalf("read the product: %v", err)
		}
		if !slices.Equal(product.cfg.CipherKey, resolverMaterial(t, keyPath)) {
			t.Errorf("CipherKey = %x, want the derivation's material", product.cfg.CipherKey)
		}
		if product.cfg.Port != 8080 {
			t.Errorf("Port = %d, want the block's 8080", product.cfg.Port)
		}
	})

	t.Run("an explicit zero counts as supplied", func(t *testing.T) {
		t.Parallel()

		reg, err := resolverAssembly{
			component: requiredResolverComponent(),
			args:      []string{"--port=0"},
			options:   []ConfigOption{ConfigDevDefaults(map[string][]byte{keyPath: slices.Repeat([]byte{0x0d}, 32)})},
		}.run(t)
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		product, err := pkgcore.Get[*requiredResolverProduct](reg)
		if err != nil {
			t.Fatalf("read the product: %v", err)
		}
		if product.cfg.Port != 0 {
			t.Errorf("Port = %d, want the explicitly supplied zero", product.cfg.Port)
		}
	})

	t.Run("a required value no source supplies fails the assembly", func(t *testing.T) {
		t.Parallel()

		_, err := resolverAssembly{component: requiredResolverComponent()}.run(t)
		if !errors.Is(err, pkgcore.ErrMissingConfigValue) {
			t.Fatalf("assemble error = %v, want ErrMissingConfigValue", err)
		}
		for _, want := range []string{resolverName, `"port"`, `"cipher_key"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not carry %q", err, want)
			}
		}
	})
}

// TestResolverComponentConfig_SkippedFieldsLeaveTheBlock pins the skip
// option's enforcement point in the resolver: a field that resolves from no
// source must not have one either, so a block entry at its key path -- or
// anywhere in a skipped container's subtree -- is dropped before the block
// reaches the component's New.
func TestResolverComponentConfig_SkippedFieldsLeaveTheBlock(t *testing.T) {
	t.Parallel()

	cfg, err := resolverAssembly{
		block: map[string]any{
			"host":     "kept",
			"port":     8080,
			"internal": "must-not-arrive",
			"managed":  map[string]any{"address": "must-not-arrive-either"},
		},
		args: []string{"--internal=also-refused"},
	}.assemble(t)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if cfg.Internal != "" {
		t.Errorf("Internal = %q, want the zero value: no source resolves a skipped field", cfg.Internal)
	}
	if cfg.Managed.Address != "" {
		t.Errorf("Managed.Address = %q, want the zero value: a skipped container's subtree resolves from no source", cfg.Managed.Address)
	}
	if cfg.Host != "kept" || cfg.Port != 8080 {
		t.Errorf("Host/Port = %q/%d, want the block's other entries untouched", cfg.Host, cfg.Port)
	}
}

// TestResolverComponentConfig_PinnedEnvironmentName pins the env option: the
// pinned variable is read exactly as spelled, and the derived spelling of
// the field's key path is not a second way in.
func TestResolverComponentConfig_PinnedEnvironmentName(t *testing.T) {
	t.Parallel()

	t.Run("the pinned variable is read", func(t *testing.T) {
		t.Parallel()

		cfg, err := resolverAssembly{
			environ: []string{"RESOLVER_PINNED_ADDR=pinned-value", "TEST_PINNED=derived-value"},
		}.assemble(t)
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		if cfg.Pinned != "pinned-value" {
			t.Errorf("Pinned = %q, want the pinned variable's value", cfg.Pinned)
		}
	})

	t.Run("the derived spelling alone resolves nothing", func(t *testing.T) {
		t.Parallel()

		cfg, err := resolverAssembly{
			environ: []string{"TEST_PINNED=derived-value"},
		}.assemble(t)
		if err != nil {
			t.Fatalf("assemble: %v", err)
		}
		if cfg.Pinned != "" {
			t.Errorf("Pinned = %q, want the zero value: only the pinned name is a source", cfg.Pinned)
		}
	})
}

// TestResolverComponentConfig_UntaggedFieldsStayBlockOnly pins the default
// state of the vocabulary: a field with no tag is supplied by the
// configuration block alone, so the field's own flag and environment
// spellings resolve nothing and the block's value stands.
func TestResolverComponentConfig_UntaggedFieldsStayBlockOnly(t *testing.T) {
	t.Parallel()

	cfg, err := resolverAssembly{
		block:   map[string]any{"plain": "from-block", "port": 8080},
		args:    []string{"--plain=from-flag"},
		environ: []string{"TEST_PLAIN=from-env"},
	}.assemble(t)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if cfg.Plain != "from-block" {
		t.Errorf("Plain = %q, want the block's value: an untagged field has no flag or environment entry", cfg.Plain)
	}
}

// TestResolverComponentConfig_TypedAndTextValuesDecode pins the conversion
// split: an integer field's flag text is parsed at the loader (a malformed
// value names the field), while a duration field's text is carried verbatim
// and converted by the decode, exactly as a block-supplied value is.
func TestResolverComponentConfig_TypedAndTextValuesDecode(t *testing.T) {
	t.Parallel()

	cfg, err := resolverAssembly{
		args: []string{"--port=0x2a", "--ttl=90s", "--debug=true"},
	}.assemble(t)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if cfg.Port != 42 {
		t.Errorf("Port = %d, want the integer literal parsed as 42", cfg.Port)
	}
	if cfg.TTL != 90*time.Second {
		t.Errorf("TTL = %v, want 90s", cfg.TTL)
	}
	if !cfg.Debug {
		t.Errorf("Debug = %v, want the flag's true", cfg.Debug)
	}

	_, err = resolverAssembly{args: []string{"--port=not-a-number"}}.assemble(t)
	if err == nil || !strings.Contains(err.Error(), "port") {
		t.Errorf("assemble error = %v, want the malformed integer named by its key path", err)
	}
}

// TestResolverComponentConfig_AssemblyChecksStillRun pins the division of
// labour with the root package: the resolver merges sources, and the
// assembly's own Prepare stage still refuses a sensitive field without its
// documentation before anything is constructed.
func TestResolverComponentConfig_AssemblyChecksStillRun(t *testing.T) {
	t.Parallel()

	type undocumented struct {
		Token string `json:"token" config:"sensitive"`
	}
	component := resolverComponent(resolverName)
	component.ConfigSchema = (*undocumented)(nil)
	_, err := resolverAssembly{component: component}.run(t)
	if !errors.Is(err, pkgcore.ErrInvalidComponent) {
		t.Fatalf("assemble error = %v, want ErrInvalidComponent", err)
	}
	if !strings.Contains(err.Error(), "sensitive") {
		t.Errorf("error %q does not name the sensitive pairing", err)
	}
}

// plainResolverSchema declares no source-opening option at all: the shape
// every component in the tree carries today, which the resolver must pass
// through unchanged.
type plainResolverSchema struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// TestResolverComponentConfig_NoDeclarationsKeepTheBlock pins the
// behaviour-preserving path: a schema whose fields declare nothing resolves
// nothing, and the resolver returns the block it was handed -- same keys,
// same order, same values -- so a component that never opted in cannot
// notice the resolver exists.
func TestResolverComponentConfig_NoDeclarationsKeepTheBlock(t *testing.T) {
	t.Parallel()

	reg := pkgcore.NewComponentRegistry()
	component := resolverComponent(resolverName)
	component.ConfigSchema = (*plainResolverSchema)(nil)
	if err := reg.Register(component); err != nil {
		t.Fatalf("register: %v", err)
	}
	loader := pkgconfig.New(pkgconfig.WithArgs([]string{"--host=from-flag"}))
	resolver := newComponentConfigResolver(loader, reg)

	block := pkgcore.ComponentConfig{}.With("host", "from-block").With("port", 8080)
	got, err := resolver.ResolveComponentConfig(resolverName, (*plainResolverSchema)(nil), block)
	if err != nil {
		t.Fatalf("ResolveComponentConfig() error = %v, want nil", err)
	}
	if !slices.Equal(got.Keys(), block.Keys()) {
		t.Fatalf("keys = %v, want the block's own %v", got.Keys(), block.Keys())
	}
	for _, key := range block.Keys() {
		want, _ := block.Get(key)
		value, _ := got.Get(key)
		if !reflect.DeepEqual(value, want) {
			t.Errorf("%s = %#v, want the block's %#v", key, value, want)
		}
	}
}

// TestResolverComponentConfig_UnregisteredComponentIsRefused pins the
// resolver's own precondition: the namespace prefix comes from the
// registry's descriptors, and a name the registry does not carry has none.
func TestResolverComponentConfig_UnregisteredComponentIsRefused(t *testing.T) {
	t.Parallel()

	loader := pkgconfig.New(pkgconfig.WithArgs(nil))
	resolver := newComponentConfigResolver(loader, pkgcore.NewComponentRegistry())
	_, err := resolver.ResolveComponentConfig("ghost", (*resolverSchema)(nil), pkgcore.ComponentConfig{})
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("ResolveComponentConfig() error = %v, want the unregistered-component refusal naming it", err)
	}
}

// TestComponentConfigPrefix pins the namespace forms the resolver computes,
// mirroring pkgcore's own key-path rule (config_schema.go's
// configKeyPrefix): no prefix when the component declares none, and the
// declared namespace normalized to end with a dot otherwise.
func TestComponentConfigPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		namespace string
		want      string
	}{
		{name: "empty namespace", namespace: "", want: ""},
		{name: "custom without a dot", namespace: "platform.authn", want: "platform.authn."},
		{name: "custom with a dot", namespace: "platform.authn.", want: "platform.authn."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reg := pkgcore.NewComponentRegistry()
			if err := reg.Register(pkgcore.Component{
				Name:            "namespaced",
				ConfigNamespace: tc.namespace,
				New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
					return &testMarker{name: "namespaced"}, nil
				},
			}); err != nil {
				t.Fatalf("register: %v", err)
			}
			got, err := componentConfigPrefix(reg, "namespaced")
			if err != nil {
				t.Fatalf("componentConfigPrefix() error = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("componentConfigPrefix() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSetAndRemoveConfigKey pin the block-editing helpers: a value lands at
// its nested local path, an entry already present is replaced in the tree's
// own spelling and position, and a removal drops the entry -- with the
// parent a removal empties.
func TestSetAndRemoveConfigKey(t *testing.T) {
	t.Parallel()

	tree := pkgcore.ComponentConfig{}.
		With("Host", "old").
		With("backend", pkgcore.NewComponentConfig(map[string]any{"addr": "keep-me", "port": 1})).
		With("tail", true)

	updated := setConfigKey(tree, "backend.port", 9090)
	if port, ok := nestedValue(t, updated, "backend")["port"]; !ok || port != 9090 {
		t.Errorf("backend.port = %#v, want 9090", port)
	}

	// The existing entry keeps its own spelling: the tree's "Host" is
	// addressed case-insensitively and not duplicated.
	replaced := setConfigKey(updated, "host", "new")
	if !slices.Equal(replaced.Keys(), []string{"Host", "backend", "tail"}) {
		t.Errorf("keys = %v, want the case-insensitive replacement in place", replaced.Keys())
	}

	trimmed := removeConfigKey(replaced, "backend.addr")
	if backend, ok := trimmed.Get("backend"); !ok {
		t.Fatal("backend is gone, want the entry that was not removed")
	} else if nested, isMapping := asMapValue(backend); !isMapping || !slices.Equal(nested.Keys(), []string{"port"}) {
		t.Errorf("backend = %#v, want only port left", backend)
	}

	emptied := removeConfigKey(replaced, "backend.port")
	emptied = removeConfigKey(emptied, "backend.addr")
	if _, ok := emptied.Get("backend"); ok {
		t.Errorf("backend = %#v, want the emptied parent dropped", emptied.Keys())
	}

	if unchanged := removeConfigKey(replaced, "absent.key"); !slices.Equal(unchanged.Keys(), replaced.Keys()) {
		t.Errorf("keys = %v, want a removal of an absent path to change nothing", unchanged.Keys())
	}

	// A segment carrying a non-mapping value is not a subtree to edit: the
	// removal leaves it alone rather than replacing it with a mapping.
	if untouched := removeConfigKey(replaced, "tail.nested"); !slices.Equal(untouched.Keys(), replaced.Keys()) {
		t.Errorf("keys = %v, want a removal under a scalar segment to change nothing", untouched.Keys())
	}
}

// TestDeclarationFormat pins the format a field's explicit values take: key
// material resolves as hexkey, a bool or a signed integer takes its own
// conversion, and every other type -- a duration included -- is carried
// verbatim for the decode to convert.
func TestDeclarationFormat(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		field pkgconfig.FieldSummary
		want  string
	}{
		{name: "key material", field: pkgconfig.FieldSummary{Derive: true, Type: reflect.TypeOf([]byte(nil))}, want: pkgconfig.FormatHexKey},
		{name: "no type", field: pkgconfig.FieldSummary{}, want: pkgconfig.FormatString},
		{name: "bool", field: pkgconfig.FieldSummary{Type: reflect.TypeOf(false)}, want: pkgconfig.FormatBool},
		{name: "signed integer", field: pkgconfig.FieldSummary{Type: reflect.TypeOf(int32(0))}, want: pkgconfig.FormatInt},
		{name: "duration", field: pkgconfig.FieldSummary{Type: reflect.TypeOf(time.Duration(0))}, want: pkgconfig.FormatString},
		{name: "unsigned integer", field: pkgconfig.FieldSummary{Type: reflect.TypeOf(uint(0))}, want: pkgconfig.FormatString},
		{name: "float", field: pkgconfig.FieldSummary{Type: reflect.TypeOf(1.5)}, want: pkgconfig.FormatString},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := declarationFormat(tc.field); got != tc.want {
				t.Errorf("declarationFormat(%+v) = %q, want %q", tc.field, got, tc.want)
			}
		})
	}
}

// TestResolverComponentConfig_MalformedSchemaIsRefused pins the resolver's
// error face before any source is read: a schema the projection refuses
// comes back from the resolver, wrapping the projection's sentinel.
func TestResolverComponentConfig_MalformedSchemaIsRefused(t *testing.T) {
	t.Parallel()

	type malformed struct {
		Field string `json:"field" config:"frobnicate"`
	}
	resolver := newComponentConfigResolver(pkgconfig.New(pkgconfig.WithArgs(nil)), pkgcore.NewComponentRegistry())
	_, err := resolver.ResolveComponentConfig(resolverName, (*malformed)(nil), pkgcore.ComponentConfig{})
	if !errors.Is(err, pkgconfig.ErrInvalidTarget) {
		t.Fatalf("ResolveComponentConfig() error = %v, want ErrInvalidTarget", err)
	}
}

// nestedValue reads a nested treed value for the assertions above.
func nestedValue(t *testing.T, tree pkgcore.ComponentConfig, key string) map[string]any {
	t.Helper()
	raw, ok := tree.Get(key)
	if !ok {
		t.Fatalf("%s is absent from the tree", key)
	}
	nested, ok := asMapValue(raw)
	if !ok {
		t.Fatalf("%s = %#v, want a mapping", key, raw)
	}
	out := make(map[string]any, nested.Len())
	for _, k := range nested.Keys() {
		out[k], _ = nested.Get(k)
	}
	return out
}

// asMapValue views a raw value as a ComponentConfig mapping.
func asMapValue(raw any) (pkgcore.ComponentConfig, bool) {
	switch v := raw.(type) {
	case pkgcore.ComponentConfig:
		return v, true
	default:
		return pkgcore.ComponentConfig{}, false
	}
}

// TestLoad_PublishesTheComponentConfigResolver pins the engine's wiring: the
// loader publishes its resolver into the registry before the Prepare stage,
// and a resolver the registry already carries -- a host's own -- stays the
// one in use instead of being joined by a second one.
func TestLoad_PublishesTheComponentConfigResolver(t *testing.T) {
	t.Parallel()

	t.Run("the loader publishes its resolver", func(t *testing.T) {
		t.Parallel()

		var host testHostConfig
		reg := pkgcore.NewComponentRegistry()
		if err := Load(context.Background(), reg, LoadSpec{Host: &host, Options: testConfigOptions(), Args: []string{}}); err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		resolver, present, err := pkgcore.GetOptional[pkgcore.ComponentConfigResolver](reg)
		if err != nil || !present {
			t.Fatalf("GetOptional() = %v, %v, %v, want the engine's resolver", resolver, present, err)
		}
		if _, ok := resolver.(*componentConfigResolver); !ok {
			t.Errorf("resolver = %T, want the engine's own implementation", resolver)
		}
	})

	t.Run("a host's own resolver stays the one in use", func(t *testing.T) {
		t.Parallel()

		hostResolver := &hostConfigResolver{}
		var host testHostConfig
		reg := pkgcore.NewComponentRegistry()
		reg.Put(pkgcore.ComponentConfigResolver(hostResolver))
		if err := Load(context.Background(), reg, LoadSpec{Host: &host, Options: testConfigOptions(), Args: []string{}}); err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		resolver, present, err := pkgcore.GetOptional[pkgcore.ComponentConfigResolver](reg)
		if err != nil || !present {
			t.Fatalf("GetOptional() = %v, %v, %v, want the host's resolver", resolver, present, err)
		}
		if resolver != pkgcore.ComponentConfigResolver(hostResolver) {
			t.Errorf("resolver = %v, want the host's own", resolver)
		}
	})

	t.Run("two host resolvers fail the load", func(t *testing.T) {
		t.Parallel()

		var host testHostConfig
		reg := pkgcore.NewComponentRegistry()
		reg.Put(pkgcore.ComponentConfigResolver(&hostConfigResolver{}))
		reg.Put(pkgcore.ComponentConfigResolver(&hostConfigResolver{}))
		err := Load(context.Background(), reg, LoadSpec{Host: &host, Options: testConfigOptions(), Args: []string{}})
		if err == nil || !strings.Contains(err.Error(), "component configuration resolver") {
			t.Fatalf("Load() error = %v, want the read of two resolvers to fail naming them", err)
		}
	})
}

// hostConfigResolver is a host-supplied resolver that answers the block it
// was handed.
type hostConfigResolver struct{}

func (*hostConfigResolver) ResolveComponentConfig(_ string, _ any, fileConfig pkgcore.ComponentConfig) (pkgcore.ComponentConfig, error) {
	return fileConfig, nil
}

// TestRunAssembly_ResolvesComponentConfiguration pins the wiring end to end
// through the engine's Run sugar: the component flag is resolved before New
// runs, and the serve step reads the value the component actually decoded.
func TestRunAssembly_ResolvesComponentConfiguration(t *testing.T) {
	var host testHostConfig
	spec := driverLoadSpec(t, &host)
	spec.Options = append(spec.Options, ConfigArgs([]string{"--host=from-flag", "--port=7070"}))
	spec.Args = []string{}
	spec.Overrides = &CompositionOverrides{Config: pkgcore.ComponentConfig{}.With("components",
		pkgcore.ComponentConfig{}.With(resolverName, nil).With("observability", false))}

	var seen resolverSchema
	serve := func(_ context.Context, reg *pkgcore.ComponentRegistry) error {
		product, err := pkgcore.Get[*resolverProduct](reg)
		if err != nil {
			return err
		}
		seen = product.cfg
		return nil
	}

	if err := RunAssembly(context.Background(), spec, serve, resolverComponent(resolverName)); err != nil {
		t.Fatalf("RunAssembly() error = %v", err)
	}
	if seen.Host != "from-flag" || seen.Port != 7070 {
		t.Errorf("served configuration = %+v, want the flag-resolved host and port", seen)
	}
}

// driftSchema is the schema the alignment check describes through both
// projections.
type driftSchema struct {
	Addr     string `json:"addr" config:"expose,env=DRIFT_ADDR,group=network"`
	Key      []byte `json:"key" config:"derive,required,sensitive,group=security"`
	CacheTTL int    `json:"cache_ttl"`
	Endpoint struct {
		URL string `json:"url" config:"expose"`
	} `json:"endpoint"`
	Hidden string `json:"hidden" config:"-"`
}

// TestResolverComponentConfig_KeyPathsMatchTheRootProjection pins the two
// halves of the key-path rule against each other over one schema: the prefix
// the engine's resolver computes for a registered component and the prefix
// pkgcore's own projection applies to the same descriptors must produce the
// same key paths, with the same options -- a drift in either the prefix rule
// or the field vocabulary fails here rather than at a boot.
func TestResolverComponentConfig_KeyPathsMatchTheRootProjection(t *testing.T) {
	const name = "cfgtest.drift"
	schema := (*driftSchema)(nil)
	if err := pkgcore.Register(pkgcore.Component{
		Name:            name,
		ConfigNamespace: "cfgtest.ns",
		ConfigSchema:    schema,
		New: func(context.Context, *pkgcore.ComponentRegistry, pkgcore.ComponentConfig) (any, error) {
			return &testMarker{name: name}, nil
		},
	}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}

	reg := pkgcore.NewComponentRegistry()
	prefix, err := componentConfigPrefix(reg, name)
	if err != nil {
		t.Fatalf("componentConfigPrefix() error = %v, want nil", err)
	}
	rootDescriptors, err := pkgcore.DescribeComponentSchema(name, schema)
	if err != nil {
		t.Fatalf("DescribeComponentSchema() error = %v, want nil", err)
	}
	summaries, err := pkgconfig.Describe(schema)
	if err != nil {
		t.Fatalf("Describe() error = %v, want nil", err)
	}

	var resolvable []pkgconfig.FieldSummary
	for _, s := range summaries {
		if !s.Skip {
			resolvable = append(resolvable, s)
		}
	}
	if len(rootDescriptors) != len(resolvable) {
		t.Fatalf("root projects %d fields, config projects %d", len(rootDescriptors), len(resolvable))
	}
	for i, descriptor := range rootDescriptors {
		summary := resolvable[i]
		if want := prefix + summary.Key; descriptor.Key != want {
			t.Errorf("root key %q, want the resolver's namespace prefix over %q", descriptor.Key, want)
		}
		if descriptor.Type != summary.TypeName() {
			t.Errorf("%s type %q, want the config projection's %q", descriptor.Key, descriptor.Type, summary.TypeName())
		}
		if descriptor.Expose != summary.Expose || descriptor.Derive != summary.Derive || descriptor.Required != summary.Required ||
			descriptor.Sensitive != summary.Sensitive || descriptor.Env != summary.Env || descriptor.Group != summary.Group {
			t.Errorf("%s options differ: root %+v, config %+v", descriptor.Key, descriptor, summary)
		}
	}
}
