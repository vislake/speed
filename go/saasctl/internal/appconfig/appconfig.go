// Package appconfig parses the bootstrap environment surface of a generated
// consumer project: APP_DEPLOYMENT_MODE, PORT, APP_DB_PATH, APP_CONFIG_KEY,
// APP_ORG_INDEX_KEY, the three authn/pki key variables (APP_AUTHN_BLIND_INDEX_KEY,
// APP_AUTHN_PII_CIPHER_KEY, APP_PKI_LOCAL_KEY_CIPHER_KEY), APP_REDIS_ADDR,
// APP_OTLP_ENDPOINT, the APP_S3_* group, the APP_SMTP_* group and
// APP_SMS_GATEWAY_URL -- the full twenty-two-variable surface -- resolved
// exactly as the generated project's own cmd/server/config.go resolves
// them.
//
// saasctl's db and config commands must see what the app they act on would
// see: db migrate opens the same SQLite path the app's configFromEnv would
// open, and config print renders the bootstrap values with their true
// provenance -- refusing exactly when the generated app's own bootstrap
// would refuse to boot, on the identical incomplete infrastructure group.
// This package is therefore a deliberate, test-pinned twin of the embedded
// template file internal/template/project/cmd/server/config.go -- the same
// twenty-two variable names, the same defaults, the same completeness rules
// (an S3 group or an SMTP pair that is only partially set is refused, never
// silently dropped to the Preset default), the same development key bytes
// and the same malformed-value error texts, with the template's
// __APP_NAME__ token replaced by the real app name the caller derives from
// the project's go.mod. One default is deliberately a FIXED literal on both
// sides rather than anything derived from the app name: the unset-
// APP_DB_PATH database path (defaultSQLitePath), which the generated
// project freezes at materialization and which therefore cannot track a
// later module-path rename (see defaultSQLitePath's own doc comment).
//
// The twin is a sibling implementation, not a shared one, and that split is
// deliberate. The template resolves its surface through go/pkgcore/config's
// loader (each variable pinned by a field's env tag, three scalar defaults
// pre-set on the target struct) and then transforms the loaded text -- hex
// decoding, numeric and boolean parsing, the completeness refusals -- in the
// same file; the template file is a build-ignored asset this module cannot
// import, and the loader exposes no per-value provenance, which is exactly
// what config print renders. So the twin carries the same transform against
// its injectable LookupEnv source instead, and appconfig_test.go re-reads
// the embedded template and fails when the two sides drift: the variable
// names (the template's env tags against the exported constants above), the
// defaults, the parse order, the error texts and the development key bytes
// are all pinned, so a template edit that renames a variable, changes a
// default, reorders the resolution or rewrites an error text fails here
// before any generated app silently disagrees with the tool that maintains
// it. The loader step the twin does not mirror is behavior-neutral by
// construction: every template field is a string, so the loaded value is the
// variable's text (or "" for an unset or emptied one) with the target's
// defaults standing where no source supplied one -- the same values the
// twin's direct lookups produce.
//
// The environment is injectable through LookupEnv so every caller can
// decide its own source: the commands pass os.LookupEnv (a generated app
// reads the process environment, and so do they), tests pass a map.
package appconfig

import (
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vislake/speed/go/pkgcore"
)

// The twenty-two environment variable names of a generated project's
// bootstrap surface, exported because the command groups that render
// provenance and the tests that pin template parity all name the same
// variables.
const (
	// DeploymentModeEnv names the environment variable selecting a
	// generated project's deployment mode. Empty defaults to standalone
	// (Load), so a generated project boots with zero external dependencies.
	DeploymentModeEnv = "APP_DEPLOYMENT_MODE"

	// PortEnv names the HTTP listen port environment variable.
	PortEnv = "PORT"

	// DBPathEnv names the SQLite database path environment variable.
	DBPathEnv = "APP_DB_PATH"

	// ConfigKeyEnv names the environment variable holding the hex-encoded
	// 32-byte master key the config module seals Sensitive values with.
	ConfigKeyEnv = "APP_CONFIG_KEY"

	// OrgIndexKeyEnv names the environment variable holding the hex-encoded
	// 32-byte HMAC key an org-wiring project's blind indexer is built from.
	OrgIndexKeyEnv = "APP_ORG_INDEX_KEY"

	// AuthnBlindIndexKeyEnv names the environment variable holding the
	// hex-encoded 32-byte HMAC key an authn-wiring project's blind indexer
	// is built from (authn.WithBlindIndexKey). Parsed in every composition,
	// consumed only by authn-wiring ones -- the same uniform-surface
	// doctrine OrgIndexKeyEnv's template comment states.
	AuthnBlindIndexKeyEnv = "APP_AUTHN_BLIND_INDEX_KEY"

	// AuthnPIICipherKeyEnv names the environment variable holding the
	// hex-encoded 32-byte AES key that seals authn's encrypted PII columns
	// (authn.RegisterPIISerializer).
	AuthnPIICipherKeyEnv = "APP_AUTHN_PII_CIPHER_KEY"

	// PKILocalKeyCipherKeyEnv names the environment variable holding the
	// hex-encoded 32-byte AES key that seals go/pki's LocalSigner
	// private-key column (pki.RegisterLocalKeySerializer).
	PKILocalKeyCipherKeyEnv = "APP_PKI_LOCAL_KEY_CIPHER_KEY"

	// RedisAddrEnv names the environment variable holding the Redis server
	// address that composes a real, MultiReplicaSafe implementation for
	// both the "eventbus" and "kv" seams. Empty -- the default -- leaves
	// both seams on the Preset's in-process implementation.
	RedisAddrEnv = "APP_REDIS_ADDR"

	// OTLPEndpointEnv names the environment variable holding the OTLP/gRPC
	// endpoint traces and metrics are pushed to. Empty -- the default --
	// leaves obs.Init on the local exporters (stdout plus the Prometheus
	// scrape endpoint the /metrics route serves); set, obs.Init composes
	// the OTLP exporters and /metrics answers 404 by design -- there is
	// no local registry to scrape.
	OTLPEndpointEnv = "APP_OTLP_ENDPOINT"

	// S3EndpointEnv, S3BucketEnv, S3AccessKeyEnv and S3SecretKeyEnv
	// together compose a real S3-compatible ObjectStore for the
	// "objectstore" seam. All four are required together; a partially set
	// group is refused, exactly as the template refuses it. S3RegionEnv,
	// S3UseSSLEnv and S3BucketLookupEnv refine the same composition and
	// are optional.
	S3EndpointEnv = "APP_S3_ENDPOINT"
	S3BucketEnv   = "APP_S3_BUCKET"
	// #nosec G101 -- this is an ENVIRONMENT VARIABLE NAME, not a credential
	// value, mirroring the template's own #nosec exception for the identical
	// gosec false positive.
	S3AccessKeyEnv = "APP_S3_ACCESS_KEY"
	// #nosec G101 -- this is an ENVIRONMENT VARIABLE NAME, not a credential
	// value: gosec's hardcoded-credential heuristic matches on the substring
	// "Secret" in the identifier alone.
	S3SecretKeyEnv = "APP_S3_SECRET_KEY"
	S3RegionEnv    = "APP_S3_REGION"
	S3UseSSLEnv    = "APP_S3_USE_SSL"

	// S3BucketLookupEnv names the addressing style the S3 composition
	// reaches its bucket with, one of "auto" (the default: derived from
	// the endpoint), "path" (host/bucket/key) or "virtual_host"
	// (bucket.host/key); any other value is refused, naming the allowed
	// set. S3BucketLookup holds the parsed spelling.
	S3BucketLookupEnv = "APP_S3_BUCKET_LOOKUP"

	// SMTPHostEnv and SMTPPortEnv together compose a real SMTP Mailer for
	// the "mailer" seam; both are required together, for the same
	// fail-loud-on-partial-config reason S3EndpointEnv's doc comment gives.
	// SMTPUsernameEnv and SMTPPasswordEnv are optional.
	SMTPHostEnv     = "APP_SMTP_HOST"
	SMTPPortEnv     = "APP_SMTP_PORT"
	SMTPUsernameEnv = "APP_SMTP_USERNAME"
	// #nosec G101 -- this is an ENVIRONMENT VARIABLE NAME, not a credential
	// value, the identical false positive S3SecretKeyEnv's own #nosec
	// comment above excepts.
	SMTPPasswordEnv = "APP_SMTP_PASSWORD"

	// SMSGatewayURLEnv names the environment variable holding the endpoint
	// authn's real SMS transport posts delivery requests to. Parsed
	// unconditionally regardless of selection, consumed only by a selection
	// whose server.go wires authn.
	SMSGatewayURLEnv = "APP_SMS_GATEWAY_URL"
)

// defaultPort is used when the PORT environment variable is unset, the
// same default the generated server uses.
const defaultPort = "8080"

// defaultSQLitePath is the SQLite database path used when APP_DB_PATH is
// unset: a FIXED literal, byte-for-byte the generated cmd/server/config.go's
// own defaultSQLitePath constant (the twin test pins the equality). It is
// deliberately not derived from appName or from the module path the caller
// resolved: the generated project's own default is a literal frozen at
// materialization, so a twin that derived its own default from the CURRENT
// go.mod module path would fork from the app the moment a consumer renamed
// the module -- the CLI would migrate and print one file while the app
// opens another. One fixed name on both sides keeps CLI-then-boot
// agreement intact under any rename, and TestAppConfigIsTheGeneratedProjectsTwin
// fails if either side's literal moves without the other.
const defaultSQLitePath = "app.db"

// configKeyHexLength is the encoded length of the required 32-byte key (2
// hex characters per byte), checked so a short or malformed key fails
// configuration loading with a precise message rather than surfacing later
// as an opaque NewCipher error -- the template's exact check.
const configKeyHexLength = 64

// devConfigKey and devOrgIndexKey are the keys used when the respective
// environment variable is unset: the documented development defaults of a
// generated project, byte-for-byte the template's -- devConfigKey the
// ascending 0x00..0x1f sequence, devOrgIndexKey the descending 0xff..0xe0
// sequence chosen to be visibly a DIFFERENT 32 bytes (OrgIndexKey's doc
// comment explains why the two must never be the same secret). Like the
// template's own copies, they are honest placeholders, never secrets a
// real deployment should keep.
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

// devBlindIndexKey, devPIICipherKey and devPKILocalKeyCipherKey are the
// keys used when the respective environment variable above is unset: the
// template's own dev family for the three authn/pki key materials,
// byte-for-byte its copies -- devBlindIndexKey the 0x40..0x5f run
// (authn.WithBlindIndexKey's indexer), devPIICipherKey the 0x60..0x7f run
// (authn's PII serializer cipher), devPKILocalKeyCipherKey the 0x80..0x9f
// run (pki's local-key serializer cipher), each visibly a placeholder,
// each a distinct value (the template's doc comment explains why), and
// each a FALLBACK only -- an environment value always wins. Like the
// template's own copies, they are honest placeholders, never secrets a
// real deployment should keep.
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

// LookupEnv reads one environment variable. os.LookupEnv satisfies it; the
// injectable shape exists so tests (and callers that want a different
// source) can supply their own.
type LookupEnv func(key string) (string, bool)

// Config is a generated project's resolved bootstrap configuration: the
// values the generated cmd/server's configFromEnv would resolve, plus a
// per-field record of which value came from the environment. Everything is
// resolved exactly as the app resolves it -- an unset or empty variable
// falls back to the same default the generated server uses, and an
// incomplete infrastructure group (a partial S3 set, a partial SMTP pair)
// fails Load exactly as it fails the generated app's own configFromEnv --
// so db migrate opens the very database the app would open, and config
// print can render each value with its true provenance and refuse exactly
// when the generated app would refuse to boot.
type Config struct {
	DeploymentMode       pkgcore.DeploymentMode
	Port                 string
	SQLitePath           string
	ConfigKey            []byte
	OrgIndexKey          []byte
	AuthnBlindIndexKey   []byte
	AuthnPIICipherKey    []byte
	PKILocalKeyCipherKey []byte

	// RedisAddr, when non-empty, composes a real Redis-backed implementation
	// of both the "eventbus" and "kv" seams -- see RedisAddrEnv's own doc
	// comment above.
	RedisAddr string

	// OTLPEndpoint, when non-empty, is handed to obs.Init as
	// obs.WithOTLPEndpoint, pushing traces and metrics to it over OTLP --
	// see OTLPEndpointEnv's own doc comment above.
	OTLPEndpoint string

	// S3Endpoint, S3Bucket, S3AccessKey, S3SecretKey, S3Region, S3UseSSL
	// and S3BucketLookup compose a real S3-compatible ObjectStore for the
	// "objectstore" seam when S3Endpoint is non-empty -- see
	// S3EndpointEnv's own doc comment above for the completeness rule.
	// Empty S3Endpoint (the default) leaves "objectstore" on the Preset's
	// local-directory default. S3BucketLookup carries the parsed spelling
	// ("auto" when unset), never the raw text.
	S3Endpoint     string
	S3Bucket       string
	S3AccessKey    string
	S3SecretKey    string
	S3Region       string
	S3UseSSL       bool
	S3BucketLookup string

	// SMTPHost, SMTPPort, SMTPUsername and SMTPPassword compose a real SMTP
	// Mailer for the "mailer" seam when SMTPHost is non-empty -- see
	// SMTPHostEnv's own doc comment above for the completeness rule. Empty
	// SMTPHost (the default) leaves "mailer" on the Preset's console
	// default.
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string

	// SMSGatewayURL composes authn's real SMS transport for its "SMS
	// sender" seam when non-empty -- see SMSGatewayURLEnv's own doc comment
	// above for what an empty value means under each deployment mode.
	// Unused by a selection whose server.go wires no authn module.
	SMSGatewayURL string

	// DeploymentModeFromEnv through SMSGatewayURLFromEnv record, per field,
	// whether the environment variable carried a non-empty value. Empty
	// counts as unset, matching the generated app's own resolution (its
	// loader-loaded string fields arrive as "" for both an unset and an
	// emptied variable, and its transform treats "" as unset): a generated
	// server cannot distinguish "set to empty" from "unset", and neither
	// can this package.
	DeploymentModeFromEnv       bool
	PortFromEnv                 bool
	SQLitePathFromEnv           bool
	ConfigKeyFromEnv            bool
	OrgIndexKeyFromEnv          bool
	AuthnBlindIndexKeyFromEnv   bool
	AuthnPIICipherKeyFromEnv    bool
	PKILocalKeyCipherKeyFromEnv bool
	RedisAddrFromEnv            bool
	OTLPEndpointFromEnv         bool
	S3EndpointFromEnv           bool
	S3BucketFromEnv             bool
	S3AccessKeyFromEnv          bool
	S3SecretKeyFromEnv          bool
	S3RegionFromEnv             bool
	S3UseSSLFromEnv             bool
	S3BucketLookupFromEnv       bool
	SMTPHostFromEnv             bool
	SMTPPortFromEnv             bool
	SMTPUsernameFromEnv         bool
	SMTPPasswordFromEnv         bool
	SMSGatewayURLFromEnv        bool
}

// Load resolves a generated project's bootstrap configuration for appName
// -- the go.mod module path's final element, the name __APP_NAME__ stood
// for at materialization; appName appears in error texts only, never in
// the SQLite default, which is the fixed defaultSQLitePath literal -- the
// one default the generated app freezes rather than derives, so deriving
// it from the module path here would fork on a module rename (see
// defaultSQLitePath's own doc comment) -- reading the twenty-two
// environment variables through lookup. The parse order, defaults,
// completeness rules and failure texts mirror the generated configFromEnv
// exactly, including its error contract: a mode that does not parse is
// returned verbatim (no appName prefix), a malformed key variable or
// infrastructure-group value reports the app name prefixed in the
// template's exact wording, and an incomplete S3 group or SMTP pair is
// refused exactly as configFromEnv refuses it -- never silently dropped
// to the Preset default.
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
		dbPath = defaultSQLitePath
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

	// The three authn/pki key materials resolve exactly like the two
	// above: their own variable when set, their dev fallback otherwise.
	// Parsed in every composition, consumed only by authn-wiring ones
	// (each key's template Env doc comment says what it protects).
	authnBlindIndexKey, authnBlindIndexKeySet, err := loadKey(appName, AuthnBlindIndexKeyEnv, devBlindIndexKey, lookup)
	if err != nil {
		return Config{}, err
	}
	cfg.AuthnBlindIndexKeyFromEnv = authnBlindIndexKeySet
	cfg.AuthnBlindIndexKey = authnBlindIndexKey

	authnPIICipherKey, authnPIICipherKeySet, err := loadKey(appName, AuthnPIICipherKeyEnv, devPIICipherKey, lookup)
	if err != nil {
		return Config{}, err
	}
	cfg.AuthnPIICipherKeyFromEnv = authnPIICipherKeySet
	cfg.AuthnPIICipherKey = authnPIICipherKey

	pkiLocalKeyCipherKey, pkiLocalKeyCipherKeySet, err := loadKey(appName, PKILocalKeyCipherKeyEnv, devPKILocalKeyCipherKey, lookup)
	if err != nil {
		return Config{}, err
	}
	cfg.PKILocalKeyCipherKeyFromEnv = pkiLocalKeyCipherKeySet
	cfg.PKILocalKeyCipherKey = pkiLocalKeyCipherKey

	// redisAddr stays empty when unset, leaving the "eventbus" and "kv"
	// seams on the Preset's in-process defaults -- see RedisAddrEnv's own
	// doc comment above.
	redisAddr, _ := lookup(RedisAddrEnv)
	cfg.RedisAddr = redisAddr
	cfg.RedisAddrFromEnv = redisAddr != ""

	// otlpEndpoint stays empty when unset, leaving obs.Init on the local
	// exporters -- see OTLPEndpointEnv's own doc comment above.
	otlpEndpoint, _ := lookup(OTLPEndpointEnv)
	cfg.OTLPEndpoint = otlpEndpoint
	cfg.OTLPEndpointFromEnv = otlpEndpoint != ""

	// s3Endpoint/s3Bucket/s3AccessKey/s3SecretKey stay empty when unset,
	// leaving the "objectstore" seam on the Preset's local-directory
	// default; when any one of them is set, all four are required -- see
	// S3EndpointEnv's own doc comment above.
	s3Endpoint, _ := lookup(S3EndpointEnv)
	s3Bucket, _ := lookup(S3BucketEnv)
	s3AccessKey, _ := lookup(S3AccessKeyEnv)
	s3SecretKey, _ := lookup(S3SecretKeyEnv)
	if s3Endpoint != "" || s3Bucket != "" || s3AccessKey != "" || s3SecretKey != "" {
		var missing []string
		if s3Endpoint == "" {
			missing = append(missing, S3EndpointEnv)
		}
		if s3Bucket == "" {
			missing = append(missing, S3BucketEnv)
		}
		if s3AccessKey == "" {
			missing = append(missing, S3AccessKeyEnv)
		}
		if s3SecretKey == "" {
			missing = append(missing, S3SecretKeyEnv)
		}
		if len(missing) > 0 {
			return Config{}, fmt.Errorf(
				"%s: an S3 ObjectStore composition needs %s set too (got some but not all of %s/%s/%s/%s)",
				appName, strings.Join(missing, ", "), S3EndpointEnv, S3BucketEnv, S3AccessKeyEnv, S3SecretKeyEnv)
		}
	}
	cfg.S3Endpoint, cfg.S3EndpointFromEnv = s3Endpoint, s3Endpoint != ""
	cfg.S3Bucket, cfg.S3BucketFromEnv = s3Bucket, s3Bucket != ""
	cfg.S3AccessKey, cfg.S3AccessKeyFromEnv = s3AccessKey, s3AccessKey != ""
	cfg.S3SecretKey, cfg.S3SecretKeyFromEnv = s3SecretKey, s3SecretKey != ""

	s3UseSSLRaw, _ := lookup(S3UseSSLEnv)
	s3UseSSL := false
	if s3UseSSLRaw != "" {
		parsed, parseErr := strconv.ParseBool(s3UseSSLRaw)
		if parseErr != nil {
			return Config{}, fmt.Errorf("%s: %s must be a valid bool, got %q: %w", appName, S3UseSSLEnv, s3UseSSLRaw, parseErr)
		}
		s3UseSSL = parsed
	}
	cfg.S3UseSSL = s3UseSSL
	cfg.S3UseSSLFromEnv = s3UseSSLRaw != ""

	s3BucketLookupRaw, _ := lookup(S3BucketLookupEnv)
	s3BucketLookup := "auto"
	switch strings.ToLower(strings.TrimSpace(s3BucketLookupRaw)) {
	case "path":
		s3BucketLookup = "path"
	case "virtual_host":
		s3BucketLookup = "virtual_host"
	case "", "auto":
		// The default: the store derives the addressing style from the
		// endpoint.
	default:
		return Config{}, fmt.Errorf(`%s: %s must be one of "auto", "path", "virtual_host", got %q`, appName, S3BucketLookupEnv, s3BucketLookupRaw)
	}
	cfg.S3BucketLookup = s3BucketLookup
	cfg.S3BucketLookupFromEnv = s3BucketLookupRaw != ""

	// smtpHost/smtpPortRaw mirror s3Endpoint/... above: both unset leaves
	// the "mailer" seam on its Preset default, and a partial APP_SMTP_* set
	// is refused rather than silently ignored.
	smtpHost, _ := lookup(SMTPHostEnv)
	smtpPortRaw, _ := lookup(SMTPPortEnv)
	var smtpPort int
	switch {
	case smtpHost == "" && smtpPortRaw == "":
		// Both unset: the "mailer" seam stays on its Preset default.
	case smtpHost == "" || smtpPortRaw == "":
		return Config{}, fmt.Errorf(
			"%s: an SMTP Mailer composition needs both %s and %s set", appName, SMTPHostEnv, SMTPPortEnv)
	default:
		parsed, parseErr := strconv.Atoi(smtpPortRaw)
		if parseErr != nil {
			return Config{}, fmt.Errorf("%s: %s must be a valid port number, got %q: %w", appName, SMTPPortEnv, smtpPortRaw, parseErr)
		}
		smtpPort = parsed
	}
	cfg.SMTPHost, cfg.SMTPHostFromEnv = smtpHost, smtpHost != ""
	cfg.SMTPPort = smtpPort
	cfg.SMTPPortFromEnv = smtpPortRaw != ""

	s3Region, _ := lookup(S3RegionEnv)
	cfg.S3Region, cfg.S3RegionFromEnv = s3Region, s3Region != ""

	smtpUsername, _ := lookup(SMTPUsernameEnv)
	cfg.SMTPUsername, cfg.SMTPUsernameFromEnv = smtpUsername, smtpUsername != ""

	smtpPassword, _ := lookup(SMTPPasswordEnv)
	cfg.SMTPPassword, cfg.SMTPPasswordFromEnv = smtpPassword, smtpPassword != ""

	smsGatewayURL, _ := lookup(SMSGatewayURLEnv)
	cfg.SMSGatewayURL, cfg.SMSGatewayURLFromEnv = smsGatewayURL, smsGatewayURL != ""

	return cfg, nil
}

// EffectiveDBPath returns the SQLite database FILE the generated app of
// the project at modPath would actually open when booted from its own
// directory: SQLitePath used exactly as-is when absolute (an absolute
// APP_DB_PATH is used verbatim by the app, so it is used verbatim here
// too), and joined onto the directory of the go.mod argument when
// relative -- the unset-APP_DB_PATH default (defaultSQLitePath, the fixed
// app.db literal) or a relative APP_DB_PATH. Relative paths resolve
// against the process working directory, and the app is documented to run
// from its go.mod's directory, so a relative value names a file in that
// directory: joining onto filepath.Dir(modPath) is exactly that, whatever
// directory the caller invokes a command from. db migrate opens this
// function's answer and config print renders it, so the two commands'
// shared answer to "which file does this project open" is one function
// and cannot fork -- migrate's own doc comment on the anchoring carries
// the full argument.
func (c Config) EffectiveDBPath(modPath string) string {
	if filepath.IsAbs(c.SQLitePath) {
		return c.SQLitePath
	}
	return filepath.Join(filepath.Dir(modPath), c.SQLitePath)
}

// loadKey resolves one of the five hex-encoded 32-byte key variables: the
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
