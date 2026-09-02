package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Struct defaults used as the lowest-priority source throughout these tests.
const (
	defaultDeploymentMode = "standalone"
	defaultDSN            = "dsn-from-struct-default"
	defaultPort           = 5432
	defaultTimeout        = 30 * time.Second
)

// Values each source contributes, chosen so that the winner is unambiguous.
const (
	fileDSN = "dsn-from-file"
	envDSN  = "dsn-from-env"
	flagDSN = "dsn-from-flag"
)

const configFileMode = 0o600

type poolConfig struct {
	MaxOpen int
}

type databaseConfig struct {
	DSN  string `config:"required"`
	Port int
	Pool poolConfig
}

type cacheConfig struct {
	Addr string
}

// CommonConfig is embedded to pin how embedded structs contribute key segments.
type CommonConfig struct {
	Region string
}

type testConfig struct {
	CommonConfig
	DeploymentMode string
	Debug          bool
	Timeout        time.Duration
	Database       databaseConfig
	Cache          *cacheConfig
	Labels         map[string]string
	Runtime        string `config:"-"`
}

func newTestConfig() *testConfig {
	return &testConfig{
		DeploymentMode: defaultDeploymentMode,
		Timeout:        defaultTimeout,
		Database: databaseConfig{
			DSN:  defaultDSN,
			Port: defaultPort,
		},
	}
}

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "speed.yaml")
	if err := os.WriteFile(path, []byte(body), configFileMode); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	return path
}

func TestLoad_NoSources_KeepsStructDefaults(t *testing.T) {
	t.Parallel()

	cfg := newTestConfig()
	if err := New(WithArgs(nil), WithEnviron(nil)).Load(cfg); err != nil {
		t.Fatalf("Load returned an unexpected error: %v", err)
	}

	if cfg.DeploymentMode != defaultDeploymentMode {
		t.Errorf("DeploymentMode = %q, want the struct default %q", cfg.DeploymentMode, defaultDeploymentMode)
	}
	if cfg.Timeout != defaultTimeout {
		t.Errorf("Timeout = %v, want the struct default %v", cfg.Timeout, defaultTimeout)
	}
	if cfg.Database.DSN != defaultDSN {
		t.Errorf("Database.DSN = %q, want the struct default %q", cfg.Database.DSN, defaultDSN)
	}
	if cfg.Database.Port != defaultPort {
		t.Errorf("Database.Port = %d, want the struct default %d", cfg.Database.Port, defaultPort)
	}
	// Fields with no default and no source must be left alone, not zeroed or allocated.
	if cfg.Database.Pool.MaxOpen != 0 {
		t.Errorf("Database.Pool.MaxOpen = %d, want 0", cfg.Database.Pool.MaxOpen)
	}
	if cfg.Cache != nil {
		t.Errorf("Cache = %+v, want nil when no source mentions it", cfg.Cache)
	}
}

func TestLoad_PriorityChain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		withFile bool
		withEnv  bool
		withFlag bool
		want     string
	}{
		{name: "no source falls back to the struct default", want: defaultDSN},
		{name: "the file beats the struct default", withFile: true, want: fileDSN},
		{name: "the environment beats the struct default", withEnv: true, want: envDSN},
		{name: "a flag beats the struct default", withFlag: true, want: flagDSN},
		{name: "the environment beats the file", withFile: true, withEnv: true, want: envDSN},
		{name: "a flag beats the file", withFile: true, withFlag: true, want: flagDSN},
		{name: "a flag beats the environment", withEnv: true, withFlag: true, want: flagDSN},
		{
			name:     "a flag beats the environment and the file together",
			withFile: true, withEnv: true, withFlag: true, want: flagDSN,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var args, environ []string
			if tt.withEnv {
				environ = []string{"SPEED_DATABASE__DSN=" + envDSN}
			}
			if tt.withFlag {
				args = []string{"--database.dsn=" + flagDSN}
			}
			opts := []Option{WithArgs(args), WithEnviron(environ)}
			if tt.withFile {
				opts = append(opts, WithConfigFile(writeConfigFile(t, "database:\n  dsn: "+fileDSN+"\n")))
			}

			cfg := newTestConfig()
			if err := New(opts...).Load(cfg); err != nil {
				t.Fatalf("Load returned an unexpected error: %v", err)
			}
			if cfg.Database.DSN != tt.want {
				t.Errorf("Database.DSN = %q, want %q", cfg.Database.DSN, tt.want)
			}
		})
	}
}

func TestLoad_KeyPathMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		env   []string
		args  []string
		file  string
		check func(t *testing.T, cfg *testConfig)
	}{
		{
			name: "two levels of nesting from the environment",
			env:  []string{"SPEED_DATABASE__DSN=nested-env-dsn"},
			check: func(t *testing.T, cfg *testConfig) {
				if cfg.Database.DSN != "nested-env-dsn" {
					t.Errorf("Database.DSN = %q, want %q", cfg.Database.DSN, "nested-env-dsn")
				}
			},
		},
		{
			name: "three levels of nesting from the environment",
			env:  []string{"SPEED_DATABASE__POOL__MAXOPEN=17"},
			check: func(t *testing.T, cfg *testConfig) {
				if cfg.Database.Pool.MaxOpen != 17 {
					t.Errorf("Database.Pool.MaxOpen = %d, want 17", cfg.Database.Pool.MaxOpen)
				}
			},
		},
		{
			name: "three levels of nesting from a flag",
			args: []string{"--database.pool.maxopen=23"},
			check: func(t *testing.T, cfg *testConfig) {
				if cfg.Database.Pool.MaxOpen != 23 {
					t.Errorf("Database.Pool.MaxOpen = %d, want 23", cfg.Database.Pool.MaxOpen)
				}
			},
		},
		{
			name: "three levels of nesting from the file",
			file: "database:\n  pool:\n    maxopen: 31\n",
			check: func(t *testing.T, cfg *testConfig) {
				if cfg.Database.Pool.MaxOpen != 31 {
					t.Errorf("Database.Pool.MaxOpen = %d, want 31", cfg.Database.Pool.MaxOpen)
				}
			},
		},
		{
			name: "a nested pointer struct is allocated on demand",
			env:  []string{"SPEED_CACHE__ADDR=localhost:6379"},
			check: func(t *testing.T, cfg *testConfig) {
				if cfg.Cache == nil {
					t.Fatal("Cache is nil, want it allocated by the loader")
				}
				if cfg.Cache.Addr != "localhost:6379" {
					t.Errorf("Cache.Addr = %q, want %q", cfg.Cache.Addr, "localhost:6379")
				}
			},
		},
		{
			name: "an embedded struct contributes its type name as a key segment",
			env:  []string{"SPEED_COMMONCONFIG__REGION=eu-west-1"},
			check: func(t *testing.T, cfg *testConfig) {
				if cfg.Region != "eu-west-1" {
					t.Errorf("Region = %q, want %q", cfg.Region, "eu-west-1")
				}
			},
		},
		{
			name: "the environment variable suffix is matched case-insensitively",
			env:  []string{"SPEED_database__DsN=case-insensitive-env"},
			check: func(t *testing.T, cfg *testConfig) {
				if cfg.Database.DSN != "case-insensitive-env" {
					t.Errorf("Database.DSN = %q, want %q", cfg.Database.DSN, "case-insensitive-env")
				}
			},
		},
		{
			name: "flag names are matched case-insensitively",
			args: []string{"--DATABASE.DSN=case-insensitive-flag"},
			check: func(t *testing.T, cfg *testConfig) {
				if cfg.Database.DSN != "case-insensitive-flag" {
					t.Errorf("Database.DSN = %q, want %q", cfg.Database.DSN, "case-insensitive-flag")
				}
			},
		},
		{
			name: "file keys are matched case-insensitively",
			file: "Database:\n  DSN: case-insensitive-file\n",
			check: func(t *testing.T, cfg *testConfig) {
				if cfg.Database.DSN != "case-insensitive-file" {
					t.Errorf("Database.DSN = %q, want %q", cfg.Database.DSN, "case-insensitive-file")
				}
			},
		},
		{
			name: "a single underscore is not a nesting separator",
			env:  []string{"SPEED_DATABASE_DSN=should-be-ignored"},
			check: func(t *testing.T, cfg *testConfig) {
				if cfg.Database.DSN != defaultDSN {
					t.Errorf("Database.DSN = %q, want the untouched default %q", cfg.Database.DSN, defaultDSN)
				}
			},
		},
		{
			name: "keys nested under a map field populate that map",
			file: "labels:\n  env: prod\n  tier: gold\n",
			check: func(t *testing.T, cfg *testConfig) {
				if got := cfg.Labels["env"]; got != "prod" {
					t.Errorf("Labels[\"env\"] = %q, want %q", got, "prod")
				}
				if got := cfg.Labels["tier"]; got != "gold" {
					t.Errorf("Labels[\"tier\"] = %q, want %q", got, "gold")
				}
			},
		},
		{
			name: "a field tagged as skipped is never populated",
			env:  []string{"SPEED_RUNTIME=should-be-ignored"},
			check: func(t *testing.T, cfg *testConfig) {
				if cfg.Runtime != "" {
					t.Errorf("Runtime = %q, want it left empty", cfg.Runtime)
				}
			},
		},
		{
			name: "strings from the environment convert into the field's type",
			env: []string{
				"SPEED_DEBUG=true",
				"SPEED_TIMEOUT=45s",
				"SPEED_DATABASE__PORT=6543",
			},
			check: func(t *testing.T, cfg *testConfig) {
				if !cfg.Debug {
					t.Error("Debug = false, want true")
				}
				if want := 45 * time.Second; cfg.Timeout != want {
					t.Errorf("Timeout = %v, want %v", cfg.Timeout, want)
				}
				if cfg.Database.Port != 6543 {
					t.Errorf("Database.Port = %d, want 6543", cfg.Database.Port)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := []Option{WithArgs(tt.args), WithEnviron(tt.env)}
			if tt.file != "" {
				opts = append(opts, WithConfigFile(writeConfigFile(t, tt.file)))
			}

			cfg := newTestConfig()
			if err := New(opts...).Load(cfg); err != nil {
				t.Fatalf("Load returned an unexpected error: %v", err)
			}
			tt.check(t, cfg)
		})
	}
}

type requiredDatabase struct {
	DSN string `config:"required"`
}

type requiredConfig struct {
	Database requiredDatabase
}

func TestLoad_RequiredValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     []string
		args    []string
		file    string
		seed    string
		wantErr bool
	}{
		{name: "no source supplies the required key", wantErr: true},
		{name: "the environment supplies it", env: []string{"SPEED_DATABASE__DSN=x"}},
		{name: "a flag supplies it", args: []string{"--database.dsn=x"}},
		{name: "the file supplies it", file: "database:\n  dsn: x\n"},
		{name: "the struct default supplies it", seed: "x"},
		{
			name:    "an empty value does not satisfy it",
			env:     []string{"SPEED_DATABASE__DSN="},
			wantErr: true,
		},
		{
			name:    "an empty value overriding a good default still fails",
			env:     []string{"SPEED_DATABASE__DSN="},
			seed:    "x",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := []Option{WithArgs(tt.args), WithEnviron(tt.env)}
			if tt.file != "" {
				opts = append(opts, WithConfigFile(writeConfigFile(t, tt.file)))
			}

			cfg := &requiredConfig{}
			cfg.Database.DSN = tt.seed
			err := New(opts...).Load(cfg)

			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Load returned an unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Load succeeded, want an error naming the missing required key")
			}
			if !errors.Is(err, ErrMissingValue) {
				t.Errorf("error does not wrap ErrMissingValue: %v", err)
			}
			// The error must be actionable: it names the key and every place looked.
			for _, want := range []string{"database.dsn", "SPEED_DATABASE__DSN", "--database.dsn"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error message does not mention %q: %v", want, err)
				}
			}
		})
	}
}

func TestLoad_InvalidValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		env        []string
		args       []string
		file       string
		wantKey    string
		wantSource string
	}{
		{
			name:       "a non-numeric environment value for an int field",
			env:        []string{"SPEED_DATABASE__PORT=not-a-number"},
			wantKey:    "database.port",
			wantSource: "environment variables",
		},
		{
			name:       "a non-numeric flag value for a deeply nested int field",
			args:       []string{"--database.pool.maxopen=abc"},
			wantKey:    "database.pool.maxopen",
			wantSource: "command-line flags",
		},
		{
			name:       "a non-numeric file value for an int field",
			file:       "database:\n  port: not-a-number\n",
			wantKey:    "database.port",
			wantSource: "the config file",
		},
		{
			name:       "an unparseable duration",
			env:        []string{"SPEED_TIMEOUT=soon"},
			wantKey:    "timeout",
			wantSource: "environment variables",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := []Option{WithArgs(tt.args), WithEnviron(tt.env)}
			if tt.file != "" {
				opts = append(opts, WithConfigFile(writeConfigFile(t, tt.file)))
			}

			err := New(opts...).Load(newTestConfig())
			if err == nil {
				t.Fatal("Load succeeded, want an error naming the offending key")
			}
			if !errors.Is(err, ErrInvalidValue) {
				t.Errorf("error does not wrap ErrInvalidValue: %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantKey) {
				t.Errorf("error message does not name the key %q: %v", tt.wantKey, err)
			}
			if !strings.Contains(err.Error(), tt.wantSource) {
				t.Errorf("error message does not name the source %q: %v", tt.wantSource, err)
			}
		})
	}
}

func TestLoad_ConfigFileIsOptional(t *testing.T) {
	t.Parallel()

	t.Run("a missing file is skipped rather than reported", func(t *testing.T) {
		t.Parallel()

		missing := filepath.Join(t.TempDir(), "absent.yaml")
		cfg := newTestConfig()
		if err := New(WithArgs(nil), WithEnviron(nil), WithConfigFile(missing)).Load(cfg); err != nil {
			t.Fatalf("Load returned an unexpected error: %v", err)
		}
		if cfg.Database.DSN != defaultDSN {
			t.Errorf("Database.DSN = %q, want the untouched default %q", cfg.Database.DSN, defaultDSN)
		}
	})

	t.Run("a file that exists but cannot be parsed is a hard error", func(t *testing.T) {
		t.Parallel()

		path := writeConfigFile(t, "database:\n\tdsn: tabs are not valid yaml\n  broken: [\n")
		err := New(WithArgs(nil), WithEnviron(nil), WithConfigFile(path)).Load(newTestConfig())
		if err == nil {
			t.Fatal("Load succeeded, want an error for the unparseable file")
		}
		if !errors.Is(err, ErrSourceUnreadable) {
			t.Errorf("error does not wrap ErrSourceUnreadable: %v", err)
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("error message does not name the file %q: %v", path, err)
		}
	})

	t.Run("a directory in place of a file is a hard error", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		err := New(WithArgs(nil), WithEnviron(nil), WithConfigFile(dir)).Load(newTestConfig())
		if err == nil {
			t.Fatal("Load succeeded, want an error for the unreadable file")
		}
		if !errors.Is(err, ErrSourceUnreadable) {
			t.Errorf("error does not wrap ErrSourceUnreadable: %v", err)
		}
	})
}

func TestLoad_InvalidTarget(t *testing.T) {
	t.Parallel()

	var typedNil *testConfig
	value := testConfig{}

	tests := []struct {
		name   string
		target any
	}{
		{name: "nil", target: nil},
		{name: "a typed nil pointer", target: typedNil},
		{name: "a struct value rather than a pointer", target: value},
		{name: "a pointer to a non-struct", target: new(int)},
		{name: "a string", target: "not a struct"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := New(WithArgs(nil), WithEnviron(nil)).Load(tt.target)
			if err == nil {
				t.Fatal("Load succeeded, want ErrInvalidTarget")
			}
			if !errors.Is(err, ErrInvalidTarget) {
				t.Errorf("error does not wrap ErrInvalidTarget: %v", err)
			}
		})
	}
}

func TestLoad_UnknownTagOptionIsRejected(t *testing.T) {
	t.Parallel()

	// "require" is the realistic slip for "required". Accepting it silently
	// would disable the required check without anyone noticing.
	type badTagConfig struct {
		Value string `config:"require"`
	}

	err := New(WithArgs(nil), WithEnviron(nil)).Load(&badTagConfig{})
	if err == nil {
		t.Fatal("Load succeeded, want the unsupported tag option to be rejected")
	}
	if !errors.Is(err, ErrInvalidTarget) {
		t.Errorf("error does not wrap ErrInvalidTarget: %v", err)
	}
	if !strings.Contains(err.Error(), "require") {
		t.Errorf("error message does not quote the offending option: %v", err)
	}
}

func TestLoad_IgnoresForeignArgsAndVariables(t *testing.T) {
	t.Parallel()

	// A bootstrap loader runs inside a process that owns flags of its own, so
	// arguments and variables it does not recognise must not derail it.
	args := []string{
		"-test.v=true",
		"-test.run", "TestSomething",
		"positional",
		"--unknown-flag", "its-value",
		"--database.dsn=" + flagDSN,
		"--",
	}
	environ := []string{
		"PATH=/usr/bin",
		"SPEED_SOMETHING__UNRELATED=ignored",
		"NOT_SPEED_DATABASE__DSN=ignored",
	}

	cfg := newTestConfig()
	if err := New(WithArgs(args), WithEnviron(environ)).Load(cfg); err != nil {
		t.Fatalf("Load returned an unexpected error: %v", err)
	}
	if cfg.Database.DSN != flagDSN {
		t.Errorf("Database.DSN = %q, want %q", cfg.Database.DSN, flagDSN)
	}
}

func TestLoad_DefaultArgsToleratesTheTestBinary(t *testing.T) {
	t.Parallel()

	// With no WithArgs option the loader reads the real os.Args, which under
	// `go test` carries the test binary's own flags.
	cfg := newTestConfig()
	if err := New(WithEnviron(nil)).Load(cfg); err != nil {
		t.Fatalf("Load returned an unexpected error for os.Args %v: %v", os.Args[1:], err)
	}
	if cfg.Database.DSN != defaultDSN {
		t.Errorf("Database.DSN = %q, want the untouched default %q", cfg.Database.DSN, defaultDSN)
	}
}

func TestCanonicaliseArgs(t *testing.T) {
	t.Parallel()

	schema, err := describe(&testConfig{})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "a single dash with an inline value",
			args: []string{"-database.dsn=a"},
			want: []string{"--database.dsn=a"},
		},
		{
			name: "a double dash with an inline value",
			args: []string{"--database.dsn=a"},
			want: []string{"--database.dsn=a"},
		},
		{
			name: "a separate value token is absorbed",
			args: []string{"--database.dsn", "a"},
			want: []string{"--database.dsn=a"},
		},
		{
			name: "a value containing an equals sign survives intact",
			args: []string{"--database.dsn=user=me;pass=secret"},
			want: []string{"--database.dsn=user=me;pass=secret"},
		},
		{
			name: "the name is lowercased",
			args: []string{"--Database.DSN=a"},
			want: []string{"--database.dsn=a"},
		},
		{
			name: "an unknown flag and its value are both dropped",
			args: []string{"--nope", "v", "--database.dsn=a"},
			want: []string{"--database.dsn=a"},
		},
		{
			name: "the value of a known flag is absorbed even when it looks like a flag",
			args: []string{"--database.dsn", "--weird", "--database.port=1"},
			want: []string{"--database.dsn=--weird", "--database.port=1"},
		},
		{
			name: "bare dashes and positionals are dropped",
			args: []string{"-", "--", "positional", "--database.dsn=a"},
			want: []string{"--database.dsn=a"},
		},
		{
			name: "a known flag with no value at all is passed through for the flag package to reject",
			args: []string{"--database.dsn"},
			want: []string{"--database.dsn"},
		},
		{
			name: "nothing recognisable yields nothing",
			args: []string{"-test.v=true", "positional"},
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := canonicaliseArgs(tt.args, schema)
			if len(got) != len(tt.want) {
				t.Fatalf("canonicaliseArgs(%q) = %q, want %q", tt.args, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("canonicaliseArgs(%q) = %q, want %q", tt.args, got, tt.want)
				}
			}
		})
	}
}

func TestLoad_FlagWithoutValueIsReported(t *testing.T) {
	t.Parallel()

	err := New(WithArgs([]string{"--database.dsn"}), WithEnviron(nil)).Load(newTestConfig())
	if err == nil {
		t.Fatal("Load succeeded, want an error for the valueless flag")
	}
	if !errors.Is(err, ErrSourceUnreadable) {
		t.Errorf("error does not wrap ErrSourceUnreadable: %v", err)
	}
}

// Defaults chosen so that a silent overwrite is visible: none of them is the
// zero value of its type, so a field that ends up zero was definitely written.
const (
	envDefaultPort    = 5432
	envDefaultRatio   = 1.5
	envDefaultDebug   = true
	envDefaultTimeout = 30 * time.Second
	envDefaultName    = "billing-api"
	envDefaultMaxOpen = 10
)

type envPoolConfig struct {
	MaxOpen int
}

// envConfig is a fixture used only by the environment-focused Load tests
// below, kept separate from testConfig so those tests do not inherit
// assumptions (embedded fields, a pointer field, a map field, a skipped
// field) from that fixture.
type envConfig struct {
	Port    int
	Ratio   float64
	Debug   bool
	Timeout time.Duration
	Name    string
	Pool    envPoolConfig
}

func newEnvConfig() *envConfig {
	return &envConfig{
		Port:    envDefaultPort,
		Ratio:   envDefaultRatio,
		Debug:   envDefaultDebug,
		Timeout: envDefaultTimeout,
		Name:    envDefaultName,
		Pool:    envPoolConfig{MaxOpen: envDefaultMaxOpen},
	}
}

// loadEnv runs a load driven only by the environment, with the flag source
// switched off so the real test binary's own arguments cannot interfere.
func loadEnv(env ...string) (*envConfig, error) {
	target := newEnvConfig()
	err := New(WithArgs(nil), WithEnviron(env)).Load(target)
	return target, err
}

// TestLoad_UnparseableEnvValueIsRefused checks that a value which cannot be
// applied to its field aborts the load with an error that names both the key
// and the source, across every scalar type the struct uses.
func TestLoad_UnparseableEnvValueIsRefused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		env     string
		wantKey string
	}{
		{"a word where an int is expected", "SPEED_PORT=not-a-number", "port"},
		{"a decimal where an int is expected", "SPEED_PORT=1.5", "port"},
		{"an int that overflows the field", "SPEED_PORT=99999999999999999999999", "port"},
		{"a negative marker with no digits", "SPEED_PORT=+", "port"},
		{"whitespace where an int is expected", "SPEED_PORT=   ", "port"},
		{"a word where a float is expected", "SPEED_RATIO=abc", "ratio"},
		{"a word where a bool is expected", "SPEED_DEBUG=maybe", "debug"},
		{"a number where a bool is expected", "SPEED_DEBUG=2", "debug"},
		{"a word where a duration is expected", "SPEED_TIMEOUT=soon", "timeout"},
		{"a bare number where a duration is expected", "SPEED_TIMEOUT=30seconds", "timeout"},
		{"a word in a nested int field", "SPEED_POOL__MAXOPEN=xyz", "pool.maxopen"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := loadEnv(tt.env)
			if err == nil {
				t.Fatalf("Load(%s) succeeded, want an error naming key %q", tt.env, tt.wantKey)
			}
			if !errors.Is(err, ErrInvalidValue) {
				t.Errorf("error does not wrap ErrInvalidValue: %v", err)
			}
			msg := err.Error()
			// Naming the key is what makes the error actionable: the operator
			// has to know which variable to fix.
			if !strings.Contains(msg, tt.wantKey) {
				t.Errorf("error does not name the key %q: %v", tt.wantKey, err)
			}
			if !strings.Contains(msg, "environment variables") {
				t.Errorf("error does not name the offending source: %v", err)
			}
			// The error must also point at where the value could come from,
			// which is the whole purpose of the "sources checked" list.
			if !strings.Contains(msg, "sources checked") {
				t.Errorf("error does not list the sources checked: %v", err)
			}
		})
	}
}

// TestLoad_ValidEnvValuesStillLoad is the control for the table above: the
// same keys, with values that do parse, must reach the struct. It proves the
// failures above come from the values and not from broken plumbing.
func TestLoad_ValidEnvValuesStillLoad(t *testing.T) {
	t.Parallel()

	got, err := loadEnv(
		"SPEED_PORT=6543",
		"SPEED_RATIO=2.25",
		"SPEED_DEBUG=false",
		"SPEED_TIMEOUT=45s",
		"SPEED_NAME=metering-api",
		"SPEED_POOL__MAXOPEN=32",
	)
	if err != nil {
		t.Fatalf("Load returned an error for valid values: %v", err)
	}

	want := envConfig{
		Port:    6543,
		Ratio:   2.25,
		Debug:   false,
		Timeout: 45 * time.Second,
		Name:    "metering-api",
		Pool:    envPoolConfig{MaxOpen: 32},
	}
	if *got != want {
		t.Errorf("loaded config = %+v, want %+v", *got, want)
	}
}

// TestLoad_EmptyEnvValueMustNotSilentlyZeroField is the case the rest of this
// section exists to set up.
//
// An empty string is not a valid int, float or bool, so by the package's own
// documented contract it is "a value that cannot be applied to the field it
// maps to" and must abort the load. Instead the loader's weak typing coerces it
// to the zero value and reports success, quietly discarding the default the
// caller had set.
//
// This matters in production rather than only on paper: SPEED_PORT=$UNSET_VAR
// in a shell, an empty value in a Kubernetes ConfigMap, or a CI variable that
// was declared but never filled all produce exactly this input, and the process
// then boots on port 0 with the operator told nothing. A fail-fast bootstrap
// loader is the one component where that must not happen.
//
// time.Duration already behaves correctly here, which is what makes this a bug
// rather than a deliberate policy: the same input is refused for one field type
// and silently swallowed for the others.
func TestLoad_EmptyEnvValueMustNotSilentlyZeroField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  string
		// was reports the default the field held before the load, and is
		// reported so a failure shows exactly what was discarded.
		was func(c *envConfig) any
	}{
		{"an empty value for an int field", "SPEED_PORT=", func(c *envConfig) any { return c.Port }},
		{"an empty value for a float field", "SPEED_RATIO=", func(c *envConfig) any { return c.Ratio }},
		{"an empty value for a bool field", "SPEED_DEBUG=", func(c *envConfig) any { return c.Debug }},
		{"an empty value for a nested int field", "SPEED_POOL__MAXOPEN=", func(c *envConfig) any { return c.Pool.MaxOpen }},
		// The control: this one is already refused, and must stay refused.
		{"an empty value for a duration field", "SPEED_TIMEOUT=", func(c *envConfig) any { return c.Timeout }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			before := tt.was(newEnvConfig())
			got, err := loadEnv(tt.env)
			if err == nil {
				t.Errorf("Load(%s) reported success and set the field to %v, discarding the default %v; "+
					"an empty string cannot be applied to this field, so Load must fail with an error wrapping ErrInvalidValue",
					tt.env, tt.was(got), before)
				return
			}
			if !errors.Is(err, ErrInvalidValue) {
				t.Errorf("error does not wrap ErrInvalidValue: %v", err)
			}
		})
	}
}

// TestLoad_EmptyEnvValueForAStringFieldIsAnHonestOverride is the deliberate
// counterpoint to the case above. An empty string *is* a representable value
// for a string field, so supplying one is a real override rather than a
// failed conversion, and it must keep working.
func TestLoad_EmptyEnvValueForAStringFieldIsAnHonestOverride(t *testing.T) {
	t.Parallel()

	got, err := loadEnv("SPEED_NAME=")
	if err != nil {
		t.Fatalf("Load returned an error for an empty string value: %v", err)
	}
	if got.Name != "" {
		t.Errorf("Name = %q, want the empty override", got.Name)
	}
	// Nothing else may be disturbed by that one variable.
	if got.Port != envDefaultPort {
		t.Errorf("Port = %d, want the untouched default %d", got.Port, envDefaultPort)
	}
}

// TestLoad_IntegerLiteralsAcceptGoSyntax characterises a surprise worth
// pinning: integer parsing runs with base detection, so an operator can write
// a port in hex or with digit separators and it is accepted. This is recorded
// rather than asserted as desirable, so that if the loader ever tightens to
// decimal-only the change is noticed instead of silently altering what
// existing deployments mean.
func TestLoad_IntegerLiteralsAcceptGoSyntax(t *testing.T) {
	t.Parallel()

	tests := []struct {
		env  string
		want int
	}{
		{"SPEED_PORT=0x10", 16},
		{"SPEED_PORT=1_0", 10},
		{"SPEED_PORT=0o17", 15},
		{"SPEED_PORT=010", 8},
	}

	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			t.Parallel()

			got, err := loadEnv(tt.env)
			if err != nil {
				t.Fatalf("Load(%s) returned an error: %v", tt.env, err)
			}
			if got.Port != tt.want {
				t.Errorf("Port = %d, want %d", got.Port, tt.want)
			}
		})
	}
}
