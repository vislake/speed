package jobs

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

func TestTask_Validate(t *testing.T) {
	tests := []struct {
		name      string
		task      Task
		wantErr   bool
		wantField string
	}{
		{
			name: "valid",
			task: Task{Type: "notes.export", TenantID: pkgcore.TenantID("tenant-a")},
		},
		{
			name:      "empty type",
			task:      Task{TenantID: pkgcore.TenantID("tenant-a")},
			wantErr:   true,
			wantField: "type",
		},
		{
			name:      "empty tenant",
			task:      Task{Type: "notes.export"},
			wantErr:   true,
			wantField: "tenant_id",
		},
		{
			name:      "both empty",
			task:      Task{},
			wantErr:   true,
			wantField: "type", // Type is checked first.
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.task.Validate()
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}

			appErr, ok := apperr.As(err)
			if !ok {
				t.Fatalf("Validate() error = %v, want an *apperr.Error", err)
			}
			if appErr.Code != ErrInvalidTask.Code {
				t.Errorf("Validate() code = %q, want %q", appErr.Code, ErrInvalidTask.Code)
			}
			if got := appErr.Params["field"]; got != tt.wantField {
				t.Errorf("Validate() field param = %v, want %q", got, tt.wantField)
			}
		})
	}
}
