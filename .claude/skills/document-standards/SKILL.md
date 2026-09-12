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

This file carries what is common to every document type, and points at each type's own standard. Load the type's standard as well before writing.

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

**No valueless counts.**
- **DO NOT** state the cardinality of a structure the document already lists: `the project contains 21 modules`, `divided into five stages`, `there are 3 extension points`. The only source of that number is the table right below it, the reader can count it, and every addition or removal requires editing it. **Write the list, never the size of the list.**
- **DO NOT** refer to enumerated items by position or count: `the former`, `the three above`, `the first two`. Inserting an item invalidates all of them. Name the item instead.
- Numbers that *are* the design stay: complexity bounds, performance and capacity targets, protocol-fixed widths, timeouts, retry counts, thresholds, and closed value sets that are themselves a constraint. The test: **when this number changes, has the design changed, or has a list merely grown?** Delete the second kind.

**One language per document.**
- Mixed-language text is allowed only for universally recognized terms, and for project-agreed terms **registered in the glossary**. Nothing else.

**Glossary.**
- **The project has exactly one glossary**, at the configured `glossary` path. It serves every document type. A glossary per documentation tree drifts apart.
- The glossary is mandatory once the documentation uses any project-agreed term.
- An entry carries the term, its definition, and its counterpart in the source language when the document language differs from it.
- Universally recognized terms stay out — registering them adds maintenance without adding meaning.
- **Once a term is registered, its wording is uniform across all documentation.** Synonym drift is a defect.

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

## 5. Common Checklist

Each document type's standard adds its own checklist on top of this one.

- [ ] Path and language resolved from `CLAUDE.md`, not assumed
- [ ] One language throughout; every mixed-in term is universally recognized or registered in the glossary
- [ ] No first person, no second person, no subjective modifiers, no restating transitions
- [ ] No counts of structures, no positional references to enumerated items
- [ ] No history, no version, no author, no date, no TODO, no schedule
- [ ] Content owned by another document is referenced by id, not restated
- [ ] Every diagram in the edited document was rendered and confirmed to succeed
- [ ] Code references carry no line numbers
- [ ] The document changed in the same commit as the code that changed what it asserts
