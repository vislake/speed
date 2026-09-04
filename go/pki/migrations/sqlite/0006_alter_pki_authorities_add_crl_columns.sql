-- Round 3's CRL generation (docs/internal/22-pki.md's "revocation" section:
-- "generate a CRL, not an OCSP responder") needs five columns
-- 0002_create_pki_authorities.sql did not anticipate -- see
-- go/pki/model.go's Authority.CRLDistributionPoint/CRLNumber/CRLPEM/
-- CRLIssuedAt/CRLNextUpdate doc comments for the full rationale of each:
--
--   * crl_distribution_point: the URL this authority's own CRL is served
--     at, set once at CA-creation time and embedded into every certificate
--     this authority signs (empty means the extension is omitted, never a
--     broken placeholder, matching every other unset-value convention this
--     module already follows).
--   * crl_number: the RFC 5280 §5.2.3 CRL sequence number, monotonically
--     increasing on every CAService.GenerateCRL call for this authority.
--   * crl_pem / crl_issued_at / crl_next_update: the most recently
--     generated CRL itself, stored so a fetch serves a stable, cached
--     document rather than recomputing one on every request.
--
-- This is the SQLite copy; see the postgres/ sibling for the identical
-- schema on that dialect.
ALTER TABLE pki_authorities ADD COLUMN crl_distribution_point VARCHAR(500) NOT NULL DEFAULT '';
ALTER TABLE pki_authorities ADD COLUMN crl_number BIGINT NOT NULL DEFAULT 0;
ALTER TABLE pki_authorities ADD COLUMN crl_pem TEXT;
ALTER TABLE pki_authorities ADD COLUMN crl_issued_at TIMESTAMP;
ALTER TABLE pki_authorities ADD COLUMN crl_next_update TIMESTAMP;
