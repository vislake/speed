package attestation

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// attestedMessage is the structured message an attestation signs: the
// three facts the gate later checks -- which object is vouched for, what
// its content digest was at attestation time, and which tenant the output
// belongs to. Field order is fixed by the struct declaration, so the
// canonical JSON produced by newMessage is byte-stable: the same object,
// digest and tenant always marshal to the same bytes, and the gate
// verifies the signature over exactly the stored bytes before it parses
// anything.
type attestedMessage struct {
	// ObjectID is the go/storage object id of the attested output.
	ObjectID string `json:"object_id"`
	// ContentSHA256 is the hex-encoded SHA-256 of the output's bytes at
	// attestation time.
	ContentSHA256 string `json:"content_sha256"`
	// TenantID is the tenant the output belongs to, as a plain string --
	// the wire-shaped spelling of the pkgcore tenant, mirroring
	// smilesim.SimulationCompletedPayload's identical field.
	TenantID string `json:"tenant_id"`
}

// newMessage builds the canonical attestedMessage bytes for content of
// objectID under tenantID: the SHA-256 digest of content, hex-encoded,
// sealed into the fixed-field JSON shape the signature is made over.
func newMessage(objectID string, tenantID string, content []byte) ([]byte, error) {
	digest := sha256.Sum256(content)
	return json.Marshal(attestedMessage{
		ObjectID:      objectID,
		ContentSHA256: hex.EncodeToString(digest[:]),
		TenantID:      tenantID,
	})
}

// parseMessage decodes canonical message bytes back into the attested
// facts. A message that does not decode is not a message this package
// ever wrote; callers treat the parse failure as a verification failure,
// never as a partial answer.
func parseMessage(message []byte) (attestedMessage, error) {
	var parsed attestedMessage
	if err := json.Unmarshal(message, &parsed); err != nil {
		return attestedMessage{}, fmt.Errorf("attestation: parse attested message: %w", err)
	}
	return parsed, nil
}

// ErrAttestationFailed is the one outward error this package's
// verification surfaces, whatever stage refused: the certificate failed
// chain verification (a revoked certificate or chain member, an expired
// one), the signature over the stored message did not verify with the
// leaf's public key, the message named a different object or tenant than
// the one being served, or the live content digest differed from the
// attested one. It is deliberately a single shape -- the cmd/server
// sharing gate (sharing_resolver.go) hands it back as the resolver error
// the sharing module answers with sharing.resource_unavailable, and
// nothing about WHY an output stopped being vouchable is something a
// visitor holding a link should be able to probe. The stage that refused
// is carried in the wrapped error a caller logs, never on the wire.
var ErrAttestationFailed = errors.New("attestation: output does not verify")

// verifyContent checks one attested output's content at serve time. leaf
// is the parsed end-entity certificate CAService.VerifyCertificate
// already vouched for; message and signature are the row's stored bytes;
// content is the live bytes about to be served for objectID under
// tenantID. It returns ErrAttestationFailed -- wrapped with the refusing
// stage, for the log -- when any of the following fails:
//
//   - the Ed25519 signature does not verify over the STORED message bytes
//     with the leaf certificate's public key;
//   - the message does not parse, or names a different object id or
//     tenant than the ones being served;
//   - the SHA-256 of the live content differs from the digest the message
//     attests.
func verifyContent(leaf *x509.Certificate, message []byte, signature []byte, objectID string, tenantID string, content []byte) error {
	pub, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("%w: leaf certificate key is %T, not ed25519", ErrAttestationFailed, leaf.PublicKey)
	}
	if !ed25519.Verify(pub, message, signature) {
		return fmt.Errorf("%w: signature over the stored message does not verify with the leaf certificate's public key", ErrAttestationFailed)
	}
	parsed, err := parseMessage(message)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrAttestationFailed, err)
	}
	if parsed.ObjectID != objectID {
		return fmt.Errorf("%w: message names object %q, not the %q being served", ErrAttestationFailed, parsed.ObjectID, objectID)
	}
	if parsed.TenantID != tenantID {
		return fmt.Errorf("%w: message names tenant %q, not the %q being served", ErrAttestationFailed, parsed.TenantID, tenantID)
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != parsed.ContentSHA256 {
		return fmt.Errorf("%w: live content digest differs from the attested digest", ErrAttestationFailed)
	}
	return nil
}
