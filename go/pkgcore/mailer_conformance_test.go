// This file lives in package pkgcore_test — the external test package,
// distinct from mailer_test.go's and smtp_mailer_test.go's internal package
// pkgcore — because it must import go/pkgcore/mailertest, which itself
// imports go/pkgcore: an internal test file (package pkgcore) importing a
// package that imports pkgcore back is an import cycle Go's toolchain
// refuses ("import cycle not allowed in test"), while an external test file
// compiles as a separate package and carries no such restriction. This is
// the mechanical exception the backend coding standard's testing-layout
// rule names for exactly this situation (package x vs. package x_test cases
// cannot share a file), not a new test-organization convention — see
// eventbus_conformance_test.go's identical note.
package pkgcore_test

import (
	"fmt"
	"net"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/internal/testutil"
	"github.com/vislake/speed/go/pkgcore/mailertest"
)

// TestSMTPMailer_ConformsToMailerContract proves pkgcore.NewSMTPMailer still
// satisfies the shared contract mailertest.AssertConforms checks, after the
// deployment-composition retrofit (Phase 1) generalized how a Kernel
// resolves and validates its Mailer seam — this is what proves the retrofit
// did not silently change NewSMTPMailer's own behavior for its existing
// callers. It needs no Docker: the fake relay is an in-process
// net.Listener, the same scripted double smtp_mailer_test.go's own
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

// smtpMailerFor builds a pkgcore.Mailer pointed at server. It duplicates the
// handful of lines smtp_mailer_test.go's own unexported mailerFor already
// has, rather than sharing one implementation, because the two live in
// different packages for the import-cycle reason this file's own doc
// comment explains (mailerFor's would-be shared home, internal/testutil,
// cannot import pkgcore at all — see fake_smtp_server.go's doc comment) —
// each side of the cycle needs its own thin pkgcore.Mailer wrapper around
// FakeSMTPServer.Addr().
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
