//go:build ignore

package main

import (
	"encoding/hex"
	"fmt"
	"os"

	"github.com/vislake/speed/go/pkgcore"
)

const (
	// defaultPort is used when the PORT environment variable is unset.
	defaultPort = "8080"

	// defaultSQLitePath is used when SPEED_DB_PATH is unset. It is a
	// relative path so `go run ./cmd/server` works with zero setup, per the
	// standalone deployment mode's no-external-dependencies promise applied
	// to the generated project's own entry point.
	defaultSQLitePath = "__APP_NAME__.db"

	// deploymentModeEnv names the environment variable selecting the
	// deployment mode. Empty defaults to standalone (configFromEnv), so the
	// generated project boots with zero external dependencies.
	deploymentModeEnv = "SPEED_DEPLOYMENT_MODE"

	// portEnv names the HTTP listen port environment variable.
	portEnv = "PORT"

	// dbPathEnv names the SQLite database path environment variable.
	dbPathEnv = "SPEED_DB_PATH"

	// configKeyEnv names the environment variable holding the hex-encoded
	// 32-byte master key the config module seals Sensitive values with
	// (config.WithCipher over dbkit.NewCipher). This is bootstrap
	// configuration, and it is the one value the project's own dynamic
	// configs table must never hold -- the key that encrypts the table
	// cannot live in the table -- so it arrives through the environment
	// like every other bootstrap value, with the documented development
	// default below.
	configKeyEnv = "SPEED_CONFIG_KEY"

	// configKeyHexLength is the encoded length of the required 32-byte key
	// (2 hex characters per byte), checked so a short or malformed
	// SPEED_CONFIG_KEY fails configuration loading with a precise message
	// rather than surfacing later as an opaque NewCipher error.
	configKeyHexLength = 64

	// orgIndexKeyEnv names the environment variable holding the hex-encoded
	// 32-byte HMAC key an org-wiring project's blind indexer is built from
	// (dbkit.NewBlindIndexer). It is a SEPARATE bootstrap secret from
	// configKeyEnv on purpose: an org-wiring project reuses the config
	// cipher (built from configKeyEnv) to also encrypt org's Invitation
	// Email column, and dbkit's own rule is that an AES key must never
	// double as an HMAC key. This file parses it in EVERY composition so
	// the bootstrap contract never changes with the selection; only
	// compositions whose server.go calls org.NewModule actually consume it
	// (server.go's buildServer doc comment says which modules a selection
	// wires).
	orgIndexKeyEnv = "SPEED_ORG_INDEX_KEY"
)

// devConfigKey is the master key used when SPEED_CONFIG_KEY is unset.
// Zero-setup standalone development must work with no environment at all,
// while config's Sensitive items demand a real 32-byte key the moment one
// is declared (the config module's Attach fails with ErrCipherRequired
// otherwise), so this file provides one -- the ascending 0x00..0x1f byte
// sequence, chosen to be visibly a placeholder and to be clearly DIFFERENT
// from devOrgIndexKey's descending 0xff..0xe0 (see orgIndexKeyEnv's doc
// comment for why the two secrets must never be the same).
//
// This default is a documented trade-off, not a pattern to copy: a key
// committed to the repository is not a secret, and real hosts must never
// do this. A real deployment sets SPEED_CONFIG_KEY from a secret store
// (or refuses to start); rotating the key loses every value the cipher
// sealed, so a real deployment's key must be stable for the life of its
// data.
var devConfigKey = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

// devOrgIndexKey is the HMAC key used when SPEED_ORG_INDEX_KEY is unset:
// the descending 0xff..0xe0 byte sequence, chosen precisely so it is
// visibly a DIFFERENT 32 bytes from devConfigKey's ascending 0x00..0x1f
// (orgIndexKeyEnv's doc comment explains why the two must never be the
// same secret). The same honest placeholder as devConfigKey -- never a
// secret a real deployment should keep -- and consumed only by org-wiring
// compositions.
var devOrgIndexKey = []byte{
	0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa, 0xf9, 0xf8,
	0xf7, 0xf6, 0xf5, 0xf4, 0xf3, 0xf2, 0xf1, 0xf0,
	0xef, 0xee, 0xed, 0xec, 0xeb, 0xea, 0xe9, 0xe8,
	0xe7, 0xe6, 0xe5, 0xe4, 0xe3, 0xe2, 0xe1, 0xe0,
}

// serverConfig is the generated project's bootstrap wiring configuration:
// the values a process must know before anything else can start
// (deployment mode, port, database path, the config master key, the org
// blind-index key). It is a plain struct read from the environment by
// configFromEnv, NOT the dynamic configuration the config module serves:
// dynamic configuration lives in the configs table and can never hold the
// very key that encrypts it, so this bootstrap struct is the deliberate
// exception -- the one configuration a project's own process always reads
// from its environment.
type serverConfig struct {
	DeploymentMode pkgcore.DeploymentMode
	Port           string
	SQLitePath     string
	ConfigKey      []byte
	OrgIndexKey    []byte
}

// configFromEnv reads serverConfig from the environment, defaulting to the
// standalone deployment mode on SQLite so `go run ./cmd/server` genuinely
// starts a working server with zero external dependencies.
func configFromEnv() (serverConfig, error) {
	deploymentModeStr := os.Getenv(deploymentModeEnv)
	if deploymentModeStr == "" {
		deploymentModeStr = string(pkgcore.DeploymentModeStandalone)
	}
	deploymentMode, err := pkgcore.ParseDeploymentMode(deploymentModeStr)
	if err != nil {
		return serverConfig{}, err
	}

	port := os.Getenv(portEnv)
	if port == "" {
		port = defaultPort
	}

	dbPath := os.Getenv(dbPathEnv)
	if dbPath == "" {
		dbPath = defaultSQLitePath
	}

	// The config master key: SPEED_CONFIG_KEY when set (a hex-encoded
	// 32-byte key -- see configKeyEnv's doc comment), the documented
	// development default otherwise (see devConfigKey's). A malformed value
	// must fail startup with a precise message rather than surface later as
	// an opaque cipher error: hex.DecodeString rejects anything that is not
	// valid hex, and the length check below rejects anything that does not
	// decode to exactly 32 bytes.
	configKey := devConfigKey
	if encoded := os.Getenv(configKeyEnv); encoded != "" {
		if len(encoded) != configKeyHexLength {
			return serverConfig{}, fmt.Errorf(
				"__APP_NAME__: %s must hold %d hex characters (a 32-byte key), got %d",
				configKeyEnv, configKeyHexLength, len(encoded))
		}
		decoded, err := hex.DecodeString(encoded)
		if err != nil {
			return serverConfig{}, fmt.Errorf("__APP_NAME__: %s: %w", configKeyEnv, err)
		}
		configKey = decoded
	}

	// The org invitation blind-index key: SPEED_ORG_INDEX_KEY when set,
	// devOrgIndexKey otherwise -- the same parsing and the same failure
	// shape as configKey above, and see orgIndexKeyEnv's doc comment for
	// why this must be a key distinct from configKey rather than the same
	// one reused.
	orgIndexKey := devOrgIndexKey
	if encoded := os.Getenv(orgIndexKeyEnv); encoded != "" {
		if len(encoded) != configKeyHexLength {
			return serverConfig{}, fmt.Errorf(
				"__APP_NAME__: %s must hold %d hex characters (a 32-byte key), got %d",
				orgIndexKeyEnv, configKeyHexLength, len(encoded))
		}
		decoded, err := hex.DecodeString(encoded)
		if err != nil {
			return serverConfig{}, fmt.Errorf("__APP_NAME__: %s: %w", orgIndexKeyEnv, err)
		}
		orgIndexKey = decoded
	}

	return serverConfig{
		DeploymentMode: deploymentMode,
		Port:           port,
		SQLitePath:     dbPath,
		ConfigKey:      configKey,
		OrgIndexKey:    orgIndexKey,
	}, nil
}
