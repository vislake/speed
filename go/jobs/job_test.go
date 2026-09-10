package jobs

import (
	"testing"
)

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
