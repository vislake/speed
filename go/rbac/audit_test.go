package rbac

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/vislake/speed/go/dbkit/audit"
	"github.com/vislake/speed/go/pkgcore"
)

// This file pins the audit side of role management: the four
// role-management write paths emit an audit row through go/dbkit/audit's
// declarative Emit mechanism, the rows carry a real Actor (and the
// dual-identity shape when the context carries one), and the module's
// declared audit vocabulary and its emission sites cannot drift apart.

// recordAuditEvents subscribes an eventRecorder to every EventRecorded the
// module's audit emissions publish. The in-memory bus delivers
// synchronously inside the emitting write, so when the write returns, the
// recorder holds every row it produced.
func recordAuditEvents(reg *pkgcore.ComponentRegistry) *eventRecorder {
	rec := &eventRecorder{}
	reg.Events.Subscribe(audit.EventRecorded, rec.record)
	return rec
}

// recordedAuditRows extracts the RecordedEvent payloads of every recorded
// EventRecorded, so a test can assert on the audit row shape.
func recordedAuditRows(rec *eventRecorder) []audit.RecordedEvent {
	var rows []audit.RecordedEvent
	for _, evt := range rec.ofType(audit.EventRecorded) {
		payload, ok := evt.Payload.(audit.RecordedEvent)
		if !ok {
			continue
		}
		rows = append(rows, payload)
	}
	return rows
}

// operatorCtx is the context shape of an administrative role-management
// write: a rbac.Subject in SystemDomain naming the operating staff member
// on the context, plus the managed tenant's own tenant context. rbac's
// audit emission derives the row's Actor from that Subject (audit.go).
func operatorCtx(operatorID string, tenant pkgcore.TenantID) context.Context {
	ctx := pkgcore.WithTenant(context.Background(), tenant)
	return WithSubject(ctx, Subject{TenantID: SystemDomain, UserID: operatorID})
}

// assertSingleAuditRow asserts rec holds exactly one audit row, that it
// describes an action on roleKey under tenant, and returns it.
func assertSingleAuditRow(t *testing.T, rec *eventRecorder, action, tenant, roleKey string) audit.RecordedEvent {
	t.Helper()
	rows := recordedAuditRows(rec)
	if len(rows) != 1 {
		t.Fatalf("got %d audit rows, want exactly 1 (before this round's fix: 0 -- the declared action was never emitted)", len(rows))
	}
	row := rows[0]
	if row.Action != action {
		t.Fatalf("audit action = %q, want %q", row.Action, action)
	}
	if row.TenantID != tenant {
		t.Fatalf("audit row tenant = %q, want %q", row.TenantID, tenant)
	}
	if row.Resource.Type != "rbac.role" || row.Resource.DisplayName != roleKey {
		t.Fatalf("audit resource = (%q, display %q), want (%q, %q)",
			row.Resource.Type, row.Resource.DisplayName, "rbac.role", roleKey)
	}
	if !row.Result.Success {
		t.Fatal("audit row records an unsuccessful outcome for a write that succeeded")
	}
	if row.Actor.ID == "" {
		t.Fatal("audit row carries an empty Actor -- a role-management write must never be recorded unattributed (the P1-rbac-audit blank)")
	}
	return row
}

func TestService_AssignRole_EmitsAnAuditedRowWithTheActingOperator(t *testing.T) {
	// An AssignRole through the service produces an audit row with a
	// non-empty actor naming the operator.
	svc, reg := newTestServiceWithRegistry(t)

	ctx := tenantCtx("tenant-a")
	if _, err := svc.DefineRole(ctx, RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	grantee := Subject{TenantID: "tenant-a", UserID: "user-1"}

	rec := recordAuditEvents(reg)
	opCtx := operatorCtx("operator-1", "tenant-a")
	if err := svc.AssignRole(opCtx, grantee, "reader", Scope{}); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	row := assertSingleAuditRow(t, rec, AuditActionRoleAssign, "tenant-a", "reader")
	if row.Actor.Type != pkgcore.ActorTypePlatformAdmin || row.Actor.ID != "operator-1" {
		t.Fatalf("actor = (%q, %q), want (platform_admin, operator-1) -- a SystemDomain subject is a platform operator",
			row.Actor.Type, row.Actor.ID)
	}
	if row.OnBehalfOf != nil {
		t.Fatalf("an ordinary (non-impersonated) write recorded OnBehalfOf %+v, want none", row.OnBehalfOf)
	}
	after := row.Changes.After
	if after["user_id"] != "user-1" || after["node_id"] != "" {
		t.Fatalf("diff after = %v, want user_id=user-1 with an empty node_id (tenant-wide)", after)
	}
}

func TestService_AssignRole_EmitsForTenantSubjectsAsUserActors(t *testing.T) {
	// A Subject in an ordinary tenant -- the shape a host's own in-tenant
	// automation installs -- derives a user Actor, never a platform-admin
	// one (audit.go's withAuditActor).
	svc, reg := newTestServiceWithRegistry(t)

	ctx := tenantCtx("tenant-a")
	if _, err := svc.DefineRole(ctx, RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	actorCtx := pkgcore.WithTenant(context.Background(), "tenant-a")
	actorCtx = WithSubject(actorCtx, Subject{TenantID: "tenant-a", UserID: "user-1"})
	grantee := Subject{TenantID: "tenant-a", UserID: "user-2"}

	rec := recordAuditEvents(reg)
	if err := svc.AssignRole(actorCtx, grantee, "reader", Scope{}); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	row := assertSingleAuditRow(t, rec, AuditActionRoleAssign, "tenant-a", "reader")
	if row.Actor.Type != pkgcore.ActorTypeUser || row.Actor.ID != "user-1" {
		t.Fatalf("actor = (%q, %q), want (user, user-1)", row.Actor.Type, row.Actor.ID)
	}
}

func TestService_RevokeRole_EmitsAnAuditedRowWithTheActingOperator(t *testing.T) {
	// A RevokeRole through the service produces an audit row with a
	// non-empty actor naming the operator.
	svc, reg := newTestServiceWithRegistry(t)

	ctx := tenantCtx("tenant-a")
	if _, err := svc.DefineRole(ctx, RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	grantee := Subject{TenantID: "tenant-a", UserID: "user-1"}
	opCtx := operatorCtx("operator-1", "tenant-a")
	if err := svc.AssignRole(opCtx, grantee, "reader", Scope{}); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	rec := recordAuditEvents(reg)
	if err := svc.RevokeRole(opCtx, grantee, "reader", Scope{}); err != nil {
		t.Fatalf("RevokeRole: %v", err)
	}
	row := assertSingleAuditRow(t, rec, AuditActionRoleRevoke, "tenant-a", "reader")
	if row.Actor.ID != "operator-1" {
		t.Fatalf("actor = %q, want operator-1", row.Actor.ID)
	}
	if row.Changes.After["user_id"] != "user-1" {
		t.Fatalf("diff after = %v, want the revoked grantee's user id recorded", row.Changes.After)
	}
}

func TestService_RestoreRole_EmitsAnAuditedRowForTheRestoredGrant(t *testing.T) {
	// RestoreRole makes a
	// grant exist again -- AssignRole's own semantics -- so its row records
	// under the assign action (the vocabulary has no restore verb), naming
	// the role and the grantee tuple.
	svc, reg := newTestServiceWithRegistry(t)

	ctx := tenantCtx("tenant-a")
	if _, err := svc.DefineRole(ctx, RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	grantee := Subject{TenantID: "tenant-a", UserID: "user-1"}
	opCtx := operatorCtx("operator-1", "tenant-a")
	if err := svc.AssignRole(opCtx, grantee, "reader", Scope{}); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if err := svc.RevokeRole(opCtx, grantee, "reader", Scope{}); err != nil {
		t.Fatalf("RevokeRole: %v", err)
	}

	rec := recordAuditEvents(reg)
	if err := svc.RestoreRole(opCtx, grantee, "reader", Scope{}); err != nil {
		t.Fatalf("RestoreRole: %v", err)
	}
	row := assertSingleAuditRow(t, rec, AuditActionRoleAssign, "tenant-a", "reader")
	if row.Actor.ID != "operator-1" {
		t.Fatalf("actor = %q, want operator-1", row.Actor.ID)
	}
}

func TestService_DefineRole_EmitsAnAuditedRow(t *testing.T) {
	// A role created
	// through DefineRole -- or through the built-in seeding that runs
	// through the same defineRole -- records rbac.role.define.
	svc, reg := newTestServiceWithRegistry(t)

	rec := recordAuditEvents(reg)
	opCtx := operatorCtx("operator-1", "tenant-a")
	role, err := svc.DefineRole(opCtx, RoleDefinition{
		Key:            "reader",
		DescriptionKey: "rbac.role.reader",
		Permissions:    []string{"notes:read"},
	})
	if err != nil {
		t.Fatalf("DefineRole: %v", err)
	}

	row := assertSingleAuditRow(t, rec, AuditActionRoleDefine, "tenant-a", "reader")
	if row.Actor.ID != "operator-1" {
		t.Fatalf("actor = %q, want operator-1", row.Actor.ID)
	}
	if row.Resource.ID != role.ID {
		t.Fatalf("resource id = %q, want the role's own id %q", row.Resource.ID, role.ID)
	}
}

func TestService_RoleWrite_RecordsTheContextActorWithOnBehalfOfUnchanged(t *testing.T) {
	// The impersonation hard rule: audit records produced during
	// impersonation must carry both the impersonated user as Actor
	// and the real administrator as OnBehalfOf. A role write whose context
	// an impersonation middleware already populated -- go/admin's
	// ImpersonationMiddleware layers WithActor(target) and
	// WithOnBehalfOf(admin) -- must land both identities on the row, with
	// rbac's own subject-derivation never overwriting the middleware's
	// Actor.
	svc, reg := newTestServiceWithRegistry(t)

	ctx := tenantCtx("tenant-a")
	if _, err := svc.DefineRole(ctx, RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	grantee := Subject{TenantID: "tenant-a", UserID: "target-user"}

	impersonatedCtx := pkgcore.WithActor(tenantCtx("tenant-a"),
		pkgcore.Actor{Type: pkgcore.ActorTypeUser, ID: "target-user"})
	impersonatedCtx = pkgcore.WithOnBehalfOf(impersonatedCtx,
		pkgcore.Actor{Type: pkgcore.ActorTypePlatformAdmin, ID: "admin-1"})

	rec := recordAuditEvents(reg)
	if err := svc.AssignRole(impersonatedCtx, grantee, "reader", Scope{}); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	rows := recordedAuditRows(rec)
	if len(rows) != 1 {
		t.Fatalf("got %d audit rows, want exactly 1 for the impersonation-era grant", len(rows))
	}
	row := rows[0]
	if row.Actor.Type != pkgcore.ActorTypeUser || row.Actor.ID != "target-user" {
		t.Fatalf("actor = (%q, %q), want the impersonated (user, target-user)", row.Actor.Type, row.Actor.ID)
	}
	if row.OnBehalfOf == nil || row.OnBehalfOf.Type != pkgcore.ActorTypePlatformAdmin || row.OnBehalfOf.ID != "admin-1" {
		t.Fatalf("on_behalf_of = %+v, want (platform_admin, admin-1) -- the impersonation hard rule", row.OnBehalfOf)
	}
}

func TestService_NoopWrites_EmitNoAuditRow(t *testing.T) {
	// A write that changed nothing -- an assign that found its grant
	// already there, a revoke of an absent grant -- emits nothing: the
	// audit trail records operations that happened, and nothing happened.
	// This also keeps the idempotent retry of a committed assign from
	// double-recording.
	svc, reg := newTestServiceWithRegistry(t)

	ctx := tenantCtx("tenant-a")
	if _, err := svc.DefineRole(ctx, RoleDefinition{Key: "reader", Permissions: []string{"notes:read"}}); err != nil {
		t.Fatalf("DefineRole: %v", err)
	}
	grantee := Subject{TenantID: "tenant-a", UserID: "user-1"}
	opCtx := operatorCtx("operator-1", "tenant-a")
	if err := svc.AssignRole(opCtx, grantee, "reader", Scope{}); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	rec := recordAuditEvents(reg)
	if err := svc.AssignRole(opCtx, grantee, "reader", Scope{}); err != nil {
		t.Fatalf("the duplicate assign must be a silent no-op: %v", err)
	}
	if rows := recordedAuditRows(rec); len(rows) != 0 {
		t.Fatalf("a no-op assign recorded %d audit rows, want none", len(rows))
	}

	if err := svc.RevokeRole(opCtx, grantee, "reader", Scope{}); err != nil {
		t.Fatalf("RevokeRole: %v", err)
	}
	rowsAfterRevoke := len(recordedAuditRows(rec))
	if err := svc.RevokeRole(opCtx, grantee, "reader", Scope{}); err == nil {
		t.Fatal("the absent-grant revoke must fail strict with ErrBindingNotFound")
	}
	// The strict revoke's ErrBindingNotFound must not have recorded either.
	if rows := recordedAuditRows(rec); len(rows) != rowsAfterRevoke {
		t.Fatal("the failed (absent-grant) revoke recorded an audit row")
	}
}

// TestAuditActions_DeclaredAndEmittedSetsAgree is the register-vs-emit
// reconciliation: a declared-but-never-emitted audit
// action is a mechanically checkable state, and this test checks it
// mechanically -- in both directions -- over this module's own non-test
// source.
//
// The check parses every non-test .go file of this package and:
//
//   - resolves the registered set from events.go's `auditActions` var --
//     the exact list Register adds to reg.AuditActions -- through the
//     AuditAction* constants' string values;
//   - resolves the emitted set from the Action field of every
//     audit.Input composite literal in the same files;
//   - asserts the registered set has an emit site for every member
//     (declared-never-emitted fails) and the emitted set contains nothing
//     undeclared (emitted-never-declared fails).
//
// The second direction is doubly enforced -- audit.Emit itself refuses an
// unregistered action with ErrActionNotRegistered at every call -- but the
// test pins it statically too, so a typo in any new site fails in this
// module's own suite before any host runs it. The check is deliberately
// source-shape-bound (an Input literal sitting directly in an emitAudit
// call, the shape every site in this module has); an emit site that
// builds its Input through a variable is invisible to this walk and must
// extend the test when it lands.
func TestAuditActions_DeclaredAndEmittedSetsAgree(t *testing.T) {
	moduleDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}

	declared := map[string]string{} // constant name -> action value
	registered := map[string]bool{} // action values Register adds
	emitted := map[string]int{}     // action values emitted at Input sites

	fset := token.NewFileSet()
	entries, err := os.ReadDir(moduleDir)
	if err != nil {
		t.Fatalf("reading the module directory: %v", err)
	}
	nonTestFiles := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		nonTestFiles[name] = file
	}

	// Pass 1: the AuditAction* constants' string values, from any const
	// block of any non-test file. Everything below resolves identifiers
	// through this map, so the pass must complete before any resolution.
	for _, file := range nonTestFiles {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
					continue
				}
				lit, ok := value.Values[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || !strings.HasPrefix(value.Names[0].Name, "AuditAction") {
					continue
				}
				declared[value.Names[0].Name] = strings.Trim(lit.Value, `"`)
			}
		}
	}

	// Pass 2: the registered set (events.go's `auditActions` var, whose
	// elements are the AuditAction* identifiers just resolved) and the
	// emitted set (every audit.Input literal's Action field, resolved the
	// same way, anywhere in the module's non-test code).
	for name, file := range nonTestFiles {
		if name == "events.go" {
			for _, decl := range file.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.VAR {
					continue
				}
				for _, spec := range gen.Specs {
					value, ok := spec.(*ast.ValueSpec)
					if !ok || len(value.Names) != 1 || value.Names[0].Name != "auditActions" {
						continue
					}
					composite, ok := value.Values[0].(*ast.CompositeLit)
					if !ok {
						continue
					}
					for _, elt := range composite.Elts {
						switch element := elt.(type) {
						case *ast.Ident:
							registered[declared[element.Name]] = true
						case *ast.BasicLit:
							if element.Kind == token.STRING {
								registered[strings.Trim(element.Value, `"`)] = true
							}
						}
					}
				}
			}
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			typeName, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || typeName.Sel.Name != "Input" {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok || key.Name != "Action" {
					continue
				}
				switch value := kv.Value.(type) {
				case *ast.Ident:
					// Action: AuditActionRoleAssign -- resolve through the
					// declared constants.
					emitted[declared[value.Name]]++
				case *ast.BasicLit:
					if value.Kind == token.STRING {
						emitted[strings.Trim(value.Value, `"`)]++
					}
				}
			}
			return true
		})
	}

	if len(registered) == 0 {
		t.Fatal("the audit-actions reconciliation found no registered actions -- events.go's auditActions var must have moved shape")
	}
	if len(emitted) == 0 {
		t.Fatal("no audit.Input site exists anywhere in the module's non-test source -- every declared action is declared-never-emitted (the P1-rbac-audit state)")
	}

	// Direction 1: declared-never-emitted. Every registered action must be
	// handed to an audit.Input somewhere in non-test code.
	for action := range registered {
		if emitted[action] == 0 {
			t.Errorf("audit action %q is registered (events.go's auditActions) but never emitted anywhere in non-test code", action)
		}
	}

	// Direction 2: emitted-never-declared. Every Input site must name a
	// registered action.
	for action, count := range emitted {
		if action == "" {
			continue // an Action field this walk could not resolve ("" value)
		}
		if !registered[action] {
			t.Errorf("audit action %q is emitted at %d Input site(s) but never registered -- audit.Emit would refuse it at runtime", action, count)
		}
	}
	if t.Failed() {
		t.Fatal("the declared and emitted audit-action sets disagree")
	}
}

// TestAuditActions_DeclaredSetMatchesTheConstants pins the tiny invariant
// the reconciliation above leans on: events.go's auditActions var lists
// exactly the three AuditAction* constants (in whatever order Register
// adds them), so the registered set and the constants the emit sites
// reference cannot drift.
func TestAuditActions_DeclaredSetMatchesTheConstants(t *testing.T) {
	want := map[string]bool{AuditActionRoleDefine: true, AuditActionRoleAssign: true, AuditActionRoleRevoke: true}
	if len(auditActions) != len(want) {
		t.Fatalf("auditActions = %v, want exactly the three declared constants %v", auditActions, want)
	}
	for _, action := range auditActions {
		if !want[action] {
			t.Fatalf("auditActions names %q, which is not one of the declared AuditAction* constants", action)
		}
	}
}
