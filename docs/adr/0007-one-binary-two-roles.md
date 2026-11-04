# 0007. One binary, two roles; agents dial out

- Status: Partly superseded by [0023](0023-a-full-build-and-an-agent-build.md): every machine runs one of two builds of `dockwell`, not one binary. The roles, and agents that dial out, stand.
- Date: 2026-11-03

## Context

The PRD settles the topology as one control plane, with headless agents everywhere. 1Panel Pro puts a full panel on every server, reached on an inbound port. Coolify holds root SSH keys to every server. Both widen the attack surface and give the same thing two places to be edited.

## Decision

- Every machine runs one binary, `dockwell`. The first install enables two roles: `control` (panel, API, state, scheduler) and `agent` (workloads). `dockwell join` on any later server enables only the agent.
- Agents connect out to the control plane over gRPC with mTLS. They open no inbound management ports, so they work behind NAT. The control plane acts as the certificate authority: a short-lived join token buys a node certificate, which rotates automatically.
- Agents cache their desired state and reconcile locally. If the control plane is unreachable, they keep serving. `dockwell local` shows status and logs, and can restart a service or roll back to a cached revision. The resulting drift is reported when the connection returns.
- Control-plane state is backed up automatically, off-site and encrypted to two other agents. `dockwell control restore` promotes any agent to the control role.
- Words: the product says *control server* and *server*. *Agent* appears in docs and the CLI. Never *master* or *slave*.

## Consequences

- One artifact to build, sign and update, and every server's page works the same way whether or not it hosts the control plane.
- Losing the control server takes the panel and API offline, but nothing else. Proving that is the phase 1 exit gate (chaos tests).
- The agent is the most sensitive code in the project: it runs as root on every server. Commands it accepts are capability-scoped, and an external audit happens before 1.0.
