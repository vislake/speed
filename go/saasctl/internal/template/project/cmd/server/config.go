//go:build ignore

package main

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
)

const (
	// defaultPort is used when the PORT environment variable is unset.
	defaultPort = "8080"

	// defaultSQLitePath is used when APP_DB_PATH is unset. It is a fixed
	// literal, deliberately INDEPENDENT of the module path this project
	// was generated under: the module path is editable by the consumer
	// (a rename is the first thing a fork does), and a default derived
	// from it at materialization would silently stop matching the file a
	// renamed project actually opens the moment saasctl's own commands
	// (db migrate, config print) derive the same default from the go.mod
	// they are handed. One fixed name on both sides keeps CLI-then-boot
	// agreement intact under any rename; an app that wants its own name
	// sets APP_DB_PATH. Relative so `go run ./cmd/server` works with zero
	// setup, per the standalone deployment mode's no-external-dependencies
	// promise applied to the generated project's own entry point.
	defaultSQLitePath = "app.db"

	// deploymentModeEnv names the environment variable selecting the
	// deployment mode. Empty defaults to standalone (configFromEnv), so the
	// generated project boots with zero external dependencies.
	deploymentModeEnv = "APP_DEPLOYMENT_MODE"

	// portEnv names the HTTP listen port environment variable.
	portEnv = "PORT"

	// dbPathEnv names the SQLite database path environment variable.
	dbPathEnv = "APP_DB_PATH"

	// configKeyEnv names the environment variable holding the hex-encoded
	// 32-byte master key the config module seals Sensitive values with
	// (config.WithCipher over dbkit.NewCipher). This is bootstrap
	// configuration, and it is the one value the project's own dynamic
	// configs table must never hold -- the key that encrypts the table
	// cannot live in the table -- so it arrives through the environment
	// like every other bootstrap value, with the documented development
	// default below.
	configKeyEnv = "APP_CONFIG_KEY"

	// configKeyHexLength is the encoded length of the required 32-byte key
	// (2 hex characters per byte), checked so a short or malformed
	// APP_CONFIG_KEY fails configuration loading with a precise message
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
	orgIndexKeyEnv = "APP_ORG_INDEX_KEY"

	// authnBlindIndexKeyEnv names the environment variable holding the
	// hex-encoded 32-byte HMAC key an authn-wiring composition's blind
	// indexer is built from (dbkit.NewBlindIndexer, consumed through
	// authn.WithBlindIndexKey, which indexes users.email_index and
	// users.phone_index). It is a SEPARATE bootstrap secret from every
	// other key in this file for the same reason orgIndexKeyEnv's doc
	// comment gives: the blind index protects the same users' columns
	// authn's PII cipher (authnPIICipherKeyEnv) seals, and dbkit's own
	// rule is that an AES key must never double as an HMAC key. This
	// file parses it in EVERY composition so the bootstrap contract
	// never changes with the selection; only compositions whose server.go
	// wires authn actually consume it. The name mirrors the reference
	// app's own APP_AUTHN_BLIND_INDEX_KEY, so one vocabulary names the
	// same key material in both places.
	authnBlindIndexKeyEnv = "APP_AUTHN_BLIND_INDEX_KEY"

	// authnPIICipherKeyEnv names the environment variable holding the
	// hex-encoded 32-byte AES key that seals authn's encrypted PII
	// columns (email, phone, TOTP secrets) via authn.RegisterPIISerializer.
	// It is deliberately a SEPARATE secret from every other key in this
	// file, including pkiLocalKeyCipherKeyEnv: dbkit's key-separation
	// rule applies across modules, not only within one (each key's own
	// doc comment below says what it protects and why it must stay
	// stable). Parsed in EVERY composition, consumed only by
	// authn-wiring ones -- the identical orgIndexKeyEnv doctrine.
	authnPIICipherKeyEnv = "APP_AUTHN_PII_CIPHER_KEY"

	// pkiLocalKeyCipherKeyEnv names the environment variable holding the
	// hex-encoded 32-byte AES key that seals go/pki's LocalSigner
	// private-key column (pki_local_keys, via pki.RegisterLocalKeySerializer).
	// pki follows authn silently in this generator's selection universe,
	// so this key is parsed whenever authn is wired -- and, per the
	// uniform-surface doctrine above, in every other composition too,
	// consumed only by authn-wiring ones.
	pkiLocalKeyCipherKeyEnv = "APP_PKI_LOCAL_KEY_CIPHER_KEY"

	// redisAddrEnv names the environment variable holding the Redis server
	// address ("host:port") that composes a REAL, MultiReplicaSafe
	// implementation for both the "eventbus" and "kv" seams -- one Redis
	// instance backs both, sharing one client, mirroring
	// examples/reference-app/cmd/server/server.go's own identical
	// APP_REDIS_ADDR wiring byte for byte (that file's own doc comment on
	// this variable has the full reasoning, including why wiring only one of
	// the two seams can never let a distributed composition pass Bootstrap).
	// Empty -- the default -- leaves both seams on the Preset's in-process
	// implementation, so `go run ./cmd/server` keeps working with zero
	// external dependencies; the distributed deployment mode requires
	// MultiReplicaSafe of every resolved seam (pkgcore.DeploymentMode.
	// RequiredCapabilities), so a distributed boot with this unset fails
	// Bootstrap's capability validation naming "eventbus" (or "kv") rather
	// than starting on an implementation that cannot actually share state
	// across replicas.
	redisAddrEnv = "APP_REDIS_ADDR"

	// s3EndpointEnv, s3BucketEnv, s3AccessKeyEnv and s3SecretKeyEnv together
	// compose a real S3-compatible ObjectStore for the "objectstore" seam
	// (pkgcore's objectstore/s3.NewObjectStore), the fourth seam
	// Kernel.Bootstrap always resolves and validates regardless of which
	// modules a selection wires -- see buildServer's own kernel-wiring
	// comment in server.go for why a distributed composition needs it wired
	// even though this selection's own modules may never call ObjectStore
	// themselves. All four are required together; a partially set group is
	// refused by configFromEnv below rather than silently falling back to
	// the Preset's local-directory default, since a partial S3 target is far
	// more likely a typo than a deliberate choice -- the identical
	// completeness rule examples/reference-app/cmd/server/server.go's own
	// s3EndpointEnv doc comment states. s3RegionEnv and s3UseSSLEnv refine
	// the same composition and are optional: Region matters to AWS S3
	// (MinIO- and RustFS-compatible servers ignore it), and s3UseSSLEnv,
	// parsed as a Go bool, defaults to false -- plain HTTP, the common case
	// for a local S3-compatible test/demo server -- when unset.
	s3EndpointEnv  = "APP_S3_ENDPOINT"
	s3BucketEnv    = "APP_S3_BUCKET"
	s3AccessKeyEnv = "APP_S3_ACCESS_KEY"
	// #nosec G101 -- this is an ENVIRONMENT VARIABLE NAME, not a credential
	// value: gosec's hardcoded-credential heuristic matches on the substring
	// "Secret" in the identifier alone.
	s3SecretKeyEnv = "APP_S3_SECRET_KEY"
	s3RegionEnv    = "APP_S3_REGION"
	s3UseSSLEnv    = "APP_S3_USE_SSL"

	// smtpHostEnv and smtpPortEnv together compose a real SMTP Mailer
	// (pkgcore.NewSMTPMailer) for the "mailer" seam; both are required
	// together, for the same fail-loud-on-partial-config reason
	// s3EndpointEnv's doc comment gives. smtpUsernameEnv and smtpPasswordEnv
	// are optional -- SMTP AUTH activates only when a username is set
	// (pkgcore.SMTPConfig.Username's own doc comment). Unset -- the default
	// -- leaves the "mailer" seam on the Preset's console default, exactly
	// like every other seam here.
	smtpHostEnv     = "APP_SMTP_HOST"
	smtpPortEnv     = "APP_SMTP_PORT"
	smtpUsernameEnv = "APP_SMTP_USERNAME"
	// #nosec G101 -- this is an ENVIRONMENT VARIABLE NAME, not a credential
	// value, the identical false positive s3SecretKeyEnv's own #nosec
	// comment above excepts.
	smtpPasswordEnv = "APP_SMTP_PASSWORD"

	// smsGatewayURLEnv names the environment variable holding the endpoint
	// authn's real SMS transport (authn.NewHTTPSMSSender) posts delivery
	// requests to. This variable is parsed here, in the shared config.go,
	// regardless of selection -- the same "the bootstrap contract never
	// changes with the selection" reasoning orgIndexKeyEnv's own doc comment
	// above already states -- but only consumed by a selection whose
	// server.go actually wires authn (buildServer's own doc comment in each
	// authn-containing selection's server.go says so). Empty under the
	// standalone deployment mode leaves authn's "SMS sender" seam on its
	// console default; empty under the distributed deployment mode leaves
	// that seam deliberately UNWIRED, so authn's own wiring-time validation
	// fails closed with authn.ErrMissingDistributedSMSSender rather than an
	// authn-containing selection silently keeping a console sender nobody in
	// a distributed replica pool is reading -- the identical three-way
	// branch examples/reference-app/cmd/server/server.go's own
	// smsGatewayURLEnv doc comment describes.
	smsGatewayURLEnv = "APP_SMS_GATEWAY_URL"
)

// devConfigKey is the master key used when APP_CONFIG_KEY is unset.
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
// do this. A real deployment sets APP_CONFIG_KEY from a secret store
// (or refuses to start); rotating the key loses every value the cipher
// sealed, so a real deployment's key must be stable for the life of its
// data.
var devConfigKey = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

// devOrgIndexKey is the HMAC key used when APP_ORG_INDEX_KEY is unset:
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

// devBlindIndexKey, devPIICipherKey and devPKILocalKeyCipherKey are the
// keys used when their own environment variables above are unset: the
// rest of this file's committed-key dev family, completing the byte-range
// pattern devConfigKey (ascending 0x00..0x1f) and devOrgIndexKey
// (descending 0xff..0xe0) began -- 0x40..0x5f, 0x60..0x7f and 0x80..0x9f,
// each visibly a placeholder and visibly DIFFERENT from every other
// member of the family, because dbkit's key-separation rule (never let
// one key double as two different AEAD or HMAC constructions) applies
// across modules, not only within one.
//
// Each protects something different and each must stay stable across
// restarts for a different reason: devBlindIndexKey must stay IDENTICAL
// across restarts or every already-stored email/phone blind index
// becomes unfindable (authn.WithBlindIndexKey consumes it);
// devPIICipherKey seals authn's encrypted PII columns (email, phone,
// TOTP secrets) via authn.RegisterPIISerializer; and devPKILocalKeyCipherKey
// seals go/pki's LocalSigner private-key column (pki_local_keys, via
// pki.RegisterLocalKeySerializer). go/pki's signing keys themselves need
// no dev-seed derivation: they are generated once by
// pki.Service.EnsurePurpose and PERSIST in this project's database
// across restarts -- the persisted-key shape, so no derivation from a
// dev seed is ever needed (the key's own doc comment in config.go
// describes that durability).
//
// These are the same documented trade-off as devConfigKey's own -- and
// now the same SHAPE as it too: each is a FALLBACK applied only when its
// environment variable is unset, never a value that overrides a
// configured one. A key committed to the repository is not a secret, and
// a real deployment must set every one of the five key variables
// (APP_CONFIG_KEY, APP_ORG_INDEX_KEY, APP_AUTHN_BLIND_INDEX_KEY,
// APP_AUTHN_PII_CIPHER_KEY, APP_PKI_LOCAL_KEY_CIPHER_KEY) from a secret
// store, or refuse to start; rotating a key that was in use loses
// whatever it protected, so a real deployment's keys must be stable for
// the life of their data.
var (
	devBlindIndexKey = []byte{
		0x40, 0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47,
		0x48, 0x49, 0x4a, 0x4b, 0x4c, 0x4d, 0x4e, 0x4f,
		0x50, 0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57,
		0x58, 0x59, 0x5a, 0x5b, 0x5c, 0x5d, 0x5e, 0x5f,
	}
	devPIICipherKey = []byte{
		0x60, 0x61, 0x62, 0x63, 0x64, 0x65, 0x66, 0x67,
		0x68, 0x69, 0x6a, 0x6b, 0x6c, 0x6d, 0x6e, 0x6f,
		0x70, 0x71, 0x72, 0x73, 0x74, 0x75, 0x76, 0x77,
		0x78, 0x79, 0x7a, 0x7b, 0x7c, 0x7d, 0x7e, 0x7f,
	}
	devPKILocalKeyCipherKey = []byte{
		0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0x87,
		0x88, 0x89, 0x8a, 0x8b, 0x8c, 0x8d, 0x8e, 0x8f,
		0x90, 0x91, 0x92, 0x93, 0x94, 0x95, 0x96, 0x97,
		0x98, 0x99, 0x9a, 0x9b, 0x9c, 0x9d, 0x9e, 0x9f,
	}
)

// serverConfig is the generated project's bootstrap wiring configuration:
// the values a process must know before anything else can start
// (deployment mode, port, database path, the config master key, the org
// blind-index key, and the three authn/pki key materials an authn-wiring
// composition's ciphers and indexer are built from). It is a plain struct
// read from the environment by configFromEnv, NOT the dynamic
// configuration the config module serves: dynamic configuration lives in
// the configs table and can never hold the very key that encrypts it, so
// this bootstrap struct is the deliberate exception -- the one
// configuration a project's own process always reads from its
// environment.
type serverConfig struct {
	DeploymentMode       pkgcore.DeploymentMode
	Port                 string
	SQLitePath           string
	ConfigKey            []byte
	OrgIndexKey          []byte
	AuthnBlindIndexKey   []byte
	AuthnPIICipherKey    []byte
	PKILocalKeyCipherKey []byte

	// RedisAddr, when non-empty, composes a real Redis-backed implementation
	// of both the "eventbus" and "kv" seams -- see redisAddrEnv's own doc
	// comment above.
	RedisAddr string

	// S3Endpoint, S3Bucket, S3AccessKey, S3SecretKey, S3Region and S3UseSSL
	// compose a real S3-compatible ObjectStore for the "objectstore" seam
	// when S3Endpoint is non-empty -- see s3EndpointEnv's own doc comment
	// above for the completeness rule. Empty S3Endpoint (the default) leaves
	// "objectstore" on the Preset's local-directory default.
	S3Endpoint  string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string
	S3Region    string
	S3UseSSL    bool

	// SMTPHost, SMTPPort, SMTPUsername and SMTPPassword compose a real SMTP
	// Mailer for the "mailer" seam when SMTPHost is non-empty -- see
	// smtpHostEnv's own doc comment above for the completeness rule. Empty
	// SMTPHost (the default) leaves "mailer" on the Preset's console
	// default.
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string

	// SMSGatewayURL composes authn's real SMS transport
	// (authn.NewHTTPSMSSender) for its "SMS sender" seam when non-empty --
	// see smsGatewayURLEnv's own doc comment above for what an empty value
	// means under each deployment mode. Unused by a selection whose
	// server.go wires no authn module.
	SMSGatewayURL string
}

// parseKeyEnv validates and decodes one hex-encoded 32-byte key
// environment variable, shared by every key material this file parses
// (the five key variables all use the same encoded shape and the same
// failure contract): a value whose encoded length is not configKeyHexLength
// is refused naming the variable and the required shape, a value of the
// right length that is not valid hex is refused naming the variable --
// both so a short or malformed key fails startup with a precise message
// rather than surfacing later as an opaque dbkit.NewCipher /
// dbkit.NewBlindIndexer error.
func parseKeyEnv(envName, encoded string) ([]byte, error) {
	if len(encoded) != configKeyHexLength {
		return nil, fmt.Errorf(
			"__APP_NAME__: %s must hold %d hex characters (a 32-byte key), got %d",
			envName, configKeyHexLength, len(encoded))
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("__APP_NAME__: %s: %w", envName, err)
	}
	return decoded, nil
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

	// Each of the five key materials resolves the same way: its own
	// environment variable when set (a hex-encoded 32-byte key -- see each
	// Env constant's doc comment), its documented development fallback
	// otherwise (see each dev* variable's doc comment). A malformed value
	// must fail startup with a precise message rather than surface later as
	// an opaque cipher error -- parseKeyEnv rejects anything that is not
	// valid hex, or that does not decode to exactly 32 bytes.
	configKey := devConfigKey
	if encoded := os.Getenv(configKeyEnv); encoded != "" {
		decoded, err := parseKeyEnv(configKeyEnv, encoded)
		if err != nil {
			return serverConfig{}, err
		}
		configKey = decoded
	}

	// The org invitation blind-index key: APP_ORG_INDEX_KEY when set,
	// devOrgIndexKey otherwise -- see orgIndexKeyEnv's doc comment for why
	// this must be a key distinct from configKey rather than the same one
	// reused. This project's org blind indexer consumes it when this
	// composition wires org (org.NewModule's invitation serializer).
	orgIndexKey := devOrgIndexKey
	if encoded := os.Getenv(orgIndexKeyEnv); encoded != "" {
		decoded, err := parseKeyEnv(orgIndexKeyEnv, encoded)
		if err != nil {
			return serverConfig{}, err
		}
		orgIndexKey = decoded
	}

	// The authn blind-index HMAC key, the authn PII cipher key and the pki
	// local-key cipher key: each resolves from its own variable when set,
	// its dev* fallback otherwise. An authn-wiring composition's server.go
	// consumes all three (authn.WithBlindIndexKey and the two serializers
	// registered before dbkit.Open); every other composition parses them
	// without consuming them, per the uniform-surface doctrine.
	authnBlindIndexKey := devBlindIndexKey
	if encoded := os.Getenv(authnBlindIndexKeyEnv); encoded != "" {
		decoded, err := parseKeyEnv(authnBlindIndexKeyEnv, encoded)
		if err != nil {
			return serverConfig{}, err
		}
		authnBlindIndexKey = decoded
	}
	authnPIICipherKey := devPIICipherKey
	if encoded := os.Getenv(authnPIICipherKeyEnv); encoded != "" {
		decoded, err := parseKeyEnv(authnPIICipherKeyEnv, encoded)
		if err != nil {
			return serverConfig{}, err
		}
		authnPIICipherKey = decoded
	}
	pkiLocalKeyCipherKey := devPKILocalKeyCipherKey
	if encoded := os.Getenv(pkiLocalKeyCipherKeyEnv); encoded != "" {
		decoded, err := parseKeyEnv(pkiLocalKeyCipherKeyEnv, encoded)
		if err != nil {
			return serverConfig{}, err
		}
		pkiLocalKeyCipherKey = decoded
	}

	// redisAddr stays empty when unset, leaving the "eventbus" and "kv"
	// seams on the Preset's in-process defaults -- see redisAddrEnv's own
	// doc comment above.
	redisAddr := os.Getenv(redisAddrEnv)

	// s3Endpoint/s3Bucket/s3AccessKey/s3SecretKey stay empty when unset,
	// leaving the "objectstore" seam on the Preset's local-directory
	// default; when any one of them is set, all four are required -- see
	// s3EndpointEnv's own doc comment above.
	s3Endpoint := os.Getenv(s3EndpointEnv)
	s3Bucket := os.Getenv(s3BucketEnv)
	s3AccessKey := os.Getenv(s3AccessKeyEnv)
	s3SecretKey := os.Getenv(s3SecretKeyEnv)
	if s3Endpoint != "" || s3Bucket != "" || s3AccessKey != "" || s3SecretKey != "" {
		var missing []string
		if s3Endpoint == "" {
			missing = append(missing, s3EndpointEnv)
		}
		if s3Bucket == "" {
			missing = append(missing, s3BucketEnv)
		}
		if s3AccessKey == "" {
			missing = append(missing, s3AccessKeyEnv)
		}
		if s3SecretKey == "" {
			missing = append(missing, s3SecretKeyEnv)
		}
		if len(missing) > 0 {
			return serverConfig{}, fmt.Errorf(
				"__APP_NAME__: an S3 ObjectStore composition needs %s set too (got some but not all of %s/%s/%s/%s)",
				strings.Join(missing, ", "), s3EndpointEnv, s3BucketEnv, s3AccessKeyEnv, s3SecretKeyEnv)
		}
	}
	s3UseSSL := false
	if raw := os.Getenv(s3UseSSLEnv); raw != "" {
		parsed, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return serverConfig{}, fmt.Errorf("__APP_NAME__: %s must be a valid bool, got %q: %w", s3UseSSLEnv, raw, parseErr)
		}
		s3UseSSL = parsed
	}

	// smtpHost/smtpPortRaw mirror s3Endpoint/... above: both unset leaves
	// the "mailer" seam on its Preset default, and a partial APP_SMTP_*
	// set is refused rather than silently ignored.
	smtpHost := os.Getenv(smtpHostEnv)
	smtpPortRaw := os.Getenv(smtpPortEnv)
	var smtpPort int
	switch {
	case smtpHost == "" && smtpPortRaw == "":
		// Both unset: the "mailer" seam stays on its Preset default.
	case smtpHost == "" || smtpPortRaw == "":
		return serverConfig{}, fmt.Errorf(
			"__APP_NAME__: an SMTP Mailer composition needs both %s and %s set", smtpHostEnv, smtpPortEnv)
	default:
		parsed, parseErr := strconv.Atoi(smtpPortRaw)
		if parseErr != nil {
			return serverConfig{}, fmt.Errorf("__APP_NAME__: %s must be a valid port number, got %q: %w", smtpPortEnv, smtpPortRaw, parseErr)
		}
		smtpPort = parsed
	}

	return serverConfig{
		DeploymentMode:       deploymentMode,
		Port:                 port,
		SQLitePath:           dbPath,
		ConfigKey:            configKey,
		OrgIndexKey:          orgIndexKey,
		AuthnBlindIndexKey:   authnBlindIndexKey,
		AuthnPIICipherKey:    authnPIICipherKey,
		PKILocalKeyCipherKey: pkiLocalKeyCipherKey,
		RedisAddr:            redisAddr,
		S3Endpoint:           s3Endpoint,
		S3Bucket:             s3Bucket,
		S3AccessKey:          s3AccessKey,
		S3SecretKey:          s3SecretKey,
		S3Region:             os.Getenv(s3RegionEnv),
		S3UseSSL:             s3UseSSL,
		SMTPHost:             smtpHost,
		SMTPPort:             smtpPort,
		SMTPUsername:         os.Getenv(smtpUsernameEnv),
		SMTPPassword:         os.Getenv(smtpPasswordEnv),
		SMSGatewayURL:        os.Getenv(smsGatewayURLEnv),
	}, nil
}
