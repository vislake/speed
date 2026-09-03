// Fixture for tools/semgrep_rules/non-constant-log-message.yml.
// Clean control: none of these patterns may fire.
package fixture

import (
	"github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
)

func goodLogging(ctx pkgcore.Context, jobID string, attempts int) {
	log := observability.FromContext(ctx)
	// The message is a constant; everything variable is an attribute.
	log.Info("job enqueued", "job_id", jobID)
	log.Warn("job retrying", "job_id", jobID, "attempts", attempts)
	log.Error("job failed")
	log.Debug("worker woke")
}
