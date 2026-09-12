package pki

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/i18n"
	"github.com/vislake/speed/go/pki/locales"
)

// TestErrors_HaveTheExpectedCodesAndStatuses pins every error this module
// declares to its code and suggested HTTP status, so an edit that
// accidentally changes either is caught here rather than downstream in a
// consumer that matched on the previous value.
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
		{"ErrAuthorityRevoked", ErrAuthorityRevoked, "pki.authority_revoked", 409},
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

// newPKICatalog builds pki's REAL locale bundle through the same
// i18n.Builder the assembly uses, so the tests render from the exact text
// the shipped files carry: a code the catalog cannot render fails here, in
// the test, rather than in production.
func newPKICatalog(t *testing.T) *i18n.Catalog {
	t.Helper()

	builder := i18n.NewBuilder()
	if err := builder.AddModule(moduleName, locales.FS); err != nil {
		t.Fatalf("build the pki message catalog: %v", err)
	}
	return builder.Build()
}

// TestErrorCatalog_EveryCodeHasBothLocales is the i18n rule enforced where
// it can actually be enforced: in code, against the merged catalog built
// from the embedded files this module ships through the same i18n.Builder
// the assembly drives.
//
// tools/check_i18n_keys.py already proves the two files carry identical key
// sets, and pkgcore/i18n's Builder.AddModule fails a bootstrap when they do
// not. Neither check covers the case that matters most here: an error code
// this module returns but that is declared in NEITHER file. Two codes
// omitted from both files equally keep the parity checks green -- yet a
// client receiving such a code has nothing to render and falls back to
// showing the raw code to a user.
func TestErrorCatalog_EveryCodeHasBothLocales(t *testing.T) {
	t.Parallel()

	catalog := newPKICatalog(t)
	for _, language := range []string{"zh-CN", "en-US"} {
		t.Run(language, func(t *testing.T) {
			t.Parallel()

			for _, code := range errorCodes {
				message, err := catalog.Lookup(language, code, map[string]any{})
				if err != nil {
					t.Errorf("error code %q has no %s message: %v; every code must ship text in both languages", code, language, err)
					continue
				}
				if message == "" {
					t.Errorf("error code %q renders as an empty %s message", code, language)
				}
			}
		})
	}
}

// TestLocales_CarryNoMessageWithoutACode is the other direction: a message
// id nothing returns is dead weight that survives forever because nobody
// can tell it is dead. The census runs through the mechanism the assembly
// uses: for each language, the module's real file is merged beside a
// synthetic file for the other language carrying EXACTLY the ids this
// package claims (errorCodes), so AddModule's own key-parity rule -- which
// compares both directions and names every differing id in its error -- is
// the comparison. An id shipped but unclaimed surfaces as a real id the
// synthetic file lacks; a claimed id with no text to render surfaces as a
// synthetic id the real file lacks. Either drift fails here.
func TestLocales_CarryNoMessageWithoutACode(t *testing.T) {
	t.Parallel()

	for _, language := range []string{"zh-CN", "en-US"} {
		t.Run(language, func(t *testing.T) {
			t.Parallel()

			other := "en-US"
			if language == other {
				other = "zh-CN"
			}
			real, err := locales.FS.ReadFile(language + ".toml")
			if err != nil {
				t.Fatalf("read the embedded %s locale: %v", language, err)
			}
			fsys := fstest.MapFS{
				language + ".toml": {Data: real},
				other + ".toml":    {Data: censusLocaleFile(errorCodes)},
			}
			builder := i18n.NewBuilder()
			if err := builder.AddModule(moduleName, fsys); err != nil {
				t.Errorf("the %s id set drifted from the ids this package claims: %v", language, err)
			}
		})
	}
}

// censusLocaleFile renders ids as one flat locale file of placeholder
// messages, the exact shape the locale-file contract prescribes: quoted
// top-level ids, non-empty string values, every id inside the module's own
// prefix.
func censusLocaleFile(ids []string) []byte {
	var b strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&b, "%q = \"census\"\n", id)
	}
	return []byte(b.String())
}
