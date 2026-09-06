package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/pkgcore"
)

// withTestPrincipal attaches a verified authn.Principal to r's context, the
// same shape tenancy.Middleware leaves after authn.Middleware verifies a
// real access token -- see requestInTenant's own doc comment for the
// tenant-context half this pairs with.
func withTestPrincipal(r *http.Request, userID, tenantID string) *http.Request {
	principal := authn.Principal{UserID: userID, TenantID: pkgcore.TenantID(tenantID), SessionID: "session-1"}
	return r.WithContext(authn.WithPrincipal(r.Context(), principal))
}

// demo_user_header_kill_switch_test.go pins the fix for Finding 1 of the
// reference-app-go.md audit: demoUserHeader (X-Demo-User) used to outrank a
// verified authn Principal UNCONDITIONALLY, with no way for an operator to
// turn that precedence off. cfg.DisableDemoUserHeader
// (APP_DISABLE_DEMO_USER_HEADER, server.go) is the kill switch this file
// proves closes the hole, and proves it changes nothing when left at its
// default.

// buildSeededUsersTestServerWithHeaderSwitch is buildSeededUsersTestServer
// (demo_users_test.go) plus one more knob: disableDemoUserHeader, threaded
// straight onto cfg.DisableDemoUserHeader before buildServer runs. false
// reproduces buildSeededUsersTestServer's own behavior exactly.
func buildSeededUsersTestServerWithHeaderSwitch(t *testing.T, password string, disableDemoUserHeader bool) *httptest.Server {
	t.Helper()

	cfg := testConfig(t)
	cfg.DemoUsersPassword = password
	cfg.DisableDemoUserHeader = disableDemoUserHeader
	handler, cleanup, _, err := buildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("buildServer: %v", err)
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

// postNoteWithDemoHeader signs a real bearer token's request to create a
// note, additionally carrying X-Demo-User=demoUser -- the shape the audit's
// failure scenario names: a real, verified-but-unprivileged session that
// also sends the demo header naming a higher-privileged demo actor.
func postNoteWithDemoHeader(t *testing.T, srv *httptest.Server, bearerToken, demoUser, text string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/notes", strings.NewReader(`{"text":"`+text+`"}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(demoUserHeader, demoUser)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/notes: %v", err)
	}
	return resp
}

// TestDemoUserHeader_KillSwitch_ClosesThePrivilegeEscalationHole is the
// mandatory end-to-end regression Finding 1 requires: a REAL invited-shaped
// session (the seeded demo-reader account, signed in through authn's real
// login route, holding notes:read and nothing else) additionally sends
// X-Demo-User: demo-owner (rbac's built-in owner role, every permission any
// module declared) on a POST /api/v1/notes -- the exact escalation the
// audit's failure scenario describes ("note-reader 会话加 X-Demo-User:
// demo-owner 即获该租户 owner 一切权限").
//
// The first subtest is the confirmed bug, reproduced against the real
// composed HTTP stack: with the kill switch left at its default (unset),
// the demo header still wins over the reader's own verified Principal and
// the write succeeds as the owner -- unchanged behavior, by design, so
// every existing demo journey keeps meaning what it always meant. The
// second subtest is the fix: with the switch enabled
// (cfg.DisableDemoUserHeader = true, what APP_DISABLE_DEMO_USER_HEADER
// sets), the SAME request is now decided against the reader's own Principal
// alone -- the header is not read at all -- and rbac refuses it.
func TestDemoUserHeader_KillSwitch_ClosesThePrivilegeEscalationHole(t *testing.T) {
	t.Run("switch at its default: the header still escalates (the confirmed bug, unchanged)", func(t *testing.T) {
		srv := buildSeededUsersTestServerWithHeaderSwitch(t, demoSeedPassword, false)

		status, code, readerToken := demoLogin(t, srv, demoReaderEmail, demoSeedPassword, "tenant-acme")
		if status != http.StatusOK {
			t.Fatalf("login as the seeded reader: status = %d, code = %q, want %d", status, code, http.StatusOK)
		}

		resp := postNoteWithDemoHeader(t, srv, readerToken, demoOwnerUserID, "escalated note")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("POST as the reader with X-Demo-User: demo-owner, switch unset: status = %d, want %d "+
				"(this IS the privilege-escalation hole Finding 1 confirms -- it must stay reproducible with the "+
				"switch at its default, or every existing demo journey built around the header winning just broke); body = %s",
				resp.StatusCode, http.StatusCreated, body)
		}
	})

	t.Run("switch enabled: the header is ignored, the reader's own grant decides, and the write is refused", func(t *testing.T) {
		srv := buildSeededUsersTestServerWithHeaderSwitch(t, demoSeedPassword, true)

		status, code, readerToken := demoLogin(t, srv, demoReaderEmail, demoSeedPassword, "tenant-acme")
		if status != http.StatusOK {
			t.Fatalf("login as the seeded reader: status = %d, code = %q, want %d", status, code, http.StatusOK)
		}

		resp := postNoteWithDemoHeader(t, srv, readerToken, demoOwnerUserID, "escalation attempt, switch enabled")
		assertPermissionDenied(t, resp, "POST as the reader with X-Demo-User: demo-owner, switch enabled")

		// The reader's OWN permission (read) must still work unaffected --
		// the switch turns the HEADER off, not the Principal fallback path
		// demo_users_test.go's own tests already pin.
		getResp := notesRequestAs(t, srv, http.MethodGet, readerToken, "", nil)
		defer getResp.Body.Close()
		if getResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(getResp.Body)
			t.Fatalf("GET as the reader, switch enabled: status = %d, want %d; body = %s", getResp.StatusCode, http.StatusOK, body)
		}
	})
}

// TestDemoResolveSubject_HeaderDisabled_IgnoresHeaderUsesPrincipal is the
// unit-level pin for demoResolveSubject's headerDisabled=true branch: given
// a request carrying BOTH demoUserHeader and a verified Principal, the
// disabled-header resolver reports the Principal's user, never the header's
// -- the exact opposite of demoSubjectResolver's own (headerDisabled=false)
// contract, which TestDemoSubjectResolver_HeaderPrecedenceAndPrincipalFallback
// (demo_subject_test.go) already pins as unchanged.
func TestDemoResolveSubject_HeaderDisabled_IgnoresHeaderUsesPrincipal(t *testing.T) {
	const principalUserID = "real-unprivileged-user"

	r := requestInTenant(http.MethodPost, "tenant-acme")
	r.Header.Set(demoUserHeader, demoOwnerUserID)
	r = withTestPrincipal(r, principalUserID, "tenant-acme")

	sub, ok := demoResolveSubject(r, true)
	if !ok {
		t.Fatal("no subject for a request with a tenant, a verified principal and a demo header")
	}
	if sub.UserID != principalUserID {
		t.Fatalf("subject user = %q, want the principal's %q -- headerDisabled must ignore the header entirely", sub.UserID, principalUserID)
	}
	if sub.TenantID != "tenant-acme" {
		t.Fatalf("subject tenant = %q, want %q", sub.TenantID, "tenant-acme")
	}

	// Without a Principal at all, a header-disabled resolver must fail
	// closed rather than fall back to the header it is disabling.
	r2 := requestInTenant(http.MethodPost, "tenant-acme")
	r2.Header.Set(demoUserHeader, demoOwnerUserID)
	if _, ok := demoResolveSubject(r2, true); ok {
		t.Fatal("headerDisabled resolved a subject from the header alone with no verified Principal; it must fail closed")
	}
}

// TestDemoSubjectResolverFor_DefaultIsByteIdenticalToDemoSubjectResolver is
// the guard test Finding 1 requires: demoSubjectResolverFor(false) --
// exactly what buildServer wires when cfg.DisableDemoUserHeader is left at
// its zero value -- must resolve identically to demoSubjectResolver itself
// in every case demo_subject_test.go already pins, so this round changes
// nothing about the header-wins default.
func TestDemoSubjectResolverFor_DefaultIsByteIdenticalToDemoSubjectResolver(t *testing.T) {
	resolver := demoSubjectResolverFor(false)

	cases := []struct {
		name       string
		tenant     string
		headerUser string
		principal  string
	}{
		{name: "header only", tenant: "tenant-acme", headerUser: demoOwnerUserID},
		{name: "principal only", tenant: "tenant-acme", principal: "real-seeded-user"},
		{name: "header wins over principal", tenant: "tenant-acme", headerUser: demoReaderUserID, principal: "real-seeded-user"},
		{name: "neither", tenant: "tenant-acme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := requestInTenant(http.MethodGet, pkgcore.TenantID(tc.tenant))
			if tc.headerUser != "" {
				r.Header.Set(demoUserHeader, tc.headerUser)
			}
			if tc.principal != "" {
				r = withTestPrincipal(r, tc.principal, tc.tenant)
			}

			gotSub, gotOK := resolver(r)
			wantSub, wantOK := demoSubjectResolver(r)
			if gotOK != wantOK || gotSub != wantSub {
				t.Fatalf("demoSubjectResolverFor(false)(r) = (%+v, %v), want demoSubjectResolver(r) = (%+v, %v)",
					gotSub, gotOK, wantSub, wantOK)
			}
		})
	}
}
