// Package appconfig parses the bootstrap environment surface of a generated
// consumer project: SPEED_DEPLOYMENT_MODE, PORT, SPEED_DB_PATH,
// SPEED_CONFIG_KEY and SPEED_ORG_INDEX_KEY, resolved exactly as the
// generated project's own cmd/server/config.go resolves them.
//
// saasctl's db and config commands must see what the app they act on would
// see: db migrate opens the same SQLite path the app's configFromEnv would
// open, and config print renders the bootstrap values with their true
// provenance. This package is therefore a deliberate, test-pinned twin of
// the embedded template file internal/template/project/cmd/server/config.go
// -- the same five variable names, the same defaults (the standalone
// deployment mode, port 8080, the <app name>.db path), the same
// development key bytes and the same malformed-value error texts, with the
// template's __APP_NAME__ token replaced by the real app name the caller
// derives from the project's go.mod. appconfig_test.go re-reads the
// embedded template and fails when the two sides drift, so a template edit
// that renames a variable, changes a default, reorders the parse or
// rewrites an error text fails here before any generated app silently
// disagrees with the tool that maintains it.
//
// The environment is injectable through LookupEnv so every caller can
// decide its own source: the commands pass os.LookupEnv (a generated app
// reads the process environment, and so do they), tests pass a map.
package appconfig

import (
	"encoding/hex"
	"fmt"

	"github.com/vislake/speed/go/pkgcore"
)

// The five environment variable names of a generated project's bootstrap
// surface, exported because the command groups that render provenance and
// the tests that pin template parity all name the same variables.
const (
	// DeploymentModeEnv names the environment variable selecting a
	// generated project's deployment mode. Empty defaults to standalone
	// (Load), so a generated project boots with zero external dependencies.
	DeploymentModeEnv = "SPEED_DEPLOYMENT_MODE"

	// PortEnv names the HTTP listen port environment variable.
	PortEnv = "PORT"

	// DBPathEnv names the SQLite database path environment variable.
	DBPathEnv = "SPEED_DB_PATH"

	// ConfigKeyEnv names the environment variable holding the hex-encoded
	// 32-byte master key the config module seals Sensitive values with.
	ConfigKeyEnv = "SPEED_CONFIG_KEY"

	// OrgIndexKeyEnv names the environment variable holding the hex-encoded
	// 32-byte HMAC key an org-wiring project's blind indexer is built from.
	OrgIndexKeyEnv = "SPEED_ORG_INDEX_KEY"
)

// defaultPort is used when the PORT environment variable is unset, the
// same default the generated server uses.
const defaultPort = "8080"

// configKeyHexLength is the encoded length of the required 32-byte key (2
// hex characters per byte), checked so a short or malformed key fails
// configuration loading with a precise message rather than surfacing later
// as an opaque NewCipher error -- the template's exact check.
const configKeyHexLength = 64

// devConfigKey and devOrgIndexKey are the keys used when the respective
// environment variable is unset: the documented development defaults of a
// generated project, byte-for-byte the template's -- devConfigKey the
// ascending 0x00..0x1f sequence, devOrgIndexKey the descending 0xff..0xe0
// sequence chosen to be visibly a DIFFERENT key (an AES key must never
// double as an HMAC key, and the two defaults are engineered not to be
// confused). Like the template's own copies, they are honest placeholders,
// never secrets a real deployment should keep.
var devConfigKey = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

var devOrgIndexKey = []byte{
	0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa, 0xf9, 0xf8,
	0xf7, 0xf6, 0xf5, 0xf4, 0xf3, 0xf2, 0xf1, 0xf0,
	0xef, 0xee, 0xed, 0xec, 0xeb, 0xea, 0xe9, 0xe8,
	0xe7, 0xe6, 0xe5, 0xe4, 0xe3, 0xe2, 0xe1, 0xe0,
}

// LookupEnv reads one environment variable. os.LookupEnv satisfies it; the
// injectable shape exists so tests (and callers that want a different
// source) can supply their own.
type LookupEnv func(key string) (string, bool)

// Config is a generated project's resolved bootstrap configuration: the
// values the generated cmd/server's configFromEnv would resolve, plus a
// per-field record of which value came from the environment. Everything is
// resolved exactly as the app resolves it -- an unset or empty variable
// falls back to the same default the generated server uses -- so db
// migrate opens the very database the app would open, and config print can
// render each value with its true provenance.
type Config struct {
	DeploymentMode pkgcore.DeploymentMode
	Port           string
	SQLitePath     string
	ConfigKey      []byte
	OrgIndexKey    []byte

	// DeploymentModeFromEnv through OrgIndexKeyFromEnv record, per field,
	// whether the environment variable carried a non-empty value. Empty
	// counts as unset, matching os.Getenv: the generated server cannot
	// distinguish "set to empty" from "unset" either, and neither can this
	// package.
	DeploymentModeFromEnv bool
	PortFromEnv           bool
	SQLitePathFromEnv     bool
	ConfigKeyFromEnv      bool
	OrgIndexKeyFromEnv    bool
}

// Load resolves a generated project's bootstrap configuration for appName
// -- the go.mod module path's final element, the name __APP_NAME__ stood
// for at materialization -- reading the five environment variables through
// lookup. The parse order, defaults and failure texts mirror the
// generated configFromEnv exactly, including its error contract: a mode
// that does not parse is returned verbatim (no appName prefix), while the
// two key variables report malformed values with the app name prefixed, in
// the template's exact wording.
func Load(appName string, lookup LookupEnv) (Config, error) {
	var cfg Config

	modeValue, _ := lookup(DeploymentModeEnv)
	if modeValue == "" {
		modeValue = string(pkgcore.DeploymentModeStandalone)
	} else {
		cfg.DeploymentModeFromEnv = true
	}
	deploymentMode, err := pkgcore.ParseDeploymentMode(modeValue)
	if err != nil {
		return Config{}, err
	}
	cfg.DeploymentMode = deploymentMode

	port, _ := lookup(PortEnv)
	if port == "" {
		port = defaultPort
	} else {
		cfg.PortFromEnv = true
	}
	cfg.Port = port

	dbPath, _ := lookup(DBPathEnv)
	if dbPath == "" {
		dbPath = appName + ".db"
	} else {
		cfg.SQLitePathFromEnv = true
	}
	cfg.SQLitePath = dbPath

	configKey, configKeySet, err := loadKey(appName, ConfigKeyEnv, devConfigKey, lookup)
	if err != nil {
		return Config{}, err
	}
	cfg.ConfigKeyFromEnv = configKeySet
	cfg.ConfigKey = configKey

	orgIndexKey, orgIndexKeySet, err := loadKey(appName, OrgIndexKeyEnv, devOrgIndexKey, lookup)
	if err != nil {
		return Config{}, err
	}
	cfg.OrgIndexKeyFromEnv = orgIndexKeySet
	cfg.OrgIndexKey = orgIndexKey

	return cfg, nil
}

// loadKey resolves one of the two hex-encoded 32-byte key variables: the
// dev default when the variable is unset or empty, the decoded value
// otherwise. A value whose encoded length is not configKeyHexLength fails
// with the template's length message; a value of the right length that is
// not valid hex fails with the template's decode message -- both naming
// appName where the template names the __APP_NAME__ token.
func loadKey(appName, envName string, dev []byte, lookup LookupEnv) ([]byte, bool, error) {
	encoded, _ := lookup(envName)
	if encoded == "" {
		return dev, false, nil
	}
	if len(encoded) != configKeyHexLength {
		return nil, false, fmt.Errorf("%s: %s must hold %d hex characters (a 32-byte key), got %d",
			appName, envName, configKeyHexLength, len(encoded))
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %s: %w", appName, envName, err)
	}
	return decoded, true, nil
}
