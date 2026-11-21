# 0107. Docker inventory and changes on a server

- Status: Accepted
- Date: 2026-11-20

## Context

Server Overview lists containers Dockwell did not make, while Apps shows the services it owns. To diagnose a machine, a person needs to see what Docker runs and keeps on it in one place. The view must preserve the ownership boundary in [ADR 0031](0031-containers-dockwell-didnt-make.md) and the plan and revision model in [ADR 0009](0009-resources-revisions-and-plans.md).

## Decision

- A server's Docker workspace shows what the selected Engine runs and keeps. The same workspace can inspect several servers. Each row has a server and its Engine identity. In a fleet, counts say which servers could answer, and a plan names the servers it touches.
- Machine inventory is an install-admin read. The agent projects typed facts before sending them. Environment, commands, arbitrary labels, driver options and health check output stay on the server. Compose project and service names are identity fields. Ownership combines those names with the control plane's records and the agent's adopted IDs. Project-scoped Apps, Compose and Volume reads retain their roles.
- Detail uses an immutable Engine ID. Existing container URLs and the legacy name-following logs keep their behavior. A missing ID can point to the container now using its name. Terminal exact-ID exec keeps the same session, host-access, passkey and audit rules as a container shell.
- Each connected agent reports capabilities. Only an open view subscribes to inventory changes and requests container samples. Reads, cursors, messages and subscriptions are bounded. When a view loses its stream it obtains a new snapshot. Logs and terminal traffic keep their existing separate streams.
- Every managed change uses its current resource and a reviewed revision plan. A configuration or lifecycle change to an external container begins with take-on. Start and stop express managed intent. Restart retains its existing transient change.
- The exception to spec revisions covers only removal of local images, build cache records and Docker networks for which Dockwell holds no desired state. A reviewed plan freezes exact identities and servers and rechecks identities and dependencies at apply. Changes in either require review again. An install admin applies the plan with a passkey. Record per-object receipts in Activity. Daemon-wide prune and force delete are prohibited. Verify an ambiguous result before resuming. All volume removals, named or anonymous, go through take-on and a revision.
- Image pull, tag, load, build, export and push use reviewed plans and receipts. These actions leave managed state untouched and sit outside the exception to principle 2. Only an install admin may apply them. Any step that deletes an artifact requires a passkey.
- Daemon settings that Dockwell takes on are revisioned field by field. A supported daemon manager validates and atomically applies the approved keys while preserving other keys. A local watchdog keeps the previous configuration available through a disruptive restart. Verify effective settings and Engine health before reporting success.

## Numbers

The agent's Docker topic uses tags 400 to 419. Inventory list, detail, subscription and notifications use 400 to 403. Hello carries the capability at 400. The terminal call and public terminal request use 400 for the Engine ID and 401 for arguments. The new public Docker messages start their fields at 1.

## Consequences

Docker, Apps and Projects share links and the same plans. An older agent can still serve its existing pages and actions, and Docker inventory reports that the agent needs an update. The agent's idle budget stays at 40 MB. Cleanup and daemon writes need real Engine checks, including interrupted apply and recovery, before their controls ship.
