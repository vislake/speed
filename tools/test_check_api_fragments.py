#!/usr/bin/env python3
"""Unit tests for check_api_fragments.py's drift contract.

Stdlib-only (unittest + tempfile), matching this directory's own "plain
executables with no third-party dependencies" convention (tools/README.md's
"Running in CI and locally" section). Run directly:

    python3 tools/test_check_api_fragments.py

The fixture tree is a small fake repository: a tools/api_fragments.json
manifest, the real tools/api_fragment_matrix.py derivation (copied, so
the gate can execute it; drift tests may plant a variant instead), a
mini .github/workflows/api-contract.yml shaped exactly like the real
file (the three jobs, the same indentation levels, `run: |` literal
blocks with `#`-leading content lines, trailing `#` comments on plain
scalars, the gate job's outputs block and the matrix reference), and
one api/ directory per manifest entry carrying openapi.yaml +
oapi-codegen.yaml. Every planted drift below must make check() report it
(exit 1 in the CLI); the machinery-level tests prove the checker catches
the drift classes the gate exists for -- a fragment added to the tree
without the manifest, a manifest entry missing from one leg, an
unregistered fragment in a leg, the matrix leg decoupled from the
manifest -- and test_real_tree_is_consistent runs the real check against
this repository, which is what the api-contract job's drift-check step
runs on every trigger.

The fixture mirrors the real tree's two fragment classes: platform
fragments registered in the manifest (alpha and beta, both merged) and
the reference app's own fragments, listed under the manifest's
`app_owned` entries instead (gamma -- the app's fragments join the
app's own merged document and SDK through the app-owned generation
leg, never this workflow's, so they must appear in none of its legs).

Planted-drift classes covered (each fails before the wiring that makes
it green -- the regression teeth this mechanism ships with):

  * fragment dir added to the tree, not in the manifest
    (test_fragment_dir_added_to_tree_without_manifest_is_drift);
  * app-owned fragment dir added to the tree without its manifest
    app_owned entry, and the app_owned listing dropped while the dir
    stays (test_app_owned_dir_added_without_manifest_entry_is_drift,
    test_manifest_app_owned_listing_removed_is_drift);
  * manifest entry whose spec/config file is missing from the tree,
    for a fragment and for an app_owned entry
    (test_registered_fragment_spec_missing_is_drift,
    test_app_owned_entry_spec_missing_is_drift);
  * one dir registered as a fragment and listed app_owned
    (test_fragment_also_listed_app_owned_is_drift);
  * the matrix leg decoupled from the manifest: a matrix not consuming
    the gate job's derived output, a gate job without the derivation
    step, a derivation whose output drops a fragment (the gate executes
    the planted script), a regeneration step running outside the matrix
    entry's directory, a missing oapi-codegen step, and a missing
    porcelain gate
    (test_matrix_not_derived_from_the_gate_output_is_drift,
    test_missing_derivation_step_is_drift,
    test_derivation_dropping_a_fragment_is_drift,
    test_regeneration_step_off_the_matrix_directory_is_drift,
    test_missing_oapi_codegen_step_is_drift,
    test_missing_porcelain_gate_in_the_matrix_job_is_drift);
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
FRAGS = [ALPHA, BETA]
# The app-owned fragment dirs of the fixture tree (the real manifest
# lists the reference app's own fragments the same way).
APP_OWNED = ["examples/reference-app/internal/gamma/api"]
MERGE_ORDER = ["go/alpha/api", "go/beta/api"]  # alpha before beta, by rank

OAPI_RUN = (
    "go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.0"
)


def manifest_text(
    frags: list[dict], app_owned: list[str] | None = None
) -> str:
    if app_owned is None:
        app_owned = APP_OWNED
    return json.dumps({"fragments": frags, "app_owned": app_owned}, indent=2) + "\n"


def workflow_text(
    frags: list[dict],
    *,
    pr_paths: list[str] | None = None,
    push_paths: list[str] | None = None,
    derive_run: str | None = None,
    matrix_consumer: str | None = None,
    regen_working_directory: str | None = None,
    include_regen: bool = True,
    include_porcelain: bool = True,
    merge_inputs: list[str] | None = None,
    include_merge: bool = True,
) -> str:
    """Render a mini api-contract.yml in the real file's exact shapes:
    the contract-gate job (drift steps + the derivation into
    $GITHUB_OUTPUT), the matrix regeneration job (fromJson of the gate
    output, whitelisting the derivation knobs the planted drifts turn),
    and the generated-surface job (the merge step and an unrelated
    sibling)."""
    frag_dirs = [f["dir"] for f in frags]
    fragment_rows = [f + "/**" for f in frag_dirs]
    other_rows = ["redocly.yaml", "contracts/**"]
    if pr_paths is None:
        pr_paths = other_rows + fragment_rows + list(m.SELF_ROWS)
    if push_paths is None:
        push_paths = other_rows + fragment_rows + list(m.SELF_ROWS)
    if derive_run is None:
        derive_run = (
            'echo "fragments=$(python3 tools/api_fragment_matrix.py)" '
            '>> "$GITHUB_OUTPUT"'
        )
    if matrix_consumer is None:
        matrix_consumer = "${{ fromJson(needs.contract-gate.outputs.fragments) }}"
    if regen_working_directory is None:
        regen_working_directory = "${{ matrix.fragment.dir }}"

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
        "  contract-gate:",
        "    name: fixture gate job",
        "    runs-on: ubuntu-latest",
        "    outputs:",
        "      fragments: ${{ steps.derive.outputs.fragments }}",
        "    steps:",
        "      - name: An unrelated setup step",
        "        uses: actions/checkout@0123456789abcdef0123456789abcdef01234567  # v7.0.1",
        "        run: |",
        "          echo hello",
    ]
    if derive_run != "":
        lines += [
            "      - name: Derive the fragment-regeneration matrix",
            "        # A comment inside the step the reader must skip.",
            "        id: derive",
            "        run: |",
            f"          {derive_run}",
        ]
    lines += [
        "  regenerate-fragments:",
        "    name: regenerate fragment ${{ matrix.fragment.name }}",
        "    needs: contract-gate",
        "    runs-on: ubuntu-latest",
        "    strategy:",
        "      fail-fast: false",
        "      matrix:",
        f"        fragment: {matrix_consumer}",
        "    steps:",
        "      - name: Check out repository",
        "        uses: actions/checkout@0123456789abcdef0123456789abcdef01234567  # v7.0.1",
    ]
    if include_regen:
        lines += [
            "      - name: Regenerate the fragment's API surface",
            "        # A comment inside the step the reader must skip.",
            "        working-directory: " + regen_working_directory,
            "        run: |",
            "          # A '#'-leading content line inside the literal.",
            f"          {OAPI_RUN} \\",
            "            -config oapi-codegen.yaml openapi.yaml",
        ]
    if include_porcelain:
        lines += [
            "      - name: Regenerated artifact must match the committed spec",
            "        run: |",
            '          if [ -n "$(git status --porcelain --untracked-files=all)" ]; then',
            "            exit 1",
            "          fi",
        ]
    lines += [
        "  generated-surface:",
        "    name: merged document and frontend sdk",
        "    needs: contract-gate",
        "    runs-on: ubuntu-latest",
        "    steps:",
        "      - name: An unrelated setup step",
        "        uses: actions/checkout@0123456789abcdef0123456789abcdef01234567  # v7.0.1",
        "        run: |",
        "          echo hello",
    ]
    if include_merge:
        lines.append(
            "      - name: Merge every fragment into speed.yaml "
            "(pinned redocly 2.51.1)"
        )
        lines.append("        run: |")
        lines.append("          mkdir -p contracts")
        lines.append("          pnpm --package=@redocly/cli@2.51.1 dlx redocly join \\")
        for frag_dir in merge_inputs if merge_inputs is not None else MERGE_ORDER:
            lines.append(f"            {frag_dir}/openapi.yaml \\")
        lines.append("            -o contracts/speed.yaml")
    lines.append("")
    return "\n".join(lines)


class CheckApiFragmentsTest(unittest.TestCase):
    def setUp(self) -> None:
        self.root = pathlib.Path(tempfile.mkdtemp(prefix="api-fragments-"))
        self._write_fixture()

    def tearDown(self) -> None:
        shutil.rmtree(self.root, ignore_errors=True)

    def _write_fixture(
        self,
        frags: list[dict] | None = None,
        app_owned: list[str] | None = None,
        script_text: str | None = None,
        **workflow_kw,
    ) -> None:
        frags = FRAGS if frags is None else frags
        if app_owned is None:
            app_owned = APP_OWNED
        for frag in frags:
            api_dir = self.root / frag["dir"]
            api_dir.mkdir(parents=True, exist_ok=True)
            (api_dir / "openapi.yaml").write_text(
                "openapi: 3.0.0\n", encoding="utf-8"
            )
            (api_dir / "oapi-codegen.yaml").write_text(
                "package: fixture\n", encoding="utf-8"
            )
        for frag_dir in app_owned:
            api_dir = self.root / frag_dir
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
            manifest_text(frags, app_owned), encoding="utf-8"
        )
        # The real derivation script, unless a drift test plants its own
        # (the gate executes this file, so the fixture must carry one).
        script = tools_dir / "api_fragment_matrix.py"
        if script_text is None:
            shutil.copyfile(
                pathlib.Path(__file__).resolve().parent
                / "api_fragment_matrix.py",
                script,
            )
        else:
            script.write_text(script_text, encoding="utf-8")
        workflow_dir = self.root / ".github" / "workflows"
        workflow_dir.mkdir(parents=True, exist_ok=True)
        (workflow_dir / "api-contract.yml").write_text(
            workflow_text(frags, **workflow_kw), encoding="utf-8"
        )

    def _check(self) -> tuple[list[str], int, int, int]:
        return m.check(str(self.root))

    def _problems(self) -> list[str]:
        return self._check()[0]

    def test_consistent_fixture_is_clean(self) -> None:
        self.assertEqual(self._problems(), [])
        problems, n_frags, n_merged, n_app_owned = self._check()
        self.assertEqual(n_frags, 2)
        self.assertEqual(n_merged, 2)
        self.assertEqual(n_app_owned, 1)

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

    def test_matrix_not_derived_from_the_gate_output_is_drift(self) -> None:
        # The regeneration leg is only as good as its derivation: a matrix
        # that stops consuming the gate job's output re-opens the silent
        # enumeration drift this mechanism exists to close.
        self._write_fixture(matrix_consumer="[alpha, beta]")
        problems = self._problems()
        self.assertTrue(
            any(
                "only fragment enumeration this leg may consume" in p
                for p in problems
            )
        )

    def test_missing_derivation_step_is_drift(self) -> None:
        self._write_fixture(derive_run="")
        problems = self._problems()
        self.assertTrue(
            any(
                "no step running tools/api_fragment_matrix.py" in p
                for p in problems
            )
        )

    def test_derivation_dropping_a_fragment_is_drift(self) -> None:
        # A derivation whose output silently drops a fragment: the gate
        # executes the script against the tree, so the planted variant
        # goes red even though the workflow and the manifest still agree
        # textually.
        script = (
            "import json\n"
            "data = json.load(open('tools/api_fragments.json'))\n"
            "entries = [\n"
            "    {'name': f['name'], 'dir': f['dir']}\n"
            "    for f in data['fragments'][:1]\n"
            "]\n"
            "print(json.dumps(entries))\n"
        )
        self._write_fixture(script_text=script)
        problems = self._problems()
        self.assertTrue(
            any(
                "does not match the manifest's fragments in manifest "
                "order" in p
                for p in problems
            )
        )

    def test_regeneration_step_off_the_matrix_directory_is_drift(self) -> None:
        self._write_fixture(regen_working_directory="go/alpha/api")
        problems = self._problems()
        self.assertTrue(
            any(
                "working-directory" in p
                and "${{ matrix.fragment.dir }}" in p
                for p in problems
            )
        )

    def test_missing_oapi_codegen_step_is_drift(self) -> None:
        self._write_fixture(include_regen=False)
        problems = self._problems()
        self.assertTrue(
            any("no oapi-codegen step" in p for p in problems)
        )

    def test_missing_porcelain_gate_in_the_matrix_job_is_drift(self) -> None:
        self._write_fixture(include_porcelain=False)
        problems = self._problems()
        self.assertTrue(
            any("no porcelain gate" in p for p in problems)
        )

    def test_fragment_missing_from_pull_request_paths_is_drift(self) -> None:
        rows = ["redocly.yaml", "contracts/**"] + [
            f["dir"] + "/**" for f in FRAGS if f["dir"] != "go/beta/api"
        ] + list(m.SELF_ROWS)
        self._write_fixture(pr_paths=rows)
        problems = self._problems()
        self.assertTrue(
            any("go/beta/api" in p and "pull_request path filter" in p for p in problems)
        )

    def test_fragment_missing_from_push_paths_is_drift(self) -> None:
        rows = ["redocly.yaml", "contracts/**"] + [
            f["dir"] + "/**" for f in FRAGS if f["dir"] != "go/beta/api"
        ] + list(m.SELF_ROWS)
        self._write_fixture(push_paths=rows)
        problems = self._problems()
        self.assertTrue(
            any("go/beta/api" in p and "push path filter" in p for p in problems)
        )

    def test_extra_fragment_shaped_path_row_is_drift(self) -> None:
        rows = ["redocly.yaml", "contracts/**"] + [
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

    def test_app_owned_dir_in_join_is_drift(self) -> None:
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

    def test_app_owned_dir_added_without_manifest_entry_is_drift(self) -> None:
        api_dir = self.root / "examples" / "reference-app" / "internal" / "delta" / "api"
        api_dir.mkdir(parents=True)
        (api_dir / "openapi.yaml").write_text("openapi: 3.0.0\n", encoding="utf-8")
        (api_dir / "oapi-codegen.yaml").write_text(
            "package: delta\n", encoding="utf-8"
        )
        problems = self._problems()
        self.assertTrue(any("internal/delta/api" in p for p in problems))
        self.assertTrue(any("app_owned" in p for p in problems))

    def test_manifest_app_owned_listing_removed_is_drift(self) -> None:
        # The app-owned dir stays on disk while the manifest stops listing
        # it: the real-tree regression class for a manifest that drops the
        # reference app's fragments without moving them out of the tree.
        self._write_fixture(app_owned=[])
        problems = self._problems()
        self.assertTrue(
            any("internal/gamma/api" in p and "app_owned" in p for p in problems)
        )

    def test_app_owned_entry_spec_missing_is_drift(self) -> None:
        (self.root / "examples" / "reference-app" / "internal" / "gamma" / "api" / "openapi.yaml").unlink()
        problems = self._problems()
        self.assertTrue(
            any(
                "app-owned fragment" in p and "openapi.yaml is missing" in p
                for p in problems
            )
        )

    def test_fragment_also_listed_app_owned_is_drift(self) -> None:
        self._write_fixture(
            frags=FRAGS,
            app_owned=APP_OWNED + ["go/alpha/api"],
        )
        problems = self._problems()
        self.assertTrue(
            any(
                "registered as a fragment and listed under 'app_owned'" in p
                for p in problems
            )
        )

    def test_self_wiring_row_missing_is_drift(self) -> None:
        rows = ["redocly.yaml", "contracts/**"] + [
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
        ]
        self._write_fixture(frags=frags, app_owned=[])
        problems = self._problems()
        self.assertTrue(any("share merge_rank 1" in p for p in problems))

    def test_real_tree_is_consistent(self) -> None:
        root = pathlib.Path(__file__).resolve().parent.parent
        problems, n_frags, n_merged, n_app_owned = m.check(str(root))
        self.assertEqual(
            problems,
            [],
            "the real tree must satisfy the fragment-enumeration gate "
            "(this is the check the api-contract job's drift step runs); "
            f"found {len(problems)} drift(s)",
        )
        self.assertEqual(n_frags, 11)
        self.assertEqual(n_merged, 11)
        self.assertEqual(n_app_owned, 3)


if __name__ == "__main__":
    unittest.main()
