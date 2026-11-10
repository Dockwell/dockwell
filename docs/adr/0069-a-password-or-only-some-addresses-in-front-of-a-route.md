# 0069. A password, or only some addresses, in front of a route

- Status: Proposed
- Date: 2026-11-10

## Context

The PRD's Network section lists "Private services: IP allowlists, basic auth, OIDC proxy in front of any service" as P1 (issue #266 tracks it as missing). Today a route answers anyone who reaches its domain: an admin tool, a staging copy or a dashboard on a domain is public, or isn't on a domain at all. 1Panel offers a password on a website and on paths under it, and allow and block lists of addresses, through its OpenResty.

What there is to build on:
- A route is already one domain, or one path under it, sent to a service's port ([ADR 0024](0024-the-caddy-ingress-driver.md)); Caddy matches a domain's longest paths first.
- A route already knows which client address to believe: the connection's, or what its own `trusted_proxies` say, in `{http.vars.dockwell_client}` ([ADR 0030](0030-routes-behind-a-tunnel-or-a-proxy.md)).
- Every change to a route is a revision with a plan ([ADR 0009](0009-resources-revisions-and-plans.md)), and a field marked `(sensitive)` is sealed at rest and masked in plans ([ADR 0022](0022-secrets-encrypted-at-rest.md)), which also sets the rule that proposing a value must never tell whether it is the one there.
- The panel's own domain isn't a route: the route kind reserves it, and the proxy sends it to the panel's socket on a route of its own ([ADR 0063](0063-the-panels-domain-set-from-the-panel.md)).

## Decision

### It is the route's: a field, so every change is a plan

`Route` gains `access`, a message with three fields: `users`, who may in with a name and a password (HTTP basic auth); `allow`, the addresses and ranges that may reach it; and `deny`, those that may not. Empty, it is what a route is today. Proposing, planning, applying, undo and rollback are a route's, with nothing new in the engine: the agent changes that one route in Caddy, which restarts nothing.

**A path is protected by its own route.** Protecting `/admin` of an app served at the root is a second route, for `/admin`, to the same service and port, with its `access`, made in the same plan; the longer path answers first. Nothing is inherited: each route says its own. A plan that leaves a route of a protected domain open says so ("app.example.com/api answers anyone, though app.example.com asks for a password").

### A password: only its hash is kept, and never shown again

- **A user** is a name (1 to 64 characters, no `:` or space) and a password. A route has at most 20.
- **An edit gives a password, or a bcrypt hash** (`$2a$`, `$2b$` or `$2y$`, as htpasswd and a 1Panel import have them). A password is at least 12 characters. The control plane keeps only a bcrypt hash at cost 10: as an edit is proposed, before anything compares or stores it, the control plane hashes a password that isn't a hash. The route kind's own checks, which the agent runs too, take only a hash. So no revision, change, event, log or server ever holds the password, and bcrypt stays out of the agent build. `password` is `(sensitive)`, so the hash is sealed at rest, masked in plans, and never in a response. There is nothing to reveal, with a passkey or without one.
- **No guess is confirmed.** A password is hashed with a new salt each time, so setting one always plans as a change, whatever it is, as setting a secret does. A hash given as a hash compares as any field: whoever has it gains nothing.
- **What an edit doesn't give, it keeps.** An edit that names a user with no password keeps that user's hash (`resource.Keeper`), so changing a route's port from a client that can't read hashes keeps every password. A user the edit leaves out is gone; a new user with no password is refused.
- **Where it goes:** to the agent of each server the route runs on, as the route's own config, and into that Caddy's config. Nothing else is sent anywhere.
- **In Caddy:** an `authentication` handler with `http_basic`, bcrypt, the domain as the realm, and Caddy's hash cache on, so a password checked once isn't hashed again on every request. The `Authorization` header is removed before the request goes on, so the app never sees the password. An app that reads its own `Authorization` header can't sit behind a password, as a browser sends one header: it takes an address list instead, or the sign-in below.
- **Never in clear text across a network.** A password is refused on a route with `plain_for_anyone`. A route with a certificate takes one, and so does a route with `tls: off` behind a tunnel or a trusted proxy, where TLS ends at the edge the person put in front.

### Only some addresses: by the address the route already believes

- `allow` and `deny` each take up to 100 addresses or ranges, IPv4 or IPv6, kept as ranges (`203.0.113.7` is `203.0.113.7/32`). An address in both is refused.
- **`deny` is checked first; with an `allow`, everything not in it is refused,** with `403 Forbidden` and a short text. Addresses are checked before the password, so an address that may not reach a route never sees the prompt.
- **The address is the one the route believes,** never one another route's proxy vouches for. Caddy works out its own `client_ip` from what any route on the server trusts, which is why a route keeps its own belief (ADR 0030: the peer, or what its `trusted_proxies` say, in `{http.vars.dockwell_client}`). So the route's protection step first sets the variable Caddy's `client_ip` matcher reads to that belief, and only then matches the lists. Behind Cloudflare or a proxy the route trusts, that's the visitor's, as sure as ADR 0030 says; another route trusting a proxy on the same server changes nothing here, and a test says so. A route with `tls: off` and no trusted proxy sees every request come from its tunnel, so an `allow` there would let all in or none: the plan says so, and how to name the tunnel's proxy.
- **"Your address" is one click.** `GetAuthState` gains the address the control plane sees the request from, empty where it's loopback or private, as through an SSH tunnel or on a LAN, so the panel offers it only when it's an address the route could see. The app's server may see a person differently from the control server (IPv4 against IPv6, or another path), so the panel says "as the panel sees you", and the plan names the address.

### Certificates are never locked out

Caddy answers ACME's HTTP-01 challenges before any route runs ("handle them separately from any user-defined handlers", in `caddyhttp.Server.ServeHTTP` as of 2.11.7, the version `scripts/ingress-tools.sh` pins), and TLS-ALPN-01 ends in the TLS handshake. So neither a password nor an address list stands between a CA and a certificate, its first or a renewal, though the CA checks from addresses nobody lists. Caddy answers only the challenges it started, so nothing is left out of a route's own matchers, and a path the app serves under `/.well-known/acme-challenge/` stays protected. A real-Caddy test pins it: Pebble issues a certificate for a domain that asks for a password and allows only an address Pebble isn't, and the route still refuses a request without one. A Caddy release that changed the order fails that test before it ships.

### The panel's own domain isn't covered

It isn't a route, so Protect isn't offered and nothing here reaches it. A password in front of the panel would break API tokens, which use `Authorization`, and an agent joining over its domain, and the panel already has passkeys. Narrowing who reaches the panel by address is the firewall's ([ADR 0052](0052-a-servers-firewall-ssh-and-fail2ban.md)).

### Who may, and what it says

- **Whoever may change a route may protect it,** and open it again: `ProposeChange` on its environment, with no new permission. The change is on the timeline and in the audit log like any other, with each password masked.
- **Opening a route up asks nothing more,** but the plan says it in amber: "Anyone can reach app.example.com again."
- **A plan says what changes in words:** "Asks for a password: 1 user, mira (password set)", "Only from 203.0.113.7 and 198.51.100.0/24", "Refuses 192.0.2.0/24".
- **The CLI and REST take the same fields.** A password goes in an edit read from a file or standard input, so it isn't in a shell's history.
- **What the proxy has reads back.** The Caddy driver reads a route's `access` back from Caddy, users and hashes included, so drift compares as for any field: a user or a range added by hand reads as that change, and anything else in those handlers reads as a hand edit.

### In the panel, slice 1

The app's domain rows and the websites' domain rows get **Protect**, a sheet with the two choices, defaults first: a password, for one user named after the signed-in person, with a password made in the browser (24 characters from a 32-letter alphabet, about 120 bits) shown once with Copy; or only some addresses, starting at "your address". Typing a password, more users and `deny` are one step away. The row then says "Protected · password" or "Only from 2 addresses".

### Sign-in through a provider: a later slice, sketched

The PRD's "OIDC proxy" is a third member of `access`, not built here: `sign_in`, the members of the route's project, through Caddy's `forward_auth` to the control plane, which signs a person in with a passkey, or with an OIDC provider once the PRD's OIDC sign-in (Access, P1) is there, and sets a cookie on the app's domain. Its record says how that cookie is scoped, and what an agent checks when the control plane is down.

## Consequences

- A route's revisions hold bcrypt hashes, sealed. Anyone with the database and the install's key can guess at them offline: a made password resists that, and a typed one is as strong as its words. That's why one is made by default and a typed one takes 12 characters.
- A Caddy on a server holds the hashes of the routes it serves, readable by root there, as its certificates are.
- Basic auth and an app's own `Authorization` header don't mix; an address list does.
- Address lists are only as good as the address a route believes, which ADR 0030 set; behind a tunnel with no trusted proxy, they say nothing, and the plan says so.
- The panel's own domain is out of scope by design.
- The agent build gains no dependency: hashing is the control plane's, outside the route kind's package, which the agent links, and the agent only writes the hash the route holds into Caddy's config.
