# Architecture Decision Record Standard

Read together with `SKILL.md`, whose common rules apply in full and are not repeated here.

**The premise for this document type**: the ADR tree is **the project's decision history**. Design and requirements documents carry no history precisely because history is delegated — text-level change to git, decision-level change to here.

This does not contradict the conclusions-only rule. What that rule bans is recording how a *document* was written; the options an ADR weighs are the **content of the decision**, not process residue. The line to hold: **an ADR is not meeting minutes.** It states what the options were at the moment of deciding. It does not narrate how the discussion got there, and it does not record who argued for what.

---

## 1. What Warrants a Record

The failure mode of this document type is inflation: hundreds of records, none of them read.

**Required:**
- A decision earns a record when it is **architecturally significant** — hard to reverse, or affecting module boundaries, public API, the data model, deployment shape, external dependencies or security posture.
- A choice that can be reversed within a day gets no record.
- **One decision per record.** A record that needs many pages is several decisions that have not been separated.

## 2. A Record Is Immutable

**A record freezes when it is accepted.** Changing a decision means writing a new record that supersedes the old one, never editing the old one.

- Typographic fixes and broken-link repairs are allowed.
- The status field is updated when the record is superseded or deprecated — that is the one substantive edit a frozen record accepts, and it exists only because a reader of the old record must not act on it unaware.
- Context, options, decision and consequences **are never edited**.

The reason is the whole point of keeping the tree: a record that can be rewritten cannot be trusted to say what was actually decided at the time, and then it has no value that git does not already provide.

## 3. Layout and Naming

```
docs/adr/                       # relocatable via CLAUDE.md
├── README.md                   # entry point: brief introduction + index grouped by topic
└── adr-<slug>-<date>.md
```

**Required:**
- **The slug names the topic, not the individual decision, and is deliberately not unique.** Every decision about the same topic carries the same slug, so a topic's decisions group together in the directory listing and in the index. This is what makes the accumulated history of one topic readable.
- **`<date>` is the date the decision was accepted, in ISO `YYYY-MM-DD`.** It never changes, because the record never changes.
- The reference form is `ADR-<slug>-<date>`.
- ISO dates sort lexicographically, so plain filename order puts a topic's records in chronological order. **No sequence number is needed, and none is used** — here as everywhere else in this standard.
- **One topic, one date, one record.** Two decisions on the same topic on the same day are either one decision, or evidence the topic is drawn too coarsely. Resolve that; do not disambiguate with a suffix.
- **The date is content, not metadata.** `SKILL.md` bans dates in documents, and that ban covers authorship and change tracking. A decision date is when the decision took effect and is what distinguishes one record from the next on the same topic.

Template: `templates/adr-record.md`.

## 4. Sections

Fixed order. Only the last is optional.

### Status

One of:
- **`Accepted`** — in force.
- **`Superseded by ADR-<slug>-<date>`** — replaced by a named record.
- **`Deprecated`** — no longer applies, with nothing replacing it.

**There is no `Proposed`.** An undecided decision is not a conclusion and does not belong in any document; a proposal under review lives in the issue tracker until it is decided.

**The supersession pair is bidirectional, and it is the one exception to the one-way reference rule.** The new record states what it supersedes; the old record's status states what superseded it. It earns the exception because the failure it prevents is worse than the rot it risks: a reader who finds a superseded record and acts on it, not knowing it was replaced.

**A newer record on the same topic does not automatically supersede an older one.** Supersession is stated explicitly in both records, or it did not happen. A topic normally accumulates decisions that all still hold.

### Context

The forces that made a decision necessary: the constraints in play, what was being blocked, what had to be true.

Facts and constraints, not narrative. **DO NOT** write how the team came to look at the problem.

### Options Considered

Each option, and what adopting it would have meant.

- **Every rejected option states why it was rejected.** An option listed with no rejection reason is decoration — it makes the record look thorough while carrying no information, and it is the most common way this section degrades.
- Options are described as they stood at the time of the decision. A better option discovered later belongs in a new record, not in this one.

### Decision

The decision, stated impersonally: `Adopt X.`

**DO NOT** write `We decided to adopt X`. This standard bans first person in every document type, and an ADR is not exempt despite the conventional wording of the form elsewhere.

### Consequences

What the project is now committed to.

**Negative consequences are mandatory.** Every architectural decision buys something at a price, and the price is the part a future reader needs. A record listing only benefits is a sales pitch, not a decision record — and it is unusable for judging, later, whether the trade still holds.

### Invalidation Conditions

**Optional.** What would show this decision to have been wrong: the measurement that would contradict it, the scale at which it stops holding, the assumption whose failure undoes it.

Worth writing when the decision rests on an assumption that can actually be checked later.

## 5. Division of Labour with the Design Document

- **The ADR carries the decision and its consequences. It does not carry the design that follows from the decision** — that belongs to the design document.
- **The design document references the ADR and does not restate it.**

Both halves matter. Describing the same thing in both places guarantees the two descriptions drift, and then neither can be trusted.

## 6. `README.md`

A brief introduction, then the index **grouped by topic**, each record listed with its date and status.

**Required:**
- **The index covers every record, each appearing exactly once.** A record absent from the index is an orphan, and an orphan is an error.
- The record is authoritative for its own status; the index rolls it up. Grouping by topic is what makes the index worth having: a reader sees a topic's decisions in order and which of them are still in force.

Template: `templates/adr-readme.md`.

## 7. What Never Appears in a Record

- Meeting minutes, discussion narrative, who argued for what
- The design detail that follows from the decision (that is the design document's)
- Proposals that have not been decided
- TODO lists, schedules, owners
- Edits to a frozen record's context, options, decision or consequences

## 8. Checklist

The common checklist in `SKILL.md` applies as well.

- [ ] The decision is architecturally significant; a same-day-reversible choice got no record
- [ ] One decision in the record
- [ ] File name is `adr-<slug>-<date>` with an ISO date; the slug names the topic and matches the topic's other records
- [ ] Status is `Accepted`, `Superseded by ...` or `Deprecated` — never `Proposed`
- [ ] If it supersedes a record, both records state the relationship
- [ ] Every rejected option states why it was rejected
- [ ] The decision is stated impersonally: `Adopt X`
- [ ] Consequences include the costs accepted, not only the benefits
- [ ] No design detail that belongs to the design document
- [ ] No edit to a frozen record beyond its status, typos and broken links
- [ ] The record appears in the README index exactly once, under its topic
