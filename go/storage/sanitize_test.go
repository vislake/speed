package storage

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/draw"
	"testing"

	"github.com/vislake/speed/go/storage/internal/testutil"
)

// This file tests the structural metadata strips of sanitize.go. The carriers
// are built to be realistic -- the JPEG tests graft genuine Exif/XMP APP1
// segments (the Exif one holding a real little-endian TIFF whose IFD points
// at a GPS IFD) and an ICC APP2 profile onto decodable images -- and every
// "kept the pixels" assertion is a full decode-and-compare of the output
// against the pristine base, so a strip that removed data along with
// metadata would fail here rather than only in the service round.

// exifPayload returns the bytes of an APP1 EXIF payload: the "Exif\0\0"
// signature followed by a little-endian TIFF whose IFD0 entry 0x8825 points
// at a GPS IFD carrying GPSLatitudeRef = "N". Structure is real enough that
// an EXIF parser would read it; the walker never looks deeper than the
// signature, so the test's load-bearing part is that the whole segment --
// GPS bytes included -- disappears.
func exifPayload() []byte {
	p := append([]byte(nil), exifSignature...)
	p = append(p, 'I', 'I', '*', 0)                          // TIFF header, little-endian magic
	p = append(p, 8, 0, 0, 0)                                // IFD0 lives at file offset 8
	p = append(p, 1, 0)                                      // IFD0 holds one entry
	p = append(p, 0x25, 0x88, 4, 0, 1, 0, 0, 0, 26, 0, 0, 0) // GPSInfo (0x8825), LONG, offset 26
	p = append(p, 0, 0, 0, 0)                                // IFD0 has no next IFD
	p = append(p, 1, 0)                                      // GPS IFD holds one entry
	p = append(p, 1, 0, 2, 0, 2, 0, 0, 0, 'N', 0, 0, 0)      // GPSLatitudeRef (0x0001), ASCII "N"
	p = append(p, 0, 0, 0, 0)                                // GPS IFD has no next IFD
	return p
}

// xmpPayload returns the bytes of an APP1 XMP packet: Adobe's namespace
// signature followed by an XMP metadata fragment.
func xmpPayload() []byte {
	p := append([]byte(nil), xmpSignature...)
	return append(p, "<x:xmpmeta xmlns:x=\"adobe:ns:meta/\"></x:xmpmeta>"...)
}

// insertAPPSegment splices a length-carrying APP segment carrying payload
// directly after the SOI marker of a JPEG, the position a camera's firmware
// would have written it in. marker is the segment's marker code (0xE1 for
// APP1, 0xE2 for APP2, ...).
func insertAPPSegment(t *testing.T, raw []byte, marker byte, payload []byte) []byte {
	t.Helper()
	if len(raw) < 2 || raw[0] != jpegMarkerPrefix || raw[1] != jpegSOI {
		t.Fatalf("base jpeg lacks its SOI prefix")
	}
	seg := []byte{jpegMarkerPrefix, marker}
	seg = binary.BigEndian.AppendUint16(seg, uint16(len(payload)+2))
	seg = append(seg, payload...)
	out := make([]byte, 0, len(raw)+len(seg))
	out = append(out, raw[:2]...)
	out = append(out, seg...)
	out = append(out, raw[2:]...)
	return out
}

// insertPNGChunk splices a chunk of the given type, carrying data, directly
// after a PNG's signature, with the CRC-32 the file format requires.
func insertPNGChunk(t *testing.T, raw []byte, chunkType string, data []byte) []byte {
	t.Helper()
	if !bytes.HasPrefix(raw, pngSignature) {
		t.Fatalf("base png lacks its signature")
	}
	chunk := make([]byte, 0, 12+len(data))
	chunk = binary.BigEndian.AppendUint32(chunk, uint32(len(data)))
	chunk = append(chunk, chunkType...)
	chunk = append(chunk, data...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	out := make([]byte, 0, len(raw)+len(chunk))
	out = append(out, raw[:len(pngSignature)]...)
	out = append(out, chunk...)
	out = append(out, raw[len(pngSignature):]...)
	return out
}

// appendTailSegment splices a length-carrying APP segment carrying payload
// after a JPEG's EOI marker, the position an uploader's appended metadata or
// a second writer's edit trail would land in. Marker codes and payloads are
// as in insertAPPSegment. Calling it repeatedly stacks segments after the
// same EOI; the caller is responsible for payloads that contain no 0xFF 0xD9
// sequence of their own.
func appendTailSegment(t *testing.T, raw []byte, marker byte, payload []byte) []byte {
	t.Helper()
	if bytes.LastIndex(raw, []byte{jpegMarkerPrefix, jpegEOI}) < 0 {
		t.Fatalf("base jpeg has no EOI marker to append after")
	}
	seg := []byte{jpegMarkerPrefix, marker}
	seg = binary.BigEndian.AppendUint16(seg, uint16(len(payload)+2))
	seg = append(seg, payload...)
	return append(append([]byte(nil), raw...), seg...)
}

// assertDecodesEqual decodes both byte slices and asserts their pixel grids
// are identical. It is the "the strip took only metadata" proof: metadata
// removal never touches entropy data, so the decoded output of a stripped
// image must equal the decoded output of the pristine one exactly.
func assertDecodesEqual(t *testing.T, label string, a, b []byte) {
	t.Helper()
	decode := func(which string, raw []byte) *image.RGBA {
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("decode %s %s: %v", label, which, err)
		}
		dst := image.NewRGBA(img.Bounds())
		draw.Draw(dst, dst.Bounds(), img, img.Bounds().Min, draw.Src)
		return dst
	}
	ca, cb := decode("first", a), decode("second", b)
	if ca.Bounds() != cb.Bounds() {
		t.Fatalf("%s: bounds differ: %v vs %v", label, ca.Bounds(), cb.Bounds())
	}
	if !bytes.Equal(ca.Pix, cb.Pix) {
		t.Fatalf("%s: decoded pixels differ", label)
	}
}

func TestSanitizeJPEG_CleanPassthrough(t *testing.T) {
	base := testutil.JPEG(t, 48, 32)
	out, err := sanitizeJPEG(base)
	if err != nil {
		t.Fatalf("sanitizeJPEG: %v", err)
	}
	if !bytes.Equal(out, base) {
		t.Fatal("metadata-free jpeg was rewritten")
	}
	assertDecodesEqual(t, "clean passthrough", out, base)
}

func TestSanitizeJPEG_StripsExifAndXMP(t *testing.T) {
	base := testutil.JPEG(t, 48, 32)
	withExif := insertAPPSegment(t, base, jpegApp1, exifPayload())
	withBoth := insertAPPSegment(t, withExif, jpegApp1, xmpPayload())
	out, err := sanitizeJPEG(withBoth)
	if err != nil {
		t.Fatalf("sanitizeJPEG: %v", err)
	}
	if bytes.Contains(out, exifSignature) {
		t.Fatal("exif signature survives the strip")
	}
	if bytes.Contains(out, xmpSignature) {
		t.Fatal("xmp signature survives the strip")
	}
	assertDecodesEqual(t, "exif+xmp strip", out, base)
	// Idempotent: the second pass over an already-clean file changes nothing.
	again, err := sanitizeJPEG(out)
	if err != nil {
		t.Fatalf("sanitizeJPEG (second pass): %v", err)
	}
	if !bytes.Equal(again, out) {
		t.Fatal("strip is not idempotent")
	}
}

func TestSanitizeJPEG_KeepsOtherMetadata(t *testing.T) {
	base := testutil.JPEG(t, 48, 32)
	// An APP1 that is neither Exif nor XMP (a vendor segment) is metadata
	// this strip does not claim to remove.
	foreign := []byte("com.example.vendor-special-data")
	// An APP2 ICC profile is explicitly kept -- stripping colour management
	// would change how the image renders, which is not this strip's job.
	icc := []byte("ICC_PROFILE\x00dental-color-profile-bytes")
	withBoth := insertAPPSegment(t, base, jpegApp1, foreign)
	withBoth = insertAPPSegment(t, withBoth, 0xE2, icc)
	out, err := sanitizeJPEG(withBoth)
	if err != nil {
		t.Fatalf("sanitizeJPEG: %v", err)
	}
	if !bytes.Contains(out, foreign) || !bytes.Contains(out, icc) {
		t.Fatal("non-target metadata was stripped")
	}
	assertDecodesEqual(t, "kept metadata", out, base)
}

func TestSanitizeJPEG_FillBytesTolerated(t *testing.T) {
	base := testutil.JPEG(t, 48, 32)
	// Extra 0xFF bytes between SOI and the first marker are legal fill; the
	// walker must not mistake them for structure.
	filled := append(append(append([]byte{}, base[:2]...), 0xFF, 0xFF, 0xFF), base[2:]...)
	out, err := sanitizeJPEG(filled)
	if err != nil {
		t.Fatalf("sanitizeJPEG with fill bytes: %v", err)
	}
	assertDecodesEqual(t, "fill bytes", out, base)
}

func TestSanitizeJPEG_StructureErrors(t *testing.T) {
	base := testutil.JPEG(t, 48, 32)
	sos := bytes.Index(base, []byte{jpegMarkerPrefix, jpegSOS})
	if sos < 0 {
		t.Fatal("test jpeg has no SOS marker")
	}
	corrupt := func(t *testing.T, name string, raw []byte) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			if _, err := sanitizeJPEG(raw); err == nil {
				t.Fatalf("sanitizeJPEG accepted structurally broken input (%s)", name)
			}
		})
	}
	corrupt(t, "missing SOI", []byte("these bytes are no jpeg"))
	corrupt(t, "SOI only", []byte{jpegMarkerPrefix, jpegSOI})
	corrupt(t, "duplicate SOI", append(append([]byte{}, base[:2]...), append([]byte{jpegMarkerPrefix, jpegSOI}, base[2:]...)...))
	corrupt(t, "EOI before scan data", append(append([]byte{}, base[:2]...), append([]byte{jpegMarkerPrefix, jpegEOI}, base[2:]...)...))
	reserved := append([]byte(nil), base...)
	reserved[3] = 0x02 // an undefined marker code where the first segment's code sits
	corrupt(t, "reserved marker code", reserved)
	corrupt(t, "truncated before scan data", base[:sos])
	corrupt(t, "segment cut mid-payload", base[:sos-3])
	// SOS is itself a length-carrying marker and its scan data must run to
	// EOI; a file cut inside either is structurally broken and refused, never
	// carried over on good faith. segEnd is where the entropy-coded data
	// begins; eoi is the terminating marker's offset.
	segEnd := sos + 2 + int(binary.BigEndian.Uint16(base[sos+2:sos+4]))
	eoi := bytes.LastIndex(base, []byte{jpegMarkerPrefix, jpegEOI})
	if segEnd >= eoi || eoi >= len(base) {
		t.Fatalf("test jpeg layout unexpected: segEnd=%d eoi=%d len=%d", segEnd, eoi, len(base))
	}
	corrupt(t, "SOS without header", base[:sos+2])
	corrupt(t, "scan header cut short", base[:sos+5])
	midScan := segEnd + (eoi-segEnd)/2
	if midScan <= segEnd || midScan >= eoi {
		t.Fatalf("mid-scan cut lands outside scan data: segEnd=%d mid=%d eoi=%d", segEnd, midScan, eoi)
	}
	corrupt(t, "scan data without end-of-image marker", base[:midScan])
}

// TestSanitizeJPEG_TrailingGarbageDiscarded pins the EOI boundary: the scan
// data's terminating marker is the end of the image, and anything an uploader
// appended after it -- an EXIF segment a second writer stuck on, XMP, or
// arbitrary bytes -- must not survive into the sanitized output. The drop is
// silent, exactly as the PNG walker drops chunks after IEND.
func TestSanitizeJPEG_TrailingGarbageDiscarded(t *testing.T) {
	base := testutil.JPEG(t, 48, 32)
	trailing := appendTailSegment(t, base, jpegApp1, exifPayload())
	trailing = appendTailSegment(t, trailing, jpegApp1, xmpPayload())
	trailing = append(trailing, "garbage bytes after end-of-image"...)
	out, err := sanitizeJPEG(trailing)
	if err != nil {
		t.Fatalf("sanitizeJPEG: %v", err)
	}
	if !bytes.Equal(out, base) {
		t.Fatal("post-EOI bytes were carried into the output")
	}
	if bytes.Contains(out, exifSignature) || bytes.Contains(out, xmpSignature) {
		t.Fatal("post-EOI metadata survives the strip")
	}
	assertDecodesEqual(t, "post-EOI garbage", out, base)
}

// TestSanitizeJPEG_TrailingFillCarriedOver pins the tail policy at the EOI
// boundary. Pure 0xFF padding after EOI is the JPEG fill convention and is
// carried over, so a padded clean file stays byte-identical through the
// strip -- a no-op the caller has nothing to write back. A tail that mixes
// fill with any other byte is appended data, and the whole tail goes: the
// output ends at the EOI marker itself.
func TestSanitizeJPEG_TrailingFillCarriedOver(t *testing.T) {
	base := testutil.JPEG(t, 48, 32)
	filled := append(append([]byte(nil), base...), 0xFF, 0xFF, 0xFF)
	out, err := sanitizeJPEG(filled)
	if err != nil {
		t.Fatalf("sanitizeJPEG with fill after EOI: %v", err)
	}
	if !bytes.Equal(out, filled) {
		t.Fatal("post-EOI fill bytes were stripped")
	}
	assertDecodesEqual(t, "post-EOI fill", out, base)

	mixed := append(append([]byte(nil), filled...), 0x00, 0x01)
	out, err = sanitizeJPEG(mixed)
	if err != nil {
		t.Fatalf("sanitizeJPEG with mixed tail: %v", err)
	}
	if !bytes.Equal(out, base) {
		t.Fatal("mixed fill-and-garbage tail was not dropped at the EOI boundary")
	}
	assertDecodesEqual(t, "mixed tail dropped", out, base)
}

// TestSanitizeJPEG_EntropyWalkSkipsStuffedAndRestartBytes pins the scan walk:
// the entropy-coded data between the SOS header and the EOI is byte-stuffed
// (0xFF 0x00), may hold restart markers (0xFFD0-0xFFD7) and ends with 0xFF
// fill before its terminating marker. The walk must treat all of those as
// scan content and end only at the real EOI, carrying the scan through
// verbatim -- byte for byte, with no refusal.
func TestSanitizeJPEG_EntropyWalkSkipsStuffedAndRestartBytes(t *testing.T) {
	base := testutil.JPEG(t, 48, 32)
	sos := bytes.Index(base, []byte{jpegMarkerPrefix, jpegSOS})
	if sos < 0 {
		t.Fatal("test jpeg has no SOS marker")
	}
	segEnd := sos + 2 + int(binary.BigEndian.Uint16(base[sos+2:sos+4]))
	crafted := append(append([]byte(nil), base[:segEnd]...),
		0x12, 0x34, 0xFF, 0x00, 0x56, 0xFF, 0xD1, 0x78, 0xFF, 0xFF, 0xFF, 0xD9)
	out, err := sanitizeJPEG(crafted)
	if err != nil {
		t.Fatalf("sanitizeJPEG over crafted scan data: %v", err)
	}
	if !bytes.Equal(out, crafted) {
		t.Fatal("scan walk did not carry the entropy data through verbatim")
	}
}

func TestSanitizePNG_CleanPassthrough(t *testing.T) {
	base := testutil.PNG(t, 32, 24)
	out, err := sanitizePNG(base)
	if err != nil {
		t.Fatalf("sanitizePNG: %v", err)
	}
	if !bytes.Equal(out, base) {
		t.Fatal("metadata-free png was rewritten")
	}
	assertDecodesEqual(t, "clean passthrough", out, base)
}

func TestSanitizePNG_StripsExifChunk(t *testing.T) {
	base := testutil.PNG(t, 32, 24)
	// The eXIf chunk carries a TIFF profile; the GPS-bearing bytes it holds
	// must vanish with the chunk.
	profile := []byte("II*\x00\x08\x00\x00\x00GPSLatitudeRef-travels-here")
	withExif := insertPNGChunk(t, base, string(pngChunkExif), profile)
	if !bytes.Contains(withExif, pngChunkExif) {
		t.Fatal("exif chunk not spliced into base")
	}
	out, err := sanitizePNG(withExif)
	if err != nil {
		t.Fatalf("sanitizePNG: %v", err)
	}
	if bytes.Contains(out, pngChunkExif) {
		t.Fatal("eXIf chunk survives the strip")
	}
	if bytes.Contains(out, []byte("GPSLatitudeRef")) {
		t.Fatal("gps profile bytes survive the strip")
	}
	assertDecodesEqual(t, "eXIf strip", out, base)
}

func TestSanitizePNG_ChunkCRCVerified(t *testing.T) {
	base := testutil.PNG(t, 32, 24)
	// Flip one byte inside the IDAT payload: the stored CRC no longer
	// matches, and the walker must refuse the file rather than pass an
	// unverifiable chunk through.
	idat := bytes.Index(base, []byte("IDAT"))
	corrupt := append([]byte(nil), base...)
	corrupt[idat+20] ^= 0x01
	if _, err := sanitizePNG(corrupt); err == nil {
		t.Fatal("sanitizePNG accepted a chunk whose CRC does not match")
	}
}

func TestSanitizePNG_StructureErrors(t *testing.T) {
	base := testutil.PNG(t, 32, 24)
	corrupt := func(t *testing.T, name string, raw []byte) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			if _, err := sanitizePNG(raw); err == nil {
				t.Fatalf("sanitizePNG accepted structurally broken input (%s)", name)
			}
		})
	}
	corrupt(t, "missing signature", []byte("not a png at all"))
	corrupt(t, "IEND removed", base[:len(base)-12])
	corrupt(t, "truncated chunk data", base[:len(base)-5])
	corrupt(t, "chunk length past end", func() []byte {
		overlong := append([]byte(nil), base[:8]...)
		overlong = append(overlong, 0x7F, 0xFF, 0xFF, 0xFF) // declared length dwarfs the file
		return append(overlong, base[8:]...)
	}())
}

func TestSanitizePNG_TrailingGarbageDiscarded(t *testing.T) {
	base := testutil.PNG(t, 32, 24)
	withGarbage := append(append([]byte(nil), base...), "garbage after end-of-image"...)
	out, err := sanitizePNG(withGarbage)
	if err != nil {
		t.Fatalf("sanitizePNG: %v", err)
	}
	if !bytes.Equal(out, base) {
		t.Fatal("trailing garbage was carried into the output")
	}
	assertDecodesEqual(t, "trailing garbage", out, base)
}

func TestSanitizeContent(t *testing.T) {
	baseJPEG := testutil.JPEG(t, 48, 32)
	basePNG := testutil.PNG(t, 32, 24)

	t.Run("jpeg without metadata unchanged", func(t *testing.T) {
		out, changed, err := sanitizeContent(baseJPEG, "image/jpeg")
		if err != nil || changed || !bytes.Equal(out, baseJPEG) {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
	})
	t.Run("jpeg exif stripped", func(t *testing.T) {
		withExif := insertAPPSegment(t, baseJPEG, jpegApp1, exifPayload())
		out, changed, err := sanitizeContent(withExif, "image/jpeg")
		if err != nil {
			t.Fatalf("sanitizeContent: %v", err)
		}
		if !changed {
			t.Fatal("exif-bearing jpeg reported unchanged")
		}
		if bytes.Contains(out, exifSignature) {
			t.Fatal("exif survives sanitizeContent")
		}
		assertDecodesEqual(t, "jpeg exif strip", out, baseJPEG)
	})
	t.Run("jpeg trailing exif stripped", func(t *testing.T) {
		withTail := appendTailSegment(t, baseJPEG, jpegApp1, exifPayload())
		out, changed, err := sanitizeContent(withTail, "image/jpeg")
		if err != nil {
			t.Fatalf("sanitizeContent: %v", err)
		}
		if !changed {
			t.Fatal("post-EOI exif-bearing jpeg reported unchanged")
		}
		if !bytes.Equal(out, baseJPEG) {
			t.Fatal("post-EOI metadata survives sanitizeContent")
		}
		assertDecodesEqual(t, "jpeg trailing exif strip", out, baseJPEG)
	})
	t.Run("png without metadata unchanged", func(t *testing.T) {
		out, changed, err := sanitizeContent(basePNG, "image/png")
		if err != nil || changed || !bytes.Equal(out, basePNG) {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
	})
	t.Run("png exif chunk stripped", func(t *testing.T) {
		withExif := insertPNGChunk(t, basePNG, string(pngChunkExif), []byte("II*\x00\x08\x00\x00\x00"))
		out, changed, err := sanitizeContent(withExif, "image/png")
		if err != nil {
			t.Fatalf("sanitizeContent: %v", err)
		}
		if !changed || bytes.Contains(out, pngChunkExif) {
			t.Fatalf("changed=%v, exif chunk present: %v", changed, bytes.Contains(out, pngChunkExif))
		}
		assertDecodesEqual(t, "png exif strip", out, basePNG)
	})
	t.Run("non-image media type passes through", func(t *testing.T) {
		opaque := []byte("an allowlisted pdf whose internals storage does not parse")
		for _, mime := range []string{"application/pdf", "image/gif", "application/octet-stream"} {
			out, changed, err := sanitizeContent(opaque, mime)
			if err != nil {
				t.Fatalf("sanitizeContent(%s): %v", mime, err)
			}
			if changed || !bytes.Equal(out, opaque) {
				t.Fatalf("%s: passthrough reported changed", mime)
			}
		}
	})
	t.Run("undecodable jpeg refused", func(t *testing.T) {
		if _, _, err := sanitizeContent([]byte("not a jpeg"), "image/jpeg"); err == nil {
			t.Fatal("sanitizeContent accepted non-jpeg bytes under an image/jpeg mime")
		}
	})
}
