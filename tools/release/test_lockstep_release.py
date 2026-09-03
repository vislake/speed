#!/usr/bin/env python3
"""Self-tests for the lockstep release coordinator.

Runnable three ways, all equivalent:

    python3 tools/release/lockstep-release.py --self-test
    python3 -m unittest discover -s tools/release
    python3 -m unittest tools.release.test_lockstep_release

The module under test is lockstep-release.py, whose hyphenated filename
cannot be imported by identifier; it is loaded through importlib
(spec_from_file_location) instead, mirroring how the coordinator's
--self-test mode runs this suite.

What the suite proves (M0 exit-condition evidence):

  * The version-form contract: the release-version regex accepts and
    rejects exactly the intended shapes (v prefix required, pre-release
    suffixes allowed).
  * go.work parsing and the runtime module-set derivation, including the
    drift gates: a go/ module missing from go.work, a go.work entry
    without a go.mod, and a consumer without a go.mod all fail loudly.
  * The npm half: uniform-version enforcement, and the web/.changeset
    fixed group covering exactly the packages found (both directions of
    the mismatch fail).
  * The first-release replace-cleanup engine as pure functions against
    the fixtures under testdata/ (never against any live go.mod): a
    cleanable transition-state go.mod yields exactly the sibling replaces
    in line order; a consumer-shaped go.mod is left alone; an untidy or
    mismatched replace raises UncleanableReplaceError.
  * The CLI gates: usage errors exit 2, --apply without the escape hatch
    exits 3 with the M4/v1.0 refusal, duplicate versions exit 1.
  * The sandbox end-to-end proof: a scratch git repository built from the
    live tree's module metadata (go.work, go/*/go.mod, web/packages/*
    package.json files, web/.changeset/config.json) produces, in default
    mode, the aggregated one-version plan at exit 0; the hatched --apply
    mode creates exactly the expected tag set -- 21 tags, one version,
    nothing extra -- and the real repository's tag set is untouched
    before and after. The npm half of the proof reports which gate ran:
    the uniform-version JSON assertion always, and -- when pnpm is on
    PATH and web/node_modules exists in the live checkout -- a real
    offline `pnpm pack` of every package proving the workspace:* rewrite
    that publishing will need.
"""

from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
import unittest.mock

TEST_DIR = os.path.dirname(os.path.abspath(__file__))
COORDINATOR_PATH = os.path.join(TEST_DIR, "lockstep-release.py")
FIXTURES_DIR = os.path.join(TEST_DIR, "testdata")

# The version the sandbox proofs use: a pre-release form that can never be
# confused with a real release version.
SELFTEST_VERSION = "v0.0.0-selftest"


def load_coordinator():
    """Import the hyphenated coordinator module through importlib."""
    spec = importlib.util.spec_from_file_location(
        "lockstep_release", COORDINATOR_PATH
    )
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


rel = load_coordinator()

# The live repository root (this worktree): found by walking up from this
# test file to the go.work marker, exactly as the coordinator finds it.
LIVE_ROOT = rel.find_repo_root(TEST_DIR)


def _write_file(path: str, text: str) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as fh:
        fh.write(text)


def _read_fixture(name: str) -> str:
    with open(os.path.join(FIXTURES_DIR, name, "go.mod"), encoding="utf-8") as fh:
        return fh.read()


def run_cli(cwd: str, *args: str) -> tuple[int, str, str]:
    """Run the coordinator as a subprocess; return (rc, stdout, stderr)."""
    proc = subprocess.run(
        [sys.executable, COORDINATOR_PATH, *args],
        cwd=cwd,
        capture_output=True,
        text=True,
        check=False,
    )
    return proc.returncode, proc.stdout, proc.stderr


class VersionPatternTest(unittest.TestCase):
    """The release-version form: the contract of the CI version input."""

    def test_valid_versions(self) -> None:
        valid = [
            "v0.0.1",
            "v1.2.3",
            "v10.20.30",
            "v1.2.3-rc.1",
            "v1.2.3-alpha.1",
            "v1.2.3-0",
            "v1.2.3-RC-2.x",
            "v0.0.0-selftest",
        ]
        for version in valid:
            with self.subTest(version=version):
                self.assertIsNotNone(rel.VERSION_PATTERN.fullmatch(version))

    def test_invalid_versions(self) -> None:
        invalid = [
            "0.3.0",          # missing the required leading v
            "v1",             # missing MINOR
            "v1.2",           # missing PATCH
            "v1.2.3.4",       # extra numeric segment
            "1.2.3-rc.1",     # missing the leading v
            "V1.2.3",         # uppercase V
            "v1.2.3+meta",    # build metadata is not a release-version form
            "v1.2.3 ",        # trailing space
            "v1.2.3-",        # empty pre-release
            "v1.2.3_rc",      # underscore is not in the class
            "v1.2.3-rc@1",    # @ is not in the class
            "version-v1.2.3", # prefix junk
            "v1.2.0-..",      # empty pre-release identifier; git rejects ".."
            "v1.2.0-.",       # bare-dot pre-release; git rejects a trailing "."
            "v1.2.0-x..y",    # empty identifier between two dots
            "v1.2.3\n",       # trailing newline: main() must reject it too
            "",               # empty
        ]
        for version in invalid:
            with self.subTest(version=version):
                self.assertIsNone(rel.VERSION_PATTERN.fullmatch(version))


class GoWorkParseTest(unittest.TestCase):
    """go.work use-block parsing, the source of the Go module set."""

    def test_block_form(self) -> None:
        text = (
            "go 1.25.0\n"
            "\n"
            "use (\n"
            "\t./examples/reference-app\n"
            "\t./go/alpha\n"
            "\t./go/beta\n"
            ")\n"
        )
        self.assertEqual(
            rel.parse_gowork_uses(text),
            ["examples/reference-app", "go/alpha", "go/beta"],
        )

    def test_single_line_and_comment_tolerance(self) -> None:
        text = (
            "// workspace file\n"
            "use ./go/alpha ./go/beta ./examples/reference-app\n"
            "godebug default=go1.25\n"
        )
        self.assertEqual(
            rel.parse_gowork_uses(text),
            ["go/alpha", "go/beta", "examples/reference-app"],
        )

    def test_trailing_closing_parenthesis_on_an_entry_line(self) -> None:
        text = "use (\n\t./go/alpha\n\t./go/beta)\n"
        self.assertEqual(rel.parse_gowork_uses(text), ["go/alpha", "go/beta"])

    def test_unterminated_block_raises(self) -> None:
        with self.assertRaises(rel.ReleaseError):
            rel.parse_gowork_uses("use (\n\t./go/alpha\n")

    def test_stray_closer_raises(self) -> None:
        with self.assertRaises(rel.ReleaseError):
            rel.parse_gowork_uses(")\nuse (\n\t./go/alpha\n)\n")


class DerivationDriftTest(unittest.TestCase):
    """Runtime Go module-set derivation and its drift gates."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.root = self._tmp.name

    def _go_work(self, text: str) -> None:
        _write_file(os.path.join(self.root, "go.work"), text)

    def _module(self, name: str) -> None:
        _write_file(
            os.path.join(self.root, "go", name, "go.mod"),
            f"module github.com/vislake/speed/go/{name}\n",
        )

    def test_happy_tree_derives_sorted_modules(self) -> None:
        self._module("beta")
        self._module("alpha")
        self._module("zeta")
        self._go_work("use (\n\t./go/alpha\n\t./go/beta\n\t./go/zeta\n)\n")
        modules, consumers = rel.derive_go_modules(self.root)
        self.assertEqual(modules, ["alpha", "beta", "zeta"])
        self.assertEqual(consumers, [])

    def test_consumer_module_derived_but_not_publishable(self) -> None:
        self._module("alpha")
        _write_file(
            os.path.join(self.root, "examples", "refapp", "go.mod"),
            "module github.com/vislake/speed/examples/refapp\n",
        )
        self._go_work(
            "use (\n\t./examples/refapp\n\t./go/alpha\n)\n"
        )
        modules, consumers = rel.derive_go_modules(self.root)
        self.assertEqual(modules, ["alpha"])
        self.assertEqual(consumers, ["examples/refapp"])

    def test_module_line_disagreeing_with_its_directory_fails_loudly(self) -> None:
        """The plan derives each tag as go/<directory>/<version>.

        A go.mod declaring a module path other than its directory name
        would be tagged under a path Go consumers could never resolve, yet
        before this gate the plan was all-green: the derivation checked
        only that a go.mod FILE existed, never what it declared.
        """
        self._module("alpha")
        _write_file(
            os.path.join(self.root, "go", "foo", "go.mod"),
            "module github.com/vislake/speed/go/bar\n",
        )
        self._go_work("use (\n\t./go/alpha\n\t./go/foo\n)\n")
        with self.assertRaises(rel.ReleaseError) as cm:
            rel.derive_go_modules(self.root)
        message = str(cm.exception)
        self.assertIn("github.com/vislake/speed/go/bar", message)
        self.assertIn("github.com/vislake/speed/go/foo", message)

    def test_module_without_a_module_line_fails_loudly(self) -> None:
        self._module("alpha")
        _write_file(
            os.path.join(self.root, "go", "foo", "go.mod"), "go 1.25.0\n"
        )
        self._go_work("use (\n\t./go/alpha\n\t./go/foo\n)\n")
        with self.assertRaises(rel.ReleaseError) as cm:
            rel.derive_go_modules(self.root)
        self.assertIn("no module directive", str(cm.exception))

    def test_module_missing_from_go_work_fails_loudly(self) -> None:
        """Drift gate: a go/ module the go.work does not register."""
        self._module("alpha")
        self._module("beta")
        self._go_work("use (\n\t./go/alpha\n)\n")
        with self.assertRaises(rel.ReleaseError) as cm:
            rel.derive_go_modules(self.root)
        message = str(cm.exception)
        self.assertIn("beta", message)
        self.assertIn("no go.work use entry", message)

    def test_go_work_entry_without_go_mod_fails_loudly(self) -> None:
        self._module("alpha")
        os.makedirs(os.path.join(self.root, "go", "ghost"), exist_ok=True)
        self._go_work("use (\n\t./go/alpha\n\t./go/ghost\n)\n")
        with self.assertRaises(rel.ReleaseError) as cm:
            rel.derive_go_modules(self.root)
        message = str(cm.exception)
        self.assertIn("ghost", message)
        self.assertIn("go.mod does not exist", message)

    def test_consumer_without_go_mod_fails_loudly(self) -> None:
        self._module("alpha")
        self._go_work(
            "use (\n\t./examples/refapp\n\t./go/alpha\n)\n"
        )
        with self.assertRaises(rel.ReleaseError) as cm:
            rel.derive_go_modules(self.root)
        message = str(cm.exception)
        self.assertIn("consumer module", message)
        self.assertIn("go.mod does not exist", message)


class NpmDerivationTest(unittest.TestCase):
    """npm half: uniform versions and fixed-group coverage."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.root = self._tmp.name
        self.packages_dir = os.path.join(self.root, "web", "packages")

    def _package(self, name: str, version: str) -> None:
        _write_file(
            os.path.join(self.packages_dir, name, "package.json"),
            json.dumps({"name": f"@speed/{name}", "version": version}),
        )

    def _changesets_config(self, groups: list[list[str]]) -> None:
        _write_file(
            os.path.join(self.root, "web", ".changeset", "config.json"),
            json.dumps({"fixed": groups}),
        )

    def test_uniform_packages_derive_sorted(self) -> None:
        self._package("zeta", "0.0.0")
        self._package("alpha", "0.0.0")
        packages = rel.derive_npm_packages(self.root)
        self.assertEqual(
            [p["name"] for p in packages],
            ["@speed/alpha", "@speed/zeta"],
        )
        rel.check_npm_uniform(packages)  # must not raise

    def test_non_uniform_versions_fail(self) -> None:
        self._package("alpha", "0.0.0")
        self._package("beta", "0.0.1")
        packages = rel.derive_npm_packages(self.root)
        with self.assertRaises(rel.ReleaseError) as cm:
            rel.check_npm_uniform(packages)
        self.assertIn("not uniform", str(cm.exception))

    def test_fixed_group_covering_exactly_the_packages_passes(self) -> None:
        self._package("alpha", "0.0.0")
        self._package("beta", "0.0.0")
        self._changesets_config([["@speed/alpha", "@speed/beta"]])
        packages = rel.derive_npm_packages(self.root)
        fixed = rel.load_changesets_fixed_group(self.root)
        rel.check_changesets_coverage(fixed, packages)  # must not raise

    def test_fixed_group_coverage_mismatch_fails_both_ways(self) -> None:
        self._package("alpha", "0.0.0")
        self._package("beta", "0.0.0")
        # Fixed group names a package that does not exist; the packages
        # half names one the group misses.
        self._changesets_config([["@speed/alpha", "@speed/gamma"]])
        packages = rel.derive_npm_packages(self.root)
        fixed = rel.load_changesets_fixed_group(self.root)
        with self.assertRaises(rel.ReleaseError) as cm:
            rel.check_changesets_coverage(fixed, packages)
        message = str(cm.exception)
        self.assertIn("beta", message)   # missing from the fixed group
        self.assertIn("gamma", message)  # listed but not a package
        # A package joins the workspace without its fixed-group entry.
        self._package("gamma", "0.0.0")
        self._changesets_config([["@speed/alpha", "@speed/beta"]])
        packages = rel.derive_npm_packages(self.root)
        fixed = rel.load_changesets_fixed_group(self.root)
        with self.assertRaises(rel.ReleaseError) as cm:
            rel.check_changesets_coverage(fixed, packages)
        self.assertIn("gamma", str(cm.exception))

    def test_missing_changesets_config_fails(self) -> None:
        self._package("alpha", "0.0.0")
        with self.assertRaises(rel.ReleaseError) as cm:
            rel.load_changesets_fixed_group(self.root)
        message = str(cm.exception)
        self.assertIn("changesets", message)
        self.assertIn("must exist", message)

    def test_single_member_group_is_invalid(self) -> None:
        self._package("alpha", "0.0.0")
        self._changesets_config([["@speed/alpha"]])
        with self.assertRaises(rel.ReleaseError) as cm:
            rel.load_changesets_fixed_group(self.root)
        self.assertIn("at least two", str(cm.exception))


class CleanupEngineTest(unittest.TestCase):
    """The first-release replace cleanup, exercised ONLY against fixtures.

    testdata/cleanable  -- a publishable module in the transition state:
    every in-repo replace is the sibling form the first release deletes
    (single-line and block shapes); a third-party fork pin must be left
    alone.
    testdata/consumer   -- the reference-app shape: module outside the
    publishable prefix; the cleanup must leave it alone entirely.
    testdata/untidy     -- publishable module carrying a consumer-shaped
    replace (../../go/...): the engine must refuse, not plan an edit.
    testdata/mismatched -- publishable module whose sibling replace
    points at the wrong directory: the engine must refuse.
    """

    def test_cleanable_module_yields_sibling_replaces_in_line_order(self) -> None:
        module, drops = rel.first_release_replace_cleanup(
            _read_fixture("cleanable")
        )
        self.assertEqual(module, "github.com/vislake/speed/go/jobs")
        self.assertEqual(
            drops,
            (
                "github.com/vislake/speed/go/pkgcore",
                "github.com/vislake/speed/go/tenancy",
                "github.com/vislake/speed/go/observability",
                "github.com/vislake/speed/go/dbkit",
            ),
        )

    def test_commented_transitional_replace_is_planned_in_both_shapes(self) -> None:
        """A trailing line comment must not change what the cleanup plans.

        `replace X => ../X // note` is legal go.mod (go mod edit parses it
        and preserves the replace), so both parser branches have to see
        through the comment. Before the comment-stripping fix the
        single-line branch silently ignored such a line -- planning no
        drop and raising nothing -- while the block branch refused the
        file outright. Either way the first release could have shipped a
        module whose go.mod still pointed at ../pkgcore.
        """
        for fixture in ("cleanable-commented", "cleanable-block-commented"):
            with self.subTest(fixture=fixture):
                module, drops = rel.first_release_replace_cleanup(
                    _read_fixture(fixture)
                )
                self.assertEqual(module, "github.com/vislake/speed/go/config")
                self.assertEqual(
                    drops, ("github.com/vislake/speed/go/pkgcore",)
                )

    def test_consumer_module_is_left_alone(self) -> None:
        module, drops = rel.first_release_replace_cleanup(
            _read_fixture("consumer")
        )
        self.assertEqual(
            module, "github.com/vislake/speed/examples/reference-app"
        )
        self.assertEqual(drops, ())

    def test_untidy_consumer_shaped_replace_is_refused(self) -> None:
        with self.assertRaises(rel.UncleanableReplaceError) as cm:
            rel.first_release_replace_cleanup(_read_fixture("untidy"))
        message = str(cm.exception)
        self.assertIn("../../go/pkgcore", message)
        self.assertIn("sibling form", message)

    def test_mismatched_sibling_replace_is_refused(self) -> None:
        with self.assertRaises(rel.UncleanableReplaceError) as cm:
            rel.first_release_replace_cleanup(_read_fixture("mismatched"))
        message = str(cm.exception)
        self.assertIn("../tenancy", message)
        self.assertIn("sibling form", message)
        self.assertIn("github.com/vislake/speed/go/pkgcore", message)

    def test_go_mod_without_module_directive_is_refused(self) -> None:
        with self.assertRaises(rel.UncleanableReplaceError):
            rel.first_release_replace_cleanup("go 1.25.0\n")


class CliGateTest(unittest.TestCase):
    """CLI-level gates that need no repository: usage and refusal codes."""

    def _run_main(self, *args: str) -> tuple[int, str, str]:
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            rc = rel.main(list(args))
        return rc, out.getvalue(), err.getvalue()

    def test_bad_version_exits_2(self) -> None:
        rc, _, err = self._run_main("1.2.3")
        self.assertEqual(rc, 2)
        self.assertIn("release-version form", err)
        self.assertIn("leading 'v'", err)

    def test_version_with_a_trailing_newline_exits_2(self) -> None:
        """main() validates with fullmatch, as the pattern tests do.

        re.match with a "$" anchor accepts a trailing newline, so before
        the fullmatch fix the CLI accepted a version the self-test's own
        pattern assertions reject -- the two disagreed about the contract
        they both claim to enforce.
        """
        rc, _, err = self._run_main("v1.2.3\n")
        self.assertEqual(rc, 2)
        self.assertIn("release-version form", err)

    def test_missing_version_exits_2(self) -> None:
        with self.assertRaises(SystemExit) as cm:
            rel.main([])
        self.assertEqual(cm.exception.code, 2)

    def test_self_test_with_other_arguments_exits_2(self) -> None:
        rc, _, err = self._run_main("--self-test", "v1.2.3")
        self.assertEqual(rc, 2)
        self.assertIn("no other arguments", err)

    def test_apply_without_escape_hatch_exits_3(self) -> None:
        """The hard gate: --apply alone refuses with the M4/v1.0 statement."""
        with tempfile.TemporaryDirectory() as tmp:
            _write_file(os.path.join(tmp, "go.work"), "use (\n)\n")
            with unittest.mock.patch.object(
                rel, "find_repo_root", return_value=tmp
            ):
                rc, _, err = self._run_main("--apply", "v1.2.3")
        self.assertEqual(rc, 3)
        self.assertIn("refused", err)
        self.assertIn("v1.0", err)
        self.assertIn("M4", err)
        self.assertIn("no publish credential", err)


class SandboxProofTest(unittest.TestCase):
    """End-to-end proofs against a scratch git repository.

    The sandbox is built from the LIVE tree's module metadata (go.work,
    go/*/go.mod, web/packages/*/package.json, web/.changeset/config.json)
    and git-initialized with one commit, so every coordinator mode that
    needs a repository runs against it while the real repository stays
    untouched -- provably: its tag set is compared before and after.
    """

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmp.cleanup)
        self.live_tags_before = rel.list_existing_tags(LIVE_ROOT)

    def build_sandbox(self, tmp_dir: str) -> str:
        sb = os.path.join(tmp_dir, "sandbox")
        os.makedirs(os.path.join(sb, "go"))
        shutil.copy2(
            os.path.join(LIVE_ROOT, "go.work"),
            os.path.join(sb, "go.work"),
        )
        for d in sorted(os.listdir(os.path.join(LIVE_ROOT, "go"))):
            src = os.path.join(LIVE_ROOT, "go", d, "go.mod")
            if os.path.isfile(src):
                os.makedirs(os.path.join(sb, "go", d), exist_ok=True)
                shutil.copy2(src, os.path.join(sb, "go", d, "go.mod"))
        # Consumer modules (today: examples/reference-app) come along too:
        # the coordinator validates their go.mod presence, and the sandbox
        # must mirror the live tree completely.
        with open(os.path.join(LIVE_ROOT, "go.work"), encoding="utf-8") as fh:
            go_work_text = fh.read()
        for entry in rel.parse_gowork_uses(go_work_text):
            if entry.startswith("go/"):
                continue
            src = os.path.join(LIVE_ROOT, entry, "go.mod")
            if os.path.isfile(src):
                os.makedirs(os.path.join(sb, entry), exist_ok=True)
                shutil.copy2(src, os.path.join(sb, entry, "go.mod"))
        for d in sorted(
            os.listdir(os.path.join(LIVE_ROOT, "web", "packages"))
        ):
            src = os.path.join(
                LIVE_ROOT, "web", "packages", d, "package.json"
            )
            if os.path.isfile(src):
                os.makedirs(
                    os.path.join(sb, "web", "packages", d), exist_ok=True
                )
                shutil.copy2(
                    src, os.path.join(sb, "web", "packages", d, "package.json")
                )
        changeset_src = os.path.join(LIVE_ROOT, "web", ".changeset")
        changeset_dst = os.path.join(sb, "web", ".changeset")
        if os.path.isfile(os.path.join(changeset_src, "config.json")):
            os.makedirs(changeset_dst, exist_ok=True)
            shutil.copy2(
                os.path.join(changeset_src, "config.json"),
                os.path.join(changeset_dst, "config.json"),
            )
        for git_cmd in (
            ("init", "-q"),
            ("config", "user.email", "lockstep-self-test@localhost"),
            ("config", "user.name", "lockstep self-test"),
            ("add", "-A"),
            ("commit", "-q", "-m", "sandbox initial state"),
        ):
            rel.run_git(sb, *git_cmd)
        return sb

    def expected_sandbox_tags(self, sb: str) -> list[str]:
        modules, _consumers = rel.derive_go_modules(sb)
        return sorted(f"go/{d}/{SELFTEST_VERSION}" for d in modules)

    def test_end_to_end_proof(self) -> None:
        """Default plan, hard refusal, hatched local tags: one version set."""
        with tempfile.TemporaryDirectory() as tmp:
            sb = self.build_sandbox(tmp)

            # Default mode: aggregated one-version plan at exit 0.
            rc, out, err = run_cli(sb, SELFTEST_VERSION)
            self.assertEqual(rc, 0, err)
            self.assertIn("Dry run:", out)
            modules, _ = rel.derive_go_modules(sb)
            npm_packages = rel.derive_npm_packages(sb)
            closing = (
                f"{len(modules)} Go modules + {len(npm_packages)} packages "
                f"-> {SELFTEST_VERSION}"
            )
            self.assertIn(closing, out)
            self.assertIn(
                "go/dbkit -> go/dbkit/" + SELFTEST_VERSION, out
            )
            self.assertEqual(rel.list_existing_tags(sb), set())

            # --apply without the escape hatch: hard refusal, exit 3.
            rc, _, err = run_cli(sb, "--apply", SELFTEST_VERSION)
            self.assertEqual(rc, 3)
            self.assertIn("M4", err)
            self.assertIn("v1.0", err)

            # Hatched --apply: exactly the expected tag set, nothing extra.
            rc, out, err = run_cli(
                sb, "--apply", SELFTEST_VERSION,
                "--allow-local-tag-creation",
            )
            self.assertEqual(rc, 0, err)
            self.assertIn("NOT done by this M0 round", out)
            self.assertIn("no publish credential", out)
            expected = self.expected_sandbox_tags(sb)
            actual = sorted(rel.list_existing_tags(sb))
            self.assertEqual(actual, expected)
            self.assertEqual(len(actual), len(expected))

            # npm half proof, reporting which gate ran.
            versions = {p["version"] for p in npm_packages}
            self.assertEqual(versions, {"0.0.0"})
            print(
                "[self-test] npm gate: uniform-version JSON assertion "
                f"passed for {len(npm_packages)} packages "
                f"(versions: {sorted(versions)})"
            )
            packed = self._try_offline_pnpm_pack()
            if packed is None:
                print(
                    "[self-test] npm gate: pnpm pack proof skipped -- pnpm "
                    "not on PATH or web/node_modules absent in the live "
                    "checkout; the uniform-version JSON assertion is the "
                    "M0 gate"
                )
            else:
                print(
                    "[self-test] npm gate: offline pnpm pack proof ran for "
                    f"{packed} packages"
                )

        # The live repository is untouched: same tag set as before.
        self.assertEqual(rel.list_existing_tags(LIVE_ROOT), self.live_tags_before)

    def _try_offline_pnpm_pack(self) -> int | None:
        """Pack every live web package offline; None when not possible."""
        if shutil.which("pnpm") is None:
            return None
        if not os.path.isdir(os.path.join(LIVE_ROOT, "web", "node_modules")):
            return None
        web_root = os.path.join(LIVE_ROOT, "web")
        packed = 0
        with tempfile.TemporaryDirectory() as dest:
            for name in ("@speed/i18n", "@speed/tokens", "@speed/ui-kit"):
                proc = subprocess.run(
                    [
                        "pnpm", "--offline", "--filter", name, "pack",
                        "--pack-destination", dest,
                    ],
                    cwd=web_root,
                    capture_output=True,
                    text=True,
                    check=False,
                )
                if proc.returncode != 0:
                    print(
                        f"[self-test] npm gate: pnpm pack of {name} failed "
                        f"({proc.returncode}): {proc.stderr.strip()[:200]}"
                    )
                    return None
                packed += 1
        return packed

    def test_drift_negative_gate_exits_1(self) -> None:
        """A go/ module missing from go.work fails the CLI with exit 1."""
        with tempfile.TemporaryDirectory() as tmp:
            sb = self.build_sandbox(tmp)
            go_work_path = os.path.join(sb, "go.work")
            with open(go_work_path, encoding="utf-8") as fh:
                text = fh.read()
            modules, _ = rel.derive_go_modules(sb)
            dropped = modules[0]
            lines = [
                line for line in text.splitlines()
                if f"./go/{dropped}" not in line
            ]
            _write_file(go_work_path, "\n".join(lines) + "\n")
            rc, out, err = run_cli(sb, SELFTEST_VERSION)
            self.assertEqual(rc, 1)
            self.assertEqual(out, "")
            self.assertIn(dropped, err)
            self.assertIn("no go.work use entry", err)

    def test_duplicate_version_gate_exits_1(self) -> None:
        """An existing tag for the version fails the CLI with exit 1."""
        with tempfile.TemporaryDirectory() as tmp:
            sb = self.build_sandbox(tmp)
            modules, _ = rel.derive_go_modules(sb)
            rel.run_git(sb, "tag", f"go/{modules[0]}/{SELFTEST_VERSION}")
            rc, out, err = run_cli(sb, SELFTEST_VERSION)
            self.assertEqual(rc, 1)
            self.assertEqual(out, "")
            self.assertIn("already released", err)
            self.assertIn(f"go/{modules[0]}/{SELFTEST_VERSION}", err)


if __name__ == "__main__":
    unittest.main()
