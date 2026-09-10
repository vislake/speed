package config_test

// Runnable documentation for the config public API. Every example here is
// compiled and executed by `go test`.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vislake/speed/go/pkgcore/config"
)

// exampleConfig is the shape of a bootstrap configuration struct: the small,
// immutable set of values a process resolves once at startup. Field names map
// to keys, lowercased and dotted, so Database.DSN is the key "database.dsn",
// the flag --database.dsn and the variable SPEED_DATABASE__DSN.
type exampleConfig struct {
	DeploymentMode string `config:"required"`
	Database       exampleDatabaseConfig
	Timeout        time.Duration

	// A field tagged "-" is never populated from any source; this one is
	// filled in later from the secret store.
	SigningKey string `config:"-"`
}

type exampleDatabaseConfig struct {
	DSN  string `config:"required"`
	Pool int
}

// ExampleLoader_Load shows the priority chain. Flags outrank the environment,
// the environment outranks the config file, and any field no source supplies
// keeps the default the caller already set on the struct.
func ExampleLoader_Load() {
	cfg := exampleConfig{
		Timeout: 5 * time.Second, // a struct default: the lowest-priority source
	}

	loader := config.New(
		config.WithArgs([]string{"--deploymentmode=distributed"}),
		config.WithEnviron([]string{
			"SPEED_DEPLOYMENTMODE=standalone", // outranked by the flag above
			"SPEED_DATABASE__DSN=postgres://localhost/speed",
			"SPEED_DATABASE__POOL=16",
		}),
	)
	if err := loader.Load(&cfg); err != nil {
		fmt.Println("load:", err)
		return
	}

	fmt.Println(cfg.DeploymentMode, cfg.Database.DSN, cfg.Database.Pool, cfg.Timeout)

	// Output:
	// distributed postgres://localhost/speed 16 5s
}

// ExampleLoader_Load_missingRequired shows the fail-fast behaviour. A required
// key no source supplied aborts startup with an error naming the key and every
// place the loader looked for it.
func ExampleLoader_Load_missingRequired() {
	var cfg exampleConfig

	err := config.New(config.WithArgs(nil), config.WithEnviron(nil)).Load(&cfg)
	fmt.Println(errors.Is(err, config.ErrMissingValue))

	// Output:
	// true
}

// ExampleWithConfigFile shows the optional file source. A missing file is
// skipped silently, since the file is a local-development convenience; a file
// that exists but cannot be parsed is a hard error.
func ExampleWithConfigFile() {
	dir, err := os.MkdirTemp("", "speed-config-example")
	if err != nil {
		fmt.Println("temp dir:", err)
		return
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "speed.yaml")
	contents := "deploymentmode: standalone\ndatabase:\n  dsn: file:speed.db\n  pool: 4\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		fmt.Println("write:", err)
		return
	}

	var cfg exampleConfig
	loader := config.New(
		config.WithConfigFile(path),
		config.WithArgs(nil),
		// The environment outranks the file, so this overrides pool: 4.
		config.WithEnviron([]string{"SPEED_DATABASE__POOL=32"}),
	)
	if err := loader.Load(&cfg); err != nil {
		fmt.Println("load:", err)
		return
	}

	fmt.Println(cfg.DeploymentMode, cfg.Database.DSN, cfg.Database.Pool)

	// Output:
	// standalone file:speed.db 32
}

// ExampleWithEnvPrefix shows a host whose environment carries its own prefix,
// and one variable its platform assigns by a name no prefix can reach: the
// pinned field reads PORT whatever the prefix is, while the other keys derive
// their names from APP_ and ignore SPEED_.
func ExampleWithEnvPrefix() {
	type hostConfig struct {
		// Port is pinned to the platform's own variable name; the key stays
		// "port", so the flag is still --port.
		Port int `config:"env=PORT"`
		// Database derives its name: APP_DATABASE under this prefix.
		Database string
	}

	var cfg hostConfig
	loader := config.New(
		config.WithEnvPrefix("APP_"),
		config.WithArgs(nil),
		config.WithEnviron([]string{
			"PORT=3123",
			"APP_DATABASE=app.db",
			"SPEED_DATABASE=ignored.db", // the old prefix is not a source any more
		}),
	)
	if err := loader.Load(&cfg); err != nil {
		fmt.Println("load:", err)
		return
	}

	fmt.Println(cfg.Port, cfg.Database)

	// Output:
	// 3123 app.db
}

// ExampleVerify shows the check a host runs once it knows which bootstrap keys
// its modules declared (the pkgcore Registry.Bootstrap seat): every declared
// key must map onto a field of the host's loader target, and the error names
// the ones that do not.
func ExampleVerify() {
	type hostConfig struct {
		Port     int `config:"env=PORT"`
		Database struct {
			DSN string
		}
	}

	err := config.Verify(&hostConfig{}, []string{"port", "database.dsn", "authn.pii_cipher_key"})
	fmt.Println(errors.Is(err, config.ErrInvalidTarget))
	fmt.Println(strings.Contains(err.Error(), "authn.pii_cipher_key"))

	// Output:
	// true
	// true
}
