#!/usr/bin/env python3
"""Scaffold the canonical stub of a new speed Go module.

docs/internal/19-dev-workflow.md's module-generator section promises
`task new:module` so that adding a module never means hand-repeating the
same skeleton (the doc lists the eight things a new module needs --
go.mod, directory skeleton, AGENTS.md, design doc, migration directory, test
skeleton, CI matrix registration, lockstep release registration -- which
in this repository is the go.work use entry itself: the release
coordinator (tools/release/lockstep-release.py) derives the per-module
tag list from go.work at runtime, so a module never registered there
cannot be released). This script
is the generator behind that task: the root Taskfile.yml's new:module task
invokes it, and --help documents the wiring contract (see the epilog).

What it scaffolds is exactly the canonical stub the not-yet-implemented
modules under go/ already carry (go/sharing, go/notification, go/storage,
...): three files, nothing more:

  go/<name>/go.mod     "module github.com/vislake/speed/go/<name>" plus the
                       stub convention's bare "go 1.23" directive. Real
                       modules with dependencies carry "go 1.25.0" and
                       require/replace blocks instead -- those lines appear
                       when an implementation round adds the first
                       dependency, not in the stub.
  go/<name>/doc.go     The one-line English package doc form every module
                       uses ("// Package sharing provides public share links
                       with expiry and access tracking."). The Go package
                       name is the module name with hyphens removed,
                       matching the repo's go/ai-gateway -> package
                       aigateway precedent. The package doc sentence is the
                       --description argument and must be ASCII (the root
                       CLAUDE.md Language Rule would flag anything else).
  go/<name>/AGENTS.md  The one-liner stub form exactly as written in the
                       existing stubs: "# <name>\n\nNot yet implemented. See
                       docs/internal/XX-*.md for the design." The design-doc
                       pointer is the --design-doc argument; it does not
                       need to exist yet, but the script warns when it does
                       not, because AGENTS.md already points at it.

Refusals and guardrails:

  * An existing directory or file at go/<name> is never overwritten -- the
    run fails instead (exit 2).
  * The module name must be lowercase letters, digits and single hyphens,
    start with a letter, contain no underscore (go module directory names
    are hyphen-convention; the repo's only underscore-named directories,
    the go/*/integration_test test tiers, are test packages rather than
    module names), and not be a Go keyword: doc.go reads "package <name
    with hyphens removed>" and "package type" cannot compile, so a
    keyword-named module would violate the canonical-stub contract that
    every scaffolded module builds. "." and ".." are refused too. All of
    this is validated before any filesystem access.
  * Nothing is ever written outside --target-dir: the scaffold only ever
    creates <target-dir>/go/<name>/{go.mod,doc.go,AGENTS.md} for
    --category go and <target-dir>/web/packages/<name>/{package.json,
    tsconfig.json, tsconfig.build.json, src/index.ts, src/index.test.ts,
    README.md, AGENTS.md} for --category npm. The npm skeleton is the
    common core the twelve shipped @speed packages share, distilled to
    the files a package needs to pass the four npm-package-ci legs
    (pnpm lint/typecheck/test/build from the package directory) before
    its implementation round ships any API: the index.ts is a doc
    comment only -- nothing is exported, so no placeholder symbol can
    ever become a released package's frozen public API -- and
    index.test.ts pins the skeleton's own identity and scripts, the
    npm-side mirror of a Go stub's compiling doc.go. docs/internal/
    19-dev-workflow.md's future "task new:npm-package" wraps this
    category exactly as new:module wraps the Go one.

After scaffolding, the script prints a registration checklist -- go.work
use entry (which is also the lockstep release registration, see below),
CI matrix row, roadmap/design-doc rows -- as actionable reminders. It never modifies any of those shared repository
files itself -- that is deliberate: a scaffolder that silently edits go.work
and CI matrices makes review diffs impossible to read, so the checklist is
the contract with the human (or with the future Taskfile task, which can
perform the mechanical registrations on top of this script).

Usage:
    python3 tools/new_module.py NAME --description '...' \
        --design-doc docs/internal/XX-name.md
    python3 tools/new_module.py NAME --description '...' \
        --design-doc docs/internal/XX-name.md --dry-run
    python3 tools/new_module.py NAME --description '...' \
        --design-doc docs/internal/XX-name.md --target-dir /tmp/sandbox

NAME is the module directory name (go/<name>). --target-dir defaults to the
repository root, detected by walking up from the current directory to the
first directory containing go.work (--target-dir overrides the detection,
which is how the script is exercised from a sandbox).

Exit codes: 0 = scaffold created (or --dry-run plan printed); 1 = unused;
2 = bad usage, validation failure, or an I/O error (reported on stderr).

Standard library only; requires Python >= 3.11 (shared floor with the
other tools/ scripts).
"""

from __future__ import annotations

import argparse
import os
import re
import sys

# Module path prefix shared by every Go module in the monorepo
# (go.mod files under go/ all read "module github.com/vislake/speed/go/X").
MODULE_PATH_PREFIX = "github.com/vislake/speed/go"

# The go directive of the canonical stub, byte for byte what every
# not-yet-implemented module's go.mod carries ("go 1.23"). Modules with
# real dependencies use "go 1.25.0" instead; the directive is bumped by the
# implementation round that adds the first dependency, not by the stub.
GO_VERSION_LINE = "go 1.23"

# Where stubs live relative to the repo root (docs/internal/02-repo-and-release.md).
GO_DIR_NAME = "go"

# Marker file that identifies the repository root during --target-dir
# detection (the root is a go.work workspace, not a module).
REPO_MARKER_FILE = "go.work"

# docs/internal/ design docs are numbered "NN-name.md"; the AGENTS.md stub
# line points at one.
DESIGN_DOC_PATTERN = re.compile(r"^docs/internal/\d{2}-[a-z0-9-]+\.md$")

# Module directory names: lowercase letter first, then lowercase letters,
# digits and single hyphens. Underscores are rejected outright -- every
# name downstream (go.work use entry, CI matrix row, release tag path
# go/<name>/<version>) is built verbatim from this one and none of them
# want underscores. The repo's own underscore-named directories (the
# go/*/integration_test test tiers that root CLAUDE.md's testing rule
# physically separates) are test packages, never module names.
NAME_PATTERN = re.compile(r"^[a-z][a-z0-9]*(-[a-z0-9]+)*$")

# The 25 Go keywords. None of them can name a package clause ("package
# type" does not compile), and doc.go's clause is "package <module name
# with hyphens removed>", so a module whose name is a keyword -- or
# strips to one, like "t-ype" -- would scaffold a doc.go that can never
# build, breaking the canonical-stub contract. Predeclared identifiers
# (nil, error, string, true, ...) are deliberately not on the list: they
# are ordinary identifiers and "package error" compiles fine.
GO_KEYWORDS = frozenset({
    "break", "case", "chan", "const", "continue", "default", "defer",
    "else", "fallthrough", "for", "func", "go", "goto", "if", "import",
    "interface", "map", "package", "range", "return", "select", "struct",
    "switch", "type", "var",
})


def validate_name(name: str, go_stub: bool) -> str | None:
    """Return an error message for an invalid module/package name, or
    None. The shared naming convention (lowercase letters, digits and
    single hyphens, no underscores) covers both categories -- every
    @speed package name in web/packages follows the same shape as the
    go/ module directories; the Go-keyword refusal applies to Go stubs
    only, where doc.go's package clause must compile."""
    if not name:
        return "name is empty"
    if name in (".", ".."):
        return f"name {name!r} is not a directory name"
    if not NAME_PATTERN.match(name):
        return (
            f"name {name!r} is not valid: use lowercase letters, "
            f"digits and single hyphens, starting with a letter; no "
            f"underscores (the repo's directory-name convention, shared "
            f"by go/ modules and web/packages/@speed names alike)"
        )
    if go_stub:
        pkg_name = name.replace("-", "")
        if pkg_name in GO_KEYWORDS:
            return (
                f"module name {name!r} is a Go keyword: doc.go would carry "
                f"'package {pkg_name}', which cannot compile; pick a name "
                f"that is not a reserved word"
            )
    return None


def package_name_for(module_name: str) -> str:
    """Go package name for the module: hyphens removed (aigateway, ...)."""
    return module_name.replace("-", "")


def build_plan(module_name: str, description: str, design_doc: str) -> list[tuple[str, str]]:
    """Return [(module-relative path, content)] for the Go stub's files."""
    pkg = package_name_for(module_name)
    files: list[tuple[str, str]] = []

    go_mod = f"module {MODULE_PATH_PREFIX}/{module_name}\n\n{GO_VERSION_LINE}\n"
    files.append(("go.mod", go_mod))

    doc_go = (
        f"// Package {pkg} {description}\n"
        f"package {pkg}\n"
    )
    files.append(("doc.go", doc_go))

    agents_md = (
        f"# {module_name}\n"
        f"\n"
        f"Not yet implemented. See {design_doc} for the design.\n"
    )
    files.append(("AGENTS.md", agents_md))
    return files


# The canonical web-package skeleton's shared file shapes, distilled
# from the twelve shipped @speed packages' common core (package.json /
# the tsconfig pair / src + a test named after its source file /
# README + AGENTS.md per package). A stub ships no API -- its index.ts
# is a doc comment only -- but its WIRING is real: the four
# npm-package-ci legs (pnpm lint / typecheck / test / build from the
# package directory, reusable-npm-package-ci.yml) all pass on the
# skeleton, so the package's CI row can be registered before its
# implementation round, exactly as a Go stub's go.mod/doc.go/AGENTS.md
# make a not-yet-implemented module a real go.work member.
NPM_SCRIPTS = ('lint', 'typecheck', 'test', 'build')


def build_npm_plan(
    module_name: str, description: str, design_doc: str
) -> list[tuple[str, str]]:
    """Return [(package-relative path, content)] for the npm stub's
    files. Every string is ASCII: the root CLAUDE.md Language Rule
    scans the tree (tools/scan_cjk.py) and these files are not under
    docs/internal/. The description is JSON-escaped into package.json,
    so a sentence containing quotes or a backslash stays well-formed."""
    import json  # noqa: PLC0415 -- stdlib, local import for one call

    package_json = (
        "{\n"
        f'  "name": "@speed/{module_name}",\n'
        '  "version": "0.0.0",\n'
        f'  "description": {json.dumps(description)},\n'
        '  "type": "module",\n'
        '  "sideEffects": false,\n'
        '  "files": ["dist", "README.md", "AGENTS.md"],\n'
        '  "exports": {\n'
        '    ".": {\n'
        '      "types": "./dist/index.d.ts",\n'
        '      "import": "./dist/index.js"\n'
        '    },\n'
        '    "./package.json": "./package.json"\n'
        '  },\n'
        '  "scripts": {\n'
        '    "lint": "eslint .",\n'
        '    "typecheck": "tsc -p tsconfig.json",\n'
        '    "test": "vitest run",\n'
        '    "build": "tsc -p tsconfig.build.json"\n'
        '  },\n'
        '  "devDependencies": {\n'
        '    "@types/node": "^26.4.1"\n'
        '  }\n'
        "}\n"
    )
    tsconfig = (
        "{\n"
        '  "extends": "../../tsconfig.base.json",\n'
        '  "include": ["src"]\n'
        "}\n"
    )
    tsconfig_build = (
        "{\n"
        '  "extends": "../../tsconfig.base.json",\n'
        '  "compilerOptions": {\n'
        "    // ESM authoring rules: NodeNext mode makes tsc emit relative\n"
        "    // imports verbatim, so sources must write explicit .js\n"
        "    // extensions (TS2835 otherwise) -- the emitted dist/ is then\n"
        "    // loadable by Node ESM and typecheckable by NodeNext\n"
        "    // consumers as-is.\n"
        '    "module": "nodenext",\n'
        '    "moduleResolution": "nodenext",\n'
        '    "noEmit": false,\n'
        '    "declaration": true,\n'
        '    "outDir": "dist",\n'
        '    "rootDir": "src"\n'
        "  },\n"
        '  "include": ["src"],\n'
        '  "exclude": ["src/**/*.test.ts"]\n'
        "}\n"
    )
    index_ts = (
        f"// Package index of the @speed/{module_name} stub.\n"
        "//\n"
        "// The canonical package skeleton this scaffolder materializes\n"
        "// carries a real build/lint/test wiring and no API yet: the\n"
        "// package's public surface lands in its implementation round,\n"
        "// in the same PR as its design doc. Nothing is exported until\n"
        "// then, so no placeholder symbol ever becomes a released\n"
        "// package's frozen public API.\n"
    )
    index_test = (
        "// Stub-wiring test of the canonical @speed/package skeleton.\n"
        "// Named after its source file (index.ts -> index.test.ts, the\n"
        "// frontend naming rule). It pins the skeleton itself -- the\n"
        "// package identity and the four npm-package-ci legs' scripts --\n"
        "// so a scaffolded package is proven green before its\n"
        "// implementation round fills the API in; delete the whole file\n"
        "// with the implementation round's real tests.\n"
        "import { readFileSync } from 'node:fs'\n"
        "import { describe, expect, it } from 'vitest'\n"
        "\n"
        "describe('@speed/%s stub skeleton', () => {\n"
        "  it('carries the package identity a consumer will resolve', () => {\n"
        "    const pkg = JSON.parse(\n"
        "      readFileSync(new URL('../package.json', import.meta.url), 'utf8'),\n"
        "    ) as { name: string; scripts: Record<string, string> }\n"
        "    expect(pkg.name).toBe('@speed/%s')\n"
        "    for (const script of ['lint', 'typecheck', 'test', 'build']) {\n"
        "      expect(pkg.scripts[script], script).toBeDefined()\n"
        "    }\n"
        "  })\n"
        "})\n"
    ) % (module_name, module_name)
    readme = (
        f"# @speed/{module_name}\n"
        "\n"
        f"Not yet implemented. See {design_doc} for the design.\n"
    )
    agents = (
        f"# AGENTS.md — @speed/{module_name}\n"
        "\n"
        f"Not yet implemented. See {design_doc} for the design.\n"
    )
    return [
        ("package.json", package_json),
        ("tsconfig.json", tsconfig),
        ("tsconfig.build.json", tsconfig_build),
        ("src/index.ts", index_ts),
        ("src/index.test.ts", index_test),
        ("README.md", readme),
        ("AGENTS.md", agents),
    ]


def registration_checklist(module_name: str, design_doc: str) -> list[str]:
    """Return the post-scaffold reminder lines for a Go module (never
    written anywhere)."""
    lines = [
        "Next steps -- the shared repository files below are intentionally "
        "left untouched by this script:",
        f"  1. go.work (repo root): add \"./{GO_DIR_NAME}/{module_name}\" to the "
        "use ( ... ) block -- the workspace is what makes go build / go test "
        "resolve the new module locally.",
        "  2. CI matrix: register the module in the fast-check/full-check "
        "workflow matrix (docs/internal/18-cicd.md, reusable-workflow "
        "design section: adding a module is one row in the orchestrating "
        "workflow's matrix list). Both pipelines are live: add the module "
        "to the matrix in .github/workflows/fast-check.yml and full-check.yml, "
        "or it is never linted, vetted or tested in CI.",
        "  3. Lockstep release: step 1's go.work use entry is this "
        "module's release registration too -- the release coordinator "
        "(tools/release/lockstep-release.py) derives the per-module tag "
        "list from go.work at runtime, so a module missing from go.work "
        "can never be tagged (docs/internal/02-repo-and-release.md: each "
        "module is tagged go/<module>/<version>, and the coordinator's "
        "completeness drift check fails a release plan loudly until the "
        "use entry lands).",
        "  4. Roadmap and design doc: register the module in the milestone "
        "that plans it (docs/internal/15-roadmap.md) and in the navigation "
        "of docs/internal/00-overview.md / 01-architecture.md (module "
        "dependency graph).",
    ]
    return lines


def npm_registration_checklist(module_name: str, design_doc: str) -> list[str]:
    """Return the post-scaffold reminder lines for an npm package
    (never written anywhere) -- the npm-side mirror of the Go
    checklist: each registration is a shared-file edit a scaffolder
    must never perform silently."""
    lines = [
        "Next steps -- the shared repository files below are intentionally "
        "left untouched by this script:",
        "  1. Workspace install: run 'pnpm install' from web/ so the new "
        "package is recorded as a workspace importer in web/pnpm-lock.yaml "
        "-- CI installs with --frozen-lockfile, so the lockfile entry must "
        "land in the same change as the package.",
        "  2. Lockstep release: add \"@speed/" + module_name + "\" to the fixed "
        "group of web/.changeset/config.json (the array whose members are "
        "bumped together) -- the npm-side release registration, the mirror "
        "of a Go module's go.work use entry: the release coordinator "
        "(tools/release/lockstep-release.py) fails a release plan whose "
        "fixed-group coverage does not match web/packages/*.",
        "  3. CI matrix: register the package in the fast-check/full-check "
        "npm matrix (reusable-npm-package-ci rows, one per web package and "
        "for examples/reference-app/web) in .github/workflows/fast-check.yml "
        "and full-check.yml -- or it is never linted, typechecked, tested "
        "or built in CI.",
        "  4. Roadmap and design doc: register the package in the milestone "
        "that plans it (docs/internal/15-roadmap.md) and, once it ships a "
        "surface, in the web-package enumeration of the root CLAUDE.md "
        "Repository Status and web/README.md.",
    ]
    return lines


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="new_module.py",
        description=(
            "Scaffold the canonical stub of a new speed Go module (go.mod "
            "+ doc.go + AGENTS.md under go/<name>) or npm package (the "
            "@speed/<name> skeleton under web/packages/<name>: package.json, "
            "the tsconfig pair, a doc-comment index.ts plus its wiring "
            "test, README and AGENTS.md). Both categories reproduce the "
            "repo's real shapes -- the Go stub the not-yet-implemented "
            "modules carry, the npm skeleton the twelve shipped @speed "
            "packages' common core distills -- and both print a "
            "registration checklist instead of touching shared files. "
            "This is the generator behind the new:module task in the root "
            "Taskfile.yml (docs/internal/19-dev-workflow.md's "
            "module-generator section)."
        ),
        epilog=(
            "Taskfile wiring: the root Taskfile.yml's new:module task (named "
            "in docs/internal/19-dev-workflow.md) implements this contract "
            "for --category go:\n"
            "\n"
            "  # Taskfile.yml\n"
            "  new:module:\n"
            "    desc: Scaffold a new Go module stub and print its registration checklist.\n"
            "    cmds:\n"
            "      - python3 tools/new_module.py '{{.NAME}}' --description '{{.DESCRIPTION}}' \\\n"
            "          --design-doc '{{.DESIGN_DOC}}'\n"
            "\n"
            "  # invoked as:\n"
            "  #   task new:module NAME=sharing DESCRIPTION='...' DESIGN_DOC=docs/internal/07-....md\n"
            "\n"
            "The npm template is the same script's --category npm (a future "
            "task new:npm-package wraps it exactly like new:module wraps "
            "the Go category)."
        ),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument("name", help="module or package directory name "
                        "(lowercase letters, digits, single hyphens; no "
                        "underscores; for a Go module additionally not a Go "
                        "keyword)")
    parser.add_argument("--description", required=True,
                        help="one-line English description of the module or "
                        "package (single sentence; ASCII only -- CJK would "
                        "fail the repo's own scan_cjk.py language check)")
    parser.add_argument("--design-doc", required=True, metavar="FILE",
                        help="repo-relative design doc the AGENTS.md stub "
                        "points at, e.g. docs/internal/07-platform-services.md "
                        "(may not exist yet -- the script warns if missing)")
    parser.add_argument("--category", choices=("go", "npm"), default="go",
                        help="what to scaffold: go (default) creates the "
                        "canonical Go module stub under go/<name>; npm "
                        "creates the canonical @speed package skeleton "
                        "under web/packages/<name>")
    parser.add_argument("--target-dir", metavar="DIR",
                        help="directory the scaffold is created under "
                        "(default: the repository root, auto-detected as "
                        "the nearest ancestor of the current directory that "
                        "contains go.work); the scaffold always lands at "
                        "<DIR>/go/<name> (go) or <DIR>/web/packages/<name> "
                        "(npm) and nothing is ever written outside <DIR>")
    parser.add_argument("--dry-run", action="store_true",
                        help="print the files that would be created without "
                        "writing anything")
    args = parser.parse_args(argv)

    go_stub = args.category == "go"
    name_error = validate_name(args.name, go_stub)
    if name_error:
        print(f"error: {name_error}", file=sys.stderr)
        return 2
    if "\n" in args.description or "\r" in args.description:
        print("error: --description must be a single line (it becomes one "
              "doc.go comment line or one package.json description)",
              file=sys.stderr)
        return 2
    try:
        args.description.encode("ascii")
    except UnicodeEncodeError:
        print("error: --description must be ASCII: the repo's Language Rule "
              "requires English (ASCII) outside docs/internal/, and "
              "tools/scan_cjk.py would flag any CJK or non-ASCII text in "
              "the generated files", file=sys.stderr)
        return 2
    if not DESIGN_DOC_PATTERN.match(args.design_doc):
        print(f"error: --design-doc {args.design_doc!r} is not a docs/internal/"
              "design doc path (expected docs/internal/NN-name.md)",
              file=sys.stderr)
        return 2

    if args.target_dir:
        target_dir = os.path.abspath(args.target_dir)
    else:
        # Default: repository root, detected by the go.work marker.
        target_dir = os.getcwd()
        while True:
            if os.path.isfile(os.path.join(target_dir, REPO_MARKER_FILE)):
                break
            parent = os.path.dirname(target_dir)
            if parent == target_dir:
                print("error: cannot find the repository root (a directory "
                      f"containing {REPO_MARKER_FILE}) from the current "
                      "directory; run from the repo or pass --target-dir "
                      "explicitly", file=sys.stderr)
                return 2
            target_dir = parent
    if not os.path.isdir(target_dir):
        print(f"error: --target-dir is not a directory: {target_dir}",
              file=sys.stderr)
        return 2

    if go_stub:
        scaffold_rel = os.path.join(GO_DIR_NAME, args.name)
        plan = build_plan(args.name, args.description, args.design_doc)
        checklist = registration_checklist(args.name, args.design_doc)
    else:
        scaffold_rel = os.path.join(
            "web", "packages", args.name
        )
        plan = build_npm_plan(args.name, args.description, args.design_doc)
        checklist = npm_registration_checklist(args.name, args.design_doc)

    module_dir = os.path.join(target_dir, scaffold_rel)

    # Overwrite refusal, checked before anything is created.
    if os.path.lexists(module_dir):
        print(f"error: refusing to overwrite existing directory: "
              f"{module_dir}", file=sys.stderr)
        return 2
    for rel, _ in plan:
        full = os.path.join(module_dir, rel)
        if os.path.lexists(full):
            print(f"error: refusing to overwrite existing file: {full}",
                  file=sys.stderr)
            return 2

    if args.dry_run:
        print(f"dry run: would scaffold {scaffold_rel} under {target_dir}")
        for rel, _ in plan:
            print(f"  create {os.path.join(scaffold_rel, rel)}")
        print("(nothing written -- --dry-run)")
        return 0

    try:
        os.makedirs(module_dir)
    except OSError as exc:
        print(f"error: cannot create {module_dir}: {exc}", file=sys.stderr)
        return 2
    for rel, content in plan:
        full = os.path.join(module_dir, rel)
        try:
            # Nested plan files (the npm skeleton's src/) need their
            # parent directory; the Go stub's three files sit at the
            # scaffold root.
            os.makedirs(os.path.dirname(full), exist_ok=True)
            with open(full, "x", encoding="utf-8") as fh:
                fh.write(content)
        except OSError as exc:
            print(f"error: cannot write {full}: {exc}", file=sys.stderr)
            return 2

    print(f"scaffolded {scaffold_rel} under {target_dir}:")
    for rel, _ in plan:
        print(f"  created {os.path.join(scaffold_rel, rel)}")
    if not os.path.isfile(os.path.join(target_dir, args.design_doc)):
        print(f"warning: {args.design_doc} does not exist yet -- the "
              "AGENTS.md stub already points at it; write the design doc "
              "and commit it in the same PR as this module/package")
    print()
    for line in checklist:
        print(line)
    return 0


if __name__ == "__main__":
    sys.exit(main())
