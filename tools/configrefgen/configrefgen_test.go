package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/vislake/speed/go/config"
	"github.com/vislake/speed/go/pkgcore"

	// loader is go/pkgcore/config: the package whose own EnvName rule the
	// rendered env column must reproduce, so the test compares the artifacts
	// against the loader rather than against a second copy of the rule.
	loader "github.com/vislake/speed/go/pkgcore/config"
)

// repoRootFromTest locates the repository root above the package directory
// (configrefgen sits two levels deep: configrefgen -> tools -> root).
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
	if cfg.DBPath != "app.db" {
		t.Errorf("DBPath = %q, want the file's app.db", cfg.DBPath)
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
		t.Errorf("%s is not the derivation of %s; run go run . from tools/configrefgen/", configExampleJSONPath, configExampleYAMLPath)
	}
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

// TestRenderedDeclaredKeysMatchTheCensus is the platform surface's drift gate
// in both directions, run over the rendered artifacts themselves: every key the
// composed modules declare appears in the Markdown, the site page and the JSON,
// and no key appears there that the census does not declare -- so neither a
// rendering that drops a declaration nor an artifact carrying an undeclared key
// passes. (The committed bytes' freshness is --check's separate job; this gate
// is about the correspondence, which no byte comparison can see.)
func TestRenderedDeclaredKeysMatchTheCensus(t *testing.T) {
	svc, declared := composeHost(t)
	doc, err := buildDocument(svc.Describe(), declared)
	if err != nil {
		t.Fatalf("buildDocument: %v", err)
	}

	want := make(map[string]bool, len(declared))
	for _, key := range declared {
		want[key.Key] = true
	}
	if len(want) == 0 {
		t.Fatal("the composed host declared no bootstrap keys; the census is this gate's other side")
	}

	for _, rendered := range []struct {
		name string
		text string
	}{
		{"docs/config-reference.md", doc.renderMarkdown()},
		{sitePagePath, doc.sitePage()},
	} {
		got := bootstrapSectionKeys(rendered.text)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s renders the bootstrap keys %v, want exactly the declared %v", rendered.name, sortedKeys(got), sortedKeys(want))
		}
	}

	var payload struct {
		BootstrapKeys []struct {
			Keys []struct {
				Key string `json:"key"`
			} `json:"keys"`
		} `json:"bootstrap_keys"`
	}
	if err := json.Unmarshal([]byte(doc.marshalJSON()), &payload); err != nil {
		t.Fatalf("unmarshal the JSON twin: %v", err)
	}
	got := make(map[string]bool)
	for _, module := range payload.BootstrapKeys {
		for _, key := range module.Keys {
			if got[key.Key] {
				t.Errorf("docs/config-reference.json carries %s more than once", key.Key)
			}
			got[key.Key] = true
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the JSON twin carries the bootstrap keys %v, want exactly the declared %v", sortedKeys(got), sortedKeys(want))
	}
}

// TestRenderedEnvNamesMatchTheLoaderRule pins the Env variable column's
// content in every rendering against the loader's own derivation rule: each
// row's cell -- the Markdown table's own column, the site page's copy of it,
// and the JSON twin's env field -- must be exactly
// loader.EnvName(loader.EnvPrefix, the row's key path). The column tells an
// operator which variable a stock loader reads for a declared key; a
// re-derived spelling here would print a name the loader never reads, which
// is the drift this test exists to catch.
func TestRenderedEnvNamesMatchTheLoaderRule(t *testing.T) {
	svc, declared := composeHost(t)
	doc, err := buildDocument(svc.Describe(), declared)
	if err != nil {
		t.Fatalf("buildDocument: %v", err)
	}

	want := make(map[string]string, len(declared))
	for _, key := range declared {
		want[key.Key] = loader.EnvName(loader.EnvPrefix, key.Key)
	}
	if len(want) == 0 {
		t.Fatal("the composed host declared no bootstrap keys; the loader rule is this gate's other side")
	}

	for _, rendered := range []struct {
		name string
		text string
	}{
		{"docs/config-reference.md", doc.renderMarkdown()},
		{sitePagePath, doc.sitePage()},
	} {
		got := bootstrapSectionEnvCells(rendered.text)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s renders the env names %v, want the loader rule's %v", rendered.name, got, want)
		}
	}

	var payload struct {
		BootstrapKeys []struct {
			Keys []struct {
				Key string `json:"key"`
				Env string `json:"env"`
			} `json:"keys"`
		} `json:"bootstrap_keys"`
	}
	if err := json.Unmarshal([]byte(doc.marshalJSON()), &payload); err != nil {
		t.Fatalf("unmarshal the JSON twin: %v", err)
	}
	got := make(map[string]string, len(declared))
	for _, module := range payload.BootstrapKeys {
		for _, key := range module.Keys {
			got[key.Key] = key.Env
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the JSON twin carries the env names %v, want the loader rule's %v", got, want)
	}
}

// bootstrapSection narrows a rendered page to its bootstrap section: from the
// bootstrap heading (inclusive) to the dynamic heading (exclusive), the region
// both bootstrap-section collectors read.
func bootstrapSection(rendered string) string {
	section := rendered
	if i := strings.Index(section, "## Bootstrap configuration"); i >= 0 {
		section = section[i:]
	}
	if i := strings.Index(section, "## Dynamic configuration"); i >= 0 {
		section = section[:i]
	}
	return section
}

// bootstrapSectionKeys collects the key-path tokens of the bootstrap section's
// rendered table rows: the first backticked cell of every "| `…` |" line.
func bootstrapSectionKeys(rendered string) map[string]bool {
	keys := map[string]bool{}
	for _, line := range strings.Split(bootstrapSection(rendered), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		if key, _, ok := strings.Cut(strings.TrimPrefix(line, "| `"), "`"); ok && key != "" {
			keys[key] = true
		}
	}
	return keys
}

// bootstrapSectionEnvCells collects the first two backticked cells of every
// bootstrap-section table row -- the key path and the env name -- so the
// rendered column can be compared with the loader rule cell by cell.
func bootstrapSectionEnvCells(rendered string) map[string]string {
	cells := map[string]string{}
	for _, line := range strings.Split(bootstrapSection(rendered), "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		key, rest, ok := strings.Cut(strings.TrimPrefix(line, "| `"), "`")
		if !ok || key == "" {
			continue
		}
		if env, _, ok := strings.Cut(strings.TrimPrefix(rest, " | `"), "`"); ok {
			cells[key] = env
		}
	}
	return cells
}

func sortedKeys(keys map[string]bool) []string {
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// TestOutputsAreDeterministic boots the schema host twice and pins that the
// rendered document is byte-identical across runs -- the property the drift
// gate relies on (a nondeterministic generator could never gate anything).
func TestOutputsAreDeterministic(t *testing.T) {
	root := repoRootFromTest(t)

	svc, declared := composeHost(t)
	doc, err := buildDocument(svc.Describe(), declared)
	if err != nil {
		t.Fatalf("buildDocument: %v", err)
	}
	first, err := renderOutputs(root, doc)
	if err != nil {
		t.Fatalf("renderOutputs: %v", err)
	}

	svc2, declared2 := composeHost(t)
	doc2, err := buildDocument(svc2.Describe(), declared2)
	if err != nil {
		t.Fatalf("second buildDocument: %v", err)
	}
	second, err := renderOutputs(root, doc2)
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
	svc, declared := composeHost(t)
	doc, err := buildDocument(svc.Describe(), declared)
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
// module whose prefix its key path carries.
func TestDeclaredModulesCoverTheCensus(t *testing.T) {
	declared := composeDeclarations(t)

	modules := declaredKeys(declared)
	seen := 0
	for _, module := range modules {
		for _, key := range module.Keys {
			seen++
			if !strings.HasPrefix(key.Key, module.Name+".") {
				t.Errorf("key %s is grouped under %s, but does not carry that module's prefix", key.Key, module.Name)
			}
		}
	}
	if seen != len(declared) {
		t.Errorf("the rendering covers %d declared keys, the census has %d", seen, len(declared))
	}
}

// TestJSONTwinShapeIsPinned pins the restructure the JSON twin carries: the
// top level holds exactly the two sections, and every bootstrap row holds
// exactly the six declared fields. DisallowUnknownFields is the regression
// latch for the fields this shape retired (a row's own module, a dangling
// example), and the key-set comparison is the other direction -- a dropped
// field fails as loudly as an unexpected one.
func TestJSONTwinShapeIsPinned(t *testing.T) {
	svc, declared := composeHost(t)
	doc, err := buildDocument(svc.Describe(), declared)
	if err != nil {
		t.Fatalf("buildDocument: %v", err)
	}
	raw := doc.marshalJSON()

	var topLevel map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &topLevel); err != nil {
		t.Fatalf("unmarshal the JSON twin's top level: %v", err)
	}
	if len(topLevel) != 2 {
		t.Fatalf("the JSON twin's top level carries %d keys (%v), want exactly the two sections", len(topLevel), sortedRawKeys(topLevel))
	}
	for _, section := range []string{"bootstrap_keys", "dynamic_items"} {
		if _, ok := topLevel[section]; !ok {
			t.Fatalf("the JSON twin's top level is missing %q (carries %v)", section, sortedRawKeys(topLevel))
		}
	}

	// The strict decode: any unknown field anywhere in the structures below
	// (a top-level map key, a bootstrap group key, a bootstrap row key) fails.
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var payload struct {
		BootstrapKeys []struct {
			Module string `json:"module"`
			Keys   []struct {
				Key         string `json:"key"`
				Env         string `json:"env"`
				Format      string `json:"format"`
				Default     string `json:"default"`
				Sensitive   bool   `json:"sensitive"`
				Description string `json:"description"`
			} `json:"keys"`
		} `json:"bootstrap_keys"`
		DynamicItems []json.RawMessage `json:"dynamic_items"`
	}
	if err := dec.Decode(&payload); err != nil {
		t.Fatalf("strict decode of the JSON twin (an unknown field means a retired one crept back): %v", err)
	}
	totalRows := 0
	for _, group := range payload.BootstrapKeys {
		totalRows += len(group.Keys)
	}
	if totalRows != len(declared) {
		t.Fatalf("the JSON twin renders %d bootstrap rows, the census declares %d", totalRows, len(declared))
	}

	// The exact field set per row, from the raw bytes.
	var grouped struct {
		BootstrapKeys []struct {
			Keys []map[string]json.RawMessage `json:"keys"`
		} `json:"bootstrap_keys"`
	}
	if err := json.Unmarshal([]byte(raw), &grouped); err != nil {
		t.Fatalf("unmarshal the bootstrap groups: %v", err)
	}
	wantFields := map[string]bool{"key": true, "env": true, "format": true, "default": true, "sensitive": true, "description": true}
	rows := 0
	for _, module := range grouped.BootstrapKeys {
		for _, row := range module.Keys {
			rows++
			if len(row) != len(wantFields) {
				t.Fatalf("bootstrap row %v carries %d fields, want exactly %d (%v)", sortedRawKeys(row), len(row), len(wantFields), sortedRawKeys(wantFields))
			}
			for field := range wantFields {
				if _, ok := row[field]; !ok {
					t.Fatalf("bootstrap row %v is missing field %q", sortedRawKeys(row), field)
				}
			}
		}
	}
	if rows != len(declared) {
		t.Fatalf("the row walk covered %d bootstrap rows, the census declares %d", rows, len(declared))
	}
}

func sortedRawKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// composeHost boots the composed schema host and returns the snapshot's parts,
// failing the test on any error.
func composeHost(t *testing.T) (*config.Service, []pkgcore.BootstrapKey) {
	t.Helper()
	snapshot, err := schemaHost(context.Background())
	if err != nil {
		t.Fatalf("schemaHost: %v", err)
	}
	return snapshot.service, snapshot.declaredKeys
}

// composeDeclarations boots the composed host and returns only the bootstrap
// census the reconciliation tests need.
func composeDeclarations(t *testing.T) []pkgcore.BootstrapKey {
	t.Helper()
	_, declared := composeHost(t)
	return declared
}
