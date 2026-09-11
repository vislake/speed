package config_test

// Runnable documentation for the config public API. Every example here is
// compiled and executed by `go test`.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

// ExampleEnvName shows the derivation a host or a tool can print instead of
// re-implementing: the environment variable name a config key is read from
// under a prefix. Nesting becomes EnvSeparator, a single underscore inside a
// segment stays, and a pinned field (config:"env=NAME") is read from its own
// name whatever this returns for its key.
func ExampleEnvName() {
	for _, key := range []string{"database.dsn", "authn.pii_cipher_key", "port"} {
		fmt.Println(config.EnvName(config.EnvPrefix, key))
	}
	fmt.Println(config.EnvName("APP_", "database.dsn"))

	// Output:
	// SPEED_DATABASE__DSN
	// SPEED_AUTHN__PII_CIPHER_KEY
	// SPEED_PORT
	// APP_DATABASE__DSN
}

// ExampleWithKeyDerivation shows the derived source of the five-source chain. A
// []byte field tagged derive holds one unit of key material: an explicit value
// from a text source must be 64 hex characters and wins; with nothing explicit
// supplied, the configured root key derives the field through the function
// WithKeyDerivation installed; with no root key configured, the struct default
// stands.
func ExampleWithKeyDerivation() {
	type hostConfig struct {
		// Explicitly configured, or derived from the root key.
		CipherKey []byte `config:"env=CIPHER_KEY,derive"`
		// Nothing supplies this one, so the derivation fills it.
		IndexKey []byte `config:"env=INDEX_KEY,derive"`
	}

	// The deriver is the host's to install. A real host wires the platform
	// composition -- pkgcore.BootstrapKeyPurpose over the key path, then
	// dbkit.DeriveKey over the root key and that purpose, in one call:
	// dbkit.DeriveBootstrapKey. This example stands in a local function,
	// because the example package must not import dbkit: dbkit sits above
	// pkgcore, and importing it back from here would be a cycle. A deriver
	// only has to be deterministic; the stand-in is deliberately not
	// cryptography.
	deriver := func(rootKey []byte, keyPath string) ([]byte, error) {
		sum := sha256.Sum256(append(append([]byte{}, rootKey...), keyPath...))
		return sum[:], nil
	}

	rootKey := bytes.Repeat([]byte{0x42}, 32)
	explicit := strings.Repeat("ab", 32)

	cfg := hostConfig{}
	loader := config.New(
		config.WithArgs(nil),
		config.WithEnviron([]string{"CIPHER_KEY=" + explicit}),
		config.WithRootKey(rootKey),
		config.WithKeyDerivation(deriver),
	)
	if err := loader.Load(&cfg); err != nil {
		fmt.Println("load:", err)
		return
	}
	fmt.Println(hex.EncodeToString(cfg.CipherKey) == explicit)
	fmt.Println(len(cfg.IndexKey) == 32 && !bytes.Equal(cfg.IndexKey, cfg.CipherKey))

	// With no root key configured, no derivation runs: both fields keep the
	// defaults the caller set before Load.
	defaultKey := bytes.Repeat([]byte{0x01}, 32)
	unconfigured := hostConfig{CipherKey: defaultKey, IndexKey: defaultKey}
	if err := config.New(config.WithArgs(nil), config.WithEnviron(nil)).Load(&unconfigured); err != nil {
		fmt.Println("load:", err)
		return
	}
	fmt.Println(bytes.Equal(unconfigured.CipherKey, defaultKey) && bytes.Equal(unconfigured.IndexKey, defaultKey))

	// Output:
	// true
	// true
	// true
}

// ExampleWithRootKeyEnv shows the environment root-key source: the loader reads
// the named variable in the same Load that derives from it, so a host that may
// not read the environment itself still wires derivation. An unset variable --
// or an emptied one -- means no root key, and the struct defaults stand.
func ExampleWithRootKeyEnv() {
	type hostConfig struct {
		CipherKey []byte `config:"env=CIPHER_KEY,derive"`
	}

	// A stand-in deriver; see ExampleWithKeyDerivation for what a real host
	// installs.
	deriver := func(rootKey []byte, keyPath string) ([]byte, error) {
		sum := sha256.Sum256(append(append([]byte{}, rootKey...), keyPath...))
		return sum[:], nil
	}
	defaultKey := bytes.Repeat([]byte{0x01}, 32)
	rootKeyText := hex.EncodeToString(bytes.Repeat([]byte{0x42}, 32))

	for _, environ := range [][]string{
		{"ROOT_KEY=" + rootKeyText}, // a configured root key: the field derives
		nil,                         // unset: no root key, so the default stands
	} {
		cfg := hostConfig{CipherKey: defaultKey}
		if err := config.New(
			config.WithArgs(nil),
			config.WithEnviron(environ),
			config.WithRootKeyEnv("ROOT_KEY"),
			config.WithKeyDerivation(deriver),
		).Load(&cfg); err != nil {
			fmt.Println("load:", err)
			return
		}
		fmt.Println(bytes.Equal(cfg.CipherKey, defaultKey))
	}

	// Output:
	// false
	// true
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

// ExampleLoader_ResolveDeclarations shows the declaration-driven entry: keys
// whose declaring module is their only owner are resolved without a struct,
// on the same five-source chain, and the result is addressed by declared key
// path. A hexkey declaration falls back to the declared defaults table
// (WithDevDefaults) when no source supplies it.
func ExampleLoader_ResolveDeclarations() {
	decls := []config.Declaration{
		{Key: "server.port", Format: config.FormatInt},
		{Key: "authn.pii_cipher_key", Format: config.FormatHexKey},
	}
	defaults := map[string][]byte{"authn.pii_cipher_key": bytes.Repeat([]byte{0x01}, 32)}

	values, err := config.New(
		config.WithArgs(nil),
		config.WithEnviron([]string{
			"SPEED_SERVER__PORT=8080",
			"SPEED_AUTHN__PII_CIPHER_KEY=" + strings.Repeat("ab", 32),
		}),
		config.WithDevDefaults(defaults),
	).ResolveDeclarations(decls)
	if err != nil {
		fmt.Println("resolve:", err)
		return
	}

	// server.port resolved to an int, and the explicitly supplied key
	// material decoded to its 32 bytes -- not the table's default.
	fmt.Println(values["server.port"])
	material := values["authn.pii_cipher_key"].([]byte)
	fmt.Println(len(material), hex.EncodeToString(material) == strings.Repeat("ab", 32))

	// A key no source supplied is absent from the result.
	absent, err := config.New(config.WithArgs(nil), config.WithEnviron(nil)).ResolveDeclarations(decls)
	if err != nil {
		fmt.Println("resolve:", err)
		return
	}
	_, present := absent["authn.pii_cipher_key"]
	fmt.Println(present)

	// Output:
	// 8080
	// 32 true
	// false
}
