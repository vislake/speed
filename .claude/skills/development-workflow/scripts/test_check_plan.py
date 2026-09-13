#!/usr/bin/env python3
"""Planted-violation suite for check_plan.py.

Stdlib-only (unittest + tempfile). Run directly:

    python3 .claude/skills/development-workflow/scripts/test_check_plan.py

Every test writes a conforming plan document to a temporary directory, plants
exactly one violation, and asserts the checker goes red naming it. The
conforming baseline is asserted green first -- without it a checker that
reports everything would pass the whole suite.
"""

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

import check_plan as m  # noqa: E402

PLAN = """# Add the second step

**Stage**: build
**Branch**: add-second-step

## Task

Add a second step to the runner.

## Work Items

| ID | Item | Owns | Depends on | Status | Updated |
|---|---|---|---|---|---|
| W1 | runner change | src/app.py | -- | done | 2026-09-13 14:20 |
| W2 | documentation | README.md | W1 | coding | 2026-09-13 14:30 |

### W1 — runner change

**Changes**: return two steps instead of one.

**Acceptance**:
- [x] run() returns 2
- [x] tests/test_app.py::test_second_step covers it

**Reviewer**: reviewer-a

### W2 — documentation

**Changes**: document the second step.

**Acceptance**:
- [ ] README names the second step

**Reviewer**: reviewer-b

## Progress Log

- `2026-09-13 14:20` `W1` coded, reviewed, no findings

## Open Findings

| ID | Item | Finding | Raised by | State |
|---|---|---|---|---|
| F1 | W2 | the wording is ambiguous | reviewer-b | open |

## Verification

| Command | Result | When |
|---|---|---|
| make test | pass | 2026-09-13 14:40 |

**Warnings**: None.

## Deliberate Omissions

- none

## Outcome

**Landed**: not landed: still building
"""

# The same plan at the end of its life: everything done, everything ticked, the
# finding settled and the landing recorded. This is what --final asks for.
ARCHIVED = (PLAN
            .replace("**Stage**: build", "**Stage**: done")
            .replace("| W1 | coding | 2026-09-13 14:30 |",
                     "| W1 | done | 2026-09-13 14:30 |")
            .replace("- [ ] README names the second step",
                     "- [x] README names the second step")
            .replace("| reviewer-b | open |", "| reviewer-b | fixed |")
            .replace("**Landed**: not landed: still building",
                     "**Landed**: `4d1bea6f` on main, fast-forward"))


class PlanCheckerTest(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.repo = pathlib.Path(self.tmp.name)
        self.plans = self.repo / ".claude" / "plans"
        self.plans.mkdir(parents=True)

    # -- helpers ---------------------------------------------------------

    def write(self, content: str, name: str = "add-second-step.md") -> pathlib.Path:
        path = self.plans / name
        path.write_text(content, encoding="utf-8")
        return path

    def findings(self, final: bool = False) -> list[str]:
        return [str(f) for path in sorted(self.plans.glob("*.md"))
                for f in m.check_plan(path, path.relative_to(self.repo).as_posix(),
                                      final=final)]

    def assert_green(self, final: bool = False) -> None:
        found = self.findings(final)
        self.assertEqual(found, [], f"expected no findings, got: {found}")

    def assert_red(self, fragment: str, final: bool = False) -> None:
        found = self.findings(final)
        self.assertTrue(any(fragment in f for f in found),
                        f"expected a finding containing {fragment!r}, got: {found}")

    # -- baseline --------------------------------------------------------

    def test_conforming_plan_is_green(self):
        self.write(PLAN)
        self.assert_green()

    def test_prose_in_another_language_is_green(self):
        """Prose follows the session's language; only the structure is fixed."""
        translated = (PLAN
                      .replace("# Add the second step", "# 添加第二步")
                      .replace("Add a second step to the runner.", "为运行器添加第二步。")
                      .replace("runner change", "运行器改动")
                      .replace("documentation", "文档")
                      .replace("return two steps instead of one.", "返回两步而不是一步。")
                      .replace("document the second step.", "写清楚第二步。")
                      .replace("README names the second step", "README 写明第二步")
                      .replace("coded, reviewed, no findings", "编码完成，已审查，无发现")
                      .replace("the wording is ambiguous", "措辞有歧义")
                      .replace("**Warnings**: None.", "**Warnings**: 无。")
                      .replace("not landed: still building", "未合并：仍在开发"))
        self.write(translated)
        self.assert_green()

    # -- template residue ------------------------------------------------

    def test_template_comment_left_in(self):
        self.write(PLAN.replace("## Task", "<!-- fill this in -->\n## Task"))
        self.assert_red("template comment block left in place")

    def test_placeholder_title(self):
        self.write(PLAN.replace("# Add the second step", "# <Task name>"))
        self.assert_red("title is still the template placeholder")

    # -- sections --------------------------------------------------------

    def test_missing_mandatory_section(self):
        self.write(PLAN.replace("## Deliberate Omissions\n\n- none\n", ""))
        self.assert_red("missing mandatory section 'Deliberate Omissions'")

    def test_sections_out_of_order(self):
        moved = PLAN.replace(
            "## Progress Log\n\n- `2026-09-13 14:20` `W1` coded, reviewed, no findings\n\n",
            "").replace(
            "## Verification",
            "## Progress Log\n\n- `2026-09-13 14:20` `W1` coded\n\n## Verification")
        self.write(moved)
        self.assert_red("sections are out of order")

    # -- stage and the required lines ------------------------------------

    def test_unknown_stage(self):
        self.write(PLAN.replace("**Stage**: build", "**Stage**: halfway"))
        self.assert_red("unknown stage 'halfway'")

    def test_verify_is_no_longer_a_stage(self):
        self.write(PLAN.replace("**Stage**: build", "**Stage**: verify"))
        self.assert_red("unknown stage 'verify'")

    def test_missing_stage_line(self):
        self.write(PLAN.replace("**Stage**: build\n", ""))
        self.assert_red("no '**Stage**:' line")

    def test_missing_warnings_line(self):
        self.write(PLAN.replace("**Warnings**: None.\n", ""))
        self.assert_red("no '**Warnings**:' line")

    def test_missing_landed_line(self):
        self.write(PLAN.replace("**Landed**: not landed: still building\n", ""))
        self.assert_red("no '**Landed**:' line")

    # -- work items ------------------------------------------------------

    def test_unknown_status(self):
        self.write(PLAN.replace("| -- | done |", "| -- | nearly |"))
        self.assert_red("unknown status 'nearly'")

    def test_items_owning_the_same_file(self):
        self.write(PLAN.replace("| W2 | documentation | README.md |",
                                "| W2 | documentation | src/app.py |"))
        self.assert_red("must partition the files they write")

    def test_items_overlapping_through_a_glob(self):
        self.write(PLAN.replace("| W2 | documentation | README.md |",
                                "| W2 | documentation | src/** |"))
        self.assert_red("must partition the files they write")

    def test_sibling_paths_are_not_an_overlap(self):
        self.write(PLAN.replace("| W2 | documentation | README.md |",
                                "| W2 | documentation | src/appendix.py |"))
        self.assert_green()

    def test_duplicate_item_id(self):
        self.write(PLAN.replace("| W2 | documentation | README.md |",
                                "| W1 | documentation | README.md |"))
        self.assert_red("duplicate item id 'W1'")

    def test_wrong_table_header(self):
        self.write(PLAN.replace("| ID | Item | Owns | Depends on | Status | Updated |",
                                "| ID | Item | Files | Status | Updated |"))
        self.assert_red("Work Items table header is")

    def test_row_with_the_wrong_cell_count(self):
        self.write(PLAN.replace(
            "| W2 | documentation | README.md | W1 | coding | 2026-09-13 14:30 |",
            "| W2 | documentation | README.md | coding |"))
        self.assert_red("expected 6")

    # -- detail blocks and acceptance ------------------------------------

    def test_item_without_a_detail_block(self):
        self.write(PLAN.replace("### W2 — documentation", "### W3 — documentation"))
        self.assert_red("W2 has no '### W2' detail block")

    def test_detail_block_absent_from_the_table(self):
        self.write(PLAN.replace("### W2 — documentation", "### W9 — documentation"))
        self.assert_red("detail block 'W9' is absent from the Work Items table")

    def test_item_without_acceptance_criteria(self):
        self.write(PLAN.replace(
            "**Acceptance**:\n- [ ] README names the second step\n\n", ""))
        self.assert_red("W2 has no acceptance criteria")

    def test_done_item_with_an_unticked_criterion(self):
        self.write(PLAN.replace("- [x] run() returns 2", "- [ ] run() returns 2"))
        self.assert_red("is done with 1 acceptance criterion(s) unticked")

    def test_a_heading_outside_work_items_is_not_a_detail_block(self):
        """A '###' under Verification heads a round of work, not an item.

        It reads exactly like a detail block -- one token, a dash, a title --
        which is how a re-verification round gets reported as an item missing
        from the table.
        """
        self.write(PLAN.replace(
            "| make test | pass | 2026-09-13 14:40 |",
            "| make test | pass | 2026-09-13 14:40 |\n\n"
            "### re-verification — 2026-09-13 15:10, independent, clean tree\n\n"
            "Ran the suite again from a clean tree."))
        self.assert_green()

    # -- open findings ---------------------------------------------------

    def test_unknown_finding_state(self):
        self.write(PLAN.replace("| reviewer-b | open |", "| reviewer-b | pondering |"))
        self.assert_red("unknown finding state 'pondering'")

    def test_a_finding_moved_to_another_task(self):
        self.write(PLAN.replace("| reviewer-b | open |",
                                "| reviewer-b | moved: settle-the-wording |"))
        self.assert_green()

    def test_a_finding_moved_to_something_that_is_not_a_slug(self):
        self.write(PLAN.replace("| reviewer-b | open |",
                                "| reviewer-b | moved: the design session |"))
        self.assert_red("is not a task slug")

    # -- the archiving gate ----------------------------------------------

    def test_an_archive_ready_plan_passes_the_gate(self):
        self.write(ARCHIVED)
        self.assert_green(final=True)

    def test_the_gate_is_off_unless_asked_for(self):
        """The baseline is mid-flight: green normally, red at the gate."""
        self.write(PLAN)
        self.assert_green()
        self.assert_red("reaches 'done' before the plan is archived", final=True)

    def test_gate_rejects_a_stage_short_of_done(self):
        self.write(ARCHIVED.replace("**Stage**: done", "**Stage**: land"))
        self.assert_red("stage is 'land'", final=True)

    def test_gate_rejects_an_item_short_of_done(self):
        self.write(ARCHIVED.replace("| W1 | done | 2026-09-13 14:30 |",
                                    "| W1 | review | 2026-09-13 14:30 |"))
        self.assert_red("W2 is 'review'", final=True)

    def test_gate_rejects_an_unticked_criterion_whatever_the_status(self):
        """The unticked check must not hinge on the status reaching done."""
        planted = ARCHIVED.replace("- [x] README names the second step",
                                   "- [ ] README names the second step") \
                          .replace("| W1 | done | 2026-09-13 14:30 |",
                                   "| W1 | review | 2026-09-13 14:30 |")
        self.write(planted)
        self.assert_red("W2 is review with 1 acceptance criterion(s) unticked",
                        final=True)

    def test_gate_rejects_an_open_finding(self):
        self.write(ARCHIVED.replace("| reviewer-b | fixed |", "| reviewer-b | open |"))
        self.assert_red("F1 is still 'open'", final=True)

    def test_gate_rejects_a_disputed_finding(self):
        self.write(ARCHIVED.replace("| reviewer-b | fixed |",
                                    "| reviewer-b | disputed |"))
        self.assert_red("F1 is still 'disputed'", final=True)

    def test_gate_accepts_a_finding_moved_to_its_own_task(self):
        self.write(ARCHIVED.replace("| reviewer-b | fixed |",
                                    "| reviewer-b | moved: settle-the-wording |"))
        self.assert_green(final=True)

    def test_gate_rejects_an_unfilled_landing(self):
        self.write(ARCHIVED.replace("**Landed**: `4d1bea6f` on main, fast-forward",
                                    "**Landed**: <merge commit sha>"))
        self.assert_red("'**Landed**:' is empty", final=True)

    def test_gate_accepts_a_task_that_did_not_land(self):
        self.write(ARCHIVED.replace("**Landed**: `4d1bea6f` on main, fast-forward",
                                    "**Landed**: not landed: superseded by the rewrite"))
        self.assert_green(final=True)

    # -- a landing the stage never caught up with -------------------------

    def test_a_recorded_commit_with_the_stage_left_behind(self):
        """The contradiction that survives archiving: landed, still at build."""
        self.write(PLAN.replace("**Landed**: not landed: still building",
                                "**Landed**: `4d1bea6f95b6a7fdc85ca9d685a4c675`"))
        self.assert_red("names a commit while the stage is 'build'")

    def test_prose_about_landing_is_not_read_as_a_commit(self):
        self.write(PLAN.replace("**Landed**: not landed: still building",
                                "**Landed**: not landed: the decdeed case is unclear"))
        self.assert_green()

    # -- the filename is the slug ----------------------------------------

    def test_filename_that_is_not_kebab_case(self):
        self.write(PLAN, name="Add Second Step.md")
        self.assert_red("is not a slug")

    def test_single_word_filename(self):
        self.write(PLAN, name="plan.md")
        self.assert_red("is not a slug")

    def test_filename_ending_in_a_counter(self):
        self.write(PLAN, name="add-second-step-3.md")
        self.assert_red("ends in a number")

    def test_filename_that_is_a_date_stamp(self):
        self.write(PLAN, name="task-20260913.md")
        self.assert_red("ends in a number")

    # -- collection ------------------------------------------------------

    def test_archived_plans_are_not_collected(self):
        self.write(PLAN)
        archive = self.plans / "archive"
        archive.mkdir()
        (archive / "old.md").write_text("# Old\n\nnothing here\n", encoding="utf-8")
        collected = [p.name for p in m.collect(self.repo, [])]
        self.assertEqual(collected, ["add-second-step.md"])

    def test_plans_path_is_read_from_the_configuration(self):
        (self.repo / "CLAUDE.md").write_text(
            "# P\n\n## Development\n\n- plans: .agent/plans\n", encoding="utf-8")
        other = self.repo / ".agent" / "plans"
        other.mkdir(parents=True)
        (other / "some-task.md").write_text(PLAN, encoding="utf-8")
        collected = [p.name for p in m.collect(self.repo, [])]
        self.assertEqual(collected, ["some-task.md"])

    def test_explicit_files_win_over_the_configured_path(self):
        path = self.write(PLAN, name="elsewhere-task.md")
        collected = m.collect(self.repo, [str(path)])
        self.assertEqual(collected, [path.resolve()])

    def test_exit_code_is_zero_with_no_plans(self):
        self.assertEqual(m.main(["--repo", str(self.repo)]), 0)

    def test_exit_code_is_one_on_a_finding(self):
        self.write(PLAN.replace("**Stage**: build", "**Stage**: halfway"))
        self.assertEqual(m.main(["--repo", str(self.repo)]), 1)

    # -- the shipped template stays in step ------------------------------

    def test_the_shipped_template_matches_the_vocabulary(self):
        template = (pathlib.Path(__file__).resolve().parents[1]
                    / "templates" / "plan.md").read_text(encoding="utf-8")
        for section in m.SECTIONS:
            self.assertIn(f"## {section}", template,
                          f"the template is missing the {section!r} section")
        self.assertIn(" | ".join(m.COLUMNS), template)
        for stage in sorted(m.STAGES):
            self.assertIn(stage, template)
        for status in sorted(m.STATUSES):
            self.assertIn(status, template)
        for state in sorted(m.FINDING_STATES):
            self.assertIn(state, template)
        self.assertIn("moved: <task-slug>", template,
                      "the template must show how a finding names the task it moved to")


if __name__ == "__main__":
    unittest.main(verbosity=2)
