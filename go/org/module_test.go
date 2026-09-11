package org

import (
	"context"
	"embed"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/componenttest"
	"github.com/vislake/speed/go/pkgcore/apperr"
)

func TestModule_Identity(t *testing.T) {
	m := NewModule(nil)

	if got := m.Name(); got != "org" {
		t.Errorf("Name() = %q, want %q", got, "org")
	}
	if got := m.DependsOn(); got != nil {
		t.Errorf("DependsOn() = %v, want nil -- org depends on no other pkgcore.Module, authn least of all", got)
	}
	if got := m.OpenAPISpec(); len(got) == 0 {
		t.Error("OpenAPISpec() is empty, want the embedded api/openapi.yaml fragment")
	}
}

// TestModule_Migrations_ExposesBothDialectsAtTheEmbedRoot pins the layout
// dbkit.MigrationRegistry.Apply requires: a postgres/ and a sqlite/
// subdirectory at the FS root, each holding the same file names. A migration
// added to one dialect and forgotten in the other fails here rather than at a
// deployment that happens to run the other engine.
func TestModule_Migrations_ExposesBothDialectsAtTheEmbedRoot(t *testing.T) {
	fs := NewModule(nil).Migrations()

	names := map[string][]string{}
	for _, dialect := range []string{"postgres", "sqlite"} {
		entries, err := fs.ReadDir(dialect)
		if err != nil {
			t.Fatalf("ReadDir(%q): %v", dialect, err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s/ holds no migration files", dialect)
		}
		for _, e := range entries {
			names[dialect] = append(names[dialect], e.Name())
		}
	}
	if len(names["postgres"]) != len(names["sqlite"]) {
		t.Fatalf("dialect file counts differ: postgres %v, sqlite %v", names["postgres"], names["sqlite"])
	}
	for i, name := range names["postgres"] {
		if names["sqlite"][i] != name {
			t.Errorf("migration %q exists in postgres/ but sqlite/ has %q at the same position", name, names["sqlite"][i])
		}
	}
}

// TestModule_Locales_ShipsBothLanguages pins the i18n rule at the module
// boundary: exactly the two languages the catalog serves, no more and no
// fewer, since Kernel.Bootstrap rejects a module that ships a file for a
// language the others do not.
func TestModule_Locales_ShipsBothLanguages(t *testing.T) {
	entries, err := NewModule(nil).Locales().ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	for _, want := range []string{"zh-CN.toml", "en-US.toml"} {
		if !got[want] {
			t.Errorf("locales are missing %s", want)
		}
	}
	if len(entries) != 2 {
		t.Errorf("locales hold %d files, want exactly zh-CN.toml and en-US.toml", len(entries))
	}
}

// TestModule_Register_DeclaresItsSurface bootstraps org through the real
// kernel -- the same path a host takes -- and asserts every declaration
// arrives on the registry. Bootstrapping rather than calling Register against
// a hand-built Registry is deliberate: it also proves org's locale files
// survive i18n.Builder.AddModule's parity validation, which only runs there.
func TestModule_Register_DeclaresItsSurface(t *testing.T) {
	reg := bootstrapTestModule(t)

	t.Run("permissions", func(t *testing.T) {
		assertContainsAll(t, reg.PermissionsSeat().Permissions(), []string{
			PermissionRead, PermissionManage, PermissionInviteMember, PermissionRemoveMember,
		})
	})

	t.Run("audit actions", func(t *testing.T) {
		// Exact match, both directions: org declares exactly the six
		// actions its write paths can produce through automatic audit
		// capture (module.go's audit block). A declared action no write
		// can ever produce -- a dead-vocabulary declaration -- or a produced
		// action nobody declared (which the
		// go/dbkit/audit persister's vocabulary gate would refuse with a
		// structured alert) both fail this assertion.
		wantActions := []string{
			AuditActionNodeCreate, AuditActionNodeUpdate,
			AuditActionMemberCreate, AuditActionMemberUpdate,
			AuditActionInvitationCreate, AuditActionInvitationUpdate,
		}
		assertContainsAll(t, reg.AuditActionsSeat().Actions(), wantActions)
		assertContainsAll(t, wantActions, reg.AuditActionsSeat().Actions())
	})

	t.Run("published events", func(t *testing.T) {
		var types []string
		for _, decl := range reg.EventsSeat().Published() {
			types = append(types, decl.Type)
			if decl.PayloadType == "" || decl.Description == "" {
				t.Errorf("event %q is declared without a payload type or description", decl.Type)
			}
		}
		assertContainsAll(t, types, []string{EventNodeCreated, EventNodeMoved, EventNodeDeleted, EventNodeRestored})
	})

	t.Run("routes", func(t *testing.T) {
		routes := reg.RoutesSeat().Routes()
		if len(routes) != 1 {
			t.Fatalf("Register mounted %d route(s), want exactly 1 (apiPath)", len(routes))
		}
		if routes[0].Path != apiPath {
			t.Errorf("mounted route path = %q, want %q", routes[0].Path, apiPath)
		}
		if routes[0].Handler == nil {
			t.Error("the mounted route carries a nil handler")
		}
	})

	t.Run("no configuration schema is declared", func(t *testing.T) {
		// org honours its bounds as package constants rather than declaring
		// a dynamic-config schema it cannot read back (it must not import
		// config). Declaring a schema nothing honours would be a lying
		// schema, so the absence is asserted rather than left implicit.
		if got := reg.ConfigSeat().Items(); len(got) != 0 {
			t.Errorf("Register declared %d config item(s); org declares none it cannot honour", len(got))
		}
	})
}

// TestModule_AuditableModels_IsExactlyTheMarkedModels pins org's exported
// capture scope (AuditableModels, the slice a host hands to
// dbkit.Options.AuditModels) to the models that actually carry the
// dbkit.Auditable marker -- the "a model gained the marker while the
// capture list forgot it" half of the AuditModels contract, the half dbkit
// cannot check at Open: Open receives no model inventory (GORM's models
// register lazily, per statement), so resolveAuditModels can refuse a
// LISTED model whose marker was removed after listing, but no mechanism
// can notice a marker ADDED to a model the list forgot -- and a marker
// with no capture list entry means that model's writes silently stop
// being audited.
//
// The model names below ARE the inventory: every org data model whose own
// file declares `var _ dbkit.Auditable`. Adding the marker to another
// model belongs to AuditableModels() and to this list in the same edit --
// this test exists so that when one of the two moves without the other,
// the divergence fails here, in org's own suite, instead of surfacing
// years later as org audit rows that stopped landing.
func TestModule_AuditableModels_IsExactlyTheMarkedModels(t *testing.T) {
	want := map[string]bool{
		"org.OrgNode":    true,
		"org.Membership": true,
		"org.Invitation": true,
	}
	got := map[string]bool{}
	for _, m := range AuditableModels() {
		got[fmt.Sprintf("%T", m)] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("AuditableModels() is missing %s, the dbkit.Auditable model the capture scope must cover", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("AuditableModels() lists %s, which is not one of the models carrying the dbkit.Auditable marker", name)
		}
	}
}

// TestModule_AuditableModels_CaptureClasses_CoverEveryField is the
// field-granularity counterpart of the model-level gate above. That gate
// pins WHICH models carry the dbkit.Auditable marker and land in
// AuditableModels(); this one pins what the write-capture mechanism will
// do with every COLUMN of those models. Each column field must fall into
// one of three classes: it is auto-redacted by the mechanism (a GORM
// serializer, or the audit:"redact" capture opt-out tag --
// captureColumnAutoRedacts), or it is listed in judgedCaptured below with
// the model author's written judgment that recording the column's
// plaintext value in the audit trail is right. There is no default class:
// a plaintext-sensitive column added to an Auditable model with no tag and
// no judgment entry fails here, in org's own suite, instead of having its
// values land verbatim in the changes column of every audit row its writes
// produce. That column is not a quiet store: go/admin serves the trail
// over HTTP to any admin:audit_read holder across every tenant, and
// compliance's RenderAuditReport exports the diff verbatim (go/dbkit/audit/
// model.go's Changes doc comment names both exits). Requiring the judgment
// to be written down, reason included, is what makes the third class an
// action rather than a silent default.
//
// The auto-redaction criteria below are read off the same struct tags
// dbkit's fieldValuesMap reads (audit_capture.go), so a field this helper
// classifies as auto-redacted is exactly a field the mechanism captures as
// "[redacted]" -- the serializer half is tag-declared by construction,
// since dbkit's RegisterEncryptedSerializer mechanism is the only way a
// serializer exists in this ecosystem. If the mechanism's criteria ever
// change, this test and go/dbkit's own audit_capture_test.go change
// together.
func TestModule_AuditableModels_CaptureClasses_CoverEveryField(t *testing.T) {
	// The third class, written down: every non-auto-redacted column of an
	// Auditable model, keyed "<model>.<GoField>" as fmt %T prints the
	// model, with the judgment that its plaintext belongs in the trail.
	// The model names ARE the inventory AuditableModels() already pins; a
	// new Auditable model or a new column without a line here fails below.
	judgedCaptured := map[string]string{
		"org.OrgNode.ID":        "application-generated UUID naming the node -- identifiers are the trail's own reference vocabulary, never content",
		"org.OrgNode.TenantID":  "owning-tenant attribution, the row's own first-class dimension and an admin read's filter key -- never personal data",
		"org.OrgNode.ParentID":  "opaque id of the parent node -- the structural edge of the org tree, which is exactly what an org.node.write diff exists to record",
		"org.OrgNode.Path":      "derived materialized-path index of the node's tree position -- structural metadata, tree-internal by construction",
		"org.OrgNode.Depth":     "depth integer derived from the path -- structural metadata, tree-internal by construction",
		"org.OrgNode.Name":      "tenant-authored display label of a business node (a group/region/store name) -- the tenant's own organizational vocabulary, not person-attributable content",
		"org.OrgNode.Kind":      "the tenant's own business classification of the node -- vocabulary, not content",
		"org.OrgNode.CreatedAt": "auto-maintained timestamp (gorm autoCreateTime) -- never application content",
		"org.OrgNode.UpdatedAt": "auto-maintained timestamp (gorm autoUpdateTime) -- never application content",
		"org.OrgNode.DeletedAt": "soft-delete marker timestamp, written through dbkit's own reflection-based soft-delete path -- lifecycle metadata, never content",
		"org.OrgNode.DeletedBy": "opaque user id of the deleting principal -- attribution, the trail's purpose; never a display name",

		"org.Membership.ID":        "application-generated UUID naming the membership row -- identifier vocabulary",
		"org.Membership.TenantID":  "owning-tenant attribution -- see OrgNode.TenantID's judgment",
		"org.Membership.UserID":    "opaque cross-module user id of the member -- attribution reference the trail exists to record, never a display name or personal attribute",
		"org.Membership.NodeID":    "opaque id of the OrgNode the membership binds to -- structural reference",
		"org.Membership.Status":    "membership-status vocabulary (active/inactive/...) -- the state transition an org.member.update diff exists to record",
		"org.Membership.CreatedAt": "auto-maintained timestamp -- never application content",
		"org.Membership.UpdatedAt": "auto-maintained timestamp -- never application content",
		"org.Membership.DeletedAt": "soft-delete marker timestamp -- lifecycle metadata, never content",
		"org.Membership.DeletedBy": "opaque user id of the removing principal -- attribution, the trail's purpose; never a display name",

		"org.Invitation.ID":            "application-generated UUID naming the invitation -- identifier vocabulary",
		"org.Invitation.TenantID":      "owning-tenant attribution -- see OrgNode.TenantID's judgment",
		"org.Invitation.NodeID":        "opaque id of the OrgNode the invitee will bind to on acceptance -- structural reference",
		"org.Invitation.InviterUserID": "opaque user id of the inviting member -- attribution, the trail's purpose; never a display name",
		"org.Invitation.Locale":        "language tag the invitation message was rendered in (e.g. zh-CN) -- delivery metadata, never content",
		"org.Invitation.TokenHash":     "SHA-256 of the single-use 32-random-byte token, recorded deliberately -- no candidate space to reverse, unique per invitation, person-attributable meaning none (see Invitation.AuditResourceType's own doc comment for the full credential-context judgment)",
		"org.Invitation.Status":        "invitation-status vocabulary (pending/accepted/revoked) -- the state machine an org.invitation.update diff exists to record",
		"org.Invitation.ExpiresAt":     "invitation expiry timestamp -- lifecycle metadata, never content",
		"org.Invitation.AcceptedAt":    "acceptance timestamp, nil until accepted -- lifecycle metadata, never content",
		"org.Invitation.CreatedAt":     "auto-maintained timestamp -- never application content",
		"org.Invitation.UpdatedAt":     "auto-maintained timestamp -- never application content",
	}

	seen := map[string]bool{}
	for _, m := range AuditableModels() {
		model := fmt.Sprintf("%T", m)
		for _, field := range captureColumnFields(m) {
			key := model + "." + field.goName
			seen[key] = true
			if captureColumnAutoRedacts(field.sf) {
				if reason, ok := judgedCaptured[key]; ok {
					t.Errorf("%s is auto-redacted by the capture mechanism (serializer or audit:\"redact\" tag) but still listed in judgedCaptured (%q) -- a stale judgment entry, not a class", key, reason)
				}
				continue
			}
			reason, ok := judgedCaptured[key]
			if !ok {
				t.Errorf("%s (column %q) has no capture class: it is not auto-redacted (no gorm serializer, no audit:\"redact\" tag) and is not listed in judgedCaptured -- its plaintext would land in every audit row's changes column, an exit readable by any admin:audit_read holder across all tenants", key, field.column)
				continue
			}
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s is listed in judgedCaptured without a written judgment -- the reason IS the judgment, and an empty one records nothing", key)
			}
		}
	}
	for key := range judgedCaptured {
		if !seen[key] {
			t.Errorf("judgedCaptured entry %q names no captured column field of any Auditable model -- a stale or misspelled judgment", key)
		}
	}
}

// captureColumnField is one schema-column leaf of an Auditable model as
// the write-capture plugin sees it: the model type flattened through its
// anonymous embedded structs (dbkit.TenantModel first among them), exactly
// the shape go/dbkit/audit_capture.go's fieldValuesMap walks.
type captureColumnField struct {
	goName string // the leaf's own Go name ("TenantID", never the embedder chain)
	column string // the gorm column: option when the field declares one, else ""
	sf     reflect.StructField
}

// captureColumnFields enumerates every schema-column leaf of v's type:
// fields gorm would give a DBName, flattened through anonymous embedded
// structs and skipping unexported fields and gorm:"-" fields -- the
// mechanism's own empty-DBName skip, mirrored here so a gorm:"-" field
// (a value deliberately kept off the schema) is never demanded to carry a
// capture judgment it can never need.
func captureColumnFields(v any) []captureColumnField {
	var out []captureColumnField
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue
			}
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				// GORM flattens an anonymous embedded struct into its own
				// fields (TenantModel's TenantID), so the gate must too:
				// the plugin captures the promoted column, not the embedder.
				walk(f.Type)
				continue
			}
			if f.Tag.Get("gorm") == "-" {
				continue
			}
			out = append(out, captureColumnField{
				goName: f.Name,
				column: gormColumnName(f),
				sf:     f,
			})
		}
	}
	walk(reflect.TypeOf(v))
	return out
}

// gormColumnName returns the column: option of f's gorm tag, or "" when
// the field declares none (gorm would then derive a name from the field's
// Go name -- the column still exists and the field still needs a class).
func gormColumnName(f reflect.StructField) string {
	for _, opt := range strings.Split(f.Tag.Get("gorm"), ";") {
		if rest, ok := strings.CutPrefix(opt, "column:"); ok {
			return rest
		}
	}
	return ""
}

// captureColumnAutoRedacts reports whether dbkit's write-capture plugin
// captures f's column as "[redacted]" on its own, by either of the two
// automatic criteria go/dbkit/audit_capture.go's fieldValuesMap applies:
// the audit:"redact" capture opt-out tag, or a GORM serializer declared
// through the gorm tag. The two are read off the same struct tags the
// mechanism itself reads (see the gate's doc comment above for why that
// mirror is exact in this ecosystem).
func captureColumnAutoRedacts(f reflect.StructField) bool {
	if v, ok := f.Tag.Lookup("audit"); ok && v == "redact" {
		return true
	}
	for _, opt := range strings.Split(f.Tag.Get("gorm"), ";") {
		if strings.HasPrefix(opt, "serializer:") {
			return true
		}
	}
	return false
}

// TestModule_Register_DoesNotDeclareAuthnsEvent is the guard for the trap
// that would only surface once the authn module ships: declaring another
// module's event type collides at bootstrap, so org must subscribe to
// authn.user.created without ever declaring it.
func TestModule_Register_DoesNotDeclareAuthnsEvent(t *testing.T) {
	reg := bootstrapTestModule(t)
	for _, decl := range reg.EventsSeat().Published() {
		if decl.Type == "authn.user.created" {
			t.Fatal("org declared authn.user.created; only authn may declare it, or both modules collide at bootstrap")
		}
		if got := decl.Type[:4]; got != "org." {
			t.Errorf("org declared event %q, which is not in org's own namespace", decl.Type)
		}
	}
}

// TestModule_Register_CoexistsWithAnotherModule proves org bootstraps
// alongside a module that declares its own permissions, audit actions and
// events -- the real host shape -- rather than only in isolation.
func TestModule_Register_CoexistsWithAnotherModule(t *testing.T) {
	reg, err := componenttest.DeclareModules(newWiredModule(t, nil), neighbourModule{})
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	assertContainsAll(t, reg.PermissionsSeat().Permissions(), []string{PermissionRead, "neighbour:read"})
}

// TestModule_Register_PerformsNoIO calls Register with a nil database, which
// is what pkgcore.Module's "it only declares" contract requires to be safe:
// any database call inside Register would panic here.
func TestModule_Register_PerformsNoIO(t *testing.T) {
	m := newWiredModule(t, nil)
	reg := componenttest.NewRegistry()
	if err := componenttest.DeclareInto(reg, m); err != nil {
		t.Fatalf("Register against a nil database: %v", err)
	}
}

func TestModule_Tree_ReturnsAUsableService(t *testing.T) {
	m := newWiredModule(t, newTestDB(t))
	ctx := tenantCtx("tenant-a")

	root, err := m.Tree().CreateRoot(ctx, "Acme Dental", "group")
	if err != nil {
		t.Fatalf("CreateRoot through the module's tree: %v", err)
	}
	if root.ID == "" {
		t.Error("the module's tree produced a node with no id")
	}
	first, second := m.Tree(), m.Tree()
	if first != second {
		t.Error("Tree() returns a different service on each call; hosts hold onto it")
	}
}

// neighbourModule stands in for any other module a host bootstraps next to
// org. It declares a disjoint surface, so a collision would mean org claimed
// a name outside its namespace.
type neighbourModule struct{}

func (neighbourModule) Name() string         { return "neighbour" }
func (neighbourModule) DependsOn() []string  { return nil }
func (neighbourModule) Migrations() embed.FS { return embed.FS{} }
func (neighbourModule) Locales() embed.FS    { return embed.FS{} }
func (neighbourModule) OpenAPISpec() []byte  { return nil }
func (neighbourModule) Register(reg *pkgcore.ComponentRegistry) error {
	if err := reg.PermissionsSeat().Add("neighbour:read"); err != nil {
		return err
	}
	return reg.AuditActionsSeat().Add("neighbour.thing.do")
}


// assertContainsAll fails t unless got holds every entry in want.
func assertContainsAll(t *testing.T, got, want []string) {
	t.Helper()
	set := make(map[string]bool, len(got))
	for _, v := range got {
		set[v] = true
	}
	for _, v := range want {
		if !set[v] {
			t.Errorf("%q is missing from %v", v, got)
		}
	}
}

// newWiredModule builds an org Module with the wiring Register requires --
// the blind indexer, a sender address and a link builder -- plus whatever the
// caller adds. Every test that bootstraps or registers goes through it, so
// the required-options rule is asserted in exactly one place (see
// TestModule_Register_RefusesAnIndexerlessBoot) rather than re-tested by
// accident everywhere else.
func newWiredModule(t *testing.T, db *gorm.DB, opts ...Option) *Module {
	t.Helper()
	wired := append([]Option{
		WithEmailIndexer(newTestEmailIndexer(t)),
		WithMailFrom(testMailFrom),
		WithInvitationLinkBuilder(testLinkBuilder),
	}, opts...)
	return NewModule(db, wired...)
}

// bootstrapTestModule bootstraps a fully wired org module through the real
// kernel and returns the registry it registered against. Going through
// Bootstrap rather than calling Register on a hand-built Registry is
// deliberate: it is the only path that also merges the locale files and so
// proves they survive i18n.Builder.AddModule's parity validation.
func bootstrapTestModule(t *testing.T, opts ...Option) *pkgcore.ComponentRegistry {
	t.Helper()
	reg, err := componenttest.DeclareModules(newWiredModule(t, nil, opts...))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	return reg
}

// TestModule_Register_RefusesAnIndexerlessBoot pins the config.Attach
// precedent: a module whose declared surface includes an encrypted-and-
// queryable column refuses to boot without the key that makes the column
// queryable, rather than starting and failing on the first write.
func TestModule_Register_RefusesAnIndexerlessBoot(t *testing.T) {
	_, err := componenttest.DeclareModules(NewModule(nil))
	if !apperr.HasCode(err, ErrEmailIndexerRequired.Code) {
		t.Fatalf("Bootstrap without an email indexer error = %v, want org.email_indexer_required", err)
	}
}

// TestModule_Register_RefusesAMailerlessBootWhileTheEmailIsOn pins the other
// boot-time validation: the invitation email is on by default, and it cannot
// be rendered into something a recipient can act on without a sender address
// and a link builder.
func TestModule_Register_RefusesAMailerlessBootWhileTheEmailIsOn(t *testing.T) {
	tests := []struct {
		name string
		opts []Option
	}{
		{"no sender and no link builder", nil},
		{"a sender but no link builder", []Option{WithMailFrom(testMailFrom)}},
		{"a link builder but no sender", []Option{WithInvitationLinkBuilder(testLinkBuilder)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]Option{WithEmailIndexer(newTestEmailIndexer(t))}, tc.opts...)
			_, err := componenttest.DeclareModules(NewModule(nil, opts...))
			if !apperr.HasCode(err, ErrInvitationMailRequired.Code) {
				t.Fatalf("Bootstrap error = %v, want org.invitation_mail_required", err)
			}
		})
	}
}

// TestModule_Register_EmailDisabled_NeedsNoMailWiring pins the escape hatch a
// host uses once something else delivers the invitation: no sender address,
// no link builder, and the boot succeeds.
func TestModule_Register_EmailDisabled_NeedsNoMailWiring(t *testing.T) {
	_, err := componenttest.DeclareModules(NewModule(nil,
			WithEmailIndexer(newTestEmailIndexer(t)),
			WithInvitationEmailDisabled(),
		))
	if err != nil {
		t.Fatalf("Bootstrap with the invitation email disabled: %v", err)
	}
}

// TestModule_Register_DeclaresTheMembershipSurface asserts the declarations
// this block adds arrive on the registry.
func TestModule_Register_DeclaresTheMembershipSurface(t *testing.T) {
	reg := bootstrapTestModule(t)

	wantActions := []string{AuditActionMemberCreate, AuditActionMemberUpdate, AuditActionInvitationCreate, AuditActionInvitationUpdate}
	actions := map[string]bool{}
	for _, action := range reg.AuditActionsSeat().Actions() {
		actions[action] = true
	}
	for _, want := range wantActions {
		if !actions[want] {
			t.Errorf("audit action %q was not registered", want)
		}
	}

	flags := map[string]pkgcore.FeatureFlag{}
	for _, flag := range reg.FeaturesSeat().Flags() {
		flags[flag.Key] = flag
	}
	invitations, ok := flags[FeatureInvitations]
	if !ok || !invitations.Default {
		t.Errorf("feature flag %q = %+v, want it registered and on by default", FeatureInvitations, invitations)
	}
	email, ok := flags[FeatureInvitationEmail]
	if !ok || !email.Default {
		t.Errorf("feature flag %q = %+v, want it registered and on by default", FeatureInvitationEmail, email)
	}
	if len(email.DependsOn) != 1 || email.DependsOn[0] != FeatureInvitations {
		t.Errorf("%q.DependsOn = %v, want [%q]", FeatureInvitationEmail, email.DependsOn, FeatureInvitations)
	}

	events := map[string]bool{}
	for _, decl := range reg.EventsSeat().Published() {
		events[decl.Type] = true
	}
	for _, want := range []string{
		EventNodeCreated, EventNodeMoved, EventNodeDeleted, EventNodeRestored,
		EventMemberInvited, EventMemberJoined, EventMemberRemoved, EventMemberRestored,
	} {
		if !events[want] {
			t.Errorf("event %q was not declared", want)
		}
	}
}

// TestModule_Register_SubscribesToTheAuthnEvent proves the subscription is
// installed on the bus a publisher actually reaches, by publishing the event
// the way authn would and observing the effect.
func TestModule_Register_SubscribesToTheAuthnEvent(t *testing.T) {
	m := newWiredModule(t, newInvitationTestDB(t))
	reg, err := componenttest.DeclareModules(m)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if err := reg.EventBus().Publish(context.Background(), pkgcore.Event{
		Type:     EventUserCreated,
		TenantID: "tenant-a",
		Payload:  map[string]any{"user_id": "u-1"},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	ctx := tenantCtx("tenant-a")
	if _, err := m.Members().Get(ctx, "u-1"); err != nil {
		t.Errorf("the bootstrapped module did not react to %s: %v", EventUserCreated, err)
	}
}

// TestModule_Options pins each option's effect on the runtime it configures.
func TestModule_Options(t *testing.T) {
	indexer := newTestEmailIndexer(t)
	m := NewModule(nil,
		WithEmailIndexer(indexer),
		WithFeatureGate(fixedGate{}),
		WithMaxDepth(3),
		WithInvitationTTL(time.Hour),
		WithMailFrom(testMailFrom),
		WithReplyTo(testMailReplyTo),
		WithInvitationLinkBuilder(testLinkBuilder),
	)
	if m.emailIndexer != indexer || m.invites.indexer != indexer {
		t.Error("WithEmailIndexer did not reach both the module and the invite service")
	}
	if m.invites.gate == nil {
		t.Error("WithFeatureGate did not reach the invite service")
	}
	if m.tree.maxDepth != 3 {
		t.Errorf("tree.maxDepth = %d, want 3", m.tree.maxDepth)
	}
	if m.invites.ttl != time.Hour {
		t.Errorf("invites.ttl = %v, want 1h", m.invites.ttl)
	}
	if m.invites.from != testMailFrom {
		t.Errorf("invites.from = %q, want %q", m.invites.from, testMailFrom)
	}
	if m.invites.replyTo != testMailReplyTo {
		t.Errorf("invites.replyTo = %q, want %q", m.invites.replyTo, testMailReplyTo)
	}

	// Nonsense values are ignored rather than stored: a tree that cannot
	// hold a child is not a tree, and an invitation that expires before it
	// is read is not an invitation.
	def := NewModule(nil, WithMaxDepth(0), WithInvitationTTL(-time.Hour))
	if def.tree.maxDepth != maxDepth {
		t.Errorf("WithMaxDepth(0) changed maxDepth to %d, want the default %d", def.tree.maxDepth, maxDepth)
	}
	if def.invites.ttl != defaultInvitationTTL {
		t.Errorf("WithInvitationTTL(-1h) changed the ttl to %v, want the default %v", def.invites.ttl, defaultInvitationTTL)
	}
}

// TestModule_WithMaxDepth_AboveCeilingIsIgnored pins the upper bound
// WithMaxDepth enforces: a host cannot configure a tree deeper than
// maxDepthCeiling, the deepest depth whose materialized path still fits the
// migrations' "path VARCHAR(1024)" column on both dialects.
//
// Without the bound, WithMaxDepth(maxDepthCeiling+1) (or any higher
// value) would be accepted verbatim, and a tree deep enough to overflow
// that column would fail at the database on PostgreSQL (distributed
// deployment mode) while SQLite's type affinity would let the identical
// operation succeed silently in standalone deployment mode -- the exact
// cross-dialect divergence path.go's own maxDepth bound rules out for the
// UNCONFIGURED default, reopened here through the host override.
func TestModule_WithMaxDepth_AboveCeilingIsIgnored(t *testing.T) {
	tooDeep := NewModule(nil, WithMaxDepth(maxDepthCeiling+1))
	if tooDeep.tree.maxDepth != maxDepth {
		t.Errorf("WithMaxDepth(%d) changed maxDepth to %d, want the default %d (value must be rejected, not clamped or accepted)",
			maxDepthCeiling+1, tooDeep.tree.maxDepth, maxDepth)
	}

	wayTooDeep := NewModule(nil, WithMaxDepth(1000))
	if wayTooDeep.tree.maxDepth != maxDepth {
		t.Errorf("WithMaxDepth(1000) changed maxDepth to %d, want the default %d", wayTooDeep.tree.maxDepth, maxDepth)
	}

	// The ceiling itself must still be accepted -- only what's beyond it is
	// rejected.
	atCeiling := NewModule(nil, WithMaxDepth(maxDepthCeiling))
	if atCeiling.tree.maxDepth != maxDepthCeiling {
		t.Errorf("WithMaxDepth(%d) = %d, want %d accepted verbatim", maxDepthCeiling, atCeiling.tree.maxDepth, maxDepthCeiling)
	}
}

// TestModule_WithMaxDepth_CeilingMatchesPathColumnWidth guards
// maxDepthCeiling (path.go) against drifting from the "path VARCHAR(1024)"
// width the postgres and sqlite migrations actually declare: the constant
// has no compiler-checked link to the SQL files, so a migration width
// change with no matching update to maxDepthCeiling would silently
// reintroduce the divergence WithMaxDepth's ceiling exists to close.
func TestModule_WithMaxDepth_CeilingMatchesPathColumnWidth(t *testing.T) {
	fs := NewModule(nil).Migrations()
	pathColumnRe := regexp.MustCompile(`(?m)^\s*path\s+VARCHAR\((\d+)\)`)

	for _, dialect := range []string{"postgres", "sqlite"} {
		content, err := fs.ReadFile(dialect + "/0001_create_org_nodes.sql")
		if err != nil {
			t.Fatalf("ReadFile(%s/0001_create_org_nodes.sql): %v", dialect, err)
		}
		match := pathColumnRe.FindSubmatch(content)
		if match == nil {
			t.Fatalf("%s migration: no \"path VARCHAR(n)\" column declaration found", dialect)
		}
		width, err := strconv.Atoi(string(match[1]))
		if err != nil {
			t.Fatalf("%s migration: parse path column width %q: %v", dialect, match[1], err)
		}
		if width != pathColumnWidth {
			t.Errorf("%s migration declares path VARCHAR(%d), but path.go's pathColumnWidth = %d -- update pathColumnWidth (and re-derive maxDepthCeiling) to match",
				dialect, width, pathColumnWidth)
		}
	}

	wantCeiling := (pathColumnWidth-1)/idSegmentLen - 1
	if maxDepthCeiling != wantCeiling {
		t.Errorf("maxDepthCeiling = %d, want %d derived from pathColumnWidth=%d and idSegmentLen=%d",
			maxDepthCeiling, wantCeiling, pathColumnWidth, idSegmentLen)
	}
	// And the boundary itself: one level beyond the ceiling must overflow
	// the column, or the ceiling is not actually tight.
	if fits := 1 + idSegmentLen*(maxDepthCeiling+1); fits > pathColumnWidth {
		t.Errorf("a tree at maxDepthCeiling (%d) already needs %d characters, over the %d-character column", maxDepthCeiling, fits, pathColumnWidth)
	}
	if overflows := 1 + idSegmentLen*(maxDepthCeiling+2); overflows <= pathColumnWidth {
		t.Errorf("a tree one level beyond maxDepthCeiling only needs %d characters, still fits the %d-character column -- the ceiling is not tight", overflows, pathColumnWidth)
	}
}

// TestModule_Accessors pins that every runtime a host reaches for is wired,
// and that the tree's member guard is one of them -- without it a cascading
// delete could orphan a membership.
func TestModule_Accessors(t *testing.T) {
	m := NewModule(nil, WithEmailIndexer(newTestEmailIndexer(t)))
	if m.Tree() == nil || m.Members() == nil || m.Invitations() == nil || m.Scope() == nil {
		t.Fatal("NewModule left part of the runtime unwired")
	}
	if m.tree.members == nil {
		t.Error("the tree has no member guard; a cascading delete could orphan a membership")
	}
}

// fixedGate is a FeatureGate that answers the same way for every flag.
type fixedGate struct {
	enabled bool
	err     error
}

func (g fixedGate) IsEnabled(_ context.Context, _ string) (bool, error) {
	return g.enabled, g.err
}

// compile-time check that a plain struct with one stdlib-typed method
// satisfies the seam. This is the whole no-import technique: *config.Service
// satisfies FeatureGate exactly this way, and neither module imports the
// other.
var _ FeatureGate = fixedGate{}

// TestModule_Register_DeclaresItsBootstrapKey pins the one process-start key
// org's contract names: the HMAC key behind the invitation-address blind
// indexer, a Sensitive hex key separate from every cipher key -- and the only
// key org declares, since the runtime schema beside it is deliberately absent.
func TestModule_Register_DeclaresItsBootstrapKey(t *testing.T) {
	declared := component().BootstrapKeys
	if len(declared) != 1 {
		t.Fatalf("the org component declared %d bootstrap keys (%v), want exactly one", len(declared), declared)
	}
	key := declared[0]
	if key.Key != "org.invitation_email_index_key" {
		t.Errorf("declared key = %q, want org.invitation_email_index_key", key.Key)
	}
	if key.Format != "hexkey" || !key.Sensitive {
		t.Errorf("declaration = %+v, want a Sensitive hexkey", key)
	}
	if key.Group != moduleName {
		t.Errorf("declaration group = %q, want the module name %q", key.Group, moduleName)
	}
	if key.Default == "" || key.Description == "" || key.Example != "" {
		t.Errorf("declaration = %+v, want a documented fallback and contract text, and no suggested value", key)
	}
}
