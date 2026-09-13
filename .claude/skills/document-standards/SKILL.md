---
name: document-standards
description: Documentation standards for design documents, requirements documents and architecture decision records — path and language configuration, the rules common to every document type, and the entry point to each type's own standard. Load before creating or editing any document under the configured documentation paths.
triggers:
  - writing a design document
  - editing a design document
  - creating an architecture overview
  - documenting a module design
  - adding a module to the design docs
  - reviewing documentation
  - writing a requirements document
  - writing an architecture decision record
globs:
  - "docs/design/**/*.md"
  - "docs/prd/**/*.md"
  - "docs/adr/**/*.md"
---

# Document Standards

The authoritative standard for project documentation. **This document tells you how to write the documents**; each document tells its reader how the system is built.

This file carries what is common to every document type, and points at each type's own standard. Load the type's standard as well before writing, and run the structural checker (section 5) before calling the work finished.

**The premise that overrides everything else**: a document states the current conclusion, not the process that produced it. It says what the system is, not how the team arrived at it, not who decided it, and not when. Every rule below follows from that premise, and the premise outranks any stylistic preference.

---

## 1. Configuration

Documentation paths and language are declared in `CLAUDE.md` under a fixed heading, with lowercase keys:

```markdown
## Documentation

- design: docs/design
- requirements: docs/prd
- adr: docs/adr
- glossary: docs/glossary.md
- language: zh-CN
```

**Required:**
- Read this block before creating or editing any document. A missing key takes its default: `design` → `docs/design`, `requirements` → `docs/prd`, `adr` → `docs/adr`, `glossary` → `docs/glossary.md`.
- Resolve the document language in this order: the `language` key; the language of the documents already in the target directory; otherwise **ask the user**. All three inconclusive means ask — never pick a language silently.
- **Project-wide language rules outrank the `language` key.** Where the project already fixes the language of a directory, that rule wins and a `language` declaration never licenses a violation of it.

## 2. Rules Common to Every Document

**Conclusions only.**
- **DO NOT** record how the document was built: no change history, no version number, no author, no date, no review record, no "originally X, later changed to Y" narrative. Git is the authority for all of it. The single exception is an ADR's decision date, which is content rather than metadata — see the ADR standard.
- **DO NOT** record project management: no TODO, no schedule, no milestone, no owner, no priority.
- **DO NOT** duplicate content that belongs to another document. Reference a requirement by id and a decision by ADR id; never restate their content.

**No first person, no subjective language.**
- **DO NOT** use first person in any form (`we`, `our`, `I`, `the author`). The assertions in a document are guaranteed by the code and the constraints, not by a narrator. **This holds for ADRs too**: write `Adopt X`, never `We decided to adopt X`.
- **DO NOT** use second person or address the reader. A standard-conforming document is not a tutorial.
- **DO NOT** use subjective modifiers and filler: `obviously`, `simply`, `easily`, `elegant`, `powerful`, `efficient`, `excellent`, `note that`, `it is worth noting`, `unfortunately`, `of course`, `in fact`. They add no information and routinely disguise unverified claims. The same categories are banned in whatever language the document is written in.
- **DO NOT** use restating transitions: `in other words`, `that is to say`, `in summary`, `to sum up`. Needing to say it in other words means the previous sentence should be rewritten instead.
- **Replace every subjective judgement with a measurable quantity or an explicit constraint.** `Performance is good` becomes `P99 latency target 50ms`. `X was chosen because Y` becomes `Adopt X (ADR-tenant-isolation-2026-03-14)`.

**A rule names what it binds.**
- **A normative sentence states the subject it constrains, not only the constraint.** `every`, `all`, `always` and `never` are the visible signal, but **a bare plural noun is the common one**: `Package-level functions and variables are limited to ...` contains no quantifier at all, and its subject is nonetheless every package-level function, including the unexported helpers no implementation can do without. `Exported package-level functions and variables ...` is the sentence that was meant.
- **The test: take an ordinary member of the subject and apply the rule to it literally.** Ordinary members come from two places — something this document already describes and plainly endorses, **or something any implementation of it must contain**. The second source is not optional: unexported helpers appear nowhere in an architecture document, and only "every Go implementation has them" catches the sentence above. If the literal reading forbids that member, the scope is missing. **A rule that outlaws the system's own accepted practice is not strict, it is unfinished.**
- **A layer's rule binds that layer.** A rule about what a component must produce does not reach what merely passes through it. `The table must cover every startup failure` reached failures the component only relays, which would have obliged it to name every failure mode of every module — in a component whose whole point is that it knows none of them.

**No valueless counts.**
- **DO NOT** state the cardinality of a structure the document already lists: `the project contains 21 modules`, `divided into five stages`, `there are 3 extension points`. The only source of that number is the table right below it, the reader can count it, and every addition or removal requires editing it. **Write the list, never the size of the list.**
- **DO NOT** refer to enumerated items by position or count: `the former`, `the three above`, `the first two`. Inserting an item invalidates all of them. Name the item instead.
- Numbers that *are* the design stay: complexity bounds, performance and capacity targets, protocol-fixed widths, timeouts, retry counts, thresholds, and closed value sets that are themselves a constraint. The test: **when this number changes, has the design changed, or has a list merely grown?** Delete the second kind.

**Criteria, not catalogues.**
- **When a list is what a rule means, write the criterion and let the list illustrate it.** A list used as the definition must be amended every time a case appears that the criterion already covered, and that amendment is exactly the edit nobody comes back to make.
- **The test is the one above**: add a member — has the design changed, or has a list merely grown? A closed value set that is itself the constraint stays exhaustive (the lifecycle's stages, the permitted status values). A list of instances that all satisfy one stated test is illustrative.
- **Say which kind it is.** An exhaustive list a reader may rely on and an open list of examples look identical on the page, so the difference belongs in the sentence that introduces the list. Leaving it unsaid is how a list written as examples comes to be cited elsewhere as the complete definition.

**One language per document.**
- Mixed-language text is allowed only for universally recognized terms, and for agreed terms **registered in the glossary that covers them** — the project glossary, or the local one of the context the document sits in. Nothing else.

**Glossary.**
- **Registering a term is the exception, not the habit.** A word earns an entry only when the project has given it a meaning the reader would not arrive at unaided, and reading it the ordinary way changes what the document says. Universally recognized vocabulary stays out, and so does a word this project uses in its ordinary sense: an entry that tells the reader what they already knew adds maintenance and dilutes the entries that carry real meaning.
- **The project glossary, at the configured `glossary` path, holds only the terms that hold project-wide** — the same meaning in every document, every document type and every part of the system. It is the one glossary every document shares, and it is mandatory once the documentation uses any term registered in it.
- **A term whose meaning is bounded by a context does not go into the project glossary.** A word defined by one module, one document or one subsystem means something else, or nothing, elsewhere — and the project glossary is read from everywhere. Registering it there makes the contextual meaning look global, which is the more expensive mistake: it licenses the term's use in documents its definition was never meant to reach.
- **Where a context genuinely needs its terms registered, it registers them itself** — the optional glossary section each document type's standard places in its fixed order, or a glossary file beside the documents of that scope — and the local glossary states the scope it applies to. **DO NOT** create one per documentation tree as a matter of course: a glossary that exists because the layout suggested one, rather than because a context needed it, is what drifts apart from the project glossary.
- **A local glossary never redefines, narrows or restates a project term.** Two definitions of one term is a defect regardless of which is right. A term that outgrows its context moves up — it enters the project glossary and leaves the local one; a project term never moves down.
- An entry carries the term, its definition, and its counterpart in the source language when the document language differs from it.
- **Once a term is registered, its wording is uniform throughout the scope that registered it** — the whole documentation for a project term, the owning context for a local one. Synonym drift is a defect.

**Examples convey intent, not runnability.**
- Example code **is not required to compile**, to be complete, or to match the code verbatim. Omit imports, error-handling boilerplate and initialization; `...` for elided parts is fine.
- **Trim every example to the part that carries design meaning.** Pasting a whole function turns the document into a copy of the code, which rots and carries no value. Runnable snippets belong in the code and its tests.
- This governs documentation only. Code-side rules requiring compilable examples (godoc `Example*` functions, module `AGENTS.md`) are unaffected.

**Diagrams render, or they do not ship.**
- **After editing a document that contains a diagram, render every diagram in that document and confirm every one succeeds.** This is mandatory, not advisory: a diagram that fails to parse is invisible to the reader, and the failure shows up nowhere in a plain text diff.
- **Verification means running a renderer over the blocks**, not reading them. A block that looks well-formed is not evidence.
- Verify every diagram in the edited document, not only the blocks touched. An edit elsewhere in the file can break an untouched block.
- An **empty diagram body** is the most frequent parse failure — `classDiagram` with no members fails outright. A diagram with nothing in it also renders blank, which carries no information even where it happens to parse.

**Timeliness.**
- **A code change that changes what a document asserts updates that document in the same commit.** This is the only mechanism that keeps documentation alive.
- When a document and the code disagree, the code is authoritative **and the disagreement is a defect**: fix the document.

**References.**
- Internal links are relative and point at files. Write the path from the documentation root consistently; mismatched directory depth is the most frequent link defect.
- Reference code by **path plus symbol name, never a line number**. Line numbers rot on the next edit.
- Reference a decision by its **ADR identifier (`ADR-<slug>-<date>`) plus link**.
- **References flow one way**: a design document references requirements and ADRs; requirements and ADRs do not link back. Bidirectional links rot. The one exception is the supersession pair between two ADRs — see the ADR standard for why it earns it.

## 3. Document Types

| Type | Default path | Standard |
|---|---|---|
| Design | `docs/design` | [Design Documents](references/design-documents.md) |
| Requirements | `docs/prd` | [Requirements Documents](references/requirements-documents.md) |
| ADR | `docs/adr` | [Decision Records](references/decision-records.md) |

## 4. Templates

`templates/` holds the starting point for each document type. They carry the mandatory section order; delete the sections that do not apply rather than leaving them empty.

## 5. Mechanical Check

`scripts/check_documents.py` checks the structural half of this standard: section
presence and order, file naming, index coverage, status vocabulary, ADR
supersession pairing, diagram bodies and the process metadata a document never
carries. It reads the paths and the language from the `## Documentation` block of
section 1, and skips a declared tree that does not exist.

**Running it is mandatory, not advisory.** After creating or editing any document
under the configured paths, run it from the repository root and leave it at exit 0:

```bash
python3 .claude/skills/document-standards/scripts/check_documents.py
```

Every finding is printed as `path:line: what is wrong`. Fix the documents; do not
silence the checker.

**What it does not check** — and therefore what review still owns: subjective
language, first and second person, restating transitions, valueless counts,
whether a non-functional number is sourced, whether a rejected option's reason is
real, whether a requirement is testable, whether a word deserves a glossary entry
at all and whether it belongs to the project glossary or to a local one, whether a
normative sentence's literal scope is the one intended, whether a list is meant as
a criterion or as an exhaustive set, and
whether a document still matches the code it asserts things about. It also does not render diagrams: it checks that a
mermaid block has a type and a body, while the rendering rule in section 2 stays a
step performed during the edit.

`scripts/test_check_documents.py` is the checker's own planted-violation suite
(`python3 .claude/skills/document-standards/scripts/test_check_documents.py`). A
new rule in this standard earns a check there, or it is not enforced.

## 6. Common Checklist

Each document type's standard adds its own checklist on top of this one.

- [ ] Path and language resolved from `CLAUDE.md`, not assumed
- [ ] One language throughout; every mixed-in term is universally recognized or registered in the glossary covering its scope
- [ ] No first person, no second person, no subjective modifiers, no restating transitions
- [ ] No counts of structures, no positional references to enumerated items
- [ ] Every normative sentence names what it binds, and its literal reading was applied to an ordinary member of that subject
- [ ] A list that defines gives the criterion with examples; a list meant to be exhaustive says so
- [ ] No history, no version, no author, no date, no TODO, no schedule
- [ ] Content owned by another document is referenced by id, not restated
- [ ] Every diagram in the edited document was rendered and confirmed to succeed
- [ ] Code references carry no line numbers
- [ ] The document changed in the same commit as the code that changed what it asserts
- [ ] `scripts/check_documents.py` was run and exits 0
