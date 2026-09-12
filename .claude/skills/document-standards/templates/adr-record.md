<!--
Template for one architecture decision record, adr-<slug>-<date>.md. Delete
every comment block before committing.

The slug names the TOPIC, not this decision -- every record about the same
topic carries the same slug, so the topic's decisions group together. The date
is when the decision was accepted, ISO YYYY-MM-DD, and it never changes.

Once accepted this record is frozen. Change the decision by writing a new
record that supersedes this one. Only the status field, typos and broken links
may be edited afterwards.

One decision per record. Write it impersonally -- "Adopt X", never "We decided
to adopt X".
-->

# <Decision title>

## Status

Accepted

<!--
Or one of:
  Superseded by [ADR-<slug>-<date>](adr-<slug>-<date>.md)
  Deprecated

Never "Proposed" -- an undecided decision belongs in the issue tracker, not
here. When this record supersedes another, state it here AND update that
record's status to point back to this one. That pair is the only bidirectional
link this standard allows.
-->

## Context

<The forces that made a decision necessary: the constraints in play, what was
blocked, what had to be true. Facts, not narrative -- do not recount how the
problem came to attention.>

## Options Considered

<!--
Every rejected option states why it was rejected. An option listed with no
rejection reason is decoration: it makes the record look thorough while
carrying nothing a future reader can use.

Describe options as they stood at the time. A better option found later
belongs in a new record.
-->

### <Option A>

<What adopting it would have meant.>

**Rejected**: <why>

### <Option B>

<What adopting it would have meant.>

**Rejected**: <why>

### <Option C>

<What adopting it means.>

**Adopted** — see Decision.

## Decision

Adopt <X>.

## Consequences

<What the project is now committed to.>

<!--
Costs are mandatory. Every architectural decision buys something at a price,
and the price is the part a future reader needs in order to judge whether the
trade still holds. A record listing only benefits is unusable for that.
-->

**Costs accepted:**

- <what this gives up, and who will encounter it>

<!--
Optional. Delete this section when the decision rests on no assumption that
can be checked later.
-->
## Invalidation Conditions

<What would show this decision to have been wrong: the measurement that would
contradict it, the scale at which it stops holding, the assumption whose
failure undoes it.>
