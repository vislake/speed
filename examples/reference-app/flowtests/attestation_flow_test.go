// attestation_flow_test.go drives the X.509 layer's consumer acceptance
// journey at the wire level through the real composed HTTP stack -- the
// same stack and canned provider smile_journey_flow_test.go uses, with the
// object store pinned to a known directory so the test can reach the
// stored bytes behind a go/storage object (the tamper leg needs to
// corrupt an output's content out from under the platform).
//
// The journey is the reference app's real use of go/pki's X.509 layer
// (internal/attestation, closed through the sharing resolver's
// attestation gate):
//
//  1. a clinic user runs a smile simulation to its succeeded status -- the
//     poll that observes the output registers and attests it
//     (CAService.IssueCertificate + CAService.SignCertificate under a
//     per-tenant "simulation.attestation" certificate);
//  2. the practice mints a share for the AI output, and an unauthenticated
//     visitor receives its real bytes -- the gate (CAService
//     .VerifyCertificate + signature + live-digest) passed;
//  3. an uploaded patient photo shares the same way, before and after the
//     legs below -- the gate only ever narrows, the blast radius is
//     exactly the AI outputs;
//  4. the output's stored bytes are tampered with out from under the
//     platform: the same visitor is refused (digest mismatch);
//  5. bytes restored, the tenant's attestation certificate is revoked over
//     pki's real HTTP operation (pki_revokeCertificate, the one wire
//     caller of it): the same visitor is refused again (chain
//     verification), while the photo share still serves;
//  6. the clinic re-opens the output (the poll of the same succeeded job):
//     the attestation is re-issued under a fresh certificate and the
//     visitor is served again;
//  7. the external-verifier leg: the platform regenerates the issuing
//     authority's CRL through the CA service's Go API, the test fetches
//     the document over pki's real HTTP CRL operation (pki_getAuthorityCrl,
//     its one wire caller) as an external verifier would, and
//     confirms with the standard library that the revoked certificate's
//     serial is listed, the replacement's is not, and the document carries
//     the authority's signature.
package flowtests

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vislake/speed/examples/reference-app/internal/app"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/sharing"
)

// TestX509Attestation_SimulationOutputSharedThroughTheChainVerifiedGate is
// the whole journey in one pass -- the seven legs of this file's header.
func TestX509Attestation_SimulationOutputSharedThroughTheChainVerifiedGate(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	srv, cfg := buildAttestationTestServer(t, imgServer)
	token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "x509-attestation")

	// A patient photo and a completed simulation over it, exactly as the
	// smile-journey flow test makes them.
	jpeg := jpegWithExif(t)
	photo := uploadPhotoAs(t, srv, token, base64.StdEncoding.EncodeToString(jpeg))
	createCaseAs(t, srv, token, "", caseCreateBody{
		PatientName:    "X.509 journey patient",
		PatientRef:     "X509-001",
		PhotoObjectIDs: []string{photo.ObjectID},
	})

	simulateBody, err := json.Marshal(map[string]any{"photo_object_id": photo.ObjectID})
	if err != nil {
		t.Fatalf("marshal simulate body: %v", err)
	}
	resp := smileSimRequest(t, srv, http.MethodPost, smileSimulatePath, token, simulateBody)
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("POST %s status = %d, want 202; body = %s", smileSimulatePath, resp.StatusCode, body)
	}
	var jobRef struct {
		JobID string `json:"job_id"`
	}
	if decodeErr := json.NewDecoder(resp.Body).Decode(&jobRef); decodeErr != nil {
		t.Fatalf("decode 202 answer: %v", decodeErr)
	}
	resp.Body.Close()
	status := waitForSmileSimSucceeded(t, srv, token, jobRef.JobID, time.Now().Add(30*time.Second))
	outputObjectID, _ := status["output_object_id"].(string)
	if outputObjectID == "" {
		t.Fatalf("succeeded job carried no output_object_id; status = %+v", status)
	}

	// The observer: a second connection to the same SQLite file the
	// running server writes, read through pki's own repositories and this
	// app's attestation table -- the same reach periodic_pki_scan_flow_test.go
	// and pki_revoke_gate_flow_test.go use.
	observerDB := openObserverDB(t, cfg)
	attestationRow := readAttestation(t, observerDB, outputObjectID)
	if attestationRow == "" {
		t.Fatalf("no attestation row for output %s after the succeeded poll -- the observation hook did not attest it", outputObjectID)
	}
	leafCert := certificateRow(t, observerDB, attestationRow, "tenant-acme")
	if leafCert.Status != pki.CertificateStatusActive {
		t.Fatalf("attestation certificate %s status = %q, want active", attestationRow, leafCert.Status)
	}
	issuingAuthority := authorityRow(t, observerDB, leafCert.AuthorityID)
	if issuingAuthority == nil {
		t.Fatalf("issuing authority %s not found", leafCert.AuthorityID)
	}

	// The shares: one for the AI output, one for the original patient
	// photo -- the photo is the control that must keep serving whatever
	// happens to the output below.
	mint := func(resourceRef string) testCreateShareResponse {
		body, marshalErr := json.Marshal(map[string]any{"resourceRef": resourceRef})
		if marshalErr != nil {
			t.Fatalf("marshal create body: %v", marshalErr)
		}
		createResp := storageRequest(t, srv, http.MethodPost, sharing.PathShares,
			token, app.DemoOwnerUserID, "application/json", strings.NewReader(string(body)))
		var share testCreateShareResponse
		decodeSharingBody(t, createResp, http.StatusCreated, "create share for "+resourceRef, &share)
		return share
	}
	outputShare := mint(outputObjectID)
	photoShare := mint(photo.ObjectID)

	readAccess := func(shareToken string) (*http.Response, string) {
		access := sharingAccessRequest(t, srv, shareToken, "")
		raw, readErr := io.ReadAll(access.Body)
		access.Body.Close()
		if readErr != nil {
			t.Fatalf("read access response: %v", readErr)
		}
		return access, string(raw)
	}

	// Leg 2: the anonymous visitor receives the attested output's real
	// bytes -- the gate (chain verification + signature + live digest)
	// passed.
	accessResp, rawOutput := readAccess(outputShare.Token)
	if accessResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s for the attested output: status = %d, want 200; body = %s",
			sharing.PathAccess, accessResp.StatusCode, rawOutput)
	}
	if rawOutput != string(imgServer.generatedPNG) {
		t.Fatalf("shared attested output differs from the vendor's generated image")
	}

	// Leg 3: the un-attested patient photo serves through the same gate.
	accessResp, rawPhoto := readAccess(photoShare.Token)
	if accessResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s for the patient photo: status = %d, want 200; body = %s",
			sharing.PathAccess, accessResp.StatusCode, rawPhoto)
	}
	if rawPhoto != string(mustReadStoredObject(t, cfg, "tenant-acme", photo.ObjectID)) {
		t.Fatalf("shared photo differs from the stored object")
	}

	// Leg 4: tamper with the output's stored bytes out from under the
	// platform. The stored file's digest no longer matches the attested
	// digest, so the very same visitor is refused with the sharing
	// module's one honest resource fault shape (502 resource_unavailable)
	// -- never bytes of altered content.
	tampered := []byte(rawOutput)
	tampered[0] ^= 0xff
	writeStoredObject(t, cfg, "tenant-acme", outputObjectID, tampered)
	refused := sharingAccessRequest(t, srv, outputShare.Token, "")
	refusedBody, _ := io.ReadAll(refused.Body)
	refused.Body.Close()
	if refused.StatusCode != http.StatusBadGateway {
		t.Fatalf("GET %s with tampered output bytes: status = %d, want 502; body = %s",
			sharing.PathAccess, refused.StatusCode, refusedBody)
	}
	assertRefusalCode(t, string(refusedBody), "sharing.resource_unavailable")

	// The photo still serves -- the gate's blast radius is exactly the
	// attested outputs.
	accessResp, _ = readAccess(photoShare.Token)
	if accessResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s for the patient photo after the tamper: status = %d, want 200",
			sharing.PathAccess, accessResp.StatusCode)
	}

	// Leg 5: restore the bytes, then revoke the tenant's attestation
	// certificate over pki's real HTTP surface -- the pki_revokeCertificate
	// operation, whose only wire caller this test is. The revocation's
	// very next gate check refuses: chain verification now answers
	// pki.certificate_revoked, so the old signature vouches for nothing.
	writeStoredObject(t, cfg, "tenant-acme", outputObjectID, imgServer.generatedPNG)
	revokeResp := storageRequest(t, srv, http.MethodPost,
		app.PkiRoutePath+"/certificates/"+attestationRow+"/revoke",
		token, app.DemoOwnerUserID, "application/json",
		strings.NewReader(`{"reason":"simulation attestation key suspected compromised"}`))
	revokeBody, _ := io.ReadAll(revokeResp.Body)
	revokeResp.Body.Close()
	if revokeResp.StatusCode != http.StatusOK {
		t.Fatalf("revoke attestation certificate over HTTP: status = %d, want 200; body = %s",
			revokeResp.StatusCode, revokeBody)
	}
	revokedRow := certificateRow(t, observerDB, attestationRow, "tenant-acme")
	if revokedRow.Status != pki.CertificateStatusRevoked {
		t.Fatalf("attestation certificate status after the HTTP revoke = %q, want revoked", revokedRow.Status)
	}

	refused = sharingAccessRequest(t, srv, outputShare.Token, "")
	refusedBody, _ = io.ReadAll(refused.Body)
	refused.Body.Close()
	if refused.StatusCode != http.StatusBadGateway {
		t.Fatalf("GET %s with a revoked attestation certificate: status = %d, want 502; body = %s",
			sharing.PathAccess, refused.StatusCode, refusedBody)
	}
	assertRefusalCode(t, string(refusedBody), "sharing.resource_unavailable")

	// Leg 6: the clinic re-opens the output -- the very same poll route
	// that first observed it succeeds. The observation finds the row under
	// a revoked certificate, issues a fresh certificate, re-signs the
	// output and replaces the row: the visitor is served again.
	waitForSmileSimSucceeded(t, srv, token, jobRef.JobID, time.Now().Add(30*time.Second))
	replaced := readAttestation(t, observerDB, outputObjectID)
	if replaced == attestationRow {
		t.Fatalf("re-observation did not replace the revoked attestation certificate (still %s)", attestationRow)
	}
	newLeaf := certificateRow(t, observerDB, replaced, "tenant-acme")
	if newLeaf.Status != pki.CertificateStatusActive {
		t.Fatalf("replacement attestation certificate %s status = %q, want active", replaced, newLeaf.Status)
	}
	accessResp, rawOutput = readAccess(outputShare.Token)
	if accessResp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s after re-attestation: status = %d, want 200; body = %s",
			sharing.PathAccess, accessResp.StatusCode, rawOutput)
	}
	if rawOutput != string(imgServer.generatedPNG) {
		t.Fatalf("re-attested share serves altered bytes")
	}

	// Leg 7: the external-verifier leg. The platform regenerates the
	// issuing authority's CRL (CAService.GenerateCRL over the same
	// database -- the module declares CRL regeneration on the
	// pkgcore.Registry.Schedules seat, so the app's host, which runs a
	// jobs.Scheduler over the finished registry's declarations,
	// schedules it by declaration; this leg's explicit call pins the
	// document it reads), and the test fetches the document over pki's
	// real HTTP operation pki_getAuthorityCrl as an external verifier
	// would. The revoked certificate's serial is listed; the
	// replacement's is not; the document's signature checks out against
	// the issuing authority's certificate with the standard library.
	crlCA := pki.NewCAService(
		pki.NewLocalSigner(observerDB), "local",
		pki.NewAuthorityRepository(observerDB),
		pki.NewCertificateRepository(observerDB),
		pki.NewCertificateRevocationRepository(observerDB),
	)
	if _, crlErr := crlCA.GenerateCRL(context.Background(), issuingAuthority.ID, 0); crlErr != nil {
		t.Fatalf("GenerateCRL: %v", crlErr)
	}
	crlResp := storageRequest(t, srv, http.MethodGet,
		app.PkiRoutePath+"/authorities/"+issuingAuthority.ID+"/crl",
		token, app.DemoOwnerUserID, "", nil)
	crlBody, _ := io.ReadAll(crlResp.Body)
	crlResp.Body.Close()
	if crlResp.StatusCode != http.StatusOK {
		t.Fatalf("GET pki authority CRL: status = %d, want 200; body = %s", crlResp.StatusCode, crlBody)
	}

	block, _ := pem.Decode(crlBody)
	if block == nil || block.Type != "X509 CRL" {
		t.Fatalf("CRL answer is not a PEM X509 CRL: %q", crlBody)
	}
	crl, err := x509.ParseRevocationList(block.Bytes)
	if err != nil {
		t.Fatalf("ParseRevocationList: %v", err)
	}
	issuerCert, err := parseCertificatePEMBytes(t, issuingAuthority.CertificatePEM)
	if err != nil {
		t.Fatalf("parse issuing authority certificate: %v", err)
	}
	if err := crl.CheckSignatureFrom(issuerCert); err != nil {
		t.Fatalf("CRL signature does not verify against the issuing authority: %v", err)
	}
	listedSerials := map[string]bool{}
	for _, entry := range crl.RevokedCertificateEntries {
		listedSerials[hex.EncodeToString(entry.SerialNumber.Bytes())] = true
	}
	if !listedSerials[revokedRow.Serial] {
		t.Errorf("CRL does not list the revoked certificate's serial %s", revokedRow.Serial)
	}
	if listedSerials[newLeaf.Serial] {
		t.Errorf("CRL lists the replacement certificate's serial %s -- only revoked rows belong in the document", newLeaf.Serial)
	}
}

// TestX509Attestation_ChainAndAttestationsSurviveARestart pins the
// idempotence of the boot-time chain creation: a second boot over the same
// database and object store finds the existing root and intermediate
// authorities (EnsureAuthorityChain's fixed-subject lookup) instead of
// minting a second chain, the attestation rows and certificates survive,
// and the same share link still serves through the gate after the
// restart.
func TestX509Attestation_ChainAndAttestationsSurviveARestart(t *testing.T) {
	imgServer := newFakeOpenAIImageServer(t)
	cfg := testConfig(t)
	cfg.AIGatewayImageBaseURL = imgServer.URL
	cfg.AIGatewayImageAPIKey = "sk-test-smilesim-key"
	objectRoot := t.TempDir()
	cfg.ObjectStoreRoot = objectRoot

	// The first server is built and torn down EXPLICITLY (its cleanup
	// stops the queue worker and closes the database), so the second boot
	// below is a genuine restart rather than two servers sharing one
	// database live.
	handler, cleanup, _, err := app.BuildServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("BuildServer (first boot): %v", err)
	}
	firstSrv := httptest.NewServer(handler)
	token := registerAndAuthenticate(t, firstSrv, cfg, "tenant-acme", "x509-restart")

	jpeg := jpegWithExif(t)
	photo := uploadPhotoAs(t, firstSrv, token, base64.StdEncoding.EncodeToString(jpeg))
	createCaseAs(t, firstSrv, token, "", caseCreateBody{
		PatientName:    "restart patient",
		PatientRef:     "RST-001",
		PhotoObjectIDs: []string{photo.ObjectID},
	})
	simulateBody, _ := json.Marshal(map[string]any{"photo_object_id": photo.ObjectID})
	resp := smileSimRequest(t, firstSrv, http.MethodPost, smileSimulatePath, token, simulateBody)
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("simulate status = %d, want 202; body = %s", resp.StatusCode, body)
	}
	var jobRef struct {
		JobID string `json:"job_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&jobRef)
	resp.Body.Close()
	status := waitForSmileSimSucceeded(t, firstSrv, token, jobRef.JobID, time.Now().Add(30*time.Second))
	outputObjectID, _ := status["output_object_id"].(string)
	if outputObjectID == "" {
		t.Fatalf("succeeded job carried no output_object_id")
	}
	share := mintShareAs(t, firstSrv, token, outputObjectID)
	access := sharingAccessRequest(t, firstSrv, share.Token, "")
	raw, _ := io.ReadAll(access.Body)
	access.Body.Close()
	if access.StatusCode != http.StatusOK || string(raw) != string(imgServer.generatedPNG) {
		t.Fatalf("pre-restart access: status = %d, want 200 with the generated image", access.StatusCode)
	}
	firstSrv.Close()
	if cleanupErr := cleanup(); cleanupErr != nil {
		t.Errorf("first server cleanup: %v", cleanupErr)
	}

	// The restart: a fresh server over the same database file and object
	// store directory (its cleanup is registered on t by the helper).
	secondSrv := buildAttestationServer(t, cfg)

	observerDB := openObserverDB(t, cfg)
	authorities := pki.NewAuthorityRepository(observerDB)
	all, err := authorities.ListAll(context.Background())
	if err != nil {
		t.Fatalf("list authorities after restart: %v", err)
	}
	roots, intermediates := 0, 0
	for _, row := range all {
		switch row.Type {
		case pki.AuthorityTypeRoot:
			roots++
		case pki.AuthorityTypeIntermediate:
			intermediates++
		}
	}
	if roots != 1 || intermediates != 1 {
		t.Fatalf("after the restart pki_authorities holds %d root and %d intermediate rows, want exactly one of each (the chain must not duplicate across boots)", roots, intermediates)
	}

	attestationRow := readAttestation(t, observerDB, outputObjectID)
	if attestationRow == "" {
		t.Fatalf("no attestation row after the restart -- the row did not survive")
	}
	if count := countAttestationRows(t, observerDB); count != 1 {
		t.Fatalf("attestations table holds %d rows after the restart, want 1", count)
	}

	// The same share link serves the same bytes through the gate on the
	// restarted process.
	access = sharingAccessRequest(t, secondSrv, share.Token, "")
	raw, _ = io.ReadAll(access.Body)
	access.Body.Close()
	if access.StatusCode != http.StatusOK || string(raw) != string(imgServer.generatedPNG) {
		t.Fatalf("post-restart access: status = %d, want 200 with the generated image", access.StatusCode)
	}
}

// --- helpers --------------------------------------------------------------

// buildAttestationTestServer wires BuildServer's real output behind an
// httptest.Server like buildSmileSimTestServer, with the AI-gateway image
// credential pointed at imgServer AND the object store pinned to a known
// directory (the tamper leg needs to reach the stored bytes).
func buildAttestationTestServer(t *testing.T, imgServer *fakeOpenAIImageServer) (*httptest.Server, app.ServerConfig) {
	t.Helper()
	cfg := testConfig(t)
	cfg.AIGatewayImageBaseURL = imgServer.URL
	cfg.AIGatewayImageAPIKey = "sk-test-smilesim-key"
	cfg.ObjectStoreRoot = t.TempDir()
	return buildAttestationServer(t, cfg), cfg
}

func buildAttestationServer(t *testing.T, cfg app.ServerConfig) *httptest.Server {
	t.Helper()
	handler, cleanup, _, err := app.BuildServer(t.Context(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() {
		if err := cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func openObserverDB(t *testing.T, cfg app.ServerConfig) *gorm.DB {
	t.Helper()
	db, err := dbkit.Open(context.Background(), dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     cfg.SQLitePath,
	})
	if err != nil {
		t.Fatalf("open observer connection to %q: %v", cfg.SQLitePath, err)
	}
	t.Cleanup(func() {
		sqlDB, dbErr := db.DB()
		if dbErr != nil {
			t.Errorf("observer connection handle: %v", dbErr)
			return
		}
		if closeErr := sqlDB.Close(); closeErr != nil {
			t.Errorf("close observer connection: %v", closeErr)
		}
	})
	return db
}

// readAttestation returns the certificate id of the current attestation
// row for objectID, or "" when no row exists.
func readAttestation(t *testing.T, db *gorm.DB, objectID string) string {
	t.Helper()
	var certificateID string
	err := db.WithContext(context.Background()).
		Raw("SELECT certificate_id FROM smilesim_attestations WHERE object_id = ?", objectID).
		Scan(&certificateID).Error
	if err != nil {
		t.Fatalf("read attestation row for %s: %v", objectID, err)
	}
	return certificateID
}

func countAttestationRows(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var count int64
	if err := db.WithContext(context.Background()).
		Raw("SELECT COUNT(*) FROM smilesim_attestations").Scan(&count).Error; err != nil {
		t.Fatalf("count attestation rows: %v", err)
	}
	return count
}

// certificateRow reads one pki_certificates row through pki's own
// repository on the observer connection (tenant-scoped).
func certificateRow(t *testing.T, db *gorm.DB, certificateID string, tenant pkgcore.TenantID) *pki.Certificate {
	t.Helper()
	repo := pki.NewCertificateRepository(db)
	cert, err := repo.FindByID(pkgcore.WithTenant(context.Background(), tenant), certificateID)
	if err != nil {
		t.Fatalf("read certificate %s: %v", certificateID, err)
	}
	return cert
}

// authorityRow reads one pki_authorities row through pki's own repository
// on the observer connection.
func authorityRow(t *testing.T, db *gorm.DB, authorityID string) *pki.Authority {
	t.Helper()
	repo := pki.NewAuthorityRepository(db)
	row, err := repo.FindByID(context.Background(), authorityID)
	if err != nil {
		t.Fatalf("read authority %s: %v", authorityID, err)
	}
	return row
}

func parseCertificatePEMBytes(t *testing.T, certPEM string) (*x509.Certificate, error) {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatalf("no PEM block in authority certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

// storedObjectPath resolves the local object store's file for one object:
// cfg.ObjectStoreRoot/<tenant>/<objectID>/original -- the layout
// go/storage's ObjectKey derives and pkgcore's LocalObjectStore serves.
func storedObjectPath(cfg app.ServerConfig, tenant string, objectID string) string {
	return filepath.Join(cfg.ObjectStoreRoot, tenant, objectID, "original")
}

func mustReadStoredObject(t *testing.T, cfg app.ServerConfig, tenant string, objectID string) []byte {
	t.Helper()
	raw, err := os.ReadFile(storedObjectPath(cfg, tenant, objectID))
	if err != nil {
		t.Fatalf("read stored object %s/%s: %v", tenant, objectID, err)
	}
	return raw
}

func writeStoredObject(t *testing.T, cfg app.ServerConfig, tenant string, objectID string, content []byte) {
	t.Helper()
	path := storedObjectPath(cfg, tenant, objectID)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir for stored object: %v", err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write stored object %s/%s: %v", tenant, objectID, err)
	}
}

func assertRefusalCode(t *testing.T, body string, wantCode string) {
	t.Helper()
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("decode refusal body %s: %v", body, err)
	}
	if envelope.Code != wantCode {
		t.Fatalf("refusal code = %q, want %q; body = %s", envelope.Code, wantCode, body)
	}
}

func mintShareAs(t *testing.T, srv *httptest.Server, token string, resourceRef string) testCreateShareResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{"resourceRef": resourceRef})
	if err != nil {
		t.Fatalf("marshal create body: %v", err)
	}
	createResp := storageRequest(t, srv, http.MethodPost, sharing.PathShares,
		token, app.DemoOwnerUserID, "application/json", strings.NewReader(string(body)))
	var share testCreateShareResponse
	decodeSharingBody(t, createResp, http.StatusCreated, "create share for "+resourceRef, &share)
	return share
}
