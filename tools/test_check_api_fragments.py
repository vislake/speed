#!/usr/bin/env python3
"""Unit tests for check_api_fragments.py's drift contract.

Stdlib-only (unittest + tempfile), matching this directory's own "plain
executables with no third-party dependencies" convention (tools/README.md's
"Running the checks" section). Run directly:

    python3 tools/test_check_api_fragments.py

The fixture tree is a small fake repository: a tools/api_fragments.json
manifest, the real tools/api_fragment_matrix.py derivation (copied, so
the gate can execute it; drift tests may plant a variant instead), and
one api/ directory per manifest entry carrying openapi.yaml +
oapi-codegen.yaml. Every planted drift below must make check() report it
(exit 1 in the CLI); the machinery-level tests prove the checker catches
the drift classes the gate exists for -- a fragment added to the tree
without the manifest, a manifest entry whose files vanished, a manifest
malformed in itself, and the derivation decoupled from the manifest --
and test_real_tree_is_consistent runs the real check against this
repository.

The fixture mirrors the real tree's two fragment classes: platform
fragments registered in the manifest (alpha and beta, both merged) and
the reference app's own fragments, listed under the manifest's
`app_owned` entries instead (gamma -- the app's fragments join the
app's own merged document and SDK through the app-owned generation
leg, never the platform one).

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
  * a manifest malformed in itself: two fragments sharing a merge_rank
    (test_duplicate_merge_rank_is_drift);
  * the derivation decoupled from the manifest: a derivation whose
    output drops a fragment, reorders them or carries keys its
    consumers do not read, and a derivation script missing altogether
    (the gate executes the script against the tree, so a planted
    variant goes red even though the manifest itself is well formed)
    (test_derivation_dropping_a_fragment_is_drift,
    test_derivation_reordering_fragments_is_drift,
    test_derivation_emitting_unknown_keys_is_drift,
    test_missing_derivation_script_is_drift).

Infrastructure errors (the manifest missing or unreadable) must exit 2,
distinct from drift (test_*_exits_2).
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


def manifest_text(
    frags: list[dict], app_owned: list[str] | None = None
) -> str:
    if app_owned is None:
        app_owned = APP_OWNED
    return json.dumps({"fragments": frags, "app_owned": app_owned}, indent=2) + "\n"


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
        with_script: bool = True,
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
        if not with_script:
            script.unlink(missing_ok=True)
        elif script_text is None:
            shutil.copyfile(
                pathlib.Path(__file__).resolve().parent
                / "api_fragment_matrix.py",
                script,
            )
        else:
            script.write_text(script_text, encoding="utf-8")

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

    def test_derivation_dropping_a_fragment_is_drift(self) -> None:
        # A derivation whose output silently drops a fragment: the gate
        # executes the script against the tree, so the planted variant
        # goes red even though the manifest is well formed and matches
        # the tree.
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

    def test_derivation_reordering_fragments_is_drift(self) -> None:
        # Manifest order is what the merge leg joins in, so a derivation
        # that reorders the entries changes the merged document.
        script = (
            "import json\n"
            "data = json.load(open('tools/api_fragments.json'))\n"
            "entries = [\n"
            "    {'name': f['name'], 'dir': f['dir']}\n"
            "    for f in reversed(data['fragments'])\n"
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

    def test_derivation_emitting_unknown_keys_is_drift(self) -> None:
        script = (
            "import json\n"
            "data = json.load(open('tools/api_fragments.json'))\n"
            "entries = [\n"
            "    {'name': f['name'], 'dir': f['dir'], 'rank': 1}\n"
            "    for f in data['fragments']\n"
            "]\n"
            "print(json.dumps(entries))\n"
        )
        self._write_fixture(script_text=script)
        problems = self._problems()
        self.assertTrue(
            any("keys its consumers do not read" in p for p in problems)
        )

    def test_missing_derivation_script_is_drift(self) -> None:
        self._write_fixture(with_script=False)
        problems = self._problems()
        self.assertTrue(
            any("api_fragment_matrix.py is missing" in p for p in problems)
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
            "the real tree must satisfy the fragment-enumeration gate; "
            f"found {len(problems)} drift(s)",
        )
        self.assertEqual(n_frags, 11)
        self.assertEqual(n_merged, 11)
        self.assertEqual(n_app_owned, 3)


if __name__ == "__main__":
    unittest.main()
