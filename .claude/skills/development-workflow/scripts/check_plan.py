#!/usr/bin/env python3
"""Structural checker for plan documents.

Checks one thing: that a plan document matches templates/plan.md in shape --
section presence and order, the closed Stage and Status vocabularies, the Work
Items table and its detail blocks, acceptance criteria, the file-ownership
partition, and the slug-shaped filename.

--final adds the archiving gate on top: the stage reached done, every work item
reached done, every acceptance criterion is ticked, every item names a reviewer
the rest of the file can account for, no finding is still open or disputed, and
the Outcome records a landing. A plan archived below that bar keeps no usable
record of what was actually delivered.

It reads no git state and judges no content. Whether an acceptance criterion is
meaningful, whether a status is honest and whether a slug actually says what the
work is are review's business, not this script's.

A plan document's prose is written in the language of the session; only its
structure -- headings, table header, the Stage and Status vocabularies -- is
fixed English, and only that is what this script matches on.

Usage:

    python3 check_plan.py                 # every plan under the configured path
    python3 check_plan.py <file> [...]    # only these files
    python3 check_plan.py --repo <path>   # resolve the configured path from here
    python3 check_plan.py --final <file>  # also apply the archiving gate
"""

from __future__ import annotations

import argparse
import re
import sys
from dataclasses import dataclass
from pathlib import Path

DEFAULT_PLANS = ".claude/plans"

# Vocabulary, matching templates/plan.md.
SECTIONS = (
    "Task", "Work Items", "Progress Log", "Open Findings", "Verification",
    "Deliberate Omissions", "Outcome",
)
STAGES = frozenset({"plan", "build", "land", "done"})
STATUSES = frozenset({"planned", "coding", "review", "fixing", "blocked", "done"})
FINDING_STATES = frozenset({"open", "fixed", "disputed", "accepted-as-is"})
# What a finding may still say when the plan is archived. `open` and `disputed`
# are not among them: an unresolved finding is either settled here or becomes a
# task of its own, and `moved: <task-slug>` names the task it became.
SETTLED_STATES = frozenset({"fixed", "accepted-as-is"})
COLUMNS = ("ID", "Item", "Owns", "Depends on", "Status", "Updated")

SECTION_RE = re.compile(r"^##\s+(?P<name>.+?)\s*$")
ITEM_BLOCK_RE = re.compile(r"^###\s+(?P<id>\S+)\s*(?:[—–-]\s*(?P<title>.*))?$")
STAGE_RE = re.compile(r"^\*\*Stage\*\*:\s*(?P<stage>.+?)\s*$", re.M)
WARNINGS_RE = re.compile(r"^\*\*Warnings\*\*:", re.M)
LANDED_RE = re.compile(r"^\*\*Landed\*\*:\s*(?P<landed>.*?)\s*$", re.M)
ACCEPTANCE_RE = re.compile(r"^\s*-\s*\[(?P<mark>[ xX])\]")
REVIEWER_RE = re.compile(r"^\*\*Reviewer\*\*:\s*(?P<who>.*?)\s*$")
SLUG_RE = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)+$")
MOVED_RE = re.compile(r"^moved:\s*(?P<slug>.+?)\s*$")
PLACEHOLDER_RE = re.compile(r"^<.*>$")
NOT_SET = {"--", "-", "", "n/a", "none", "tbd"}


@dataclass(frozen=True)
class Finding:
    location: str
    message: str

    def __str__(self) -> str:
        return f"{self.location}: {self.message}"


def load_plans_path(repo: Path) -> str:
    """Read `plans` from the `## Development` block of CLAUDE.md."""
    claude_md = repo / "CLAUDE.md"
    if not claude_md.is_file():
        return DEFAULT_PLANS
    in_block = False
    for line in claude_md.read_text(encoding="utf-8").splitlines():
        stripped = line.strip()
        if stripped.startswith("## "):
            in_block = stripped[3:].strip().lower() == "development"
            continue
        if in_block:
            match = re.match(r"^-\s*plans\s*:\s*(.+?)\s*$", stripped)
            if match:
                return match.group(1)
    return DEFAULT_PLANS


def cells(line: str) -> list[str]:
    return [cell.strip() for cell in line.strip().strip("|").split("|")]


def is_separator(line: str) -> bool:
    return bool(re.fullmatch(r"\|[\s:|-]+\|", line.strip()))


def table_in(lines: list[str], start: int, end: int) -> list[tuple[int, str]]:
    """Rows of the first pipe table between start and end, as (line number, text)."""
    rows: list[tuple[int, str]] = []
    seen = False
    for index in range(start, end):
        line = lines[index].strip()
        if line.startswith("|"):
            seen = True
            rows.append((index + 1, line))
        elif seen and line:
            break
    return rows


def owned(cell: str) -> list[str]:
    if cell.lower() in NOT_SET:
        return []
    return [part.strip() for part in re.split(r"[,;]", cell) if part.strip()]


def overlap(left: str, right: str) -> bool:
    def bare(path: str) -> str:
        return re.sub(r"/?\*+$", "", path).rstrip("/")

    a, b = bare(left), bare(right)
    if not a or not b:
        return False
    return a == b or a.startswith(b + "/") or b.startswith(a + "/")


def names_a_commit(text: str) -> bool:
    """True when the text carries something shaped like a commit sha."""
    return any(7 <= len(token) <= 40
               and all(char in "0123456789abcdef" for char in token)
               and any(char.isdigit() for char in token)
               for token in re.findall(r"[0-9a-zA-Z]+", text))


def check_filename(path: Path, rel: str) -> list[Finding]:
    stem = path.stem
    if not SLUG_RE.match(stem):
        return [Finding(rel, (
            f"filename {path.name!r} is not a slug; use lowercase kebab-case "
            "naming what the work is, the same slug as the task and its workflow"
        ))]
    if stem.rsplit("-", 1)[-1].isdigit():
        return [Finding(rel, (
            f"filename {path.name!r} ends in a number; a counter or a date is "
            "not a name — say what the work is"
        ))]
    return []


def check_plan(path: Path, rel: str, final: bool = False) -> list[Finding]:
    findings = check_filename(path, rel)
    text = path.read_text(encoding="utf-8", errors="replace")
    lines = text.splitlines()

    if "<!--" in text:
        lineno = next(i + 1 for i, line in enumerate(lines) if "<!--" in line)
        findings.append(Finding(f"{rel}:{lineno}", (
            "template comment block left in place; delete every comment block "
            "when the plan is filled in"
        )))
    for index, line in enumerate(lines):
        if line.startswith("# "):
            if line.strip() == "# <Task name>":
                findings.append(Finding(
                    f"{rel}:{index + 1}", "title is still the template placeholder"))
            break

    found = [(match.group("name"), index)
             for index, line in enumerate(lines)
             if (match := SECTION_RE.match(line))]
    names = [name for name, _ in found]
    for required in SECTIONS:
        if required not in names:
            findings.append(Finding(rel, f"missing mandatory section {required!r}"))
    present = [name for name in names if name in SECTIONS]
    if present != [name for name in SECTIONS if name in present]:
        findings.append(Finding(rel, (
            f"sections are out of order: {' -> '.join(present)}; the fixed order "
            f"is {' -> '.join(SECTIONS)}"
        )))

    stage = STAGE_RE.search(text)
    stage_value = stage.group("stage") if stage else None
    if not stage:
        findings.append(Finding(rel, "no '**Stage**:' line"))
    elif stage_value not in STAGES:
        findings.append(Finding(rel, (
            f"unknown stage {stage_value!r}; the stages are "
            f"{', '.join(sorted(STAGES))}"
        )))
    elif final and stage_value != "done":
        findings.append(Finding(rel, (
            f"stage is {stage_value!r}; a plan reaches 'done' before it is archived"
        )))
    if "Verification" in names and not WARNINGS_RE.search(text):
        findings.append(Finding(rel, (
            "the Verification section has no '**Warnings**:' line; warnings are "
            "recorded even when there were none"
        )))
    landed = LANDED_RE.search(text)
    if "Outcome" in names and not landed:
        findings.append(Finding(rel, "the Outcome section has no '**Landed**:' line"))
    elif landed:
        value = landed.group("landed")
        if final and (not value or PLACEHOLDER_RE.match(value)):
            findings.append(Finding(rel, (
                "'**Landed**:' is empty; record the merge commit, or why the task "
                "did not land"
            )))
        elif names_a_commit(value) and stage_value in STAGES and stage_value != "done":
            findings.append(Finding(rel, (
                f"'**Landed**:' names a commit while the stage is {stage_value!r}; a "
                "change that has landed leaves the plan at 'done'"
            )))

    bounds = dict(found)
    if "Work Items" not in bounds:
        return findings

    def section_span(name: str) -> tuple[int, int]:
        start = bounds[name]
        later = [index for _, index in found if index > start]
        return start, (min(later) if later else len(lines))

    start, end = section_span("Work Items")
    rows = table_in(lines, start, end)
    if not rows:
        findings.append(Finding(rel, "the Work Items section has no table"))
        return findings
    header = cells(rows[0][1])
    if tuple(header) != COLUMNS:
        findings.append(Finding(f"{rel}:{rows[0][0]}", (
            f"Work Items table header is {' | '.join(header)}; expected "
            f"{' | '.join(COLUMNS)}"
        )))
        return findings

    items: dict[str, dict[str, str]] = {}
    owners: list[tuple[str, str, int]] = []
    for lineno, line in rows[1:]:
        if is_separator(line):
            continue
        row_cells = cells(line)
        if len(row_cells) != len(COLUMNS):
            findings.append(Finding(f"{rel}:{lineno}", (
                f"work item row has {len(row_cells)} cells, expected {len(COLUMNS)}"
            )))
            continue
        row = dict(zip(COLUMNS, row_cells))
        item_id = row["ID"]
        if item_id in items:
            findings.append(Finding(f"{rel}:{lineno}", f"duplicate item id {item_id!r}"))
            continue
        items[item_id] = {**row, "line": str(lineno)}
        if row["Status"] not in STATUSES:
            findings.append(Finding(f"{rel}:{lineno}", (
                f"unknown status {row['Status']!r} for {item_id}; the statuses "
                f"are {', '.join(sorted(STATUSES))}"
            )))
        elif final and row["Status"] != "done":
            findings.append(Finding(f"{rel}:{lineno}", (
                f"{item_id} is {row['Status']!r}; every work item reaches 'done' "
                "before the plan is archived"
            )))
        for one in owned(row["Owns"]):
            owners.append((item_id, one, lineno))

    for i, (left_id, left_path, lineno) in enumerate(owners):
        for right_id, right_path, _ in owners[i + 1:]:
            if left_id != right_id and overlap(left_path, right_path):
                findings.append(Finding(f"{rel}:{lineno}", (
                    f"{left_id} and {right_id} both own {left_path!r} / "
                    f"{right_path!r}; items that run in parallel must partition "
                    "the files they write — merge them, or serialize one behind "
                    "the other through Depends on"
                )))

    # Detail blocks are the Work Items section's own. A '###' under Verification
    # or Progress Log heads a round of work, not an item, and reading those as
    # orphaned detail blocks is noise that teaches everyone to ignore the exit
    # code.
    starts = [(match.group("id"), index)
              for index, line in enumerate(lines[start:end], start=start)
              if (match := ITEM_BLOCK_RE.match(line))]
    blocks: dict[str, tuple[int, int]] = {}
    for position, (item_id, index) in enumerate(starts):
        stop = starts[position + 1][1] if position + 1 < len(starts) else end
        blocks[item_id] = (index, stop)

    # A reviewer is accounted for by the rest of the file: the log of what
    # happened, the findings they raised, or the verification round they ran.
    # The Work Items section is excluded on purpose -- the Reviewer line cannot
    # be its own evidence.
    elsewhere = "\n".join(
        "\n".join(lines[slice(*section_span(name))])
        for name in ("Progress Log", "Open Findings", "Verification")
        if name in bounds)

    for item_id, row in items.items():
        if item_id not in blocks:
            findings.append(Finding(
                f"{rel}:{row['line']}", f"{item_id} has no '### {item_id}' detail block"))
            continue
        block_start, block_end = blocks[item_id]
        marks = [match.group("mark").lower()
                 for line in lines[block_start:block_end]
                 if (match := ACCEPTANCE_RE.match(line))]
        if not marks:
            findings.append(Finding(f"{rel}:{block_start + 1}", (
                f"{item_id} has no acceptance criteria; an item with nothing to "
                "satisfy cannot be reviewed"
            )))
        elif (final or row["Status"] == "done") and " " in marks:
            findings.append(Finding(f"{rel}:{row['line']}", (
                f"{item_id} is {row['Status']} with {marks.count(' ')} acceptance "
                "criterion(s) unticked"
            )))

        reviewer = next((match.group("who")
                         for line in lines[block_start:block_end]
                         if (match := REVIEWER_RE.match(line))), None)
        if reviewer is None:
            findings.append(Finding(f"{rel}:{block_start + 1}", (
                f"{item_id} has no '**Reviewer**:' line; an item records who "
                "checked it, never who wrote it"
            )))
        elif final and (not reviewer or PLACEHOLDER_RE.match(reviewer)):
            findings.append(Finding(f"{rel}:{block_start + 1}", (
                f"{item_id} names no reviewer; the review happened by the time "
                "the plan is archived, so the name is known by then"
            )))
        elif final and reviewer not in elsewhere:
            findings.append(Finding(f"{rel}:{block_start + 1}", (
                f"{item_id} names {reviewer!r} as its reviewer, and nothing else "
                "in the file mentions them; name the reviewer or the review round "
                "that actually ran, so the Progress Log, the findings or the "
                "Verification record accounts for it"
            )))
    for item_id, (block_start, _) in blocks.items():
        if item_id not in items:
            findings.append(Finding(f"{rel}:{block_start + 1}", (
                f"detail block {item_id!r} is absent from the Work Items table"
            )))

    if "Open Findings" in bounds:
        start, end = section_span("Open Findings")
        for lineno, line in table_in(lines, start, end)[1:]:
            if is_separator(line):
                continue
            row_cells = cells(line)
            if len(row_cells) != 5:
                continue
            finding_id, state = row_cells[0], row_cells[4]
            moved = MOVED_RE.match(state)
            if moved:
                if not SLUG_RE.match(moved.group("slug")):
                    findings.append(Finding(f"{rel}:{lineno}", (
                        f"{finding_id} moved to {moved.group('slug')!r}, which is "
                        "not a task slug; a moved finding names the task that "
                        "will settle it, so it can be found from either end"
                    )))
            elif state not in FINDING_STATES:
                findings.append(Finding(f"{rel}:{lineno}", (
                    f"unknown finding state {state!r}; the states are "
                    f"{', '.join(sorted(FINDING_STATES))}, or 'moved: <task-slug>'"
                )))
            elif final and state not in SETTLED_STATES:
                findings.append(Finding(f"{rel}:{lineno}", (
                    f"{finding_id} is still {state!r}; before archiving, a finding "
                    "is fixed, accepted-as-is, or moved to the task that will "
                    "settle it — an archived plan is untracked and nobody reads "
                    "it again"
                )))
    return findings


def collect(repo: Path, given: list[str]) -> list[Path]:
    if given:
        return [Path(one).resolve() for one in given]
    plans = repo / load_plans_path(repo)
    if not plans.is_dir():
        return []
    return sorted(plans.glob("*.md"))


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("files", nargs="*", help="plan documents to check")
    parser.add_argument("--repo", default=".", help="repository root")
    parser.add_argument("--final", action="store_true", help=(
        "apply the archiving gate as well: stage done, every item done, every "
        "acceptance criterion ticked, no finding left open or disputed"))
    args = parser.parse_args(argv)

    repo = Path(args.repo).resolve()
    paths = collect(repo, args.files)
    if not paths:
        print("no plan documents to check", file=sys.stderr)
        return 0

    findings: list[Finding] = []
    for path in paths:
        if not path.is_file():
            findings.append(Finding(str(path), "no such file"))
            continue
        try:
            rel = path.relative_to(repo).as_posix()
        except ValueError:
            rel = str(path)
        findings += check_plan(path, rel, final=args.final)

    for finding in findings:
        print(finding)
    if findings:
        print(f"\n{len(findings)} finding(s) in {len(paths)} plan document(s).",
              file=sys.stderr)
        return 1
    print(f"{len(paths)} plan document(s) OK"
          + (", ready to archive" if args.final else ""))
    return 0


if __name__ == "__main__":
    sys.exit(main())
