package pkgcore_test

import (
	"context"
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
)

// exampleListerConfig is the schema the examples describe and resolve. Its
// fields carry the whole declaration vocabulary: an exposed field with a
// pinned environment variable and a documentation group, a derive-tagged
// key-material field, and a plain field supplied by the configuration block
// alone.
type exampleListerConfig struct {
	Addr      string `json:"addr" config:"expose,env=APP_LISTEN_ADDR,group=network"`
	CipherKey []byte `json:"cipher_key" config:"derive,required,sensitive,group=security"`
	CacheTTL  int    `json:"cache_ttl" config:"group=tuning"`
}

func (exampleListerConfig) ConfigDocs() map[string]pkgcore.FieldDoc {
	return map[string]pkgcore.FieldDoc{
		"addr":       {Description: "the address the lister binds", Default: "127.0.0.1:8080", Example: "0.0.0.0:8080"},
		"cipher_key": {Description: "32 bytes of key material the lister seals its cache with"},
		"cache_ttl":  {Description: "seconds a listing stays cached", Default: "0 (no caching)"},
	}
}

// ExampleDescribeComponentSchema describes a component's configuration
// declaration the way a --help listing or the generated configuration
// reference reads it: one descriptor per field, each carrying the field's
// final key path, how it resolves, and its ConfigDocs entry.
func ExampleDescribeComponentSchema() {
	descriptors, err := pkgcore.DescribeComponentSchema("lister", (*exampleListerConfig)(nil))
	if err != nil {
		fmt.Println("describe:", err)
		return
	}
	for _, d := range descriptors {
		fmt.Printf("%s type=%s expose=%t derive=%t required=%t sensitive=%t env=%q group=%q\n",
			d.Key, d.Type, d.Expose, d.Derive, d.Required, d.Sensitive, d.Env, d.Group)
		fmt.Println("  " + d.Doc.Description)
	}

	// Output:
	// components.lister.addr type=string expose=true derive=false required=false sensitive=false env="APP_LISTEN_ADDR" group="network"
	//   the address the lister binds
	// components.lister.cipher_key type=[]byte expose=true derive=true required=true sensitive=true env="" group="security"
	//   32 bytes of key material the lister seals its cache with
	// components.lister.cache_ttl type=int expose=false derive=false required=false sensitive=false env="" group="tuning"
	//   seconds a listing stays cached
}

// exampleResolver is the ComponentConfigResolver an assembling engine puts
// into the registry. A real engine merges the five sources -- command-line
// flags over environment variables over an optional config file over the
// root-key derivation over declared defaults -- and returns the merged
// block; this stand-in states one merge result: the listen address a
// higher-priority source supplies, and the derived key material a required
// derive-tagged field resolves to.
type exampleResolver struct{}

func (exampleResolver) ResolveComponentConfig(_ string, _ any, fileConfig pkgcore.ComponentConfig) (pkgcore.ComponentConfig, error) {
	return fileConfig.
		With("addr", "0.0.0.0:9090").
		With("cipher_key", []byte("0123456789abcdef0123456789abcdef")), nil
}

// ExampleComponentConfigResolver assembles one component through the
// resolver seam: the composition carries only the file-level block, the
// resolver's merged result replaces it, and New receives the final values by
// decoding the block as always -- the required key-material field is
// satisfied by the derivation's output, so the assembly passes its
// declaration checks.
func ExampleComponentConfigResolver() {
	ctx := context.Background()
	type lister struct {
		addr      string
		keyLength int
	}

	reg := pkgcore.NewComponentRegistry()
	if err := reg.Register(pkgcore.Component{
		Name:         "example.lister",
		ConfigSchema: (*exampleListerConfig)(nil),
		New: func(_ context.Context, _ *pkgcore.ComponentRegistry, cfg pkgcore.ComponentConfig) (any, error) {
			var c exampleListerConfig
			if err := cfg.Decode(&c); err != nil {
				return nil, err
			}
			return &lister{addr: c.Addr, keyLength: len(c.CipherKey)}, nil
		},
	}); err != nil {
		fmt.Println("register:", err)
		return
	}

	reg.Put(exampleResolver{})
	reg.Put(pkgcore.NewComponentConfig(map[string]any{
		"components": map[string]any{
			"example.lister": map[string]any{"addr": "127.0.0.1:8080"},
		},
	}))

	if err := reg.Prepare(ctx); err != nil {
		fmt.Println("prepare:", err)
		return
	}
	if err := reg.Construct(ctx); err != nil {
		fmt.Println("construct:", err)
		return
	}

	built, err := pkgcore.Get[*lister](reg)
	if err != nil {
		fmt.Println("get:", err)
		return
	}
	fmt.Println("lister bound to", built.addr)
	fmt.Println("key material bytes:", built.keyLength)

	// Output:
	// lister bound to 0.0.0.0:9090
	// key material bytes: 32
}
