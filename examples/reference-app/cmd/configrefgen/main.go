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
//   - The BOOTSTRAP layer: the environment variables a process resolves at
//     startup, before the dynamic layer exists. The bootstrap surface is
//     host-authored -- each host reads its own variables with its own
//     resolution rules (the reference app and the generated consumer
//     projects both read os.Getenv directly; go/pkgcore/config provides the
//     generalized loader mechanism with flags > SPEED_* environment > an
//     optional YAML/JSON file > struct defaults, which no shipped host
//     currently drives) -- so there is no declarative schema to extract.
//     This reference documents the reference app's own bootstrap surface,
//     the host this repository boots: the variable inventory is walked out
//     of the app's source (bootstrap.go), and the per-variable facts
//     (type, default, whether the value is a secret, what it configures)
//     come from a curated table kept honest by a coverage gate -- every
//     environment variable the app's source reads must appear in the table
//     and every table entry must be read, or the generator fails.
//
// The outputs are docs/config-reference.md and docs/config-reference.json
// at the repository root, plus the root .env.example (the dev-flow
// environment carrier this repository gitignores without a committed
// example). Every output is deterministic and byte-identical across runs.
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

	svc, err := schemaHost(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "configrefgen: compose schema host:", err)
		return 1
	}

	moduleDir := filepath.Join(root, "examples", "reference-app")
	doc, bootRows, err := buildDocument(moduleDir, svc.Describe())
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
