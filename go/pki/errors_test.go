package pki

import (
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"
)

// TestErrors_HaveTheExpectedCodesAndStatuses pins every error this module
// declares to its code and suggested HTTP status, so a future edit that
// accidentally changes either is caught here rather than downstream in a
// consumer that matched on the old value.
func TestErrors_HaveTheExpectedCodesAndStatuses(t *testing.T) {
	tests := []struct {
		name       string
		err        *apperr.Error
		wantCode   string
		wantStatus int
	}{
		{"ErrAuthorityNotFound", ErrAuthorityNotFound, "pki.authority_not_found", 404},
		{"ErrKeyNotFound", ErrKeyNotFound, "pki.key_not_found", 404},
		{"ErrNoActiveKey", ErrNoActiveKey, "pki.no_active_key", 404},
		{"ErrAlgorithmUnsupportedBySigner", ErrAlgorithmUnsupportedBySigner, "pki.algorithm_unsupported_by_signer", 400},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", tc.err.Code, tc.wantCode)
			}
			if tc.err.Status != tc.wantStatus {
				t.Errorf("Status = %d, want %d", tc.err.Status, tc.wantStatus)
			}
		})
	}
}
