package storage

import (
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore/apperr"

	"github.com/vislake/speed/go/storage/internal/testutil"
)

// This file tests the revalidation primitives of validate.go. The pipeline
// ordering they serve belongs to ObjectService, whose own suite drives whole
// lifecycles; here each check is pinned in isolation -- its error code, its
// parameters, and its verdict on the boundary cases that decide whether a
// declared value is a claim or a lie.

func TestCanonicalMediaType(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already canonical", "image/png", "image/png"},
		{"parameter stripped", "image/png; charset=binary", "image/png"},
		{"case folded", "IMAGE/JPEG", "image/jpeg"},
		{"case folded with parameters", "Image/Jpeg;foo=bar", "image/jpeg"},
		{"whitespace trimmed", "  image/png  ", "image/png"},
		{"compound type preserved", "image/svg+xml", "image/svg+xml"},
		{"empty input stays empty", "", ""},
		{"unparseable degrades to guess", "not a media type", "not a media type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalMediaType(tc.in); got != tc.want {
				t.Fatalf("canonicalMediaType(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestProbeMediaType(t *testing.T) {
	if got, want := probeMediaType(testutil.JPEG(t, 4, 4)), "image/jpeg"; got != want {
		t.Fatalf("jpeg probe = %q, want %q", got, want)
	}
	if got, want := probeMediaType(testutil.PNG(t, 4, 4)), "image/png"; got != want {
		t.Fatalf("png probe = %q, want %q", got, want)
	}
	// A non-image body probes as text, never as the image a caller's header
	// claimed -- the probe looks at bytes, which is the point.
	if got, want := probeMediaType([]byte("plain text body")), "text/plain; charset=utf-8"; got != want {
		t.Fatalf("text probe = %q, want %q", got, want)
	}
}

func TestValidSHA256Hex(t *testing.T) {
	valid := strings.Repeat("0123456789abcdef", 4)
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"valid digest", valid, true},
		{"too short", valid[:63], false},
		{"too long", valid + "0", false},
		{"uppercase refused", strings.ToUpper(valid), false},
		{"non-hex character", strings.Repeat("g", 64), false},
		{"empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validSHA256Hex(tc.in); got != tc.want {
				t.Fatalf("validSHA256Hex() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSHA256HexDigest(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"empty input", nil, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"known vector", []byte("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sha256HexDigest(tc.in); got != tc.want {
				t.Fatalf("sha256HexDigest() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCheckStoredSize(t *testing.T) {
	if err := checkStoredSize(100, 100); err != nil {
		t.Fatalf("equal sizes rejected: %v", err)
	}
	err := checkStoredSize(99, 100)
	if !hasCode(err, "storage.size_mismatch") {
		t.Fatalf("mismatch code = %v, want storage.size_mismatch", err)
	}
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error is not an *apperr.Error: %v", err)
	}
	if got, want := appErr.Params["declared"], int64(100); got != want {
		t.Fatalf("declared param = %v, want %v", got, want)
	}
	if got, want := appErr.Params["actual"], int64(99); got != want {
		t.Fatalf("actual param = %v, want %v", got, want)
	}
}

func TestCheckContentChecksum(t *testing.T) {
	raw := []byte("the bytes that arrived")
	digest := sha256HexDigest(raw)
	if err := checkContentChecksum(raw, digest); err != nil {
		t.Fatalf("matching checksum rejected: %v", err)
	}
	// An empty declared checksum means the uploader claimed nothing, so
	// there is nothing to contradict.
	if err := checkContentChecksum(raw, ""); err != nil {
		t.Fatalf("empty declared checksum rejected: %v", err)
	}
	// A checksum of different bytes is a claim the stored bytes dishonour.
	err := checkContentChecksum(raw, sha256HexDigest([]byte("different bytes")))
	if !hasCode(err, "storage.checksum_mismatch") {
		t.Fatalf("mismatch code = %v, want storage.checksum_mismatch", err)
	}
}

func TestCheckAllowedMediaType(t *testing.T) {
	allowed := []string{"image/jpeg", "image/png"}
	if err := checkAllowedMediaType("image/png", allowed); err != nil {
		t.Fatalf("allowlisted type refused: %v", err)
	}
	err := checkAllowedMediaType("image/gif", allowed)
	if !hasCode(err, "storage.type_not_allowed") {
		t.Fatalf("refusal code = %v, want storage.type_not_allowed", err)
	}
	appErr, ok := apperr.As(err)
	if !ok {
		t.Fatalf("error is not an *apperr.Error: %v", err)
	}
	if got, want := appErr.Params["allowed"], "image/jpeg,image/png"; got != want {
		t.Fatalf("allowed param = %q, want %q", got, want)
	}
	// An empty allowlist admits nothing; the service resolves a nil
	// configuration to the module default before this check ever runs.
	if err := checkAllowedMediaType("image/jpeg", nil); !hasCode(err, "storage.type_not_allowed") {
		t.Fatalf("empty allowlist refused nothing: %v", err)
	}
}

func TestCheckDeclaredTypeMatches(t *testing.T) {
	cases := []struct {
		name     string
		declared string
		probed   string
		wantErr  bool
	}{
		{"exact match", "image/png", "image/png", false},
		{"declared canonicalized", "IMAGE/JPEG", "image/jpeg", false},
		{"declared with parameters", "image/jpeg; charset=binary", "image/jpeg", false},
		{"no declaration, no contradiction", "", "text/html", false},
		{"declared contradicts probe", "image/jpeg", "text/html", true},
		{"case differs from probe", "image/png", "image/PNG", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkDeclaredTypeMatches(tc.declared, tc.probed)
			if tc.wantErr && !hasCode(err, "storage.type_mismatch") {
				t.Fatalf("code = %v, want storage.type_mismatch", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("match rejected: %v", err)
			}
		})
	}
}

func TestDecodeImageFacts(t *testing.T) {
	t.Run("jpeg dimensions", func(t *testing.T) {
		w, h, err := decodeImageFacts(testutil.JPEG(t, 64, 32), 1<<30)
		if err != nil {
			t.Fatalf("decodeImageFacts: %v", err)
		}
		if w != 64 || h != 32 {
			t.Fatalf("dimensions = %dx%d, want 64x32", w, h)
		}
	})
	t.Run("png dimensions", func(t *testing.T) {
		w, h, err := decodeImageFacts(testutil.PNG(t, 11, 7), 1<<30)
		if err != nil {
			t.Fatalf("decodeImageFacts: %v", err)
		}
		if w != 11 || h != 7 {
			t.Fatalf("dimensions = %dx%d, want 11x7", w, h)
		}
	})
	t.Run("pixel ceiling", func(t *testing.T) {
		_, _, err := decodeImageFacts(testutil.PNG(t, 100, 100), 5000)
		if !hasCode(err, "storage.pixel_limit_exceeded") {
			t.Fatalf("code = %v, want storage.pixel_limit_exceeded", err)
		}
		appErr, _ := apperr.As(err)
		if got, want := appErr.Params["max_pixels"], int64(5000); got != want {
			t.Fatalf("max_pixels param = %v, want %v", got, want)
		}
	})
	t.Run("exactly at ceiling admitted", func(t *testing.T) {
		if _, _, err := decodeImageFacts(testutil.PNG(t, 50, 100), 5000); err != nil {
			t.Fatalf("exact-ceiling image rejected: %v", err)
		}
	})
	t.Run("undecodable bytes", func(t *testing.T) {
		_, _, err := decodeImageFacts([]byte("this is not an image at all"), 1<<30)
		if !hasCode(err, "storage.image_unreadable") {
			t.Fatalf("code = %v, want storage.image_unreadable", err)
		}
		// The decoder's own error rides along as the cause, kept out of any
		// response body.
		if _, hasCause := apperr.As(err); !hasCause {
			t.Fatal("image_unreadable error carries no cause")
		}
	})
}

func TestIsImageMediaType(t *testing.T) {
	for _, mt := range []string{"image/jpeg", "image/png", "image/gif", "image/svg+xml"} {
		if !isImageMediaType(mt) {
			t.Fatalf("isImageMediaType(%q) = false, want true", mt)
		}
	}
	for _, mt := range []string{"text/plain", "application/pdf", "image", "IMAGE/JPEG"} {
		if isImageMediaType(mt) {
			t.Fatalf("isImageMediaType(%q) = true, want false", mt)
		}
	}
}
