# Architecture decision records

Each record captures one decision: the context, what we chose, and what follows from it. Records are numbered and never rewritten. When a later decision replaces an earlier one, the old record is marked superseded and both stay.

The records are the numbered files in this folder, in order, and each one's Status line says whether a later record changed it. `make adrs` lists them with their titles and statuses. There's no table of them here to keep: every pull request that added a record added a row to it, and two pull requests that each added one conflicted there ([ADR 0051](0051-files-that-merge-without-conflicts.md)).

Still open from the PRD: the config-as-code format (extended Compose is recommended; needed in phase 3), and the [open questions](../product/prd.md#risks-open-questions-and-decisions).

## Writing one

Copy this into `NNNN-short-title.md` with the next number.

```markdown
# NNNN. Title in sentence case

- Status: Proposed | Accepted | Superseded by NNNN
- Date: YYYY-MM-DD

## Context

The forces at play, and what the PRD or earlier records say.

## Decision

What we chose, stated plainly.

## Consequences

What becomes easier, what becomes harder, and what we now have to do.
```
