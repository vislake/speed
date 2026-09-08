#!/usr/bin/env python3
"""Unit tests for affected_go_modules.py's scope computation.

Stdlib-only (unittest), matching this directory's conventions. Run
directly:

    python3 tools/test_affected_go_modules.py

The pure core under test is paths_to_modules (changed paths + go.work
module enumeration + per-module require edges -> the affected module
set, downstream closure included); the git plumbing that feeds it is
exercised against the real repository by the Taskfile `test` task and by
`python3 tools/affected_go_modules.py` itself, not by this suite.

Each test pins one scope rule:

  * test_path_in_module_owns_that_module -- a file under go/dbkit is
    dbkit's change; its own go.mod is dbkit's change too.
  * test_sibling_directory_name_does_not_collide -- go/dbkit/x must not
    be credited to go/dbkitx.
  * test_workspace_file_scopes_to_all -- go.work / go.work.sum changes
    affect every module.
  * test_unknown_path_under_source_tree_scopes_to_all -- a path under
    go/ (or examples/) that no registered module owns (a scaffolded
    module awaiting its go.work entry) must not silently drop out of
    scope.
  * test_non_go_paths_map_to_nothing -- tools/, web/, docs/, .github/
    and root files affect no module.
  * test_empty_changes_fall_back_to_all -- no changed paths (a clean
    tree on the base ref) still scopes to ALL, so a scoped run can
    never silently run nothing.
  * test_downstream_closure -- a change to one module brings in every
    module whose go.mod requires it, transitively; a module nobody
    requires stays alone.
  * test_parse_go_mod_edges_ignores_third_party -- require lines for
    non-speed modules are not edges.

The fixture module graph mirrors the real go.work topology loosely:
dbkit and observability sit on pkgcore; authn sits on dbkit, ratelimit
and pkgcore; org on dbkit and authn; admin on authn, org and rbac; the
reference app on authn, org, storage and notification; rbac on dbkit
only (mirroring that rbac must never import authn).
"""

from __future__ import annotations

import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import affected_go_modules as m  # noqa: E402

MODULES = [
    "examples/reference-app",
    "go/admin",
    "go/authn",
    "go/dbkit",
    "go/notification",
    "go/observability",
    "go/org",
    "go/pkgcore",
    "go/ratelimit",
    "go/rbac",
    "go/storage",
]

EDGES = {
    "examples/reference-app": {
        "go/authn", "go/org", "go/storage", "go/notification",
    },
    "go/admin": {"go/authn", "go/org", "go/rbac", "go/dbkit"},
    "go/authn": {"go/dbkit", "go/ratelimit", "go/pkgcore"},
    "go/dbkit": {"go/pkgcore"},
    "go/notification": {"go/dbkit", "go/ratelimit", "go/pkgcore"},
    "go/observability": {"go/pkgcore"},
    "go/org": {"go/dbkit", "go/authn", "go/ratelimit"},
    "go/ratelimit": {"go/pkgcore"},
    "go/rbac": {"go/dbkit"},
    "go/storage": {"go/dbkit", "go/ratelimit", "go/jobs"},
}


def scope(paths):
    """The affected set for changed paths over the fixture graph, as a
    plain frozenset of module dirs (ALL expands to the full set)."""
    result = m.paths_to_modules(paths, MODULES, EDGES)
    if result == {"ALL"}:
        return frozenset(MODULES)
    return frozenset(result)


class ScopeRules(unittest.TestCase):
    # dbkit's downstream closure over the fixture graph: authn, rbac,
    # org, admin, storage and notification require dbkit directly, and
    # the reference app requires authn/org/storage/notification.
    DBKIT_CLOSURE = frozenset(
        {
            "go/dbkit", "go/authn", "go/rbac", "go/org", "go/admin",
            "go/storage", "go/notification", "examples/reference-app",
        }
    )

    def test_path_in_module_owns_that_module(self):
        # A file under go/dbkit is dbkit's change -- and, because a
        # module's own change can break its dependents, the scope is
        # dbkit plus its downstream closure.
        self.assertEqual(
            scope(["go/dbkit/repository.go"]), self.DBKIT_CLOSURE
        )
        # A module's own go.mod/go.sum are the module's files.
        self.assertEqual(
            scope(["go/dbkit/go.mod", "go/dbkit/go.sum"]),
            self.DBKIT_CLOSURE,
        )

    def test_sibling_directory_name_does_not_collide(self):
        # go/dbkit/x must not be credited to a hypothetical go/dbkitx
        # prefix, and a path that merely shares a prefix with a module
        # dir stays outside it.
        self.assertEqual(
            scope(["go/dbkit/x/y.go"]), self.DBKIT_CLOSURE
        )

    def test_workspace_file_scopes_to_all(self):
        self.assertEqual(
            scope(["go.work"]), frozenset(MODULES)
        )
        self.assertEqual(
            scope(["go.work.sum"]), frozenset(MODULES)
        )
        # A workspace edit beside a module edit still means everything.
        self.assertEqual(
            scope(["go.work", "go/rbac/rbac.go"]), frozenset(MODULES)
        )

    def test_unknown_path_under_source_tree_scopes_to_all(self):
        # A module directory with code but no go.work entry yet (the
        # scaffolder-to-registration window) must not drop out of scope.
        self.assertEqual(
            scope(["go/brandnew/service.go"]), frozenset(MODULES)
        )
        self.assertEqual(
            scope(["examples/another-app/main.go"]), frozenset(MODULES)
        )

    def test_non_go_paths_map_to_nothing(self):
        # A change touching only non-Go trees has an empty module set,
        # which the ALL fallback turns into the full suite -- task test
        # never silently runs nothing.
        for path in [
            "tools/scan_cjk.py",
            "web/packages/tokens/src/index.ts",
            "docs/internal/00-overview.md",
            ".github/workflows/fast-check.yml",
            "Taskfile.yml",
            "README.md",
        ]:
            with self.subTest(path=path):
                self.assertEqual(scope([path]), frozenset(MODULES))

    def test_empty_changes_fall_back_to_all(self):
        self.assertEqual(scope([]), frozenset(MODULES))

    def test_downstream_closure(self):
        # pkgcore is the dependency floor: everyone requires it,
        # transitively or directly.
        self.assertEqual(
            scope(["go/pkgcore/kernel.go"]), frozenset(MODULES)
        )
        # org's dependents are admin and the reference app (via its own
        # require of org) -- org's own change plus the closure.
        self.assertEqual(
            scope(["go/org/tree.go"]),
            {"go/org", "go/admin", "examples/reference-app"},
        )
        # rbac is required only by admin.
        self.assertEqual(
            scope(["go/rbac/service.go"]), {"go/rbac", "go/admin"}
        )
        # The reference app has no dependents.
        self.assertEqual(
            scope(["examples/reference-app/cmd/server/server.go"]),
            {"examples/reference-app"},
        )
        # A module change plus its dependent's own change dedupes.
        self.assertEqual(
            scope(["go/org/tree.go", "go/admin/role.go"]),
            {"go/org", "go/admin", "examples/reference-app"},
        )


class Parsing(unittest.TestCase):
    def test_parse_go_work_entries(self):
        text = "go 1.26.8\n\nuse (\n\t./go/dbkit\n\t./examples/reference-app\n)\n"
        self.assertEqual(
            m.parse_go_work(text), ["go/dbkit", "examples/reference-app"]
        )

    def test_parse_go_mod_edges(self):
        text = """module github.com/vislake/speed/go/admin

go 1.26.0

replace github.com/vislake/speed/go/pkgcore => ../pkgcore

require (
\tgithub.com/BurntSushi/toml v1.6.0
\tgithub.com/vislake/speed/go/authn v0.0.0-00010101000000-000000000000
\tgithub.com/vislake/speed/go/rbac v0.0.0-00010101000000-000000000000
\tgolang.org/x/crypto v0.56.0 // indirect
)
"""
        self.assertEqual(
            m.parse_go_mod_edges(text), {"go/authn", "go/rbac"}
        )

    def test_parse_go_mod_edges_single_line_require(self):
        text = "module x\n\ngo 1.26.0\n\nrequire github.com/vislake/speed/go/jobs v0.0.0-00010101000000-000000000000\n"
        self.assertEqual(m.parse_go_mod_edges(text), {"go/jobs"})


if __name__ == "__main__":
    unittest.main()
