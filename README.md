<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="design/system/brand/dockwell-logotype-dark.svg">
    <img src="design/system/brand/dockwell-logotype.svg" alt="Dockwell" height="36">
  </picture>
</p>

<p align="center">An open-source control plane for your servers, from one VPS to a fleet of thousands.<br>Websites, apps, databases and deploys in one panel.</p>

> **Status: Foundations.** Nothing is ready to install yet. This repository holds the product requirements, the design system and the plan. The agent and control plane are being built here in the open; the [milestones](https://github.com/Dockwell/dockwell/milestones) show where things stand.

## Why Dockwell

Running your own servers today means stitching together two half-products. Server panels give one box a friendly home but stop at the box. Deployment platforms turn a push into a running container but treat the host as plumbing. Dockwell is both, on one data model, and one install grows from a single server to a fleet without a migration.

- **A real server panel.** Websites, PHP and Node runtimes, one-click apps, databases, files, a terminal, firewall and cron.
- **Real deploys.** Push to deploy from GitHub, GitLab, Gitea or any Git host, with zero-downtime rollouts, automatic rollback and a preview for every pull request.
- **Free multi-server.** Add a server with one command. Multiple servers, backups, monitoring and alerts are free, for good.
- **Plan, apply, undo.** Every change, from a deploy to a firewall rule, shows its plan before it runs and rolls back in one step.
- **One model, every surface.** The web panel, API, CLI, MCP server and config files are views of the same objects, so anything you can do in one you can do in all.
- **Works from your phone.** Every screen ships with its phone layout, because servers break when you're away from your desk.

The bar for the first release: a live HTTPS URL within 90 seconds of install.

## How it works

![Agents keep serving even when the control plane is away](docs/product/architecture.svg)

Every server runs one small `dockwell` binary. The first install also runs the control plane: the panel, API, revision store and scheduler. Servers you add later run only the agent, which dials out over mTLS, so they need no open ports and work behind NAT. Agents keep their own desired state, so if the control plane goes away, your apps keep serving.

Underneath it is plain Docker, Compose, Caddy and systemd. Uninstalling Dockwell leaves your workloads running.

## Free and paid

Operating your own machines, any number of them, is free and open source. Paid editions cover what organizations need to govern many people and many servers: SAML and SCIM, custom roles and policy, approvals, long audit retention, a highly available control plane and support. A hosted control plane is also offered. The rule is to paywall the organization, never the machine.

## Roadmap

![Each phase ships something people can use and ends at a gate](docs/product/roadmap.svg)

## In this repository

| Path | What it holds |
| --- | --- |
| [`docs/product/prd.md`](docs/product/prd.md) | Product requirements: users, principles, architecture, every requirement by phase, the signature flows and the targets |
| [`docs/adr/`](docs/adr/) | Architecture decision records |
| [`docs/releasing.md`](docs/releasing.md) | How releases are built, signed and verified |
| [`design/`](design/README.md) | The design system (tokens, components, logo, fonts) and every screen mock, desktop and phone |

## Contributing

Dockwell is at the very start, so the most useful contributions right now are issues and discussion: about the requirements, the design, and what you'd need to move your servers over. See [CONTRIBUTING.md](CONTRIBUTING.md). Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md).

To report a security problem, follow [SECURITY.md](SECURITY.md) rather than opening a public issue.

## License

Dockwell is licensed under the [GNU Affero General Public License v3.0](LICENSE).
