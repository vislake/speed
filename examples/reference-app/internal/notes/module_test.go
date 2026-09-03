package notes

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestModule_Name(t *testing.T) {
	m := NewModule(nil)
	if got, want := m.Name(), "notes"; got != want {
		t.Fatalf("Name() = %q, want %q", got, want)
	}
}

func TestModule_DependsOn_IsEmpty(t *testing.T) {
	m := NewModule(nil)
	if deps := m.DependsOn(); len(deps) != 0 {
		t.Fatalf("DependsOn() = %v, want empty -- notes depends on infrastructure only, and no other business module exists yet to depend on", deps)
	}
}

func TestModule_Migrations_ContainsBothDialects(t *testing.T) {
	m := NewModule(nil)
	migrationsFS := m.Migrations()

	for _, dialect := range []string{"postgres", "sqlite"} {
		entries, err := fs.ReadDir(migrationsFS, dialect)
		if err != nil {
			t.Fatalf("read %s dir: %v", dialect, err)
		}
		if len(entries) == 0 {
			t.Fatalf("%s migrations directory is empty", dialect)
		}
		if !strings.HasSuffix(entries[0].Name(), ".sql") {
			t.Fatalf("%s/%s is not a .sql file", dialect, entries[0].Name())
		}
	}
}

func TestModule_Locales_ContainsBothLanguages(t *testing.T) {
	m := NewModule(nil)
	localesFS := m.Locales()

	for _, lang := range []string{"zh-CN.toml", "en-US.toml"} {
		content, err := fs.ReadFile(localesFS, lang)
		if err != nil {
			t.Fatalf("read %s: %v", lang, err)
		}
		if !strings.Contains(string(content), "notes.text_required") {
			t.Fatalf("%s does not contain the notes.text_required key", lang)
		}
	}
}

// TestModule_Locales_ContainsEveryHandlerErrorCode guards against the gap
// TestModule_Locales_ContainsBothLanguages cannot see: that test only
// proves the zh-CN and en-US key *sets* match each other, so it stays green
// even when both files are equally incomplete relative to what Handler
// actually returns. handler.go's NotesCreateNote and NotesListNotes can
// return four distinct apperr codes -- ErrTextRequired, ErrTextTooLong,
// errInternal, and the apperr.Invalid("notes.invalid_request_body") call
// inlined in NotesCreateNote (kept as a literal here, not a named var,
// because handler.go itself never names it either) -- and each one must
// resolve to real text in both locale files, per root CLAUDE.md's
// internationalization rule ("New text must ship with both zh-CN and en-US
// resources") and backend coding standard §12.
func TestModule_Locales_ContainsEveryHandlerErrorCode(t *testing.T) {
	m := NewModule(nil)
	localesFS := m.Locales()

	codes := []string{
		ErrTextRequired.Code,
		ErrTextTooLong.Code,
		errInternal.Code,
		"notes.invalid_request_body",
	}

	for _, lang := range []string{"zh-CN.toml", "en-US.toml"} {
		content, err := fs.ReadFile(localesFS, lang)
		if err != nil {
			t.Fatalf("read %s: %v", lang, err)
		}
		for _, code := range codes {
			// The quoted-key form, not a bare substring match, so a
			// mention of the code in this file's header comment could
			// never make the check pass without a real "key" = "value"
			// entry backing it.
			wantEntry := `"` + code + `" =`
			if !strings.Contains(string(content), wantEntry) {
				t.Errorf("%s has no locale entry for error code %q (want a line containing %s)", lang, code, wantEntry)
			}
		}
	}
}

func TestModule_OpenAPISpec_IsNonEmptyAndDescribesNotesPath(t *testing.T) {
	m := NewModule(nil)
	spec := string(m.OpenAPISpec())

	if spec == "" {
		t.Fatal("OpenAPISpec() returned empty bytes")
	}
	if !strings.Contains(spec, "openapi:") {
		t.Fatal("OpenAPISpec() does not look like an OpenAPI document (no 'openapi:' key)")
	}
	if !strings.Contains(spec, apiPath) {
		t.Fatalf("OpenAPISpec() does not mention %q", apiPath)
	}
}

// TestModule_Register_DeclaresRoutesPermissionsEventsAndAuditActions
// exercises every registrar surface Register touches (root task's explicit
// goal: "genuinely exercise the registry surface rather than only its
// minimum"), not only reg.Routes.Mount.
func TestModule_Register_DeclaresRoutesPermissionsEventsAndAuditActions(t *testing.T) {
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	m := NewModule(nil)

	if err := m.Register(reg); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	routes := reg.Routes.Routes()
	if len(routes) != 1 {
		t.Fatalf("got %d mounted routes, want 1", len(routes))
	}
	if routes[0].Path != apiPath {
		t.Fatalf("mounted route Path = %q, want %q", routes[0].Path, apiPath)
	}
	if routes[0].Handler == nil {
		t.Fatal("mounted route Handler is nil")
	}

	perms := reg.Permissions.Permissions()
	wantPerms := map[string]bool{PermissionRead: false, PermissionWrite: false}
	for _, p := range perms {
		if _, ok := wantPerms[p]; ok {
			wantPerms[p] = true
		}
	}
	for p, found := range wantPerms {
		if !found {
			t.Errorf("permission %q was not registered (got %v)", p, perms)
		}
	}

	published := reg.Events.Published()
	if len(published) != 1 {
		t.Fatalf("got %d published event declarations, want 1", len(published))
	}
	if published[0].Type != EventNoteCreated {
		t.Errorf("published event Type = %q, want %q", published[0].Type, EventNoteCreated)
	}
	if published[0].PayloadType != eventNoteCreatedPayloadType {
		t.Errorf("published event PayloadType = %q, want %q", published[0].PayloadType, eventNoteCreatedPayloadType)
	}

	actions := reg.AuditActions.Actions()
	found := false
	for _, a := range actions {
		if a == AuditActionNoteCreate {
			found = true
		}
	}
	if !found {
		t.Errorf("audit action %q was not registered (got %v)", AuditActionNoteCreate, actions)
	}
}

// compile-time check that *Module satisfies pkgcore.Module -- redundant
// with module.go's own assertion, kept here too as a visible part of this
// file's own test surface.
var _ pkgcore.Module = (*Module)(nil)

// TestModule_Register_DeclaresConfigSchemaAndFeatureFlags pins the schema
// this module registers for the config module to freeze at Attach
// (go/config/module.go's Attach doc comment): two configuration items with
// the exact Public/Sensitive shape cmd/server's public endpoint depends on
// (brand.site_name served unauthenticated, support.reply_email never), and
// two feature flags whose DependsOn chain (premium_upsell on smile_preview)
// exercises config's dependency resolution. The registrars validate on Add,
// so Register failing here would already be caught by the other Register
// test; this test proves the *declarations*, the meaning the reference
// app's own endpoints will serve.
func TestModule_Register_DeclaresConfigSchemaAndFeatureFlags(t *testing.T) {
	reg := pkgcore.NewRegistry(pkgcore.NewMemoryEventBus(), pkgcore.NewMemoryKVStore(), pkgcore.NewConsoleMailer())
	m := NewModule(nil)

	if err := m.Register(reg); err != nil {
		t.Fatalf("Register() error = %v", err)
	}

	items := reg.Config.Items()
	var siteName, replyEmail *pkgcore.ConfigItem
	for i := range items {
		switch items[i].Key {
		case ConfigKeyBrandSiteName:
			siteName = &items[i]
		case ConfigKeySupportReplyEmail:
			replyEmail = &items[i]
		}
	}
	if siteName == nil {
		t.Fatalf("config item %q was not registered (got %v)", ConfigKeyBrandSiteName, items)
	}
	if !siteName.Public {
		t.Errorf("config item %q Public = false, want true -- the public endpoint may only serve explicitly public items", ConfigKeyBrandSiteName)
	}
	if siteName.Default != "Smile Studio" {
		t.Errorf("config item %q Default = %q, want %q", ConfigKeyBrandSiteName, siteName.Default, "Smile Studio")
	}
	if siteName.Type != "string" {
		t.Errorf("config item %q Type = %q, want %q", ConfigKeyBrandSiteName, siteName.Type, "string")
	}
	if replyEmail == nil {
		t.Fatalf("config item %q was not registered (got %v)", ConfigKeySupportReplyEmail, items)
	}
	if !replyEmail.Sensitive {
		t.Errorf("config item %q Sensitive = false, want true -- a mail configuration must be encrypted at rest and withheld from the public endpoint", ConfigKeySupportReplyEmail)
	}

	flags := reg.Features.Flags()
	var smilePreview, premiumUpsell *pkgcore.FeatureFlag
	for i := range flags {
		switch flags[i].Key {
		case FeatureFlagSmilePreview:
			smilePreview = &flags[i]
		case FeatureFlagPremiumUpsell:
			premiumUpsell = &flags[i]
		}
	}
	if smilePreview == nil {
		t.Fatalf("feature flag %q was not registered (got %v)", FeatureFlagSmilePreview, flags)
	}
	if smilePreview.Default {
		t.Errorf("feature flag %q Default = true, want false", FeatureFlagSmilePreview)
	}
	if premiumUpsell == nil {
		t.Fatalf("feature flag %q was not registered (got %v)", FeatureFlagPremiumUpsell, flags)
	}
	if !premiumUpsell.Default {
		t.Errorf("feature flag %q Default = false, want true", FeatureFlagPremiumUpsell)
	}
	if len(premiumUpsell.DependsOn) != 1 || premiumUpsell.DependsOn[0] != FeatureFlagSmilePreview {
		t.Errorf("feature flag %q DependsOn = %v, want [%q]", FeatureFlagPremiumUpsell, premiumUpsell.DependsOn, FeatureFlagSmilePreview)
	}
}
