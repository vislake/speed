//go:build ignore

package main

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/config"
	objectstores3 "github.com/vislake/speed/go/pkgcore/objectstore/s3"
)

const (
	// envPrefix is the prefix every bootstrap variable of this project
	// carries, with one exception: PORT, the unprefixed name hosting
	// platforms inject. It is handed to the loader as config.WithEnvPrefix,
	// and every field of hostConfig below additionally pins its variable's
	// exact spelling with the env tag option -- this project's variables are
	// flat and single-underscored, a shape the loader's nesting derivation
	// (a double underscore marks each level) never produces, so the pin is
	// what makes the loader read these names. The prefix stays declared
	// rather than left implicit because it is the project's naming
	// convention: a field added later without a pin would be read under it.
	envPrefix = "APP_"

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

	// configKeyHexLength is the encoded length of the required 32-byte key
	// (2 hex characters per byte), checked so a short or malformed
	// APP_CONFIG_KEY fails configuration loading with a precise message
	// rather than surfacing later as an opaque NewCipher error.
	configKeyHexLength = 64
)

// devConfigKey is the master key used when APP_CONFIG_KEY is unset.
// Zero-setup standalone development must work with no environment at all,
// while config's Sensitive items demand a real 32-byte key the moment one
// is declared (the config module's Attach fails with ErrCipherRequired
// otherwise), so this file provides one -- the ascending 0x00..0x1f byte
// sequence, chosen to be visibly a placeholder and to be clearly DIFFERENT
// from devOrgIndexKey's descending 0xff..0xe0 (see the APP_ORG_INDEX_KEY
// field's doc comment for why the two secrets must never be the same).
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
// (the APP_ORG_INDEX_KEY field's doc comment explains why the two must
// never be the same secret). The same honest placeholder as devConfigKey
// -- never a secret a real deployment should keep -- and consumed only by
// org-wiring compositions.
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

// hostConfig is the loader target carrying this project's whole bootstrap
// surface, one field per variable. Every field pins its variable's exact
// name with the env tag option (the surface is flat and single-underscored,
// which the loader's nesting derivation never produces), and the three
// fields whose unset behavior is a scalar default -- DeploymentMode, Port
// and DBPath -- are pre-set by hostConfigDefaults as the loader's
// lowest-priority source.
//
// Field types are the loader's text type deliberately. A string field
// holds its variable's text verbatim, so "unset" and "set to the empty
// string" both arrive as "" -- the same value a direct environment read
// reports -- and the project's whole documented resolution rule ("unset or
// empty falls back") holds without per-field exceptions. That is also why
// the numeric- and boolean-shaped variables (APP_SMTP_PORT,
// APP_S3_USE_SSL) are strings here: their empty value must keep meaning
// "unset", and their validated parsing is part of serverConfigFrom below,
// so a malformed value fails with this file's own precise message naming
// the variable.
type hostConfig struct {
	// DeploymentMode names APP_DEPLOYMENT_MODE: the deployment topology
	// this process runs as -- "standalone" (the default) or "distributed".
	// An unset or emptied variable reads as standalone; anything else that
	// is not a known mode refuses the boot, naming the parse failure.
	DeploymentMode string `config:"env=APP_DEPLOYMENT_MODE"`

	// Port names PORT -- unprefixed on purpose, the variable hosting
	// platforms inject. Unset or emptied reads as defaultPort.
	Port string `config:"env=PORT"`

	// DBPath names APP_DB_PATH: the SQLite database file, relative to the
	// working directory unless absolute. Unset or emptied reads as
	// defaultSQLitePath, so `go run ./cmd/server` starts on a fresh local
	// file with zero setup.
	DBPath string `config:"env=APP_DB_PATH"`

	// ConfigKey names APP_CONFIG_KEY: the hex-encoded 32-byte master key
	// the config module seals Sensitive values with (config.WithCipher
	// over dbkit.NewCipher). This is bootstrap configuration, and it is
	// the one value the project's own dynamic configs table must never
	// hold -- the key that encrypts the table cannot live in the table --
	// so it arrives through the environment like every other bootstrap
	// value, with the documented development default (devConfigKey) when
	// unset.
	ConfigKey string `config:"env=APP_CONFIG_KEY"`

	// OrgIndexKey names APP_ORG_INDEX_KEY: the hex-encoded 32-byte HMAC
	// key an org-wiring project's blind indexer is built from
	// (org.NewEmailIndexer). It is a SEPARATE bootstrap secret from
	// ConfigKey on purpose: an org-wiring project reuses the config
	// cipher (built from ConfigKey) to also encrypt org's Invitation
	// Email column, and dbkit's own rule is that an AES key must never
	// double as an HMAC key. Parsed in EVERY composition so the bootstrap
	// contract never changes with the selection; only compositions whose
	// server.go calls org.NewModule actually consume it (server.go's
	// buildServer doc comment says which modules a selection wires).
	OrgIndexKey string `config:"env=APP_ORG_INDEX_KEY"`

	// AuthnBlindIndexKey names APP_AUTHN_BLIND_INDEX_KEY: the hex-encoded
	// 32-byte HMAC key an authn-wiring composition's blind indexer is
	// built from (dbkit.NewBlindIndexer, consumed through
	// authn.WithBlindIndexKey, which indexes users.email_index and
	// users.phone_index). It is a SEPARATE bootstrap secret from every
	// other key in this file for the same reason OrgIndexKey's doc
	// comment gives: the blind index protects the same users' columns
	// authn's PII cipher (AuthnPIICipherKey) seals, and dbkit's own
	// rule is that an AES key must never double as an HMAC key. Parsed in
	// EVERY composition so the bootstrap contract never changes with the
	// selection; only compositions whose server.go wires authn actually
	// consume it. The name mirrors the reference app's own
	// APP_AUTHN_BLIND_INDEX_KEY, so one vocabulary names the same key
	// material in both places.
	AuthnBlindIndexKey string `config:"env=APP_AUTHN_BLIND_INDEX_KEY"`

	// AuthnPIICipherKey names APP_AUTHN_PII_CIPHER_KEY: the hex-encoded
	// 32-byte AES key that seals authn's encrypted PII columns (email,
	// phone, TOTP secrets) via authn.RegisterPIISerializer. It is
	// deliberately a SEPARATE secret from every other key in this file,
	// including PKILocalKeyCipherKey: dbkit's key-separation rule applies
	// across modules, not only within one (each key's own doc comment
	// below says what it protects and why it must stay stable). Parsed in
	// EVERY composition, consumed only by authn-wiring ones -- the
	// identical OrgIndexKey doctrine.
	AuthnPIICipherKey string `config:"env=APP_AUTHN_PII_CIPHER_KEY"`

	// PKILocalKeyCipherKey names APP_PKI_LOCAL_KEY_CIPHER_KEY: the
	// hex-encoded 32-byte AES key that seals go/pki's LocalSigner
	// private-key column (pki_local_keys, via pki.RegisterLocalKeySerializer).
	// pki follows authn silently in this generator's selection universe,
	// so this key is parsed whenever authn is wired -- and, per the
	// uniform-surface doctrine above, in every other composition too,
	// consumed only by authn-wiring ones.
	PKILocalKeyCipherKey string `config:"env=APP_PKI_LOCAL_KEY_CIPHER_KEY"`

	// RedisAddr names APP_REDIS_ADDR: the Redis server address ("host:port")
	// that composes a REAL, MultiReplicaSafe implementation for both the
	// "eventbus" and "kv" seams -- one Redis instance backs both, sharing
	// one client, mirroring examples/reference-app/internal/app/server.go's
	// own identical APP_REDIS_ADDR wiring byte for byte (that file's own
	// doc comment on this variable has the full reasoning, including why
	// wiring only one of the two seams can never let a distributed
	// composition pass Bootstrap). Empty -- the default -- leaves both
	// seams on the Preset's in-process implementation, so
	// `go run ./cmd/server` keeps working with zero external dependencies;
	// the distributed deployment mode requires MultiReplicaSafe of every
	// resolved seam (pkgcore.DeploymentMode.RequiredCapabilities), so a
	// distributed boot with this unset fails Bootstrap's capability
	// validation naming "eventbus" (or "kv") rather than starting on an
	// implementation that cannot actually share state across replicas.
	RedisAddr string `config:"env=APP_REDIS_ADDR"`

	// OTLPEndpoint names APP_OTLP_ENDPOINT: the OTLP/gRPC endpoint
	// ("host:port", the syntax go/observability's own
	// Config.OTLPEndpoint doc comment describes) traces and metrics are
	// pushed to. Empty -- the default -- leaves obs.Init on the local
	// exporters (traces and metrics to stdout, plus the in-process
	// Prometheus handler the /metrics route serves once main.go's blank
	// import of go/observability/exporter/prometheus has registered it),
	// so zero-setup standalone development keeps working with nothing
	// running; set it to push both signals at a collector over OTLP,
	// which is what main.go hands obs.WithOTLPEndpoint exactly when this
	// is non-empty (an explicitly empty option is indistinguishable from
	// an absent one, and the local-exporters default needs no option at
	// all). The endpoint is the one switch for exporter selection:
	// obs.Init takes no deployment mode, and once an endpoint is set the
	// /metrics route answers 404 by design -- there is no local registry
	// to scrape, the collector is where metrics go. The OTLP exporter
	// factory the option composes is registered by main.go's blank import
	// of go/observability/exporter/otlp (WithOTLPEndpoint fails naming
	// that import when it was never registered).
	OTLPEndpoint string `config:"env=APP_OTLP_ENDPOINT"`

	// S3Endpoint, S3Bucket, S3AccessKey and S3SecretKey name
	// APP_S3_ENDPOINT/APP_S3_BUCKET/APP_S3_ACCESS_KEY/APP_S3_SECRET_KEY:
	// the four variables that together compose a real S3-compatible
	// ObjectStore for the "objectstore" seam
	// (pkgcore's objectstore/s3.NewObjectStore), the fourth seam
	// Kernel.Bootstrap always resolves and validates regardless of which
	// modules a selection wires -- see buildServer's own kernel-wiring
	// comment in server.go for why a distributed composition needs it wired
	// even though this selection's own modules may never call ObjectStore
	// themselves. All four are required together; a partially set group is
	// refused by serverConfigFrom rather than silently falling back to
	// the Preset's local-directory default, since a partial S3 target is far
	// more likely a typo than a deliberate choice -- the identical
	// completeness rule examples/reference-app/internal/app/server.go's own
	// S3 group doc comment states. S3Region, S3UseSSL and S3BucketLookup
	// below refine the same composition.
	S3Endpoint  string `config:"env=APP_S3_ENDPOINT"`
	S3Bucket    string `config:"env=APP_S3_BUCKET"`
	S3AccessKey string `config:"env=APP_S3_ACCESS_KEY"`
	// #nosec G101 -- this is an ENVIRONMENT VARIABLE NAME, not a credential
	// value: gosec's hardcoded-credential heuristic matches on the substring
	// "Secret" in the identifier alone.
	S3SecretKey string `config:"env=APP_S3_SECRET_KEY"`

	// S3Region names APP_S3_REGION: the region of the S3 composition above.
	// Region matters to AWS S3 (MinIO- and RustFS-compatible servers
	// ignore it). Optional: this is the first of the group's two optional
	// refinements, and an empty value means "unset" like every other
	// string field here.
	S3Region string `config:"env=APP_S3_REGION"`

	// S3UseSSL names APP_S3_USE_SSL: whether the S3 composition above
	// speaks TLS. Optional, and text rather than bool for the reason
	// hostConfig's own doc comment gives: an emptied APP_S3_USE_SSL must
	// keep meaning "unset" (the documented default false), so
	// serverConfigFrom parses the text itself and refuses a value
	// strconv.ParseBool rejects, naming the variable.
	S3UseSSL string `config:"env=APP_S3_USE_SSL"`

	// S3BucketLookup names APP_S3_BUCKET_LOOKUP: the addressing style the
	// S3 composition above uses to reach its bucket. Optional and only
	// meaningful alongside a complete group; serverConfigFrom parses the
	// text into the store's own enum -- "auto" (or unset) leaves the
	// style to the endpoint-derived default, "path" addresses the bucket
	// as a path segment, "virtual_host" as the endpoint's first host
	// label -- and refuses any other value, naming the allowed set.
	S3BucketLookup string `config:"env=APP_S3_BUCKET_LOOKUP"`

	// SMTPHost, SMTPPort, SMTPUsername and SMTPPassword name
	// APP_SMTP_HOST/APP_SMTP_PORT/APP_SMTP_USERNAME/APP_SMTP_PASSWORD:
	// SMTPHost and SMTPPort together compose a real SMTP Mailer
	// (pkgcore.NewSMTPMailer) for the "mailer" seam; both are required
	// together, for the same fail-loud-on-partial-config reason the S3
	// group above gives. SMTPUsername and SMTPPassword are optional --
	// SMTP AUTH activates only when a username is set
	// (pkgcore.SMTPConfig.Username's own doc comment). Unset -- the default
	// -- leaves the "mailer" seam on the Preset's console default, exactly
	// like every other seam here. APP_SMTP_PORT is text rather than int
	// for hostConfig's own reason (an emptied variable keeps meaning
	// "unset"); serverConfigFrom parses it, refusing a value strconv.Atoi
	// rejects.
	SMTPHost string `config:"env=APP_SMTP_HOST"`
	SMTPPort string `config:"env=APP_SMTP_PORT"`
	// #nosec G101 -- an ENVIRONMENT VARIABLE NAME, not a credential value.
	SMTPUsername string `config:"env=APP_SMTP_USERNAME"`
	// #nosec G101 -- this is an ENVIRONMENT VARIABLE NAME, not a credential
	// value, the identical false positive S3SecretKey's own #nosec
	// comment above excepts.
	SMTPPassword string `config:"env=APP_SMTP_PASSWORD"`

	// SMSGatewayURL names APP_SMS_GATEWAY_URL: the endpoint the real HTTP
	// SMS transport (pkgcore.NewHTTPSMSSender) posts delivery requests to.
	// This variable is loaded in the shared config, regardless of
	// selection -- the same "the bootstrap contract never changes with the
	// selection" reasoning OrgIndexKey's own doc comment above already
	// states -- but only consumed by a selection whose server.go actually
	// wires authn (buildServer's own doc comment in each authn-containing
	// selection's server.go says so). Empty under the standalone
	// deployment mode leaves authn's "SMS sender" seam on its console
	// default; empty under the distributed deployment mode leaves that
	// seam deliberately UNWIRED, so authn's own wiring-time validation
	// fails closed with authn.ErrMissingDistributedSMSSender rather than an
	// authn-containing selection silently keeping a console sender nobody in
	// a distributed replica pool is reading -- the identical three-way
	// branch examples/reference-app/internal/app/server.go's own
	// SMSGatewayURL doc comment describes.
	SMSGatewayURL string `config:"env=APP_SMS_GATEWAY_URL"`
}

// hostConfigDefaults returns the loader target with its three scalar
// defaults pre-set -- the loader's fourth source, applied where no flag,
// environment variable or config file (none is wired here) supplied a
// value. Every other field's empty value is its documented unset
// behavior: the key materials' dev fallbacks, the seam variables' "leave
// the Preset default" and the cross-variable rules all live in
// serverConfigFrom, which is also what treats an explicitly emptied
// variable as unset (a string field arrives as "" either way, and the
// empty string is a source's value, so the loader does not restore these
// defaults over it).
func hostConfigDefaults() hostConfig {
	return hostConfig{
		DeploymentMode: string(pkgcore.DeploymentModeStandalone),
		Port:           defaultPort,
		DBPath:         defaultSQLitePath,
	}
}

// loadHostConfig resolves the bootstrap surface from the process
// environment through the loader: flags first, then the environment under
// the APP_ prefix (PORT and every other variable pinned by its own env
// tag), no config file, then hostConfigDefaults. This project's own
// resolution rules -- its text-to-type parsing, its dev-key fallbacks and
// its cross-variable refusals -- are the transform that follows;
// WithEnvPrefix is the whole option wiring, because the surface's
// variables are flat and pinned by name, and with no field derived from a
// root key there is no derivation to configure.
func loadHostConfig() (hostConfig, error) {
	hc := hostConfigDefaults()
	err := config.New(config.WithEnvPrefix(envPrefix)).Load(&hc)
	return hc, err
}

// serverConfig is the generated project's bootstrap wiring configuration:
// the values a process must know before anything else can start
// (deployment mode, port, database path, the config master key, the org
// blind-index key, the three authn/pki key materials an authn-wiring
// composition's ciphers and indexer are built from, the infrastructure
// seam addresses and the OTLP endpoint). It is the transform output of
// the loader-filled hostConfig, NOT the dynamic configuration the config
// module serves: dynamic configuration lives in the configs table and can
// never hold the very key that encrypts it, so this bootstrap struct is
// the deliberate exception -- the one configuration a project's own
// process always reads from its environment.
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
	// of both the "eventbus" and "kv" seams -- see the APP_REDIS_ADDR
	// field's own doc comment above.
	RedisAddr string

	// OTLPEndpoint, when non-empty, is handed to obs.Init as
	// obs.WithOTLPEndpoint (main.go), pushing traces and metrics to it
	// over OTLP -- see the APP_OTLP_ENDPOINT field's own doc comment
	// above for the default and the exporter registration the option
	// needs.
	OTLPEndpoint string

	// S3Endpoint, S3Bucket, S3AccessKey, S3SecretKey, S3Region, S3UseSSL
	// and S3BucketLookup compose a real S3-compatible ObjectStore for the
	// "objectstore" seam when S3Endpoint is non-empty -- see the APP_S3_*
	// fields' own doc comment above for the completeness rule. Empty
	// S3Endpoint (the default) leaves "objectstore" on the Preset's
	// local-directory default.
	S3Endpoint     string
	S3Bucket       string
	S3AccessKey    string
	S3SecretKey    string
	S3Region       string
	S3UseSSL       bool
	S3BucketLookup objectstores3.BucketLookupType

	// SMTPHost, SMTPPort, SMTPUsername and SMTPPassword compose a real SMTP
	// Mailer for the "mailer" seam when SMTPHost is non-empty -- see the
	// APP_SMTP_* fields' own doc comment above for the completeness rule.
	// Empty SMTPHost (the default) leaves "mailer" on the Preset's console
	// default.
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string

	// SMSGatewayURL composes the real HTTP SMS transport
	// (pkgcore.NewHTTPSMSSender) for authn's "SMS sender" seam when non-empty --
	// see the APP_SMS_GATEWAY_URL field's own doc comment above for what an
	// empty value means under each deployment mode. Unused by a selection
	// whose server.go wires no authn module.
	SMSGatewayURL string
}

// parseKeyEnv validates and decodes one hex-encoded 32-byte key
// value, given the variable name it was read from (the name a refusal
// reports), shared by every key material this file resolves (the five key
// variables all use the same encoded shape and the same failure
// contract): a value whose encoded length is not configKeyHexLength
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

// configFromEnv resolves serverConfig from the process environment
// through the loader, defaulting to the standalone deployment mode on
// SQLite so `go run ./cmd/server` genuinely starts a working server with
// zero external dependencies. It is loadHostConfig plus the transform
// below, in one call, for every caller that wants the assembled config
// rather than the raw surface.
func configFromEnv() (serverConfig, error) {
	hc, err := loadHostConfig()
	if err != nil {
		return serverConfig{}, err
	}
	return serverConfigFrom(hc)
}

// serverConfigFrom transforms a loaded bootstrap surface into the
// serverConfig the assembly consumes: it parses the deployment mode,
// decodes the five hex key materials (each falling back to its documented
// development default when unset), parses the text-typed numeric and
// boolean variables, and enforces the cross-variable completeness rules
// (the S3 group and the SMTP pair) that a single-variable loader cannot
// state. Every refusal names the variable an operator must change.
//
// Strings arrive from the loader with "" for both an unset and an
// explicitly emptied variable -- the same value a direct environment read
// reports -- so every default and "unset" branch below treats "" as
// unset, and the surface's resolution rule is uniform across all
// variables.
func serverConfigFrom(hc hostConfig) (serverConfig, error) {
	deploymentModeStr := hc.DeploymentMode
	if deploymentModeStr == "" {
		deploymentModeStr = string(pkgcore.DeploymentModeStandalone)
	}
	deploymentMode, err := pkgcore.ParseDeploymentMode(deploymentModeStr)
	if err != nil {
		return serverConfig{}, err
	}

	port := hc.Port
	if port == "" {
		port = defaultPort
	}

	dbPath := hc.DBPath
	if dbPath == "" {
		dbPath = defaultSQLitePath
	}

	// Each of the five key materials resolves the same way: its own
	// variable's hex value when set (checked by parseKeyEnv -- see its own
	// doc comment), its documented development fallback otherwise (see
	// each dev* variable's doc comment). A malformed value must fail
	// startup with a precise message rather than surface later as an
	// opaque cipher error.
	configKey := devConfigKey
	if encoded := hc.ConfigKey; encoded != "" {
		decoded, decodeErr := parseKeyEnv("APP_CONFIG_KEY", encoded)
		if decodeErr != nil {
			return serverConfig{}, decodeErr
		}
		configKey = decoded
	}

	// The org invitation blind-index key: APP_ORG_INDEX_KEY when set,
	// devOrgIndexKey otherwise -- see the field's doc comment for why
	// this must be a key distinct from configKey rather than the same one
	// reused. This project's org blind indexer consumes it when this
	// composition wires org (org.NewModule's invitation serializer).
	orgIndexKey := devOrgIndexKey
	if encoded := hc.OrgIndexKey; encoded != "" {
		decoded, decodeErr := parseKeyEnv("APP_ORG_INDEX_KEY", encoded)
		if decodeErr != nil {
			return serverConfig{}, decodeErr
		}
		orgIndexKey = decoded
	}

	// The authn blind-index HMAC key, the authn PII cipher key and the pki
	// local-key cipher key: each resolves from its own variable when set,
	// its dev* fallback otherwise. An authn-wiring composition's server.go
	// consumes all three (authn.WithBlindIndexKey and the two serializers
	// registered before dbkit.Open); every other composition resolves them
	// without consuming them, per the uniform-surface doctrine.
	authnBlindIndexKey := devBlindIndexKey
	if encoded := hc.AuthnBlindIndexKey; encoded != "" {
		decoded, decodeErr := parseKeyEnv("APP_AUTHN_BLIND_INDEX_KEY", encoded)
		if decodeErr != nil {
			return serverConfig{}, decodeErr
		}
		authnBlindIndexKey = decoded
	}
	authnPIICipherKey := devPIICipherKey
	if encoded := hc.AuthnPIICipherKey; encoded != "" {
		decoded, decodeErr := parseKeyEnv("APP_AUTHN_PII_CIPHER_KEY", encoded)
		if decodeErr != nil {
			return serverConfig{}, decodeErr
		}
		authnPIICipherKey = decoded
	}
	pkiLocalKeyCipherKey := devPKILocalKeyCipherKey
	if encoded := hc.PKILocalKeyCipherKey; encoded != "" {
		decoded, decodeErr := parseKeyEnv("APP_PKI_LOCAL_KEY_CIPHER_KEY", encoded)
		if decodeErr != nil {
			return serverConfig{}, decodeErr
		}
		pkiLocalKeyCipherKey = decoded
	}

	// redisAddr stays empty when unset, leaving the "eventbus" and "kv"
	// seams on the Preset's in-process defaults -- see the APP_REDIS_ADDR
	// field's own doc comment above.
	redisAddr := hc.RedisAddr

	// otlpEndpoint stays empty when unset, leaving obs.Init on the local
	// exporters -- see the APP_OTLP_ENDPOINT field's own doc comment
	// above.
	otlpEndpoint := hc.OTLPEndpoint

	// s3Endpoint/s3Bucket/s3AccessKey/s3SecretKey stay empty when unset,
	// leaving the "objectstore" seam on the Preset's local-directory
	// default; when any one of them is set, all four are required -- see
	// the S3 group's own doc comment above.
	s3Endpoint := hc.S3Endpoint
	s3Bucket := hc.S3Bucket
	s3AccessKey := hc.S3AccessKey
	s3SecretKey := hc.S3SecretKey
	if s3Endpoint != "" || s3Bucket != "" || s3AccessKey != "" || s3SecretKey != "" {
		var missing []string
		if s3Endpoint == "" {
			missing = append(missing, "APP_S3_ENDPOINT")
		}
		if s3Bucket == "" {
			missing = append(missing, "APP_S3_BUCKET")
		}
		if s3AccessKey == "" {
			missing = append(missing, "APP_S3_ACCESS_KEY")
		}
		if s3SecretKey == "" {
			missing = append(missing, "APP_S3_SECRET_KEY")
		}
		if len(missing) > 0 {
			return serverConfig{}, fmt.Errorf(
				"__APP_NAME__: an S3 ObjectStore composition needs %s set too (got some but not all of APP_S3_ENDPOINT/APP_S3_BUCKET/APP_S3_ACCESS_KEY/APP_S3_SECRET_KEY)",
				strings.Join(missing, ", "))
		}
	}
	s3UseSSL := false
	if raw := hc.S3UseSSL; raw != "" {
		parsed, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return serverConfig{}, fmt.Errorf("__APP_NAME__: APP_S3_USE_SSL must be a valid bool, got %q: %w", raw, parseErr)
		}
		s3UseSSL = parsed
	}

	// s3BucketLookup is APP_S3_BUCKET_LOOKUP's parsed enum: unset and
	// "auto" keep the store's endpoint-derived default, the other two
	// legal values pin one addressing style each, and anything else is
	// refused here -- naming the variable -- rather than reaching the
	// store's own validation.
	s3BucketLookup := objectstores3.BucketLookupAuto
	switch strings.ToLower(strings.TrimSpace(hc.S3BucketLookup)) {
	case "path":
		s3BucketLookup = objectstores3.BucketLookupPath
	case "virtual_host":
		s3BucketLookup = objectstores3.BucketLookupVirtualHost
	case "", "auto":
		// The default: the endpoint-derived style stays selected.
	default:
		return serverConfig{}, fmt.Errorf(`__APP_NAME__: APP_S3_BUCKET_LOOKUP must be one of "auto", "path", "virtual_host", got %q`, hc.S3BucketLookup)
	}

	// smtpHost/smtpPortRaw mirror the S3 group above: both unset leaves
	// the "mailer" seam on its Preset default, and a partial APP_SMTP_*
	// set is refused rather than silently ignored.
	smtpHost := hc.SMTPHost
	smtpPortRaw := hc.SMTPPort
	var smtpPort int
	switch {
	case smtpHost == "" && smtpPortRaw == "":
		// Both unset: the "mailer" seam stays on its Preset default.
	case smtpHost == "" || smtpPortRaw == "":
		return serverConfig{}, fmt.Errorf(
			"__APP_NAME__: an SMTP Mailer composition needs both APP_SMTP_HOST and APP_SMTP_PORT set")
	default:
		parsed, parseErr := strconv.Atoi(smtpPortRaw)
		if parseErr != nil {
			return serverConfig{}, fmt.Errorf("__APP_NAME__: APP_SMTP_PORT must be a valid port number, got %q: %w", smtpPortRaw, parseErr)
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
		OTLPEndpoint:         otlpEndpoint,
		S3Endpoint:           s3Endpoint,
		S3Bucket:             s3Bucket,
		S3AccessKey:          s3AccessKey,
		S3SecretKey:          s3SecretKey,
		S3Region:             hc.S3Region,
		S3UseSSL:             s3UseSSL,
		S3BucketLookup:       s3BucketLookup,
		SMTPHost:             smtpHost,
		SMTPPort:             smtpPort,
		SMTPUsername:         hc.SMTPUsername,
		SMTPPassword:         hc.SMTPPassword,
		SMSGatewayURL:        hc.SMSGatewayURL,
	}, nil
}
