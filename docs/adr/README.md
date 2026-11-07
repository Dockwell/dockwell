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
| [0007](0007-one-binary-two-roles.md) | One binary, two roles; agents dial out | Partly superseded by 0023 |
| [0008](0008-panel-libraries.md) | Libraries and tooling for the panel | Accepted |
| [0009](0009-resources-revisions-and-plans.md) | One resource model, with revisions and plans for every kind | Accepted |
| [0010](0010-install-script-verifies-with-pinned-cosign.md) | The install script verifies releases with a pinned cosign | Accepted |
| [0011](0011-claim-and-sign-in.md) | Claim over a tunnel or a domain, then sign in with passkeys | Accepted; 0024 changes who holds ports 80 and 443 once the server has a route |
| [0012](0012-apache-for-api-schema-and-clients.md) | Apache-2.0 for the API schema and its generated clients | Accepted |
| [0013](0013-mirrors-for-releases-and-images.md) | Configurable mirrors for releases, images and the install script | Accepted |
| [0014](0014-object-model-and-store.md) | The object model, and one store on SQLite and Postgres | Accepted |
| [0015](0015-rest-and-openapi-from-the-schema.md) | REST paths and an OpenAPI document from the schema | Accepted |
| [0016](0016-scoped-api-tokens.md) | Scoped API tokens | Accepted |
| [0017](0017-the-change-api-and-the-agent-stream.md) | The change API, and the engine's calls over the agent stream | Accepted |
| [0018](0018-keep-the-budgets-in-rss.md) | Keep the memory budgets in RSS, measured steadily | Accepted |
| [0019](0019-a-cli-generated-from-the-schema.md) | A CLI generated from the schema, with local access on the control server | Accepted |
| [0020](0020-the-event-timeline.md) | The event timeline | Accepted |
| [0021](0021-the-docker-driver.md) | The Docker driver, with Compose's names and labels | Accepted; 0027 changes how a service is replaced |
| [0022](0022-secrets-encrypted-at-rest.md) | Variables and secrets, encrypted at rest and referenced by services | Accepted |
| [0023](0023-a-full-build-and-an-agent-build.md) | A full build and an agent build, from one codebase | Accepted |
| [0024](0024-the-caddy-ingress-driver.md) | The Caddy ingress driver, on the server's own network | Accepted; 0027 corrects what it says of reloads |
| [0025](0025-logs-per-deploy.md) | Live logs per deploy, on connections of their own, bounded end to end | Accepted |
| [0026](0026-the-app-screens.md) | The app screens, and what they needed of the API | Accepted |
| [0027](0027-health-checks-rollouts-and-rollback.md) | Health checks, rolling replacement and automatic rollback | Accepted |
| [0028](0028-the-plan-sheet.md) | The plan sheet: one shell, what can't be taken back first, and failures said twice | Accepted |
| [0029](0029-a-catalog-format-fed-from-other-catalogs.md) | A catalog format of our own, fed from other catalogs | Accepted |
| [0030](0030-routes-behind-a-tunnel-or-a-proxy.md) | Routes behind a tunnel or a proxy | Accepted |
| [0031](0031-containers-dockwell-didnt-make.md) | The containers Dockwell didn't make | Accepted |
| [0032](0032-files-on-a-server.md) | Files on a server, for a person, outside revisions | Accepted |
| [0033](0033-backups-of-volumes-and-databases.md) | Backups of volumes and databases | Accepted |
| [0034](0034-a-servers-public-address.md) | A server's public address, and what may be claimed of it | Accepted |
| [0035](0035-containers-named-after-their-environment.md) | A service's containers are named after its project and environment | Accepted |
| [0036](0036-deleting-a-project-takes-its-environments.md) | Deleting a project takes its environments | Accepted |
| [0038](0038-websites-as-projects-of-their-own.md) | Websites, as projects of their own | Accepted |

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
