// Package testutil carries test-support helpers shared by more than one of
// this app's test packages -- the command's own suites under cmd/server and
// the app-level ones under flowtests -- so the two sides cannot drift apart.
// It is test-only: nothing in this app's executable code may import it.
package testutil

import (
	"os"
	"testing"
)

// typedBootstrapEnvNames are the bootstrap variables this app's loader target
// declares as an int or bool field, by the variable name its pin states. An
// explicitly emptied one of these is a load-time refusal -- a field of no
// string representation has no "unset through an empty value" reading -- so
// clearing them means removing them from the environment, not emptying them
// (ClearBootstrapEnv below).
var typedBootstrapEnvNames = map[string]struct{}{
	"APP_S3_USE_SSL":                  {},
	"APP_READ_FLY_CLIENT_IP":          {},
	"APP_SMTP_PORT":                   {},
	"APP_FAIL_SELF_SERVICE_PROVISION": {},
}

// bootstrapEnvNames is every variable the reference app's bootstrap surface
// reads (internal/app's bootstrap.go): the host fields' pins as they spell
// them, and the six platform key materials' variables as the loader derives
// them from the embedded declaration's key paths -- the path's dot spelled
// as a double underscore under the APP_ prefix.
var bootstrapEnvNames = []string{
	"APP_DEPLOYMENT_MODE",
	"PORT",
	"APP_DB_PATH",
	"APP_REDIS_ADDR",
	"APP_OTLP_ENDPOINT",
	"APP_PUBLIC_ORIGIN",
	"APP_WEB_DIST",
	"APP_TRUSTED_PROXIES",
	"APP_READ_FLY_CLIENT_IP",
	"APP_S3_ENDPOINT",
	"APP_S3_BUCKET",
	"APP_S3_ACCESS_KEY",
	"APP_S3_SECRET_KEY",
	"APP_S3_REGION",
	"APP_S3_USE_SSL",
	"APP_S3_BUCKET_LOOKUP",
	"APP_OBJECT_STORE_ROOT",
	"APP_SMTP_HOST",
	"APP_SMTP_PORT",
	"APP_SMTP_USERNAME",
	"APP_SMTP_PASSWORD",
	"APP_SMS_GATEWAY_URL",
	"APP_ROOT_KEY",
	"APP_CONFIG__CIPHER_KEY",
	"APP_ORG__INVITATION_EMAIL_INDEX_KEY",
	"APP_NOTIFICATION__CONTACT_INDEX_KEY",
	"APP_PKI__LOCAL_KEY_CIPHER_KEY",
	"APP_AUTHN__BLIND_INDEX_KEY",
	"APP_AUTHN__PII_CIPHER_KEY",
	"APP_DEMO_USERS_PASSWORD",
	"APP_DEMO_PLATFORM_STAFF_PASSWORD",
	"APP_AI_GATEWAY_IMAGE_BASE_URL",
	"APP_AI_GATEWAY_IMAGE_API_KEY",
	"APP_DISABLE_QUEUE_WORKER",
	"APP_DISABLE_DEMO_USER_HEADER",
	"APP_FAIL_SELF_SERVICE_PROVISION",
}

// ClearBootstrapEnv clears the reference app's whole bootstrap surface from
// the process environment for the duration of the test, so that a test's
// outcome never depends on the ambient environment a test binary happens to
// run in -- PORT in particular is commonly preset by hosting platforms, and an
// ambient value would make a boot test fail (or pass for the wrong reason)
// outside a clean shell. Every variable is restored once the test finishes.
//
// A string-valued variable is emptied with t.Setenv, which the loader reads
// exactly as a direct read would: the field arrives at "" and the transform
// resolves it to the same default an unset variable gets. A variable whose
// field is an int or bool is removed from the environment instead, because an
// explicitly emptied one refuses the load rather than reading as unset.
func ClearBootstrapEnv(t *testing.T) {
	t.Helper()
	for _, name := range bootstrapEnvNames {
		if _, typed := typedBootstrapEnvNames[name]; typed {
			UnsetEnv(t, name)
			continue
		}
		t.Setenv(name, "")
	}
}

// UnsetEnv removes name from the process environment for the duration of the
// test and restores whatever it held before -- t.Setenv's own discipline, for
// the variables an empty value is not equivalent to an unset one for.
func UnsetEnv(t *testing.T, name string) {
	t.Helper()
	previous, had := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
	t.Cleanup(func() {
		if !had {
			_ = os.Unsetenv(name)
			return
		}
		_ = os.Setenv(name, previous)
	})
}
