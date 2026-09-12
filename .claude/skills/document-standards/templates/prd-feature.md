<!--
Template for a capability requirements document, features/prd-<slug>.md.
Delete every comment block before committing.

Functional Requirements is the ONLY mandatory section. Delete every other
section that has no content -- never leave an empty heading. The sections
that do appear keep the order below.

Requirements state the problem, not the solution: no schema, no API shape, no
class names, no technology choices. Commercial facts enter only as verifiable
constraints, never as arguments.

Flows do not belong here. A business process spanning several requirements is
a journey and lives in journeys/.

The slug is lowercase kebab-case, names the capability, and never changes --
moving a requirement to another document changes its id and breaks every
reference to it.
-->

# <Capability name>

<!-- Optional. -->
## Problem Statement

<What has to be solved, and for whom.>

<!-- Optional. -->
## Scope

### Out of Scope

<!-- Optional. -->
## Actors

<Who interacts with this capability, and with what authority. System
participants, not marketing personas.>

## Functional Requirements

<!--
Mandatory section.

One requirement, one id, one testable assertion. Split anything compound.
Use MUST / SHOULD / MAY, or the document language's established equivalents.
Never "as far as possible", "preferably", "consider supporting".

NNN is unique within THIS document, starting at 001. A retired number is
never reused.

Status is Implemented or Not implemented. There is no "Partially
implemented": a requirement that is partly done is too coarse -- split it.
End-to-end test status is a separate axis from implementation status.
-->

### REQ-<slug>-001

The system MUST <single testable assertion>.

- **Acceptance**: <how passing and failing are told apart, unambiguously>
- **Status**: Not implemented
- **E2E test**: Not implemented

### REQ-<slug>-002

The system MUST <single testable assertion>.

- **Acceptance**: <how passing and failing are told apart, unambiguously>
- **Status**: Implemented
- **E2E test**: `path/to/spec.ts` `test name`

<!--
Optional.

Every number here comes from the user or from a citable external authority --
a regulation, a contract, an interface agreement with a counterpart system.
Never pick a plausible-looking value. A dimension the user has not raised is
omitted entirely, not filled with a default. When a number is needed and has
not been supplied, ask the user.
-->
## Non-Functional Requirements

### REQ-<slug>-010

<Quantified, sourced statement.>

- **Acceptance**: <how it is measured>
- **Status**: Not implemented
- **E2E test**: Not implemented

<!-- Optional: conceptual data only -- what exists, how long it is kept, how sensitive it is. Not a schema. -->
## Data Requirements

<!-- Optional: which external systems and actors are involved. Not an interface design. -->
## External Interface Requirements

<!-- Optional: regulatory, business and external technical constraints imposed from outside. -->
## Constraints

<!-- Optional. -->
## Assumptions and Dependencies
