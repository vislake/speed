# Requirements Document Standard

Read together with `SKILL.md`, whose common rules apply in full and are not repeated here.

**The premise for this document type**: a requirements document states what the system must do, seen from outside the system. **Its primary reader is a developer.** It leans engineering — it describes the behaviour and the constraints the system must exhibit, and it does not argue the commercial value of that behaviour.

---

## 1. Two Boundaries

A requirements document sits between a commercial document above it and a design document below it, and both boundaries leak if left unstated.

**Downward — requirements state the problem, not the solution.** A document that specifies a database table, an API shape, a class name or a technology choice has stopped being a requirements document.
- An **externally imposed constraint is a legitimate requirement**: must run without network access, must integrate with a named counterpart system, must satisfy a named regulation.
- An **internal choice made to implement something is not**: adopt PostgreSQL, decouple through a message queue.
- The test: **if this were not written down, would the team still be free to choose?** Free → it is design. Not free → it is a requirement.

**Upward — commercial reasoning stays out.** A commercial fact enters only once it has been converted into a verifiable constraint on the system, and it is then **written as a constraint, never as an argument**.
- Out: `usage-based pricing improves conversion, so metering is needed`.
- In: `The system MUST meter API invocations and MUST be able to export per-billing-period usage detail` — with an id and acceptance criteria, like any other requirement.

These two rules are a pair: the first stops the document leaking down into implementation, the second stops it leaking up into business.

## 2. Layout and Naming

```
docs/prd/                       # relocatable via CLAUDE.md
├── README.md                   # entry point: brief introduction + complete index
├── overview.md                 # system overview and overall scope boundary
├── journeys/
│   └── journey-<slug>.md       # one journey per file
└── features/
    └── prd-<slug>.md           # one capability per file
```

**Required:**
- `README.md` and `overview.md` are fixed names. `journey-<slug>` applies inside `journeys/`, `prd-<slug>` inside `features/`.
- **File names carry no sequence number**, anywhere. Ordering lives in the README index.
- A slug is lowercase kebab-case and ASCII only, and names what the file covers. Unlike a design slug, it does not have to match anything in the code.
- **A slug is permanent**, and so is the split it implies. **Moving a requirement to a different document changes its id and breaks every reference to it** — decide the dimension the documents are split along once, before writing them.

`README.md` carries a brief introduction and the index. **The index covers every file in the tree, each appearing exactly once**; a file absent from it is an orphan, and an orphan is an error. Status does not appear there — it lives on each requirement and each journey, in the document that owns it.

`overview.md` describes what the system is, which capabilities it offers to the outside, and the overall scope boundary. It does not describe product positioning or a target market.

Templates: `templates/prd-readme.md`, `templates/prd-overview.md`.

## 3. User Journeys

A journey is **one complete path a user takes through the system**, seen from outside. It is the unit an end-to-end test naturally covers, which is why it carries one.

**A journey records four things:**
- **Who** — the actor taking the path, and the authority they hold.
- **Under what precondition** — what must already be true before the path can start.
- **What they do** — the steps, in order.
- **What they get** — the observable outcome the path ends in.

All four are mandatory. A path missing any of them is not yet a journey: with no actor it cannot be attributed, with no precondition it cannot be set up, with no steps there is nothing to run, and with no outcome there is nothing to assert. **These are also exactly what an end-to-end test needs** — the actor to sign in as, the fixture to arrange, the actions to drive, the assertion to make — which is why a journey maps onto one test rather than onto several.

**Required:**
- **A journey is a real business process, and it spans several requirements.** This is definitional, not incidental: a path that exercises a single requirement is not a journey, because that requirement's own acceptance criteria already cover it. A journey exists to verify what no individual requirement can — that the requirements compose into a process the business actually runs.
- Journeys are drawn from processes the business actually performs, not constructed by walking through the feature list. A walkthrough invented to touch every screen tests the screens; it does not test a business process.
- A requirements documentation set has journeys. They live in `journeys/`, **one journey per file**. A file describing more than one path is split into separate files.
- **The slug is the journey's identifier** — there is no separate number, and file names carry no sequence number. Reference a journey by its file.
- The directory is flat. **A journey that crosses several capabilities is still one journey file**; journeys are not organized by capability, because the paths users actually take do not respect capability boundaries.
- **Steps are written from the actor's point of view.** **DO NOT** write internal steps the actor cannot observe — that is design leaking into requirements.
- A journey may carry a flow diagram when it branches. A linear journey needs none.
- **Every journey records its end-to-end test**: path plus test name, or `Not implemented`. Because one journey is one file, this is the honest granularity for end-to-end coverage — the journeys with no test are the end-to-end gap, and scanning the directory shows it.

**Journeys and requirements do not replace each other.** A requirement is an assertion about one behaviour; a journey is a business process running across many of them. A journey therefore exercises several requirements, and a requirement appears in several journeys. The same end-to-end test may be referenced from both: a reference is not a status, so it does not drift.

Template: `templates/prd-journey.md`.

## 4. Sections of a Capability Requirements Document

**Functional requirements is the only mandatory section.** Every other section is included when it has content and **deleted outright when it does not** — never left as an empty heading.

The order below is fixed. It is an ordering rule, not a completeness rule: which sections appear is the document's choice, but the sections that do appear follow this order, so documents can be read against each other.

1. **Problem statement** — what has to be solved, and for whom.
2. **Scope** — and, explicitly, what is out of scope.
3. **Actors** — who interacts with the system, and with what authority. These are system participants, **not marketing personas**.
4. **Functional requirements** — mandatory.
5. **Non-functional requirements** — quantified, and sourced (section 7).
6. **Data requirements** — what data exists conceptually, its retention, its privacy classification. **Not a schema.**
7. **External interface requirements** — which external systems and actors the system interacts with. **Not an interface design.**
8. **Constraints** — regulatory, business and external technical constraints imposed from outside.
9. **Assumptions and dependencies**.

Flows do not appear here. They belong to `journeys/`.

Template: `templates/prd-feature.md`.

## 5. Writing a Requirement

**One requirement, one id, one testable assertion.**
- A compound requirement (`the system supports export, and notifies the user afterwards`) cannot be tested as a unit. Split it into separate requirements with separate ids.
- Use normative keywords with fixed meaning — `MUST` / `SHOULD` / `MAY`, or the document language's established equivalents. **DO NOT** write `as far as possible`, `preferably`, `consider supporting`. A statement that cannot be violated cannot be verified.
- **Every requirement is verifiable.** A statement that cannot be tested, measured or demonstrated is not a requirement.
- Acceptance criteria are written so that passing and failing are unambiguous. **Gherkin is not required** — forcing `Given / When / Then` onto a simple requirement multiplies its length without adding precision.

**Identifiers.**
- The form is `REQ-<slug>-NNN`, where `<slug>` is the document's slug.
- **`NNN` is unique within its document**, starting at `001`. It is not globally unique; the slug is what keeps a cross-document reference unambiguous.
- **A retired id is never reused.** Deleting a requirement retires its number; the next requirement takes the next unused one.

## 6. Requirement Status

Every requirement records whether it is implemented, and whether its end-to-end test exists. These are **two independent axes** — implemented with no end-to-end test is a common state, and it must stay visible rather than being averaged away.

**Required:**
- Implementation status is one of exactly **`Implemented`** or **`Not implemented`**, rendered in the document's language with consistent wording.
- **There is no `Partially implemented`.** A requirement that is partly done is too coarsely written: **split it** into requirements that are each individually answerable.
- This is the reason the rule earns its place: the status field defines the granularity of a requirement as **the smallest unit a yes/no answer applies to**. A requirement that resists the split was never verifiable.
- The end-to-end test axis records whether the test exists. When it does, reference it by **path plus test name, never a line number**.

This differs deliberately from a design document, which does have `Partially implemented`. The two types look at the system from different places: a requirements document is the customer's view, where `partly usable` is not a meaningful answer to `does this work`; a design document is the implementation view, where partial implementation is a real and informative state.

## 7. Nothing Is Fabricated

**A requirement traces to something the user stated, or to a citable external authority** — a regulation, a contract, an interface agreement with a counterpart system. Requirements are not invented by whoever writes the document.

**Non-functional numbers are where this rule bites.** Availability, latency, throughput, capacity, retention period:

- **DO NOT** pick a plausible-looking number. `99.99%`, `P99 < 10ms`, `10000 concurrent users` are precisely the strings that autocomplete well — quantified, verifiable, formally compliant with every other rule here, and baseless.
- **Passing the quantifiable check is not passing the sourced check.** They are two independent gates, and the second one is the one that gets skipped.
- A dimension the user has not raised is **omitted entirely**, not filled with a default. No source means no requirement.
- When a number is genuinely needed and the user has not supplied it, **ask the user**. Do not decide it for them, and do not park it in the document as an open question — an undecided item is not a conclusion and does not belong in any document.

The consequence of getting this wrong is expensive and delayed: downstream treats an invented number as a real constraint and makes architectural trade-offs against it.

## 8. What Never Appears in a Requirements Document

**Solution content:**
- Schema, API shape, class names, technology choices
- UI visual specifications
- Internal steps in a journey that the actor cannot observe

**Project management:**
- Effort estimates, schedules, milestones, owners, priority

**Commercial content:**
- Business goals and business model
- Market and competitive analysis
- Pricing strategy, ROI and revenue projections
- Go-to-market and promotion plans
- Marketing personas, business KPIs

**Process residue:**
- Open questions and undecided items
- Alternatives considered and the reasoning behind the choice (reference the ADR instead)

## 9. Checklist

The common checklist in `SKILL.md` applies as well.

- [ ] File names match `prd-<slug>` or `journey-<slug>`, with no sequence number
- [ ] Every requirement states a problem, not a solution
- [ ] No commercial argument — commercial facts appear only as verifiable constraints
- [ ] Each requirement is a single testable assertion with a normative keyword
- [ ] Each requirement has acceptance criteria with an unambiguous pass or fail
- [ ] Ids follow `REQ-<slug>-NNN`, unique within the document, with no reused retired number
- [ ] Every requirement carries implementation status and end-to-end test status
- [ ] No `Partially implemented` anywhere — any such requirement has been split
- [ ] One journey per file; every journey records who, the precondition, the steps and the outcome
- [ ] Every journey is a process the business actually runs, spanning several requirements
- [ ] Every journey records its end-to-end test
- [ ] Journey steps are observable by the actor; no internal steps
- [ ] Every non-functional number traces to the user or a citable external authority
- [ ] No dimension filled with a default the user never raised
- [ ] Sections with no content are deleted, not left empty; the sections present follow the fixed order
- [ ] Every file appears in the README index exactly once
