<!--
Shape of a heartbeat report. Unlike the plan document, this is not a file: it is
what the coder says in the session each tick, so the sections below are a shape
to fill, not a document to write to disk.

Two rules govern the shape before any of the content does:

  - **Write it in the language the user is using.** This file is English because
    the skill is; the report is for a person reading a terminal.
  - **Keep it to one screen.** A report arrives every interval. One that has to
    be scrolled will stop being read, and the tick that finally matters will be
    missed along with the rest.

Every fact here comes from a plan document or from git. Never from an agent's
account of itself: a finished agent is not a merged change.
-->

# Quiet tick

<!--
When nothing moved, this is the whole report -- one line, marked as a no-op so
consecutive quiet ticks collapse instead of scrolling. Do not pad it into a full
report to look busy, and do not invent progress for a workflow that has not
reported.
-->

`<HH:MM>` · <N>/<slots> busy · <M> queued · CI <green> · no change

---

# Full tick

<!--
The header line, always first: the clock, the slots in use against the limit,
the depth of the queue, the state of CI. Busy slots against the limit rather
than a bare count -- "2/4 busy" with a queue behind it is a report that free
capacity is going unused, which is the one thing the header should make obvious.
-->

`<HH:MM>` · <N>/<slots> busy · <M> queued · CI <green | red: what broke>

<!--
Tables, not a block per task: a report arrives every interval, and the vertical
space a heading and four bullets cost per task is what pushes it off the screen.

Every task is referenced by its slug (coder-role.md, section 3) -- the same string that names
its plan document and its workflow. Never by a number or an id: a report the
reader has to decode is a report they stop reading.

Each table appears only when it has rows. Drop the whole section otherwise --
an empty table says nothing and costs two lines to say it.
-->

## Running

<!--
Stage comes from each plan document's Stage line; Since last and Blocked come
from its Work Items table and Progress Log. Mark a task dispatched on this tick
with * -- that, plus the Queued table shrinking, is how dispatch shows up. There
is no separate section for it.
-->

| Task | Stage | Since last | Blocked |
|---|---|---|---|
| <task-slug> | <stage> | <items that reached done> | <item, on what> or -- |
| <task-slug> * | plan | -- | -- |

## Queued

<!--
Accepted but not yet dispatched. Waiting on says why, concretely: no free slot,
an overlapping file set held by a running task, an unanswered question. A queue
entry with no reason reads as a task that was forgotten, which is how tasks
actually do get forgotten.

A row saying "no free slot" while the header shows a free one is a contradiction
the reader will catch before you do -- fill the slot instead (coder-role.md, section 3).
-->

| Task | Waiting on |
|---|---|
| <task-slug> | <no free slot / overlaps <task-slug> / answer to <question>> |

## Completed since last tick

<!--
Landed is read from git, never from the workflow's account of itself. Left out
carries the deliberate omissions from the plan document, and Findings carries
where its findings went -- settled in place, or moved to the task that will
settle them. This is the last tick that mentions either: the plan document is
archived on it, and nothing reads it afterwards.
-->

| Task | Landed | Verified | Left out | Findings |
|---|---|---|---|---|
| <task-slug> | <merge commit> | <what was run> | <omission> or -- | <settled / moved to <task-slug>> |

## Needs you

<!--
Only when the user has to act. A question does not wait for the next tick just
because it arrived between two of them (coder-role.md, section 7) -- it is asked when it
arises, and this table is where it stays visible until it is answered.

Blocks says what stops until the answer comes; "nothing yet" is a real answer
and worth writing, because it tells the user the question can wait. Waiting
since is the tick the question was first asked, not the tick it was last
repeated -- an hour-old question should read as an hour old.
-->

| Question | Blocks | Waiting since |
|---|---|---|
| <the question or decision> | <task or item held up> or nothing yet | <HH:MM> |
