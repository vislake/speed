package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/pkgcore"
)

// repoRootFromTest locates the repository root above the package directory
// (cmd/configrefgen sits five levels deep: configrefgen -> cmd ->
// reference-app -> examples -> root).
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root not found above the test's working directory")
		}
		dir = parent
	}
}

// TestConfigExampleYAMLLoadsThroughTheLoader is the load-verification of
// the repository-root config.example.yaml: the file must genuinely parse
// through go/pkgcore/config's loader against the loader-shaped target
// struct this package carries, with the documented precedence chain (flags
// > env > file > struct defaults) pinned on the real file's own keys.
func TestConfigExampleYAMLLoadsThroughTheLoader(t *testing.T) {
	root := repoRootFromTest(t)
	path := filepath.Join(root, "config.example.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config.example.yaml missing at repository root: %v", err)
	}

	// The file alone: every key the file supplies resolves, the keys it
	// leaves unset keep the struct defaults.
	cfg, err := loadConfigExample(path)
	if err != nil {
		t.Fatalf("loading config.example.yaml through the loader failed: %v", err)
	}
	if cfg.DeploymentMode != "standalone" {
		t.Errorf("DeploymentMode = %q, want the file's standalone", cfg.DeploymentMode)
	}
	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want the struct default 8080 (file spelling must decode)", cfg.Port)
	}
	if cfg.DBPath != "reference-app.db" {
		t.Errorf("DBPath = %q, want the file's reference-app.db", cfg.DBPath)
	}

	// Environment beats the file: SPEED_DEPLOYMENTMODE and SPEED_SMTP__PORT
	// outrank the file's own values for the same keys.
	cfg = exampleBootstrapDefaults()
	loader := configLoaderFor(path, nil, []string{
		"SPEED_DEPLOYMENTMODE=distributed",
		"SPEED_SMTP__PORT=25",
	})
	if err := loader.Load(&cfg); err != nil {
		t.Fatalf("loader with env failed: %v", err)
	}
	if cfg.DeploymentMode != "distributed" {
		t.Errorf("DeploymentMode = %q, want the environment's distributed (env must outrank the file)", cfg.DeploymentMode)
	}
	if cfg.SMTP.Port != 25 {
		t.Errorf("SMTP.Port = %d, want 25 from SPEED_SMTP__PORT", cfg.SMTP.Port)
	}

	// Flags beat the environment: --deploymentmode=standalone outranks the
	// SPEED_DEPLOYMENTMODE=distributed env above.
	cfg = exampleBootstrapDefaults()
	loader = configLoaderFor(path, []string{"--deploymentmode=standalone"}, []string{
		"SPEED_DEPLOYMENTMODE=distributed",
	})
	if err := loader.Load(&cfg); err != nil {
		t.Fatalf("loader with flag failed: %v", err)
	}
	if cfg.DeploymentMode != "standalone" {
		t.Errorf("DeploymentMode = %q, want the flag's standalone (flags must outrank the environment)", cfg.DeploymentMode)
	}
}

// TestConfigExampleJSONLoadsThroughTheLoader is the JSON half of the
// load-verification: the loader's file source accepts YAML or JSON, so the
// committed JSON counterpart must load through the same real loader and
// resolve to exactly what the YAML file resolves to. The two files share one
// source (the JSON is derived from the YAML), so this test pins both that the
// derivation is loadable as JSON and that the pair agrees key for key.
func TestConfigExampleJSONLoadsThroughTheLoader(t *testing.T) {
	root := repoRootFromTest(t)
	jsonPath := filepath.Join(root, configExampleJSONPath)
	if _, err := os.Stat(jsonPath); err != nil {
		t.Fatalf("%s missing at repository root: %v", configExampleJSONPath, err)
	}

	fromJSON, err := loadConfigExample(jsonPath)
	if err != nil {
		t.Fatalf("loading %s through the loader failed: %v", configExampleJSONPath, err)
	}
	fromYAML, err := loadConfigExample(filepath.Join(root, configExampleYAMLPath))
	if err != nil {
		t.Fatalf("loading %s through the loader failed: %v", configExampleYAMLPath, err)
	}
	if fromJSON != fromYAML {
		t.Errorf("the JSON example resolves to %+v, the YAML example to %+v; the pair must agree", fromJSON, fromYAML)
	}

	// The JSON file is derived, never hand-written: a fresh derivation of the
	// committed YAML must reproduce the committed bytes exactly.
	derived, err := deriveConfigExampleJSON(root)
	if err != nil {
		t.Fatalf("deriveConfigExampleJSON: %v", err)
	}
	committed, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read %s: %v", configExampleJSONPath, err)
	}
	if string(committed) != derived {
		t.Errorf("%s is not the derivation of %s; run go run ./cmd/configrefgen from examples/reference-app/", configExampleJSONPath, configExampleYAMLPath)
	}
}

// TestBootstrapInventoryAndTableAgree runs the coverage gate the generator
// itself runs: every env variable the app source reads has a curated table
// row and vice versa. A new variable added to internal/app without a table
// row fails here before the drift gate ever runs.
func TestBootstrapInventoryAndTableAgree(t *testing.T) {
	root := repoRootFromTest(t)
	moduleDir := filepath.Join(root, "examples", "reference-app")
	if _, problems, err := bootstrapRows(moduleDir); err != nil {
		t.Fatalf("bootstrapRows: %v", err)
	} else if len(problems) > 0 {
		t.Fatalf("bootstrap inventory/table mismatch:\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestPlatformEnvNamesBridgeIsBijective pins the transition's bridge against
// the declarations the composed host actually produces: every declared key is
// bridged to exactly one variable, every bridge entry names a declared key, and
// the pair's facts agree -- the reconciliation the generator runs before it
// renders anything.
func TestPlatformEnvNamesBridgeIsBijective(t *testing.T) {
	declared, _ := composeDeclarations(t)

	problems := reconcileDeclarations(declared)
	if len(problems) > 0 {
		t.Fatalf("the declarations and the bootstrap table disagree:\n  %s", strings.Join(problems, "\n  "))
	}

	declaredKeys := make(map[string]struct{}, len(declared))
	for _, key := range declared {
		declaredKeys[key.Key] = struct{}{}
	}
	for key, env := range platformEnvNames {
		if _, ok := declaredKeys[key]; !ok {
			t.Errorf("platformEnvNames bridges %s, which no composed module declares", key)
		}
		if _, ok := bootstrapTable[env]; !ok {
			t.Errorf("platformEnvNames bridges %s to %s, which has no bootstrap table row", key, env)
		}
	}
	if len(declaredKeys) != len(platformEnvNames) {
		t.Errorf("the composed modules declared %d keys, the bridge maps %d; the two must cover the same set", len(declaredKeys), len(platformEnvNames))
	}
}

// TestReconcileDeclarations_CatchesDrift is the negative control for the gate
// above: a declaration whose facts no longer match its curated row must be
// reported rather than rendered, in both directions.
func TestReconcileDeclarations_CatchesDrift(t *testing.T) {
	declared, _ := composeDeclarations(t)

	t.Run("a misdeclared fact", func(t *testing.T) {
		drifted := append([]pkgcore.BootstrapKey(nil), declared...)
		drifted[0].Format = "string"
		problems := reconcileDeclarations(drifted)
		if len(problems) == 0 {
			t.Fatal("reconcileDeclarations accepted a declaration whose format contradicts its curated row")
		}
		if !strings.Contains(strings.Join(problems, "\n"), drifted[0].Key) {
			t.Errorf("problems = %v, want them to name %s", problems, drifted[0].Key)
		}
	})

	t.Run("a declaration with no bridged variable", func(t *testing.T) {
		extra := append(append([]pkgcore.BootstrapKey(nil), declared...), pkgcore.BootstrapKey{
			Key:         "authn.something_new",
			Format:      "string",
			Description: "a key nothing bridges to a variable",
			Group:       "authn",
		})
		problems := reconcileDeclarations(extra)
		if len(problems) == 0 {
			t.Fatal("reconcileDeclarations accepted a declared key with no bridged variable")
		}
		for _, want := range []string{"authn.something_new", "platformEnvNames"} {
			if !strings.Contains(strings.Join(problems, "\n"), want) {
				t.Errorf("problems = %v, want them to mention %s", problems, want)
			}
		}
	})

	t.Run("a declaration attributed to the wrong module", func(t *testing.T) {
		misgrouped := append([]pkgcore.BootstrapKey(nil), declared...)
		misgrouped[0].Group = "elsewhere"
		problems := reconcileDeclarations(misgrouped)
		if len(problems) == 0 {
			t.Fatal("reconcileDeclarations accepted a declaration whose group is not its key's module")
		}
	})
}

// TestOverlappingKeys_RefusesOneKeyOnTwoLayers pins the second machine
// defence: a bootstrap key that is also a runtime configuration item would mean
// two things at once, so the generator must refuse to render it.
func TestOverlappingKeys_RefusesOneKeyOnTwoLayers(t *testing.T) {
	declared := []pkgcore.BootstrapKey{{
		Key:         "authn.password_min_length",
		Format:      "int",
		Description: "the runtime item's identifier, declared on the wrong layer",
		Group:       "authn",
	}}
	descriptors := []config.ConfigItemDescriptor{{Key: "authn.password_min_length"}}
	if got := overlappingKeys(descriptors, declared); len(got) != 1 || got[0] != "authn.password_min_length" {
		t.Errorf("overlappingKeys() = %v, want the one doubly declared key", got)
	}
	if got := overlappingKeys(nil, declared); len(got) != 0 {
		t.Errorf("overlappingKeys(nil, ...) = %v, want no overlap against an empty schema", got)
	}
}

// TestSitePageTargetsTheUserGuideArea pins where the generated site page lands:
// the site's user-guide reference area, beside the error-code index's own
// generated page, in the language whose content the generator writes.
func TestSitePageTargetsTheUserGuideArea(t *testing.T) {
	if want := "docs/site/content.en/docs/user-guide/configuration.md"; sitePagePath != want {
		t.Errorf("sitePagePath = %q, want %q", sitePagePath, want)
	}
}

// TestEnvExampleRendersEveryKeyOnce pins the generated .env.example shape:
// every curated row renders exactly one assignment line naming the row's
// own variable, so the committed carrier can never silently drop a key the
// app reads.
func TestEnvExampleRendersEveryKeyOnce(t *testing.T) {
	rows := make([]bootstrapVar, 0, len(bootstrapTable))
	for env, row := range bootstrapTable {
		row.env = env
		rows = append(rows, row)
	}
	rendered := renderEnvExample(rows)
	for _, row := range rows {
		line := "\n" + row.env + "="
		if !strings.Contains(rendered, line) {
			t.Errorf(".env.example rendering is missing the %s assignment", row.env)
		}
		if strings.Count(rendered, line) != 1 {
			t.Errorf(".env.example rendering assigns %s more than once", row.env)
		}
	}
}

// TestOutputsAreDeterministic boots the schema host twice and pins that the
// rendered document is byte-identical across runs -- the property the drift
// gate relies on (a nondeterministic generator could never gate anything).
func TestOutputsAreDeterministic(t *testing.T) {
	root := repoRootFromTest(t)
	moduleDir := filepath.Join(root, "examples", "reference-app")

	svc, declared, composed := composeHost(t)
	doc, boot, err := buildDocument(moduleDir, svc.Describe(), declared, composed)
	if err != nil {
		t.Fatalf("buildDocument: %v", err)
	}
	first, err := renderOutputs(root, doc, boot)
	if err != nil {
		t.Fatalf("renderOutputs: %v", err)
	}

	svc2, declared2, composed2 := composeHost(t)
	doc2, boot2, err := buildDocument(moduleDir, svc2.Describe(), declared2, composed2)
	if err != nil {
		t.Fatalf("second buildDocument: %v", err)
	}
	second, err := renderOutputs(root, doc2, boot2)
	if err != nil {
		t.Fatalf("second renderOutputs: %v", err)
	}

	if len(first) != len(second) {
		t.Fatalf("output sets differ in size: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].path != second[i].path {
			t.Fatalf("output %d is %s in one run and %s in the other", i, first[i].path, second[i].path)
		}
		if first[i].content != second[i].content {
			t.Errorf("output %s is not deterministic across runs", first[i].path)
		}
	}
}

// TestSitePageCarriesFrontMatter pins the two things the site build needs from
// the generated page: Hugo front matter as the first bytes, and the tables the
// reference renders, so the page is the reference rather than a pointer to it.
func TestSitePageCarriesFrontMatter(t *testing.T) {
	root := repoRootFromTest(t)
	moduleDir := filepath.Join(root, "examples", "reference-app")
	svc, declared, composed := composeHost(t)
	doc, _, err := buildDocument(moduleDir, svc.Describe(), declared, composed)
	if err != nil {
		t.Fatalf("buildDocument: %v", err)
	}

	page := doc.sitePage()
	if !strings.HasPrefix(page, "---\n") {
		t.Fatalf("the site page does not start with Hugo front matter:\n%.60s", page)
	}
	for _, want := range []string{
		"title:",
		"## Bootstrap configuration",
		"### Bootstrap keys declared by platform modules",
		"## Dynamic configuration",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the site page is missing %q", want)
		}
	}
}

// TestDeclaredModulesCoverTheCensus pins the per-module rendering against the
// declarations themselves: every declared key appears exactly once, under the
// module its key names, and the modules that declared nothing are reported
// rather than omitted.
func TestDeclaredModulesCoverTheCensus(t *testing.T) {
	declared, composed := composeDeclarations(t)

	modules, silent := declaredKeys(declared, composed)
	seen := 0
	for _, module := range modules {
		for _, key := range module.Keys {
			seen++
			if module.Name != key.Module {
				t.Errorf("key %s is grouped under %s, want its own module %s", key.Key, module.Name, key.Module)
			}
			if !strings.HasPrefix(key.Key, module.Name+".") {
				t.Errorf("key %s is grouped under %s, but does not carry that module's prefix", key.Key, module.Name)
			}
		}
	}
	if seen != len(declared) {
		t.Errorf("the rendering covers %d declared keys, the census has %d", seen, len(declared))
	}
	if len(silent) == 0 {
		t.Error("no module was reported as declaring no bootstrap keys; the composed set always has some")
	}
	for _, name := range silent {
		for _, module := range modules {
			if module.Name == name {
				t.Errorf("module %s is reported as silent and as declaring keys", name)
			}
		}
	}
}

// composeHost boots the composed schema host and returns the snapshot's parts,
// failing the test on any error.
func composeHost(t *testing.T) (*config.Service, []pkgcore.BootstrapKey, []string) {
	t.Helper()
	snapshot, err := schemaHost(context.Background())
	if err != nil {
		t.Fatalf("schemaHost: %v", err)
	}
	return snapshot.service, snapshot.declaredKeys, snapshot.composedModules
}

// composeDeclarations boots the composed host and returns only the bootstrap
// census the reconciliation tests need.
func composeDeclarations(t *testing.T) ([]pkgcore.BootstrapKey, []string) {
	t.Helper()
	_, declared, composed := composeHost(t)
	return declared, composed
}
