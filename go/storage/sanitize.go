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
// precede the image's scan data (JPEG APP segments) or live in the file's
// chunk stream (PNG eXIf). Metadata that a hostile encoder smuggles into the
// entropy-coded data itself -- or after the first scan in a hierarchical
// JPEG -- is out of scope for a structural strip; the module's own tests
// construct the carriers this walker knows, and anything it cannot verify
// structurally is refused rather than passed through. Every error returned
// here is a plain error; the caller maps it onto ErrImageUnreadable so the
// refusal carries the code index's shape and the cause keeps the detail.
//
// Both walkers are strict about structure (bounds, lengths, CRCs, required
// terminators) and fail closed on anything they cannot account for: a file
// whose metadata could be stripped but whose structure cannot be verified is
// refused, never passed through on good faith.

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
// At SOS the scan data takes over and parsing stops: from that byte on the
// input is entropy-coded and byte-stuffed, so it cannot be walked safely and
// is carried over verbatim. This is what confines the strip to metadata that
// precedes the scan, the scope this file's header records.
func sanitizeJPEG(raw []byte) ([]byte, error) {
	if len(raw) < 2 || raw[0] != jpegMarkerPrefix || raw[1] != jpegSOI {
		return nil, errors.New("jpeg: missing SOI marker")
	}
	clean := make([]byte, 0, len(raw))
	clean = append(clean, jpegMarkerPrefix, jpegSOI)
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
			return nil, errors.New("jpeg: end-of-image marker before scan data")
		case code == jpegSOS:
			// Scan data is byte-stuffed and must not be parsed: carry the
			// SOS marker and everything after it over verbatim.
			clean = append(clean, raw[markerStart:]...)
			return clean, nil
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
	return nil, errors.New("jpeg: no scan data (missing SOS marker)")
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
