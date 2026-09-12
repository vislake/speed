# Design Document Standard

Read together with `SKILL.md`, whose common rules apply in full and are not repeated here.

**The premise for this document type**: a design document is the *final design conclusion*, not a design log. Alternatives considered, the choice made and the reasoning behind it belong to the ADR; the design document carries only the reference.

---

## 1. Layout and Naming

```
docs/design/                        # relocatable via CLAUDE.md
├── README.md                       # entry point: brief introduction + complete index
├── architecture.md                 # high-level overview
└── modules/
    ├── design-<slug>.md
    └── design-<slug>-<aspect>.md   # only when a module document grows unwieldy
```

The glossary is **not** part of this tree. The project has one glossary, shared by every document type, at the configured `glossary` path.

**Required:**
- `README.md` and `architecture.md` are fixed names — the explicit exceptions to the `design-<slug>` pattern, which applies inside `modules/`.
- **File names carry no sequence number**, anywhere. Ordering lives in the README index, not in file names.
- A slug is lowercase kebab-case, ASCII only, and **matches the module's directory or package name in the code**, so the document-to-code mapping is mechanical.
- **A slug is permanent.** Renaming breaks every link that points at it.
- One document per real module boundary in the code. A unit with no external interface contract of its own is folded into its parent module's document, not given a file.
- An oversized module document may split by aspect into `design-<slug>-<aspect>.md`, still with no sequence number. The parent slug stays the prefix.

## 2. `README.md`

A brief introduction to the system, then the index.

**Required:**
- **The index covers every file in the tree, each appearing exactly once.** A file absent from the index is an orphan, and an orphan is an error.
- The introduction states what the system is and what it is for. It does not summarize the architecture — `architecture.md` does that, and two summaries drift.
- Implementation status does not appear here. The roll-up belongs to `architecture.md`.

Template: `templates/design-readme.md`.

## 3. `architecture.md`

The high-level view of the whole system.

**Required sections:**
- **Overview** — the system's shape and its decomposition into modules.
- **Module dependency graph** — mandatory diagram.
- **Logical deployment architecture** — mandatory diagram.
- **Cross-cutting design** — the concerns no single module owns: error model, authentication and authorization boundaries, data consistency and transaction boundaries, concurrency model, failure and degradation behaviour, observability, performance and capacity targets, security boundaries. Omit a heading only when the concern genuinely does not exist in the system.
- **Architecture invariants** — the rules the system must not violate, for example a strictly bottom-up dependency direction. These are what a review can actually adjudicate against, so each is stated so that a violation is recognizable.
- **Implementation status** — one line per module, linking to that module's document. Roll-up only; detail lives in the module document.

**Conditional sections** — include when the trigger holds, delete outright otherwise:
- **Physical deployment architecture** — when the system runs as more than one process or replica.
- **Inter-module call relationships** — when call chains are non-trivial.
- **Data flow** — when data moves across module boundaries.

Template: `templates/design-architecture.md`.

## 4. Module Design Documents

Fixed section order, so modules can be read against each other and checked mechanically:

1. **Responsibilities** — and, explicitly, **non-responsibilities**. Writing down what the module does not own is the cheapest available defence against scope creep.
2. **External interface contract** — what other modules may call, and what stability is promised. This is the most valuable part of a module document.
3. **Dependencies** — what it depends on, and whether the direction satisfies the architecture invariants.
4. **Diagrams** — UML and call relationships, selected per section 5.
5. **Core data structures**.
6. **Core algorithms and flows** — with complexity analysis for performance-sensitive algorithms. Procedural flows need no complexity analysis.
7. **Error and failure semantics** — failure modes, retry, idempotency, degradation behaviour.
8. **Concurrency and transaction boundaries** — when applicable.
9. **Extension points** — when applicable.
10. **Implementation status**.

Template: `templates/design-module.md`.

## 5. Diagrams

**Required:**
- **Mermaid is the only diagram format.** No ASCII art, no linked images, no binary sources. A diagram must be reviewable as a text diff.
- **Every diagram carries accompanying prose.** The diagram carries structure; the constraints, the invariants and the pointer to the rationale are carried by the text. A section holding a diagram and nothing else is incomplete.
- **Select UML diagram types by fact, never to fill a template.** Class diagram for core data structures and their relationships; sequence diagram for cross-module interaction; state diagram for entities with a lifecycle; activity diagram for branch-heavy flows. When the module holds no corresponding fact, draw nothing.
- Rendering must be verified after every edit — a common rule, stated in `SKILL.md`.

## 6. Implementation Status

The fastest-rotting content in any design document, and therefore the most tightly constrained.

**Required:**
- The value is one of exactly `Implemented`, `Partially implemented`, `Not implemented`, rendered in the document's language with consistent wording. **DO NOT** write vague states such as `mostly complete` or `basically working`.
- **`Partially implemented` must name what is missing.** Without that, the entry carries no information.
- Every entry carries a **code anchor: path plus symbol name, no line number**. A status is an assertion falsifiable against the code, not an impression.
- **The module document is authoritative for its own module**; `architecture.md` carries one roll-up line per module. The same fact written out in detail twice will drift.

`Partially implemented` exists here and **does not exist in a requirements document**. The two document types look at the system from different places: a design document is the implementation view, where partial implementation is a real and informative state; a requirements document is the outside view, where it is not.

## 7. What Never Appears in a Design Document

- Change history, version numbers, authors, dates, review records
- TODO lists, schedules, milestones, owners
- Requirement content (reference the requirement id instead)
- Alternatives considered, the choice made, and the reasoning behind it (reference the ADR instead)
- Large pasted code (signatures or pseudocode only, and only where they carry design meaning)
- API reference material (that is generated from the code)
- Narrative about how the design evolved

## 8. Checklist

The common checklist in `SKILL.md` applies as well.

- [ ] File name matches `design-<slug>`, with no sequence number; the slug matches the module's name in the code
- [ ] No alternatives or rationale — ADR referenced by id instead
- [ ] Every diagram is Mermaid and carries prose
- [ ] No diagram drawn to fill a template slot
- [ ] Conditional sections that do not apply are deleted, not left empty
- [ ] Module document follows the fixed section order, including non-responsibilities
- [ ] External interface contract states what is callable and what stability is promised
- [ ] Complexity analysis present for every performance-sensitive algorithm
- [ ] Every implementation-status entry has a code anchor and, if partial, names what is missing
- [ ] Status detail appears only in the module document; `architecture.md` carries one line per module
- [ ] Every file appears in the README index exactly once
