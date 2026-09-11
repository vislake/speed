package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"image"
	_ "image/gif"  // register the GIF decoder, part of the pixel-check envelope
	_ "image/jpeg" // register the JPEG decoder
	_ "image/png"  // register the PNG decoder
	"mime"
	"net/http"
	"sort"
	"strings"
)

// This file holds the server-side content-revalidation primitives: the checks
// Complete's pipeline runs over the bytes as they actually arrived, and the
// checksum/media-type helpers Create uses to refuse a malformed declaration
// before any byte is uploaded. Everything here is pure -- bytes and strings
// in, verdicts and errors out -- so the pipeline ordering policy (which check
// runs first, which error wins) lives with its only consumer, ObjectService,
// and every primitive is testable on its own.
//
// The trust model the checks implement: the uploader's Declared* values are
// claims, and the probe of the stored bytes is the authority. A claim the
// bytes do not honour fails the upload; a claim that is only malformed fails
// it earlier, before a byte of body is ever accepted.

// canonicalMediaType folds a declared media type into the module's comparison
// form: parameters stripped (a browser may append "; charset=binary"), case
// lowered ("Image/JPEG" is "image/jpeg"), surrounding whitespace trimmed.
// Unparseable input degrades to a trimmed, lowercased, parameter-stripped
// guess rather than an error, so the declared-vs-probed comparison downstream
// still has a value to compare -- a value that will not match the probe, which
// is the correct outcome for a media type that is not one.
func canonicalMediaType(raw string) string {
	if mt, _, err := mime.ParseMediaType(strings.TrimSpace(raw)); err == nil {
		return strings.ToLower(mt)
	}
	if i := strings.IndexByte(raw, ';'); i >= 0 {
		raw = raw[:i]
	}
	return strings.ToLower(strings.TrimSpace(raw))
}

// probeMediaType reports the media type http.DetectContentType assigns to
// head, the probe the pipeline trusts over any declared value. The probe is
// content-addressable: it looks at magic bytes, never at a filename or a
// header a caller controls, so it is the server-side authority for what the
// bytes actually are. DetectContentType examines at most 512 bytes, which is
// enough for every magic the module's allowlist can admit.
func probeMediaType(head []byte) string {
	return http.DetectContentType(head)
}

// validSHA256Hex reports whether s is a well-formed SHA-256 digest: exactly
// 64 lowercase hex characters. Uppercase is refused on purpose so the stored
// comparison form is the canonical one and no two spellings of one digest can
// drift apart.
func validSHA256Hex(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// sha256HexDigest returns the SHA-256 digest of b in the module's canonical
// lowercase-hex form.
func sha256HexDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// checkStoredSize compares the byte count the store actually held against the
// size the uploader declared. A mismatch means the stored bytes are not the
// upload the create described, and the completed row must not claim a size it
// cannot honour.
func checkStoredSize(actual, declared int64) error {
	if actual == declared {
		return nil
	}
	return ErrSizeMismatch.WithParam("declared", declared).WithParam("actual", actual)
}

// checkContentChecksum compares the digest of the stored bytes against the
// checksum the uploader declared. An empty declared checksum is a no-op: the
// uploader chose not to claim one, and the pipeline stores the bytes' own
// digest in the finalized row instead.
func checkContentChecksum(raw []byte, declared string) error {
	if declared == "" {
		return nil
	}
	if sha256HexDigest(raw) != declared {
		return ErrChecksumMismatch
	}
	return nil
}

// checkAllowedMediaType refuses a probed media type outside the module's
// configured allowlist. The allowlist is the module's own bound, configured
// once by the host -- never a per-object judgement -- and the probe, not the
// declared type, is what is checked against it: a file that claims image/png
// but probes as something else entirely is refused even if the claim was
// allowlisted.
func checkAllowedMediaType(probed string, allowed []string) error {
	for _, t := range allowed {
		if t == probed {
			return nil
		}
	}
	return ErrTypeNotAllowed.WithParam("allowed", strings.Join(allowed, ","))
}

// checkDeclaredTypeMatches reconciles the declared media type with the probe
// of the stored bytes. An empty declaration is a no-op -- no claim, nothing
// to be contradicted -- and the probe then stands alone. A declaration the
// bytes contradict is refused: storing an HTML page under a claimed image/jpeg
// would hand a browser a content-type lie.
func checkDeclaredTypeMatches(declared, probed string) error {
	if declared == "" {
		return nil
	}
	if canonicalMediaType(declared) != probed {
		return ErrTypeMismatch
	}
	return nil
}

// isImageMediaType reports whether a probed media type names a raster image.
// It gates the pixel checks: only image/* types have a pixel grid for the
// ceiling and the dimension columns to bound.
func isImageMediaType(mt string) bool {
	return strings.HasPrefix(mt, "image/")
}

// mediaTypeCoverage is the module's record of what it can do to one media
// type's bytes: pixel-check them (a decoder is registered in this file's
// import block) and metadata-strip them (sanitize.go ships a structural
// walker for the type, over the location/authorship carrier classes that
// walker's own scope note enumerates -- strippable is measured against
// that declared coverage, never against payload bytes no structural walk
// can classify).
type mediaTypeCoverage struct {
	decodable  bool
	strippable bool
}

// mediaTypeSafety is the module's one place of record tying the three facts
// the whitelist's safety promise rests on -- which probed types have a
// registered decoder, which have metadata-strip coverage, and what the
// whitelist may therefore admit. Register enforces the whitelist side of the
// table (module.go: every WithAllowedTypes entry must pass
// checkAdmittedMediaType), so a type with a decoder but no strip walker --
// image/gif below -- can never be admitted by configuration: a host that
// wants it must first give the module strip coverage for it, the same
// change that would flip this table's strippable half.
var mediaTypeSafety = map[string]mediaTypeCoverage{
	// The module default allowlist: full coverage, both halves true.
	"image/jpeg": {decodable: true, strippable: true},
	"image/png":  {decodable: true, strippable: true},
	// image/gif is decodable (its decoder sits in the pixel-check envelope
	// above) but has NO metadata-strip coverage: sanitize.go ships no GIF
	// walker, so a GIF's comment and application extensions would ride
	// through the strip stage untouched. The type is recorded here with its
	// honest halves -- decodable true, strippable false -- so the admission
	// gate refuses it with the missing half named, rather than letting a
	// pixel-checked, un-stripped GIF into the whitelist as if it carried
	// both protections.
	"image/gif": {decodable: true, strippable: false},
}

// checkAdmittedMediaType reports whether the whitelist may admit mt: the
// module must be able to apply its full safety envelope -- pixel-check AND
// metadata-strip -- to every byte that will complete under the type. A type
// missing either half is refused with ErrAllowedTypeUnsupported naming the
// type and the types that do carry full coverage, so a host configuring the
// whitelist is told what it asked for and what it could have asked for.
// Module.Register is the gate's caller; a configured type cannot reach the
// service's allowlist past it.
func checkAdmittedMediaType(mt string) error {
	caps, known := mediaTypeSafety[mt]
	if known && caps.decodable && caps.strippable {
		return nil
	}
	return ErrAllowedTypeUnsupported.
		WithParam("type", mt).
		WithParam("admissible", strings.Join(admissibleMediaTypes(), ","))
}

// admissibleMediaTypes returns the media types the whitelist may currently
// admit -- every table entry carrying both halves of the safety envelope --
// in sorted order, for the admission gate's refusal message and its tests.
func admissibleMediaTypes() []string {
	var out []string
	for mt, caps := range mediaTypeSafety {
		if caps.decodable && caps.strippable {
			out = append(out, mt)
		}
	}
	sort.Strings(out)
	return out
}

// decodeImageFacts decodes enough of buf to establish its pixel dimensions,
// applying the module's configured pixel ceiling on the way. It is the
// pipeline's header-only decode: image.DecodeConfig parses the format's
// header (the SOF marker for JPEG, the IHDR chunk for PNG), never the entropy
// data, so a 100 MiB upload costs a bounded decode even at its ceiling.
//
// The ceiling bounds decode memory downstream -- a small file can declare a
// gigantic pixel grid, which is exactly the pixel-bomb shape -- so pixels are
// limited independently of bytes. Decode failures and non-positive dimensions
// report ErrImageUnreadable with the decoder's error as the cause; both mean
// the bytes' own header cannot establish what the row would need to claim.
func decodeImageFacts(buf []byte, maxPixels int64) (width, height int, err error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(buf))
	if err != nil {
		return 0, 0, ErrImageUnreadable.WithCause(err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return 0, 0, ErrImageUnreadable
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return 0, 0, ErrPixelLimitExceeded.WithParam("max_pixels", maxPixels)
	}
	return cfg.Width, cfg.Height, nil
}
