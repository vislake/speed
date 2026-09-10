//go:build integration

// Package tencent_test holds go/pkgcore/sms/tencent's integration tier: a
// single env-gated leg that drives the adapter's real signed HTTP dialogue
// with Tencent Cloud SMS against Tencent's genuine gateway. It is physically
// separate from the package's unit tests and carries the "integration" build
// tag: a plain "go test ./..." never compiles or runs anything in this
// directory.
//
// # Why the leg is env-gated (and what it needs to run)
//
// Tencent Cloud SMS has no sandbox environment: every SendSms call charges
// the operator's account and delivers a real text message, so this leg can
// only run in an environment that holds one operator's live credentials and
// a phone that operator is willing to receive a test message on (a single
// send costs roughly CNY 0.045). Without them the leg SKIPS ITSELF with an
// explicit recorded note, the reference-app e2e suite's env-gated self-skip
// shape the alipay sandbox leg already mirrors. The credentials come from
// Tencent's API-key console (SecretId/SecretKey) and SMS console (SDK
// AppID, signature and template). Required variables:
//
//	TENCENT_SMS_SECRET_ID             an API SecretId allowed to call SendSms
//	TENCENT_SMS_SECRET_KEY            the matching SecretKey
//	TENCENT_SMS_SDK_APP_ID            the SMS SDK AppID (1400...)
//	TENCENT_SMS_SIGN_NAME             an approved SMS signature content
//	TENCENT_SMS_TEMPLATE_ID           an approved template's id
//	TENCENT_SMS_TO                    the recipient phone, E.164 "+86..." form
//
// Optional variables (the defaults serve the phone-login verification-code
// message this repository's own flows produce):
//
//	TENCENT_SMS_MESSAGE_ID           the message id the template is mapped
//	                                 for (default
//	                                 "authn.sms.verification_code")
//	TENCENT_SMS_LOCALE               the locale it is mapped for (default
//	                                 "zh-CN")
//	TENCENT_SMS_TEMPLATE_VARS        the comma-separated seam parameter
//	                                 names the template's positional
//	                                 variables take, in order (default
//	                                 "code,minutes"); each is sent with a
//	                                 plausible value -- "code" gets 123456,
//	                                 "minutes" gets 5, anything else gets
//	                                 the variable's own name
//
// TENCENT_SMS_REGION is optional (defaults to ap-guangzhou).
//
// # What the leg proves, and the boundary it leaves
//
// The package's unit tier pins the TC3 canonical request and Authorization
// header against independently precomputed vectors, but nothing offline
// proves Tencent's real gateway ACCEPTS this package's signature and request
// shape: the gateway answers a bad signature, a wrong region or an
// unapproved template with its own error envelope, and the leg fails with
// Tencent's code text on any of them. A successful send is the one proof
// that the whole request shape matches what the real gateway verifies.
package tencent_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/sms/tencent"
)

// tencentSMSEnv names the environment variables this leg reads.
const (
	envSecretID     = "TENCENT_SMS_SECRET_ID"
	envSecretKey    = "TENCENT_SMS_SECRET_KEY"
	envSdkAppID     = "TENCENT_SMS_SDK_APP_ID"
	envSignName     = "TENCENT_SMS_SIGN_NAME"
	envTemplateID   = "TENCENT_SMS_TEMPLATE_ID"
	envMessageID    = "TENCENT_SMS_MESSAGE_ID"
	envLocale       = "TENCENT_SMS_LOCALE"
	envTemplateVars = "TENCENT_SMS_TEMPLATE_VARS"
	envRegion       = "TENCENT_SMS_REGION"
	envTo           = "TENCENT_SMS_TO"
)

// defaultMessageID, defaultLocale and defaultTemplateVars are the leg's
// defaults: the phone-login verification-code message this repository's own
// flows deliver, whose template takes code and minutes as {1} and {2}.
const (
	defaultMessageID    = "authn.sms.verification_code"
	defaultLocale       = "zh-CN"
	defaultTemplateVars = "code,minutes"
)

// configFromEnv assembles the tencent.Config this leg drives, skipping the
// test with an explicit recorded note when the credentials are not present
// -- an operator-secret-gated test must say, in its own skip message, exactly
// which secret is missing and why the test cannot run without it.
func configFromEnv(t *testing.T) tencent.Config {
	t.Helper()

	messageID := os.Getenv(envMessageID)
	if messageID == "" {
		messageID = defaultMessageID
	}
	locale := os.Getenv(envLocale)
	if locale == "" {
		locale = defaultLocale
	}
	vars := os.Getenv(envTemplateVars)
	if vars == "" {
		vars = defaultTemplateVars
	}

	cfg := tencent.Config{
		SecretID:  os.Getenv(envSecretID),
		SecretKey: os.Getenv(envSecretKey),
		SdkAppID:  os.Getenv(envSdkAppID),
		SignName:  os.Getenv(envSignName),
		Templates: map[string]tencent.Template{
			locale + "/" + messageID: {
				ID:     os.Getenv(envTemplateID),
				Params: strings.Split(vars, ","),
			},
		},
		Region: os.Getenv(envRegion),
	}

	var missing []string
	if cfg.SecretID == "" {
		missing = append(missing, envSecretID)
	}
	if cfg.SecretKey == "" {
		missing = append(missing, envSecretKey)
	}
	if cfg.SdkAppID == "" {
		missing = append(missing, envSdkAppID)
	}
	if cfg.SignName == "" {
		missing = append(missing, envSignName)
	}
	if os.Getenv(envTemplateID) == "" {
		missing = append(missing, envTemplateID)
	}
	if os.Getenv(envTo) == "" {
		missing = append(missing, envTo)
	}
	if len(missing) > 0 {
		t.Skipf(
			"self-skip: the Tencent Cloud SMS live leg needs the operator's own live credentials and a recipient phone, which %s is/are not set (Tencent Cloud SMS has no sandbox: each run sends one real text message and charges the account, so the leg runs only when an operator deliberately provides an API SecretId with SendSms permission, the SMS console's SDK AppID, an approved signature and an approved template, plus an E.164 phone in %s). Set them and re-run: go test -tags=integration ./sms/tencent/integration_test/",
			strings.Join(missing, ", "), envTo,
		)
	}
	return cfg
}

// liveMessage returns the SMS this leg sends: the configured mapping's own
// (locale, message-id) key, with a plausible value for each parameter the
// configured template declares -- 123456 for "code", 5 for "minutes", the
// variable's own name for anything else -- so the leg runs against an
// arbitrary approved template.
func liveMessage(cfg tencent.Config, to string) pkgcore.SMS {
	for key, tpl := range cfg.Templates {
		locale, messageID, _ := strings.Cut(key, "/")
		params := make(map[string]string, len(tpl.Params))
		for _, name := range tpl.Params {
			switch name {
			case "code":
				params[name] = "123456"
			case "minutes":
				params[name] = "5"
			default:
				params[name] = name
			}
		}
		return pkgcore.SMS{
			To:        to,
			Text:      "[speed authn] tencent live-leg message; your code is 123456",
			MessageID: messageID,
			Locale:    locale,
			Params:    params,
		}
	}
	return pkgcore.SMS{}
}

// TestSender_LiveTencentGateway_SendsOneMessage drives one Send through the
// real adapter against Tencent's genuine gateway. On the unit tier no request
// ever left the process: the scripted transport asserted the signed request
// and answered a canned envelope, so whether Tencent's own gateway accepts
// this package's TC3 signature and request shape was unproven -- precisely
// the property this leg exists to prove. A signature, encoding or placement
// defect fails loudly with the gateway's own code text, and a successful
// send is the acceptance.
func TestSender_LiveTencentGateway_SendsOneMessage(t *testing.T) {
	cfg := configFromEnv(t)

	sender, err := tencent.NewSender(cfg)
	if err != nil {
		t.Fatalf("NewSender: %v (a config error here means the %s/%s/%s values do not satisfy the documented Config shape)", err, envSecretID, envSdkAppID, envTemplateID)
	}

	err = sender.Send(context.Background(), liveMessage(cfg, os.Getenv(envTo)))
	if err != nil {
		t.Fatalf("Send against the real Tencent Cloud SMS gateway: %v (if this is an AuthFailure.*/FailedOperation.* error, the SecretId in %s must be allowed to call SendSms in the region of %s, %s must be an approved signature, %s an approved template id for the %s/%s message, and %s must list exactly the seam parameters its positional variables take)", err, envSecretID, envRegion, envSignName, envTemplateID, envLocale, envMessageID, envTemplateVars)
	}
}
