# Dockwell — product requirements

> This is the source of truth for the PRD; edit it here. It was imported on 3 Nov 2026 from the original Claude Doc (revision 62). Price points and the revenue target are left out of this public copy on purpose. Decisions taken since are recorded in [`docs/adr/`](../adr/).

Nov 2, 2026 · @Ziyang Zeng

## TL;DR

Dockwell is an open-source control plane that runs a single VPS as well as 1Panel does and ships code across a fleet as well as Coolify or Dokploy do — in one product, on one data model.

- **What it is:** a small agent on every machine plus a control plane you self-host or we host. Websites, runtimes, one-click apps, databases, Git-driven deploys, previews and fleet operations share one UI, API, CLI and MCP surface.
- **Who it's for:** owners of 1–5 VMs, small and mid-size teams running 5–100 nodes, and platform teams running hundreds to thousands.
- **The bet:** "server panel" and "deployment PaaS" are one job split across two product categories. Put both under one model — every resource is a versioned service placed on nodes — and one install grows from a single box to a fleet without a migration.
- **The wedge:** multi-node, backups, monitoring and alerts are free forever. We charge for the organization layer (SAML/SCIM, policy, audit retention, HA control plane, support) and for a hosted control plane. Rule: paywall the organization, never the machine.
- **The bar:** a live HTTPS URL within 90 seconds of install, an interface that becomes the reference for the category, and every feature usable from a phone.

Companion: the [Dockwell design system](../../design/system/README.md) defines the tokens and components, and the [screen mocks](../../design/README.md#screens) show the screens, that this PRD's experience requirements refer to.

## Problem and opportunity

Running your own servers today means stitching together two half-products, and the seam between them is where time, money and outages go.

**Server panels stop at the box.** 1Panel gives a single VPS a friendly home: app store, websites, PHP and Node runtimes, files, databases, firewall, cron. But operating a second machine from the same console is Pro-only, as are website backups and email/IM alerts, and there is no Git-to-production pipeline.

**Deployment PaaSes stop at the app.** Coolify and Dokploy turn a push into a running container with TLS. They treat the host as plumbing: host-level administration (native websites, firewall, files, OS updates) is thin or absent.

**Nobody scales cleanly from 1 to 1,000.** Coolify's extra servers need an external load balancer, cannot carry Compose apps or persistent volumes, and do not get PR previews; its docs now mark Docker Swarm deprecated. Dokploy scales only through Swarm. Larger teams jump to Kubernetes and lose the panel entirely.

**The interface has not kept up.** Most of the category looks like a stock admin template: dense tables, stacked modals, page reloads, no preview of what a change will do. Keyboard-first navigation, live state and safe undo are rare.

The opportunity is one product that is the best single-server panel, the best self-hosted PaaS, and a fleet manager that stays simple at scale — with design quality treated as a feature, not a coat of paint.

## Landscape

No product owns the whole arc: panels own the box, PaaSes own the push, and fleet tools own the plumbing — and each one omits or paywalls what the others do well.

| Product | Shape | Multi-node | Git → deploy | Host admin depth | What's paid | Where it stops |
| --- | --- | --- | --- | --- | --- | --- |
| [1Panel](https://1panel.pro/pro-edition) | Single-server panel, GPLv3 | Pro-only unified console | No | Deep: websites, PHP/Node runtimes, OpenResty, files, DBs, firewall, cron, 185+ apps | Pro $80/yr or $200 perpetual (V2): multi-node, website backup, email/IM alerts, advanced WAF, mobile apps, branding | No CI/CD; one license binds to one device |
| [Coolify](https://coolify.io/docs/core/infrastructure/scaling/multi-server-deployments) | Self-hosted PaaS, agentless over SSH, Apache 2.0 | Same app copied to standalone servers via a registry | Yes; 280+ one-click services | Thin | Nothing self-hosted; optional hosted control plane | Extra servers: no Compose, no volumes, no previews, bring your own load balancer; Swarm marked deprecated; v5 targets full scaling |
| [Dokploy](https://laoujin.github.io/Atlas/research/2026-06-08-self-hosted-paas-for-homelab-pr-previews-decision-framework-2026/dokploy/) | Self-hosted PaaS on Swarm + Traefik | Docker Swarm | Yes; native PR previews | Thin | Proprietary enterprise tier (SSO, white-label, HA, advanced monitoring); cloud from $4.50/mo | Swarm-only scaling; \~630 MB idle since v0.27.1 |
| [Komodo](https://komo.do/) | Core + Periphery agent, Rust, GPL-3.0 | Many servers and swarms | Builds from Git, webhook redeploys | Ops view: stats, shells, stacks | Free | No websites, runtimes or app catalog |
| [Portainer](https://www.portainer.io/) | Container and Kubernetes UI | Yes | GitOps for stacks; no source builds | Containers only | Business Edition free to 3 nodes, then from $1,045/yr | Neither a PaaS nor a server panel |
| [Easypanel](https://easypanel.io/pricing) | Docker panel with a PaaS feel | Clustering on higher tiers | Yes | Thin | Free for 3 projects, then per server | Project caps; per-server licensing |
| [Uncloud](https://uncloud.run/) | CLI over a WireGuard mesh, no control plane, Apache 2.0 | Peer-to-peer cluster | Compose-based deploys | None | Free | No UI; deliberately no auto-rescheduling |

Three lessons carry into the requirements:

- **MCP, CLI and API generated from one schema are now table stakes** — 1Panel, Coolify (v4.1) and Dokploy (v0.29) all ship them. Our edge is plan-first safety and audit, not the existence of an MCP server.
- **Swarm is the wrong foundation to bet on.** Coolify has deprecated it and Uncloud markets itself on not needing it. A Docker-native scheduler over a WireGuard mesh gives Swarm's benefits without its quorum and networking pain.
- **Free multi-node is the clearest wedge against 1Panel**, and a real panel is the clearest wedge against Coolify and Dokploy.

## Users and scale tiers

Dockwell is designed for the solo operator first and grows into team and fleet needs by revealing capability as it is needed — never by cluttering the one-server experience.

| Tier | Typical fleet | Who | Top jobs to be done | What makes them switch |
| --- | --- | --- | --- | --- |
| Solo | 1–5 VMs, often across providers | Indie developers, self-hosters, freelancers, homelabs | Install an app in a click; host a site with TLS; deploy from Git; back up; get paged when something dies | 1Panel's paywall on the second server; running 1Panel and Coolify side by side on one box |
| Team | 5–100 nodes, 2–4 environments | Startups, agencies, internal-tools teams with no full-time platform engineer | A preview per PR; staging → prod promotion; client and project isolation; shared access; uptime alerts | Swarm pain, load-balancer glue, per-server licensing |
| Fleet | 100–1,000+ nodes, regions plus on-prem | Platform and SRE teams, managed service providers | Policy and approvals; SSO; audit; staged fleet rollouts; air-gapped installs | Kubernetes is too heavy for the VM estate, and those hosts still need a panel |

**Primary persona — the operator of one.** Builds products on a few rented VMs, is fluent in Docker but resents YAML archaeology, and wants to see the state of everything in one glance from a laptop or phone.

**Secondary persona — the accidental platform engineer.** The one person at a 20–80 person company who owns "the servers". Needs teammates to ship safely without root, and needs to answer "what changed?" in seconds during an incident.

**Tertiary persona — the fleet owner.** Runs hundreds of VMs that will never move to Kubernetes. Needs guarantees: who can do what, what ran where, and the ability to roll a change out in waves and stop it.

## Product principles

Every feature is judged against these nine rules; a feature that breaks one needs an explicit exception in this doc.

1. **One model, every surface.** A website, an app-store install, a Git-deployed service and a cron job are the same kind of object. The UI, API, CLI, Git config and MCP server are views of that one model — anything doable in one is doable in all.
2. **Every change is a revision.** Clicks, CLI calls and pushes all produce a versioned revision with a diff. Changes are previewed (plan), applied, and reversible in one action — including Nginx configs, firewall rules and env vars. A restart is one exception: it changes no spec, so it is a change with a plan and no revision, and there is nothing to undo ([ADR 0027](../adr/0027-health-checks-rollouts-and-rollback.md)). Setting a server's public address is the other: it changes nothing on a server, only what Dockwell says of it, so it is a setting on the activity log, which keeps the address it replaced so it can be put back ([ADR 0034](../adr/0034-a-servers-public-address.md)).
3. **Paywall the organization, never the machine.** Operating your own servers — any number of them — is free: multi-node, backups, monitoring, alerts, previews, basic roles.
4. **Start as a panel, grow into a platform.** A single-server user never sees clusters, pools or policies until they add a second node or a teammate. No migration between tiers.
5. **The control plane is optional at runtime.** If it goes down, nodes keep serving and reconcile to their last known desired state. It is never in the request path.
6. **No lock-in, ever.** Underneath it is plain Docker, Compose, Caddy/OpenResty and systemd. Uninstalling leaves workloads running, and any service can be exported as Compose.
7. **Fast is a feature.** Live state is streamed, not polled; interactions respond in under 100 ms; nothing reloads the page.
8. **Calm by default, deep on demand.** The first screen answers "is everything OK?" Density, raw config and terminals are one keystroke away, never in the way.
9. **Every feature works from a phone.** Nothing is desktop-only; each screen ships with its 360 px layout, and approvals and incident response are designed for one hand.

## Core model and architecture

Dockwell is a control plane plus one small agent per machine; agents dial out, hold their own desired state, and keep serving traffic if the control plane disappears.

![Agents keep serving even when the control plane is away: surfaces and Git/CI feed the control plane (API and auth, revision store, scheduler, builds and events); agents on each node connect out over mTLS and talk to each other over a WireGuard mesh.](architecture.svg)

Every surface writes to the same revision store; the scheduler places revisions on nodes; agents reconcile locally and talk to each other over the mesh.

### Components

- **Agent** — one static binary under systemd: the agent build of `dockwell`, which links none of the control plane. Runs Docker and Compose workloads, ingress, systemd units, files, firewall, metrics and logs. Connects out over gRPC with mTLS, caches desired state, reconciles locally, and self-updates from signed releases.
- **Control plane** — the API (one schema generating REST, SDKs, CLI and MCP), the revision store, the scheduler, build orchestration, the event timeline and the web UI. Runs on any node or as Dockwell Cloud.
- **Mesh** — a WireGuard overlay with built-in DNS service discovery and NAT traversal. Invisible on a single box; switched on when a second node joins.
- **Builders** — any node can build. Images move node to node layer by layer, so an external registry is optional.

### Object model

| Object | What it is |
| --- | --- |
| Organization | Identity, billing and policy boundary |
| Project | A product or client; scopes members, secrets and domains |
| Environment | dev, staging, prod and so on inside a project, each with its own config |
| Service | Anything that runs: App, Compose stack, Website, Database, Catalog app or Job |
| Revision | Immutable snapshot of a service's spec; every change creates one |
| Deployment | A revision placed on nodes, with health and rollout state |
| Node, Pool | A machine running the agent; a label-defined group of nodes |
| Domain, Certificate, Secret, Volume, Backup policy | Resources that services reference rather than copy |
| Connection | An external service (Supabase, R2, Stripe) with named fields and per-environment values; apps bind to it through a variable mapping |
| Route, Ingress | Domain + path to a service in an environment; the servers that accept public traffic and forward it over the private network |

### Scaling modes

| Mode | Nodes | Control plane | State store |
| --- | --- | --- | --- |
| Single box | 1 | Same machine as the agent | SQLite |
| Fleet | 2–250 | One instance on any node, or Cloud | SQLite or Postgres |
| High availability | 250–2,000 | Three replicas behind a load balancer | Postgres |
| Federated | 2,000+ or multi-region | One per region under one organization view | Postgres per region |

### Control plane and agents

One control plane, headless agents everywhere: the first install runs both roles on the user's only server, and every server added later runs just the agent, with no web UI and no open ports.

- **Two roles, two builds.** Every machine runs `dockwell`. The first install runs the full build, with `control` (UI, API, state, scheduler) and `agent` (workloads). `dw join` on any later server installs the agent build, which has only the agent: under 40 MB, outbound mTLS, so it works behind NAT and on home networks. Both builds come from one codebase and every release ([ADR 0023](../adr/0023-a-full-build-and-an-agent-build.md)).
- **The first server becomes the control server.** It still runs apps like any other server; it also hosts the panel. Nothing is replicated between peers, and no server is a lesser copy of another.
- **Headless is not less manageable.** Every server's page (files, terminal, firewall, processes, packages) works through its agent and looks identical whether or not that server hosts the control plane. One URL serves the whole fleet.
- **Agents hold their own desired state.** If the control plane is down, servers keep serving and keep reconciling. `dockwell local` on any server shows status and logs and can restart or roll back to a cached revision; those changes report back as drift when the connection returns.

| Model | Where the UI lives | How servers connect | Where it breaks |
| --- | --- | --- | --- |
| 1Panel Pro | A full panel on every server, plus a master console | Master reaches each server's agent on an inbound port (9999 by default) and syncs config down | Two places to edit the same thing; every server exposes an admin login; N upgrades and licenses |
| Coolify | One control plane | No agent; SSH into each server with a root key | Control plane holds root keys to everything; SSH must be open; no local reconciliation |
| Dockwell | One control plane | Every server runs the agent and dials out over mTLS | Losing the control server, covered below |

**Failure and recovery.** Losing the control server takes the panel and API offline; workloads elsewhere keep running. The control-plane state (SQLite plus config) is backed up off-site automatically and copied, encrypted, to two other agents. `dockwell control restore` promotes any agent; with no backup, it rebuilds state from what the agents report. Teams that cannot accept the gap run three replicas (Organization) or use Cloud.

**Moving and merging.**

- *Move control plane* is an ordinary plan: pick a small dedicated server or Cloud, apply, and the UI is briefly offline while workloads are untouched. Dockwell suggests it around five servers or when the control server is busy.
- *Connect to another Dockwell* merges two standalone installs: the second install's apps are adopted by the first control plane, its own panel retires, and it becomes an agent, with no container restarts.

**Words.** The product says *control server* and *server*; *agent* appears in docs and the CLI. Never *master* or *slave*.

## Feature requirements

Launch (P0) makes Dockwell the best single-server panel with real CI/CD across any number of nodes; Team (P1) adds environments, previews, the mesh and fleet actions; Fleet (P2) adds governance for large estates.

| Pillar | Requirement | Priority |
| --- | --- | --- |
| Fleet | One-command node join: outbound-only agent, join token, no inbound ports | P0 · Launch |
| Fleet | Node inventory: health, resources, OS and Docker versions, tags, groups | P0 · Launch |
| Fleet | Provision nodes from Hetzner, DigitalOcean, Vultr, AWS and others with an API token | P1 · Team |
| Fleet | Fleet actions across a tag selector in canary waves: OS patches, Docker and agent upgrades, SSH key rotation | P1 · Team |
| Fleet | Pools: label-based node groups the scheduler places replicas into; drain, cordon and replace a node | P1 · Team |
| Fleet | Federation of several control planes or regions under one organization | P2 · Fleet |
| Deploy | Service kinds: App (Git, image, Dockerfile), Compose stack, Website, Database, Catalog app, Job | P0 · Launch |
| Deploy | Builds from Git via Dockerfile, Railpack/Nixpacks or buildpacks, with layer cache | P0 · Launch |
| Deploy | GitHub, GitLab, Gitea/Forgejo, Bitbucket and generic Git; push-to-deploy webhooks | P0 · Launch |
| Deploy | CI integration: deploy API, GitHub Action and GitLab component, deploy an image CI already built, status reported back to the PR | P0 · Launch |
| Deploy | Zero-downtime rolling deploys with health checks and automatic rollback | P0 · Launch |
| Deploy | Revisions with diffs and one-click rollback for every service and config | P0 · Launch |
| Deploy | Environments (dev, staging, prod) with promotion of the same built image | P1 · Team |
| Deploy | A preview per PR, including Compose stacks, seeded from a DB snapshot, with a TTL | P1 · Team |
| Deploy | Replicas across nodes with built-in load balancing over the mesh | P1 · Team |
| Deploy | Dedicated build nodes; ephemeral cloud builders later | P1 · Team |
| Deploy | Approvals and deploy windows per environment | P2 · Fleet |
| Server | Web terminal with tabs and shared sessions; file manager with code editor | P0 · Launch |
| Server | Websites: static, PHP (per-site version), reverse proxy, redirects; raw config editor with validation | P0 · Launch |
| Server | One-click runtimes: PHP, Node.js, Python, Go, Java | P0 · Launch |
| Server | Scheduled tasks: shell, script, URL, backup | P0 · Launch |
| Server | Firewall rules, fail2ban, SSH hardening | P0 · Launch |
| Server | Importers for 1Panel and plain Compose | P0 · Launch |
| Server | Processes, systemd services, package updates, system logs | P1 · Team |
| Server | Importers for Coolify and Dokploy | P1 · Team |
| Network | Domains and automatic TLS: HTTP-01, DNS-01 via providers, wildcards | P0 · Launch |
| Network | WireGuard mesh with service discovery across nodes, providers and NAT | P1 · Team |
| Network | One domain routed to replicas on many nodes, health-checked, with DNS and load-balancer provider integrations | P1 · Team |
| Network | Ingress pools per region, provider load balancers, floating IPs and geo steering; opt-in built-in nameserver | P2 · Fleet |
| Network | Private services: IP allowlists, basic auth, OIDC proxy in front of any service | P1 · Team |
| Network | Rate limiting and a baseline WAF rule set | P1 · Team |
| Data | Databases: Postgres, MySQL/MariaDB, Redis/Valkey, MongoDB, ClickHouse | P0 · Launch |
| Data | Backups of databases, volumes, websites and node snapshots to S3, R2, B2 or SFTP; schedules, retention, encryption, one-click restore | P0 · Launch |
| Data | Restore drills: scheduled test restores that prove backups work | P1 · Team |
| Data | Move volumes and services between nodes | P1 · Team |
| Data | Point-in-time recovery for Postgres | P2 · Fleet |
| Observe | Live node and service metrics; live, searchable logs per deploy | P0 · Launch |
| Observe | Uptime checks and alerts to email, Slack, Discord, Telegram, ntfy, webhooks | P0 · Launch |
| Observe | One event timeline correlating deploys, config changes and alerts | P0 · Launch |
| Observe | Log and metric drains (OpenTelemetry, Loki, Datadog) | P1 · Team |
| Access | Passkeys and 2FA; owner, admin, developer, viewer roles scoped by project | P0 · Launch |
| Access | Scoped API tokens; audit log | P0 · Launch |
| Access | Encrypted secrets per environment, referenced not copied | P0 · Launch |
| Access | Connections to external services, with templates and per-app variable mapping | P0 · Launch |
| Access | Credentials created per app through integrations (scoped R2 tokens, Supabase branches for previews); rotation as a plan | P1 · Team |
| Access | OIDC sign-in (GitHub, Google, any OIDC provider) | P1 · Team |
| Access | SAML, SCIM, custom roles, policy rules, long audit retention | P2 · Fleet |
| Access | External secret stores (Vault, 1Password, cloud KMS) | P2 · Fleet |
| Automate | API, typed SDKs and CLI generated from one schema | P0 · Launch |
| Automate | Config as code: a dockwell.toml or Compose file round-trips with the UI; GitOps sync | P1 · Team |
| Automate | MCP server with plan-first semantics: agents propose plans, humans approve destructive steps | P1 · Team |
| Automate | Ops assistant that explains failed deploys and proposes fixes as plans | P1 · Team |
| Catalog | Curated catalog of 200+ apps; versioned templates; upgrades take a backup first | P0 · Launch |
| Catalog | Custom catalogs from a Git URL; import Coolify and Dokploy templates and any Compose file | P1 · Team |

## Signature experiences

These eight flows are where Dockwell must be visibly better than anything in the category; each gets a design spike and a usability test before build.

1. **Ninety seconds to a live URL.** One install command, a passkey to claim the instance, then a single "New" sheet that accepts a repo URL, an image, a Compose file or a catalog app. TLS, health checks and a public URL arrive without a settings page.
2. **Add a node with one paste.** "Add node" shows a one-line command with a short-lived join token, or creates the VM through a provider API. The node appears live in the fleet view within seconds, already on the mesh, with its hardware and OS detected.
3. **Plan, apply, undo.** Every change — a deploy, an env var, a firewall rule, an Nginx edit — opens a plan sheet: the diff, the nodes affected, the expected downtime (usually none). Apply streams progress inline; the timeline keeps an Undo for every revision.
4. **Fleet actions in waves.** Select nodes by tag (`env:prod region:eu`), pick an action, choose waves (1 → 10% → rest) and a stop condition. A live wave view shows each node turning green, and halts automatically on the first failure.
5. **Previews for everything.** Any PR — including multi-service Compose apps — gets its own environment, URL and seeded database, posted back to the PR. It sleeps when idle and is destroyed on merge.
6. **Command bar as the front door.** ⌘K reaches any node, service, log, setting or action, across the fleet, with fuzzy search and verbs ("restart api in staging"). Every screen is fully keyboard-operable.
7. **Incident view.** When an alert fires, one screen lines up the last deploys and config changes, the metric that broke, and the matching log lines. The assistant summarizes likely causes and offers a rollback plan for a human to approve.
8. **Move in, don't start over.** Importers read an existing 1Panel, Coolify or Dokploy host (or a folder of Compose files) and produce Dockwell services without touching running containers until the user applies the plan.

## Scaling the experience from one server to a fleet

Every screen is designed for many servers and then collapses for one, so going from one server to fifty adds columns and choices but never changes a layout, a URL or a habit.

1. **A server is a property, not a destination.** Apps, websites, databases, domains and backups are always fleet-wide lists; where each runs is a field. With one server that field is hidden; with several it becomes a column, a filter and a *Runs on* choice. Machine-level things (files, terminal, firewall, processes, packages) live on the server's own page, which exists from day one at `/servers/<id>/…`; at two servers the nav item *Server* becomes *Servers (2)* and the familiar page becomes one of two.
2. **Never show a choice with one option.** The rule below removes multi-server complexity for solo users without special cases, and the CLI and API follow it too (`dw deploy` needs no `--on` until there is a choice).
3. **Layouts stay; representations adapt to count.** Each region of a screen keeps its place while its contents switch representation by cardinality (second table). Logs show a source column only with more than one source; metrics aggregate with a per-server breakdown on hover; the terminal opens directly on one server and asks which server, or runs on a selection, on many.
4. **Every action targets a set; one server is a set of one.** Restart, patch, firewall rule and agent upgrade all take a selector. The plan sheet is the same; waves and a stop condition appear only when the set has more than one member, so the plan-apply-undo habit learned on one server is the fleet skill.
5. **The second server is a designed moment.** *Add server* is always on the server page and in ⌘K, and is suggested only on real signals (disk above 85%, sustained CPU, a request for staging). After the join, a short sheet says what changed: nothing moved, existing apps still run where they were, new apps ask where to run, the private network is on. It offers optional plans: move an app, run an app on both, use the new server for builds or staging. Removing the server reverses it.
6. **Fleet-first users get the same product.** Provider import (20 servers from one token), labels at join, selectors, bulk actions and the fleet grid as home are all there because the collapse rules never fire for them. Power tools are available at any size and required at none.
7. **Stateful things explain themselves.** A database with volumes stays pinned and says why; moving it is an explicit plan with data copy and expected downtime. Auto placement always says why it chose a server ("hz-2: most free memory").

| Dimension | With one | With two or more |
| --- | --- | --- |
| Servers | *Server* in nav; no placement field; no server column | *Servers (n)*; *Runs on: Auto*; server column and filter |
| Projects | Hidden default project | Project in the scope bar |
| Environments | Hidden production | Environment switcher; promote staging to production |
| People | No roles or approvals UI | Access, approvals, actors in the timeline |
| Regions and pools | Hidden | Grouping in the fleet view and in plans |

| Servers | Overview shows |
| --- | --- |
| 1 | That server's health: CPU, memory, disk, load, apps, websites |
| 2–12 | One card per server |
| 13–500 | Fleet grid of tiles |
| 500+ | Tiles grouped by pool or region with counts; drill in |

**Gates and measures.** Every screen spec states its 0, 1, 7, 70 and 7,000-server states before build. Usability targets: users who stay on one server never meet a server choice; going from one server to an app running on two takes under 5 minutes; fleet-first users import 20 servers and run a first fleet action in under 10 minutes. Track the share of solo users who add a second server and keep it for 30 days.

## Mobile

Every feature is operable from a phone browser at 360 px wide, with no reduced companion app, because servers break when people are away from their desks.

| Desktop pattern | On a phone |
| --- | --- |
| Left rail | Bottom bar with Overview, Apps, Servers, Activity and More; scope switcher at the top |
| ⌘K command bar | Search button in the top bar opens the same command list full screen |
| Tables | Stacked rows: name, status and one key value; tap for detail |
| Side sheets (plans, settings) | Full-height sheets with a sticky footer holding the primary action |
| Fleet grid | Same tiles at the small size; the inspect readout pins to the bottom |
| Terminal | Full screen with a key bar (Esc, Tab, Ctrl, arrows, \|, \~), paste and text zoom |
| File editor | Edit with syntax highlighting; a diff before every save |
| Logs | Full-screen stream, Follow toggle, level filter chips |
| Hover readouts | Tap to inspect; nothing is available only on hover |

- **Touch rules.** Hit areas of at least 44 px (controls stay 40 px tall); primary actions in the bottom third for one-handed use; safe-area insets respected; no swipe-only or long-press-only actions.
- **Approvals from a notification.** A push notification opens the plan sheet; approve with a passkey (Face ID, fingerprint) in two taps.
- **Installable.** The panel is a PWA: home-screen icon, push notifications for alerts, finished deploys and approval requests, and the last-known state readable offline behind a clear *Not live* banner.
- **Requirements.** Mobile parity for every screen (P0, a launch gate checked at 360 px and 390 px); push notifications and phone approvals (P1); offline last-known state (P1).

## Connections

Apps reach outside services through Connections: one saved credential set per service, mapped into each app under the variable names that app expects, so one change updates every app that uses it.

A connection is an external service with named fields: a Supabase project has host, port, user, password, pooled URL, anon key and service key; an R2 bucket has endpoint, bucket, access key ID and secret. Apps bind to a connection, and the binding maps fields to the container's own names. One Supabase project and one R2 bucket feed four apps:

| App | Variables the container expects | From |
| --- | --- | --- |
| api | `DATABASE_URL` | `supabase.pooled_url` + `?sslmode=require` |
| storefront-web | `NEXT_PUBLIC_SUPABASE_URL`, `NEXT_PUBLIC_SUPABASE_ANON_KEY` | `supabase.url`, `supabase.anon_key` |
| n8n | `DB_POSTGRESDB_HOST`, `_PORT`, `_USER`, `_PASSWORD` | `supabase.pooled_url`, split into parts |
| images (imgproxy) | `IMGPROXY_S3_ENDPOINT`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | `r2.endpoint`, `r2.access_key_id`, `r2.secret` |

- **Templates.** Supabase, Neon, Cloudflare R2, Amazon S3, Backblaze B2, Stripe, Resend, Postmark, SMTP, OpenAI, Anthropic and Sentry at launch, plus Custom for anything else. A template declares its fields, which ones are secret, and a connection test.
- **Mapping is suggested, not typed.** Catalog apps ship a manifest of the names they expect, so binding is one click. For repos, names come from `.env.example`, Compose files and a source scan, matched to fields. Transforms cover the rest: join parts into a URL, split a URL into parts, append, base64, or write to a file.
- **Injection.** Values reach containers as environment variables at start, as files under `/run/secrets/` for images that read `*_FILE` variables, or rendered into a config template. They are never written into Compose files, image layers or logs. A server receives only the secrets of apps placed on it; the agent decrypts them in memory.
- **Environments.** One connection holds a value set per environment, so production and staging can point at different Supabase projects under the same binding. Previews inherit staging unless the connection can create its own.
- **Credentials Dockwell creates.** With the matching integration connected, Dockwell creates the credential instead of storing a pasted one: an R2 bucket and a token scoped to it for each app, or a Supabase branch per pull-request preview. It revokes them when the app or preview is deleted.
- **Rotation is a plan.** Editing a value opens a plan listing every app and environment that will restart, rolled one replica at a time; the old value stays valid until every replica is healthy. Each connection has a Used by view and expiry reminders.
- **Bind is not read.** Developers can bind a production connection without seeing its values. Revealing a value needs a passkey and lands in the audit log; API tokens cannot read values unless given `connections:read`.
- **Test from the server that runs the app.** The connection test runs through the agent on the chosen server, which catches firewall and IP allow-list problems, and shows the outgoing IPs to add at the provider.
- **Boundary with Integrations.** Integrations let Dockwell act on your accounts (Git, DNS, cloud providers, backup storage); Connections are credentials your apps use. An integration can create connections, which is how per-app R2 tokens work.

On one server, Connections works the same with no environment column, and catalog installs offer Connect next to Create for databases and storage.

## Fleet routing

In a fleet, domains point at ingress servers, never at app servers. Ingress forwards over the private network to healthy replicas wherever they run, so apps move, scale and recover without a DNS change.

Comparable platforms leave this half done. [Vercel](https://vercel.com/kb/guide/a-record-and-caa-with-vercel) gives every domain one anycast IP and routes over its own edge network, which self-hosters cannot copy. [Coolify](https://coolify.io/docs/core/infrastructure/scaling/multi-server-deployments) deploys copies to several servers but states that this "does not distribute traffic by itself"; you add a load balancer. [Dokploy](https://mintlify.com/dokploy/dokploy/infrastructure/docker-swarm) runs Traefik on Swarm managers and leaves DNS to you. Dockwell owns the whole path: records at your DNS provider, ingress, balancing and certificates.

**The path of a request**

1. DNS answers with the healthy ingress servers for the domain, nearest region first where the provider supports it.
2. An ingress server terminates TLS with the certificate the control plane issued.
3. It matches the route (domain + path) to a service in an environment, never to a server.
4. It picks a healthy replica, preferring its own region, and forwards over the private network.
5. If the replica fails mid-request, idempotent requests retry once on another replica.

**Defaults grow with the fleet**

| Fleet | Domains point at | Who balances | Setup |
| --- | --- | --- | --- |
| 1 server | That server's IP | Its proxy, across local replicas | None; no routing UI |
| 2 to about 10 servers | Every healthy server; all are ingress | DNS spreads clients; ingress picks a replica | None; records kept current through the DNS integration |
| Larger or several regions | An ingress pool per region | A provider load balancer or floating IP per pool; geo steering across regions | Pick the pool and entry; Dockwell creates the load balancer through the provider |
| On-prem or private | A floating IP on the ingress servers, or a tunnel | keepalived moves the IP; Cloudflare Tunnel needs no open ports | Pick in Ingress settings |

- **Entry, default.** Dockwell writes A and AAAA records at the provider you already use (Cloudflare, Route 53, Hetzner DNS, DigitalOcean, deSEC, or any RFC 2136 server), with a 60 s TTL. An unhealthy ingress leaves every record within 30 s; clients retry the other addresses.
- **Entry, larger fleets.** Provider load balancers (Hetzner, DigitalOcean, AWS NLB), reserved or floating IPs, and Cloudflare proxying or Load Balancing are created and kept in sync through Integrations.
- **Dockwell as your nameserver.** Possible but off by default. Delegating NS records lets Dockwell answer with healthy ingress by region, but authoritative DNS needs at least two independent networks, and an outage takes every domain down. It ships later as opt-in for on-prem fleets; Dockwell Cloud may offer hosted anycast DNS.
- **Routes.** Domain + path → service. Pinned routes send a domain to named servers, for legacy single-box websites only. TCP and UDP routes by port, TLS passthrough by SNI, and wildcard routes for previews (`*.preview.acme.dev`).
- **Balancing.** Least connections, same region first then spill over, health checks every 2 s, 30 s connection draining during rollouts, optional sticky sessions, WebSockets and HTTP/3.
- **Certificates.** The control plane issues each certificate once (DNS-01 through the integration, or HTTP-01 answered by any ingress), stores it encrypted and pushes it to ingress servers. One renewal per domain, not one per server.
- **Seeing it.** In fleet mode the Domains page adds an Ingress tab and a traffic path per domain: records, ingress servers with their share of requests, and the replicas behind them. Trace a request shows the DNS answer, ingress, route and replica for any URL.

## Packaging and business model

Everything needed to operate your own machines is free and open source; revenue comes from what organizations need to govern many people and many nodes, and from running the control plane for them.

| Edition | For | Includes | Price model |
| --- | --- | --- | --- |
| Community | Everyone, self-hosted | Unlimited nodes, users and projects; every panel and deploy feature; previews; backups; monitoring and alerts; OIDC sign-in; four built-in roles; API, CLI, MCP; 30-day audit log | Free, open source |
| Organization | Teams with compliance or scale needs, self-hosted | Everything in Community, plus SAML and SCIM, custom roles and policy rules, approvals and deploy windows, unlimited audit retention and export, HA control plane, federation, air-gapped bundles, white-label, support SLA | Per node per year, with a minimum |
| Cloud | Anyone who doesn't want to run the control plane | Hosted, HA control plane for your own nodes; managed upgrades; off-site config backups; Organization features as add-ons | First 3 nodes free, then per node per month |

**Why this split holds:** the free tier is strictly more generous than 1Panel OSS (multi-node, website backups and alerts included) and than Portainer's 3-node Business allowance, which makes solo users advocates. Paid features are the ones procurement asks for, so revenue scales with organizations that have budget, not with hobbyists' server counts.

**Reference points:** 1Panel Pro is $80/yr or $200 perpetual per license; Dokploy's hosted control plane starts at $4.50/mo with extra servers at $4.50 each; Portainer Business is free to 3 nodes, then from $1,045/yr.

## Non-functional requirements

The platform must be light enough for a 1 GB VPS and sturdy enough for a 1,000-node estate; these targets are launch gates, not aspirations.

| Area | Requirement | Target |
| --- | --- | --- |
| Footprint | Agent memory at idle | < 40 MB RSS |
| Footprint | Control plane + agent on one node, idle | < 250 MB RSS (Dokploy idles near 630 MB) |
| Footprint | Install to running panel | < 60 s on a 1 vCPU VPS |
| Scale | Nodes per control plane (single instance / HA) | 250 / 2,000 |
| Scale | Services per control plane | 20,000 |
| Performance | UI interaction response | p95 < 100 ms |
| Performance | First load of the app shell | < 1.5 s on a cold cache |
| Performance | Live log and metric latency, node → browser | < 1 s |
| Reliability | Workloads when the control plane is down | Keep running; agents reconcile to last desired state |
| Reliability | Failed deploy | Automatic rollback to last healthy revision |
| Reliability | Control plane availability (HA mode) | 99.95% |
| Security | Agent network posture | Outbound-only mTLS; no inbound management ports |
| Security | Identity | Per-node certificates, rotated automatically; passkeys and TOTP for people |
| Security | Secrets | Envelope-encrypted at rest; never written to logs or plans |
| Security | Supply chain | Signed releases, SBOM, reproducible builds; catalog images pinned by digest |
| Security | Destructive actions | Plan + typed confirmation; recoverable for 7 days where data allows |
| Platforms | Node OS and CPU | Debian, Ubuntu, RHEL family, Alpine; amd64 and arm64 |
| Access | Accessibility | WCAG 2.2 AA; full keyboard operation; reduced-motion support |
| Access | Languages at launch | English and Simplified Chinese; community translations after |
| Access | Devices | Every feature at 360 px; 44 px touch targets; installable PWA with push |

## Roadmap and milestones

The first public release is the Solo beta around month 7: a better 1Panel with real deploys and free multi-node, which earns the users that the Team and Fleet phases are built for.

![Each phase ships something people can use and ends at a gate: 1 Foundations (months 0–3), 2 Solo beta (3–7), 3 Team 1.0 (7–12), 4 Fleet (12+).](roadmap.svg)

A phase ships only when its gate passes; the design system and component library start in Foundations so every later screen is assembled, not invented.

## Success metrics

The north-star metric is **weekly active nodes** — nodes that ran a managed workload and received at least one change or check that week — because it grows only when people trust Dockwell with real machines.

| Metric | Definition | Target 6 months after launch |
| --- | --- | --- |
| Time to first live URL | Install command → first service healthy behind HTTPS, median | < 3 min (design goal 90 s) |
| Activation | Installs that deploy ≥ 1 service within 24 h | ≥ 60% |
| Expansion | Active installs with ≥ 2 nodes | ≥ 35% |
| Retention | Installs still active at day 30 | ≥ 45% |
| Deploy success | Deploys reaching healthy without manual intervention | ≥ 97% |
| Recovery | Alert → rollback applied, median, for teams using rollbacks | < 5 min |
| Migration | Installs created through an importer | ≥ 15% |
| Design quality | Task success in quarterly usability tests on the eight signature flows | ≥ 90%, SUS ≥ 85 |

Telemetry is opt-in, anonymous and documented; every metric above must be computable from that opt-in data or from Cloud.

## Risks, open questions and decisions

The largest risk is scope — this is three products in one — so every mitigation below trades breadth for a sharper first release.

| Risk | Mitigation |
| --- | --- |
| Panel + PaaS + fleet manager is too much to build well | Ship the Solo tier first; reuse Compose, Caddy, OpenResty and WireGuard instead of inventing; win switchers with importers rather than day-one parity |
| Our own scheduler and mesh become a distributed-systems tar pit | Keep the scheduler deliberately small (spread, pin, pool labels; no preemption); follow Uncloud's proven mesh pattern; offer Kubernetes as an optional target later |
| A root-level agent on every node is a high-value target | Outbound-only mTLS, signed agent updates, capability-scoped commands, an external audit before 1.0, a public bug bounty |
| A generous free tier starves revenue | Make Cloud the main revenue line; map Organization features to procurement checklists; track conversion by fleet size |
| Incumbents close the gap (Coolify v5 multi-server; 1Panel freeing multi-node) | Compete on what is hard to retrofit: one object model, plan/undo for everything, and design quality |
| Design ambition slows shipping | The design system is the contract; components and patterns ship before screens |

**Decisions needed** (recommendation first):

- [x] License: AGPL-3.0 core with source-available Organization modules, or Apache 2.0 throughout. Recommend AGPL — keeps self-hosting free and deters hosted resale. **Decided:** AGPL-3.0, with a CLA ([ADR 0001](../adr/0001-license-agpl-and-cla.md)). The API schema and its generated clients are Apache-2.0, so integrations can embed them ([ADR 0012](../adr/0012-apache-for-api-schema-and-clients.md)).
- [x] Agent and control-plane language: Go or Rust. Recommend Go for the Docker SDK, ecosystem and contributor pool. **Decided:** Go ([ADR 0002](../adr/0002-go.md)).
- [x] State store: recommend SQLite for single-node and Postgres for HA, behind one interface. **Decided** ([ADR 0004](../adr/0004-sqlite-then-postgres.md)).
- [x] Ingress: recommend Caddy by default, with OpenResty available per node for PHP-heavy hosts and 1Panel parity. **Decided:** Caddy first; OpenResty later ([ADR 0005](../adr/0005-caddy-ingress.md)).
- [ ] Config-as-code format: a new dockwell.toml, or Compose with `x-dockwell` extensions. Recommend extended Compose for portability.
- [x] Web stack: recommend React + TypeScript on a component library built from the Dockwell design system, with live updates over WebSocket. **Decided:** React + TypeScript; live updates use the schema's server streams, with WebSocket kept for the terminal ([ADR 0006](../adr/0006-web-react-typescript.md), [ADR 0003](../adr/0003-one-schema-protobuf-connect.md)).
- [ ] Name: Dockwell, chosen 3 Nov. dockwell.dev, dockwell.io and the GitHub org were free that day; register them and run a trademark search.
- [x] Topology: one control plane and headless agents everywhere; HA control plane in the Organization tier and Cloud. ([ADR 0007](../adr/0007-one-binary-two-roles.md))
- [x] Terminology: *server* in the UI, CLI, API and docs; *agent* for the binary; never *master* or *slave*.
- [x] Mobile: every feature operable at 360 px is a launch gate.

**Open questions:**

- Can the catalog bootstrap from 1Panel's app store format, and does its license allow that? **Decided:** its licence allows it, on terms we can keep. The catalog has a format of its own, and entries from 1Panel and other catalogs are converted into it when the catalog is built, so the same app shows once ([ADR 0029](../adr/0029-a-catalog-format-fed-from-other-catalogs.md)).
- Are virtual machines in scope? 1Panel's Enterprise edition now manages VMs; a Proxmox/libvirt integration could be a P2 item.
- How do we serve users in mainland China (mirrors, registries, install scripts) from day one? **Decided:** mirrors that anyone can run, named explicitly, with everything from them checked as a download from GitHub is; Dockwell runs no infrastructure in China ([ADR 0013](../adr/0013-mirrors-for-releases-and-images.md)).

## Sources

- [1Panel Pro Edition — pricing and OSS vs Pro comparison](https://1panel.pro/pro-edition)
- [Coolify Docs — Multi-server deployments](https://coolify.io/docs/core/infrastructure/scaling/multi-server-deployments)
- [Atlas — Dokploy: Self-Hosted PaaS for Docker-Native Deployments (June 2026)](https://laoujin.github.io/Atlas/research/2026-06-08-self-hosted-paas-for-homelab-pr-previews-decision-framework-2026/dokploy/)
- [Komodo — Build and deployment system](https://komo.do/)
- [Portainer pricing in 2026 (iTechGuides)](https://www.itechguides.com/portainer-pricing-is-portainer-free-in-2026/)
- [Easypanel — Pricing](https://easypanel.io/pricing)
- [Uncloud — Multi-node Docker Compose for production](https://uncloud.run/)
