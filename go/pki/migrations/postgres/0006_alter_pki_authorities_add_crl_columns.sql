-- CRL generation (the module's revocation design: generate a CRL, not an
-- OCSP responder) needs five further columns on pki_authorities -- see
-- go/pki/model.go's Authority.CRLDistributionPoint/CRLNumber/CRLPEM/
-- CRLIssuedAt/CRLNextUpdate doc comments for the full rationale of each.
--
-- This is the PostgreSQL copy; see the sqlite/ sibling for the identical
-- schema on that dialect.
ALTER TABLE pki_authorities ADD COLUMN crl_distribution_point VARCHAR(500) NOT NULL DEFAULT '';
ALTER TABLE pki_authorities ADD COLUMN crl_number BIGINT NOT NULL DEFAULT 0;
ALTER TABLE pki_authorities ADD COLUMN crl_pem TEXT;
ALTER TABLE pki_authorities ADD COLUMN crl_issued_at TIMESTAMP;
ALTER TABLE pki_authorities ADD COLUMN crl_next_update TIMESTAMP;
