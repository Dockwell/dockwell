# Architecture decision records

Each record captures one decision: the context, what we chose, and what follows from it. Records are numbered and never rewritten. When a later decision replaces an earlier one, the old record is marked superseded and both stay.

| # | Decision | Status |
| --- | --- | --- |
| [0001](0001-license-agpl-and-cla.md) | AGPL-3.0 core, contributions under a CLA | Accepted |
| [0002](0002-go.md) | Go for the agent and the control plane | Accepted |
| [0003](0003-one-schema-protobuf-connect.md) | One API schema in Protobuf, served with Connect | Accepted |
| [0004](0004-sqlite-then-postgres.md) | SQLite by default, Postgres for high availability | Accepted |
| [0005](0005-caddy-ingress.md) | Caddy as the default ingress | Accepted |
| [0006](0006-web-react-typescript.md) | React and TypeScript for the panel, on the Dockwell design system | Accepted |
| [0007](0007-one-binary-two-roles.md) | One binary, two roles; agents dial out | Accepted |

Still open from the PRD: the config-as-code format (extended Compose is recommended; needed in phase 3), and the [open questions](../product/prd.md#risks-open-questions-and-decisions).

## Writing one

Copy this into `NNNN-short-title.md` with the next number, and add it to the table above.

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
