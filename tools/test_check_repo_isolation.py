#!/usr/bin/env python3
"""Unit tests for check_repo_isolation.py's coverage rules.

Stdlib-only (unittest + tempfile), matching this directory's own "plain
executables with no third-party dependencies" convention (tools/README.md's
"Running in CI and locally" section). Run directly:

    python3 tools/test_check_repo_isolation.py

Regression coverage for the internal/testutil exemption and the
equivalent-suite rule:

  * internal/testutil is the repo-mandated home of shared test helpers;
    test doubles there embed dbkit.Repository[T] so OTHER packages'
    tests can exercise the real base, and the checker must not demand a
    tenancytest.AssertIsolated call beside them.
    test_internal_testutil_fakes_are_not_candidates fails before the
    exemption (exit 1 on a module whose only embedding lives under
    internal/testutil).
  * A repository embedding whose record cannot satisfy tenancytest's ID
    convention is covered by the in-package equivalent suite named
    Test<TypeName>_AssertIsolated (the smilesim shape).
    test_equivalent_suite_name_is_recognized fails before that rule.
  * A genuinely uncovered repository still fails the check -- the rule
    must never go soft on the shape it guards
    (test_uncovered_repository_still_fails).
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_repo_isolation as m  # noqa: E402

# Faithful Go text for the fixtures. Files never need to compile -- the
# checker is textual -- but they mirror the real shapes (imports, the
# embedding, the AssertIsolated call) so the tests cannot pass for the
# wrong reason.
_DBKIT_IMPORT = '"github.com/vislake/speed/go/dbkit"'
_TT_IMPORT = '"github.com/vislake/speed/go/tenancy/tenancytest"'

_REPOSITORY_GO = """\
package alpha

import (
	{dbkit}

	"github.com/vislake/speed/go/pkgcore"
)

type Widget struct {{
	ID       string `gorm:"primaryKey"`
	TenantID string
}}

func (w Widget) GetTenantID() pkgcore.TenantID {{
	return pkgcore.TenantID(w.TenantID)
}}

type Repository struct {{
	*dbkit.Repository[Widget]
}}
""".format(dbkit=_DBKIT_IMPORT)

_ASSERT_ISOLATED_TEST_GO = """\
package alpha

import (
	"testing"

	{dbkit}
	{tt}

	"github.com/vislake/speed/go/pkgcore"
)

func TestRepository_AssertIsolated(t *testing.T) {{
	repo := NewRepository(nil)
	tenancytest.AssertIsolated(t, repo.Repository, func(tenant pkgcore.TenantID) *Widget {{
		return &Widget{{ID: "w", TenantID: string(tenant)}}
	}})
}}
""".format(dbkit=_DBKIT_IMPORT, tt=_TT_IMPORT)

_FAKE_GO = """\
package testutil

import (
	{dbkit}

	"github.com/vislake/speed/go/pkgcore"
)

// FakeRepository is a shared test double: other packages embed it to
// exercise the real generic base, so no tenancytest call belongs here.
type FakeRepository struct {{
	*dbkit.Repository[FakeWidget]
}}
""".format(dbkit=_DBKIT_IMPORT)


class RunTests(unittest.TestCase):
    def _root_with(self, files: dict[str, str]) -> str:
        td = tempfile.TemporaryDirectory()
        self.addCleanup(td.cleanup)
        for rel, content in files.items():
            path = pathlib.Path(td.name) / rel
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content, encoding="utf-8")
        return td.name

    def _module_files(self, extra: dict[str, str]) -> dict[str, str]:
        files = {
            "alpha/go.mod": "module example.test/alpha\n\ngo 1.25\n",
        }
        files.update(extra)
        return files

    def test_covered_repository_passes(self):
        root = self._root_with(self._module_files({
            "alpha/repository.go": _REPOSITORY_GO,
            "alpha/repository_test.go": _ASSERT_ISOLATED_TEST_GO,
        }))
        self.assertEqual(m.run(root), 0)

    def test_internal_testutil_fakes_are_not_candidates(self):
        # The module's ONLY Repository embedding lives under
        # internal/testutil (the repo-mandated shared-test-helpers home):
        # nothing may be required of it, so the module passes with no
        # test file at all. Fails before the internal/testutil exemption
        # -- without it, the fake is reported uncovered.
        root = self._root_with(self._module_files({
            "alpha/internal/testutil/fake.go": _FAKE_GO,
        }))
        self.assertEqual(m.run(root), 0)

    def test_uncovered_repository_still_fails(self):
        # A genuinely uncovered tenant-scoped repository -- a shipped
        # embedding with no tenancytest call anywhere in its package's
        # tests -- must keep failing the check.
        root = self._root_with(self._module_files({
            "alpha/repository.go": _REPOSITORY_GO,
        }))
        self.assertEqual(m.run(root), 1)

    def test_uncovered_repository_with_unrelated_calls_still_fails(self):
        # Multi-candidate discipline: in a package with two repository
        # types, an AssertIsolated call naming one type's model does not
        # cover the other -- the per-type mention rule stays enforced.
        two_repositories_go = _REPOSITORY_GO + """

type Photo struct {
	ID       string `gorm:"primaryKey"`
	TenantID string
}

func (p Photo) GetTenantID() pkgcore.TenantID {
	return pkgcore.TenantID(p.TenantID)
}

type PhotoRepository struct {
	*dbkit.Repository[Photo]
}
"""
        root = self._root_with(self._module_files({
            "alpha/repository.go": two_repositories_go,
            "alpha/repository_test.go": _ASSERT_ISOLATED_TEST_GO,
        }))
        self.assertEqual(m.run(root), 1)

    def test_equivalent_suite_name_is_recognized(self):
        # A Repository embedding whose record cannot satisfy tenancytest's
        # ID convention (a Create-only embedding over a differently-keyed
        # primary key -- the smilesim/SimulationStore shape) is covered by
        # the package's equivalent suite named exactly
        # Test<TypeName>_AssertIsolated, with no tenancytest import at all.
        store_go = """\
package alpha

import (
	{dbkit}

	"github.com/vislake/speed/go/pkgcore"
)

type simulationRecord struct {{
	JobID    string `gorm:"primaryKey"`
	TenantID string
}}

func (r simulationRecord) GetTenantID() pkgcore.TenantID {{
	return pkgcore.TenantID(r.TenantID)
}}

type SimulationStore struct {{
	*dbkit.Repository[simulationRecord]
}}
""".format(dbkit=_DBKIT_IMPORT)
        suite_go = """\
package alpha

import (
	"context"
	"testing"

	"github.com/vislake/speed/go/pkgcore"
)

func TestSimulationStore_AssertIsolated(t *testing.T) {{
	store := &SimulationStore{{}}
	ctx := pkgcore.WithTenant(context.Background(), "t-a")
	_ = store
	_ = ctx
}}
""".format(dbkit=_DBKIT_IMPORT)
        root = self._root_with(self._module_files({
            "alpha/simulation_store.go": store_go,
            "alpha/simulation_store_test.go": suite_go,
        }))
        self.assertEqual(m.run(root), 0)

    def test_near_miss_suite_name_is_not_recognized(self):
        # The equivalent-suite name is canonical and exact: a differently
        # named isolation test (no AssertIsolated marker in the function
        # name) does not cover the embedding.
        store_go = """\
package alpha

import (
	{dbkit}

	"github.com/vislake/speed/go/pkgcore"
)

type simulationRecord struct {{
	JobID    string `gorm:"primaryKey"`
	TenantID string
}}

func (r simulationRecord) GetTenantID() pkgcore.TenantID {{
	return pkgcore.TenantID(r.TenantID)
}}

type SimulationStore struct {{
	*dbkit.Repository[simulationRecord]
}}
""".format(dbkit=_DBKIT_IMPORT)
        suite_go = """\
package alpha

import (
	"testing"
)

func TestSimulationStore_Isolation(t *testing.T) {{
}}
"""
        root = self._root_with(self._module_files({
            "alpha/simulation_store.go": store_go,
            "alpha/simulation_store_test.go": suite_go,
        }))
        self.assertEqual(m.run(root), 1)


if __name__ == "__main__":
    unittest.main()
