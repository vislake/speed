package main

import (
	"context"
	"encoding/json"
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
// reference-app-go.md audit -- demoUserHeader (X-Demo-User) used to outrank
// a verified authn Principal UNCONDITIONALLY, with no way for an operator
// to turn that precedence off -- and, since the later Finding 2 round, its
// extension to the app's SECOND demo identity header: cfg.DisableDemoUserHeader
// (APP_DISABLE_DEMO_USER_HEADER, server.go) is the kill switch this file
// proves closes the hole on BOTH headers -- the rbac gate's X-Demo-User
// and the attribution seams' X-Demo-User-Id (demoOrgSubjectResolver /
// demoNotesSubjectResolver) -- and proves it changes nothing when left at
// its default.

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
// audit's failure scenario describes: a note-reader session that adds the
// X-Demo-User: demo-owner header thereby gains that tenant's owner-level
// access to everything.
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

// TestDemoUserIDHeader_KillSwitch_NoLongerImpersonatesAnySurface is the
// mandatory regression Finding 2 requires, on the SECOND demo identity
// header this app reads: demoOrgUserHeader ("X-Demo-User-Id", server.go)
// names the acting user for every attribution seam demoOrgSubjectResolver
// and demoNotesSubjectResolver serve -- notes' create handler, the cases
// surface, org's caller-scoped invitation endpoints and the notification
// module's whole surface. Before this round the kill switch
// (APP_DISABLE_DEMO_USER_HEADER) only reached demoUserHeader
// ("X-Demo-User") in the rbac gate; the X-Demo-User-Id resolvers were wired
// unconditionally, so with the switch ON a caller could still impersonate
// any user id on every one of those routes. This test proves the switch now
// covers the attribution header too, on two surfaces end to end:
//
//   - the cases list (a demoNotesSubjectResolver surface): with the switch
//     ON, listing "my cases" while sending X-Demo-User-Id naming a
//     different user must still answer the caller's OWN cases -- the
//     header is not read at all.
//   - the notification inbox (a demoOrgSubjectResolver surface): with the
//     switch ON, a request carrying NO demo header must resolve from the
//     verified Principal instead of being refused as subject-less, and a
//     request carrying a foreign X-Demo-User-Id must answer the same
//     principal-resolved result.
func TestDemoUserIDHeader_KillSwitch_NoLongerImpersonatesAnySurface(t *testing.T) {
	t.Run("cases list: switch on, a foreign X-Demo-User-Id cannot hijack the caller's own list", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.DisableDemoUserHeader = true
		handler, cleanup, _, err := buildServer(t.Context(), cfg)
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

		token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "killswitch-cases-owner")

		// A case created with NO header is attributed to the verified
		// Principal -- the caller's own user id.
		created := createCaseAs(t, srv, token, "", caseCreateBody{PatientName: "kill switch case"})
		if created.CreatorUserID == "" {
			t.Fatal("created case carries no creator_user_id")
		}

		// Listing the caller's own cases while sending a FOREIGN
		// X-Demo-User-Id must still answer the caller's own case: with the
		// switch on the header is not read at all. (The pre-fix bug: the
		// header was honored, the list was keyed to the foreign id, and the
		// caller's own case vanished from its own "my cases" answer.)
		resp := casesRequestAs(t, srv, http.MethodGet, casesPath, token, demoNotesCreatorUserID, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("GET %s with a foreign X-Demo-User-Id, switch on: status = %d, want %d; body = %s",
				casesPath, resp.StatusCode, http.StatusOK, body)
		}
		var list struct {
			Cases []testCase `json:"cases"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
			t.Fatalf("decode cases list: %v", err)
		}
		if len(list.Cases) != 1 {
			t.Fatalf("cases list under a foreign X-Demo-User-Id, switch on: %d rows, want the caller's own 1 -- "+
				"the attribution header must not be read when the kill switch is on (it was: the list was keyed to %q)",
				len(list.Cases), demoNotesCreatorUserID)
		}
		if list.Cases[0].CreatorUserID != created.CreatorUserID {
			t.Fatalf("listed case creator = %q, want the caller's own %q", list.Cases[0].CreatorUserID, created.CreatorUserID)
		}
	})

	t.Run("notification inbox: switch on, the verified Principal resolves with no demo header at all", func(t *testing.T) {
		cfg := testConfig(t)
		cfg.DisableDemoUserHeader = true
		handler, cleanup, _, err := buildServer(t.Context(), cfg)
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

		token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "killswitch-notif-owner")

		// A request with a verified token and NO demo header must be
		// resolved from the Principal -- with the switch on the header-only
		// scaffold stops applying. (The pre-fix bug: the notification
		// surface answered subject_unresolved to every header-less request,
		// verified Principal or not.)
		var inbox struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
		}
		notifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages", token, "", nil, http.StatusOK, &inbox)
		if len(inbox.Messages) != 0 {
			t.Fatalf("fresh inbox = %d messages, want 0", len(inbox.Messages))
		}

		// A foreign X-Demo-User-Id must change nothing about that answer:
		// the same principal-resolved, empty inbox.
		var spoofed struct {
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
		}
		notifRequest(t, srv, http.MethodGet, "/api/v1/notifications/messages", token, demoNotesCreatorUserID, nil, http.StatusOK, &spoofed)
		if len(spoofed.Messages) != 0 {
			t.Fatalf("inbox under a foreign X-Demo-User-Id = %d messages, want the principal's empty 0", len(spoofed.Messages))
		}
	})

	t.Run("switch at its default: the X-Demo-User-Id surfaces stay header-only, unchanged", func(t *testing.T) {
		// The no-behavior-change guard: with the switch at its default the
		// notification surface keeps refusing a header-less request with its
		// per-operation subject_unresolved -- the pinned pre-existing
		// behavior this round must not have touched.
		srv, cfg, _ := buildTestServer(t)

		token := registerAndAuthenticate(t, srv, cfg, "tenant-acme", "killswitch-default-owner")

		env := notifError(t, srv, http.MethodGet, "/api/v1/notifications/messages", token, "", nil, http.StatusUnauthorized)
		if env.Code == nil || *env.Code != "notification.subject_unresolved" {
			t.Fatalf("header-less notification request, switch at its default: code = %v, want %q",
				env.Code, "notification.subject_unresolved")
		}
	})
}

// TestDemoOrgSubjectResolver_HeaderDisabled_UsesPrincipalIgnoresHeader is
// the unit-level pin for the headerDisabled branch of the resolver that
// serves org's, notification's and integration's caller-scoped surfaces
// (server.go): given a request carrying BOTH demoOrgUserHeader and a
// verified Principal, the disabled resolver reports the Principal's user,
// never the header's -- and with no Principal at all it fails closed rather
// than falling back to the header it is disabling. The zero-value
// (header-enabled) resolver must keep its original header-only contract
// untouched: the header wins when present, and a header-less request with
// only a Principal still fails closed.
func TestDemoOrgSubjectResolver_HeaderDisabled_UsesPrincipalIgnoresHeader(t *testing.T) {
	const principalUserID = "real-unprivileged-user"

	withBoth := func() *http.Request {
		r := requestInTenant(http.MethodGet, "tenant-acme")
		r.Header.Set(demoOrgUserHeader, demoNotesCreatorUserID)
		return withTestPrincipal(r, principalUserID, "tenant-acme")
	}

	disabled := demoOrgSubjectResolver{headerDisabled: true}
	userID, ok := disabled.Subject(withBoth())
	if !ok {
		t.Fatal("the disabled resolver reported no subject for a request with a verified principal and a demo header")
	}
	if userID != principalUserID {
		t.Fatalf("disabled resolver user = %q, want the principal's %q -- headerDisabled must ignore the attribution header entirely",
			userID, principalUserID)
	}

	// Without a Principal at all, the disabled resolver must fail closed
	// rather than fall back to the header it is disabling.
	headerOnly := requestInTenant(http.MethodGet, "tenant-acme")
	headerOnly.Header.Set(demoOrgUserHeader, demoNotesCreatorUserID)
	if _, has := disabled.Subject(headerOnly); has {
		t.Fatal("the disabled resolver resolved a subject from the attribution header alone with no verified Principal; it must fail closed")
	}

	// The zero value stays byte-identical to the original header-only
	// resolver: the header wins over the Principal...
	got, ok := (demoOrgSubjectResolver{}).Subject(withBoth())
	if !ok || got != demoNotesCreatorUserID {
		t.Fatalf("default org resolver = (%q, %v), want the header's %q -- the header-enabled default must be unchanged",
			got, ok, demoNotesCreatorUserID)
	}
	// ...and a header-less, Principal-only request still fails closed.
	principalOnly := withTestPrincipal(requestInTenant(http.MethodGet, "tenant-acme"), principalUserID, "tenant-acme")
	if _, has := (demoOrgSubjectResolver{}).Subject(principalOnly); has {
		t.Fatal("the default org resolver resolved a subject from the Principal alone; it must stay header-only")
	}
}

// TestDemoNotesSubjectResolver_HeaderDisabled_UsesPrincipalIgnoresHeader is
// the sibling pin for the resolver serving notes' create handler and the
// cases surface (server.go): headerDisabled=true must ignore
// demoOrgUserHeader and resolve from the verified Principal alone -- the
// header-then-Principal default, where the header wins when both are
// present, must stay untouched.
func TestDemoNotesSubjectResolver_HeaderDisabled_UsesPrincipalIgnoresHeader(t *testing.T) {
	const principalUserID = "real-unprivileged-user"

	withBoth := func() *http.Request {
		r := requestInTenant(http.MethodGet, "tenant-acme")
		r.Header.Set(demoOrgUserHeader, demoNotesCreatorUserID)
		return withTestPrincipal(r, principalUserID, "tenant-acme")
	}

	disabled := demoNotesSubjectResolver{headerDisabled: true}
	userID, ok := disabled.Subject(withBoth())
	if !ok {
		t.Fatal("the disabled resolver reported no subject for a request with a verified principal and a demo header")
	}
	if userID != principalUserID {
		t.Fatalf("disabled resolver user = %q, want the principal's %q -- headerDisabled must ignore the attribution header entirely",
			userID, principalUserID)
	}

	// Without a Principal at all, the disabled resolver must fail closed
	// rather than fall back to the header it is disabling.
	headerOnly := requestInTenant(http.MethodGet, "tenant-acme")
	headerOnly.Header.Set(demoOrgUserHeader, demoNotesCreatorUserID)
	if _, has := disabled.Subject(headerOnly); has {
		t.Fatal("the disabled resolver resolved a subject from the attribution header alone with no verified Principal; it must fail closed")
	}

	// The zero value stays byte-identical to the original
	// header-then-Principal resolver: the header wins when both are
	// present, and the Principal supplies the user only when no header
	// rides along.
	got, ok := (demoNotesSubjectResolver{}).Subject(withBoth())
	if !ok || got != demoNotesCreatorUserID {
		t.Fatalf("default notes resolver = (%q, %v), want the header's %q -- the header-first default must be unchanged",
			got, ok, demoNotesCreatorUserID)
	}
	principalOnly := withTestPrincipal(requestInTenant(http.MethodGet, "tenant-acme"), principalUserID, "tenant-acme")
	got, ok = (demoNotesSubjectResolver{}).Subject(principalOnly)
	if !ok || got != principalUserID {
		t.Fatalf("default notes resolver with no header = (%q, %v), want the principal's %q",
			got, ok, principalUserID)
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
