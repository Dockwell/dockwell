# 0005. Caddy as the default ingress

- Status: Accepted
- Date: 2026-11-03

## Context

Every server that takes public traffic needs a proxy that terminates TLS, routes domains and paths to services, and reloads without dropping connections. The PRD recommended Caddy by default, with OpenResty available per server for PHP-heavy hosts and 1Panel parity. In a fleet, the control plane issues each certificate once and pushes it to the ingress servers.

## Decision

- Caddy is the default ingress. It runs as its own process, as a systemd unit or a container. The agent manages it through Caddy's admin API with JSON config, and does not embed it.
- On a single server, Caddy issues certificates itself over ACME. In a fleet, the control plane issues them and the agent loads them into Caddy.
- The ingress config is a resource kind in the revision store, so every routing change has a plan and an undo.
- PHP websites run as Caddy plus a `php-fpm` container per PHP version.
- OpenResty per server comes later (phase 2 or after), as a second ingress driver behind the same interface.

## Consequences

- Caddy's memory sits outside the agent's 40 MB budget, and Caddy can restart or update without restarting the agent.
- Automatic HTTPS, HTTP/3 and graceful reloads come for free.
- The website raw-config editor (the `Website-Detail` mock) edits a Caddy site block until OpenResty arrives. Importing 1Panel's Nginx configs verbatim needs OpenResty, or a converter. Decide when the 1Panel importer is designed in phase 2.
