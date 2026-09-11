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
// a hint of its shape. This constant is only the marker's rendering; the
// DECISION of which variables are secrets is the single redactedEnv list
// below, which the row renderer (valueColumn) consults for every line.
const redactedMarker = "[redacted]"

// redactedEnv is the single, concentrated declaration of which bootstrap
// variables' resolved values never print: the six key materials and the
// three infrastructure credentials -- this system's durable secrets, the
// bytes that unlock ciphertext, HMAC indexes and remote credentials. The
// members are the APP_* variables this map's keys name; the print
// usage text enumerates the same members by name in its secret-variable
// paragraph, and print_test.go's paragraph test keeps that enumeration
// equal to this key set, so the operator-facing copy cannot drift from
// the decision the row renderer enforces. The gateway URL is a URL only
// by shape: authn's HTTP SMS transport has no credential channel separate
// from its endpoint, so an operator who must authenticate to the gateway
// can only put the credentials IN the URL -- the URL is where the
// credential lives, and it redacts on the same ground as every other
// member of the list. Everything else on the bootstrap surface is
// connection topology -- hosts, ports, paths, bucket names, URLs -- or a
// public identifier (an S3 access key ID, an SMTP username), which this
// command can carry: config print renders the operator's own environment
// back to their own terminal and no CI or other pipeline runs it, so the
// output has no wider audience, while the members of this list are the
// system's durable secrets and an accidental paste of the output into a
// log or ticket must not ship them. The decision lives here, once: the
// row renderer consults this list for every line it prints, so a
// bootstrap variable not declared here prints its plaintext by default --
// this list is the checklist the declaration act is performed against,
// and the whole-output tests force every added row past it either way.
var redactedEnv = map[string]bool{
	appconfig.ConfigKeyEnv:            true,
	appconfig.OrgIndexKeyEnv:          true,
	appconfig.AuthnBlindIndexKeyEnv:   true,
	appconfig.AuthnPIICipherKeyEnv:    true,
	appconfig.PKILocalKeyCipherKeyEnv: true,
	appconfig.NotificationIndexKeyEnv: true,
	appconfig.S3SecretKeyEnv:          true,
	appconfig.SMTPPasswordEnv:         true,
	appconfig.SMSGatewayURLEnv:        true,
}

// The unset-provenance text for each row that is not a scalar default: the
// infrastructure seam a variable composes when unset, matching
// cmd/server/config.go's own doc comments on what an empty value means.
const (
	unsetRedis        = "unset or empty (eventbus/kv stay on the in-process default)"
	unsetOTLP         = "unset or empty (observability stays on the local exporters)"
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
  APP_CONFIG__CIPHER_KEY  the config-module cipher key (64 hex characters)
  APP_ORG__INVITATION_EMAIL_INDEX_KEY the org blind-index key (64 hex characters)
  APP_AUTHN__BLIND_INDEX_KEY the authn blind-index HMAC key (64 hex characters)
  APP_AUTHN__PII_CIPHER_KEY  the authn PII cipher key (64 hex characters)
  APP_PKI__LOCAL_KEY_CIPHER_KEY the pki local-key cipher key (64 hex characters)
  APP_NOTIFICATION__CONTACT_INDEX_KEY the notification contact-index key (64 hex characters)
  APP_REDIS_ADDR          Redis address composing the eventbus/kv seams
  APP_OTLP_ENDPOINT       OTLP/gRPC endpoint traces and metrics are pushed to
                          (unset: the local exporters)
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

The six key variables (APP_CONFIG__CIPHER_KEY,
APP_ORG__INVITATION_EMAIL_INDEX_KEY, APP_AUTHN__BLIND_INDEX_KEY,
APP_AUTHN__PII_CIPHER_KEY, APP_PKI__LOCAL_KEY_CIPHER_KEY and
APP_NOTIFICATION__CONTACT_INDEX_KEY) and the three infrastructure credentials
(APP_S3_SECRET_KEY, APP_SMTP_PASSWORD and APP_SMS_GATEWAY_URL) are
secrets: their values never print, only a [redacted] marker in their
place, whatever the environment holds -- the redaction decision is
redactedEnv's single declaration in print.go, and every rendered row
consults it. The gateway URL is one of them by shape, not by name:
authn's HTTP SMS transport has no credential channel separate from the
URL, so the URL is where an operator's gateway credentials live. An S3
group or SMTP pair that is only partially set is refused, exactly as
the generated app's own bootstrap refuses it.

The sqlite path row is the one row that resolves one step further than
the bootstrap itself: every value this command reports is the value the
generated app would actually use, and a relative database path -- the
app.db default when APP_DB_PATH is unset, or a relative APP_DB_PATH --
is used by the app relative to the directory it runs from, which is the
go.mod argument's directory. The row's value column therefore shows the
effective file (anchored to that directory, exactly the file db migrate
opens and the file a boot from the project's own directory opens), and
when the raw value differs from it -- a relative APP_DB_PATH -- the raw
value is shown in the source column too, so "the value" and "the
effective value" are both visible.

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
// provenance. The value column of every row renders through valueColumn,
// which consults the single redactedEnv declaration: the six key
// variables, the S3 secret key, the SMTP password and the SMS gateway
// URL render as [redacted] whatever the environment holds, every other
// variable prints its resolved value.
//
// The sqlite path row is the one row whose value column shows the
// EFFECTIVE file rather than the raw resolved value: a relative database
// path -- the app.db default or a relative APP_DB_PATH -- resolves
// against the directory the app runs from, the go.mod argument's
// directory, and the file that actually opens is what the row reports,
// through cfg.EffectiveDBPath, the single shared resolution db migrate
// opens its database with. The raw value and its provenance stay visible
// in the source column when the two differ (sqlitePathSource), so the
// operator sees both the input and the effective file.
func print(modPath string) (string, error) {
	proj, err := project.Read(modPath)
	if err != nil {
		return "", err
	}
	cfg, err := appconfig.Load(proj.AppName, os.LookupEnv)
	if err != nil {
		return "", err
	}
	effectiveDBPath := cfg.EffectiveDBPath(modPath)

	// One row per bootstrap variable, in the order the generated app's own
	// config.go resolves them (deployment mode, port, database path, the
	// six key materials, then the infrastructure addresses and the OTLP
	// endpoint). Every row names
	// its environment variable, so the renderer below can decide its value
	// column against redactedEnv -- the per-line choices are this table.
	rows := []struct {
		label  string
		env    string
		value  string
		source string
	}{
		{
			"deployment mode", appconfig.DeploymentModeEnv, string(cfg.DeploymentMode),
			provenance(appconfig.DeploymentModeEnv, cfg.DeploymentModeFromEnv,
				fmt.Sprintf("unset or empty (default %s)", cfg.DeploymentMode)),
		},
		{
			"port", appconfig.PortEnv, cfg.Port,
			provenance(appconfig.PortEnv, cfg.PortFromEnv, fmt.Sprintf("unset or empty (default %s)", cfg.Port)),
		},
		{
			// The sqlite path row's value is the effective FILE a boot
			// opens (cfg.EffectiveDBPath -- relative values anchored to the
			// go.mod argument's directory, the app's documented run
			// directory), not the raw resolved value, because this
			// command's whole purpose is answering which file the app
			// would actually use; the raw value stays visible in the
			// source column when the two differ (sqlitePathSource).
			"sqlite path", appconfig.DBPathEnv, effectiveDBPath,
			sqlitePathSource(cfg, effectiveDBPath),
		},
		// The six key rows carry no value at all, not merely a redacted
		// rendering of one: their raw bytes never enter this table, so the
		// redaction cannot leak them through any future edit to valueColumn
		// or the list -- the marker they render comes from redactedEnv
		// declaring them secrets, the decision this file concentrates.
		{
			"config key", appconfig.ConfigKeyEnv, "",
			provenance(appconfig.ConfigKeyEnv, cfg.ConfigKeyFromEnv, "unset or empty (development default)"),
		},
		{
			"org index key", appconfig.OrgIndexKeyEnv, "",
			provenance(appconfig.OrgIndexKeyEnv, cfg.OrgIndexKeyFromEnv, "unset or empty (development default)"),
		},
		{
			"authn blind index key", appconfig.AuthnBlindIndexKeyEnv, "",
			provenance(appconfig.AuthnBlindIndexKeyEnv, cfg.AuthnBlindIndexKeyFromEnv, "unset or empty (development default)"),
		},
		{
			"authn pii cipher key", appconfig.AuthnPIICipherKeyEnv, "",
			provenance(appconfig.AuthnPIICipherKeyEnv, cfg.AuthnPIICipherKeyFromEnv, "unset or empty (development default)"),
		},
		{
			"pki local key cipher key", appconfig.PKILocalKeyCipherKeyEnv, "",
			provenance(appconfig.PKILocalKeyCipherKeyEnv, cfg.PKILocalKeyCipherKeyFromEnv, "unset or empty (development default)"),
		},
		{
			"notification index key", appconfig.NotificationIndexKeyEnv, "",
			provenance(appconfig.NotificationIndexKeyEnv, cfg.NotificationIndexKeyFromEnv, "unset or empty (development default)"),
		},

		{
			"redis addr", appconfig.RedisAddrEnv, cfg.RedisAddr,
			provenance(appconfig.RedisAddrEnv, cfg.RedisAddrFromEnv, unsetRedis),
		},
		{
			"otlp endpoint", appconfig.OTLPEndpointEnv, cfg.OTLPEndpoint,
			provenance(appconfig.OTLPEndpointEnv, cfg.OTLPEndpointFromEnv, unsetOTLP),
		},
		{
			"s3 endpoint", appconfig.S3EndpointEnv, cfg.S3Endpoint,
			provenance(appconfig.S3EndpointEnv, cfg.S3EndpointFromEnv, unsetS3Group),
		},
		{
			"s3 bucket", appconfig.S3BucketEnv, cfg.S3Bucket,
			provenance(appconfig.S3BucketEnv, cfg.S3BucketFromEnv, unsetS3Group),
		},
		{
			"s3 access key", appconfig.S3AccessKeyEnv, cfg.S3AccessKey,
			provenance(appconfig.S3AccessKeyEnv, cfg.S3AccessKeyFromEnv, unsetS3Group),
		},
		{
			"s3 secret key", appconfig.S3SecretKeyEnv, cfg.S3SecretKey,
			provenance(appconfig.S3SecretKeyEnv, cfg.S3SecretKeyFromEnv, unsetS3Group),
		},
		{
			"s3 region", appconfig.S3RegionEnv, cfg.S3Region,
			provenance(appconfig.S3RegionEnv, cfg.S3RegionFromEnv, unsetS3Optional),
		},
		{
			"s3 use ssl", appconfig.S3UseSSLEnv, strconv.FormatBool(cfg.S3UseSSL),
			provenance(appconfig.S3UseSSLEnv, cfg.S3UseSSLFromEnv, "unset or empty (default false)"),
		},
		{
			"s3 bucket lookup", appconfig.S3BucketLookupEnv, cfg.S3BucketLookup,
			provenance(appconfig.S3BucketLookupEnv, cfg.S3BucketLookupFromEnv, "unset or empty (default auto)"),
		},
		{
			"smtp host", appconfig.SMTPHostEnv, cfg.SMTPHost,
			provenance(appconfig.SMTPHostEnv, cfg.SMTPHostFromEnv, unsetSMTPGroup),
		},
		{
			"smtp port", appconfig.SMTPPortEnv, smtpPortValue(cfg),
			provenance(appconfig.SMTPPortEnv, cfg.SMTPPortFromEnv, unsetSMTPGroup),
		},
		{
			"smtp username", appconfig.SMTPUsernameEnv, cfg.SMTPUsername,
			provenance(appconfig.SMTPUsernameEnv, cfg.SMTPUsernameFromEnv, unsetSMTPOptional),
		},
		{
			"smtp password", appconfig.SMTPPasswordEnv, cfg.SMTPPassword,
			provenance(appconfig.SMTPPasswordEnv, cfg.SMTPPasswordFromEnv, unsetSMTPOptional),
		},
		{
			"sms gateway url", appconfig.SMSGatewayURLEnv, cfg.SMSGatewayURL,
			provenance(appconfig.SMSGatewayURLEnv, cfg.SMSGatewayURLFromEnv, unsetSMS),
		},
	}

	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "%-16s %-12s %s\n", r.label, valueColumn(r.env, r.value), r.source)
	}
	return b.String(), nil
}

// valueColumn renders one row's value column: redactedMarker when
// redactedEnv declares the row's variable a secret, the resolved value
// otherwise. Routing every row through this one renderer is what makes
// the redaction decision the single redactedEnv list -- a future
// sensitive variable whose row forgets to declare it there prints its
// plaintext, which is exactly the state the list exists to make visible
// (see redactedEnv's doc comment). A redacted row's resolved value never
// reaches this function's output either way.
func valueColumn(env, value string) string {
	if redactedEnv[env] {
		return redactedMarker
	}
	return value
}

// sqlitePathSource renders the sqlite path row's source column: where
// the RAW value came from, and the raw value itself when it differs from
// the effective file the value column shows. A relative raw value -- the
// unset-APP_DB_PATH default (the fixed app.db literal, carried inside the
// unset text below) or a relative APP_DB_PATH -- names a file in the
// app's own directory while the effective column shows that file anchored
// to the go.mod argument's directory, and "the value" and "the effective
// value" are two different things an operator can need both of; an
// absolute APP_DB_PATH is used exactly as the app would use it, so raw
// and effective coincide and no raw parenthetical is needed.
func sqlitePathSource(cfg appconfig.Config, effective string) string {
	if !cfg.SQLitePathFromEnv {
		return fmt.Sprintf("unset or empty (default %s)", cfg.SQLitePath)
	}
	source := fmt.Sprintf("from %s", appconfig.DBPathEnv)
	if cfg.SQLitePath != effective {
		source = fmt.Sprintf("%s (raw value: %s)", source, cfg.SQLitePath)
	}
	return source
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
