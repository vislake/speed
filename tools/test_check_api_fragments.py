#!/usr/bin/env python3
"""Unit tests for check_api_fragments.py's drift contract.

Stdlib-only (unittest + tempfile), matching this directory's own "plain
executables with no third-party dependencies" convention (tools/README.md's
"Running in CI and locally" section). Run directly:

    python3 tools/test_check_api_fragments.py

The fixture tree is a small fake repository: a tools/api_fragments.json
manifest, a mini .github/workflows/api-contract.yml shaped exactly like
the real file (the same indentation levels, `run: |` literal blocks with
`#`-leading content lines, trailing `#` comments on plain scalars), and
one api/ directory per fragment carrying openapi.yaml +
oapi-codegen.yaml. Every planted drift below must make check() report it
(exit 1 in the CLI); the machinery-level tests prove the checker catches
the drift classes the gate exists for -- a fragment added to the tree
without the manifest, a manifest entry missing from one leg, an
unregistered fragment in a leg -- and test_real_tree_is_consistent runs
the real check against this repository, which is what the api-contract
job's drift-check step runs on every trigger.

Planted-drift classes covered (each fails before the wiring that makes
it green -- the regression teeth this mechanism ships with, per root
CLAUDE.md's bug-fix policy):

  * fragment dir added to the tree, not in the manifest
    (test_fragment_dir_added_to_tree_without_manifest_is_drift);
  * manifest entry whose spec/config file is missing from the tree
    (test_registered_fragment_spec_missing_is_drift);
  * manifest entry with no oapi-codegen regeneration step, and a
    regeneration step for a dir the manifest does not register
    (test_registered_fragment_missing_from_regen_steps_is_drift,
    test_extra_regen_step_for_unregistered_dir_is_drift);
  * manifest entry missing from one trigger path filter, and a
    fragment-shaped path-filter row naming an unregistered dir
    (test_fragment_missing_from_pull_request_paths_is_drift,
    test_fragment_missing_from_push_paths_is_drift,
    test_extra_fragment_shaped_path_row_is_drift);
  * a merged fragment dropped from / swapped in / added to the redocly
    join input list (test_merged_fragment_dropped_from_join_is_drift,
    test_join_order_reversed_is_drift, test_non_merged_fragment_in_
    join_is_drift);
  * the gate's own files missing from a trigger path filter
    (test_self_wiring_row_missing_is_drift).

Infrastructure errors (manifest or workflow missing or unreadable) must
exit 2, distinct from drift (test_*_exits_2).
"""

from __future__ import annotations

import json
import pathlib
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_api_fragments as m  # noqa: E402

ALPHA = {"name": "alpha", "dir": "go/alpha/api", "merge_rank": 1}
BETA = {"name": "beta", "dir": "go/beta/api", "merge_rank": 2}
GAMMA = {
    "name": "gamma",
    "dir": "examples/reference-app/internal/gamma/api",
}
FRAGS = [ALPHA, BETA, GAMMA]
MERGE_ORDER = ["go/alpha/api", "go/beta/api"]  # alpha before beta, by rank

OAPI_RUN = (
    "go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0"
)


def manifest_text(frags: list[dict]) -> str:
    return json.dumps({"fragments": frags}, indent=2) + "\n"


def workflow_text(
    frags: list[dict],
    *,
    pr_paths: list[str] | None = None,
    push_paths: list[str] | None = None,
    regen_skip: tuple[str, ...] = (),
    regen_extra: tuple[str, ...] = (),
    merge_inputs: list[str] | None = None,
    include_merge: bool = True,
) -> str:
    """Render a mini api-contract.yml in the real file's exact shapes."""
    frag_dirs = [f["dir"] for f in frags]
    fragment_rows = [f + "/**" for f in frag_dirs]
    other_rows = ["redocly.yaml", "build/openapi/**"]
    if pr_paths is None:
        pr_paths = other_rows + fragment_rows + list(m.SELF_ROWS)
    if push_paths is None:
        push_paths = other_rows + fragment_rows + list(m.SELF_ROWS)

    lines: list[str] = [
        "name: api-contract (fixture)",
        "# A header comment the reader must skip.",
        "on:",
        "  pull_request:",
        "    paths:",
    ]
    lines += [f"      - {row}" for row in pr_paths]
    lines += [
        "  workflow_dispatch:",
        "  push:",
        "    branches: [main]",
        "    paths:",
    ]
    lines += [f"      - {row}" for row in push_paths]
    lines += [
        "concurrency:",
        "  group: api-contract-fixture",
        "  cancel-in-progress: true",
        "permissions:",
        "  contents: read",
        "jobs:",
        "  api-contract:",
        "    name: fixture job",
        "    runs-on: ubuntu-latest",
        "    steps:",
    ]

    regen_dirs = [d for d in frag_dirs if d not in regen_skip] + list(
        regen_extra
    )
    for idx, frag_dir in enumerate(regen_dirs):
        lines.append(
            f"      - name: Regenerate surface {idx} (pinned oapi-codegen "
            f"v2.8.0)"
        )
        lines.append("        # A comment inside the step the reader must skip.")
        lines.append("        run: |")
        lines.append(f"          cd {frag_dir}")
        lines.append("          # A '#'-leading content line inside the literal.")
        lines.append(f"          {OAPI_RUN} \\")
        lines.append("            -config oapi-codegen.yaml openapi.yaml")
        lines.append(
            "      - name: Regenerated artifact must match the committed spec"
        )
        lines.append("        run: |")
        lines.append(
            '          if [ -n "$(git status --porcelain --untracked-files=all)" ]; then'
        )
        lines.append("            exit 1")
        lines.append("          fi")

    if include_merge:
        lines.append(
            "      - name: Merge every fragment into speed.yaml "
            "(pinned redocly 2.51.1)"
        )
        lines.append("        run: |")
        lines.append("          mkdir -p build/openapi")
        lines.append("          pnpm --package=@redocly/cli@2.51.1 dlx redocly join \\")
        for frag_dir in merge_inputs if merge_inputs is not None else MERGE_ORDER:
            lines.append(f"            {frag_dir}/openapi.yaml \\")
        lines.append("            -o build/openapi/speed.yaml")

    lines += [
        "      - name: An unrelated setup step",
        "        uses: actions/checkout@0123456789abcdef0123456789abcdef01234567  # v7.0.1",
        "        run: |",
        "          echo hello",
        "",
    ]
    return "\n".join(lines)


class CheckApiFragmentsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.root = pathlib.Path(tempfile.mkdtemp(prefix="api-fragments-"))
        self._write_fixture()

    def tearDown(self) -> None:
        shutil.rmtree(self.root, ignore_errors=True)

    def _write_fixture(self, frags: list[dict] | None = None, **workflow_kw) -> None:
        frags = FRAGS if frags is None else frags
        for frag in frags:
            api_dir = self.root / frag["dir"]
            api_dir.mkdir(parents=True, exist_ok=True)
            (api_dir / "openapi.yaml").write_text(
                "openapi: 3.0.0\n", encoding="utf-8"
            )
            (api_dir / "oapi-codegen.yaml").write_text(
                "package: fixture\n", encoding="utf-8"
            )
        tools_dir = self.root / "tools"
        tools_dir.mkdir(parents=True, exist_ok=True)
        (tools_dir / "api_fragments.json").write_text(
            manifest_text(frags), encoding="utf-8"
        )
        workflow_dir = self.root / ".github" / "workflows"
        workflow_dir.mkdir(parents=True, exist_ok=True)
        (workflow_dir / "api-contract.yml").write_text(
            workflow_text(frags, **workflow_kw), encoding="utf-8"
        )

    def _check(self) -> tuple[list[str], int, int]:
        return m.check(str(self.root))

    def _problems(self) -> list[str]:
        return self._check()[0]

    def test_consistent_fixture_is_clean(self) -> None:
        self.assertEqual(self._problems(), [])
        problems, n_frags, n_merged = self._check()
        self.assertEqual(n_frags, 3)
        self.assertEqual(n_merged, 2)

    def test_fragment_dir_added_to_tree_without_manifest_is_drift(self) -> None:
        api_dir = self.root / "go" / "extra" / "api"
        api_dir.mkdir(parents=True)
        (api_dir / "openapi.yaml").write_text("openapi: 3.0.0\n", encoding="utf-8")
        (api_dir / "oapi-codegen.yaml").write_text(
            "package: extra\n", encoding="utf-8"
        )
        problems = self._problems()
        self.assertTrue(any("go/extra/api" in p for p in problems))
        self.assertTrue(any("not registered" in p for p in problems))

    def test_registered_fragment_spec_missing_is_drift(self) -> None:
        (self.root / "go" / "beta" / "api" / "openapi.yaml").unlink()
        problems = self._problems()
        self.assertTrue(any("beta" in p and "openapi.yaml is missing" in p for p in problems))

    def test_registered_fragment_missing_from_regen_steps_is_drift(self) -> None:
        self._write_fixture(regen_skip=("go/beta/api",))
        problems = self._problems()
        self.assertTrue(
            any(
                "go/beta/api" in p and "no oapi-codegen regeneration step" in p
                for p in problems
            )
        )

    def test_extra_regen_step_for_unregistered_dir_is_drift(self) -> None:
        self._write_fixture(regen_extra=("go/zzz/api",))
        problems = self._problems()
        self.assertTrue(
            any("regenerates go/zzz/api" in p for p in problems)
        )

    def test_fragment_missing_from_pull_request_paths_is_drift(self) -> None:
        rows = ["redocly.yaml", "build/openapi/**"] + [
            f["dir"] + "/**" for f in FRAGS if f["dir"] != "go/beta/api"
        ] + list(m.SELF_ROWS)
        self._write_fixture(pr_paths=rows)
        problems = self._problems()
        self.assertTrue(
            any("go/beta/api" in p and "pull_request path filter" in p for p in problems)
        )

    def test_fragment_missing_from_push_paths_is_drift(self) -> None:
        rows = ["redocly.yaml", "build/openapi/**"] + [
            f["dir"] + "/**" for f in FRAGS if f["dir"] != "go/beta/api"
        ] + list(m.SELF_ROWS)
        self._write_fixture(push_paths=rows)
        problems = self._problems()
        self.assertTrue(
            any("go/beta/api" in p and "push path filter" in p for p in problems)
        )

    def test_extra_fragment_shaped_path_row_is_drift(self) -> None:
        rows = ["redocly.yaml", "build/openapi/**"] + [
            f["dir"] + "/**" for f in FRAGS
        ] + ["go/zzz/api/**"] + list(m.SELF_ROWS)
        self._write_fixture(pr_paths=rows)
        problems = self._problems()
        self.assertTrue(
            any("path filter names go/zzz/api/**" in p for p in problems)
        )

    def test_merged_fragment_dropped_from_join_is_drift(self) -> None:
        self._write_fixture(merge_inputs=["go/alpha/api"])
        problems = self._problems()
        self.assertTrue(
            any("redocly join step's input list" in p and "go/beta/api/openapi.yaml" in p for p in problems)
        )

    def test_join_order_reversed_is_drift(self) -> None:
        self._write_fixture(merge_inputs=["go/beta/api", "go/alpha/api"])
        problems = self._problems()
        self.assertTrue(
            any("merge_rank order" in p for p in problems)
        )

    def test_non_merged_fragment_in_join_is_drift(self) -> None:
        self._write_fixture(
            merge_inputs=[
                "go/alpha/api",
                "examples/reference-app/internal/gamma/api",
                "go/beta/api",
            ]
        )
        problems = self._problems()
        self.assertTrue(
            any("redocly join step's input list" in p for p in problems)
        )

    def test_self_wiring_row_missing_is_drift(self) -> None:
        rows = ["redocly.yaml", "build/openapi/**"] + [
            f["dir"] + "/**" for f in FRAGS
        ] + ["tools/api_fragments.json"]
        self._write_fixture(pr_paths=rows)
        problems = self._problems()
        self.assertTrue(
            any(
                "tools/check_api_fragments.py is missing from the "
                "pull_request path filter" in p
                for p in problems
            )
        )

    def test_merge_step_missing_exits_2(self) -> None:
        self._write_fixture(include_merge=False)
        with self.assertRaises(SystemExit) as cm:
            self._check()
        self.assertEqual(cm.exception.code, 2)

    def test_missing_workflow_exits_2(self) -> None:
        (self.root / ".github" / "workflows" / "api-contract.yml").unlink()
        with self.assertRaises(SystemExit) as cm:
            self._check()
        self.assertEqual(cm.exception.code, 2)

    def test_unreadable_workflow_exits_2(self) -> None:
        (self.root / ".github" / "workflows" / "api-contract.yml").write_text(
            "this is not [ valid yaml {(\n", encoding="utf-8"
        )
        with self.assertRaises(SystemExit) as cm:
            self._check()
        self.assertEqual(cm.exception.code, 2)

    def test_missing_manifest_exits_2(self) -> None:
        (self.root / "tools" / "api_fragments.json").unlink()
        with self.assertRaises(SystemExit) as cm:
            self._check()
        self.assertEqual(cm.exception.code, 2)

    def test_unreadable_manifest_exits_2(self) -> None:
        (self.root / "tools" / "api_fragments.json").write_text(
            "{ not json", encoding="utf-8"
        )
        with self.assertRaises(SystemExit) as cm:
            self._check()
        self.assertEqual(cm.exception.code, 2)

    def test_duplicate_merge_rank_is_drift(self) -> None:
        frags = [
            dict(ALPHA),
            dict(ALPHA, name="beta", dir="go/beta/api"),
            GAMMA,
        ]
        self._write_fixture(frags=frags)
        problems = self._problems()
        self.assertTrue(any("share merge_rank 1" in p for p in problems))

    def test_real_tree_is_consistent(self) -> None:
        root = pathlib.Path(__file__).resolve().parent.parent
        problems, n_frags, n_merged = m.check(str(root))
        self.assertEqual(
            problems,
            [],
            "the real tree must satisfy the fragment-enumeration gate "
            "(this is the check the api-contract job's drift step runs); "
            f"found {len(problems)} drift(s)",
        )
        self.assertEqual(n_frags, 13)
        self.assertEqual(n_merged, 6)


if __name__ == "__main__":
    unittest.main()
