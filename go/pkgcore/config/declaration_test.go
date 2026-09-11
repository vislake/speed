package config

import (
	"bytes"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// declarationMirror is the struct the declaration-path tests resolve the same
// inputs against: its field key paths, types and derive tag are the exact
// reflection-path spelling of declarationKeys below, so one set of sources
// driven through both paths must produce identical results.
type declarationMirror struct {
	Server struct {
		Addr  string
		Port  int
		Debug bool
	}
	Key struct {
		Cipher []byte `config:"derive"`
	}
}

func newDeclarationMirror() *declarationMirror {
	cfg := &declarationMirror{}
	cfg.Key.Cipher = testDevKey
	return cfg
}

// declarationKeys is the declaration spelling of declarationMirror: the same
// key paths, the same formats.
var declarationKeys = []Declaration{
	{Key: "server.addr", Format: FormatString},
	{Key: "server.port", Format: FormatInt},
	{Key: "server.debug", Format: FormatBool},
	{Key: "key.cipher", Format: FormatHexKey},
}

// declarationDefaults is the declared defaults table mirroring the struct
// default newDeclarationMirror pre-fills.
func declarationDefaults() map[string][]byte {
	return map[string][]byte{"key.cipher": testDevKey}
}

// resolveDeclarationMirror runs opts through both paths -- Load over the
// mirrored struct, ResolveDeclarations over the mirrored declarations -- and
// returns both results for comparison.
func resolveDeclarationMirror(t *testing.T, opts []Option) (*declarationMirror, map[string]any) {
	t.Helper()

	cfg := newDeclarationMirror()
	if err := New(opts...).Load(cfg); err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	values, err := New(opts...).ResolveDeclarations(declarationKeys)
	if err != nil {
		t.Fatalf("ResolveDeclarations() error = %v, want nil", err)
	}
	return cfg, values
}

// assertMirrorMatchesDeclaration compares one declaration-path result against
// the reflection-path struct field by field, so a divergence in value or in
// shape fails with the key that diverged. A text key the declaration path
// left out of its result is the absent state a struct field holds by staying
// at its zero value -- the one output-shape difference the two paths have by
// construction -- so absence is compared against the zero value rather than
// treated as a mismatch.
func assertMirrorMatchesDeclaration(t *testing.T, cfg *declarationMirror, values map[string]any) {
	t.Helper()

	if got, present := values["server.addr"]; present {
		if got != cfg.Server.Addr {
			t.Errorf("server.addr = %#v, want the struct's %q", got, cfg.Server.Addr)
		}
	} else if cfg.Server.Addr != "" {
		t.Errorf("server.addr is absent from the result, but the struct holds %q", cfg.Server.Addr)
	}
	if got, present := values["server.port"]; present {
		if got != cfg.Server.Port {
			t.Errorf("server.port = %#v, want the struct's %d", got, cfg.Server.Port)
		}
	} else if cfg.Server.Port != 0 {
		t.Errorf("server.port is absent from the result, but the struct holds %d", cfg.Server.Port)
	}
	if got, present := values["server.debug"]; present {
		if got != cfg.Server.Debug {
			t.Errorf("server.debug = %#v, want the struct's %v", got, cfg.Server.Debug)
		}
	} else if cfg.Server.Debug {
		t.Errorf("server.debug is absent from the result, but the struct holds true")
	}
	got, ok := values["key.cipher"].([]byte)
	if !ok {
		t.Fatalf("key.cipher = %#v, want []byte", values["key.cipher"])
	}
	if !bytes.Equal(got, cfg.Key.Cipher) {
		t.Errorf("key.cipher = %s, want the struct's %s", hex.EncodeToString(got), hex.EncodeToString(cfg.Key.Cipher))
	}
}

// TestResolveDeclarations_MatchesTheReflectionPath drives one source set at a
// time through both paths and asserts identical results: the text conversion
// rules, the key-material three-stage order and the precedence between them
// are the same functions, so the two paths cannot drift.
func TestResolveDeclarations_MatchesTheReflectionPath(t *testing.T) {
	t.Parallel()

	rootKey := bytes.Repeat([]byte{0x5a}, 32)
	cipherHex := strings.Repeat("3c", 32)
	file := writeConfigFile(t, "server:\n  addr: from-file\nkey:\n  cipher: "+cipherHex+"\n")

	tests := []struct {
		name string
		opts []Option
	}{
		{
			name: "no source at all: the fallback stands on both paths",
			opts: []Option{WithArgs(nil), WithEnviron(nil)},
		},
		{
			name: "text values from the environment",
			opts: []Option{WithArgs(nil), WithEnviron([]string{
				"SPEED_SERVER__ADDR=from-env",
				"SPEED_SERVER__PORT=0x2a",
				"SPEED_SERVER__DEBUG=true",
			})},
		},
		{
			name: "text values from flags over the environment",
			opts: []Option{
				WithArgs([]string{"--server.addr=from-flag", "--server.port=7"}),
				WithEnviron([]string{"SPEED_SERVER__ADDR=from-env"}),
			},
		},
		{
			name: "the config file supplies text and key material",
			opts: []Option{WithArgs(nil), WithEnviron(nil), WithConfigFile(file)},
		},
		{
			name: "the derivation beats the fallback",
			opts: []Option{
				WithArgs(nil),
				WithEnviron(nil),
				WithRootKey(rootKey),
				WithKeyDerivation((&recordingDeriver{}).derive),
			},
		},
		{
			name: "an explicit value beats the derivation and the fallback",
			opts: []Option{
				WithArgs(nil),
				WithEnviron([]string{"SPEED_KEY__CIPHER=" + cipherHex, "SPEED_SERVER__PORT=99"}),
				WithRootKey(rootKey),
				WithKeyDerivation((&recordingDeriver{}).derive),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// The declared defaults table mirrors the struct default
			// newDeclarationMirror pre-fills, so both paths carry the same
			// lowest-priority source in every case.
			opts := append([]Option{WithDevDefaults(declarationDefaults())}, tt.opts...)
			cfg, values := resolveDeclarationMirror(t, opts)
			assertMirrorMatchesDeclaration(t, cfg, values)
		})
	}
}

// TestResolveDeclarations_AbsentKeyStaysOutOfTheMap pins the one output-shape
// difference the declaration path has by construction: a key no source
// supplied and no table entry stands for is simply absent, while the struct
// path's field keeps the value its caller pre-filled.
func TestResolveDeclarations_AbsentKeyStaysOutOfTheMap(t *testing.T) {
	t.Parallel()

	values, err := New(WithArgs(nil), WithEnviron(nil)).ResolveDeclarations(declarationKeys)
	if err != nil {
		t.Fatalf("ResolveDeclarations() error = %v, want nil", err)
	}
	for _, key := range []string{"server.addr", "server.port", "server.debug", "key.cipher"} {
		if _, present := values[key]; present {
			t.Errorf("ResolveDeclarations() returned %q, want the key absent: no source supplied it", key)
		}
	}
	if len(values) != 0 {
		t.Errorf("ResolveDeclarations() = %#v, want an empty map", values)
	}
}

// TestResolveDeclarations_EmptyValuesReadAsUnset pins the emptied-value rule
// on the declaration path: an emptied variable for a hexkey key neither
// overrides the table nor the derivation, and an emptied variable for a
// string key is an honest override, exactly as the reflection path judges
// them.
func TestResolveDeclarations_EmptyValuesReadAsUnset(t *testing.T) {
	t.Parallel()

	rootKey := bytes.Repeat([]byte{0x5a}, 32)

	values, err := New(
		WithArgs(nil),
		WithEnviron([]string{"SPEED_KEY__CIPHER=", "SPEED_SERVER__ADDR="}),
		WithRootKey(rootKey),
		WithKeyDerivation((&recordingDeriver{}).derive),
		WithDevDefaults(declarationDefaults()),
	).ResolveDeclarations(declarationKeys)
	if err != nil {
		t.Fatalf("ResolveDeclarations() error = %v, want nil", err)
	}
	got, ok := values["key.cipher"].([]byte)
	if !ok {
		t.Fatalf("key.cipher = %#v, want the derivation's bytes", values["key.cipher"])
	}
	want, _ := (&recordingDeriver{}).derive(rootKey, "key.cipher")
	if !bytes.Equal(got, want) {
		t.Errorf("key.cipher = %s, want the derivation's %s", hex.EncodeToString(got), hex.EncodeToString(want))
	}
	if addr := values["server.addr"]; addr != "" {
		t.Errorf("server.addr = %#v, want the emptied override \"\"", addr)
	}
}

// TestResolveDeclarations_FailuresMatchTheReflectionPath pins that a value
// the loader refuses is refused the same way on both paths: the same
// sentinel, the same named key, the same consulted-source list up to its
// last entry, which names the schema's own lowest-priority source.
func TestResolveDeclarations_FailuresMatchTheReflectionPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		opts []Option
		// key is the declared key the refusal must name; empty for refusals
		// that precede any key's resolution.
		key string
		// namesSources marks refusals that report the consulted-source list.
		namesSources bool
		chain        error
	}{
		{
			name:         "a malformed int",
			opts:         []Option{WithArgs(nil), WithEnviron([]string{"SPEED_SERVER__PORT=not-a-number"})},
			key:          "server.port",
			namesSources: true,
			chain:        ErrInvalidValue,
		},
		{
			name:         "a malformed bool",
			opts:         []Option{WithArgs(nil), WithEnviron([]string{"SPEED_SERVER__DEBUG=perhaps"})},
			key:          "server.debug",
			namesSources: true,
			chain:        ErrInvalidValue,
		},
		{
			name:         "a key-material value of the wrong length",
			opts:         []Option{WithArgs(nil), WithEnviron([]string{"SPEED_KEY__CIPHER=0011"})},
			key:          "key.cipher",
			namesSources: true,
			chain:        ErrInvalidValue,
		},
		{
			name:  "a root key of the wrong size",
			opts:  []Option{WithArgs(nil), WithEnviron(nil), WithRootKey([]byte("short"))},
			chain: ErrInvalidRootKey,
		},
		{
			name:  "an unreadable config file",
			opts:  []Option{WithArgs(nil), WithEnviron(nil), WithConfigFile(t.TempDir())},
			chain: ErrSourceUnreadable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			structErr := New(tt.opts...).Load(newDeclarationMirror())
			if structErr == nil {
				t.Fatalf("Load() error = nil, want a refusal")
			}
			if !errors.Is(structErr, tt.chain) {
				t.Fatalf("Load() error = %v, want it to wrap %v", structErr, tt.chain)
			}

			_, declErr := New(tt.opts...).ResolveDeclarations(declarationKeys)
			if declErr == nil {
				t.Fatalf("ResolveDeclarations() error = nil, want a refusal")
			}
			if !errors.Is(declErr, tt.chain) {
				t.Errorf("ResolveDeclarations() error = %v, want it to wrap %v", declErr, tt.chain)
			}
			if tt.key != "" && !strings.Contains(declErr.Error(), tt.key) {
				t.Errorf("ResolveDeclarations() error = %v, want it to name the key %q", declErr, tt.key)
			}
			if tt.namesSources {
				if !strings.Contains(declErr.Error(), declaredDefaultsSource) {
					t.Errorf("ResolveDeclarations() error = %v, want the consulted sources to end in %q", declErr, declaredDefaultsSource)
				}
				if strings.Contains(declErr.Error(), "the default set on the target struct") {
					t.Errorf("ResolveDeclarations() error = %v, must not claim a target struct", declErr)
				}
			}
		})
	}
}

// TestResolveDeclarations_RootKeyWithoutADeriverIsAWiringError pins that the
// derivation-wiring precondition is the reflection path's own, judging the
// declaration's derives the way it judges a struct's derive-tagged fields.
func TestResolveDeclarations_RootKeyWithoutADeriverIsAWiringError(t *testing.T) {
	t.Parallel()

	_, err := New(
		WithArgs(nil),
		WithEnviron(nil),
		WithRootKey(bytes.Repeat([]byte{0x5a}, 32)),
	).ResolveDeclarations(declarationKeys)
	if !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("ResolveDeclarations() error = %v, want ErrInvalidTarget", err)
	}

	// Declarations with no hexkey carry no derivation and accept a root key.
	_, err = New(
		WithArgs(nil),
		WithEnviron(nil),
		WithRootKey(bytes.Repeat([]byte{0x5a}, 32)),
	).ResolveDeclarations(declarationKeys[:3])
	if err != nil {
		t.Errorf("ResolveDeclarations() error = %v, want nil for a set with no hexkey declaration", err)
	}
}

// TestResolveDeclarations_UnknownFormatIsRefused pins the closed-set check.
func TestResolveDeclarations_UnknownFormatIsRefused(t *testing.T) {
	t.Parallel()

	_, err := New(WithArgs(nil), WithEnviron(nil)).ResolveDeclarations([]Declaration{
		{Key: "server.addr", Format: "duration"},
	})
	if !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("ResolveDeclarations() error = %v, want ErrInvalidTarget", err)
	}
	if !strings.Contains(err.Error(), "duration") {
		t.Errorf("ResolveDeclarations() error = %v, want it to name the offending format", err)
	}
}

// TestResolveDeclarations_KeyShapesAreValidated pins the path-shape checks:
// an empty path, an uppercase segment and an empty segment are all refused
// before any source is read, and a repeated key is refused as ambiguous.
func TestResolveDeclarations_KeyShapesAreValidated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		decls []Declaration
	}{
		{
			name:  "an empty key path",
			decls: []Declaration{{Key: "", Format: FormatString}},
		},
		{
			name:  "an uppercase segment",
			decls: []Declaration{{Key: "server.Addr", Format: FormatString}},
		},
		{
			name:  "an empty segment",
			decls: []Declaration{{Key: "server..addr", Format: FormatString}},
		},
		{
			name:  "a leading delimiter",
			decls: []Declaration{{Key: ".server.addr", Format: FormatString}},
		},
		{
			name: "a repeated key",
			decls: []Declaration{
				{Key: "server.addr", Format: FormatString},
				{Key: "server.addr", Format: FormatString},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := New(WithArgs(nil), WithEnviron(nil)).ResolveDeclarations(tt.decls)
			if !errors.Is(err, ErrInvalidTarget) {
				t.Errorf("ResolveDeclarations() error = %v, want ErrInvalidTarget", err)
			}
		})
	}
}

// TestResolveDeclarations_DeclaredDefaultsTableRules pins the table's own
// contract: its entries serve hexkey declarations alone, an entry for a
// declared key of another format is a wiring error, an entry no declaration
// names is inert (one table spans a platform whose binaries carry different
// module subsets), and a value of the wrong size is refused naming the key.
func TestResolveDeclarations_DeclaredDefaultsTableRules(t *testing.T) {
	t.Parallel()

	t.Run("an entry for a declared non-hexkey key is refused", func(t *testing.T) {
		t.Parallel()

		_, err := New(WithArgs(nil), WithEnviron(nil), WithDevDefaults(map[string][]byte{
			"server.addr": []byte("not a key"),
		})).ResolveDeclarations(declarationKeys)
		if !errors.Is(err, ErrInvalidTarget) {
			t.Fatalf("ResolveDeclarations() error = %v, want ErrInvalidTarget", err)
		}
		if !strings.Contains(err.Error(), "server.addr") || !strings.Contains(err.Error(), FormatString) {
			t.Errorf("ResolveDeclarations() error = %v, want it to name the key and its declared format", err)
		}
	})

	t.Run("an entry no declaration names is inert", func(t *testing.T) {
		t.Parallel()

		values, err := New(WithArgs(nil), WithEnviron(nil), WithDevDefaults(map[string][]byte{
			"org.invitation_email_index_key": testDevKey,
			"key.cipher":                     testDevKey,
		})).ResolveDeclarations(declarationKeys)
		if err != nil {
			t.Fatalf("ResolveDeclarations() error = %v, want nil", err)
		}
		if _, present := values["org.invitation_email_index_key"]; present {
			t.Errorf("ResolveDeclarations() resolved an undeclared table entry; want it inert")
		}
		if _, present := values["key.cipher"]; !present {
			t.Errorf("ResolveDeclarations() left the declared key unresolved, want the table entry standing")
		}
	})

	t.Run("a table value of the wrong size is refused", func(t *testing.T) {
		t.Parallel()

		_, err := New(WithArgs(nil), WithEnviron(nil), WithDevDefaults(map[string][]byte{
			"key.cipher": []byte("short"),
		})).ResolveDeclarations(declarationKeys)
		if !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("ResolveDeclarations() error = %v, want ErrInvalidValue", err)
		}
		if !strings.Contains(err.Error(), "key.cipher") {
			t.Errorf("ResolveDeclarations() error = %v, want it to name the key", err)
		}
	})

	t.Run("the table shares no buffer with the result", func(t *testing.T) {
		t.Parallel()

		table := map[string][]byte{"key.cipher": append([]byte(nil), testDevKey...)}
		values, err := New(WithArgs(nil), WithEnviron(nil), WithDevDefaults(table)).ResolveDeclarations(declarationKeys)
		if err != nil {
			t.Fatalf("ResolveDeclarations() error = %v, want nil", err)
		}
		material := values["key.cipher"].([]byte)
		material[0] ^= 0xff
		if !bytes.Equal(table["key.cipher"], testDevKey) {
			t.Errorf("mutating the result changed the table's own bytes; want a copy")
		}
	})
}

// TestResolveDeclarations_TableHasNoEffectOnLoad pins the option's boundary:
// the declared defaults table is consumed by the declaration path alone, and
// a struct-backed load neither reads it nor grows a default from it.
func TestResolveDeclarations_TableHasNoEffectOnLoad(t *testing.T) {
	t.Parallel()

	cfg := &declarationMirror{}
	if err := New(WithArgs(nil), WithEnviron(nil), WithDevDefaults(declarationDefaults())).Load(cfg); err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.Key.Cipher != nil {
		t.Errorf("Key.Cipher = %x, want nil: the table must not feed a struct-backed load", cfg.Key.Cipher)
	}
}

// TestResolveDeclarations_EmptySetResolvesToAnEmptyMap pins the degenerate
// shape the engine's no-declared-key assembly takes.
func TestResolveDeclarations_EmptySetResolvesToAnEmptyMap(t *testing.T) {
	t.Parallel()

	values, err := New(WithArgs(nil), WithEnviron(nil)).ResolveDeclarations(nil)
	if err != nil {
		t.Fatalf("ResolveDeclarations() error = %v, want nil", err)
	}
	if len(values) != 0 {
		t.Errorf("ResolveDeclarations(nil) = %#v, want an empty map", values)
	}
}

// TestResolveDeclarations_OutputShapesMatchTheFormat pins the concrete Go
// shape each format resolves to, so a consumer's type assertion is
// contract not coincidence.
func TestResolveDeclarations_OutputShapesMatchTheFormat(t *testing.T) {
	t.Parallel()

	values, err := New(
		WithArgs(nil),
		WithEnviron([]string{
			"SPEED_SERVER__ADDR=addr",
			"SPEED_SERVER__PORT=8080",
			"SPEED_SERVER__DEBUG=true",
		}),
		WithDevDefaults(declarationDefaults()),
	).ResolveDeclarations(declarationKeys)
	if err != nil {
		t.Fatalf("ResolveDeclarations() error = %v, want nil", err)
	}

	want := map[string]any{
		"server.addr":  "addr",
		"server.port":  8080,
		"server.debug": true,
		"key.cipher":   testDevKey,
	}
	for key, wantValue := range want {
		if got := values[key]; !reflect.DeepEqual(got, wantValue) {
			t.Errorf("values[%q] = %#v, want %#v", key, got, wantValue)
		}
	}
}
