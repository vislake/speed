package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/vislake/speed/go/saasctl/internal/appconfig"
	"github.com/vislake/speed/go/saasctl/internal/project"
)

// defaultModPath is the go.mod the print command reads when the command
// line names none: the working directory's, matching the sibling
// commands' default.
const defaultModPath = "go.mod"

// redactedMarker stands in for a secret variable's bytes in the rendered
// value column. A secret is never printed: whatever the environment holds,
// the output carries only this marker -- never the value itself, and never
// a hint of its shape. Covers the two key variables (APP_CONFIG_KEY,
// APP_ORG_INDEX_KEY) and the two credential variables of the infrastructure
// groups (APP_S3_SECRET_KEY, APP_SMTP_PASSWORD).
const redactedMarker = "[redacted]"

// The unset-provenance text for each row that is not a scalar default: the
// infrastructure seam a variable composes when unset, matching
// cmd/server/config.go's own doc comments on what an empty value means.
const (
	unsetRedis        = "unset or empty (eventbus/kv stay on the in-process default)"
	unsetS3Group      = "unset or empty (objectstore stays on the local-directory default)"
	unsetS3Optional   = "unset or empty (optional S3 refinement; used only when the group above is set)"
	unsetSMTPGroup    = "unset or empty (mailer stays on the console default)"
	unsetSMTPOptional = "unset or empty (optional SMTP refinement; used only when the group above is set)"
	unsetSMS          = "unset or empty (SMS sender seam left unwired: console default under standalone, refused under distributed)"
)

// printUsage is the print subcommand's help text.
const printUsage = `Usage: saasctl config print [go.mod]

Show how a project's bootstrap configuration resolves: read the go.mod
(the [go.mod] argument, defaulting to ./go.mod) for the project's app
name, resolve the project's bootstrap environment the way the generated
app's configFromEnv resolves it -- refusing exactly when the generated
app's own bootstrap would refuse to boot -- and render the resolved
values with each one's provenance: which variable carried a value, and
which fell back to its default.

The bootstrap variables:

  APP_DEPLOYMENT_MODE     the deployment mode (standalone or distributed)
  PORT                    the HTTP port
  APP_DB_PATH             the SQLite database path
  APP_CONFIG_KEY          the configuration master key (64 hex characters)
  APP_ORG_INDEX_KEY       the org blind-index key (64 hex characters)
  APP_REDIS_ADDR          Redis address composing the eventbus/kv seams
  APP_S3_ENDPOINT         S3-compatible endpoint (with bucket/access/secret
                          key below, required together)
  APP_S3_BUCKET           S3 bucket name
  APP_S3_ACCESS_KEY       S3 access key
  APP_S3_SECRET_KEY       S3 secret key
  APP_S3_REGION           S3 region (optional)
  APP_S3_USE_SSL          whether the S3 endpoint speaks TLS (optional bool)
  APP_SMTP_HOST           SMTP host (with port below, required together)
  APP_SMTP_PORT           SMTP port
  APP_SMTP_USERNAME       SMTP AUTH username (optional)
  APP_SMTP_PASSWORD       SMTP AUTH password (optional)
  APP_SMS_GATEWAY_URL     authn's HTTP SMS transport endpoint

The two key variables and the S3 secret key / SMTP password are secrets:
their values never print, only a [redacted] marker in their place,
whatever the environment holds. An S3 group or SMTP pair that is only
partially set is refused, exactly as the generated app's own bootstrap
refuses it.

Examples:

  saasctl config print
  saasctl config print /path/to/project/go.mod

Exit codes: 0 success or help, 2 usage error, 1 execution error.
`

// runPrint parses the print invocation and reports the rendered rows on
// stdout, following the migrate subcommand's conventions: a flag parse
// error is a usage error (exit 2), help is success (exit 0), an execution
// failure is reported on stderr (exit 1).
func runPrint(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("print", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		_, _ = fmt.Fprint(stderr, printUsage)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	modPath := defaultModPath
	if paths := flags.Args(); len(paths) > 1 {
		return usageError(stderr, fmt.Errorf("expected at most one go.mod path, got %d", len(paths)))
	} else if len(paths) == 1 {
		modPath = paths[0]
	}
	rendered, err := print(modPath)
	if err != nil {
		return reportError(stderr, err)
	}
	_, _ = fmt.Fprint(stdout, rendered)
	return 0
}

// usageError reports a malformed invocation: the message plus the print
// usage on stderr, exit code 2.
func usageError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "saasctl config print: %v\n\n%s", err, printUsage)
	return 2
}

// reportError reports a failed execution: one line on stderr, exit code
// 1.
func reportError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "saasctl config print: %v\n", err)
	return 1
}

// print resolves the bootstrap configuration of the project at modPath --
// the go.mod supplies the app name, the environment supplies the full
// bootstrap surface, appconfig.Load resolves each variable to the value
// the generated app would boot on, refusing exactly when the generated
// app's own bootstrap would refuse (an unparsable deployment mode, a
// malformed key, or an incomplete S3/SMTP infrastructure group) -- and
// renders one line per value: the label, the resolved value, and its
// provenance. The two key variables and the S3 secret key / SMTP password
// render as [redacted] in the value column whatever the environment
// holds.
func print(modPath string) (string, error) {
	proj, err := project.Read(modPath)
	if err != nil {
		return "", err
	}
	cfg, err := appconfig.Load(proj.AppName, os.LookupEnv)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	write := func(label, value, source string) {
		fmt.Fprintf(&b, "%-16s %-12s %s\n", label, value, source)
	}
	write("deployment mode", string(cfg.DeploymentMode),
		provenance(appconfig.DeploymentModeEnv, cfg.DeploymentModeFromEnv,
			fmt.Sprintf("unset or empty (default %s)", cfg.DeploymentMode)))
	write("port", cfg.Port,
		provenance(appconfig.PortEnv, cfg.PortFromEnv, fmt.Sprintf("unset or empty (default %s)", cfg.Port)))
	write("sqlite path", cfg.SQLitePath,
		provenance(appconfig.DBPathEnv, cfg.SQLitePathFromEnv, fmt.Sprintf("unset or empty (default %s)", cfg.SQLitePath)))
	write("config key", redactedMarker,
		provenance(appconfig.ConfigKeyEnv, cfg.ConfigKeyFromEnv, "unset or empty (development default)"))
	write("org index key", redactedMarker,
		provenance(appconfig.OrgIndexKeyEnv, cfg.OrgIndexKeyFromEnv, "unset or empty (development default)"))

	write("redis addr", cfg.RedisAddr, provenance(appconfig.RedisAddrEnv, cfg.RedisAddrFromEnv, unsetRedis))
	write("s3 endpoint", cfg.S3Endpoint, provenance(appconfig.S3EndpointEnv, cfg.S3EndpointFromEnv, unsetS3Group))
	write("s3 bucket", cfg.S3Bucket, provenance(appconfig.S3BucketEnv, cfg.S3BucketFromEnv, unsetS3Group))
	write("s3 access key", cfg.S3AccessKey, provenance(appconfig.S3AccessKeyEnv, cfg.S3AccessKeyFromEnv, unsetS3Group))
	write("s3 secret key", redactedMarker, provenance(appconfig.S3SecretKeyEnv, cfg.S3SecretKeyFromEnv, unsetS3Group))
	write("s3 region", cfg.S3Region, provenance(appconfig.S3RegionEnv, cfg.S3RegionFromEnv, unsetS3Optional))
	write("s3 use ssl", strconv.FormatBool(cfg.S3UseSSL),
		provenance(appconfig.S3UseSSLEnv, cfg.S3UseSSLFromEnv, "unset or empty (default false)"))
	write("smtp host", cfg.SMTPHost, provenance(appconfig.SMTPHostEnv, cfg.SMTPHostFromEnv, unsetSMTPGroup))
	write("smtp port", smtpPortValue(cfg), provenance(appconfig.SMTPPortEnv, cfg.SMTPPortFromEnv, unsetSMTPGroup))
	write("smtp username", cfg.SMTPUsername, provenance(appconfig.SMTPUsernameEnv, cfg.SMTPUsernameFromEnv, unsetSMTPOptional))
	write("smtp password", redactedMarker, provenance(appconfig.SMTPPasswordEnv, cfg.SMTPPasswordFromEnv, unsetSMTPOptional))
	write("sms gateway url", cfg.SMSGatewayURL, provenance(appconfig.SMSGatewayURLEnv, cfg.SMSGatewayURLFromEnv, unsetSMS))
	return b.String(), nil
}

// smtpPortValue renders the SMTP port column: blank when unset (0 would
// read as a real port number and mislead, since the generated app never
// treats port 0 as meaningful here), the resolved port otherwise.
func smtpPortValue(cfg appconfig.Config) string {
	if !cfg.SMTPPortFromEnv {
		return ""
	}
	return strconv.Itoa(cfg.SMTPPort)
}

// provenance describes where one resolved value came from: the variable
// that carried it (fromEnv), or the exact text describing what it fell
// back to when unset -- a scalar default for the five original bootstrap
// variables, or which seam stays on its Preset default for an
// infrastructure variable. The unset text is never empty and never
// renders a secret's bytes.
func provenance(envName string, fromEnv bool, unsetText string) string {
	if fromEnv {
		return fmt.Sprintf("from %s", envName)
	}
	return unsetText
}
