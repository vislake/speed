package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	svc, err := schemaHost(context.Background())
	if err != nil {
		t.Fatalf("schemaHost: %v", err)
	}
	doc, boot, err := buildDocument(moduleDir, svc.Describe())
	if err != nil {
		t.Fatalf("buildDocument: %v", err)
	}
	first := renderOutputs(root, doc, boot)

	svc2, err := schemaHost(context.Background())
	if err != nil {
		t.Fatalf("second schemaHost: %v", err)
	}
	doc2, boot2, err := buildDocument(moduleDir, svc2.Describe())
	if err != nil {
		t.Fatalf("second buildDocument: %v", err)
	}
	second := renderOutputs(root, doc2, boot2)

	if len(first) != len(second) {
		t.Fatalf("output sets differ in size: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].content != second[i].content {
			t.Errorf("output %s is not deterministic across runs", first[i].path)
		}
	}
}
