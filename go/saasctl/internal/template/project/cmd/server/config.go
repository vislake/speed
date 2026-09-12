//go:build ignore

package main

// This file is the generated project's bootstrap configuration surface: the
// loader target the assembly fills (hostConfig), the transform that turns it
// into the serverConfig the composition consumes, and the committed
// development keys zero-setup development falls back to.
//
// The six platform key materials are declared by the module components the
// composition imports -- each module declares its own key, and the
// declarations ride the components -- so this file restates none of them.
// The engine's loader resolves every declaration on its own chain: the key
// path's derived environment variable -- uppercased, dots doubled -- so
// config.cipher_key reads APP_CONFIG__CIPHER_KEY and authn.blind_index_key
// reads APP_AUTHN__BLIND_INDEX_KEY, and an unset variable leaves the
// development key below standing (bootstrapDevDefaults is the loader's
// lowest-priority table). A set variable must hold 64 hex characters (a
// 32-byte key), and anything else fails the load naming the key path and the
// variable the loader read. This project wires no root key (the committed
// development keys are its unset fallback), so the derivation tier stays
// unconfigured here.
//
// Every other variable of this project's surface is flat and
// single-underscored, a shape the loader's nesting derivation never
// produces, so each field below pins its exact spelling with the env tag
// option. Their resolution rules are the transform below's, which is also
// where a malformed or incomplete group is refused.

import (
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
	// alongside the declared defaults table (bootstrapDevDefaults) the
	// declared key materials fall back to. This project installs no root
	// key -- the committed development keys are the unset fallback, so no
	// APP_ROOT_KEY is read and the derivation tier stays unconfigured.
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
)

// devConfigKey is the config-module cipher key bootstrapDevDefaults carries
// when APP_CONFIG__CIPHER_KEY is unset. Zero-setup standalone development
// must work with no environment at all, while config's Sensitive items
// demand a real 32-byte key the moment one is declared (the config module's
// Attach fails with ErrCipherRequired otherwise), so this file provides one
// -- the ascending 0x00..0x1f byte sequence, chosen to be visibly a
// placeholder and to be clearly DIFFERENT from every other dev key's bytes
// (see devOrgIndexKey's own doc comment for why the secrets must never be
// the same).
//
// This default is a documented trade-off, not a pattern to copy: a key
// committed to the repository is not a secret, and real hosts must never
// do this. A real deployment sets APP_CONFIG__CIPHER_KEY from a secret
// store (or refuses to start); rotating the key loses every value the
// cipher sealed, so a real deployment's key must be stable for the life of
// its data.
var devConfigKey = []byte{
	0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f,
	0x10, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17,
	0x18, 0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f,
}

// devOrgIndexKey is the org invitation blind-index HMAC key
// bootstrapDevDefaults carries when APP_ORG__INVITATION_EMAIL_INDEX_KEY is
// unset: the descending 0xff..0xe0
// byte sequence, chosen precisely so it is visibly a DIFFERENT 32 bytes
// from devConfigKey's ascending 0x00..0x1f (the reason is dbkit's
// key-separation rule: an AES key must never double as an HMAC key). The
// same honest placeholder as devConfigKey -- never a secret a real
// deployment should keep -- and consumed only by org-wiring compositions.
var devOrgIndexKey = []byte{
	0xff, 0xfe, 0xfd, 0xfc, 0xfb, 0xfa, 0xf9, 0xf8,
	0xf7, 0xf6, 0xf5, 0xf4, 0xf3, 0xf2, 0xf1, 0xf0,
	0xef, 0xee, 0xed, 0xec, 0xeb, 0xea, 0xe9, 0xe8,
	0xe7, 0xe6, 0xe5, 0xe4, 0xe3, 0xe2, 0xe1, 0xe0,
}

// devBlindIndexKey, devPIICipherKey, devPKILocalKeyCipherKey and
// devNotificationIndexKey are the rest of this file's committed-key dev
// family, completing the byte-range pattern devConfigKey (ascending
// 0x00..0x1f) and devOrgIndexKey (descending 0xff..0xe0) began --
// 0x20..0x3f, 0x40..0x5f, 0x60..0x7f and 0x80..0x9f, each visibly a
// placeholder and visibly DIFFERENT from every other member of the family,
// because dbkit's key-separation rule (never let one key double as two
// different AEAD or HMAC constructions) applies across modules, not only
// within one.
//
// Each protects something different and each must stay stable across
// restarts for a different reason: devBlindIndexKey must stay IDENTICAL
// across restarts or every already-stored email/phone blind index
// becomes unfindable (authn.WithBlindIndexKey consumes it); devPIICipherKey
// seals authn's encrypted PII columns (email, phone, TOTP secrets) via
// authn.RegisterPIISerializer; devPKILocalKeyCipherKey seals go/pki's
// LocalSigner private-key column (pki_local_keys, via
// pki.RegisterLocalKeySerializer) -- the signing keys themselves are
// generated once by pki.Service.EnsurePurpose and PERSIST in this project's
// database across restarts, so no derivation from a dev seed is ever
// needed; and devNotificationIndexKey stands behind
// notification.contact_index_key, which this generator's selections do not
// wire today but which the declared defaults table carries uniformly, so a
// selection that later adds notification finds its key supplied.
//
// These are the same documented trade-off as devConfigKey's own: each is a
// FALLBACK applied only when its environment variable is unset, never a
// value that overrides a configured one. A key committed to the repository
// is not a secret, and a real deployment must set every one of the six key
// variables from a secret store, or refuse to start; rotating a key that
// was in use loses whatever it protected, so a real deployment's keys must
// be stable for the life of their data.
var (
	devNotificationIndexKey = []byte{
		0x20, 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27,
		0x28, 0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f,
		0x30, 0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37,
		0x38, 0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f,
	}
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

// hostConfig is the loader target carrying this project's own bootstrap
// surface: one pinned field per host variable. The six key materials are
// deliberately absent -- the importing module components' ConfigSchemas
// declare them as derive fields, and the assembly's loader resolves those
// fields into the bootstrap material the wiring reads. The three fields whose unset behavior is a
// scalar default -- DeploymentMode, Port and DBPath -- are pre-set by
// hostConfigDefaults as the loader's lowest-priority source; every other
// field's empty value is its documented unset behavior, handled by
// serverConfigFrom below.
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

	// RedisAddr names APP_REDIS_ADDR: the Redis server address ("host:port")
	// that composes a REAL, MultiReplicaSafe implementation for both the
	// "eventbus" and "kv" seams -- one Redis instance backs both, sharing
	// one client. Empty -- the default -- leaves both seams on the builtin composition's
	// in-process implementation, so `go run ./cmd/server` keeps working
	// with zero external dependencies; the distributed deployment mode
	// requires MultiReplicaSafe of every resolved seam
	// (pkgcore.DeploymentMode.RequiredCapabilities), so a distributed boot
	// with this unset fails the assembly's capability validation naming
	// "eventbus" (or "kv") rather than starting on an implementation that
	// cannot actually share state across replicas.
	RedisAddr string `config:"env=APP_REDIS_ADDR"`

	// OTLPEndpoint names APP_OTLP_ENDPOINT: the OTLP/gRPC endpoint
	// ("host:port") traces and metrics are pushed to. Empty -- the default
	// -- leaves the engine's observability init on the local exporters
	// (traces and metrics to stdout, plus the in-process Prometheus handler
	// the /metrics route serves once main.go's blank import of
	// go/observability/exporter/prometheus has registered it); set it to
	// push both signals at a collector over OTLP. Once an endpoint is set,
	// /metrics answers 404 by design -- there is no local registry to
	// scrape, the collector is where metrics go. The OTLP exporter factory
	// the endpoint composes is registered by main.go's blank import of
	// go/observability/exporter/otlp.
	OTLPEndpoint string `config:"env=APP_OTLP_ENDPOINT"`

	// S3Endpoint, S3Bucket, S3AccessKey and S3SecretKey name
	// APP_S3_ENDPOINT/APP_S3_BUCKET/APP_S3_ACCESS_KEY/APP_S3_SECRET_KEY:
	// the four variables that together compose a real S3-compatible
	// ObjectStore for the "objectstore" seam. All four are required
	// together; a partially set group is refused by serverConfigFrom rather
	// than silently falling back to the builtin composition's local-directory default,
	// since a partial S3 target is far more likely a typo than a deliberate
	// choice. S3Region, S3UseSSL and S3BucketLookup below refine the same
	// composition.
	S3Endpoint  string `config:"env=APP_S3_ENDPOINT"`
	S3Bucket    string `config:"env=APP_S3_BUCKET"`
	S3AccessKey string `config:"env=APP_S3_ACCESS_KEY"`
	// #nosec G101 -- this is an ENVIRONMENT VARIABLE NAME, not a credential
	// value: gosec's hardcoded-credential heuristic matches on the substring
	// "Secret" in the identifier alone.
	S3SecretKey string `config:"env=APP_S3_SECRET_KEY"`

	// S3Region names APP_S3_REGION: the region of the S3 composition above.
	// Region matters to AWS S3 (MinIO- and RustFS-compatible servers
	// ignore it). Optional, and an empty value means "unset" like every
	// other string field here.
	S3Region string `config:"env=APP_S3_REGION"`

	// S3UseSSL names APP_S3_USE_SSL: whether the S3 composition above
	// speaks TLS. Optional, and text rather than bool for hostConfig's own
	// reason: an emptied APP_S3_USE_SSL must keep meaning "unset" (the
	// documented default false), so serverConfigFrom parses the text itself
	// and refuses a value strconv.ParseBool rejects, naming the variable.
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
	// -- leaves the "mailer" seam on the builtin composition's console default, exactly
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
	// selection -- the uniform-surface doctrine -- but only consumed by a
	// selection whose server.go actually wires authn. Empty under the
	// standalone deployment mode leaves authn's "SMS sender" seam on its
	// console default; empty under the distributed deployment mode leaves
	// that seam deliberately UNWIRED, so authn's own wiring-time validation
	// fails closed with authn.ErrMissingDistributedSMSSender rather than an
	// authn-containing selection silently keeping a console sender nobody
	// in a distributed replica pool is reading.
	SMSGatewayURL string `config:"env=APP_SMS_GATEWAY_URL"`
}

// hostConfigDefaults returns the loader target with its three scalar
// defaults pre-set -- the loader's lowest-priority source, applied where no
// flag, environment variable or config file (none is wired here) supplied a
// value. Every other field's
// empty value is its documented unset behavior: the seam variables' "leave
// the builtin default" and the cross-variable rules all live in
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

// bootstrapDevDefaults returns this file's six committed development keys as
// the declared defaults table: the loader's lowest-priority source for the
// declared key materials, keyed by the declared key path each declaring
// module's component carries. It is handed to the assembly's loader options
// (the selection's loaderOptions()), which is the pass that resolves them;
// an entry naming a key no imported module declares is inert,
// so one table serves every selection -- a composition that imports no authn
// module simply has nothing to resolve authn.blind_index_key for.
func bootstrapDevDefaults() map[string][]byte {
	return map[string][]byte{
		"authn.blind_index_key":          devBlindIndexKey,
		"authn.pii_cipher_key":           devPIICipherKey,
		"config.cipher_key":              devConfigKey,
		"notification.contact_index_key": devNotificationIndexKey,
		"org.invitation_email_index_key": devOrgIndexKey,
		"pki.local_key_cipher_key":       devPKILocalKeyCipherKey,
	}
}

// loadHostConfig resolves the bootstrap surface from the process
// environment through the loader: flags first, then the environment
// (PORT and every pinned variable under its exact env tag), no config file,
// then hostConfigDefaults. The declared key materials resolve in the
// assembly's own pass, over the module components' declarations and the same
// process environment, with bootstrapDevDefaults as the table no individual
// variable overrides -- this pass exists so the values that depend on the
// loaded configuration (the database DSN, the listen address, the seam
// composition) are resolved before the engine assembles anything, and the
// engine's own pass re-resolves the identical sources into the identical
// target, so the two cannot disagree.
func loadHostConfig() (hostConfig, error) {
	hc := hostConfigDefaults()
	loader := config.New(config.WithEnvPrefix(envPrefix))
	if err := loader.Load(&hc); err != nil {
		return hostConfig{}, err
	}
	return hc, nil
}

// configFromEnv resolves the serverConfig and the loader target it was
// derived from, defaulting to the standalone deployment mode on SQLite so
// `go run ./cmd/server` genuinely starts a working server with zero
// external dependencies. It is loadHostConfig plus the transform below, in
// one call, for every caller that wants the assembled config rather than
// the raw surface. The loader target travels onward because the assembly
// hands it to the engine as the configuration target; the declared key
// materials resolve in the assembly's own pass.
func configFromEnv() (serverConfig, hostConfig, error) {
	hc, err := loadHostConfig()
	if err != nil {
		return serverConfig{}, hostConfig{}, err
	}
	cfg, err := serverConfigFrom(hc)
	if err != nil {
		return serverConfig{}, hostConfig{}, err
	}
	return cfg, hc, nil
}

// serverConfig is the generated project's bootstrap wiring configuration:
// the values a process must know before anything else can start (deployment
// mode, port, database path, the infrastructure seam addresses and the OTLP
// endpoint) -- everything except the six platform key materials, which the
// assembly's declared-key resolution carries. It is the transform
// output of the loader-filled hostConfig, NOT the dynamic configuration the
// config module serves: dynamic configuration lives in the configs table
// and can never hold the very key that encrypts it, so this bootstrap
// struct is the deliberate exception -- the one configuration a project's
// own process always reads from its environment.
type serverConfig struct {
	DeploymentMode pkgcore.DeploymentMode
	Port           string
	SQLitePath     string

	// RedisAddr, when non-empty, composes a real Redis-backed implementation
	// of both the "eventbus" and "kv" seams -- see the APP_REDIS_ADDR
	// field's own doc comment above.
	RedisAddr string

	// OTLPEndpoint, when non-empty, is handed to the observability
	// component's configuration in server.go, pushing traces and metrics to
	// it over OTLP -- see the APP_OTLP_ENDPOINT field's own doc comment
	// above for the default and the exporter registration the endpoint
	// needs.
	OTLPEndpoint string

	// S3Endpoint, S3Bucket, S3AccessKey, S3SecretKey, S3Region, S3UseSSL
	// and S3BucketLookup compose a real S3-compatible ObjectStore for the
	// "objectstore" seam when S3Endpoint is non-empty -- see the APP_S3_*
	// fields' own doc comment above for the completeness rule. Empty
	// S3Endpoint (the default) leaves "objectstore" on the builtin composition's
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
	// Empty SMTPHost (the default) leaves "mailer" on the builtin composition's console
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

// serverConfigFrom transforms a loaded bootstrap surface into the
// serverConfig the assembly consumes: it parses the deployment mode,
// parses the text-typed numeric and boolean variables, and enforces the
// cross-variable completeness rules (the S3 group and the SMTP pair) that a
// single-variable loader cannot state. Every refusal names the variable an
// operator must change.
//
// The six key materials are not part of this transform: the assembly's
// loader resolved them off the declaring components (an explicit
// 64-hex-character variable, or the declared defaults table), and the
// wiring reads them from the published bootstrap material.
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

	// redisAddr stays empty when unset, leaving the "eventbus" and "kv"
	// seams on the builtin composition's in-process defaults -- see the APP_REDIS_ADDR
	// field's own doc comment above.
	redisAddr := hc.RedisAddr

	// otlpEndpoint stays empty when unset, leaving the engine's
	// observability init on the local exporters -- see the
	// APP_OTLP_ENDPOINT field's own doc comment above.
	otlpEndpoint := hc.OTLPEndpoint

	// s3Endpoint/s3Bucket/s3AccessKey/s3SecretKey stay empty when unset,
	// leaving the "objectstore" seam on the builtin composition's local-directory
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
	// the "mailer" seam on its builtin default, and a partial APP_SMTP_*
	// set is refused rather than silently ignored.
	smtpHost := hc.SMTPHost
	smtpPortRaw := hc.SMTPPort
	var smtpPort int
	switch {
	case smtpHost == "" && smtpPortRaw == "":
		// Both unset: the "mailer" seam stays on its builtin default.
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
		DeploymentMode: deploymentMode,
		Port:           port,
		SQLitePath:     dbPath,
		RedisAddr:      redisAddr,
		OTLPEndpoint:   otlpEndpoint,
		S3Endpoint:     s3Endpoint,
		S3Bucket:       s3Bucket,
		S3AccessKey:    s3AccessKey,
		S3SecretKey:    s3SecretKey,
		S3Region:       hc.S3Region,
		S3UseSSL:       s3UseSSL,
		S3BucketLookup: s3BucketLookup,
		SMTPHost:       smtpHost,
		SMTPPort:       smtpPort,
		SMTPUsername:   hc.SMTPUsername,
		SMTPPassword:   hc.SMTPPassword,
		SMSGatewayURL:  hc.SMSGatewayURL,
	}, nil
}
