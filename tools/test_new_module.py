#!/usr/bin/env python3
"""Unit tests for new_module.py's scaffold plans and validation.

Stdlib-only (unittest), matching this directory's conventions. Run
directly:

    python3 tools/test_new_module.py

The Go plan's content is pinned by the script's own --dry-run and by
every scaffolded module's compile in CI, so this suite pins the parts
that are cheap to pin in-process: the Go plan's three-file shape (the
existing contract, regression-guarded here), the npm plan's seven-file
skeleton (the new category), the JSON well-formedness of its
package.json, and the shared name validation's two category rules.

The npm plan's CI-greenness (pnpm lint/typecheck/test/build from the
package directory) is proven live against the workspace by the batch's
probe run, not by this suite -- a scaffold test that ran node tooling
would violate this directory's stdlib-only rule.
"""

from __future__ import annotations

import json
import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import new_module as m  # noqa: E402

DESIGN = "docs/internal/07-platform-services.md"


class GoPlanTests(unittest.TestCase):
    def test_go_plan_is_the_three_file_stub(self):
        plan = m.build_plan("sharing", "public share links.", DESIGN)
        self.assertEqual(
            [rel for rel, _ in plan],
            ["go.mod", "doc.go", "AGENTS.md"],
        )
        contents = dict(plan)
        self.assertEqual(
            contents["go.mod"],
            "module github.com/vislake/speed/go/sharing\n\ngo 1.23\n",
        )
        self.assertIn("package sharing\n", contents["doc.go"])
        self.assertIn(f"See {DESIGN} for the design.", contents["AGENTS.md"])


class NpmPlanTests(unittest.TestCase):
    def _plan(self, name="probe-ui"):
        return dict(
            m.build_npm_plan(
                name, "A test probe package for the npm scaffold.", DESIGN
            )
        )

    def test_npm_plan_ships_the_seven_file_skeleton(self):
        plan = self._plan()
        self.assertEqual(
            sorted(plan),
            sorted(
                [
                    "package.json",
                    "tsconfig.json",
                    "tsconfig.build.json",
                    "src/index.ts",
                    "src/index.test.ts",
                    "README.md",
                    "AGENTS.md",
                ]
            ),
        )

    def test_package_json_is_well_formed_and_canonical(self):
        plan = self._plan()
        pkg = json.loads(plan["package.json"])
        self.assertEqual(pkg["name"], "@speed/probe-ui")
        self.assertEqual(pkg["version"], "0.0.0")
        self.assertEqual(
            sorted(pkg["scripts"]),
            sorted(["lint", "typecheck", "test", "build"]),
        )
        self.assertEqual(
            pkg["exports"]["."],
            {"types": "./dist/index.d.ts", "import": "./dist/index.js"},
        )

    def test_package_json_escapes_the_description(self):
        plan = self._plan()
        pkg = json.loads(plan["package.json"])
        self.assertEqual(
            pkg["description"],
            "A test probe package for the npm scaffold.",
        )
        quoted = self._plan()["package.json"]
        self.assertIn('"description": "A test probe package', quoted)
        # A description containing quotes must stay well-formed JSON.
        tricky = dict(
            m.build_npm_plan(
                "probe-ui", 'Sentences with "quotes" and a \\ backslash.', DESIGN
            )
        )
        pkg2 = json.loads(tricky["package.json"])
        self.assertEqual(
            pkg2["description"], 'Sentences with "quotes" and a \\ backslash.'
        )

    def test_index_exports_nothing(self):
        # A stub must not invent public API: a placeholder symbol would
        # become a released package's frozen export once the stub ships
        # (lockstep releases the package the moment its registration
        # lands). The doc-comment-only index is the honest stub surface.
        plan = self._plan()
        index = plan["src/index.ts"]
        self.assertFalse(
            any(line.startswith("export ") for line in index.splitlines()),
            "the stub index must carry no export statement",
        )
        self.assertIn("no API yet", index)

    def test_index_test_pins_identity_and_scripts(self):
        plan = self._plan()
        test = plan["src/index.test.ts"]
        self.assertIn("expect(pkg.name).toBe('@speed/probe-ui')", test)
        for script in ("lint", "typecheck", "test", "build"):
            self.assertIn(script, test)

    def test_tsconfig_pair_matches_package_conventions(self):
        plan = self._plan()
        self.assertEqual(
            json.loads(plan["tsconfig.json"]),
            {"extends": "../../tsconfig.base.json", "include": ["src"]},
        )
        # tsconfig.build.json carries a comment (the ESM authoring note
        # every shipped package's build config repeats), so it is jsonc,
        # not strict JSON -- asserted textually like the real files.
        build = plan["tsconfig.build.json"]
        self.assertIn('"extends": "../../tsconfig.base.json"', build)
        self.assertIn('"module": "nodenext"', build)
        self.assertIn('"exclude": ["src/**/*.test.ts"]', build)
        self.assertIn('"outDir": "dist"', build)

    def test_readme_and_agents_point_at_the_design_doc(self):
        plan = self._plan()
        self.assertIn(f"See {DESIGN} for the design.", plan["README.md"])
        self.assertIn(f"See {DESIGN} for the design.", plan["AGENTS.md"])


class NameValidationTests(unittest.TestCase):
    def test_go_category_refuses_go_keywords(self):
        self.assertIsNotNone(m.validate_name("type", go_stub=True))
        self.assertIsNone(m.validate_name("type", go_stub=False))
        self.assertIsNone(m.validate_name("probe-ui", go_stub=True))

    def test_shared_convention_rejects_bad_shapes(self):
        for name in ("", ".", "..", "Probe", "probe_ui", "a b", "-ui"):
            with self.subTest(name=name):
                self.assertIsNotNone(
                    m.validate_name(name, go_stub=False), name
                )


if __name__ == "__main__":
    unittest.main()
