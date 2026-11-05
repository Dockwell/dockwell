# 0030. Routes behind a tunnel or a proxy

- Status: Accepted. It changes what a route can be, and which ports its server holds, from [0024](0024-the-caddy-ingress-driver.md).
- Date: 2026-11-05

## Context

A route's certificate comes over HTTP-01 or TLS-ALPN-01, so the internet must reach the server on 80 or 443. A server at home or in an office, on a private address, can't be reached that way. It's commonly served through Cloudflare Tunnel or Cloudflare's proxy, which end TLS themselves and send plain HTTP on. Such a server's certificates stay failed for ever, so its routes have no way to work. Issue #85 asks for that to change, for any proxy in front, without naming one provider.

Three things in [ADR 0024](0024-the-caddy-ingress-driver.md) were written for a server the internet reaches directly: Caddy holds 80 and 443 on a server's first route, `X-Forwarded-For` is always overwritten, and a route can only send to a service.

## Decision

### A route can take no certificate

`tls: off` on a route says TLS ends before this server. The field names what it means, not who does it, so it reads the same for a tunnel, a proxy and a private network. Empty gets a certificate, as every route does now.

- **It goes on a server of its own in Caddy's config.** `dockwell_http`, which keeps the panel's rules on plain HTTP, listens on the HTTP port alone. Caddy applies no automatic HTTPS to a server that listens only there (`automaticHTTPSPhase1`, at the pinned version): it asks for no certificate for those names, and redirects none of them. A route with a certificate stays on `dockwell`, so it can't be reached over plain HTTP by anything Dockwell writes, and the panel's route can't either. Caddy puts its own redirects for other domains after the last route with a host, so they come after these.
- **A second lock.** Such a route's matcher also says `protocol: http`, which is what marks it when it reads back, and it can't answer over TLS whatever else changes.
- **One ETag covers both servers.** A change now goes through `/config/apps/http/servers` as one `PATCH`, so moving a route from one to the other is one reload, and a change made between reading and writing is still caught.
- **A name with no route gets a 404 there,** from a last route with no matcher, since Caddy would otherwise answer an empty 200, which reads as a site that works. It goes after Caddy's redirects, so a domain with a certificate still redirects.
- **What closes routes covers them.** Every route answers 503 when the ingress network's subnet is taken (ADR 0024). The route that says so has no matcher, on `dockwell`. On `dockwell_http` it matches these domains alone, as the panel's domain and domains with certificates must go on redirecting.
- **A domain is served one way or the other.** All of a domain's routes take the same setting, and a plan that would mix them is refused, as its plain HTTP would answer for some paths and redirect for others, and its certificate would have no one state. A plan changes them together; its steps change them one by one, so only the plan's check holds this.
- **The domain's certificate reads `none`,** with the message "None: TLS ends before this server", not pending or failed. It asks nothing of DNS, which points at what's in front, so the agent reads none, says nothing of it in a plan, and doesn't watch it as waiting.
- **It doesn't count against the limits on what waits for a certificate,** as it asks the authority for nothing.
- **The API's `url` says `http://`,** which is what this server serves and always answers. It can't tell what's in front, and on a private network there may be nothing to answer HTTPS.

### The plan says so, and it isn't an alarm

`PlanStep.unencrypted` is set when a route starts serving plain HTTP, whether it's made that way or changed to it:

> Traffic for this route reaches hz-1 unencrypted, as TLS ends before it. That's fine on a tunnel or a private network, and not on the open internet, where anyone on the way can read it.

The panel shows it in amber, not red. It isn't a `PlanWarning`, which makes a plan destructive, so only a person could apply it. Turning a certificate off needs no person: a token can already deploy any image to the service and read its traffic. It's worked out from the specs, so a plan made for an offline server says it too.

### The panel's own domain can't be served this way

Its passkeys and cookies need HTTPS. `route.Validate` already refuses a route on it or under it; one with `tls: off` says why. A panel with a domain still has its site behind Caddy on the first route, whatever that route takes, and that site needs HTTPS, so such a server holds every port.

### A server holds only the ports its routes need

A tunnel needs nothing on 443. So a server whose routes all take no certificate binds port 80 alone, and can run beside whatever holds 443.

- **`dockwell` stays in the config with no listeners.** Caddy takes a server with `listen: []`, binds nothing for it, and applies no automatic HTTPS, as no port differs from the HTTP one. So it makes no ACME account either. Every change sets `listen` from the routes on it, in the same reload.
- **The first route starts Caddy on 80 alone,** from a bootstrap that says so. Its plan says `starts`, with `80/tcp`.
- **A route that wants a certificate takes the rest while Caddy runs,** which is a reload: nothing stops. `INGRESS_CHANGE_TAKES_PORTS` says so, the plan checks they're free and refuses by name if not, and the step checks again before anything changes. The last such route going is `INGRESS_CHANGE_FREES_PORTS`.
- **A service can publish what the ingress doesn't hold.** The rule from ADR 0024 follows what's held. One on 443 stops a later route with a certificate, by name.

A tunnel's own container can reach Caddy, which is on the server's own network, listening on every address:

| `cloudflared` runs | Its service URL | What Caddy sees it as | Note |
| --- | --- | --- | --- |
| With `--network host` | `http://localhost:80` | `127.0.0.1` | Host access, for a person to approve, as any container on that network is |
| On a bridge, with `--add-host host.docker.internal:host-gateway` | `http://host.docker.internal:80` | Its address on the bridge | A firewall such as ufw treats this as incoming, so it needs to allow it |

The Docker job runs both. A service on an `internal` environment network has no way to the server.

### What a failed certificate says

`routeCertMessage` adds, to a failure and to a certificate waiting on DNS that points elsewhere: "If this server sits behind a tunnel, or a proxy that ends TLS, it needs no certificate: set tls to off on its routes." A rate limit, a CAA record and a refused name aren't what a tunnel causes, so those don't say it. The panel's own certificate never does, as its domain can't take the setting. The plan's words on DNS that points elsewhere say the same.

### Which proxy to believe

ADR 0024 overwrites `X-Forwarded-For` and strips what else a client might claim. Behind a tunnel, Caddy's peer is the tunnel, so every client reads as that. A route's `trusted_proxies` names who to believe, each an address, a range, or `cloudflare` for Cloudflare's published ranges.

- **Per route, in its spec,** so it has a plan, a history and an undo like the rest of a route. A setting per server would be a new kind of resource for one list.
- **Caddy decides this per server,** for a request before it matches a route. So `dockwell` and `dockwell_http` each trust what any route names, and what a route reads comes from its headers: a route that names a proxy sets `X-Real-IP` and `X-Forwarded-For` from `{http.vars.client_ip}`, and one that doesn't overwrites those, `X-Forwarded-Proto` and `X-Forwarded-Host` from the connection, so what another route names doesn't reach it. A route that names one proxy also believes what another route names; a plan says who, and a person approved each.
- **Read from the right.** `trusted_proxies_strict` is on: Caddy's default takes the leftmost address, which is whatever the client sent, through any proxy that appends to the header as Cloudflare does. Strict takes the first from the right that isn't a trusted proxy. So a service reads one address in each, the client's, and what a client wrote further left doesn't reach it to be read.
- **A proxy's scheme comes through.** Through a proxy it trusts, Caddy keeps `X-Forwarded-Proto`, so an app knows its visitors came over HTTPS. One that sees `http` may redirect them for ever.
- **What's still stripped** stays stripped, `CF-Connecting-IP` among them, even from a proxy it trusts: Caddy can't remove a header for one peer and keep it for another. A service reads `X-Real-IP` or `X-Forwarded-For`.
- **The preset is a list in the agent's source,** as Caddy's module for it is a third-party build. A range Cloudflare adds later isn't believed until a release has it, which fails safe: a service reads Cloudflare's address, not a forged one. Caddy's config holds the ranges, so a new list reads as drift on a route that names the preset, and reverting applies it.

**What a token can do.** Believing a proxy makes what it says of a client reach services as true, and apps use that for allowlists and limits. So naming one is host access, of the new kind `trusted_proxy`, and a token gets `needs_session`, for a preset too, and for each one a change adds. Removing one needs no person, and undoing that does. A list wider than `/8`, or any spelling of everything such as `::ffff:0:0/96`, is refused for everyone, in Validate: it isn't naming a proxy, and a person who approves it has made client addresses forgeable for nothing.

### A route can reach a port on the server itself

For something that isn't a container, such as a process on the host, or a container someone else runs on a loopback port. `host_port` stands in for `service` and `port`.

- **Host access, of the new kind `server_port`,** under ADR 0021's gate, with the port in the plan. It reaches past containers, to whatever listens there. Another port is another approval.
- **Dockwell's own ports are refused,** on plan and on apply: the panel's, the agent listener's, and those it holds its public site on, which the control plane tells its agent. So are the proxy's own, which would dial itself. Reaching the panel's loopback listener from the network is exactly what ADR 0011 refuses. A server that joined has no such listeners.
- **Caddy dials `127.0.0.1:<port>`,** from the server's own network. Nothing a token sends becomes config: the port is a number, and the address comes from the agent.
- **It reads back as itself, so it isn't drift.** A dial the table doesn't know still reads as drift, and so does anything beside it.
- **It needs no network.** Routes to the server's own ports alone don't make the ingress network or its keeper.
- **A process listening only on IPv6 isn't reached,** as Caddy dials IPv4.

### A real certificate with no open port

DNS-01 through Cloudflare gives one, and it is [#94](https://github.com/Dockwell/dockwell/issues/94), not this. It's not small:

- **Caddy's official image has no DNS providers.** It needs Caddy built with `caddy-dns/cloudflare`, as an image of Dockwell's own, published and pinned by digest in place of the official one, so a mirror still can't change what runs.
- **A route would name a secret,** with `tls: dns` and the name of its API token, through the value path ADR 0022 gives services. Caddy's config would hold the token, so what's said of the panel's key applies: it goes in the last config and the agent's copy, and goes when the last route does.
- **That token can edit a zone's DNS,** so naming one is what a person approves.
- **`dns` in `issuers[].challenges`** on one automation policy per domain, so others keep HTTP-01. Failures get their own problem type, with the fix. Wildcards, refused today for want of this, are decided then.

## Consequences

- **What's between whatever ends TLS and this server is unencrypted.** The plan says so. On a tunnel that's the loopback or a bridge; on a proxy over the internet, as with Cloudflare's Flexible mode, it's anyone on the way.
- **Port 80 answers on every address.** A route with `tls: off` is reachable by whoever can reach that port, whatever is in front, so a server with a public address needs its firewall to let only its proxy in.
- **`Proxy.Start` takes whether to start on the plain port alone.** `ingress.Upstreams` doesn't change, and a route's upstreams move through a rollout the same way whichever server it's on.
- **A panel on a domain, on a server with no open ports, still can't get a certificate,** and is reached through the SSH tunnel.
- **Names no public CA issues for are still refused,** though a route with `tls: off` doesn't ask one. It can be lifted without changing what a route means.
- **Memory:** nothing here moves an idle agent, as `make budgets` shows in the pull request.
- **The threat model** is in [docs/security/ingress.md](../security/ingress.md), and [docs/install.md](../install.md#behind-cloudflare-tunnel-or-another-proxy) says how to set this up.

## Tests

- **Rules:** each refusal on its own (`route_test.go`, `tunnel_test.go`).
- **The gate,** through the API with a real token, on both stores (`TestARoutesReachNeedsAPerson`): a port and a proxy each need a person, the same again doesn't, less doesn't and its undo does, and everything is refused to everyone.
- **`DOCKWELL_TEST_CADDY`:** plain HTTP with no redirect and no certificate, nothing bound on 443, both kinds beside each other, a route moved between them in one reload, a WebSocket through those reloads, closing them, removing one with Caddy down, a hand edit, the panel's rules beside them, and what a service reads through a proxy it trusts, one it doesn't and a route that names none, right to left.
- **`make docker-e2e`:** a route on real Docker with no ACME order made and its state `none`, `cloudflared`'s two ways to reach Caddy, a port on the server with a token refused and a person applying it, 443 taken and freed, and Dockwell's own port refused.
