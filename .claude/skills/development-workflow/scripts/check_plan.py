#!/usr/bin/env python3
"""Structural checker for plan documents.

Checks one thing: that a plan document matches templates/plan.md in shape --
section presence and order, the closed Stage and Status vocabularies, the Work
Items table and its detail blocks, acceptance criteria, the file-ownership
partition, and the slug-shaped filename.

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
COLUMNS = ("ID", "Item", "Owns", "Depends on", "Status", "Updated")

SECTION_RE = re.compile(r"^##\s+(?P<name>.+?)\s*$")
ITEM_BLOCK_RE = re.compile(r"^###\s+(?P<id>\S+)\s*(?:[—–-]\s*(?P<title>.*))?$")
STAGE_RE = re.compile(r"^\*\*Stage\*\*:\s*(?P<stage>.+?)\s*$", re.M)
WARNINGS_RE = re.compile(r"^\*\*Warnings\*\*:", re.M)
LANDED_RE = re.compile(r"^\*\*Landed\*\*:", re.M)
ACCEPTANCE_RE = re.compile(r"^\s*-\s*\[(?P<mark>[ xX])\]")
SLUG_RE = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)+$")
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


def check_plan(path: Path, rel: str) -> list[Finding]:
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
    if not stage:
        findings.append(Finding(rel, "no '**Stage**:' line"))
    elif stage.group("stage") not in STAGES:
        findings.append(Finding(rel, (
            f"unknown stage {stage.group('stage')!r}; the stages are "
            f"{', '.join(sorted(STAGES))}"
        )))
    if "Verification" in names and not WARNINGS_RE.search(text):
        findings.append(Finding(rel, (
            "the Verification section has no '**Warnings**:' line; warnings are "
            "recorded even when there were none"
        )))
    if "Outcome" in names and not LANDED_RE.search(text):
        findings.append(Finding(rel, "the Outcome section has no '**Landed**:' line"))

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

    starts = [(match.group("id"), index)
              for index, line in enumerate(lines)
              if (match := ITEM_BLOCK_RE.match(line))]
    blocks: dict[str, tuple[int, int]] = {}
    for position, (item_id, index) in enumerate(starts):
        stop = starts[position + 1][1] if position + 1 < len(starts) else len(lines)
        for _, section_index in found:
            if index < section_index < stop:
                stop = section_index
        blocks[item_id] = (index, stop)

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
        elif row["Status"] == "done" and " " in marks:
            findings.append(Finding(f"{rel}:{row['line']}", (
                f"{item_id} is done with {marks.count(' ')} acceptance "
                "criterion(s) unticked"
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
            if len(row_cells) == 5 and row_cells[4] not in FINDING_STATES:
                findings.append(Finding(f"{rel}:{lineno}", (
                    f"unknown finding state {row_cells[4]!r}; the states are "
                    f"{', '.join(sorted(FINDING_STATES))}"
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
        findings += check_plan(path, rel)

    for finding in findings:
        print(finding)
    if findings:
        print(f"\n{len(findings)} finding(s) in {len(paths)} plan document(s).",
              file=sys.stderr)
        return 1
    print(f"{len(paths)} plan document(s) OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())
