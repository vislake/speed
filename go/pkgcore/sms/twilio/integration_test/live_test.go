//go:build integration

// Package twilio_test holds go/pkgcore/sms/twilio's integration tier: a single
// env-gated leg that drives the adapter's real HTTP dialogue with Twilio's
// REST API against Twilio's genuine gateway. It is physically separate from
// the package's unit tests and carries the "integration" build tag: a plain
// "go test ./..." never compiles or runs anything in this directory.
//
// # Why the leg is env-gated (and what it needs to run)
//
// Every Messages-resource call charges the operator's account and delivers a
// real text message (a single send costs roughly USD 0.008 on the standard
// rate card), so this leg can only run in an environment that holds one
// operator's live credentials and a phone that operator is willing to
// receive a test message on -- a trial account additionally only delivers to
// numbers verified in its console. Without them the leg SKIPS ITSELF with an
// explicit recorded note, the reference-app e2e suite's env-gated self-skip
// shape the alipay sandbox leg already mirrors. Required variables:
//
//	TWILIO_SMS_ACCOUNT_SID           the account's SID (AC...)
//	TWILIO_SMS_AUTH_TOKEN            the account's API auth token
//	TWILIO_SMS_FROM                  a number/short code/alphanumeric sender
//	                                 the account may send from -- OR
//	TWILIO_SMS_MESSAGING_SERVICE_SID a Messaging Service (MG...) to send
//	                                 through (exactly one of the two)
//	TWILIO_SMS_TO                    the recipient phone, E.164 "+..." form
//
// # What the leg proves, and the boundary it leaves
//
// The package's unit tier pins the whole wire shape offline -- Twilio's
// Messages request carries no signature, so Basic auth, the resource URL and
// the form body are fully deterministic -- but nothing offline proves a real
// Twilio account ACCEPTS the request: the API answers an invalid credential,
// an unowned From or a trial-account restriction with its own error
// envelope, and the leg fails with Twilio's code text on any of them. A
// successful send is the one proof that the request shape and credential
// plumbing match what the real API verifies.
package twilio_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/sms/twilio"
)

// twilioSMSEnv names the environment variables this leg reads.
const (
	envAccountSID = "TWILIO_SMS_ACCOUNT_SID"
	envAuthToken  = "TWILIO_SMS_AUTH_TOKEN"
	envFrom       = "TWILIO_SMS_FROM"
	envMsgSvcSID  = "TWILIO_SMS_MESSAGING_SERVICE_SID"
	envTo         = "TWILIO_SMS_TO"
)

// configFromEnv assembles the twilio.Config this leg drives, skipping the
// test with an explicit recorded note when the credentials are not present
// -- an operator-secret-gated test must say, in its own skip message, exactly
// which secret is missing and why the test cannot run without it.
func configFromEnv(t *testing.T) twilio.Config {
	t.Helper()

	cfg := twilio.Config{
		AccountSID:          os.Getenv(envAccountSID),
		AuthToken:           os.Getenv(envAuthToken),
		From:                os.Getenv(envFrom),
		MessagingServiceSID: os.Getenv(envMsgSvcSID),
	}

	var missing []string
	if cfg.AccountSID == "" {
		missing = append(missing, envAccountSID)
	}
	if cfg.AuthToken == "" {
		missing = append(missing, envAuthToken)
	}
	if cfg.From == "" && cfg.MessagingServiceSID == "" {
		missing = append(missing, envFrom+" or "+envMsgSvcSID)
	}
	if os.Getenv(envTo) == "" {
		missing = append(missing, envTo)
	}
	if len(missing) > 0 {
		t.Skipf(
			"self-skip: the Twilio SMS live leg needs the operator's own live credentials and a recipient phone, which %s is/are not set (Twilio SMS has no sandbox for this resource: each run sends one real text message and charges the account -- a trial account additionally only delivers to console-verified numbers -- so the leg runs only when an operator deliberately provides an AccountSID/AuthToken pair, exactly one sender of %s/%s, and an E.164 phone in %s). Set them and re-run: go test -tags=integration ./sms/twilio/integration_test/",
			strings.Join(missing, ", "), envFrom, envMsgSvcSID, envTo,
		)
	}
	return cfg
}

// TestSender_LiveTwilioGateway_SendsOneMessage drives one Send through the
// real adapter against Twilio's genuine REST API. On the unit tier no request
// ever left the process: the scripted transport asserted the wire shape and
// answered a canned envelope, so whether a real Twilio account accepts this
// adapter's request -- credentials plumbing included -- was unproven,
// precisely the property this leg exists to prove. A credential, URL or body
// defect fails loudly with Twilio's own code text, and a successful send is
// the acceptance.
func TestSender_LiveTwilioGateway_SendsOneMessage(t *testing.T) {
	cfg := configFromEnv(t)

	sender, err := twilio.NewSender(cfg)
	if err != nil {
		t.Fatalf("NewSender: %v (a config error here means the %s/%s/%s/%s values do not satisfy the documented Config shape)", err, envAccountSID, envFrom, envMsgSvcSID, envTo)
	}

	err = sender.Send(context.Background(), pkgcore.SMS{
		To:   os.Getenv(envTo),
		Text: "[speed authn] twilio live-leg message; your code is 123456",
	})
	if err != nil {
		t.Fatalf("Send against the real Twilio REST API: %v (if this is a 20003/21211/21614-class error, the AuthToken in %s must match %s, the sender in %s/%s must belong to that account, and %s must be an E.164 number a trial account has verified)", err, envAuthToken, envAccountSID, envFrom, envMsgSvcSID, envTo)
	}
}
