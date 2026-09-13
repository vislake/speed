#!/usr/bin/env python3
"""Structural checker for the document-standards skill.

The skill (.claude/skills/document-standards/) is the authoritative
standard for the documentation trees declared in CLAUDE.md's
"## Documentation" block. Most of that standard is a judgement call --
whether a sentence is subjective, whether a requirement traces to a real
source, whether an option's rejection reason is honest. This script
checks the part that is not: the STRUCTURE. Section presence and order,
file naming, index coverage, status vocabulary, supersession pairing,
diagram bodies and the metadata a document must never carry.

A green run means the documents are shaped like the standard says. It
does not mean they are correct; review owns the rest.

Trees, paths and language come from CLAUDE.md's "## Documentation" block
(the keys the skill defines: design, requirements, adr, glossary,
language). A tree the block declares but that does not exist on disk is
skipped, not failed -- a project using only design documents and ADRs is
a normal configuration.

Usage:
    python3 tools/check_documents.py [--root PATH]

--root defaults to the current directory and must hold CLAUDE.md. Paths
in the report are relative to --root. Exit code is 0 when every check
passes, 1 on any finding, 2 on a usage or configuration error.

WHAT IS CHECKED

  Common to every document (SKILL.md section 2):
    * exactly one H1, and it opens the document
    * no process metadata: change history, version, author, date, review
      record headings or field lines (an ADR's date lives in its file
      name, which is content -- see the ADR standard)
    * no project management: TODO / FIXME / TBD markers
    * no code reference carrying a line number (path.go:123)
    * every mermaid block has a diagram type and a non-empty body
    * a section holding a diagram carries prose as well
    * headings are written in the document's configured language
    * the configured glossary file exists

  Design tree (references/design-documents.md):
    * README.md and architecture.md are present
    * modules/ file names match design-<slug>[-<aspect>], kebab-case
      ASCII, with no sequence number anywhere
    * README index covers every file in the tree exactly once, and every
      index link resolves
    * architecture.md carries its required sections in the fixed order,
      and the module-dependency and logical-deployment sections each
      carry a mermaid diagram
    * a module document carries its mandatory sections in the fixed
      order, including the Non-Responsibilities subsection, and carries
      no section outside the standard's set
    * every implementation-status entry uses one of the three canonical
      values, names what is missing when partial, and carries a code
      anchor when it claims any implementation
    * architecture.md's status roll-up is one row per module document

  ADR tree (references/decision-records.md):
    * file names match adr-<slug>-<ISO date>, with a real calendar date
    * the fixed section order, with Invalidation Conditions optional
    * status is Accepted, Superseded by ADR-<slug>-<date> or Deprecated
      -- never Proposed
    * a supersession is stated in both records, and the record named as
      the successor exists
    * Options Considered lists at least two options
    * README index covers every record exactly once, and each index row
      agrees with the record's own date and status

  Requirements tree (references/requirements-documents.md):
    * README.md and overview.md are present; journeys/journey-<slug> and
      features/prd-<slug> naming, no sequence numbers
    * README index covers every file exactly once
    * a capability document carries Functional Requirements, and the
      sections it does carry follow the fixed order
    * requirement ids match REQ-<doc slug>-NNN, are unique within the
      document and carry both a status and an end-to-end test field
    * requirement status is Implemented or Not implemented -- the
      standard has no Partially implemented here
    * a journey records who, the precondition, the steps, the outcome
      and its end-to-end test

WHAT IS DELIBERATELY NOT CHECKED

  Judgement, not structure: subjective modifiers, first and second
  person, restating transitions, valueless counts, fabricated
  non-functional numbers, whether a rejected option's stated reason is
  real, whether a requirement is testable, whether a journey is a
  process the business runs. A regex over these produces false
  accusations, which is worse than no check -- code review owns them.

  Rendering: whether a mermaid block actually parses. This script checks
  that a block has a type and a body; running a renderer needs a browser
  and belongs to the editing workflow the skill mandates.

  Timeliness: whether a document still matches the code it asserts
  things about. No mechanical check can know.
"""

from __future__ import annotations

import argparse
import datetime
import os
import re
import sys

CONFIG_KEYS = ("design", "requirements", "adr", "glossary", "language")
CONFIG_DEFAULTS = {
    "design": "docs/design",
    "requirements": "docs/prd",
    "adr": "docs/adr",
    "glossary": "docs/glossary.md",
}
LANGUAGES = ("en", "zh-CN")


def names(en: str, zh: str) -> dict[str, str]:
    """A canonical section name in each supported document language.

    One name per language, matching the templates: a section written
    under a synonym is a finding, because documents that name the same
    section differently cannot be read against each other."""
    return {"en": en, "zh-CN": zh}


def canonical(name: dict[str, str], language: str) -> str:
    return name[language]


# Section tables. Each row is (key, names, mandatory) in the standard's
# fixed order; the order of the list IS the order rule.
ARCHITECTURE_SECTIONS = [
    ("overview", names("Overview", "概览"), True),
    ("module-dependency-graph", names("Module Dependency Graph", "模块依赖图"), True),
    ("logical-deployment", names("Logical Deployment Architecture", "逻辑部署架构"), True),
    ("physical-deployment", names("Physical Deployment Architecture", "物理部署架构"), False),
    ("inter-module-calls", names("Inter-Module Call Relationships", "模块间调用关系"), False),
    ("data-flow", names("Data Flow", "数据流"), False),
    ("cross-cutting", names("Cross-Cutting Design", "跨切面设计"), True),
    ("invariants", names("Architecture Invariants", "架构不变量"), True),
    ("status", names("Implementation Status", "实现状态"), True),
]
MODULE_SECTIONS = [
    ("responsibilities", names("Responsibilities", "职责"), True),
    ("glossary", names("Glossary", "术语表"), False),
    ("external-interface", names("External Interface Contract", "外部接口契约"), True),
    ("dependencies", names("Dependencies", "依赖"), True),
    ("diagrams", names("UML Diagrams", "UML 图"), False),
    ("data-structures", names("Core Data Structures", "核心数据结构"), False),
    ("algorithms", names("Core Algorithms and Flows", "核心算法与流程"), False),
    ("errors", names("Error and Failure Semantics", "错误与失败语义"), False),
    ("concurrency", names("Concurrency and Transaction Boundaries", "并发与事务边界"), False),
    ("extension-points", names("Extension Points", "扩展点"), False),
    ("status", names("Implementation Status", "实现状态"), True),
]
NON_RESPONSIBILITIES = names("Non-Responsibilities", "非职责")
ADR_SECTIONS = [
    ("status", names("Status", "状态"), True),
    ("context", names("Context", "背景"), True),
    ("options", names("Options Considered", "考虑过的选项"), True),
    ("decision", names("Decision", "决策"), True),
    ("consequences", names("Consequences", "后果"), True),
    ("invalidation", names("Invalidation Conditions", "失效条件"), False),
]
FEATURE_SECTIONS = [
    ("problem", names("Problem Statement", "问题陈述"), False),
    ("scope", names("Scope", "范围"), False),
    ("actors", names("Actors", "参与者"), False),
    ("glossary", names("Glossary", "术语表"), False),
    ("functional", names("Functional Requirements", "功能需求"), True),
    ("non-functional", names("Non-Functional Requirements", "非功能需求"), False),
    ("data", names("Data Requirements", "数据需求"), False),
    ("external-interface", names("External Interface Requirements", "外部接口需求"), False),
    ("constraints", names("Constraints", "约束"), False),
    ("assumptions", names("Assumptions and Dependencies", "假设与依赖"), False),
]
JOURNEY_SECTIONS = [
    ("who", names("Who", "参与者"), True),
    ("precondition", names("Precondition", "前置条件"), True),
    ("steps", names("Steps", "步骤"), True),
    ("outcome", names("Outcome", "结果"), True),
    ("alternate-paths", names("Alternate Paths", "备选路径"), False),
    ("requirements", names("Requirements Exercised", "覆盖的需求"), False),
    ("e2e", names("End-to-End Test", "端到端测试"), True),
]
OVERVIEW_SECTIONS = [
    ("what", names("What the System Is", "系统是什么"), True),
    ("capabilities", names("Capabilities", "能力"), True),
    ("scope", names("Scope Boundary", "范围边界"), True),
    ("actors", names("Actors", "参与者"), False),
]
INDEX_SECTION = names("Index", "索引")

# Status vocabularies.
DESIGN_STATUS = {
    "implemented": names("Implemented", "已实现"),
    "partial": names("Partially implemented", "部分实现"),
    "not-implemented": names("Not implemented", "未实现"),
}
REQUIREMENT_STATUS = {
    "implemented": names("Implemented", "已实现"),
    "not-implemented": names("Not implemented", "未实现"),
}
VAGUE_STATUS = ("mostly complete", "basically working", "almost done",
                "基本完成", "大部分完成", "基本可用", "大致完成", "基本实现")
ADR_ACCEPTED = names("Accepted", "已接受")
ADR_DEPRECATED = names("Deprecated", "已废弃")
ADR_PROPOSED = names("Proposed", "提议")
SUPERSEDED_RE = re.compile(r"(?:Superseded by|已?被).{0,12}?(ADR-[a-z0-9]+(?:-[a-z0-9]+)*-\d{4}-\d{2}-\d{2})")
SUPERSEDES_RE = re.compile(
    r"(?:supersedes?\s+(ADR-[a-z0-9-]+-\d{4}-\d{2}-\d{2}))"
    r"|(?:(ADR-[a-z0-9-]+-\d{4}-\d{2}-\d{2})[^\n]{0,24}?取代)"
    r"|(?:取代[^\n]{0,24}?(ADR-[a-z0-9-]+-\d{4}-\d{2}-\d{2}))",
    re.IGNORECASE)

# Metadata a document never carries (SKILL.md section 2).
METADATA_HEADINGS = ("change history", "revision history", "changelog", "version history",
                     "document history", "authors", "author", "review record", "reviewers",
                     "变更历史", "修订历史", "变更记录", "修订记录", "版本历史", "文档历史",
                     "作者", "审阅记录", "评审记录")
METADATA_FIELD_RE = re.compile(
    r"^\s*(?:[-*+]\s*)?\**\s*"
    r"(Version|Author|Authors|Date|Last updated|Last modified|Revision|Reviewed by|Reviewers|Owner|Priority"
    r"|版本|作者|日期|更新日期|最后更新|修订|审阅人|评审人|负责人|优先级)"
    r"\**\s*[:：]", re.IGNORECASE)
TASK_MARKER_RE = re.compile(r"(?<![\w-])(TODO|FIXME|TBD)(?![\w-])|待办|待定")
LINE_NUMBER_REF_RE = re.compile(
    r"(?<![\w/])[\w.-]*[\w-]\.(?:go|ts|tsx|js|jsx|py|sql|yaml|yml|toml|json|md|sh)[:#]L?\d+")
HEADING_RE = re.compile(r"^(#{1,6})\s+(.*?)\s*#*\s*$")
FENCE_RE = re.compile(r"^\s{0,3}(`{3,}|~{3,})\s*(\S*)")
LINK_RE = re.compile(r"\[[^\]]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
TABLE_SEPARATOR_RE = re.compile(r"^\s*\|?[\s:|-]+\|[\s:|-]*$")
SLUG_RE = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")
ISO_DATE_RE = re.compile(r"^\d{4}-\d{2}-\d{2}$")
REQ_ID_RE = re.compile(r"^req-([a-z0-9]+(?:-[a-z0-9]+)*)-(\d+)$")
CODE_ANCHOR_RE = re.compile(r"`[^`]*[./][^`]*`")


class Finding:
    """One structural defect, reported as path:line: message."""

    def __init__(self, path: str, line: int, message: str):
        self.path = path
        self.line = line
        self.message = message

    def render(self) -> str:
        where = f"{self.path}:{self.line}" if self.line else self.path
        return f"{where}: {self.message}"

    def sort_key(self) -> tuple:
        return (self.path, self.line, self.message)


class Section:
    """A heading and the lines below it, up to the next heading of the
    same or a shallower level. The level-0 section is the preamble."""

    def __init__(self, level: int, title: str, line: int):
        self.level = level
        self.title = title
        self.line = line
        self.lines: list[tuple[int, str, bool, str]] = []

    def prose_lines(self) -> list[tuple[int, str]]:
        return [(n, t) for n, t, in_fence, _ in self.lines
                if not in_fence and t.strip() and not HEADING_RE.match(t)
                and not FENCE_RE.match(t)]

    def fences(self) -> list[tuple[int, str, list[str]]]:
        """(start line, language, body lines) for every fenced block."""
        out: list[tuple[int, str, list[str]]] = []
        current: list[str] | None = None
        start = 0
        lang = ""
        for n, text, in_fence, fence_lang in self.lines:
            opening = FENCE_RE.match(text) and not in_fence
            closing = FENCE_RE.match(text) and in_fence and current is not None
            if opening:
                current, start, lang = [], n, fence_lang
            elif closing:
                out.append((start, lang, current or []))
                current = None
            elif current is not None:
                current.append(text)
        if current is not None:
            out.append((start, lang, current))
        return out


def scan_lines(text: str) -> list[tuple[int, str, bool, str]]:
    """(line number, text, inside-a-fence, fence language) for every line.

    A fence delimiter line itself is reported with the in_fence state it
    establishes: the opening delimiter is outside, the closing one is
    inside, so a section's prose scan never reads either as prose."""
    out: list[tuple[int, str, bool, str]] = []
    fence: str | None = None
    lang = ""
    for n, raw in enumerate(text.splitlines(), start=1):
        match = FENCE_RE.match(raw)
        if fence is None and match:
            out.append((n, raw, False, match.group(2)))
            fence, lang = match.group(1)[0], match.group(2)
            continue
        if fence is not None and match and match.group(1)[0] == fence:
            out.append((n, raw, True, lang))
            fence, lang = None, ""
            continue
        out.append((n, raw, fence is not None, lang))
    return out


def parse_sections(text: str) -> list[Section]:
    sections = [Section(0, "", 0)]
    for n, raw, in_fence, lang in scan_lines(text):
        heading = None if in_fence else HEADING_RE.match(raw)
        if heading:
            section = Section(len(heading.group(1)), heading.group(2).strip(), n)
            sections.append(section)
        sections[-1].lines.append((n, raw, in_fence, lang))
    return sections


def strip_comments(text: str) -> str:
    """Blank out HTML comment bodies, keeping every line number intact.

    Template comment blocks are instructions to the author, not document
    content, so no content rule applies inside one."""
    def blank(match: re.Match) -> str:
        return re.sub(r"[^\n]", " ", match.group(0))
    return re.sub(r"<!--.*?-->", blank, text, flags=re.DOTALL)


def normalize_heading(title: str) -> str:
    """Heading text without inline markup, link syntax or trailing
    punctuation, so `## **Status**` and `## Status` compare equal."""
    title = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", title)
    title = title.replace("*", "").replace("`", "").replace("_", " ")
    title = re.sub(r"^\s*\d+[.、)]\s*", "", title)
    return title.strip().strip(":：。.").strip().casefold()


def match_section(title: str, table: list, language: str) -> tuple[str | None, str | None]:
    """(key, wrong-language) for a heading against a section table."""
    normalized = normalize_heading(title)
    for key, name, _ in table:
        if normalize_heading(name[language]) == normalized:
            return key, None
    for key, name, _ in table:
        for other in LANGUAGES:
            if other != language and normalize_heading(name[other]) == normalized:
                return key, other
    return None, None


def read(path: str) -> str:
    with open(path, encoding="utf-8") as handle:
        return handle.read()


def markdown_files(tree: str) -> list[str]:
    out = []
    for dirpath, dirnames, filenames in os.walk(tree):
        dirnames[:] = sorted(d for d in dirnames if not d.startswith("."))
        for name in sorted(filenames):
            if name.endswith(".md"):
                out.append(os.path.join(dirpath, name))
    return sorted(out)


def parse_documentation_config(root: str) -> tuple[dict[str, str], list[str]]:
    """The "## Documentation" block of CLAUDE.md, with the skill's defaults
    filled in. Errors are usage errors -- an unresolved language means the
    checker cannot know which vocabulary applies."""
    path = os.path.join(root, "CLAUDE.md")
    if not os.path.isfile(path):
        return {}, [f"CLAUDE.md not found at {path}"]
    config: dict[str, str] = {}
    in_block = False
    for raw in read(path).splitlines():
        heading = HEADING_RE.match(raw)
        if heading:
            if in_block:
                break
            in_block = normalize_heading(heading.group(2)) == "documentation"
            continue
        if not in_block:
            continue
        entry = re.match(r"^\s*[-*+]\s*([A-Za-z-]+)\s*[:：]\s*(\S+)\s*$", raw)
        if entry and entry.group(1).lower() in CONFIG_KEYS:
            config[entry.group(1).lower()] = entry.group(2).strip("`")
    errors = []
    if not config:
        errors.append("CLAUDE.md has no \"## Documentation\" block; the skill "
                      "requires one declaring design, requirements, adr, "
                      "glossary and language")
    declared = dict(config)
    for key, default in CONFIG_DEFAULTS.items():
        config.setdefault(key, default)
    language = config.get("language")
    if language and language not in LANGUAGES:
        errors.append(f"unsupported documentation language {language!r}; this "
                      f"checker knows {', '.join(LANGUAGES)}")
    elif not language:
        errors.append("CLAUDE.md's \"## Documentation\" block declares no "
                      "language; the checker cannot resolve the section "
                      "vocabulary without it")
    config["_declared"] = ",".join(sorted(declared))
    return config, errors


# --- checks common to every document -------------------------------------

def check_common(rel_path: str, text: str, language: str) -> list[Finding]:
    found: list[Finding] = []
    sections = parse_sections(text)
    lines = scan_lines(text)

    titles = [s for s in sections if s.level == 1]
    if not titles:
        found.append(Finding(rel_path, 1, "no H1 title"))
    elif len(titles) > 1:
        for extra in titles[1:]:
            found.append(Finding(rel_path, extra.line,
                                 "second H1 title; a document has exactly one"))
    if titles:
        preamble = sections[0].prose_lines()
        if preamble:
            found.append(Finding(rel_path, preamble[0][0],
                                 "content above the H1 title"))

    for section in sections:
        if section.level == 0:
            continue
        if normalize_heading(section.title) in {normalize_heading(h) for h in METADATA_HEADINGS}:
            found.append(Finding(rel_path, section.line,
                                 f"process metadata section {section.title!r}; git is the "
                                 f"authority for history, versions, authors and review records"))

    for n, raw, in_fence, _ in lines:
        if in_fence or FENCE_RE.match(raw):
            continue
        field = METADATA_FIELD_RE.match(raw)
        if field:
            found.append(Finding(rel_path, n,
                                 f"process metadata field {field.group(1)!r}; a document "
                                 f"states conclusions, not how it was produced"))
        marker = TASK_MARKER_RE.search(raw)
        if marker:
            found.append(Finding(rel_path, n,
                                 f"project-management marker {marker.group(0)!r}; TODOs, "
                                 f"schedules and owners belong to the tracker"))
        reference = LINE_NUMBER_REF_RE.search(raw)
        if reference:
            found.append(Finding(rel_path, n,
                                 f"code reference with a line number ({reference.group(0)}); "
                                 f"reference a path plus a symbol name"))

    for section in sections:
        diagrams = [f for f in section.fences() if f[1].lower() == "mermaid"]
        for start, _, body in diagrams:
            content = [line for line in body if line.strip()]
            if not content:
                found.append(Finding(rel_path, start, "empty mermaid block"))
                continue
            if len(content) == 1:
                found.append(Finding(rel_path, start,
                                     f"mermaid block declares {content[0].strip()!r} and "
                                     f"nothing else; an empty diagram body renders blank"))
        if diagrams and not section.prose_lines():
            found.append(Finding(rel_path, section.line,
                                 f"section {section.title!r} holds a diagram and no prose; "
                                 f"the text carries the constraints the diagram cannot"))
    return found


def check_heading_language(rel_path: str, text: str, language: str,
                           table: list) -> list[Finding]:
    found = []
    for section in parse_sections(text):
        if section.level != 2:
            continue
        _, wrong = match_section(section.title, table, language)
        if wrong:
            found.append(Finding(rel_path, section.line,
                                 f"section {section.title!r} is written in {wrong}; the "
                                 f"documentation language is {language}"))
    return found


def check_section_order(rel_path: str, text: str, language: str, table: list,
                        level: int = 2, allow_unknown: bool = False) -> list[Finding]:
    """Mandatory sections present, recognized sections in the fixed order,
    and (unless allow_unknown) no heading outside the standard's set."""
    found: list[Finding] = []
    order = [key for key, _, _ in table]
    seen: list[tuple[str, int]] = []
    for section in parse_sections(text):
        if section.level != level:
            continue
        key, wrong = match_section(section.title, table, language)
        if key is None:
            if not allow_unknown:
                found.append(Finding(rel_path, section.line,
                                     f"section {section.title!r} is not part of this document "
                                     f"type's fixed section set"))
            continue
        if wrong is None:
            seen.append((key, section.line))
    for key, name, mandatory in table:
        if mandatory and key not in [k for k, _ in seen]:
            found.append(Finding(rel_path, 0,
                                 f"missing mandatory section {canonical(name, language)!r}"))
    positions = [(order.index(key), key, line) for key, line in seen if key in order]
    for earlier, later in zip(positions, positions[1:]):
        if later[0] < earlier[0]:
            expected = dict((k, canonical(n, language)) for k, n, _ in table)
            found.append(Finding(rel_path, later[2],
                                 f"section {expected[later[1]]!r} comes after "
                                 f"{expected[earlier[1]]!r}; the section order is fixed"))
    duplicates = [k for k, _ in seen if [x for x, _ in seen].count(k) > 1]
    for key in sorted(set(duplicates)):
        name = dict((k, canonical(n, language)) for k, n, _ in table)[key]
        found.append(Finding(rel_path, 0, f"section {name!r} appears more than once"))
    return found


def check_filename_sequence(rel_path: str) -> list[Finding]:
    stem = os.path.basename(rel_path)[:-3]
    for token in stem.split("-"):
        if token.isdigit():
            return [Finding(rel_path, 0,
                            f"file name carries a sequence number ({token}); ordering "
                            f"lives in the README index, not in file names")]
    return []


def check_index(readme_path: str, rel_readme: str, tree: str, root: str,
                language: str, label: str) -> list[Finding]:
    """Every file of the tree appears in the README index exactly once, and
    every index link resolves."""
    found: list[Finding] = []
    text = strip_comments(read(readme_path))
    sections = parse_sections(text)
    linked: dict[str, list[int]] = {}
    for n, raw, in_fence, _ in scan_lines(text):
        if in_fence:
            continue
        for target in LINK_RE.findall(raw):
            if re.match(r"^[a-z][a-z0-9+.-]*:", target) or target.startswith("#"):
                continue
            clean = target.split("#", 1)[0]
            if not clean.endswith(".md"):
                continue
            resolved = os.path.normpath(os.path.join(os.path.dirname(readme_path), clean))
            if not os.path.isfile(resolved):
                found.append(Finding(rel_readme, n,
                                     f"index link {target!r} points at no file"))
                continue
            if os.path.commonpath([os.path.abspath(resolved), os.path.abspath(tree)]) != os.path.abspath(tree):
                continue
            linked.setdefault(os.path.relpath(resolved, root), []).append(n)

    for rel, occurrences in sorted(linked.items()):
        if len(occurrences) > 1:
            found.append(Finding(rel_readme, occurrences[1],
                                 f"{rel} appears in the index more than once"))
    for path in markdown_files(tree):
        rel = os.path.relpath(path, root)
        if os.path.abspath(path) == os.path.abspath(readme_path):
            continue
        if rel not in linked:
            found.append(Finding(rel, 0,
                                 f"absent from the {label} index ({rel_readme}); an orphan "
                                 f"is an error"))
    if not any(match_section(s.title, [("index", INDEX_SECTION, True)], language)[0]
               for s in sections if s.level == 2) and label == "design":
        found.append(Finding(rel_readme, 0,
                             f"no {canonical(INDEX_SECTION, language)!r} section"))
    return found


# --- design tree ---------------------------------------------------------

def status_of_row(row: str, vocabulary: dict, language: str) -> list[str]:
    keys = []
    for key, name in vocabulary.items():
        if name[language].casefold() in row.casefold():
            keys.append(key)
    if "implemented" in keys and "partial" in keys:
        keys.remove("implemented")  # "Partially implemented" contains "implemented"
    if "not-implemented" in keys and "implemented" in keys:
        keys.remove("implemented")
    return keys


def status_entries(section: Section) -> list[tuple[int, str]]:
    """The status section's data rows: table rows and list items alike."""
    entries = []
    header_seen = False
    for n, raw in section.prose_lines():
        stripped = raw.strip()
        if stripped.startswith("|"):
            if TABLE_SEPARATOR_RE.match(stripped):
                header_seen = True
                continue
            if not header_seen:
                continue  # the header row
            entries.append((n, stripped))
        elif re.match(r"^[-*+]\s+", stripped):
            entries.append((n, stripped))
    return entries


def check_design_status(rel_path: str, sections: list[Section], language: str,
                        roll_up: bool) -> list[Finding]:
    found: list[Finding] = []
    for section in sections:
        if section.level != 2:
            continue
        key, _ = match_section(section.title, MODULE_SECTIONS, language)
        if key != "status":
            continue
        entries = status_entries(section)
        if not entries:
            found.append(Finding(rel_path, section.line,
                                 "implementation status section carries no entry"))
        for n, row in entries:
            for vague in VAGUE_STATUS:
                if vague.casefold() in row.casefold():
                    found.append(Finding(rel_path, n,
                                         f"vague status {vague!r}; the value is one of "
                                         f"{'/'.join(canonical(v, language) for v in DESIGN_STATUS.values())}"))
            keys = status_of_row(row, DESIGN_STATUS, language)
            if not keys:
                found.append(Finding(rel_path, n,
                                     f"status entry carries none of "
                                     f"{'/'.join(canonical(v, language) for v in DESIGN_STATUS.values())}"))
                continue
            if len(keys) > 1:
                found.append(Finding(rel_path, n, "status entry carries more than one status value"))
                continue
            if roll_up:
                if not LINK_RE.search(row):
                    found.append(Finding(rel_path, n,
                                         "roll-up row does not link to the module document"))
                continue
            if keys[0] == "partial":
                remainder = row.replace(canonical(DESIGN_STATUS["partial"], language), "")
                cells = [c.strip() for c in remainder.strip("|").split("|")]
                if len(cells) < 2 or not cells[1].strip(" —-"):
                    found.append(Finding(rel_path, n,
                                         "partial status names nothing that is missing"))
            if keys[0] in ("implemented", "partial") and not CODE_ANCHOR_RE.search(row):
                found.append(Finding(rel_path, n,
                                     "status entry claims implementation with no code anchor "
                                     "(path plus symbol name)"))
    return found


def check_design_tree(tree: str, root: str, language: str) -> list[Finding]:
    found: list[Finding] = []
    rel_tree = os.path.relpath(tree, root)
    readme = os.path.join(tree, "README.md")
    architecture = os.path.join(tree, "architecture.md")
    for required in (readme, architecture):
        if not os.path.isfile(required):
            found.append(Finding(os.path.relpath(required, root), 0,
                                 "required design document is missing"))
    modules_dir = os.path.join(tree, "modules")

    for path in markdown_files(tree):
        rel = os.path.relpath(path, root)
        text = strip_comments(read(path))
        name = os.path.basename(path)
        is_module = os.path.dirname(path) == modules_dir
        found += check_common(rel, text, language)
        if name not in ("README.md", "architecture.md"):
            found += check_filename_sequence(rel)
        if is_module:
            if not name.startswith("design-"):
                found.append(Finding(rel, 0,
                                     "module document name does not match design-<slug>"))
            else:
                slug = name[len("design-"):-len(".md")]
                if not SLUG_RE.match(slug):
                    found.append(Finding(rel, 0,
                                         f"slug {slug!r} is not lowercase kebab-case ASCII"))
            found += check_heading_language(rel, text, language, MODULE_SECTIONS)
            found += check_section_order(rel, text, language, MODULE_SECTIONS)
            found += check_design_status(rel, parse_sections(text), language, roll_up=False)
            responsibilities = [s for s in parse_sections(text) if s.level == 3
                                and normalize_heading(s.title)
                                == normalize_heading(canonical(NON_RESPONSIBILITIES, language))]
            if not responsibilities:
                found.append(Finding(rel, 0,
                                     f"no {canonical(NON_RESPONSIBILITIES, language)!r} subsection; what a "
                                     f"module does not own is written down"))
        elif name == "architecture.md":
            sections = parse_sections(text)
            found += check_heading_language(rel, text, language, ARCHITECTURE_SECTIONS)
            found += check_section_order(rel, text, language, ARCHITECTURE_SECTIONS)
            found += check_design_status(rel, sections, language, roll_up=True)
            for key in ("module-dependency-graph", "logical-deployment"):
                target = [s for s in sections if s.level == 2
                          and match_section(s.title, ARCHITECTURE_SECTIONS, language)[0] == key]
                for section in target:
                    if not any(f[1].lower() == "mermaid" for f in section.fences()):
                        found.append(Finding(rel, section.line,
                                             f"section {section.title!r} carries no mermaid "
                                             f"diagram; this diagram is mandatory"))
        elif name != "README.md" and os.path.dirname(path) == tree:
            found.append(Finding(rel, 0,
                                 f"unexpected document at the design tree root; a module "
                                 f"document lives in {rel_tree}/modules/"))
        if os.path.dirname(path) not in (tree, modules_dir):
            found.append(Finding(rel, 0, "design tree has no directory below modules/"))

    if os.path.isfile(readme):
        found += check_index(readme, os.path.relpath(readme, root), tree, root,
                             language, "design")
        if os.path.isdir(modules_dir):
            documented = set()
            for path in markdown_files(modules_dir):
                documented.add(os.path.relpath(path, os.path.dirname(readme)))
    if os.path.isfile(architecture):
        text = strip_comments(read(architecture))
        rolled: set[str] = set()
        for section in parse_sections(text):
            if section.level == 2 and match_section(
                    section.title, ARCHITECTURE_SECTIONS, language)[0] == "status":
                for n, row in status_entries(section):
                    for target in LINK_RE.findall(row):
                        rolled.add(os.path.normpath(
                            os.path.join(os.path.dirname(architecture), target.split("#")[0])))
        if os.path.isdir(modules_dir):
            for path in markdown_files(modules_dir):
                if os.path.normpath(path) not in rolled:
                    found.append(Finding(os.path.relpath(architecture, root), 0,
                                         f"implementation status has no row for "
                                         f"{os.path.relpath(path, root)}"))
    return found


# --- adr tree ------------------------------------------------------------

def adr_id(name: str) -> str | None:
    stem = name[:-3] if name.endswith(".md") else name
    if not stem.startswith("adr-"):
        return None
    parts = stem.rsplit("-", 3)
    if len(parts) < 4:
        return None
    slug, date = parts[0], "-".join(parts[1:])
    if not ISO_DATE_RE.match(date) or not SLUG_RE.match(slug[len("adr-"):]):
        return None
    return f"ADR-{slug[len('adr-'):]}-{date}"


def check_adr_record(path: str, rel: str, text: str, language: str) -> tuple[list[Finding], str | None, str | None]:
    """Findings, the record's own id, and the id it declares as its successor."""
    found: list[Finding] = []
    name = os.path.basename(path)
    record_id = adr_id(name)
    if record_id is None:
        found.append(Finding(rel, 0,
                             "file name does not match adr-<slug>-<YYYY-MM-DD>"))
        found += check_filename_sequence(rel)
        return found, None, None
    date = record_id.rsplit("-", 3)
    date_text = "-".join(date[1:])
    try:
        datetime.date.fromisoformat(date_text)
    except ValueError:
        found.append(Finding(rel, 0, f"file name carries no real calendar date ({date_text})"))

    found += check_heading_language(rel, text, language, ADR_SECTIONS)
    found += check_section_order(rel, text, language, ADR_SECTIONS)

    sections = parse_sections(text)
    superseded_by = None
    for section in sections:
        if section.level != 2:
            continue
        key, _ = match_section(section.title, ADR_SECTIONS, language)
        if key == "status":
            body = " ".join(t for _, t in section.prose_lines())
            accepted = canonical(ADR_ACCEPTED, language).casefold() in body.casefold()
            deprecated = canonical(ADR_DEPRECATED, language).casefold() in body.casefold()
            supersession = SUPERSEDED_RE.search(body)
            if canonical(ADR_PROPOSED, language).casefold() in body.casefold():
                found.append(Finding(rel, section.line,
                                     "status is Proposed; an undecided decision belongs to "
                                     "the issue tracker, not to a record"))
            elif supersession:
                superseded_by = supersession.group(1)
            elif not (accepted or deprecated):
                found.append(Finding(rel, section.line,
                                     f"status is none of {canonical(ADR_ACCEPTED, language)!r}, "
                                     f"{canonical(ADR_DEPRECATED, language)!r} or a supersession by a "
                                     f"named record"))
        if key == "options":
            options = [s for s in sections if s.level == 3 and s.line > section.line
                       and s.line < next((x.line for x in sections
                                          if x.level == 2 and x.line > section.line), 1 << 30)]
            if len(options) < 2:
                found.append(Finding(rel, section.line,
                                     f"options considered lists {len(options)} option(s); a "
                                     f"decision weighs alternatives, each an H3"))
    return found, record_id, superseded_by


def check_adr_tree(tree: str, root: str, language: str) -> list[Finding]:
    found: list[Finding] = []
    readme = os.path.join(tree, "README.md")
    if not os.path.isfile(readme):
        found.append(Finding(os.path.relpath(readme, root), 0,
                             "the ADR tree has no README index"))
    records: dict[str, str] = {}
    successors: dict[str, str] = {}
    bodies: dict[str, str] = {}
    statuses: dict[str, tuple[str, int]] = {}

    for path in markdown_files(tree):
        rel = os.path.relpath(path, root)
        if os.path.basename(path) == "README.md":
            continue
        if os.path.dirname(path) != tree:
            found.append(Finding(rel, 0, "the ADR tree is flat; records live beside README.md"))
            continue
        text = strip_comments(read(path))
        found += check_common(rel, text, language)
        record_findings, record_id, superseded_by = check_adr_record(path, rel, text, language)
        found += record_findings
        if record_id:
            records[record_id] = rel
            bodies[record_id] = text
            if superseded_by:
                successors[record_id] = superseded_by
            for section in parse_sections(text):
                if section.level == 2 and match_section(
                        section.title, ADR_SECTIONS, language)[0] == "status":
                    statuses[record_id] = (" ".join(t for _, t in section.prose_lines()),
                                           section.line)

    for old, new in sorted(successors.items()):
        if new not in records:
            found.append(Finding(records[old], statuses[old][1],
                                 f"status names {new} as the successor, and no such record exists"))
            continue
        if not SUPERSEDES_RE.search(bodies[new]) or old not in bodies[new]:
            found.append(Finding(records[new], 0,
                                 f"does not state that it supersedes {old}; the supersession "
                                 f"pair is stated in both records"))

    if os.path.isfile(readme):
        rel_readme = os.path.relpath(readme, root)
        found += check_index(readme, rel_readme, tree, root, language, "ADR")
        text = strip_comments(read(readme))
        topics = [s for s in parse_sections(text) if s.level == 2]
        if not topics:
            found.append(Finding(rel_readme, 0,
                                 "the index is not grouped by topic; each topic is an H2"))
        for n, raw, in_fence, _ in scan_lines(text):
            if in_fence or not raw.strip().startswith("|") or TABLE_SEPARATOR_RE.match(raw.strip()):
                continue
            targets = [t for t in LINK_RE.findall(raw) if t.endswith(".md")]
            if not targets:
                continue
            record_id = adr_id(os.path.basename(targets[0]))
            if record_id is None or record_id not in records:
                continue
            date_text = "-".join(record_id.rsplit("-", 3)[1:])
            if date_text not in LINK_RE.sub("", raw):
                found.append(Finding(rel_readme, n,
                                     f"index row for {record_id} does not carry the record's "
                                     f"own date"))
            row_status = raw.split("|")
            body = statuses.get(record_id, ("", 0))[0]
            for key, name in (("accepted", ADR_ACCEPTED), ("deprecated", ADR_DEPRECATED)):
                in_record = name[language].casefold() in body.casefold()
                in_row = any(name[language].casefold() in cell.casefold()
                             for cell in row_status[2:])
                if in_record != in_row:
                    found.append(Finding(rel_readme, n,
                                         f"index row for {record_id} disagrees with the "
                                         f"record's own status"))
                    break
    return found


# --- requirements tree ---------------------------------------------------

def check_requirement_entries(rel: str, text: str, language: str, slug: str) -> list[Finding]:
    found: list[Finding] = []
    sections = parse_sections(text)
    seen: dict[str, int] = {}
    status_label = {"en": "status", "zh-CN": "状态"}[language]
    e2e_label = {"en": "e2e test", "zh-CN": "端到端测试"}[language]
    for index, section in enumerate(sections):
        if section.level != 3:
            continue
        title = normalize_heading(section.title)
        if not title.startswith("req-"):
            continue
        match = REQ_ID_RE.match(title)
        if not match:
            found.append(Finding(rel, section.line,
                                 f"requirement id {section.title!r} does not match "
                                 f"REQ-<slug>-NNN"))
            continue
        if match.group(1) != slug:
            found.append(Finding(rel, section.line,
                                 f"requirement id carries slug {match.group(1)!r}; the "
                                 f"document's slug is {slug!r}"))
        if len(match.group(2)) != 3:
            found.append(Finding(rel, section.line,
                                 f"requirement number {match.group(2)!r} is not three digits"))
        if title in seen:
            found.append(Finding(rel, section.line,
                                 f"requirement id {title} is used twice; an id is unique "
                                 f"within its document"))
        seen[title] = section.line

        body = [t for _, t in section.prose_lines()]
        joined = "\n".join(body)
        if VAGUE_STATUS and any(v.casefold() in joined.casefold() for v in VAGUE_STATUS):
            found.append(Finding(rel, section.line, "vague status wording"))
        if canonical(DESIGN_STATUS["partial"], language).casefold() in joined.casefold():
            found.append(Finding(rel, section.line,
                                 f"{canonical(DESIGN_STATUS['partial'], language)!r} does not exist in a "
                                 f"requirements document; split the requirement"))
        has_status = False
        has_e2e = False
        for line in body:
            label = re.match(r"^\s*[-*+]\s*\**\s*([^*:：]+?)\s*\**\s*[:：]", line)
            if not label:
                continue
            name = label.group(1).strip().casefold()
            if name == status_label:
                has_status = True
                if not status_of_row(line.split(":", 1)[-1], REQUIREMENT_STATUS, language):
                    found.append(Finding(rel, section.line,
                                         f"status is neither "
                                         f"{canonical(REQUIREMENT_STATUS['implemented'], language)!r} nor "
                                         f"{canonical(REQUIREMENT_STATUS['not-implemented'], language)!r}"))
            if name == e2e_label:
                has_e2e = True
        if not has_status:
            found.append(Finding(rel, section.line, "requirement records no implementation status"))
        if not has_e2e:
            found.append(Finding(rel, section.line, "requirement records no end-to-end test status"))
    return found


def check_requirements_tree(tree: str, root: str, language: str) -> list[Finding]:
    found: list[Finding] = []
    readme = os.path.join(tree, "README.md")
    overview = os.path.join(tree, "overview.md")
    for required in (readme, overview):
        if not os.path.isfile(required):
            found.append(Finding(os.path.relpath(required, root), 0,
                                 "required requirements document is missing"))
    journeys = os.path.join(tree, "journeys")
    features = os.path.join(tree, "features")

    for path in markdown_files(tree):
        rel = os.path.relpath(path, root)
        name = os.path.basename(path)
        text = strip_comments(read(path))
        found += check_common(rel, text, language)
        if name not in ("README.md", "overview.md"):
            found += check_filename_sequence(rel)
        parent = os.path.dirname(path)
        if parent == features:
            if not name.startswith("prd-"):
                found.append(Finding(rel, 0, "capability document name does not match prd-<slug>"))
                continue
            slug = name[len("prd-"):-len(".md")]
            if not SLUG_RE.match(slug):
                found.append(Finding(rel, 0, f"slug {slug!r} is not lowercase kebab-case ASCII"))
            found += check_heading_language(rel, text, language, FEATURE_SECTIONS)
            found += check_section_order(rel, text, language, FEATURE_SECTIONS)
            found += check_requirement_entries(rel, text, language, slug)
        elif parent == journeys:
            if not name.startswith("journey-"):
                found.append(Finding(rel, 0, "journey document name does not match journey-<slug>"))
                continue
            slug = name[len("journey-"):-len(".md")]
            if not SLUG_RE.match(slug):
                found.append(Finding(rel, 0, f"slug {slug!r} is not lowercase kebab-case ASCII"))
            found += check_heading_language(rel, text, language, JOURNEY_SECTIONS)
            found += check_section_order(rel, text, language, JOURNEY_SECTIONS)
        elif parent == tree and name == "overview.md":
            found += check_section_order(rel, text, language, OVERVIEW_SECTIONS,
                                         allow_unknown=True)
        elif parent == tree and name != "README.md":
            found.append(Finding(rel, 0,
                                 "unexpected document at the requirements tree root; a "
                                 "capability lives in features/, a journey in journeys/"))
        elif parent not in (tree, journeys, features):
            found.append(Finding(rel, 0,
                                 "requirements tree has no directory below journeys/ and features/"))

    if os.path.isfile(readme):
        found += check_index(readme, os.path.relpath(readme, root), tree, root,
                             language, "requirements")
    return found


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Structural checker for the document-standards skill.")
    parser.add_argument("--root", default=".",
                        help="repository root holding CLAUDE.md (default: .)")
    args = parser.parse_args(argv)
    root = os.path.abspath(args.root)
    if not os.path.isdir(root):
        print(f"error: --root is not a directory: {args.root}", file=sys.stderr)
        return 2

    config, errors = parse_documentation_config(root)
    if errors:
        for error in errors:
            print(f"error: {error}", file=sys.stderr)
        return 2
    language = config["language"]

    found: list[Finding] = []
    checked = []
    for key, checker, label in (("design", check_design_tree, "design"),
                                ("adr", check_adr_tree, "ADR"),
                                ("requirements", check_requirements_tree, "requirements")):
        tree = os.path.join(root, config[key])
        if not os.path.isdir(tree):
            continue
        checked.append(f"{label} ({os.path.relpath(tree, root)})")
        found += checker(tree, root, language)

    if not checked:
        print("no documentation tree found at the configured paths "
              f"(design: {config['design']}, adr: {config['adr']}, "
              f"requirements: {config['requirements']})")
        return 0

    glossary = os.path.join(root, config["glossary"])
    if not os.path.isfile(glossary):
        found.append(Finding(config["glossary"], 0,
                             "the configured glossary does not exist; the project has "
                             "exactly one, shared by every document type"))

    for finding in sorted(found, key=Finding.sort_key):
        print(finding.render())
    if found:
        print(f"FAILED: {len(found)} structural problem(s) in {', '.join(checked)}; "
              f"the standard is .claude/skills/document-standards/")
        return 1
    print(f"OK: {', '.join(checked)} structurally conform to document-standards "
          f"(language: {language})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
