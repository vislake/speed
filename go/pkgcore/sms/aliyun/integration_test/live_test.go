//go:build integration

// Package aliyun_test holds go/pkgcore/sms/aliyun's integration tier: a single
// env-gated leg that drives the adapter's real signed HTTP dialogue with
// Aliyun SMS against Aliyun's genuine gateway. It is physically separate
// from the package's unit tests and carries the "integration" build tag: a
// plain "go test ./..." never compiles or runs anything in this directory.
//
// # Why the leg is env-gated (and what it needs to run)
//
// Aliyun SMS has no sandbox environment: every SendSms call charges the
// operator's account and delivers a real text message, so this leg can only
// run in an environment that holds one operator's live credentials and a
// phone that operator is willing to receive a test message on (a single send
// costs roughly CNY 0.045). Without them the leg SKIPS ITSELF with an
// explicit recorded note, the reference-app e2e suite's env-gated self-skip
// shape the alipay sandbox leg already mirrors. The credentials come from
// Aliyun's RAM console (the AccessKey) and SMS console (signature and
// template). Required variables:
//
//	ALIYUN_SMS_ACCESS_KEY_ID        an AccessKey whose owner may call SendSms
//	ALIYUN_SMS_ACCESS_KEY_SECRET    the matching AccessKey secret
//	ALIYUN_SMS_SIGN_NAME            an approved SMS signature name
//	ALIYUN_SMS_TEMPLATE_CODE        an approved template whose single
//	                                variable is named by
//	                                ALIYUN_SMS_TEMPLATE_PARAM_NAME (default
//	                                "content" -- see Config.TemplateParamName)
//	ALIYUN_SMS_TO                   the recipient phone, in a form Aliyun
//	                                accepts (E.164 "+86..." works domestically)
//
// # What the leg proves, and the boundary it leaves
//
// The package's unit tier pins the RPC signature against Aliyun's own
// documentation vector and an independently precomputed SendSms-shaped one,
// but nothing offline proves Aliyun's real gateway ACCEPTS this package's
// signature and parameter placement: the gateway answers a bad signature,
// an unapproved template or an unregistered number with a signed business
// error envelope, and the leg fails with Aliyun's own code text on any of
// them. A successful send is the one proof that the whole request shape --
// common parameters, form placement, encoding -- matches what the real
// gateway verifies.
package aliyun_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/sms/aliyun"
)

// aliyunSMSEnv names the environment variables this leg reads.
const (
	envAccessKeyID     = "ALIYUN_SMS_ACCESS_KEY_ID"
	envAccessKeySecret = "ALIYUN_SMS_ACCESS_KEY_SECRET"
	envSignName        = "ALIYUN_SMS_SIGN_NAME"
	envTemplateCode    = "ALIYUN_SMS_TEMPLATE_CODE"
	envTemplateParam   = "ALIYUN_SMS_TEMPLATE_PARAM_NAME"
	envTo              = "ALIYUN_SMS_TO"
)

// configFromEnv assembles the aliyun.Config this leg drives, skipping the
// test with an explicit recorded note when the credentials are not present
// -- an operator-secret-gated test must say, in its own skip message, exactly
// which secret is missing and why the test cannot run without it.
func configFromEnv(t *testing.T) aliyun.Config {
	t.Helper()

	cfg := aliyun.Config{
		AccessKeyID:       os.Getenv(envAccessKeyID),
		AccessKeySecret:   os.Getenv(envAccessKeySecret),
		SignName:          os.Getenv(envSignName),
		TemplateCode:      os.Getenv(envTemplateCode),
		TemplateParamName: os.Getenv(envTemplateParam),
	}
	if cfg.TemplateParamName == "" {
		cfg.TemplateParamName = "content"
	}

	var missing []string
	if cfg.AccessKeyID == "" {
		missing = append(missing, envAccessKeyID)
	}
	if cfg.AccessKeySecret == "" {
		missing = append(missing, envAccessKeySecret)
	}
	if cfg.SignName == "" {
		missing = append(missing, envSignName)
	}
	if cfg.TemplateCode == "" {
		missing = append(missing, envTemplateCode)
	}
	if os.Getenv(envTo) == "" {
		missing = append(missing, envTo)
	}
	if len(missing) > 0 {
		t.Skipf(
			"self-skip: the Aliyun SMS live leg needs the operator's own live credentials and a recipient phone, which %s is/are not set (Aliyun SMS has no sandbox: each run sends one real text message and charges the account, so the leg runs only when an operator deliberately provides an AccessKey with SendSms permission, an approved signature and a single-variable template whose variable is named by %s, plus a phone in %s). Set them and re-run: go test -tags=integration ./sms/aliyun/integration_test/",
			strings.Join(missing, ", "), envTemplateParam, envTo,
		)
	}
	return cfg
}

// TestSender_LiveAliyunGateway_SendsOneMessage drives one Send through the
// real adapter against Aliyun's genuine gateway. On the unit tier no request
// ever left the process: the scripted transport asserted the signed form and
// answered a canned envelope, so whether Aliyun's own gateway accepts this
// package's RPC signature and parameter placement was unproven -- precisely
// the property this leg exists to prove. A signature, encoding or placement
// defect fails loudly with the gateway's own error text, and a successful
// send is the acceptance.
func TestSender_LiveAliyunGateway_SendsOneMessage(t *testing.T) {
	cfg := configFromEnv(t)

	sender, err := aliyun.NewSender(cfg)
	if err != nil {
		t.Fatalf("NewSender: %v (a config error here means the %s/%s values do not satisfy the documented Config shape)", err, envAccessKeyID, envAccessKeySecret)
	}

	err = sender.Send(context.Background(), pkgcore.SMS{
		To:   os.Getenv(envTo),
		Text: "[speed authn] aliyun live-leg message; your code is 123456",
	})
	if err != nil {
		t.Fatalf("Send against the real Aliyun SMS gateway: %v (if this is an isv.*/SignatureDoesNotMatch business error, the AccessKey in %s must have AliyunDNSFullAccess or the SMS permission, %s must be an approved signature, and %s an approved template whose single variable is named by %s)", err, envAccessKeyID, envSignName, envTemplateCode, envTemplateParam)
	}
}
