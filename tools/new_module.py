#!/usr/bin/env python3
"""Scaffold the canonical stub of a new speed Go module.

`task new:module` exists so that adding a module never means hand-repeating
the same skeleton (go.mod, directory skeleton, AGENTS.md, design doc,
migration directory, test skeleton). In this repository the go.work use
entry is the registration: the release coordinator
(tools/release/lockstep-release.py) derives the per-module tag list from
go.work at runtime, and the CI module sets (the fast-check/full-check/
security matrices and the coverage gate's gated set) derive from it too,
so a module registered there is released and checked together. This
script is the generator behind that task: the root Taskfile.yml's
new:module task invokes it, and --help documents the wiring contract
(see the epilog).

What it scaffolds is the canonical stub shape of a Go module under go/
(three files, nothing more):

  go/<name>/go.mod     "module github.com/vislake/speed/go/<name>" plus a
                       go directive derived from the workspace's go.work.
                       A module with real dependencies carries its own
                       directive and require/replace blocks instead --
                       those lines appear with the first dependency, not
                       in the stub.
  go/<name>/doc.go     The one-line English package doc form every module
                       uses ("// Package sharing provides public share links
                       with expiry and access tracking."). The Go package
                       name is the module name with hyphens removed,
                       matching the repo's go/ai-gateway -> package
                       aigateway precedent. The package doc sentence is the
                       --description argument and must be ASCII: the
                       repo-wide CJK scan would flag anything else.
  go/<name>/AGENTS.md  The one-liner stub form exactly as written in the
                       existing stubs: "# <name>\n\nNot yet implemented. See
                       docs/internal/XX-*.md for the design." The design-doc
                       pointer is the --design-doc argument; it does not
                       need to exist yet, but the script warns when it does
                       not, because AGENTS.md already points at it.

A third category, `--category app`, scaffolds an application-side business
module -- the shape examples/reference-app's notes module carries -- into
an existing application: <target-dir>/internal/<name>/ with the module's
doc.go, model.go, repository.go, handler.go (implementing the
spec-generated api.ServerInterface, the compile-time assertion included),
handler_test.go, the dual-dialect migration pair (migrations/fs.go plus
identical sqlite/ and postgres/ SQL), the bilingual locale pair
(locales/fs.go plus zh-CN.toml and en-US.toml), and the module's OpenAPI
fragment (api/openapi.yaml plus its oapi-codegen.yaml). The application
root -- <target-dir>, which must hold the application's go.mod -- is read
to derive the module's import paths, so a scaffolded module compiles
where it lands. The generated api/<name>-server.gen.go is deliberately
NOT written by this script: it is the pinned generator's artifact, and
the checklist's first step is the exact oapi-codegen command that
produces it (the module compiles once that file exists).

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
    creates <target-dir>/internal/<name>/... for --category app,
    <target-dir>/go/<name>/{go.mod,doc.go,AGENTS.md} for --category go and
    <target-dir>/web/packages/<name>/{package.json,
    tsconfig.json, tsconfig.build.json, src/index.ts, src/index.test.ts,
    README.md, AGENTS.md} for --category npm. The npm skeleton is the
    common core the twelve shipped @speed packages share, distilled to
    the files a package needs to pass the four npm-package-ci legs
    (pnpm lint/typecheck/test/build from the package directory) while
    it carries no API yet: the index.ts is a doc comment only --
    nothing is exported, so no placeholder symbol can
    ever become a released package's frozen public API -- and
    index.test.ts pins the skeleton's own identity and scripts, the
    npm-side mirror of a Go stub's compiling doc.go. docs/internal/
    19-dev-workflow.md's future "task new:npm-package" wraps this
    category exactly as new:module wraps the Go one.

After scaffolding, the script prints a registration checklist -- go.work
use entry (the CI and release registration, see below), coverage-baseline
row, integration-tier rows when the module ships one, roadmap/design-doc
rows -- as actionable reminders. It never modifies any of those shared repository
files itself -- that is deliberate: a scaffolder that silently edits go.work
and CI matrices makes review diffs impossible to read, so the checklist is
the contract with the human.

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

# The scaffolded go.mod's go directive derives from the repository's own
# go.work: the workspace's language version, rendered in the module
# convention (major.minor, patch zero -- the patch component of the
# workspace directive is the toolchain floor pinned in .mise.toml, and
# every shipped module's go.mod declares the two-component language
# version). A module with real dependencies carries its own go directive
# plus require/replace blocks instead; those lines are added with the
# first dependency, not by the stub.
GO_WORK_FILE = "go.work"
GO_WORK_GO_DIRECTIVE = re.compile(r"^go\s+(\d+)\.(\d+)(?:\.\d+)?\s*$", re.MULTILINE)


def read_go_language_version(repo_root: str) -> str | None:
    """The go directive a scaffolded module declares, derived from the
    repository's go.work at repo_root: "<major>.<minor>.0".

    The workspace file is the membership authority for every module this
    script scaffolds, so a checkout whose go.work is missing or carries
    no parseable go directive is a broken source tree, not a default to
    invent: the caller turns None into a hard error."""
    try:
        with open(
            os.path.join(repo_root, GO_WORK_FILE), encoding="utf-8"
        ) as fh:
            text = fh.read()
    except OSError:
        return None
    match = GO_WORK_GO_DIRECTIVE.search(text)
    if not match:
        return None
    return f"{match.group(1)}.{match.group(2)}.0"

# Every Go module in the repository lives under go/ at the repo root.
GO_DIR_NAME = "go"

# Marker file that identifies the repository root during --target-dir
# detection (the root is a go.work workspace, not a module).
REPO_MARKER_FILE = "go.work"

# The repository this script ships in (tools/ is its directory), the
# anchor for facts that describe the repository's own conventions rather
# than the target directory -- the scaffolded go.mod's go directive is
# the workspace's (read_go_language_version), and the conventions travel
# with the script, not with a sandbox --target-dir.
SCRIPT_REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# docs/internal/ design docs are numbered "NN-name.md"; the AGENTS.md stub
# line points at one.
DESIGN_DOC_PATTERN = re.compile(r"^docs/internal/\d{2}-[a-z0-9-]+\.md$")

# Module directory names: lowercase letter first, then lowercase letters,
# digits and single hyphens. Underscores are rejected outright -- every
# name downstream (go.work use entry, CI matrix row, release tag path
# go/<name>/<version>) is built verbatim from this one and none of them
# want underscores. The repo's own underscore-named directories (the
# go/*/integration_test test tiers, physically separate test packages)
# are test packages, never module names.
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


def read_app_module_path(target_dir: str) -> str | None:
    """The module path of the application rooted at target_dir: the
    `module` line of its go.mod, or None when the directory holds none
    (or one without a module line). Line-based on purpose -- the tool
    needs exactly this one path to write the scaffolded module's import
    statements -- and the app's go.mod itself is never touched."""
    try:
        with open(os.path.join(target_dir, "go.mod"), encoding="utf-8") as fh:
            for line in fh:
                stripped = line.strip()
                if stripped.startswith("module "):
                    return stripped[len("module "):].strip()
    except OSError:
        return None
    return None


def build_plan(
    module_name: str,
    description: str,
    design_doc: str,
    go_language_version: str,
) -> list[tuple[str, str]]:
    """Return [(module-relative path, content)] for the Go stub's files.
    go_language_version is the go.work-derived language version (see
    read_go_language_version)."""
    pkg = package_name_for(module_name)
    files: list[tuple[str, str]] = []

    go_mod = (
        f"module {MODULE_PATH_PREFIX}/{module_name}\n\n"
        f"go {go_language_version}\n"
    )
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
# skeleton, so the package's CI row can be registered while it is still
# a stub, exactly as a Go stub's go.mod/doc.go/AGENTS.md
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
        "// carries a real build/lint/test wiring and no API yet; the\n"
        "// package's public API ships in the same PR as its design doc.\n"
        "// Nothing is exported until then, so no placeholder symbol ever\n"
        "// becomes a released package's frozen public API.\n"
    )
    index_test = (
        "// Stub-wiring test of the canonical @speed/package skeleton.\n"
        "// Named after its source file (index.ts -> index.test.ts, the\n"
        "// frontend naming rule). It pins the skeleton itself -- the\n"
        "// package identity and the four npm-package-ci legs' scripts --\n"
        "// so a scaffolded package is proven green before any API\n"
        "// exists; delete the whole file when the real tests land.\n"
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


# --- app-category scaffolding -------------------------------------------

# The pinned generator and its version, as the app-owned regeneration legs
# invoke it (Taskfile.yml's api:gen:app; the app's oapi-codegen.yaml names
# the same pin). The scaffold never runs it -- it prints the command as
# the checklist's first step -- but the template it writes must stay in
# step with the version this constant names.
APP_API_GENERATOR = (
    "go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0"
    " -config oapi-codegen.yaml openapi.yaml"
)


def app_names(name: str) -> dict[str, str]:
    """Derive the scaffold's derived names from the module name: the Go
    package name (hyphens removed), the entity type (the name's parts
    capitalized, a trailing "s" singularized), the entity's lowercase key
    for event/audit/locale ids, the camel-cased prefix the spec's schemas
    and operation ids carry, and the SQL table name (hyphens to
    underscores: an unquoted SQL identifier cannot contain a hyphen)."""
    parts = name.split("-")
    last = parts[-1]
    singular = last[:-1] if last.endswith("s") and len(last) > 1 else last
    entity = "".join(part.capitalize() for part in parts[:-1]) + singular.capitalize()
    return {
        "pkg": name.replace("-", ""),
        "entity": entity,
        "entity_key": entity.lower(),
        "ci": "".join(part.capitalize() for part in parts),
        "table": name.replace("-", "_"),
    }


def render_app_file(template: str, name: str, derived: dict[str, str], app_module: str) -> str:
    """Substitute the app skeleton's placeholders in one file template.
    One left-to-right pass over the template text: the replacement values
    (the module name, the app's module path) are written out and never
    re-scanned, so a name that happens to contain another placeholder's
    text survives."""
    out = template
    for key, value in (
        ("__APP_MODULE__", app_module),
        ("__NAME__", name),
        ("__ENTITY_KEY__", derived["entity_key"]),
        ("__ENTITY__", derived["entity"]),
        ("__CI__", derived["ci"]),
    ):
        out = out.replace(key, value)
    return out


APP_DOC_GO = """// Package __NAME__ __DESCRIPTION__.
//
// It is this application's __ENTITY_KEY__ module: a tenant-scoped
// resource exposed over the application's own HTTP surface and composed
// by its assembly (cmd/server) like every other module.
//
// The module owns its whole slice of the application: its schema
// (migrations/, one dual-dialect pair per migration), its copy
// (locales/, zh-CN and en-US), its API fragment (api/openapi.yaml) and
// its HTTP surface (handler.go, implementing the fragment's generated
// ServerInterface). It registers its route, permissions, event and
// audit action through the module contract's Register method (module.go).
package __PKG__
"""

APP_MODULE_GO = """package __PKG__

import (
	"embed"

	"gorm.io/gorm"

	"github.com/vislake/speed/go/pkgcore"

	"__APP_MODULE__/internal/__NAME__/locales"
	"__APP_MODULE__/internal/__NAME__/migrations"
)

//go:embed api/openapi.yaml
var openAPISpecYAML []byte

const (
	// moduleName is the module's name (the value Name() answers) and the
	// name dbkit.MigrationRegistry keys its dependency graph on.
	moduleName = "__NAME__"

	// apiPath is the path this module's routes are mounted at. It must
	// agree with the paths declared in api/openapi.yaml -- the fragment is
	// what oapi-codegen turns into the generated route patterns (the
	// generated HandlerFromMux call in handler.go) and into the
	// ServerInterface Handler implements, so a path changed in one place
	// only leaves the endpoint dead.
	apiPath = "/api/v1/__NAME__"

	// PermissionRead and PermissionWrite are this module's
	// resource:action permission strings. Declaring them here puts them
	// in the permission catalog rbac freezes after the assembly runs; granting
	// them to the roles that should hold them is the application's job.
	PermissionRead  = "__NAME__:read"
	PermissionWrite = "__NAME__:write"

	// Event__ENTITY__Created is the domain event type published whenever
	// a __ENTITY_KEY__ is created, following the
	// "<module>.<entity>.<action>" convention (pkgcore's EventDecl.Type).
	Event__ENTITY__Created = "__NAME__.__ENTITY_KEY__.created"

	// AuditActionCreate is this module's audit action string, in the
	// present-tense verb form the audit trail uses (the event above is
	// the past-tense fact the same operation produces).
	AuditActionCreate = "__NAME__.__ENTITY_KEY__.create"
)

// __ENTITY__CreatedPayload is the concrete type carried in the
// pkgcore.Event.Payload of every Event__ENTITY__Created event.
type __ENTITY__CreatedPayload struct {
	// ID is the created record's id (__ENTITY__.ID).
	ID string

	// TenantID is the owning tenant, carried as a plain string since the
	// payload is a wire-shaped event type, not a pkgcore-typed one.
	TenantID string
}

// Module carries the module contract for __NAME__.
type Module struct {
	repo    *Repository
	handler *Handler
}

// NewModule returns a Module backed by db. db is expected to come from
// dbkit.Open; constructing a Module performs no I/O of its own --
// opening, migrating and registering db is the application assembly's
// job (register NewModule(db) with the migration registry there, then
// put its descriptor on the assembly's component registry).
func NewModule(db *gorm.DB) *Module {
	return &Module{repo: NewRepository(db)}
}

// Name implements the module contract.
func (m *Module) Name() string { return moduleName }

// DependsOn implements the module contract. This module depends on
// infrastructure only, never on another business module -- an ID
// reference plus a domain event is the contract between modules.
func (m *Module) DependsOn() []string { return nil }

// Migrations implements the module contract.
func (m *Module) Migrations() embed.FS { return migrations.FS }

// Locales implements the module contract.
func (m *Module) Locales() embed.FS { return locales.FS }

// OpenAPISpec implements the module contract: this module's own OpenAPI
// fragment, the single source of its API surface. api/__NAME__-server.gen.go
// derives from it (see api/oapi-codegen.yaml for the pinned generator
// run) and Handler implements the generated ServerInterface.
func (m *Module) OpenAPISpec() []byte { return openAPISpecYAML }

// Register implements the module contract: the declaration body the
// assembly's registry drives. Per the registry's own rules it must not
// perform I/O; it only declares. Notification types, config items and
// feature flags a module grows later register through the same seats
// (the reference app's notes module demonstrates each shape).
func (m *Module) Register(reg *pkgcore.ComponentRegistry) error {
	if err := reg.AuditActionsSeat().Add(AuditActionCreate); err != nil {
		return err
	}

	// The audit seat is handed to NewHandler so the create path can call
	// audit.Emit against the exact registrar AuditActionCreate was
	// declared on (Emit validates the action string against it).
	m.handler = NewHandler(m.repo, reg.EventBus(), reg.AuditActionsSeat())
	// The route is mounted through the http component's route face
	// (pkgcore.RouteRegistrar, provided by go/app/httpserve): MountRoute is
	// a no-op in a composition that serves no HTTP, so this module's
	// descriptor declares the face as an OPTIONAL dependency.
	if err := pkgcore.MountRoute(reg, apiPath, m.handler); err != nil {
		return err
	}

	if err := reg.PermissionsSeat().Add(PermissionRead, PermissionWrite); err != nil {
		return err
	}
	return reg.EventsSeat().Publishes(pkgcore.EventDecl{
		Type:        Event__ENTITY__Created,
		PayloadType: "__PKG__.__ENTITY__CreatedPayload",
		Description: "Published whenever a __ENTITY_KEY__ is created for a tenant.",
	})
}
"""

APP_MODEL_GO = """package __PKG__

import (
	"time"

	"github.com/vislake/speed/go/dbkit"
)

// __ENTITY__ is this module's tenant-scoped record. It embeds
// dbkit.TenantModel for the tenant_id column and GetTenantID -- the
// accessor dbkit.Repository[__ENTITY__]'s TenantScoped constraint
// requires. The id is generated in the application (see handler.go),
// already globally unique on its own, so a plain non-key tenant_id
// column with its own index is enough.
type __ENTITY__ struct {
	// ID is an application-generated UUID (see handler.go), never a
	// database-generated one.
	ID string `gorm:"column:id;primaryKey;size:36"`

	// TenantModel promotes the tenant_id column and GetTenantID.
	dbkit.TenantModel

	// Name is the record's display name. Replace it with this module's
	// real fields -- and with their columns in BOTH migration files.
	Name string `gorm:"column:name;size:200;not null"`

	// CreatedAt is stamped once, at creation.
	CreatedAt time.Time `gorm:"column:created_at;not null"`
}

// TableName pins the table name the migrations create, so the model
// never depends on GORM's pluralization of the struct name.
func (__ENTITY__) TableName() string { return "__TABLE__" }
"""

APP_REPOSITORY_GO = """package __PKG__

import (
	"gorm.io/gorm"

	"github.com/vislake/speed/go/dbkit"
)

// Repository is this module's tenant-scoped data-access type. It embeds
// dbkit.Repository[__ENTITY__] instead of holding a *gorm.DB directly
// (the multi-tenant isolation discipline): Create, FindByID, Update,
// Delete and List are promoted from the embedded base unchanged, and
// every query resolves its tenant from the context, failing closed when
// there is none. Queries this module needs beyond that minimal surface
// get their own methods here.
type Repository struct {
	*dbkit.Repository[__ENTITY__]
}

// NewRepository returns a Repository backed by db, expected to come from
// dbkit.Open, already migrated with this module's own Migrations().
func NewRepository(db *gorm.DB) *Repository {
	return &Repository{Repository: dbkit.NewRepository[__ENTITY__](db)}
}
"""

APP_HANDLER_GO = """package __PKG__

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/vislake/speed/go/dbkit/audit"
	obs "github.com/vislake/speed/go/observability"
	"github.com/vislake/speed/go/pkgcore"
	"github.com/vislake/speed/go/pkgcore/apperr"
	"github.com/vislake/speed/go/pkgcore/httpapi"

	"__APP_MODULE__/internal/__NAME__/api"
)

// maxNameLength is the maximum number of characters -- Unicode code
// points, not bytes -- a __ENTITY_KEY__ name may contain. It must match
// the other representations of the same limit that cannot reference this
// constant: api/openapi.yaml's maxLength and the VARCHAR(200) column in
// both migration files.
const maxNameLength = 200

// maxRequestBodyBytes bounds the request body before it is decoded, the
// same 64 KiB bound the platform's own handlers apply to a body feeding
// an unbounded json.Decoder.
const maxRequestBodyBytes = 1 << 16

// ErrNameRequired is returned when a create request's name is empty or
// all whitespace; its localized text lives in this module's Locales()
// resources (locales/{zh-CN,en-US}.toml, key "__NAME__.name_required"),
// never hardcoded here -- a handler returns the structured code alone and
// the client resolves the message through its own i18n catalog.
var ErrNameRequired = apperr.Invalid("__NAME__.name_required")

// ErrNameTooLong is returned when a create request's name exceeds
// maxNameLength characters (key "__NAME__.name_too_long").
var ErrNameTooLong = apperr.Invalid("__NAME__.name_too_long")

// errInternal is returned when something below this handler fails in a
// way that is not itself an *apperr.Error -- see writeError.
var errInternal = apperr.Internal("__NAME__.internal_error")

// Handler serves this module's HTTP endpoints by implementing the
// spec-generated api.ServerInterface (api/__NAME__-server.gen.go; the
// compile-time assertion at the bottom of this file makes "spec changed,
// handler not" a compile failure instead of a runtime surprise). It must
// run downstream of the application's tenancy middleware on a
// non-allowlisted path: every method reads the tenant that middleware
// already resolved into the request context, never a request parameter,
// header or body.
type Handler struct {
	repo         *Repository
	bus          pkgcore.EventBus
	auditActions pkgcore.AuditActionRegistrar
	mux          *http.ServeMux
}

// NewHandler returns a Handler serving repo's records, publishing
// Event__ENTITY__Created on bus and recording an AuditEvent through
// audit.Emit (validated against auditActions) whenever a record is
// created. bus may be nil, in which case creating still succeeds but
// nothing is published; auditActions must not be nil when bus is
// non-nil. The generated api.HandlerFromMux helper derives the routing
// (method+path patterns) from api/openapi.yaml itself, so the spec is
// the one place this module's paths are spelled out.
func NewHandler(repo *Repository, bus pkgcore.EventBus, auditActions pkgcore.AuditActionRegistrar) *Handler {
	h := &Handler{repo: repo, bus: bus, auditActions: auditActions}
	h.mux = http.NewServeMux()
	api.HandlerFromMux(h, h.mux)
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// __CI__Create__ENTITY__ implements api.ServerInterface: it handles
// POST /api/v1/__NAME__, creating a record under the caller's tenant and
// publishing Event__ENTITY__Created. The request body decodes into the
// spec-generated request type, which carries no tenant field: the tenant
// is always the one the tenancy middleware resolved into the request
// context.
func (h *Handler) __CI__Create__ENTITY__(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	tenant, err := pkgcore.MustTenantFromContext(ctx)
	if err != nil {
		// Unreachable in normal operation (this module's path is never
		// allowlisted, so the tenancy middleware already rejected any
		// request that could reach here without a tenant); handled anyway
		// rather than assumed away, because a handler must never rely
		// solely on a lower layer's fail-closed behavior.
		writeError(w, errInternal.WithCause(err))
		return
	}
	// The tenant is now known on ctx; AnnotateTenant attaches it to this
	// request's span (see its own doc comment in go/observability).
	obs.AnnotateTenant(ctx)

	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	var req api.__CI__Create__ENTITY__Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, apperr.Invalid("__NAME__.invalid_request_body").WithCause(err))
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, ErrNameRequired)
		return
	}
	if length := utf8.RuneCountInString(name); length > maxNameLength {
		writeError(w, ErrNameTooLong.WithParam("limit", maxNameLength).WithParam("length", length))
		return
	}

	record := &__ENTITY__{ID: uuid.NewString(), Name: name, CreatedAt: time.Now().UTC()}
	if err := h.repo.Create(ctx, record); err != nil {
		writeError(w, err)
		return
	}

	h.recordCreatedAudit(ctx, record)
	h.publishCreated(ctx, tenant, record)

	obs.FromContext(ctx).Info("__ENTITY_KEY__ created", "__ENTITY_KEY___id", record.ID)

	httpapi.WriteJSON(w, http.StatusCreated, to__ENTITY__Response(record))
}

// __CI__List implements api.ServerInterface: it handles
// GET /api/v1/__NAME__, returning every record belonging to the caller's
// tenant.
func (h *Handler) __CI__List(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	obs.AnnotateTenant(ctx)

	records, err := h.repo.List(ctx)
	if err != nil {
		writeError(w, err)
		return
	}

	// The slice is always allocated, never left nil, so "items" is
	// present on the wire as [] even for an empty list.
	items := make([]api.__CI____ENTITY__, 0, len(records))
	for i := range records {
		items = append(items, to__ENTITY__Response(&records[i]))
	}
	resp := api.__CI__ListResponse{Items: &items}

	obs.FromContext(ctx).Info("__ENTITY_KEY__s listed", "__ENTITY_KEY___count", len(items))

	httpapi.WriteJSON(w, http.StatusOK, resp)
}

// recordCreatedAudit records an AuditEvent for the new record through
// audit.Emit, the declarative collection mechanism go/dbkit/audit
// documents. A failure is logged at Error level, not returned: the
// record itself was already committed by the time this runs, so a failure
// to record its audit trail must not turn an otherwise successful create
// into a 500 for the caller.
func (h *Handler) recordCreatedAudit(ctx context.Context, record *__ENTITY__) {
	if h.bus == nil {
		return
	}
	err := audit.Emit(ctx, h.bus, h.auditActions, audit.Input{
		Action:   AuditActionCreate,
		Resource: audit.Resource{Type: "__ENTITY_KEY__", ID: record.ID},
		Result:   audit.Result{Success: true},
	})
	if err != nil {
		obs.FromContext(ctx).Error("__NAME__.__ENTITY_KEY__.create audit event emit failed",
			"__ENTITY_KEY___id", record.ID, "error", err)
	}
}

// publishCreated publishes Event__ENTITY__Created for record on the
// EventBus the Module obtained from the Registry. A publish failure is
// logged, not returned, for the same reason recordCreatedAudit's is: the
// record is already committed, so a subscriber's failure must not turn
// the create into a 500.
func (h *Handler) publishCreated(ctx context.Context, tenant pkgcore.TenantID, record *__ENTITY__) {
	if h.bus == nil {
		return
	}
	evt := pkgcore.Event{
		Type:     Event__ENTITY__Created,
		TenantID: tenant,
		Payload: __ENTITY__CreatedPayload{
			ID:       record.ID,
			TenantID: string(tenant),
		},
	}
	if err := h.bus.Publish(ctx, evt); err != nil {
		obs.FromContext(ctx).Error("__NAME__.__ENTITY_KEY__.created event publish failed",
			"__ENTITY_KEY___id", record.ID, "error", err)
	}
}

// to__ENTITY__Response converts record to its spec-generated JSON
// response type. Every field of the generated type is optional in
// api/openapi.yaml, hence pointer-typed; this function always sets them,
// so every field is always present on the wire. CreatedAt is truncated
// to the whole second to keep the RFC3339 rendering free of a fractional
// part.
func to__ENTITY__Response(record *__ENTITY__) api.__CI____ENTITY__ {
	createdAt := record.CreatedAt.Truncate(time.Second)
	return api.__CI____ENTITY__{
		CreatedAt: &createdAt,
		ID:        &record.ID,
		Name:      &record.Name,
	}
}

// writeError writes err to w as the coded error envelope (see
// pkgcore/httpapi): an *apperr.Error keeps its own code, params and
// status; anything else -- something below this handler did not classify
// it -- is folded into errInternal so a caller never sees raw Go error
// text either way.
func writeError(w http.ResponseWriter, err error) {
	httpapi.WriteError(w, err, errInternal)
}

// compile-time check that *Handler implements the api.ServerInterface
// generated from this module's api/openapi.yaml -- the enforcement half
// of the spec-first flow: add an operation to the fragment, regenerate,
// and this assertion stops compiling until Handler implements it.
var _ api.ServerInterface = (*Handler)(nil)
"""

APP_HANDLER_TEST_GO = """package __PKG__

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vislake/speed/go/dbkit"
	_ "github.com/vislake/speed/go/dbkit/dialect/sqlite"
	"github.com/vislake/speed/go/pkgcore"
)

// TestHandler_CreateAndList drives this module end to end over a real
// SQLite database -- the migrated schema, the repository, the handler
// and the generated api types together. It is the module's starting
// smoke proof; grow the real cases here.
func TestHandler_CreateAndList(t *testing.T) {
	ctx := context.Background()
	db, err := dbkit.Open(ctx, dbkit.Options{
		Dialect: dbkit.DialectSQLite,
		DSN:     filepath.Join(t.TempDir(), "__NAME__.db"),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	// This module's own migrations, applied the way any application
	// applies them: through dbkit.MigrationRegistry, one transaction per
	// module.
	registry := dbkit.NewMigrationRegistry()
	if regErr := registry.Register(NewModule(db)); regErr != nil {
		t.Fatalf("register migrations: %v", regErr)
	}
	if applyErr := registry.Apply(ctx, db, dbkit.DialectSQLite); applyErr != nil {
		t.Fatalf("apply migrations: %v", applyErr)
	}

	// bus and auditActions are nil: a create through this handler still
	// succeeds and simply publishes nothing, the contract NewHandler's
	// doc comment states.
	handler := NewHandler(NewRepository(db), nil, nil)
	tenantCtx := pkgcore.WithTenant(ctx, "tenant-a")

	create := httptest.NewRequest(http.MethodPost, "/api/v1/__NAME__", strings.NewReader(`{"name":"probe"}`))
	create = create.WithContext(tenantCtx)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, create)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d (body: %s)", rec.Code, http.StatusCreated, rec.Body.String())
	}

	list := httptest.NewRequest(http.MethodGet, "/api/v1/__NAME__", nil)
	list = list.WithContext(tenantCtx)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, list)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "probe") {
		t.Fatalf("list body does not carry the created record: %s", rec.Body.String())
	}
}
"""

APP_MIGRATIONS_FS_GO = """// Package migrations embeds this module's versioned SQL migration
// files, one subdirectory per dialect, for Module's Migrations() method.
//
// It is its own tiny leaf package because a //go:embed directive's
// patterns resolve relative to the directory of the .go file that
// carries it: for the embedded FS to expose "postgres" and "sqlite" at
// its root -- the layout dbkit.MigrationRegistry.Apply expects from every
// module -- the embedding file must live where those two names are its
// immediate children.
package migrations

import "embed"

// FS embeds this module's postgres/*.sql and sqlite/*.sql migration
// files. The two dialect copies stay identical modulo the declared
// column-type synonyms (BLOB/BYTEA, REAL/DOUBLE PRECISION,
// INTEGER/BIGINT); tools/check_migration_parity.py enforces the pairing
// and the parity across every migration set in the repository.
//
//go:embed postgres sqlite
var FS embed.FS
"""

APP_MIGRATION_0001 = """-- __NAME__ is this application's tenant-scoped __ENTITY_KEY__ module
-- (see the module package's doc.go). Kept identical between the two
-- dialect copies: no dialect-specific types, no gen_random_uuid(), no
-- NOW().
--
-- id is an application-generated UUID (see handler.go), already
-- globally unique on its own, so it alone is the primary key here;
-- tenant_id gets its own secondary index instead of participating in a
-- composite primary key.
CREATE TABLE __TABLE__ (
    id         VARCHAR(36)  NOT NULL,
    tenant_id  VARCHAR(64)  NOT NULL,
    name       VARCHAR(200) NOT NULL,
    created_at TIMESTAMP    NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX idx___TABLE___tenant_id ON __TABLE__ (tenant_id);
"""

APP_LOCALES_FS_GO = """// Package locales embeds this module's zh-CN and en-US message
// resources, for Module's Locales() method. The two files must carry
// exactly the same message ids (the repository's i18n parity rule);
// tools/check_i18n_keys.py and pkgcore/i18n's Builder.AddModule both
// enforce it.
package locales

import "embed"

// FS embeds this module's zh-CN.toml and en-US.toml locale resources.
//
//go:embed zh-CN.toml en-US.toml
var FS embed.FS
"""

# The locale pair's templates are real files beside this script
# (new_module_locales/), not inline strings: tools/ is an English-only
# source tree (the repository's Language Rule, enforced by
# tools/scan_cjk.py), while a zh-CN message catalog is Chinese by design.
# Carrying the pair as actual zh-CN.toml/en-US.toml files -- the same
# content shape the scaffold writes -- keeps the source tree's rule
# intact and puts the pair under tools/check_i18n_keys.py's own key-set
# parity check for free.
APP_LOCALE_DIR_NAME = "new_module_locales"
APP_LOCALE_FILES = ("locales/zh-CN.toml", "locales/en-US.toml")


def read_app_locale_templates() -> list[tuple[str, str]] | None:
    """Read the locale pair templates beside this script, or None when
    they are missing (main reports that as an error rather than
    scaffolding a module whose locale pair is absent)."""
    base = os.path.join(os.path.dirname(os.path.abspath(__file__)),
                        APP_LOCALE_DIR_NAME)
    pair: list[tuple[str, str]] = []
    for rel in APP_LOCALE_FILES:
        name = os.path.basename(rel)
        try:
            with open(os.path.join(base, name), encoding="utf-8") as fh:
                pair.append((rel, fh.read()))
        except OSError:
            return None
    return pair

APP_OPENAPI_YAML = """openapi: 3.0.3
info:
  title: __NAME__
  description: >-
    The __NAME__ module: __DESCRIPTION__. This fragment is the
    application's own API, not platform API: it regenerates
    api/__NAME__-server.gen.go with the pinned oapi-codegen run
    documented in api/oapi-codegen.yaml, and handler.go implements the
    generated ServerInterface. It follows the operationId
    (<module>_<action><Resource>) and schema-name (<Module><Type>)
    naming conventions.
  version: 0.0.0
paths:
  /api/v1/__NAME__:
    post:
      operationId: __NAME___create__ENTITY__
      tags: [__NAME__]
      summary: Create a __ENTITY_KEY__ for the caller's tenant.
      description: >-
        The tenant is always the one the application's tenancy middleware
        already resolved from the request; there is no tenant_id field
        anywhere on this request -- the tenant comes from the resolved
        request context, never from anything the caller sends.
      requestBody:
        required: true
        content:
          application/json:
            schema:
              $ref: '#/components/schemas/__CI__Create__ENTITY__Request'
      responses:
        '201':
          description: The __ENTITY_KEY__ was created.
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/__CI____ENTITY__'
        '400':
          description: >-
            The request body was missing, malformed, had an empty name,
            or a name exceeding the maxLength declared below.
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/__CI__Error'
    get:
      operationId: __NAME___list
      tags: [__NAME__]
      summary: List every __ENTITY_KEY__ belonging to the caller's tenant.
      responses:
        '200':
          description: The caller's tenant's __ENTITY_KEY__s, oldest registration order first.
          content:
            application/json:
              schema:
                $ref: '#/components/schemas/__CI__ListResponse'
components:
  schemas:
    __CI__Create__ENTITY__Request:
      type: object
      required:
        - name
      properties:
        name:
          type: string
          minLength: 1
          maxLength: 200
          description: The __ENTITY_KEY__'s display name.
    __CI____ENTITY__:
      type: object
      properties:
        id:
          type: string
          description: Application-generated UUID.
        name:
          type: string
        created_at:
          type: string
          format: date-time
    __CI__ListResponse:
      type: object
      properties:
        items:
          type: array
          items:
            $ref: '#/components/schemas/__CI____ENTITY__'
    __CI__Error:
      type: object
      description: >-
        The structured {code, params} error envelope every speed API
        returns instead of localized text: code is a stable
        machine-readable identifier naming exactly one failure, and
        params carries the structured per-code details when the failure
        has any. The server never renders a message -- a client maps
        each code to its own locale's text.
      properties:
        code:
          type: string
          example: __NAME__.name_required
        params:
          type: object
          additionalProperties: true
"""

APP_OAPI_CODEGEN_YAML = """# Generator configuration for this module's OpenAPI fragment
# (openapi.yaml in this directory). The module's generated surface is
# regenerated with oapi-codegen pinned at v2.8.0 -- the same version the
# application's own api-generation leg pins; keep the two in lockstep.
#
# generate.std-http-server emits the pure net/http ServerInterface --
# plain (w http.ResponseWriter, r *http.Request) method signatures --
# which handler.go implements, plus the Handler/HandlerFromMux/
# HandlerWithOptions registration helpers whose method+path patterns are
# derived from the fragment itself.
#
# output-options.name-normalizer: the standard Go initialism list, so a
# spec property "id" becomes the Go field ID.
package: api
generate:
  models: true
  std-http-server: true
output-options:
  name-normalizer: ToCamelCaseWithInitialisms
output: __NAME__-server.gen.go
"""


def build_app_plan(
    module_name: str,
    description: str,
    app_module_path: str,
    locale_pair: list[tuple[str, str]],
) -> list[tuple[str, str]]:
    """Return [(module-relative path, content)] for the app-module
    skeleton, the shape examples/reference-app's notes module carries.
    The description is embedded in doc.go and the OpenAPI fragment; the
    application's own module path (read from the app's go.mod by main)
    makes the generated import paths correct where the module lands, and
    locale_pair carries the zh-CN/en-US templates main read from beside
    this script (see read_app_locale_templates)."""
    derived = app_names(module_name)
    derived["pkg"] = module_name.replace("-", "")
    sql = APP_MIGRATION_0001
    files: list[tuple[str, str]] = [
        ("doc.go", APP_DOC_GO),
        ("module.go", APP_MODULE_GO),
        ("model.go", APP_MODEL_GO),
        ("repository.go", APP_REPOSITORY_GO),
        ("handler.go", APP_HANDLER_GO),
        ("handler_test.go", APP_HANDLER_TEST_GO),
        ("migrations/fs.go", APP_MIGRATIONS_FS_GO),
        (f"migrations/sqlite/0001_create_{derived['table']}.sql", sql),
        (f"migrations/postgres/0001_create_{derived['table']}.sql", sql),
        ("locales/fs.go", APP_LOCALES_FS_GO),
        *locale_pair,
        ("api/openapi.yaml", APP_OPENAPI_YAML),
        ("api/oapi-codegen.yaml", APP_OAPI_CODEGEN_YAML),
    ]
    rendered: list[tuple[str, str]] = []
    for rel, template in files:
        content = render_app_file(template, module_name, derived, app_module_path)
        content = content.replace("__PKG__", derived["pkg"])
        content = content.replace("__TABLE__", derived["table"])
        content = content.replace("__DESCRIPTION__", description)
        rendered.append((rel, content))
    return rendered


def app_registration_checklist(module_name: str, target_dir: str) -> list[str]:
    """Return the post-scaffold reminder lines for an application-side
    module (never written anywhere)."""
    return [
        "Next steps -- the application's shared files below are "
        "intentionally left untouched by this script:",
        f"  1. Generate the module's api package: from "
        f"{os.path.join(target_dir, 'internal', module_name, 'api')} run "
        f"`{APP_API_GENERATOR}`. The module does not compile until "
        f"api/{module_name}-server.gen.go exists, and it is committed like "
        "every other generated artifact (regenerate it in the same change "
        "as any fragment edit). If the application has its own "
        "api-generation leg (the reference app's task api:gen:app is the "
        "shape), add this fragment to it there and run that task instead.",
        "  2. Wire the module in the application's assembly: construct it "
        f"with internal/{module_name}.NewModule(db), register it with the "
        "dbkit.MigrationRegistry beside every other module BEFORE the "
        "startup Apply, and put its descriptor on the assembly's "
        "component registry so its Register runs (routes, permissions, "
        "event, audit action). "
        "The host-neutral parts of that assembly are shared through the "
        "platform's composition toolkit, github.com/vislake/speed/go/app; "
        "compose them the way the generated server.go does.",
        "  3. Grant the module's permissions: '" + module_name + ":read' "
        "and '" + module_name + ":write' are in the permission catalog "
        "after the assembly runs; grant them to the roles that should hold them "
        "wherever the application already grants permissions.",
        "  4. Run `go mod tidy` in the application: the scaffold's imports "
        "(uuid, the dbkit packages) may currently be indirect requires; "
        "tidy promotes what the new code imports directly.",
        "  5. Localized text: locales/zh-CN.toml and locales/en-US.toml "
        "already carry the module's error codes in both languages. New "
        "user-facing text must ship in both files (the repository's i18n "
        "parity rule).",
    ]


def registration_checklist(module_name: str, design_doc: str) -> list[str]:
    """Return the post-scaffold reminder lines for a Go module (never
    written anywhere)."""
    lines = [
        "Next steps -- the shared repository files below are intentionally "
        "left untouched by this script:",
        f"  1. go.work (repo root): add \"./{GO_DIR_NAME}/{module_name}\" to the "
        "use ( ... ) block -- the workspace is what makes go build / go test "
        "resolve the new module locally, and the entry is the module's "
        "whole registration (steps 2 and 3).",
        "  2. CI: the go-module sets derive from go.work at run time -- "
        "the fast-check/full-check/security matrices "
        "(reusable-go-module-set.yml) and the coverage gate's gated set "
        "(tools/check_coverage_baseline.py) -- so the use entry is the "
        "module's CI registration and there is no matrix row to add. Two "
        "follow-ups it triggers: record the module's coverage baseline "
        "once its unit suite is green (python3 "
        "tools/check_coverage_baseline.py --update --module "
        f"\"{GO_DIR_NAME}/{module_name}\"; the go-module-ci coverage leg "
        "goes red with 'has no baseline row' until the row lands), and if "
        "the module ships a Docker-backed integration tier (a "
        "//go:build integration test file), add it to full-check.yml's "
        "integration-tiers matrix and Taskfile.yml's INTEGRATION_DIRS -- "
        "tools/check_integration_tiers.py gates that pair against the "
        "tree.",
        "  3. Lockstep release: the same go.work use entry is this "
        "module's release registration -- the release coordinator "
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
        "  3. CI matrix: register the package in the npm-packages matrix "
        "of .github/workflows/fast-check.yml (reusable-npm-package-ci rows, "
        "one per web package and for examples/reference-app/web) -- or it "
        "is never linted, typechecked, tested or built in CI. Unlike the "
        "go modules' derived CI sets, this npm matrix is hand-maintained: "
        "keep it in step with web/pnpm-workspace.yaml's package list.",
        "  4. Roadmap and design doc: register the package in the milestone "
        "that plans it (docs/internal/15-roadmap.md) and, once it ships a "
        "surface, in the web-package enumeration of web/README.md.",
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
    parser.add_argument("--design-doc", metavar="FILE",
                        help="repo-relative design doc the AGENTS.md stub "
                        "points at, e.g. docs/internal/07-platform-services.md "
                        "(may not exist yet -- the script warns if missing); "
                        "required for --category go and npm, not used by "
                        "--category app")
    parser.add_argument("--category", choices=("go", "npm", "app"),
                        default="go",
                        help="what to scaffold: go (default) creates the "
                        "canonical Go module stub under go/<name>; npm "
                        "creates the canonical @speed package skeleton "
                        "under web/packages/<name>; app creates the "
                        "application-side module skeleton (the reference "
                        "app's notes-module shape) at "
                        "<target-dir>/internal/<name> inside an existing "
                        "application (the target must hold the "
                        "application's go.mod)")
    parser.add_argument("--target-dir", metavar="DIR",
                        help="directory the scaffold is created under "
                        "(default: the repository root, auto-detected as "
                        "the nearest ancestor of the current directory that "
                        "contains go.work); the scaffold always lands at "
                        "<DIR>/go/<name> (go), <DIR>/web/packages/<name> "
                        "(npm) or <DIR>/internal/<name> (app, where <DIR> "
                        "is the application root holding its go.mod) and "
                        "nothing is ever written outside <DIR>")
    parser.add_argument("--dry-run", action="store_true",
                        help="print the files that would be created without "
                        "writing anything")
    args = parser.parse_args(argv)

    go_stub = args.category in ("go", "app")
    app_module = args.category == "app"
    if not app_module and not args.design_doc:
        print("error: --design-doc is required for --category go and npm "
              "(the AGENTS.md/README stub points at it); it is not used by "
              "--category app", file=sys.stderr)
        return 2
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
    if args.design_doc and not DESIGN_DOC_PATTERN.match(args.design_doc):
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

    if app_module:
        # The application-side module lands inside an existing application,
        # whose go.mod is what makes the generated import paths correct
        # where the module lands -- refused here, before anything is
        # created, when the target holds none.
        app_module_path = read_app_module_path(target_dir)
        if app_module_path is None:
            print(f"error: --category app scaffolds into an application: "
                  f"{target_dir} holds no go.mod, so the module's import "
                  "paths cannot be derived; pass --target-dir naming the "
                  "application root (the directory with its go.mod)",
                  file=sys.stderr)
            return 2
        scaffold_rel = os.path.join("internal", args.name)
        locale_pair = read_app_locale_templates()
        if locale_pair is None:
            print(f"error: the app scaffold's locale pair templates are "
                  f"missing from {APP_LOCALE_DIR_NAME}/ beside this script "
                  "(zh-CN.toml and en-US.toml); restore them -- the "
                  "scaffold must not ship a module without its bilingual "
                  "locale pair", file=sys.stderr)
            return 2
        plan = build_app_plan(
            args.name, args.description, app_module_path, locale_pair
        )
        checklist = app_registration_checklist(args.name, target_dir)
    elif go_stub:
        scaffold_rel = os.path.join(GO_DIR_NAME, args.name)
        language_version = read_go_language_version(SCRIPT_REPO_ROOT)
        if language_version is None:
            print(f"error: no go directive readable from "
                  f"{os.path.join(SCRIPT_REPO_ROOT, GO_WORK_FILE)} -- the "
                  "scaffolded module's go.mod derives its go directive "
                  "from the workspace's, and a checkout without one is "
                  "broken; restore go.work, or run the script from the "
                  "repository it scaffolds for", file=sys.stderr)
            return 2
        plan = build_plan(
            args.name, args.description, args.design_doc, language_version
        )
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
    if args.design_doc and not os.path.isfile(
        os.path.join(target_dir, args.design_doc)
    ):
        print(f"warning: {args.design_doc} does not exist yet -- the "
              "AGENTS.md stub already points at it; write the design doc "
              "and commit it in the same PR as this module/package")
    print()
    for line in checklist:
        print(line)
    return 0


if __name__ == "__main__":
    sys.exit(main())
