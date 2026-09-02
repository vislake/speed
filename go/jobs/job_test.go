package jobs

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// isJobNotFound reports whether err is, or wraps, ErrJobNotFound, matched
// by Code rather than identity -- the same discipline dbkit's own
// isRecordNotFound test helpers use for ErrRecordNotFound (see
// dbkit/AGENTS.md's Rules section), so a future decoration of
// ErrJobNotFound with WithParam/WithCause (which derives a new *apperr.Error
// instance, per apperr's own doc comment) does not silently break every
// test asserting this error across the package's test files -- store_test.go
// and standalone_queue_test.go both use this, which is why it lives here rather
// than duplicated in each.
func isJobNotFound(err error) bool {
	appErr, ok := apperr.As(err)
	return ok && appErr.Code == ErrJobNotFound.Code
}

func TestStatus_Terminal(t *testing.T) {
	tests := []struct {
		name   string
		status Status
		want   bool
	}{
		{name: "pending", status: StatusPending, want: false},
		{name: "running", status: StatusRunning, want: false},
		{name: "retrying", status: StatusRetrying, want: false},
		{name: "succeeded", status: StatusSucceeded, want: true},
		{name: "dead_letter", status: StatusDeadLetter, want: true},
		{name: "cancelled", status: StatusCancelled, want: true},
		{name: "unknown value", status: Status("bogus"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.status.Terminal(); got != tt.want {
				t.Errorf("Status(%q).Terminal() = %v, want %v", tt.status, got, tt.want)
			}
		})
	}
}
