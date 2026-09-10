package main

// config_example.go carries the target struct the repository-root
// config.example.yaml loads against, and the load-verification unit test in
// configrefgen_test.go drives the real pkgcore/config loader over the real
// committed file. The struct mirrors the reference app's own bootstrap item
// set in loader shape (see config.example.yaml's header comment for why the
// file is the format's example rather than a file a shipped host reads).
//
// The struct's fields are deliberately loader-shaped: exported fields map
// to dotted keys lowercased, a nested struct contributes its type name's
// fields as deeper segments, a `config:"required"` tag makes a key
// mandatory, and the defaults assigned here are the lowest-priority source.
// Keys map to env variables as SPEED_<UPPERCASED KEY WITH __ FOR DOTS> and
// to flags as --key.

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"

	"github.com/vislake/speed/go/pkgcore/config"
)

// configExampleYAMLPath is the hand-written YAML example the JSON counterpart
// is derived from; configExampleJSONPath is the derived artifact, committed
// next to it.
const (
	configExampleYAMLPath = "config.example.yaml"
	configExampleJSONPath = "config.example.json"
)

// deriveConfigExampleJSON renders the JSON counterpart of the repository-root
// config.example.yaml: the loader accepts YAML or JSON for the same key set, so
// the two committed examples demonstrate one format each, and deriving the JSON
// from the YAML means the pair cannot disagree about a key or a value -- a
// hand-written twin could, and nothing would catch it. The derivation runs the
// same YAML parser the loader's file source runs, so what it reads is what the
// loader reads; koanf's own JSON marshalling sorts the keys, keeping the output
// byte-identical across runs.
func deriveConfigExampleJSON(root string) (string, error) {
	path := filepath.Join(root, configExampleYAMLPath)
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return "", fmt.Errorf("derive %s from %s: %w", configExampleJSONPath, configExampleYAMLPath, err)
	}
	out, err := json.MarshalIndent(k.Raw(), "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal %s: %w", configExampleJSONPath, err)
	}
	return string(out) + "\n", nil
}

// exampleBootstrapConfig is the loader target config.example.yaml is
// verified against.
type exampleBootstrapConfig struct {
	// DeploymentMode is the topology the process runs as; no source may
	// leave it unset (config:"required"). The loader derives the env name
	// SPEED_DEPLOYMENTMODE and the flag --deploymentmode from it.
	DeploymentMode string `config:"required"`

	// Port is the HTTP listen port, defaulting to 8080.
	Port string

	// DBPath is the SQLite database file, defaulting to reference-app.db.
	DBPath string

	// Redis and SMTP mirror the app's seam variables in nested form: the
	// block in config.example.yaml maps onto redis.addr and the smtp.*
	// keys, whose env spellings are SPEED_REDIS__ADDR and SPEED_SMTP__*.
	Redis exampleRedisConfig
	SMTP  exampleSMTPConfig
}

type exampleRedisConfig struct {
	Addr string
}

type exampleSMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
}

// exampleBootstrapDefaults returns the struct with the defaults the loader
// falls back to when no source supplies a key (the lowest-priority source).
func exampleBootstrapDefaults() exampleBootstrapConfig {
	return exampleBootstrapConfig{
		Port:   "8080",
		DBPath: "reference-app.db",
	}
}

// configLoaderFor returns a Loader pointed at the config.example.yaml file
// with the given injected args (os.Args[1:] form) and environment
// ("KEY=value" form). The injectable shapes exist so the load-verification
// test can pin the precedence chain without mutating the process
// environment.
func configLoaderFor(path string, args, environ []string) *config.Loader {
	return config.New(
		config.WithConfigFile(path),
		config.WithArgs(args),
		config.WithEnviron(environ),
	)
}

// loadConfigExample loads the repository-root config.example.yaml through
// the real loader with no environment and no flags, returning the resolved
// target. The file is optional in general (a missing one is skipped
// silently), but the load-verification test has already verified it exists.
func loadConfigExample(path string) (exampleBootstrapConfig, error) {
	cfg := exampleBootstrapDefaults()
	err := configLoaderFor(path, nil, nil).Load(&cfg)
	return cfg, err
}
