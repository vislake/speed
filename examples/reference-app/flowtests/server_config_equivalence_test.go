package flowtests

// server_config_equivalence_test.go pins the behavioural equivalence of the
// two resolutions of this app's process environment: for every documented
// shape of the environment, the direct-read reference implementation
// (legacyConfigFromEnv below, kept in this file as the oracle) and the
// loader-driven app.ConfigFromEnv must produce the same effective
// ServerConfig -- the same dev-default path, the same APP_ROOT_KEY
// derivation, the same individual-override precedence, and the same
// cross-variable refusals naming the same variable.
//
// The one deliberate divergence -- an explicitly emptied int/bool variable is
// a load refusal rather than a silent "unset" -- is not equivalence and is
// pinned separately, by
// TestConfigFromEnv_EmptyTypedVariable_RefusesTheLoad at the bottom of this
// file.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/vislake/speed/examples/reference-app/internal/app"
	"github.com/vislake/speed/examples/reference-app/internal/app/demo"
	"github.com/vislake/speed/examples/reference-app/internal/testutil"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
)

// legacyConfigFromEnv is the direct-read reference implementation this
// file's oracle is built on: the os.Getenv reads with the per-variable
// rules the bootstrap fields' doc comments carry (an empty string reads as
// unset for every string-valued variable; the two bools and the two ints go
// through strconv, so an emptied one is their zero value; the six key
// materials resolve through the same three-tier precedence; the S3, SMTP,
// object-store and Fly-client-IP combinations refuse the boot with the same
// rule). It exists only as this file's oracle: production resolution is
// app.ConfigFromEnv for the host's own keys, and the assembly's
// declared-key resolution for the six materials the second return carries.
func legacyConfigFromEnv() (app.ServerConfig, map[string][]byte, error) {
	deploymentModeStr := os.Getenv("APP_DEPLOYMENT_MODE")
	if deploymentModeStr == "" {
		deploymentModeStr = string(pkgcore.DeploymentModeStandalone)
	}
	deploymentMode, err := pkgcore.ParseDeploymentMode(deploymentModeStr)
	if err != nil {
		return app.ServerConfig{}, nil, err
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = app.DefaultPort
	}

	dbPath := os.Getenv("APP_DB_PATH")
	if dbPath == "" {
		dbPath = app.DefaultSQLitePath
	}

	failProvisionCount := 0
	if raw := os.Getenv("APP_FAIL_SELF_SERVICE_PROVISION"); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil {
			return app.ServerConfig{}, nil, fmt.Errorf("reference-app: %s must be a whole number of provisioning attempts to fail (absent or 0 disables the injection), got %q: %w", "APP_FAIL_SELF_SERVICE_PROVISION", raw, parseErr)
		}
		if parsed < 0 {
			return app.ServerConfig{}, nil, fmt.Errorf("reference-app: %s must not be negative (absent or 0 disables the injection), got %d", "APP_FAIL_SELF_SERVICE_PROVISION", parsed)
		}
		failProvisionCount = parsed
	}

	var rootKey []byte
	if encoded := os.Getenv("APP_ROOT_KEY"); encoded != "" {
		decoded, decodeErr := legacyParseHexKeyEnv("APP_ROOT_KEY", encoded)
		if decodeErr != nil {
			return app.ServerConfig{}, nil, decodeErr
		}
		rootKey = decoded
	}

	configKey, err := legacyResolveKey(rootKey, "config.cipher_key", "APP_CONFIG__CIPHER_KEY", app.DevConfigKey)
	if err != nil {
		return app.ServerConfig{}, nil, err
	}
	orgIndexKey, err := legacyResolveKey(rootKey, "org.invitation_email_index_key", "APP_ORG__INVITATION_EMAIL_INDEX_KEY", app.DevOrgIndexKey)
	if err != nil {
		return app.ServerConfig{}, nil, err
	}
	notificationIndexKey, err := legacyResolveKey(rootKey, "notification.contact_index_key", "APP_NOTIFICATION__CONTACT_INDEX_KEY", app.DevNotificationIndexKey)
	if err != nil {
		return app.ServerConfig{}, nil, err
	}
	pkiLocalKeyCipherKey, err := legacyResolveKey(rootKey, "pki.local_key_cipher_key", "APP_PKI__LOCAL_KEY_CIPHER_KEY", app.DevPKILocalKeyCipherKey)
	if err != nil {
		return app.ServerConfig{}, nil, err
	}
	authnBlindIndexKey, err := legacyResolveKey(rootKey, "authn.blind_index_key", "APP_AUTHN__BLIND_INDEX_KEY", app.DevBlindIndexKey)
	if err != nil {
		return app.ServerConfig{}, nil, err
	}
	authnPIICipherKey, err := legacyResolveKey(rootKey, "authn.pii_cipher_key", "APP_AUTHN__PII_CIPHER_KEY", app.DevPIICipherKey)
	if err != nil {
		return app.ServerConfig{}, nil, err
	}

	s3Endpoint := os.Getenv("APP_S3_ENDPOINT")
	s3Bucket := os.Getenv("APP_S3_BUCKET")
	s3AccessKey := os.Getenv("APP_S3_ACCESS_KEY")
	s3SecretKey := os.Getenv("APP_S3_SECRET_KEY")
	if s3Endpoint != "" || s3Bucket != "" || s3AccessKey != "" || s3SecretKey != "" {
		if s3Endpoint == "" || s3Bucket == "" || s3AccessKey == "" || s3SecretKey == "" {
			return app.ServerConfig{}, nil, errors.New("reference-app: an incomplete APP_S3_* composition")
		}
	}
	s3UseSSL := false
	if raw := os.Getenv("APP_S3_USE_SSL"); raw != "" {
		parsed, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return app.ServerConfig{}, nil, fmt.Errorf("reference-app: %s must be a valid bool, got %q: %w", "APP_S3_USE_SSL", raw, parseErr)
		}
		s3UseSSL = parsed
	}

	readFlyClientIP := false
	if raw := os.Getenv("APP_READ_FLY_CLIENT_IP"); raw != "" {
		parsed, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return app.ServerConfig{}, nil, fmt.Errorf("reference-app: %s must be a valid bool, got %q: %w", "APP_READ_FLY_CLIENT_IP", raw, parseErr)
		}
		readFlyClientIP = parsed
	}
	if readFlyClientIP && len(legacySplitTrustedProxies(os.Getenv("APP_TRUSTED_PROXIES"))) == 0 {
		return app.ServerConfig{}, nil, errors.New("reference-app: APP_READ_FLY_CLIENT_IP is true but APP_TRUSTED_PROXIES is empty")
	}

	objectStoreRoot := os.Getenv("APP_OBJECT_STORE_ROOT")
	if objectStoreRoot != "" && s3Endpoint != "" {
		return app.ServerConfig{}, nil, errors.New("reference-app: APP_OBJECT_STORE_ROOT and an APP_S3_* composition name two different ObjectStores for one seam")
	}

	smtpHost := os.Getenv("APP_SMTP_HOST")
	smtpPortRaw := os.Getenv("APP_SMTP_PORT")
	var smtpPort int
	switch {
	case smtpHost == "" && smtpPortRaw == "":
	case smtpHost == "" || smtpPortRaw == "":
		return app.ServerConfig{}, nil, errors.New("reference-app: an SMTP Mailer composition needs both APP_SMTP_HOST and APP_SMTP_PORT set")
	default:
		parsed, parseErr := strconv.Atoi(smtpPortRaw)
		if parseErr != nil {
			return app.ServerConfig{}, nil, fmt.Errorf("reference-app: %s must be a valid port number, got %q: %w", "APP_SMTP_PORT", smtpPortRaw, parseErr)
		}
		smtpPort = parsed
	}

	publicOrigin := os.Getenv("APP_PUBLIC_ORIGIN")
	if publicOrigin == "" {
		publicOrigin = "http://localhost:" + port
	}

	// The six key materials the pre-declaration resolution produced, keyed by
	// declared key path: the material face of the equivalence comparison.
	materials := map[string][]byte{
		"config.cipher_key":              configKey,
		"org.invitation_email_index_key": orgIndexKey,
		"notification.contact_index_key": notificationIndexKey,
		"pki.local_key_cipher_key":       pkiLocalKeyCipherKey,
		"authn.blind_index_key":          authnBlindIndexKey,
		"authn.pii_cipher_key":           authnPIICipherKey,
	}

	cfg := app.ServerConfig{
		DeploymentMode:            deploymentMode,
		Port:                      port,
		SQLitePath:                dbPath,
		RedisAddr:                 os.Getenv("APP_REDIS_ADDR"),
		OTLPEndpoint:              os.Getenv("APP_OTLP_ENDPOINT"),
		S3Endpoint:                s3Endpoint,
		S3Bucket:                  s3Bucket,
		S3AccessKey:               s3AccessKey,
		S3SecretKey:               s3SecretKey,
		S3Region:                  os.Getenv("APP_S3_REGION"),
		S3UseSSL:                  s3UseSSL,
		S3BucketLookup:            os.Getenv("APP_S3_BUCKET_LOOKUP"),
		ObjectStoreRoot:           objectStoreRoot,
		SMTPHost:                  smtpHost,
		SMTPPort:                  smtpPort,
		SMTPUsername:              os.Getenv("APP_SMTP_USERNAME"),
		SMTPPassword:              os.Getenv("APP_SMTP_PASSWORD"),
		SMSGatewayURL:             os.Getenv("APP_SMS_GATEWAY_URL"),
		DisableQueueWorker:        os.Getenv("APP_DISABLE_QUEUE_WORKER") != "",
		DisableDemoUserHeader:     os.Getenv("APP_DISABLE_DEMO_USER_HEADER") != "",
		TrustedProxies:            legacySplitTrustedProxies(os.Getenv("APP_TRUSTED_PROXIES")),
		ReadFlyClientIP:           readFlyClientIP,
		WebDistDir:                os.Getenv("APP_WEB_DIST"),
		HostTenants:               demo.DemoHostTenants,
		PublicOrigin:              publicOrigin,
		DemoUsersPassword:         os.Getenv("APP_DEMO_USERS_PASSWORD"),
		DemoPlatformStaffPassword: os.Getenv("APP_DEMO_PLATFORM_STAFF_PASSWORD"),
		AIGatewayImageBaseURL:     os.Getenv("APP_AI_GATEWAY_IMAGE_BASE_URL"),
		AIGatewayImageAPIKey:      os.Getenv("APP_AI_GATEWAY_IMAGE_API_KEY"),
		FailSelfServiceProvision:  legacyProvisionFailureInjector(failProvisionCount),
	}
	// The SMTP group resolves to the four target fields alone, exactly like
	// production: no Mailer is built here either -- BuildServer composes
	// "mailer.smtp" from those fields through the builtin composition's config channel.
	return cfg, materials, nil
}

// legacyParseHexKeyEnv and legacyResolveKey mirror the deleted direct-read
// helpers' validation and precedence exactly, so the oracle's refusals land
// where production's do. The oracle derives through the platform composition
// (pkgcore.BootstrapKeyPurpose over the declared key path, then
// dbkit.DeriveKey) and spells each key's variable name itself -- production
// reads the name its loader derives from the embedded declaration's key path
// -- so a path or a spelling the two sides disagree on shows up as an
// equivalence failure rather than passing on both sides of a shared
// restatement.
func legacyParseHexKeyEnv(envName, encoded string) ([]byte, error) {
	if len(encoded) != 64 {
		return nil, fmt.Errorf("reference-app: %s must hold 64 hex characters (a 32-byte key), got %d", envName, len(encoded))
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("reference-app: %s: %w", envName, err)
	}
	return decoded, nil
}

func legacyResolveKey(rootKey []byte, keyPath, envName string, devDefault []byte) ([]byte, error) {
	key := devDefault
	if rootKey != nil {
		purpose, err := pkgcore.BootstrapKeyPurpose(keyPath)
		if err != nil {
			return nil, fmt.Errorf("reference-app: derive %s from APP_ROOT_KEY: %w", keyPath, err)
		}
		derived, err := dbkit.DeriveKey(rootKey, purpose)
		if err != nil {
			return nil, fmt.Errorf("reference-app: derive %s from APP_ROOT_KEY: %w", keyPath, err)
		}
		key = derived
	}
	if encoded := os.Getenv(envName); encoded != "" {
		decoded, err := legacyParseHexKeyEnv(envName, encoded)
		if err != nil {
			return nil, err
		}
		key = decoded
	}
	return key, nil
}

func legacySplitTrustedProxies(raw string) []string {
	if raw == "" {
		return nil
	}
	var proxies []string
	for _, entry := range strings.Split(raw, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			proxies = append(proxies, entry)
		}
	}
	return proxies
}

// legacyProvisionFailureInjector mirrors newProvisionFailureInjector's
// per-account budget, so the oracle's hook can be compared behaviourally with
// the resolved one.
func legacyProvisionFailureInjector(count int) func(userID string) error {
	if count < 1 {
		return nil
	}
	remaining := make(map[string]int)
	return func(userID string) error {
		left, seen := remaining[userID]
		if !seen {
			left = count
		}
		if left > 0 {
			remaining[userID] = left - 1
			return errors.New("APP_FAIL_SELF_SERVICE_PROVISION injected a provisioning failure")
		}
		return nil
	}
}

// equivalenceEnvCase is one documented environment shape: the variables the
// case sets (after testutil.ClearBootstrapEnv has unset everything), plus the
// variable name a refusal must name when the case is a refusal case.
type equivalenceEnvCase struct {
	name    string
	env     map[string]string
	wantErr string
	// refusalAt names the layer whose resolution refuses the case:
	// refusalAtConfig (the default) is the host pass ConfigFromEnv drives,
	// refusalAtMaterial the assembly's declared-key resolution. The oracle
	// resolves both layers, so either refusal is the same pre-declaration
	// semantics; the field pins where today's resolution surfaces it.
	refusalAt string
}

const (
	refusalAtConfig   = "config"
	refusalAtMaterial = "material"
)

// equivalenceEnvCases is the documented case set the migration must preserve:
// the dev-default path, the root-derivation path, the individual-override
// path, the empty-string path, every composition refusal, and the fully
// configured composition.
func equivalenceEnvCases() []equivalenceEnvCase {
	completeS3 := map[string]string{
		"APP_S3_ENDPOINT":      "https://objects.example.test",
		"APP_S3_BUCKET":        "bucket",
		"APP_S3_ACCESS_KEY":    "access-key",
		"APP_S3_SECRET_KEY":    "secret-key",
		"APP_S3_REGION":        "eu-central-1",
		"APP_S3_USE_SSL":       "true",
		"APP_S3_BUCKET_LOOKUP": "path",
	}
	completeSMTP := map[string]string{
		"APP_SMTP_HOST":     "smtp.example.test",
		"APP_SMTP_PORT":     "587",
		"APP_SMTP_USERNAME": "mailer@example.test",
		"APP_SMTP_PASSWORD": "smtp-password",
	}
	rootSecret := "5f4dcc3b5aa765d61d8327deb882cf99aabbccddeeff00112233445566778899"
	return []equivalenceEnvCase{
		{name: "zero environment (dev defaults)"},
		{
			name: "empty strings on every string-valued variable read as unset",
			env: map[string]string{
				"APP_DEPLOYMENT_MODE": "", "PORT": "", "APP_DB_PATH": "",
				"APP_REDIS_ADDR": "", "APP_OTLP_ENDPOINT": "", "APP_PUBLIC_ORIGIN": "",
				"APP_WEB_DIST": "", "APP_TRUSTED_PROXIES": "",
				"APP_OBJECT_STORE_ROOT": "", "APP_SMTP_HOST": "", "APP_SMTP_USERNAME": "",
				"APP_SMTP_PASSWORD": "", "APP_SMS_GATEWAY_URL": "",
				"APP_S3_BUCKET_LOOKUP": "",
				"APP_ROOT_KEY":         "", "APP_CONFIG__CIPHER_KEY": "", "APP_ORG__INVITATION_EMAIL_INDEX_KEY": "",
				"APP_NOTIFICATION__CONTACT_INDEX_KEY": "", "APP_PKI__LOCAL_KEY_CIPHER_KEY": "",
				"APP_AUTHN__BLIND_INDEX_KEY": "", "APP_AUTHN__PII_CIPHER_KEY": "",
				"APP_DEMO_USERS_PASSWORD": "", "APP_DEMO_PLATFORM_STAFF_PASSWORD": "",
				"APP_AI_GATEWAY_IMAGE_BASE_URL": "", "APP_AI_GATEWAY_IMAGE_API_KEY": "",
				"APP_DISABLE_QUEUE_WORKER": "", "APP_DISABLE_DEMO_USER_HEADER": "",
			},
		},
		{
			name: "every variable set",
			env: map[string]string{
				"APP_DEPLOYMENT_MODE": "distributed", "PORT": "9999",
				"APP_DB_PATH":    "/tmp/reference-app-equivalence.db",
				"APP_REDIS_ADDR": "127.0.0.1:6380", "APP_OTLP_ENDPOINT": "collector.example.test:4317",
				"APP_PUBLIC_ORIGIN": "https://app.example.test", "APP_WEB_DIST": "/srv/web/dist",
				"APP_TRUSTED_PROXIES":              " 172.16.0.0/12, 203.0.113.10 , ",
				"APP_READ_FLY_CLIENT_IP":           "true",
				"APP_DEMO_USERS_PASSWORD":          "demo users passphrase",
				"APP_DEMO_PLATFORM_STAFF_PASSWORD": "platform staff passphrase",
				"APP_AI_GATEWAY_IMAGE_BASE_URL":    "http://images.example.test/v1",
				"APP_AI_GATEWAY_IMAGE_API_KEY":     "demo-image-key",
				"APP_DISABLE_QUEUE_WORKER":         "1", "APP_DISABLE_DEMO_USER_HEADER": "yes",
				"APP_FAIL_SELF_SERVICE_PROVISION": "3",
			},
		},
		{
			name: "a complete S3 composition",
			env:  maps(completeS3),
		},
		{
			name: "a complete SMTP composition with AUTH credentials",
			env:  maps(completeSMTP),
		},
		{
			name: "an SMS gateway alone",
			env:  map[string]string{"APP_SMS_GATEWAY_URL": "https://sms.example.test/send"},
		},
		{
			name: "root key alone derives all six materials",
			env:  map[string]string{"APP_ROOT_KEY": rootSecret},
		},
		{
			name: "an individual key overrides its derivation",
			env: map[string]string{
				"APP_ROOT_KEY":           rootSecret,
				"APP_CONFIG__CIPHER_KEY": "0f0e0d0c0b0a090807060504030201001f1e1d1c1b1a19181716151413121110",
				"APP_DEPLOYMENT_MODE":    "standalone",
			},
		},
		{
			name:    "invalid deployment mode refuses",
			env:     map[string]string{"APP_DEPLOYMENT_MODE": "not-a-deployment-mode"},
			wantErr: "not-a-deployment-mode",
		},
		{
			name:      "a malformed individual key refuses at the assembly's resolution",
			env:       map[string]string{"APP_ORG__INVITATION_EMAIL_INDEX_KEY": "not-64-hex-chars"},
			wantErr:   "APP_ORG__INVITATION_EMAIL_INDEX_KEY",
			refusalAt: refusalAtMaterial,
		},
		{
			name:    "a malformed root key refuses",
			env:     map[string]string{"APP_ROOT_KEY": "zzzz"},
			wantErr: "APP_ROOT_KEY",
		},
		{
			name:    "an incomplete S3 composition refuses",
			env:     map[string]string{"APP_S3_ENDPOINT": "https://objects.example.test"},
			wantErr: "APP_S3_BUCKET",
		},
		{
			name:    "an incomplete SMTP composition refuses",
			env:     map[string]string{"APP_SMTP_HOST": "smtp.example.test"},
			wantErr: "APP_SMTP_PORT",
		},
		{
			name:    "a non-numeric SMTP port refuses",
			env:     map[string]string{"APP_SMTP_HOST": "smtp.example.test", "APP_SMTP_PORT": "not-a-port"},
			wantErr: "APP_SMTP_PORT",
		},
		{
			name:    "the Fly declaration without declared proxies refuses",
			env:     map[string]string{"APP_READ_FLY_CLIENT_IP": "true"},
			wantErr: "APP_TRUSTED_PROXIES",
		},
		{
			name: "an object-store root beside an S3 composition refuses",
			env: maps(completeS3, map[string]string{
				"APP_OBJECT_STORE_ROOT": "/var/lib/reference-app/objects",
			}),
			wantErr: "APP_OBJECT_STORE_ROOT",
		},
		{
			name:    "a negative provisioning-failure count refuses",
			env:     map[string]string{"APP_FAIL_SELF_SERVICE_PROVISION": "-1"},
			wantErr: "APP_FAIL_SELF_SERVICE_PROVISION",
		},
		{
			name:    "a non-numeric provisioning-failure count refuses",
			env:     map[string]string{"APP_FAIL_SELF_SERVICE_PROVISION": "one"},
			wantErr: "APP_FAIL_SELF_SERVICE_PROVISION",
		},
	}
}

func maps(parts ...map[string]string) map[string]string {
	merged := map[string]string{}
	for _, part := range parts {
		for key, value := range part {
			merged[key] = value
		}
	}
	return merged
}

// TestConfigFromEnv_MatchesThePreLoaderResolution is the equivalence pin: for
// every case, the oracle and the loader-driven resolution must agree -- the
// same ServerConfig, or a refusal naming the same variable.
func TestConfigFromEnv_MatchesThePreLoaderResolution(t *testing.T) {
	for _, tc := range equivalenceEnvCases() {
		t.Run(tc.name, func(t *testing.T) {
			testutil.ClearBootstrapEnv(t)
			for key, value := range tc.env {
				t.Setenv(key, value)
			}

			want, wantMaterial, wantErr := legacyConfigFromEnv()
			got, gotErr := app.ConfigFromEnv()

			if tc.wantErr == "" {
				if wantErr != nil || gotErr != nil {
					t.Fatalf("resolution errors: legacy %v, loader-driven %v; want none", wantErr, gotErr)
				}
				if err := serverConfigsEquivalent(got, want); err != nil {
					t.Fatalf("loader-driven ConfigFromEnv diverged from the pre-loader resolution: %v", err)
				}

				// The material face: the same environment, resolved by the
				// assembly's declared-key chain, must reproduce the oracle's
				// six materials key by key.
				material, materialErr := resolveDeclaredMaterial(t)
				if materialErr != nil {
					t.Fatalf("resolving the declared key material: %v", materialErr)
				}
				for keyPath, wantKey := range wantMaterial {
					gotKey := declaredMaterialKey(t, material, keyPath)
					if !bytes.Equal(gotKey, wantKey) {
						t.Errorf("declared material %s = %x, want the pre-declaration resolution's %x", keyPath, gotKey, wantKey)
					}
				}
				return
			}
			if wantErr == nil {
				t.Fatalf("legacy resolution accepted the case, want a refusal naming %s", tc.wantErr)
			}

			if tc.refusalAt == refusalAtMaterial {
				// The host pass resolves none of the declared keys, so its
				// own resolution accepts the environment; the refusal is the
				// assembly's, naming the same variable the oracle named.
				if gotErr != nil {
					t.Fatalf("ConfigFromEnv refused a case whose refusal belongs to the declared-key resolution: %v", gotErr)
				}
				_, materialErr := resolveDeclaredMaterial(t)
				if materialErr == nil {
					t.Fatalf("the declared-key resolution accepted the case, want a refusal naming %s", tc.wantErr)
				}
				if !strings.Contains(materialErr.Error(), tc.wantErr) {
					t.Errorf("declared-key refusal does not name %s: %v", tc.wantErr, materialErr)
				}
				return
			}

			if gotErr == nil {
				t.Fatalf("loader-driven ConfigFromEnv accepted the case, want a refusal naming %s", tc.wantErr)
			}
			if !strings.Contains(gotErr.Error(), tc.wantErr) {
				t.Errorf("loader-driven refusal does not name %s: %v", tc.wantErr, gotErr)
			}
		})
	}
}

// TestConfigFromEnv_EmptyTypedVariable_RefusesTheLoad pins the one case the
// equivalence set above cannot express: the four variables whose fields are
// an int or a bool are refused when explicitly emptied, where the direct-read
// oracle treats "" as unset. The refusal is the loader's own -- a field with
// no representation for an empty value -- and it is what keeps an unset shell
// variable behind an empty value from silently booting on a zero.
func TestConfigFromEnv_EmptyTypedVariable_RefusesTheLoad(t *testing.T) {
	for _, name := range []string{
		"APP_S3_USE_SSL",
		"APP_READ_FLY_CLIENT_IP",
		"APP_SMTP_PORT",
		"APP_FAIL_SELF_SERVICE_PROVISION",
	} {
		t.Run(name, func(t *testing.T) {
			testutil.ClearBootstrapEnv(t)
			t.Setenv(name, "")

			if _, err := app.ConfigFromEnv(); err == nil {
				t.Fatalf("ConfigFromEnv with %s emptied: want a load refusal, got nil", name)
			} else if !strings.Contains(err.Error(), name) {
				t.Errorf("refusal does not name %s: %v", name, err)
			}

			// The direct-read oracle accepts the same injection and reads it
			// as unset -- the divergence this pin records.
			if _, _, err := legacyConfigFromEnv(); err != nil {
				t.Fatalf("oracle no longer accepts the emptied %s, so this pin no longer records a behaviour change: %v", name, err)
			}
		})
	}
}

// serverConfigValues is the comparable projection of a resolved configuration:
// every field whose value is directly comparable, with the two fields that
// carry a function or a seam implementation left to their own comparisons
// below.
type serverConfigValues struct {
	deploymentMode            pkgcore.DeploymentMode
	port                      string
	sqlitePath                string
	redisAddr                 string
	otlpEndpoint              string
	s3Endpoint                string
	s3Bucket                  string
	s3AccessKey               string
	s3SecretKey               string
	s3Region                  string
	s3UseSSL                  bool
	s3BucketLookup            string
	objectStoreRoot           string
	smtpHost                  string
	smtpPort                  int
	smtpUsername              string
	smtpPassword              string
	smsGatewayURL             string
	disableQueueWorker        bool
	disableDemoUserHeader     bool
	trustedProxies            string
	readFlyClientIP           bool
	webDistDir                string
	hostTenants               string
	publicOrigin              string
	demoUsersPassword         string
	demoPlatformStaffPassword string
	aiGatewayImageBaseURL     string
	aiGatewayImageAPIKey      string
}

// comparableValues projects a resolved configuration onto the comparable
// fields, so the equivalence comparison never runs reflect.DeepEqual over the
// struct's function- or interface-typed fields (which DeepEqual cannot judge
// usefully).
func comparableValues(c app.ServerConfig) serverConfigValues {
	return serverConfigValues{
		deploymentMode:            c.DeploymentMode,
		port:                      c.Port,
		sqlitePath:                c.SQLitePath,
		redisAddr:                 c.RedisAddr,
		otlpEndpoint:              c.OTLPEndpoint,
		s3Endpoint:                c.S3Endpoint,
		s3Bucket:                  c.S3Bucket,
		s3AccessKey:               c.S3AccessKey,
		s3SecretKey:               c.S3SecretKey,
		s3Region:                  c.S3Region,
		s3UseSSL:                  c.S3UseSSL,
		s3BucketLookup:            c.S3BucketLookup,
		objectStoreRoot:           c.ObjectStoreRoot,
		smtpHost:                  c.SMTPHost,
		smtpPort:                  c.SMTPPort,
		smtpUsername:              c.SMTPUsername,
		smtpPassword:              c.SMTPPassword,
		smsGatewayURL:             c.SMSGatewayURL,
		disableQueueWorker:        c.DisableQueueWorker,
		disableDemoUserHeader:     c.DisableDemoUserHeader,
		trustedProxies:            fmt.Sprint(c.TrustedProxies),
		readFlyClientIP:           c.ReadFlyClientIP,
		webDistDir:                c.WebDistDir,
		hostTenants:               fmt.Sprint(c.HostTenants),
		publicOrigin:              c.PublicOrigin,
		demoUsersPassword:         c.DemoUsersPassword,
		demoPlatformStaffPassword: c.DemoPlatformStaffPassword,
		aiGatewayImageBaseURL:     c.AIGatewayImageBaseURL,
		aiGatewayImageAPIKey:      c.AIGatewayImageAPIKey,
	}
}

// serverConfigsEquivalent compares two resolved configurations field by field,
// holding the two fields that carry a function or a seam implementation to
// their own comparisons.
func serverConfigsEquivalent(got, want app.ServerConfig) error {
	if gotValues, wantValues := comparableValues(got), comparableValues(want); !reflect.DeepEqual(gotValues, wantValues) {
		return fmt.Errorf("fields differ:\n got: %+v\nwant: %+v", gotValues, wantValues)
	}
	if (got.Mailer == nil) != (want.Mailer == nil) {
		return fmt.Errorf("Mailer presence differs: got %v, want %v", got.Mailer, want.Mailer)
	}
	if got.Mailer != nil && !reflect.DeepEqual(got.Mailer, want.Mailer) {
		return fmt.Errorf("Mailer differs: got %#v, want %#v", got.Mailer, want.Mailer)
	}
	if (got.FailSelfServiceProvision == nil) != (want.FailSelfServiceProvision == nil) {
		return fmt.Errorf("FailSelfServiceProvision presence differs")
	}
	if got.FailSelfServiceProvision != nil {
		// Functions are not comparable, so the two hooks are compared by
		// driving them: the same account sequence must fail and succeed
		// identically (a per-account budget, so an interleaved sequence is a
		// stronger probe than one account alone).
		sequence := []string{"account-a", "account-b", "account-a", "account-a", "account-b", "account-c", "account-a", "account-b"}
		for i, account := range sequence {
			gotErr := got.FailSelfServiceProvision(account)
			wantErr := want.FailSelfServiceProvision(account)
			if (gotErr == nil) != (wantErr == nil) {
				return fmt.Errorf("FailSelfServiceProvision attempt %d (%s): gotErrIsNil=%t, wantErrIsNil=%t", i, account, gotErr == nil, wantErr == nil)
			}
		}
	}
	return nil
}

// TestServerConfigsEquivalent_DetectsADifference guards the helper itself: two
// configurations that differ in one compared field must not compare equal, or
// every equivalence case above would pass vacuously.
func TestServerConfigsEquivalent_DetectsADifference(t *testing.T) {
	base := app.ServerConfig{Port: "8080", SQLitePath: "reference-app.db"}
	if err := serverConfigsEquivalent(base, base); err != nil {
		t.Fatalf("identical configurations compared unequal: %v", err)
	}
	changed := base
	changed.Port = "9090"
	if err := serverConfigsEquivalent(changed, base); err == nil {
		t.Fatal("configurations differing in Port compared equal, want a difference")
	}
	changed = base
	changed.FailSelfServiceProvision = func(string) error { return errors.New("injected") }
	if err := serverConfigsEquivalent(changed, base); err == nil {
		t.Fatal("configurations differing in FailSelfServiceProvision presence compared equal, want a difference")
	}
}

// TestLegacyProvisionFailureInjector_MatchesTheResolvedHook is the oracle's own
// self-check: the test-local injector must behave like the production hook the
// resolution arms, or the equivalence comparison for that field would be
// checking two differently-shaped budgets.
func TestLegacyProvisionFailureInjector_MatchesTheResolvedHook(t *testing.T) {
	testutil.ClearBootstrapEnv(t)
	t.Setenv("APP_FAIL_SELF_SERVICE_PROVISION", "2")

	cfg, err := app.ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv: %v", err)
	}
	if cfg.FailSelfServiceProvision == nil {
		t.Fatal("FailSelfServiceProvision is nil with the variable at 2")
	}
	oracle := legacyProvisionFailureInjector(2)
	for i, account := range []string{"a", "b", "a", "a", "b"} {
		gotErr := cfg.FailSelfServiceProvision(account)
		wantErr := oracle(account)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("attempt %d (%s): resolved hook err=%v, oracle err=%v", i, account, gotErr, wantErr)
		}
	}
	material, err := resolveDeclaredMaterial(t)
	if err != nil {
		t.Fatalf("resolve the declared key material: %v", err)
	}
	if got := declaredMaterialKey(t, material, "config.cipher_key"); !bytes.Equal(got, app.DevConfigKey) {
		t.Fatalf("config.cipher_key material = %x, want the dev default %x", got, app.DevConfigKey)
	}
}
