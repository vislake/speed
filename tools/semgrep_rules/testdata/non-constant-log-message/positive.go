// Fixture for tools/semgrep_rules/non-constant-log-message.yml.
// Planted violations: every pattern shape must fire on this file.
// This file is NOT shipped code -- it proves the rule fires.
package fixture

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

func badLogging(ctx pkgcore.Context, tenantID string) {
	log := observability.FromContext(ctx)
	log.Info(fmt.Sprintf("enqueued for tenant %s", tenantID))       // fires: Sprintf message
	log.Warn("retrying job " + tenantID)                            // fires: concatenation message
	msg := fmt.Sprintf("dead letter for %s", tenantID)
	log.Error(msg)                                                  // fires: variable message
	if tenantID == "" {
		log.Debug(fmt.Sprintf("no tenant on %d", 42))               // fires: Sprintf on Debug
	}
}

func stdlibLookalike(w http.ResponseWriter, body string) {
	// net/http helper -- filtered by pattern-not, must NOT fire even
	// though its shape collides with a logger call ($M=Error).
	http.Error(w, body, http.StatusInternalServerError)
}

func testingLookalikes(t *testing.T, b *testing.B, tb testing.TB, problem string) {
	// Go testing reporting methods -- filtered by pattern-not, must NOT
	// fire even though their shape collides with a logger call
	// ($RECV=t|b|tb, $M=Error). These appear outside *_test.go too:
	// exported assertion helpers such as go/saasctl's appconfigtest.
	t.Error(problem)
	b.Error(problem)
	tb.Error(problem)
}
