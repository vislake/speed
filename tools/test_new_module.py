#!/usr/bin/env python3
"""Unit tests for new_module.py's scaffold plans and validation.

Stdlib-only (unittest), matching this directory's conventions. Run
directly:

    python3 tools/test_new_module.py

The Go plan's content is pinned by the script's own --dry-run and by
the scaffold compile gate in fast-check's repo-checks job, which
materializes both categories for real: the go-category stub builds in a
throwaway directory, and the app-category skeleton is scaffolded into
the reference app, code-generated with the pinned oapi-codegen, built
and smoke-tested (the step reads the generator's own pinned command).
This suite therefore pins the parts that are cheap to pin in-process:
the Go plan's three-file shape and its go.work-derived go directive
(the existing contract, regression-guarded here), the npm plan's
seven-file skeleton (the new category), the JSON well-formedness of its
package.json, and the shared name validation's two category rules.

The npm plan's CI-greenness (pnpm lint/typecheck/test/build from the
package directory) is proven live against the workspace by the batch's
probe run, not by this suite -- a scaffold test that ran node tooling
would violate this directory's stdlib-only rule.
"""

from __future__ import annotations

import json
import pathlib
import re
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import new_module as m  # noqa: E402

DESIGN = "docs/internal/07-platform-services.md"


class GoPlanTests(unittest.TestCase):
    def test_go_plan_is_the_three_file_stub(self):
        plan = m.build_plan("sharing", "public share links.", DESIGN, "1.26.0")
        self.assertEqual(
            [rel for rel, _ in plan],
            ["go.mod", "doc.go", "AGENTS.md"],
        )
        contents = dict(plan)
        self.assertEqual(
            contents["go.mod"],
            "module github.com/vislake/speed/go/sharing\n\ngo 1.26.0\n",
        )
        self.assertIn("package sharing\n", contents["doc.go"])
        self.assertIn(f"See {DESIGN} for the design.", contents["AGENTS.md"])


class GoLanguageVersionTests(unittest.TestCase):
    def test_version_reads_the_workspace_directive_with_patch_zero(self):
        # The scaffolded module's go directive is the workspace's language
        # version in the module convention: the go.work patch component is
        # the toolchain floor, not a module-level fact.
        with tempfile.TemporaryDirectory() as td:
            pathlib.Path(td, "go.work").write_text(
                "go 1.31.5\n\nuse (\n\t./go/alpha\n)\n", encoding="utf-8"
            )
            self.assertEqual(m.read_go_language_version(td), "1.31.0")

    def test_version_accepts_a_two_component_workspace_directive(self):
        with tempfile.TemporaryDirectory() as td:
            pathlib.Path(td, "go.work").write_text("go 1.27\n", encoding="utf-8")
            self.assertEqual(m.read_go_language_version(td), "1.27.0")

    def test_missing_workspace_yields_none(self):
        with tempfile.TemporaryDirectory() as td:
            self.assertIsNone(m.read_go_language_version(td))

    def test_real_repository_version_matches_every_module_convention(self):
        # The pin against drift: what the script would stamp into a new
        # go.mod is exactly what every shipped module's go.mod declares
        # (major.minor.0, from this repository's own go.work).
        version = m.read_go_language_version(m.SCRIPT_REPO_ROOT)
        self.assertIsNotNone(version)
        go_mod = pathlib.Path(m.SCRIPT_REPO_ROOT, "go", "authn", "go.mod")
        self.assertIn(f"\ngo {version}\n", go_mod.read_text(encoding="utf-8"))


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


class AppPlanTests(unittest.TestCase):
    def _plan(self, name="widgets", description="provides the widget resource"):
        pair = m.read_app_locale_templates()
        self.assertIsNotNone(pair, "the locale pair templates must ship beside the script")
        return dict(m.build_app_plan(name, description, "example.com/probe-app", pair))

    def test_app_plan_ships_the_module_shape(self):
        plan = self._plan()
        self.assertEqual(
            sorted(plan),
            sorted(
                [
                    "doc.go",
                    "module.go",
                    "model.go",
                    "repository.go",
                    "handler.go",
                    "handler_test.go",
                    "migrations/fs.go",
                    "migrations/sqlite/0001_create_widgets.sql",
                    "migrations/postgres/0001_create_widgets.sql",
                    "locales/fs.go",
                    "locales/zh-CN.toml",
                    "locales/en-US.toml",
                    "api/openapi.yaml",
                    "api/oapi-codegen.yaml",
                ]
            ),
        )

    def test_app_plan_derives_names_from_the_module_name(self):
        plan = self._plan()
        module = plan["module.go"]
        self.assertIn('moduleName = "widgets"', module)
        self.assertIn('apiPath = "/api/v1/widgets"', module)
        self.assertIn("type Widget struct", plan["model.go"])
        self.assertIn('func (Widget) TableName() string { return "widgets" }', plan["model.go"])
        self.assertIn("func (h *Handler) WidgetsCreateWidget(", plan["handler.go"])
        # The import paths carry the application's own module path, read
        # from the app's go.mod, so the module compiles where it lands.
        self.assertIn('"example.com/probe-app/internal/widgets/migrations"', module)
        self.assertIn('"example.com/probe-app/internal/widgets/api"', plan["handler.go"])

    def test_app_plan_hyphen_and_irregular_names(self):
        derived = m.app_names("patient-records")
        self.assertEqual(derived["entity"], "PatientRecord")
        self.assertEqual(derived["table"], "patient_records")
        self.assertEqual(derived["ci"], "PatientRecords")
        self.assertEqual(derived["pkg"], "patientrecords")
        self.assertEqual(m.app_names("inventory")["entity"], "Inventory")

    def test_app_plan_migration_pair_is_identical(self):
        plan = self._plan()
        self.assertEqual(
            plan["migrations/sqlite/0001_create_widgets.sql"],
            plan["migrations/postgres/0001_create_widgets.sql"],
        )
        self.assertIn("CREATE TABLE widgets (", plan["migrations/sqlite/0001_create_widgets.sql"])

    def test_app_plan_operation_ids_match_the_handler_methods(self):
        # The spec's operationId and the handler's method name must agree
        # (the generated ServerInterface is built from the former and
        # implemented by the latter); the scaffold derives both from the
        # module name, so this pins the two derivations together.
        plan = self._plan()
        self.assertIn("operationId: widgets_createWidget", plan["api/openapi.yaml"])
        self.assertIn("operationId: widgets_list", plan["api/openapi.yaml"])
        self.assertIn("func (h *Handler) WidgetsCreateWidget(", plan["handler.go"])
        self.assertIn("func (h *Handler) WidgetsList(", plan["handler.go"])
        self.assertIn("var _ api.ServerInterface = (*Handler)(nil)", plan["handler.go"])

    def test_app_plan_locale_pair_mirrors_the_templates(self):
        plan = self._plan()
        pair = dict(m.read_app_locale_templates())
        self.assertEqual(plan["locales/zh-CN.toml"], pair["locales/zh-CN.toml"].replace("__NAME__", "widgets"))
        self.assertEqual(plan["locales/en-US.toml"], pair["locales/en-US.toml"].replace("__NAME__", "widgets"))
        # Same message-id set in both languages (the i18n parity rule),
        # read straight off the rendered pair.

        def ids(text):
            return set(re.findall(r'^"([^"]+)" =', text, flags=re.MULTILINE))

        self.assertEqual(ids(plan["locales/zh-CN.toml"]), ids(plan["locales/en-US.toml"]))
        self.assertTrue(ids(plan["locales/en-US.toml"]))

    def test_app_plan_leaves_no_placeholders(self):
        plan = self._plan()
        for rel, content in plan.items():
            for token in ("__NAME__", "__ENTITY__", "__ENTITY_KEY__", "__CI__",
                          "__PKG__", "__TABLE__", "__APP_MODULE__", "__DESCRIPTION__"):
                self.assertNotIn(token, content, rel)

    def test_app_category_requires_a_go_module_target(self):
        with tempfile.TemporaryDirectory() as empty:
            code = m.main([
                "widgets", "--category", "app",
                "--description", "provides the widget resource",
                "--target-dir", empty, "--dry-run",
            ])
            self.assertEqual(code, 2, "a target with no go.mod must be refused")
        with tempfile.TemporaryDirectory() as app:
            pathlib.Path(app, "go.mod").write_text(
                "module example.com/probe-app\n\ngo 1.26.0\n", encoding="utf-8"
            )
            code = m.main([
                "widgets", "--category", "app",
                "--description", "provides the widget resource",
                "--target-dir", app, "--dry-run",
            ])
            self.assertEqual(code, 0)

    def test_app_category_takes_no_design_doc(self):
        with tempfile.TemporaryDirectory() as app:
            pathlib.Path(app, "go.mod").write_text(
                "module example.com/probe-app\n\ngo 1.26.0\n", encoding="utf-8"
            )
            code = m.main([
                "widgets", "--category", "app",
                "--description", "provides the widget resource",
                "--design-doc", "docs/internal/07-platform-services.md",
                "--target-dir", app, "--dry-run",
            ])
            self.assertEqual(code, 0, "--design-doc stays optional for app")

    def test_go_and_npm_categories_still_require_the_design_doc(self):
        code = m.main(["sharing", "--description", "public share links."])
        self.assertEqual(code, 2)


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
