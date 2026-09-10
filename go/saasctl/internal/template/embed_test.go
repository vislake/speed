package template

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/vislake/speed/go/authn"
	"github.com/vislake/speed/go/dbkit"
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite" // registers dbkit.DialectSQLite for dbkit.Open
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pki"
	"github.com/vislake/speed/go/tenancy"
)

// validSelectionKeys is the one enumeration of the five legal selection
// keys, mirrored here and in internal/new's validation (new cannot import
// this package's tests and template cannot import new -- the template
// package is new's dependency, not the other way around). The two sides
// cross-check each other through their shared ground truth: a selection
// key new accepts must exist in the embed tree or new's materialization
// fails, and a directory in the embed tree new does not accept is dead
// weight that TestSelectionDirectoriesMatchValidKeys below refuses. The
// selection-key grammar itself is SelectionKey's: sorted modules joined by
// '+', or "none" for the empty set.
var validSelectionKeys = []string{"authn+org+rbac", "authn+rbac", "authn+org", "authn", "none"}

// TestEmbeddedGoFilesStartWithBuildIgnoreLine walks the whole embedded
// project tree and asserts that every .go file starts with BuildIgnoreLine
// -- the compile-containment convention (A2): the skeleton must never
// compile, vet or lint as part of this module. A template edit that adds a
// .go file without the marker fails here, and independently in new's own
// generator guard (internal/new), so the convention is enforced in two
// places as its doc comment promises.
func TestEmbeddedGoFilesStartWithBuildIgnoreLine(t *testing.T) {
	goFiles := 0
	err := fs.WalkDir(Project, ProjectRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		goFiles++
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			return err
		}
		if !strings.HasPrefix(string(content), BuildIgnoreLine+"\n") {
			t.Errorf("%s: first line is not the build-ignore marker %q", path, BuildIgnoreLine)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded project tree: %v", err)
	}
	// The shared trio plus one server.go per selection: a drift in either
	// direction (a new template file, a removed one) is a real convention
	// change and this count is the tripwire.
	wantGoFiles := 3 + len(validSelectionKeys)
	if goFiles != wantGoFiles {
		t.Errorf("embedded tree holds %d .go files, want %d", goFiles, wantGoFiles)
	}
}

// TestSelectionDirectoriesMatchValidKeys asserts the embed tree holds
// exactly the five legal selection directories -- no more, no fewer, none
// of them files -- and that each holds exactly the two files a selection
// drives: its go.mod.txt (the go.mod document -- the require set, with the
// seeds pruned by the development-proof tidy run -- stored under the inert
// .txt name because go:embed refuses to descend into a subdirectory
// containing a go.mod, and renamed by new at materialization) and its
// server.go (the import, module and middleware set).
func TestSelectionDirectoriesMatchValidKeys(t *testing.T) {
	entries, err := fs.ReadDir(Project, ProjectRoot+"/selection")
	if err != nil {
		t.Fatalf("read selection directory: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			got[e.Name()] = true
			inner, err := fs.ReadDir(Project, ProjectRoot+"/selection/"+e.Name())
			if err != nil {
				t.Fatalf("read selection %s: %v", e.Name(), err)
			}
			want := map[string]bool{"go.mod.txt": true, "server.go": true}
			for _, f := range inner {
				if f.IsDir() {
					t.Errorf("selection %s holds a nested directory %s, want only go.mod.txt and server.go", e.Name(), f.Name())
					continue
				}
				if !want[f.Name()] {
					t.Errorf("selection %s holds unexpected file %s, want only go.mod.txt and server.go", e.Name(), f.Name())
				}
			}
		} else {
			t.Errorf("project/selection holds a file %s, want only selection directories", e.Name())
		}
	}
	for _, key := range validSelectionKeys {
		if !got[key] {
			t.Errorf("missing selection directory for valid key %s", key)
		}
	}
	for key := range got {
		found := false
		for _, valid := range validSelectionKeys {
			if key == valid {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("selection directory %s is not one of the valid keys %v", key, validSelectionKeys)
		}
	}
}

// TestSharedFilesPresent asserts every file SharedFiles names exists in
// the embed tree -- the shared set materializes verbatim in every
// selection, so a missing entry would fail every `saasctl new` run.
func TestSharedFilesPresent(t *testing.T) {
	for _, name := range SharedFiles {
		content, err := fs.ReadFile(Project, ProjectRoot+"/"+name)
		if err != nil {
			t.Errorf("shared file %s: %v", name, err)
			continue
		}
		if strings.HasSuffix(name, ".go") {
			if !strings.HasPrefix(string(content), BuildIgnoreLine+"\n") {
				t.Errorf("shared .go file %s does not start with the build-ignore marker", name)
			}
		}
	}
}

// TestProjectReadmeNamesTheShippedMaintenanceCommands asserts the generated
// project's README -- a shared file, materialized verbatim into every
// selection -- documents the maintenance commands that are wired today
// (`saasctl upgrade`, `saasctl db migrate`, `saasctl config print`) and
// presents nothing that shipped only later as available now. The README is
// the generated project's first documentation, so a lifecycle claim that
// drifted from the CLI's real surface would mislead every consumer project
// that reads it.
func TestProjectReadmeNamesTheShippedMaintenanceCommands(t *testing.T) {
	content, err := fs.ReadFile(Project, ProjectRoot+"/README.md")
	if err != nil {
		t.Fatalf("read project README: %v", err)
	}
	readme := string(content)
	const section = "## Editing and regenerating"
	start := strings.Index(readme, section)
	if start < 0 {
		t.Fatalf("README has no %q section", section)
	}
	body := readme[start:]
	if end := strings.Index(body, "\n## "); end >= 0 {
		body = body[:end]
	}
	// Fold the prose's line wrapping: a command name split across two
	// wrapped lines must still count as present.
	body = strings.Join(strings.Fields(body), " ")
	for _, cmd := range []string{"saasctl upgrade", "saasctl db migrate", "saasctl config print"} {
		if !strings.Contains(body, cmd) {
			t.Errorf("%s section does not name the shipped command %q", section, cmd)
		}
	}
	// The stale wording this pins against presented upgrade, db and config
	// as "later rounds" and claimed config inspects its dynamic
	// configuration; config print renders the bootstrap environment only,
	// and dynamic-configuration print is a separate, unimplemented
	// surface.
	for _, stale := range []string{"inspects its dynamic configuration", "`saasctl db` runs", "`saasctl config` inspects"} {
		if strings.Contains(body, stale) {
			t.Errorf("%s section still carries the stale claim %q", section, stale)
		}
	}
}

// TestSelectionGoModsCarryTokens asserts each selection's go.mod document
// (the go.mod.txt asset, renamed by new when it materializes) is a
// token-carrying template, never a materialized artifact: the module line
// still names TokenAppName, every replace directive still points at
// TokenSpeedRoot (a relative path -- an absolute leaked path would mean a
// development-proof golden was committed without its tokens converted
// back), and no replace ever points outside the speed module graph.
func TestSelectionGoModsCarryTokens(t *testing.T) {
	for _, key := range validSelectionKeys {
		path := ProjectRoot + "/selection/" + key + "/go.mod.txt"
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		mod := string(content)
		if !strings.HasPrefix(mod, "module "+TokenAppName+"\n") {
			t.Errorf("%s: module line does not name the app token, first line %q", path, firstLine(mod))
		}
		if !strings.Contains(mod, TokenSpeedRoot) {
			t.Errorf("%s: no replace points at the speed-root token", path)
		}
		if strings.Contains(mod, "=> /") {
			t.Errorf("%s: a replace directive carries an absolute path (tokens not converted back?)", path)
		}
		for _, line := range strings.Split(mod, "\n") {
			if !strings.HasPrefix(line, "replace ") {
				continue
			}
			if !strings.Contains(line, "=> "+TokenSpeedRoot+"/go/") {
				t.Errorf("%s: replace directive %q does not point at the speed module graph", path, line)
			}
		}
	}
}

// TestSelectionServerGoMatchesSelectionKey cross-checks each selection's
// server.go against its own key: the module constructors it calls, the
// middleware pieces it composes and the Bootstrap argument order are the
// selection's whole meaning (A5), so a template edit that lets the file
// and its directory drift apart fails here even though the file itself --
// build-ignored -- would never fail a compile.
func TestSelectionServerGoMatchesSelectionKey(t *testing.T) {
	expected := []struct {
		key              string
		modules          []string // in Bootstrap argument order
		contains, absent []string
	}{
		{
			key:      "authn+org+rbac",
			modules:  []string{"pkiModule", "authnModule", "orgModule", "configModule", "rbacModule"},
			contains: []string{"authn.NewModule(", "org.NewModule(", "rbac.NewModule(", "pki.NewModule(", "authn.NewPrincipalResolver()", "authnModule.Service().Verifier()"},
		},
		{
			key:      "authn+rbac",
			modules:  []string{"pkiModule", "authnModule", "configModule", "rbacModule"},
			contains: []string{"authn.NewModule(", "rbac.NewModule(", "pki.NewModule(", "authn.NewPrincipalResolver()", "authnModule.Service().Verifier()"},
			absent:   []string{"org.NewModule("},
		},
		{
			key:      "authn+org",
			modules:  []string{"pkiModule", "authnModule", "orgModule", "configModule"},
			contains: []string{"authn.NewModule(", "org.NewModule(", "pki.NewModule(", "authn.NewPrincipalResolver()", "authnModule.Service().Verifier()"},
			absent:   []string{"rbac.NewModule("},
		},
		{
			key:      "authn",
			modules:  []string{"pkiModule", "authnModule", "configModule"},
			contains: []string{"authn.NewModule(", "pki.NewModule(", "authn.NewPrincipalResolver()", "authnModule.Service().Verifier()"},
			absent:   []string{"org.NewModule(", "rbac.NewModule("},
		},
		{
			key:      "none",
			modules:  []string{"configModule"},
			contains: []string{"config.NewModule("},
			absent: []string{
				"authn.NewModule(", "org.NewModule(", "rbac.NewModule(",
				"authn.Middleware(", "tenancy.Middleware(", "authn.NewPrincipalResolver()",
				"authnAPIPath", "authnPreAuthAllowlist", "RegisterPIISerializer", "devSigningKeySeed",
			},
		},
	}
	for _, want := range expected {
		path := ProjectRoot + "/selection/" + want.key + "/server.go"
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		server := string(content)
		// Bootstrap's module argument list is the composition's skeleton;
		// assert the exact order as one line so a reordering that changes
		// migration-vs-registration semantics cannot pass silently.
		bootstrapLine := "Bootstrap(ctx, " + strings.Join(want.modules, ", ") + ")"
		if !strings.Contains(server, bootstrapLine) {
			t.Errorf("%s: missing Bootstrap call %q", path, bootstrapLine)
		}
		migrationList := "[]pkgcore.Module{" + strings.Join(want.modules, ", ") + "}"
		if !strings.Contains(server, migrationList) {
			t.Errorf("%s: missing migration registration list %q", path, migrationList)
		}
		for _, s := range want.contains {
			if !strings.Contains(server, s) {
				t.Errorf("%s: selection %s should contain %q", path, want.key, s)
			}
		}
		for _, s := range want.absent {
			if strings.Contains(server, s) {
				t.Errorf("%s: selection %s must not contain %q", path, want.key, s)
			}
		}
	}
}

// TestAuthnSelectionsExemptAuthnSubtreeByStructure is the structural half
// of the P1 regression: an anonymous enterprise-OIDC authorize/callback
// request (provider "oidc:<tenant>", a per-tenant name no allowlist can
// enumerate) must reach authn's own handler instead of being refused 403
// tenancy.tenant_unresolved by tenancy.Middleware. The exemption must be
// STRUCTURAL -- every route under authn's API path mounted ahead of
// tenancy.Middleware -- never an enumerated allowlist, which is exactly
// the fixed-channel enumeration that failed: each authn selection's
// server.go must mount the authn subtree on topMux directly behind
// authn.Middleware (topMux.Handle(hostcore.AuthnAPIPath, ...) with the
// tenancy
// chain as the "/" fallback), route-split in mountModuleRoutes by path
// prefix, and must NOT carry a pre-auth allowlist for authn paths at all
// -- no authnPreAuthAllowlist function, no per-provider social entries,
// no register/login literals next to tenancy.WithAllowlist. A template
// edit that reintroduces the enumeration (or drops the structural
// dispatch) fails here before any generated project inherits the bug.
func TestAuthnSelectionsExemptAuthnSubtreeByStructure(t *testing.T) {
	for key, mountCall := range map[string]string{
		"authn+org+rbac": "mountModuleRoutes(authnMux, moduleMux, reg, rbacService)",
		"authn+rbac":     "mountModuleRoutes(authnMux, moduleMux, reg, rbacService)",
		"authn+org":      "mountModuleRoutes(authnMux, moduleMux, reg)",
		"authn":          "mountModuleRoutes(authnMux, moduleMux, reg)",
	} {
		path := ProjectRoot + "/selection/" + key + "/server.go"
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		server := string(content)
		for _, want := range []string{
			"pkgcore.MountRoutes(topMux, pkgcore.MountedRoute{Path: hostcore.AuthnAPIPath, Handler: authnMux})",
			mountCall,
			"strings.HasPrefix(route.Path, hostcore.AuthnAPIPath)",
			"handler := authn.Middleware(authnModule.Service().Verifier())(topMux)",
		} {
			if !strings.Contains(server, want) {
				t.Errorf("%s: missing the structural exemption marker %q", path, want)
			}
		}
		// The fixed enumeration must be gone: no pre-auth allowlist
		// function, no allowlist entry naming an authn path or provider.
		for _, stale := range []string{
			"authnPreAuthAllowlist", "ProviderDingTalk", "ProviderFeishu",
			`tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/register")`,
			`tenancy.WithAllowlist(http.MethodGet, authnAPIPath+"/social/`,
		} {
			if strings.Contains(server, stale) {
				t.Errorf("%s: the fixed pre-auth enumeration survived: %q must not appear", path, stale)
			}
		}
	}
}

// TestRBACSelectionsAdoptTheRouteTable pins the adoption of the platform's
// route-authorization mechanism where the skeleton used to leave the gate
// table to the owner: each rbac-bearing selection must declare its routes
// through rbac.GuardRoutes over a routeRules table (so a mounted path with
// no declared decision fails the build), mark the platform's pre-auth
// surfaces public explicitly, and gate the modules that perform no
// permission check of their own; the three selections without rbac must not
// reference the mechanism at all (they carry no rbac dependency to gate
// with).
func TestRBACSelectionsAdoptTheRouteTable(t *testing.T) {
	for _, key := range []string{"authn+org+rbac", "authn+rbac"} {
		content, err := fs.ReadFile(Project, ProjectRoot+"/selection/"+key+"/server.go")
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		server := string(content)
		for _, want := range []string{
			"rbac.GuardRoutes(az, reg.Routes.Routes(), routeRules())",
			"func routeRules() []rbac.RouteRule {",
			"{Path: hostcore.AuthnAPIPath, Access: pkgcore.RouteAccess{Public: true}}",
			"{Path: config.PathPublic, Access: pkgcore.RouteAccess{Public: true}}",
			"{Path: config.PathSystemFeatures, Access: pkgcore.RouteAccess{Public: true}}",
			"{Path: pkiAPIPath, Access: pkgcore.RouteAccess{Permission: pkiPermissionFor}}",
		} {
			if !strings.Contains(server, want) {
				t.Errorf("%s: the route table adoption marker %q is missing from the template", key, want)
			}
		}
	}

	org, err := fs.ReadFile(Project, ProjectRoot+"/selection/authn+org+rbac/server.go")
	if err != nil {
		t.Fatalf("authn+org+rbac: %v", err)
	}
	for _, want := range []string{
		"Access: pkgcore.RouteAccess{Permission: orgPermissionFor}",
		"Exempt: orgAcceptInvitationRequest,",
		"return org.PermissionRemoveMember",
		"return org.PermissionInviteMember",
	} {
		if !strings.Contains(string(org), want) {
			t.Errorf("authn+org+rbac: the org entry marker %q is missing from the template", want)
		}
	}

	for _, key := range []string{"authn+org", "authn", "none"} {
		content, err := fs.ReadFile(Project, ProjectRoot+"/selection/"+key+"/server.go")
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		if strings.Contains(string(content), "rbac.GuardRoutes") {
			t.Errorf("%s: a selection without the rbac module references rbac.GuardRoutes", key)
		}
	}
}

// TestSelectionKey pins SelectionKey's rendering: the canonical sorted
// '+'-joined form of a validated --with set, "none" for the empty set, and
// a caller's input slice left untouched (the caller may reuse it).
func TestSelectionKey(t *testing.T) {
	tests := []struct {
		name string
		with []string
		want string
	}{
		{name: "empty", with: []string{}, want: "none"},
		{name: "already sorted", with: []string{"authn", "rbac", "org"}, want: "authn+org+rbac"},
		{name: "reverse sorted input", with: []string{"org", "rbac", "authn"}, want: "authn+org+rbac"},
		{name: "pair", with: []string{"rbac", "authn"}, want: "authn+rbac"},
		{name: "single", with: []string{"org"}, want: "org"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input := append([]string(nil), tt.with...)
			if got := SelectionKey(tt.with); got != tt.want {
				t.Errorf("SelectionKey(%v) = %q, want %q", tt.with, got, tt.want)
			}
			for i := range input {
				if input[i] != tt.with[i] {
					t.Errorf("SelectionKey mutated its input: element %d changed from %q to %q", i, tt.with[i], input[i])
					break
				}
			}
		})
	}
}

// TestStripBuildIgnoreRoundTrip asserts the template .go layout Strip
// depends on -- marker line, one blank line, then content -- and that
// stripping is byte-exact against that layout: Strip must reproduce the
// template-minus-marker document, which is what new's materialized
// project is compared against, so the two cannot drift. A template edit
// that changes the mandated layout (double blank line, no blank line, a
// marker that moved) fails here first.
func TestStripBuildIgnoreRoundTrip(t *testing.T) {
	prefix := []byte(BuildIgnoreLine + "\n\n")
	err := fs.WalkDir(Project, ProjectRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			return err
		}
		if !bytes.HasPrefix(content, prefix) {
			t.Errorf("%s: does not open with the marker and one blank line", path)
			return nil
		}
		stripped, err := StripBuildIgnore(content)
		if err != nil {
			t.Errorf("%s: StripBuildIgnore: %v", path, err)
			return nil
		}
		if want := bytes.TrimPrefix(content, prefix); !bytes.Equal(stripped, want) {
			t.Errorf("%s: StripBuildIgnore is not byte-identical to template-minus-marker", path)
		}
		if bytes.HasPrefix(stripped, []byte("\n")) {
			t.Errorf("%s: stripped file still starts with a blank line", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded project tree: %v", err)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestAuthnSelectionsConsumeTheThreeKeyMaterialsFromServerConfig pins the
// server side of the key-material contract: every authn-wiring selection's
// server.go must build the blind index, the PII cipher and the pki
// local-key cipher from the cfg fields config.go resolves
// (APP_AUTHN_BLIND_INDEX_KEY / APP_AUTHN_PII_CIPHER_KEY /
// APP_PKI_LOCAL_KEY_CIPHER_KEY, each with its dev fallback), never from
// bare dev constants used unconditionally -- an operator who sets all the
// APP_* key variables must not still be running on committed public key
// bytes. No selection may carry the harm-amplifying claim that such a
// constant is "the same documented trade-off as config.go's devConfigKey"
// (false: devConfigKey is a fallback behind an env override, a bare
// constant has no override at all). The "none" selection wires no authn,
// so it must carry none of the three usages. The config.go half of the
// contract (the env declarations, the parse blocks and the dev fallbacks)
// is pinned by appconfig's own twin tests, which re-read that file.
func TestAuthnSelectionsConsumeTheThreeKeyMaterialsFromServerConfig(t *testing.T) {
	authnKeys := []string{
		"dbkit.NewCipher(cfg.AuthnPIICipherKey)",
		"dbkit.NewCipher(cfg.PKILocalKeyCipherKey)",
		"authn.WithBlindIndexKey(cfg.AuthnBlindIndexKey)",
	}
	banished := []string{
		"dbkit.NewCipher(devPIICipherKey)",
		"dbkit.NewCipher(devPKILocalKeyCipherKey)",
		"authn.WithBlindIndexKey(devBlindIndexKey)",
		"same documented trade-off as config.go's devConfigKey",
	}
	for _, key := range validSelectionKeys {
		path := ProjectRoot + "/selection/" + key + "/server.go"
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := string(content)
		if key == "none" {
			for _, want := range authnKeys {
				if strings.Contains(src, want) {
					t.Errorf("%s (the authn-less selection) references %s; no authn module is wired there, so no key material may be consumed", key, want)
				}
			}
			continue
		}
		for _, want := range authnKeys {
			if !strings.Contains(src, want) {
				t.Errorf("%s does not consume key material through %s; the skeleton must use the cfg-resolved key (env override with dev fallback), never a bare dev constant", key, want)
			}
		}
		for _, banned := range banished {
			if strings.Contains(src, banned) {
				t.Errorf("%s still carries %q; unconditional dev-constant use (or its harm-amplifier justification) must be gone", key, banned)
			}
		}
	}
}

// TestOrgSelectionsBuildTheInvitationIndexerOverEmailIndexColumn pins how
// every org-wiring selection binds org's encrypted invitation column: the
// serializer through org's own registrar and the indexer through org's own
// constructor, so neither the GORM serializer name nor the index column
// crosses the wiring as a hand-typed string (org's own doc comments give
// the reason: dbkit refuses an EMPTY column name but has no guard for a
// non-empty wrong one, so the exact SQL name must stay owned by the package
// that pins it to the schema). The template source itself is what gets
// pinned here: these build-ignored files never compile in this repository,
// so a selection that drifted back to constructing the indexer by hand
// would ship into every consumer project `saasctl new` materializes before
// anything anywhere failed. The selections that wire no org module must
// mention neither call.
func TestOrgSelectionsBuildTheInvitationIndexerOverEmailIndexColumn(t *testing.T) {
	const wantRegistrar = "org.RegisterEmailSerializer(cipher)"
	const wantConstructor = "org.NewEmailIndexer(cfg.OrgIndexKey)"
	const stale = "dbkit.NewBlindIndexer("
	for _, key := range validSelectionKeys {
		path := ProjectRoot + "/selection/" + key + "/server.go"
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		src := string(content)
		switch key {
		case "authn+org", "authn+org+rbac":
			if !strings.Contains(src, wantRegistrar) || !strings.Contains(src, wantConstructor) {
				t.Errorf("%s does not bind org's encrypted column through org's own registrar and indexer constructor", key)
			}
			if strings.Contains(src, stale) {
				t.Errorf("%s still constructs a blind indexer over dbkit directly; go/org owns its index column", key)
			}
		default:
			if strings.Contains(src, wantRegistrar) || strings.Contains(src, wantConstructor) {
				t.Errorf("%s wires no org module, so its server.go must register no org serializer and build no org indexer", key)
			}
		}
	}
}

// TestPreauthExemption_DrivesTheGeneratedProjectShape is the behavioral
// half of the P1 regression: an anonymous enterprise-OIDC authorize
// request -- provider "oidc:acme", the dynamic per-tenant name authn
// derives from authn.ProviderOIDCPrefix + a tenant id, which no fixed
// allowlist can enumerate -- must reach authn's own handler instead of
// being refused 403 tenancy.tenant_unresolved before it. The test
// composes the real authn, pki and tenancy packages in exactly the shape
// the generated server.go templates produce (the structural twin,
// TestAuthnSelectionsExemptAuthnSubtreeByStructure, pins those templates
// to this shape byte-level): authn's own subtree mounted on topMux
// directly behind authn.Middleware, tenancy.Middleware wrapping only the
// other routes. Authn answers a provider it has never seen with its own
// coded refusal -- 400 authn.provider_unknown -- which is the proof the
// request crossed the tenancy layer: the allowlist shape (asserted by the legacyShape leg below)
// answered this exact request 403 tenancy.tenant_unresolved.
func TestPreauthExemption_DrivesTheGeneratedProjectShape(t *testing.T) {
	handler := buildComposedHandler(t, composedShapeNew)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/authn/social/oidc:acme/authorize?redirect_uri=https%3A%2F%2Fapp.example%2Fcb", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET oidc:acme authorize = %d %s; authn's provider-unknown refusal is 400 (any non-403 answer that reaches authn proves the exemption; the pre-fix answer was the tenancy 403)", rec.Code, rec.Body.String())
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != "authn.provider_unknown" {
		t.Fatalf("authorize body = %q, want the authn.provider_unknown envelope (code %q, err %v)", rec.Body.String(), body.Code, err)
	}
}

// TestPreauthExemption_CallbackAlsoReachesAuthn drives the callback half
// of the same pair. An anonymous POST with no usable state answers authn's
// own refusal once it reaches the handler; the assertion that matters is
// the negative one -- never the tenancy 403 the allowlist shape produced
// for this path.
func TestPreauthExemption_CallbackAlsoReachesAuthn(t *testing.T) {
	handler := buildComposedHandler(t, composedShapeNew)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/authn/social/oidc:acme/callback", strings.NewReader(`{"code":"c","state":"s"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusForbidden && strings.Contains(rec.Body.String(), "tenancy.tenant_unresolved") {
		t.Fatalf("oidc callback refused by tenancy: %d %s; the callback must reach authn's own handler", rec.Code, rec.Body.String())
	}
}

// TestPreauthExemption_LegacyAllowlistShapeRefusesTheSameRequest is the
// anchor that shows why the structural exemption exists: compose the
// allowlist shape -- the whole mux, authn subtree included, wrapped in
// tenancy.Middleware with a fixed allowlist enumerating the built-in
// social channels -- and the identical anonymous oidc:acme authorize
// request is refused 403 tenancy.tenant_unresolved, because no fixed
// enumeration can contain a per-tenant provider name. This leg is the
// negative control: if it started passing, the tenancy layer would no
// longer refuse unlisted anonymous pairs and the structural exemption
// would be moot; if the current shape ever changed into the allowlist
// one, the first test would fail.
func TestPreauthExemption_LegacyAllowlistShapeRefusesTheSameRequest(t *testing.T) {
	handler := buildComposedHandler(t, composedShapeLegacy)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/authn/social/oidc:acme/authorize", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "tenancy.tenant_unresolved") {
		t.Fatalf("legacy allowlist shape answered %d %s; want the 403 tenancy.tenant_unresolved the fixed enumeration produced for an unenumerable provider", rec.Code, rec.Body.String())
	}
}

// composedShape selects which generated-server shape the composed handler
// mirrors: the structural exemption (new) or the fixed allowlist (legacy).
type composedShape int

const (
	composedShapeNew composedShape = iota
	composedShapeLegacy
)

// buildComposedHandler assembles a REAL authn module (with pki as its
// KeySource, over a real SQLite file) and composes the HTTP chain exactly
// as the generated server.go does under the requested shape. The authn
// handler is reached the way the generated code reaches it: through
// reg.Routes after Kernel.Bootstrap registered the module.
func buildComposedHandler(t *testing.T, shape composedShape) http.Handler {
	t.Helper()
	ctx := context.Background()
	gdb, err := dbkit.Open(ctx, dbkit.Options{Dialect: dbkit.DialectSQLite, DSN: t.TempDir() + "/preauth.db"})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, closeErr := gdb.DB(); closeErr == nil {
			_ = sqlDB.Close()
		}
	})

	pkiModule := pki.NewModule(gdb)
	blindIndexKey := make([]byte, 32)
	authnModule, err := authn.NewModule(gdb, authn.WithKeySource(pkiModule.Service()), authn.WithBlindIndexKey(blindIndexKey))
	if err != nil {
		t.Fatalf("authn.NewModule: %v", err)
	}
	reg, err := pkgcore.NewKernel(pkgcore.WithDeploymentMode(pkgcore.DeploymentModeStandalone)).
		Bootstrap(ctx, pkiModule, authnModule)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	// The generated mountModuleRoutes split and the topMux dispatch,
	// spelled as the templates spell it -- pkgcore.MountRoutes is the
	// mounting rule both sides share.
	const authnAPIPath = "/api/v1/authn"
	authnMux := http.NewServeMux()
	protectedMux := http.NewServeMux()
	for _, route := range reg.Routes.Routes() {
		target := protectedMux
		if strings.HasPrefix(route.Path, authnAPIPath) {
			target = authnMux
		}
		pkgcore.MountRoutes(target, route)
	}

	if shape == composedShapeLegacy {
		// The allowlist shape: every route -- authn's subtree included --
		// behind tenancy.Middleware, with authn's pre-auth operations
		// allowlisted one (method, path) pair at a time, the built-in
		// social channels enumerated by hand.
		allMux := http.NewServeMux()
		for _, route := range reg.Routes.Routes() {
			allMux.Handle(route.Path, route.Handler)
			if !strings.HasSuffix(route.Path, "/") {
				allMux.Handle(route.Path+"/", route.Handler)
			}
		}
		opts := []tenancy.MiddlewareOption{
			tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/register"),
			tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/login/password"),
			tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/login/sms/request"),
			tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/login/sms"),
			tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/token/refresh"),
		}
		for _, provider := range []string{
			authn.ProviderGoogle, authn.ProviderGitHub, authn.ProviderWeChat,
			authn.ProviderDingTalk, authn.ProviderFeishu,
		} {
			opts = append(opts,
				tenancy.WithAllowlist(http.MethodGet, authnAPIPath+"/social/"+provider+"/authorize"),
				tenancy.WithAllowlist(http.MethodPost, authnAPIPath+"/social/"+provider+"/callback"),
			)
		}
		return authn.Middleware(authnModule.Service().Verifier())(
			tenancy.Middleware(authn.NewPrincipalResolver(), opts...)(allMux))
	}

	topMux := http.NewServeMux()
	pkgcore.MountRoutes(topMux, pkgcore.MountedRoute{Path: authnAPIPath, Handler: authnMux})
	topMux.Handle("/", tenancy.Middleware(authn.NewPrincipalResolver(),
		tenancy.WithAllowlist(http.MethodGet, "/healthz"),
		tenancy.WithAllowlist(http.MethodHead, "/healthz"),
	)(protectedMux))
	return authn.Middleware(authnModule.Service().Verifier())(topMux)
}

// TestPreauthExemption_ComposedShapeIsTheTemplatesOwn pins the test's
// composition constants to the template files it claims to mirror: if the
// generated server.go templates compose differently (a renamed constant,
// a different dispatch), the behavior test would be testing a shape the
// templates do not produce -- this twin assertion closes that gap by
// requiring the templates to carry the very markers this test's
// composition is built from.
func TestPreauthExemption_ComposedShapeIsTheTemplatesOwn(t *testing.T) {
	for _, key := range []string{"authn+org+rbac", "authn+rbac", "authn+org", "authn"} {
		content, err := fs.ReadFile(Project, ProjectRoot+"/selection/"+key+"/server.go")
		if err != nil {
			t.Errorf("%s: %v", key, err)
			continue
		}
		server := string(content)
		for _, marker := range []string{
			"pkgcore.MountRoutes(topMux, pkgcore.MountedRoute{Path: hostcore.AuthnAPIPath, Handler: authnMux})",
			"handler := authn.Middleware(authnModule.Service().Verifier())(topMux)",
		} {
			if !strings.Contains(server, marker) {
				t.Errorf("%s: the composed-shape twin marker %q is missing from the template", key, marker)
			}
		}
	}
}

// The tests below pin the generated project README against the template
// it ships beside: the golden/embed tests elsewhere in this package check
// the template tree's structure, and these tests check that the README's
// own PROSE agrees with the template's own source. Each test extracts
// ground truth directly from the template source text (never a
// hand-copied literal these tests could themselves fall behind) and
// compares it against the README.

// readmeContent reads the embedded, shared project README once per test.
func readmeContent(t *testing.T) string {
	t.Helper()
	content, err := fs.ReadFile(Project, ProjectRoot+"/README.md")
	if err != nil {
		t.Fatalf("read project README: %v", err)
	}
	return string(content)
}

// goDirectivePattern matches a go.mod's "go X.Y.Z" directive line.
var goDirectivePattern = regexp.MustCompile(`(?m)^go (\d+\.\d+\.\d+)$`)

// TestReadmeGoVersionMatchesGoModTxt: the "Go X.Y.Z or newer" prerequisite
// line in the README must name the exact version every selection's own
// go.mod.txt declares in its "go" directive -- not a stale figure from an
// earlier toolchain line. The check runs against every one of the five
// legal selections (README's own claim is selection-independent), so a
// selection whose go.mod.txt drifts from the others is caught too.
func TestReadmeGoVersionMatchesGoModTxt(t *testing.T) {
	readme := readmeContent(t)
	readmeVersionPattern := regexp.MustCompile(`Go (\d+\.\d+\.\d+) or newer`)
	m := readmeVersionPattern.FindStringSubmatch(readme)
	if m == nil {
		t.Fatal(`README does not state a "Go X.Y.Z or newer" prerequisite; the version-parity check has nothing to compare against`)
	}
	readmeVersion := m[1]

	for _, key := range validSelectionKeys {
		path := ProjectRoot + "/selection/" + key + "/go.mod.txt"
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		directive := goDirectivePattern.FindStringSubmatch(string(content))
		if directive == nil {
			t.Errorf("%s: no \"go X.Y.Z\" directive found", path)
			continue
		}
		if directive[1] != readmeVersion {
			t.Errorf("%s declares go %s but the README's Prerequisites section states Go %s or newer; the two have drifted",
				path, directive[1], readmeVersion)
		}
	}
}

// envVarBacktickPattern matches a backtick-quoted, all-uppercase (with
// digits and underscores) token in the README -- the exact shape every one
// of the twenty-two bootstrap environment variable names takes when the
// README refers to it (e.g. APP_S3_ENDPOINT or PORT, each wrapped in a
// pair of backticks).
var envVarBacktickPattern = regexp.MustCompile("`([A-Z][A-Z0-9_]*)`")

// configGoEnvVarPattern matches one `config:"env=<NAME>"` struct-tag
// option in the template's config.go -- the loader target's env tag is
// where each bootstrap variable's name lives (the names are pinned, not
// derived) -- deliberately duplicated from the identical pattern in
// internal/appconfig/appconfig_test.go's own drift-proof test rather than
// shared, since the two packages check two different kinds of drift (that
// one checks the Go twin's parse behavior, this one checks the README's
// prose) and neither should import the other's test helpers to do it.
var configGoEnvVarPattern = regexp.MustCompile(`config:"env=([A-Za-z0-9_]+)"`)

// readEnvVarNamesFromConfigGo extracts the set of environment variable
// names the embedded template's cmd/server/config.go actually parses.
func readEnvVarNamesFromConfigGo(t *testing.T) map[string]bool {
	t.Helper()
	content, err := fs.ReadFile(Project, ProjectRoot+"/cmd/server/config.go")
	if err != nil {
		t.Fatalf("read the embedded template config.go: %v", err)
	}
	names := map[string]bool{}
	for _, m := range configGoEnvVarPattern.FindAllStringSubmatch(string(content), -1) {
		names[m[1]] = true
	}
	if len(names) == 0 {
		t.Fatal("extracted zero environment variable names from config.go; the extraction pattern itself has drifted")
	}
	return names
}

// TestReadmeEnvTableMatchesConfigGoExactly is the env-var consistency
// test: every environment variable the template's config.go actually
// parses must be documented in the README (this README is the consumer's
// first-run manual, so an undocumented variable is a gap), and every
// backtick-quoted, all-caps token the README's Bootstrap Environment
// section presents as a variable must be one config.go genuinely parses
// -- never a stale or invented name.
func TestReadmeEnvTableMatchesConfigGoExactly(t *testing.T) {
	configVars := readEnvVarNamesFromConfigGo(t)
	readme := readmeContent(t)

	readmeVars := map[string]bool{}
	for _, m := range envVarBacktickPattern.FindAllStringSubmatch(readme, -1) {
		readmeVars[m[1]] = true
	}

	for name := range configVars {
		if !readmeVars[name] {
			t.Errorf("config.go parses %s but the README never mentions it; this README is the consumer's first-run manual and must document it", name)
		}
	}
	for name := range readmeVars {
		if !configVars[name] {
			t.Errorf("README names %s as a bootstrap variable but config.go does not parse it; the README has drifted ahead of (or never matched) the template", name)
		}
	}
}

// devKeyBacktickPattern matches a backtick-quoted "dev*" identifier in the
// README -- the shape every committed development key placeholder takes
// when the README names it (e.g. devConfigKey or devPKILocalKeyCipherKey,
// each wrapped in a pair of backticks).
var devKeyBacktickPattern = regexp.MustCompile("`(dev[A-Za-z0-9]+)`")

// devKeyDeclPattern matches one "dev<Name> = " assignment -- a var
// declaration's own opening line, whether config.go's own standalone
// "var dev<Name> = []byte{" form or an aligned one inside a var (...)
// block with no per-line "var" keyword (each selection's server.go).
var devKeyDeclPattern = regexp.MustCompile(`(?m)^\s*(?:var\s+)?(dev[A-Za-z0-9]+)\s*=`)

// TestReadmeNamesOnlyRealDevKeyIdentifiers is the stale-name guard: it
// collects every "dev*" identifier actually declared across the shared
// config.go and every selection's own server.go, then asserts the README's
// own "dev*" references are a subset of that real set -- a
// named-identifier check, not a single-string grep, so any identifier the
// README names that no generated file declares (a deleted one such as
// devSigningKeySeed, say) fails the test, naming the orphan.
func TestReadmeNamesOnlyRealDevKeyIdentifiers(t *testing.T) {
	real := map[string]bool{}
	collect := func(path string) {
		content, err := fs.ReadFile(Project, path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range devKeyDeclPattern.FindAllStringSubmatch(string(content), -1) {
			real[m[1]] = true
		}
	}
	collect(ProjectRoot + "/cmd/server/config.go")
	for _, key := range validSelectionKeys {
		collect(ProjectRoot + "/selection/" + key + "/server.go")
	}
	if len(real) == 0 {
		t.Fatal("extracted zero dev* key identifiers from the template; the extraction pattern itself has drifted")
	}

	readme := readmeContent(t)
	referenced := map[string]bool{}
	for _, m := range devKeyBacktickPattern.FindAllStringSubmatch(readme, -1) {
		referenced[m[1]] = true
	}
	if len(referenced) == 0 {
		t.Fatal("README names zero dev* key identifiers; the reference extraction pattern itself has drifted")
	}
	for name := range referenced {
		if !real[name] {
			t.Errorf("README names %s but no template file declares it; the README refers to an identifier that does not exist in the generated project", name)
		}
	}
}

// TestReadmeEditingSectionStillNamesTheShippedCommands is a narrow
// regression guard for TestProjectReadmeNamesTheShippedMaintenanceCommands
// in embed_test.go: the "Editing and regenerating" section's own
// [redacted]-secrets sentence must still name every secret-shaped print
// row (the five key variables plus the two infrastructure credentials),
// so the README's own description of `saasctl config print` stays
// truthful once the command renders more than five lines.
func TestReadmeEditingSectionStillNamesTheShippedCommands(t *testing.T) {
	readme := readmeContent(t)
	const section = "## Editing and regenerating"
	start := strings.Index(readme, section)
	if start < 0 {
		t.Fatalf("README has no %q section", section)
	}
	body := strings.Join(strings.Fields(readme[start:]), " ")
	for _, want := range []string{"S3 secret key", "SMTP password"} {
		if !strings.Contains(body, want) {
			t.Errorf("%s section's config-print description does not mention %q; it will misrepresent which rows are redacted", section, want)
		}
	}
}
