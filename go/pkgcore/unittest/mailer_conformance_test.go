// This file lives in package unittest — this module's dedicated unit-test
// directory for unit-tier suites with no single source file as their target
// (the backend coding standard's testing-layout rule). A seam-contract
// driver of the module root's own built-ins is such a suite, and it must be
// black-box against package pkgcore: the driver imports go/pkgcore's
// mailertest support package, which itself imports go/pkgcore, and an
// internal test file (package pkgcore) importing a package that imports
// pkgcore back is an import cycle Go's toolchain refuses ("import cycle not
// allowed in test"). An external test package carries no such restriction —
// see eventbus_conformance_test.go's identical note.
package unittest

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/internal/testutil"
	"github.com/vislake/speed/go/pkgcore/mailertest"
)

// TestSMTPMailer_ConformsToMailerContract proves pkgcore.NewSMTPMailer still
// satisfies the shared contract mailertest.AssertConforms checks, after the
// deployment-composition retrofit (Phase 1) generalized how the assembly
// resolves and validates its Mailer seam — this is what proves the retrofit
// did not silently change NewSMTPMailer's own behavior for its existing
// callers. It needs no Docker: the fake relay is an in-process
// net.Listener, the same scripted double mailer_smtp_test.go's own
// wire-level tests exercise (see internal/testutil/fake_smtp_server.go),
// so it runs in the plain unit tier rather than integration_test/.
//
// AssertConforms calls the factory once per subtest, so each subtest gets
// its own fake relay accepting exactly the messages that subtest sends —
// nothing here depends on a relay surviving past its own subtest.
func TestSMTPMailer_ConformsToMailerContract(t *testing.T) {
	t.Parallel()
	mailertest.AssertConforms(t, func() pkgcore.Mailer {
		server := testutil.StartFakeSMTPServer(t, testutil.FakeSMTPOptions{})
		return smtpMailerFor(t, server)
	})
}

// TestSMTPMailer_PermanentRefusalCarriesTheSentinel drives the contract
// suite's sentinel clause (mailertest.AssertPermanentFailure) through the
// SMTP leg's scripted relay: the relay refuses the recipient with its 550,
// and the mailer's returned error must wrap
// pkgcore.ErrTransportPermanent -- the signal a delivery caller splits
// terminal refusals from retryable failures by. The clause is asserted here
// rather than inside AssertConforms because provoking a real refusal takes a
// backend scripted to refuse, which the shared suite's generic factory
// cannot be.
func TestSMTPMailer_PermanentRefusalCarriesTheSentinel(t *testing.T) {
	t.Parallel()

	server := testutil.StartFakeSMTPServer(t, testutil.FakeSMTPOptions{
		Reject: func(string) bool { return true },
	})
	mailer := smtpMailerFor(t, server)

	err := mailer.Send(context.Background(), pkgcore.Mail{
		From: "ops@example.com", To: []string{"ada@example.com"},
		Subject: "mailertest permanent refusal", Text: "body",
	})
	if err == nil {
		t.Fatal("Send() error = nil, want the relay's 550 refusal")
	}
	mailertest.AssertPermanentFailure(t, err)
}

// smtpMailerFor builds a pkgcore.Mailer pointed at server. It duplicates the
// handful of lines mailer_smtp_test.go's own unexported mailerFor already
// has, rather than sharing one implementation, because the two live in
// different packages for the import-cycle reason this file's own doc
// comment explains (mailerFor's would-be shared home, internal/testutil,
// cannot import pkgcore at all — see fake_smtp_server.go's doc comment) —
// each side of the package boundary needs its own thin pkgcore.Mailer
// wrapper around FakeSMTPServer.Addr().
func smtpMailerFor(t *testing.T, server *testutil.FakeSMTPServer) pkgcore.Mailer {
	t.Helper()
	host, port, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatalf("split relay address %q: %v", server.Addr(), err)
	}
	var portNumber int
	if _, err := fmt.Sscanf(port, "%d", &portNumber); err != nil {
		t.Fatalf("parse relay port %q: %v", port, err)
	}
	return pkgcore.NewSMTPMailer(pkgcore.SMTPConfig{
		Host:               host,
		Port:               portNumber,
		TLSMode:            pkgcore.SMTPTLSModeAuto,
		InsecureSkipVerify: true,
	})
}
