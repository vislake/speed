package storage

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/draw"
	"strings"
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

// xmpExtensionPayload returns the payload of one APP1 extended-XMP segment,
// laid out as the XMP specification's JPEG storage rules give it: the
// "http://ns.adobe.com/xmp/extension/\0" signature, then the pair's 128-bit
// GUID as 32 ASCII hex characters, then the full length of the serialized
// extended XMP and this portion's offset within it (each a big-endian
// uint32), then the portion's own bytes. The walker never looks deeper than
// the signature, so the header's load-bearing part is that the whole
// segment -- portion bytes included -- disappears.
func xmpExtensionPayload(guid string, fullLen, offset uint32, portion []byte) []byte {
	p := append([]byte(nil), "http://ns.adobe.com/xmp/extension/\x00"...)
	p = append(p, guid...)
	p = binary.BigEndian.AppendUint32(p, fullLen)
	p = binary.BigEndian.AppendUint32(p, offset)
	return append(p, portion...)
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

// TestSanitizeJPEG_StripsExtendedXMP pins the XMP vocabulary's overflow
// carrier. The XMP specification caps what a standard XMP packet may hold at
// what one APP1 segment carries, so a serialized package that outgrows the
// cap -- large edit histories or regional data genuinely do -- is split
// across APP1 segments under a second signature, each segment leading with
// the extension signature, the pair's GUID, the package's full length and
// the portion's offset, then the portion's bytes, and the main packet
// naming the GUID in its xmpNote:HasExtendedXMP property. The extension
// carrier is a standard carrier of the same vocabulary the main packet
// rides, so both must die: the fixture below is a real pair as a writer
// emits it -- a standard main packet followed by the extension segments in
// offset order -- whose GPS position and creator the overflow moved into
// the extension portions, so their survival in the served bytes, not merely
// "the strip ran", is what the assertions pin.
func TestSanitizeJPEG_StripsExtendedXMP(t *testing.T) {
	base := testutil.JPEG(t, 48, 32)
	// The pair's shared identifier: one 128-bit GUID as 32 ASCII hex
	// characters, matching between HasExtendedXMP and every extension
	// segment header.
	const guid = "0f6e8a2b3c4d5e6f708192a3b4c5d6e7"
	main := `<x:xmpmeta xmlns:x="adobe:ns:meta/">` +
		`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` +
		`<rdf:Description rdf:about="" xmlns:xmpNote="http://ns.adobe.com/xmp/note/" ` +
		`xmpNote:HasExtendedXMP="` + guid + `">` +
		`</rdf:Description></rdf:RDF></x:xmpmeta>`
	mainPayload := append(append([]byte(nil), xmpSignature...), main...)
	// The serialized extended XMP, built large on purpose: an APP1 payload
	// ceiling of 65,533 bytes minus the extension header (35-byte signature,
	// 32-byte GUID, two 4-byte lengths) leaves at most 65,458 bytes of
	// package per segment, and a package that fits one segment would never
	// be split -- the overflow is what makes the extension carrier real.
	// The package holds the GPS position and the creator, moved here out of
	// the main packet, beside an editing history (xmpMM) padded past the
	// cap.
	history := strings.Repeat(
		`<rdf:li rdf:parseType="Resource"><stEvt:action>edited</stEvt:action>`+
			`<stEvt:when>2026-09-08T09:00:00+08:00</stEvt:when>`+
			`<stEvt:softwareAgent>Adobe Photoshop Lightroom Classic</stEvt:softwareAgent>`+
			`<stEvt:changed>/metadata</stEvt:changed></rdf:li>`, 300)
	ext := `<x:xmpmeta xmlns:x="adobe:ns:meta/">` +
		`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` +
		`<rdf:Description rdf:about="" xmlns:exif="http://ns.adobe.com/exif/1.0/" ` +
		`xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:xmpMM="http://ns.adobe.com/xap/1.0/mm/" ` +
		`xmlns:stEvt="http://ns.adobe.com/xap/1.0/sType/ResourceEvent#" ` +
		`exif:GPSLatitude="31,2304N" exif:GPSLongitude="121,4737E">` +
		`<dc:creator><rdf:Seq><rdf:li>Jane Doe</rdf:li></rdf:Seq></dc:creator>` +
		`<xmpMM:History><rdf:Seq>` + history + `</rdf:Seq></xmpMM:History>` +
		`</rdf:Description></rdf:RDF></x:xmpmeta>`
	if len(ext) <= 65458 {
		t.Fatalf("fixture extended xmp does not overflow one APP1 segment: %d bytes", len(ext))
	}
	split := len(ext) / 2
	fullLen := uint32(len(ext))
	// insertAPPSegment lands each new segment directly after SOI, so the
	// calls below run in reverse and the file ends up in the standard's
	// order: the main packet first, then the extension segments by offset.
	withExt := insertAPPSegment(t, base, jpegApp1,
		xmpExtensionPayload(guid, fullLen, uint32(split), []byte(ext[split:])))
	withExt = insertAPPSegment(t, withExt, jpegApp1,
		xmpExtensionPayload(guid, fullLen, 0, []byte(ext[:split])))
	withPair := insertAPPSegment(t, withExt, jpegApp1, mainPayload)
	out, err := sanitizeJPEG(withPair)
	if err != nil {
		t.Fatalf("sanitizeJPEG: %v", err)
	}
	if bytes.Contains(out, xmpSignature) {
		t.Fatal("main xmp packet survives the strip")
	}
	if bytes.Contains(out, []byte("http://ns.adobe.com/xmp/extension/\x00")) {
		t.Fatal("extended-xmp segments survive the strip")
	}
	assertNoneContain(t, "extended xmp strip", out, "31,2304N", "121,4737E", "Jane Doe", guid)
	assertDecodesEqual(t, "extended xmp strip", out, base)
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

// assertNoneContain fails when out contains any of the marker-content
// needles, naming the first one found. It is the "no marker content
// survives into the served bytes" proof: a strip that merely reported
// success while the content rode through fails here, and a strip that
// removed the whole segment or chunk cannot fail it.
func assertNoneContain(t *testing.T, label string, out []byte, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if bytes.Contains(out, []byte(n)) {
			t.Fatalf("%s: marker content %q survives in the stripped bytes", label, n)
		}
	}
}

// iptcField appends one IPTC-IIM data set to dst in the wire form a
// Photoshop image-resource block carries: the 0x1C field tag, record
// number 2 (IPTC), the data-set number, a two-byte big-endian length and
// the value. An IPTC parser reads these fields; the walker's decision to
// drop the carrier never depends on its internal layout, so the load on
// this construction is that the content really is IPTC-IIM data.
func iptcField(dst []byte, dataset byte, value string) []byte {
	dst = append(dst, 0x1C, 0x02, dataset)
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(value)))
	return append(dst, value...)
}

// irbIPTCPayload returns the bytes of an APP13 payload that is a real
// Photoshop image-resource block (IRB): the "Photoshop 3.0" signature,
// the IRB length, and one 8BIM resource of id 0x0404 (the IPTC-NAA
// record) whose data is an IPTC-IIM stream holding By-line (2:25), City
// (2:90) and Copyright notice (2:116) data sets -- the authorship and
// location vocabulary whose carrier this payload is.
func irbIPTCPayload() []byte {
	p := []byte("Photoshop 3.0\x00")
	var res []byte
	res = append(res, "8BIM"...)
	res = binary.BigEndian.AppendUint16(res, 0x0404) // IPTC-NAA record
	res = append(res, 0x00, 0x00)                    // empty Pascal name, padded to even
	var iim []byte
	iim = iptcField(iim, 0x19, "Jane Doe")                    // 2:25 By-line
	iim = iptcField(iim, 0x5A, "Shanghai")                    // 2:90 City
	iim = iptcField(iim, 0x74, "Copyright (c) 2026 Jane Doe") // 2:116 Copyright notice
	res = binary.BigEndian.AppendUint32(res, uint32(len(iim)))
	res = append(res, iim...)
	if len(iim)%2 == 1 {
		res = append(res, 0x00) // resource data is padded to an even length
	}
	p = binary.BigEndian.AppendUint32(p, uint32(len(res)))
	return append(p, res...)
}

// itxtXMPData returns the data field of an iTXt chunk carrying an XMP
// packet under the keyword the PNG specification reserves for Adobe XMP,
// in the uncompressed form: keyword, NUL, compression flag 0, compression
// method 0, language tag, NUL, translated keyword, NUL, then the packet.
// The packet holds a GPS position (exif namespace) and an author (Dublin
// Core creator), so the content needles name both protected classes.
func itxtXMPData() []byte {
	d := []byte("XML:com.adobe.xmp\x00")
	d = append(d, 0x00, 0x00) // compression flag 0, compression method 0
	d = append(d, "x-default\x00"...)
	d = append(d, "\x00"...)
	xmp := `<x:xmpmeta xmlns:x="adobe:ns:meta/">` +
		`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` +
		`<rdf:Description rdf:about="" xmlns:exif="http://ns.adobe.com/exif/1.0/" ` +
		`xmlns:dc="http://purl.org/dc/elements/1.1/" exif:GPSLatitude="31,2304N" ` +
		`exif:GPSLongitude="121,4737E">` +
		`<dc:creator><rdf:Seq><rdf:li>Jane Doe</rdf:li></rdf:Seq></dc:creator>` +
		`</rdf:Description></rdf:RDF></x:xmpmeta>`
	return append(d, xmp...)
}

// ztxtCommentData returns the data field of a zTXt chunk: keyword, NUL,
// compression method 0 (zlib), and the compressed comment text, returned
// whole so the test can assert the compressed bytes' absence verbatim --
// content an assertion on the plain text could not see.
func ztxtCommentData() (compressed []byte) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write([]byte("GPS: 31.2304 N 121.4737 E; Studio: Jane Doe")); err != nil {
		panic(err) // writing to a bytes.Buffer cannot fail
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func TestSanitizeJPEG_StripsApp13IPTC(t *testing.T) {
	base := testutil.JPEG(t, 48, 32)
	// A real IRB-with-IPTC APP13 (0xED), spliced in where Photoshop would
	// have written it, plus a second APP13 whose payload is not an IRB --
	// the boundary pin: the covered carrier is the IRB, so a non-IRB APP13
	// rides through exactly as an APP1 that is neither EXIF nor XMP does.
	withIPTC := insertAPPSegment(t, base, 0xED, irbIPTCPayload())
	foreign := []byte("com.example.vendor-app13-not-an-irb")
	withBoth := insertAPPSegment(t, withIPTC, 0xED, foreign)
	out, err := sanitizeJPEG(withBoth)
	if err != nil {
		t.Fatalf("sanitizeJPEG: %v", err)
	}
	assertNoneContain(t, "app13 iptc strip", out, "Jane Doe", "Shanghai", "Copyright (c) 2026", "Photoshop 3.0")
	if !bytes.Contains(out, foreign) {
		t.Fatal("non-IRB APP13 payload was stripped")
	}
	assertDecodesEqual(t, "app13 ip13 strip", out, base)
	again, err := sanitizeJPEG(out)
	if err != nil {
		t.Fatalf("sanitizeJPEG (second pass): %v", err)
	}
	if !bytes.Equal(again, out) {
		t.Fatal("strip is not idempotent")
	}
}

func TestSanitizeJPEG_StripsCommentSegments(t *testing.T) {
	base := testutil.JPEG(t, 48, 32)
	// COM (0xFE) is the free-text comment marker: no signature can
	// classify its payload, so the whole marker is the covered carrier
	// and a comment that names a photographer and a GPS position must not
	// survive -- free text is exactly how either protected class can
	// arrive without any structured vocabulary.
	comment := "photographer Jane Doe; GPS 31.2304 N 121.4737 E"
	withComment := insertAPPSegment(t, base, 0xFE, []byte(comment))
	out, err := sanitizeJPEG(withComment)
	if err != nil {
		t.Fatalf("sanitizeJPEG: %v", err)
	}
	assertNoneContain(t, "comment strip", out, "photographer Jane Doe", "GPS 31.2304 N 121.4737 E")
	assertDecodesEqual(t, "comment strip", out, base)
	again, err := sanitizeJPEG(out)
	if err != nil {
		t.Fatalf("sanitizeJPEG (second pass): %v", err)
	}
	if !bytes.Equal(again, out) {
		t.Fatal("strip is not idempotent")
	}
}

func TestSanitizePNG_StripsITXtXMP(t *testing.T) {
	base := testutil.PNG(t, 32, 24)
	// iTXt under the XML:com.adobe.xmp keyword is the carrier the PNG
	// specification gives Adobe XMP -- the same geotag/authorship packet
	// APP1 carries on JPEG -- so the GPS position and creator inside it
	// must not survive into the served bytes.
	withXMP := insertPNGChunk(t, base, "iTXt", itxtXMPData())
	out, err := sanitizePNG(withXMP)
	if err != nil {
		t.Fatalf("sanitizePNG: %v", err)
	}
	assertNoneContain(t, "itxt xmp strip", out, "31,2304N", "121,4737E", "Jane Doe", "XML:com.adobe.xmp")
	assertDecodesEqual(t, "itxt xmp strip", out, base)
	again, err := sanitizePNG(out)
	if err != nil {
		t.Fatalf("sanitizePNG (second pass): %v", err)
	}
	if !bytes.Equal(again, out) {
		t.Fatal("strip is not idempotent")
	}
}

func TestSanitizePNG_StripsTextChunkFamily(t *testing.T) {
	base := testutil.PNG(t, 32, 24)
	// The rest of the text family: a tEXt pair under the Author and
	// Location keywords, and a zTXt whose comment text is zlib-compressed
	// (the content needle is the compressed bytes themselves). tIME is the
	// boundary pin -- an ancillary chunk outside the text family rides
	// through, like every chunk that is not a covered carrier.
	withText := insertPNGChunk(t, base, "tEXt", []byte("Author\x00Jane Doe"))
	withText = insertPNGChunk(t, withText, "tEXt", []byte("Location\x00Shanghai"))
	zblob := ztxtCommentData()
	withText = insertPNGChunk(t, withText, "zTXt", append([]byte("Comment\x00\x00"), zblob...))
	withText = insertPNGChunk(t, withText, "tIME", []byte{0x07, 0xE6, 0x09, 0x08, 0x12, 0x34, 0x56})
	out, err := sanitizePNG(withText)
	if err != nil {
		t.Fatalf("sanitizePNG: %v", err)
	}
	assertNoneContain(t, "text family strip", out, "Jane Doe", "Shanghai")
	if bytes.Contains(out, zblob) {
		t.Fatal("zTXt comment content survives in the stripped bytes")
	}
	for _, chunkType := range []string{"tEXt", "zTXt"} {
		if bytes.Contains(out, []byte(chunkType)) {
			t.Fatalf("text-family chunk type %q survives the strip", chunkType)
		}
	}
	if !bytes.Contains(out, []byte("tIME")) {
		t.Fatal("chunk outside the text family was stripped")
	}
	assertDecodesEqual(t, "text family strip", out, base)
	again, err := sanitizePNG(out)
	if err != nil {
		t.Fatalf("sanitizePNG (second pass): %v", err)
	}
	if !bytes.Equal(again, out) {
		t.Fatal("strip is not idempotent")
	}
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
	t.Run("jpeg app13 and comment stripped", func(t *testing.T) {
		withMeta := insertAPPSegment(t, baseJPEG, 0xED, irbIPTCPayload())
		withMeta = insertAPPSegment(t, withMeta, 0xFE, []byte("photographer Jane Doe"))
		out, changed, err := sanitizeContent(withMeta, "image/jpeg")
		if err != nil {
			t.Fatalf("sanitizeContent: %v", err)
		}
		if !changed {
			t.Fatal("ip13/comment-bearing jpeg reported unchanged")
		}
		assertNoneContain(t, "jpeg app13+comment", out, "Jane Doe", "Shanghai", "Photoshop 3.0", "photographer Jane Doe")
		assertDecodesEqual(t, "jpeg app13+comment strip", out, baseJPEG)
	})
	t.Run("png text chunks stripped", func(t *testing.T) {
		withXMP := insertPNGChunk(t, basePNG, "iTXt", itxtXMPData())
		out, changed, err := sanitizeContent(withXMP, "image/png")
		if err != nil {
			t.Fatalf("sanitizeContent: %v", err)
		}
		if !changed {
			t.Fatal("itxt-bearing png reported unchanged")
		}
		assertNoneContain(t, "png itxt xmp", out, "31,2304N", "Jane Doe", "XML:com.adobe.xmp")
		assertDecodesEqual(t, "png itxt xmp strip", out, basePNG)
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
