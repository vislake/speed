package storage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
)

// This file strips metadata from image bytes before they are stored, per the
// B2 revalidation policy: location and authorship metadata that arrived with
// an upload must not survive into the object the platform serves to other
// people. Stripping is structural, not re-encoding: the decodable pixel data
// passes through untouched, so the sanitized output decodes to exactly the
// image the uploader sent -- only the metadata containers are gone.
//
// Scope, deliberately: the walkers below cover the metadata carriers that
// ride in the file's marker stream -- JPEG APP segments wherever they appear,
// before the first scan, between the scans of a progressive or hierarchical
// JPEG, or appended after the image's EOI marker -- and the PNG chunk stream
// (eXIf). Metadata that a hostile encoder smuggles into the entropy-coded
// data itself is out of scope for a structural strip: the JPEG walker walks
// the entropy data only to locate where the scan ends, never to parse it. A
// file the walker cannot verify structurally is refused rather than passed
// through. Every error returned here is a plain error; the caller maps it
// onto ErrImageUnreadable so the refusal carries the code index's shape and
// the cause keeps the detail.
//
// Both walkers are strict about structure (bounds, lengths, CRCs, required
// terminators) and fail closed on anything they cannot account for: a file
// whose metadata could be stripped but whose structure cannot be verified is
// refused, never passed through on good faith. Bytes that sit outside the
// verified structure are the one thing they drop rather than refuse -- the
// PNG walker ends its output at IEND, the JPEG walker at EOI.

var (
	// exifSignature prefixes the payload of the APP1 segment that carries
	// EXIF -- the container that holds GPS coordinates and camera
	// authorship. The signature is "Exif" followed by two zero bytes and a
	// TIFF header, per the EXIF-in-JPEG convention.
	exifSignature = []byte("Exif\x00\x00")
	// xmpSignature prefixes the APP1 payload of Adobe's XMP packet, the
	// sibling metadata carrier (camera serials, geotags, editing history)
	// that shares the APP1 marker with EXIF.
	xmpSignature = []byte("http://ns.adobe.com/xap/1.0/\x00")
)

const (
	// jpegMarkerPrefix is the 0xFF byte every JPEG marker starts with.
	jpegMarkerPrefix = 0xFF
	// jpegSOI marks the start of image.
	jpegSOI = 0xD8
	// jpegEOI marks the end of image.
	jpegEOI = 0xD9
	// jpegSOS starts the entropy-coded scan data.
	jpegSOS = 0xDA
	// jpegApp1 is the APP1 marker, the segment EXIF and XMP ride in.
	jpegApp1 = 0xE1
	// jpegTEM is the standalone "temporary private use" marker.
	jpegTEM = 0x01
)

// sanitizeJPEG returns a copy of raw with its EXIF and XMP APP1 segments
// removed, or an error when the JPEG structure cannot be verified. Only
// APP1 is dropped -- APP2's ICC profile is kept, as are APP0's JFIF header,
// quantisation and Huffman tables, and everything else a decoder needs.
//
// The walk is marker-based: SOI, then one marker per step, each either
// standalone (TEM, the restart markers), length-carrying (everything a
// decoder needs tables or metadata from), or SOS. Structure the walker
// cannot verify fails closed: fill runs that run off the end, a reserved
// marker code, a segment length past the end of the data, an EOI or a
// duplicate SOI before any scan data, and -- after a complete walk -- the
// absence of an SOS at all.
//
// A SOS is dispatched as the length-carrying marker it is, and the walk
// continues past the scan it starts: the entropy-coded data in between is
// walked only to find where the scan ends -- byte-stuffed 0xFF 0x00 pairs,
// restart markers (0xFFD0-0xFFD7) and 0xFF fill are all scan content -- and
// carried over verbatim. Whatever marker terminates the scan is then
// dispatched like any other: in a progressive or hierarchical JPEG that
// marker may begin another segment or scan, so only the file's final EOI
// ends the walk. An EOI is the required terminator of the last scan: a file
// whose entropy data runs off the end without one, or whose tail after the
// last segment never reaches one, is refused -- a file the walker cannot
// verify structurally is never passed through. Bytes after the EOI sit
// outside the image's structure: pure 0xFF fill is conventionally legal
// padding and is carried over so a padded clean file stays byte-identical,
// while a tail containing any other byte is appended data and is dropped at
// the EOI boundary, mirroring the PNG walker's treatment of chunks after
// IEND.
func sanitizeJPEG(raw []byte) ([]byte, error) {
	if len(raw) < 2 || raw[0] != jpegMarkerPrefix || raw[1] != jpegSOI {
		return nil, errors.New("jpeg: missing SOI marker")
	}
	clean := make([]byte, 0, len(raw))
	clean = append(clean, jpegMarkerPrefix, jpegSOI)
	scanSeen := false
	for i := 2; i < len(raw); {
		// A marker boundary: the code is preceded by its 0xFF and any fill
		// bytes (repeated 0xFF) a producer may have padded between markers.
		markerStart := i
		if raw[i] != jpegMarkerPrefix {
			return nil, fmt.Errorf("jpeg: expected marker at offset %d, found 0x%02x", i, raw[i])
		}
		for i < len(raw) && raw[i] == jpegMarkerPrefix {
			i++
		}
		if i >= len(raw) {
			return nil, errors.New("jpeg: truncated marker")
		}
		code := raw[i]
		i++
		switch {
		case code == jpegTEM || (code >= 0xD0 && code <= 0xD7):
			// Standalone markers: TEM and RST0-RST7 carry no payload.
			clean = append(clean, jpegMarkerPrefix, code)
		case code == jpegSOI:
			return nil, errors.New("jpeg: duplicate SOI marker")
		case code == jpegEOI:
			if !scanSeen {
				return nil, errors.New("jpeg: end-of-image marker before scan data")
			}
			clean = append(clean, jpegMarkerPrefix, jpegEOI)
			// The EOI ends the image; what follows is outside its
			// structure. A tail of pure 0xFF fill is conventionally legal
			// padding and is kept, so a padded clean file passes through
			// byte-identical; any other tail byte marks appended data, and
			// the whole tail is dropped rather than carried over.
			fill := true
			for _, b := range raw[i:] {
				if b != jpegMarkerPrefix {
					fill = false
					break
				}
			}
			if fill {
				clean = append(clean, raw[i:]...)
			}
			return clean, nil
		case code == jpegSOS:
			// SOS is a length-carrying marker whose payload lists the scan's
			// components; the entropy-coded data follows it.
			if i+1 >= len(raw) {
				return nil, errors.New("jpeg: truncated segment length")
			}
			segLen := int(raw[i])<<8 | int(raw[i+1])
			if segLen < 2 {
				return nil, errors.New("jpeg: segment length below 2")
			}
			segEnd := i + segLen
			if segEnd > len(raw) {
				return nil, errors.New("jpeg: segment extends past end of data")
			}
			clean = append(clean, raw[markerStart:segEnd]...)
			scanSeen = true
			scanEnd, err := jpegScanDataEnd(raw, segEnd)
			if err != nil {
				return nil, err
			}
			// The entropy data itself is carried over untouched; only its
			// extent was walked. scanEnd points at the 0xFF of the marker
			// that terminates the scan, which the loop's next iteration
			// dispatches -- EOI for a finished image, or another segment or
			// scan marker when this file carries more scans.
			clean = append(clean, raw[segEnd:scanEnd]...)
			i = scanEnd
		case code < 0xC0:
			return nil, fmt.Errorf("jpeg: reserved marker 0x%02x at offset %d", code, markerStart)
		default:
			// A length-carrying segment: two big-endian bytes that count
			// themselves, so the segment spans [markerStart, markerStart+2+len).
			if i+1 >= len(raw) {
				return nil, errors.New("jpeg: truncated segment length")
			}
			segLen := int(raw[i])<<8 | int(raw[i+1])
			if segLen < 2 {
				return nil, errors.New("jpeg: segment length below 2")
			}
			segEnd := i + segLen
			if segEnd > len(raw) {
				return nil, errors.New("jpeg: segment extends past end of data")
			}
			payload := raw[i+2 : segEnd]
			if code == jpegApp1 && (bytes.HasPrefix(payload, exifSignature) || bytes.HasPrefix(payload, xmpSignature)) {
				// Dropped: this APP1 is EXIF or XMP, the carriers this
				// strip exists for.
			} else {
				clean = append(clean, raw[markerStart:segEnd]...)
			}
			i = segEnd
		}
	}
	if scanSeen {
		return nil, errors.New("jpeg: scan data without end-of-image marker")
	}
	return nil, errors.New("jpeg: no scan data (missing SOS marker)")
}

// jpegScanDataEnd walks the entropy-coded data a SOS header left at raw[start:]
// and returns the offset of the marker that terminates the scan -- the first
// byte of its 0xFF. The walk is deliberately shallow: everything it steps
// over is scan content whose bytes pass through verbatim, so the walk only
// needs to tell scan content from a marker boundary. 0xFF followed by 0x00
// is a byte-stuffed 0xFF data byte, 0xFFD0-0xFFD7 are restart markers that
// may legally interrupt the scan between MCUs, and a run of 0xFF before a
// marker is fill; any other 0xFF marks the scan's end. An entropy run that
// reaches the end of the data without a terminating marker is a truncated
// scan and is refused.
func jpegScanDataEnd(raw []byte, start int) (int, error) {
	for i := start; i < len(raw); {
		if raw[i] != jpegMarkerPrefix {
			i++
			continue
		}
		if i+1 >= len(raw) {
			return 0, errors.New("jpeg: scan data without end-of-image marker")
		}
		switch nxt := raw[i+1]; {
		case nxt == 0x00:
			i += 2 // a byte-stuffed 0xFF data byte
		case nxt >= 0xD0 && nxt <= 0xD7:
			i += 2 // a restart marker between MCUs
		case nxt == jpegMarkerPrefix:
			i++ // 0xFF fill before the terminating marker
		default:
			return i, nil // the terminating marker begins at raw[i]
		}
	}
	return 0, errors.New("jpeg: scan data without end-of-image marker")
}

// pngSignature is the eight-byte file signature every PNG starts with.
var pngSignature = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}

// pngChunkExif is the chunk type of the EXIF chunk the walker drops.
var pngChunkExif = []byte("eXIf")

// pngChunkEnd is the chunk type that terminates a PNG's chunk stream. It is
// the walker's required terminator: a PNG without IEND is truncated, however
// plausible its pixel chunks looked.
var pngChunkEnd = []byte("IEND")

// sanitizePNG returns a copy of raw with its eXIf chunk removed, or an error
// when the PNG structure cannot be verified. Every chunk is walked and its
// CRC-32 checked against the stored value -- the checksum is what makes the
// walk authoritative: a chunk whose bytes cannot be verified is refused
// rather than passed through. Chunks after IEND are outside the file's
// structure and are not carried over; output is a valid PNG regardless of
// what the upload appended.
func sanitizePNG(raw []byte) ([]byte, error) {
	if len(raw) < len(pngSignature) || !bytes.Equal(raw[:len(pngSignature)], pngSignature) {
		return nil, errors.New("png: missing signature")
	}
	clean := make([]byte, 0, len(raw))
	clean = append(clean, pngSignature...)
	for i := len(pngSignature); i < len(raw); {
		// The chunk header: four bytes of length, four bytes of type, then
		// the data and its four-byte CRC-32 (IEEE, computed over type and
		// data).
		if i+8 > len(raw) {
			return nil, errors.New("png: truncated chunk header")
		}
		chunkLen := int(binary.BigEndian.Uint32(raw[i : i+4]))
		chunkType := raw[i+4 : i+8]
		dataEnd := i + 8 + chunkLen
		if dataEnd+4 > len(raw) {
			return nil, errors.New("png: truncated chunk data")
		}
		stored := binary.BigEndian.Uint32(raw[dataEnd : dataEnd+4])
		if computed := crc32.ChecksumIEEE(raw[i+4 : dataEnd]); stored != computed {
			return nil, fmt.Errorf("png: crc mismatch in %q chunk", chunkType)
		}
		switch {
		case bytes.Equal(chunkType, pngChunkExif):
			// Dropped: the EXIF chunk, the carrier this strip exists for.
		case bytes.Equal(chunkType, pngChunkEnd):
			clean = append(clean, raw[i:dataEnd+4]...)
			return clean, nil
		default:
			clean = append(clean, raw[i:dataEnd+4]...)
		}
		i = dataEnd + 4
	}
	return nil, errors.New("png: missing IEND chunk")
}

// sanitizeContent strips metadata from bytes the pipeline already probed as
// mime, returning the sanitized bytes and whether they changed. The media
// type dispatch is exact: image/jpeg and image/png have a structural strip;
// every other allowlisted type has no metadata carrier this module knows how
// to strip, so it passes through unchanged with changed=false -- the
// pipeline's choice to store the bytes or not belongs to its caller, this
// function only reports.
func sanitizeContent(raw []byte, mime string) ([]byte, bool, error) {
	switch mime {
	case "image/jpeg":
		out, err := sanitizeJPEG(raw)
		if err != nil {
			return nil, false, err
		}
		return out, !bytes.Equal(out, raw), nil
	case "image/png":
		out, err := sanitizePNG(raw)
		if err != nil {
			return nil, false, err
		}
		return out, !bytes.Equal(out, raw), nil
	default:
		return raw, false, nil
	}
}
