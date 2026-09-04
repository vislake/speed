package pki

import (
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/vislake/speed/go/pki/locales"
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
		{"ErrCertificateRevoked", ErrCertificateRevoked, "pki.certificate_revoked", 409},
		{"ErrSignerUnavailable", ErrSignerUnavailable, "pki.signer_unavailable", 500},
		{"ErrPropagationWindowNotElapsed", ErrPropagationWindowNotElapsed, "pki.propagation_window_not_elapsed", 409},
		{"ErrCRLNotGenerated", ErrCRLNotGenerated, "pki.crl_not_generated", 404},
		{"ErrInternal", ErrInternal, "pki.internal_error", 500},
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

// TestErrorCatalog_EveryCodeHasBothLocales is the i18n rule enforced where
// it can actually be enforced: in code, against the embedded files this
// module ships.
//
// tools/check_i18n_keys.py already proves the two files carry identical key
// sets, and pkgcore/i18n's Builder.AddModule fails a bootstrap when they do
// not. Neither of those notices the case that matters most here: an error
// code returned by handler.go but declared in NEITHER file -- exactly the
// bug this test reproduces (pki.invalid_request_body and
// pki.revocation_reason_required were built inline via apperr.Invalid(...)
// in handler.go, absent from errors.go's var block and from both locale
// files; because both files omitted them equally, the key-parity check
// between the two languages could never have caught it). A client receiving
// such a code has nothing to render and falls back to showing the raw code
// to a user.
func TestErrorCatalog_EveryCodeHasBothLocales(t *testing.T) {
	t.Parallel()

	for _, language := range []string{"zh-CN", "en-US"} {
		t.Run(language, func(t *testing.T) {
			messages := loadLocale(t, language)
			for _, code := range errorCodes {
				if _, ok := messages[code]; !ok {
					t.Errorf("error code %q has no %s message; every code must ship text in both languages", code, language)
				}
			}
		})
	}
}

// TestLocales_CarryNoMessageWithoutACode is the other direction: a message
// id nothing returns is dead weight that survives forever because nobody
// can tell it is dead.
func TestLocales_CarryNoMessageWithoutACode(t *testing.T) {
	t.Parallel()

	known := make(map[string]bool, len(errorCodes))
	for _, code := range errorCodes {
		known[code] = true
	}

	for _, language := range []string{"zh-CN", "en-US"} {
		for id := range loadLocale(t, language) {
			if !known[id] {
				t.Errorf("%s carries message %q, which no error code in errors.go returns", language, id)
			}
		}
	}
}

// loadLocale reads one embedded locale file into a flat id -> text map.
func loadLocale(t *testing.T, language string) map[string]string {
	t.Helper()

	raw, err := locales.FS.ReadFile(language + ".toml")
	if err != nil {
		t.Fatalf("read the embedded %s locale: %v", language, err)
	}
	messages := map[string]string{}
	if err := toml.Unmarshal(raw, &messages); err != nil {
		t.Fatalf("parse the embedded %s locale: %v -- every entry must be one flat top-level message, with no grouping sections", language, err)
	}
	return messages
}
