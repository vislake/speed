// Command configrefgen generates this repository's committed configuration
// reference from the live configuration schema.
//
// The reference documents two configuration layers of a speed-based
// application:
//
//   - The DYNAMIC layer: the items and feature flags that live in the
//     configs table and are served through go/config's Service -- the
//     configuration an operator edits at runtime. The source of truth is
//     the frozen runtime schema of a real host, enumerated through
//     go/config's exported Describe() surface (config.Service.Describe and
//     config.ConfigItemDescriptor in go/config/describe.go, built for
//     "an external tool (a Markdown configuration-reference generator, an
//     admin console)"). A live schema only exists once a real host has
//     bootstrapped and frozen it, so this command composes that host
//     itself (host.go): the six platform modules whose Register folds items
//     or feature flags into the schema (authn, metering, compliance,
//     sharing, pki and org) plus the config module, bootstrapped on a
//     throwaway in-memory database, schema frozen by Attach. The module
//     composition is the cost of this mechanism: it must import the
//     declaring modules, which only a top-of-graph host module can do
//     without inflating a low-tier module's dependency floor, so this
//     command lives in the reference-app module (the one module whose go.mod
//     already requires every platform module) and composes the PLATFORM
//     modules directly rather than through the app's own assembly, keeping
//     the reference app's own demo items (notes' brand/support keys and its
//     two flags) out of the platform reference.
//
//   - The BOOTSTRAP layer: the process-start input resolved before the dynamic
//     layer exists. It has two sources. The platform keys come from the
//     modules themselves: every module that consumes process-start input
//     declares it on the registry's bootstrap seat (pkgcore.BootstrapKey,
//     reg.Bootstrap.Add), and this command renders those declarations as
//     per-module key lists -- what the key protects, its format, whether it is
//     secret material, the fallback an operator should expect -- straight from
//     the census, never from a hand-kept copy. The reference app's own
//     variables come from the app: the inventory is walked out of its source
//     (bootstrap.go), and the per-variable facts (type, default, whether the
//     value is a secret, what it configures) come from a curated table kept
//     honest by a coverage gate -- every environment variable the app's source
//     reads must appear in the table and every table entry must be read, or
//     the generator fails. The two sides meet in one transitional bridge
//     (platformEnvNames): the app still reads its variables directly, so the
//     variables must still be listed, while the keys they carry are the
//     modules' own contracts, and a pair that disagrees fails the generator.
//     A module that consumes no process-start input declares no keys; the
//     reference says so, rather than filling the gap with a placeholder.
//
// The outputs are docs/config-reference.md and docs/config-reference.json at
// the repository root, the root .env.example (the dev-flow environment carrier
// this repository gitignores without a committed example), config.example.json
// (the JSON counterpart of the committed YAML config-file example, derived so
// the pair cannot drift), and the documentation site's copy of the reference
// (docs/site/content.en/docs/user-guide/configuration.md). Every output is
// deterministic and byte-identical across runs.
//
// Usage (from the examples/reference-app module directory; the repository
// root is located automatically as the nearest ancestor carrying go.work):
//
//	go run ./cmd/configrefgen [--check]
//
// --check exits nonzero (printing a diff) instead of writing, for the CI
// wiring in docs-check.yml.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("configrefgen", flag.ContinueOnError)
	repoRoot := fs.String("repo-root", "", "repository root (default: the nearest ancestor of the working directory that carries a go.work file)")
	check := fs.Bool("check", false, "exit nonzero if the outputs are not already up to date, instead of writing them")
	if err := fs.Parse(args); err != nil {
		fmt.Fprintln(os.Stderr, "configrefgen:", err)
		return 2
	}
	// The schema host's Kernel.Bootstrap announces its seam composition and
	// its SurvivesRestart warnings on slog.Default; this command is a
	// deterministic doc generator, not a boot, so those banners are noise.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	root := *repoRoot
	if root == "" {
		var err error
		root, err = findRepoRoot()
		if err != nil {
			fmt.Fprintln(os.Stderr, "configrefgen:", err, "(pass --repo-root)")
			return 2
		}
	}
	root, err := filepath.Abs(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configrefgen:", err)
		return 1
	}

	snapshot, err := schemaHost(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "configrefgen: compose schema host:", err)
		return 1
	}

	moduleDir := filepath.Join(root, "examples", "reference-app")
	doc, bootRows, err := buildDocument(moduleDir, snapshot.service.Describe(), snapshot.declaredKeys, snapshot.composedModules)
	if err != nil {
		fmt.Fprintln(os.Stderr, "configrefgen:", err)
		return 1
	}

	if *check {
		return checkOutputs(root, doc, bootRows)
	}
	return writeOutputs(root, doc, bootRows)
}

// findRepoRoot returns the nearest ancestor of the working directory that
// carries this repository's go.work file, so the command works from the
// module directory (its documented run location) without a flag.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no repository root (a directory with go.work) found above the working directory")
		}
		dir = parent
	}
}
