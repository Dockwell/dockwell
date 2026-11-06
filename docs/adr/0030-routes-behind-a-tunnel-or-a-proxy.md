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
- **What closes routes covers them.** Every route answers 503 when the ingress network's subnet is taken (ADR 0024). The route that says so has no matcher, on `dockwell`. On `dockwell_http` it matches what these routes match, as the panel's domain and domains with certificates must go on redirecting, and someone a route doesn't answer must see no difference.
- **A domain is served one way or the other.** All of a domain's routes take the same setting. It's a claim the engine holds, as for a domain's one environment: it sees every route and the whole of a change, when it's proposed and again as it applies, so a plan made while the server was offline can't get past it, and a change can turn them together though its steps go one by one.
- **The domain's certificate reads `none`,** with the message "None: TLS ends before this server", not pending or failed. It asks nothing of DNS, which points at what's in front, so the agent reads none, says nothing of it in a plan, and doesn't watch it as waiting.
- **It doesn't count against the limits on what waits for a certificate,** as it asks the authority for nothing.
- **The API's `url` says `http://`,** which is what this server serves. It can't tell what's in front, and on a private network there may be nothing to answer HTTPS. The panel is about where a visitor goes, so its row, copy and link say `https://`, and `http://` only for a route that says `plain_for_anyone`.

### A route with no certificate doesn't answer the whole internet

Caddy holds port 80 on every address. On a server with a public address and no firewall, `curl -H 'Host: admin.example.com' http://SERVER_IP/` would go around a tunnel, and any Access policy or WAF in it. So who a route answers is decided by the peer on the connection, with Caddy's `remote_ip` matcher, on the route itself. That avoids the bind conflict a loopback listener would have beside `:80`.

| The route | It answers |
| --- | --- |
| Names `trusted_proxies`, with `cloudflare` or addresses and ranges | Those peers alone |
| Names none | This server's own loopback, `127.0.0.0/8` and `::1` |
| Says `plain_for_anyone` | Anyone who can reach the port. A person approves it, and the plan says so in amber |

- **Anyone else gets what a name with no route gets,** the same 404, so they can't tell the route is there.
- **A blanket rule for private addresses would let a whole LAN in,** as real servers sit on private addresses, so there's none. A container on a bridge names its network's range in `trusted_proxies`, which a person approves, and the install guide says how. That one is also believed, as what's on that network is what's in front, so it wants a network of its own: a service with no networks runs on Docker's default `bridge`, and a token can deploy one, so naming that range answers whatever a token deploys. A range names everything on it, and a network joined with `external_networks` can hold other projects' services ([ADR 0021](0021-the-docker-driver.md)), so the guide says to give a proxy a network of its own.
- **This server is its own network too.** A service with `host_network`, which a person approves, comes from loopback, as any process on the server does, so it can reach a route that names none.
- **It's on the route, with no listener of its own.** HTTP-01 for other routes on the same port goes first, as Caddy handles a challenge before any route (`Server.ServeHTTP`), and a test holds that against Pebble.
- **A route with a certificate has no such limit.** It answers over HTTPS with its own certificate, whatever it believes, as it always has.

A route behind a tunnel on the server's own network works with nothing named, and every visitor reads as `127.0.0.1` until it names one.

### The plan says so, and it isn't an alarm

`PlanStep.unencrypted` is set when a route starts serving plain HTTP, whether it's made that way or changed to it:

> Traffic for this route reaches hz-1 unencrypted, as TLS ends before it. That's fine on a tunnel or a private network, and not on the open internet, where anyone on the way can read it.

The panel shows it in amber, not red. It isn't a `PlanWarning`, which makes a plan destructive, or a `PlanNote`, which is for what no step says, and a step says this. `PlanStep.answers` says who it answers, also in amber:

> It answers only what runs on hz-1 itself, such as a tunnel on its own network. Anyone else gets what a name with no route gets.

It's all worked out from the specs, so a plan made for an offline server says it too.

### Taking a live domain off HTTPS needs a person

`https://live.example.com` starts failing, plain `http://` answers 200, and a browser that saw HSTS refuses it until that runs out. So a route with `tls: off` on a domain any route served with a certificate within the undo window is host access, of the new kind `https_off`, and the plan says what stops. The engine reads the route's history as it does for a takeover, so a delete and a new route, in one change or two, is the same as an edit. A domain that has only been plain is a token's to serve, as it can already deploy any image to the service, and to go on editing, once a person has said so.

### The panel's own domain can't be served this way

Its passkeys and cookies need HTTPS. `route.Validate` already refuses a route on it or under it; one with `tls: off` says why. A panel with a domain still has its site behind Caddy on the first route, whatever that route takes, and that site needs HTTPS, so such a server holds every port.

### A server holds only the ports its routes need

A tunnel needs nothing on 443. So a server whose routes all take no certificate binds port 80 alone, and can run beside whatever holds 443.

- **`dockwell` stays in the config with no listeners.** Caddy takes a server with `listen: []`, binds nothing for it, and applies no automatic HTTPS, as no port differs from the HTTP one. So it makes no ACME account either. Every change sets `listen` from the routes on it, in the same reload, and only a route that names a host counts.
- **The first route starts Caddy on 80 alone,** from a bootstrap that says so. Its plan says `starts`, with `80/tcp`.
- **A route that wants a certificate takes the rest while Caddy runs,** which is a reload: nothing stops. `INGRESS_CHANGE_TAKES_PORTS` says so, the plan checks they're free and refuses by name if not, and the step checks again before anything changes. The last such route going is `INGRESS_CHANGE_FREES_PORTS`.
- **A service can publish what the ingress doesn't hold.** The rule from ADR 0024 follows what's held. One on 443 stops a later route with a certificate, by name.

What's on the other side of a tunnel can reach Caddy, which is on the server's own network, listening on every address. Cloudflare's docs give `docker run` with a token and a service URL such as `http://localhost:80`, and say nothing of how a container reaches its host, so this much is Docker's:

| A container runs | It dials | What Caddy sees it as | Note |
| --- | --- | --- | --- |
| With `--network host` | `http://127.0.0.1:80` | `127.0.0.1` | Host access, for a person to approve, as any container on that network is. `localhost` may reach `::1`, which a route naming `127.0.0.1` doesn't answer |
| On a network of its own, with `--add-host host.docker.internal:host-gateway` | `http://host.docker.internal:80` | Its address on that network | Needs that network's range named, and not Docker's default `bridge`, where a service with no networks runs. A firewall such as ufw treats this as incoming, so it needs to allow it |

CI's Service end to end on Docker job proves each of these with a container that does what `cloudflared` would, not `cloudflared` itself, which needs Cloudflare's account. A service on an `internal` environment network has no way to the server.

### What a failed certificate says

`routeCertMessage` adds, to a failure and to a certificate waiting on DNS that points elsewhere: "If this server sits behind a tunnel, or a proxy that ends TLS, it needs no certificate: set tls to off on its routes." A rate limit, a CAA record and a refused name aren't what a tunnel causes, so those don't say it. The panel's own certificate never does, as its domain can't take the setting. The plan's words on DNS that points elsewhere say the same.

### Which proxy to believe

ADR 0024 overwrites `X-Forwarded-For` and strips what else a client might claim. Behind a tunnel, Caddy's peer is the tunnel, so every client reads as that. A route's `trusted_proxies` names who to believe, each an address, a range, or `cloudflare` for Cloudflare's published ranges. It's in the route's spec, so it has a plan, a history and an undo like the rest of a route.

**Belief is per route.** Caddy decides what it trusts per server, before it matches a route, so `dockwell` and `dockwell_http` each trust what any route names. A route then decides for itself: its first handler is a subroute that sets two request variables, with a matcher on the peer, and its `reverse_proxy` writes its headers from those.

- From a peer the route didn't name, the client is the peer, and the scheme is what the connection came over, whatever another route on the server names.
- From a peer it named, the client is `{http.vars.client_ip}`, which Caddy finds in `X-Forwarded-For` from the right, as `trusted_proxies_strict` has it. Its default takes the leftmost, which is whatever the client sent, through any proxy that appends to the header, as Cloudflare does.
- **What Caddy can't do per route:** that walk from the right skips an address any route on the server names, not only this one's. So anyone whose outgoing address is in a range named on any route on that server gets past it, to whatever they wrote further left, and so does a customer of a shared CDN or cloud range on a route that names it. They get a forged address and nothing else. Routes naming no proxy, routes naming only the preset, and the panel aren't affected. The plan names who else is believed only by naming what's on the route itself, so read the threat model before naming a shared range. `TestAnotherRoutesRangeIsWalkedPastOnRealCaddy` pins this as a known limit.
- Headers a client might claim are stripped, and `X-Forwarded-Host`, `X-Forwarded-Proto`, `X-Forwarded-For` and `X-Real-IP` are written by Caddy. A visitor's `X-Forwarded-Host` passes through a proxy as anything else does, and an app builds a password reset link from it, so it's the route's own domain. So is `Host`, on every kind of route, as Caddy matches on the name before the last colon: `Host: app.example.net:1,evil.example.org` reaches the route, and Rails, Rack and Werkzeug's ProxyFix read the last of that list, by their source. A port a proxy in front keeps in `Host` goes with that. A route from before this reads as Dockwell's own, and the next change on its server writes this over it. `https` from a named peer is taken at its word, so an app knows its visitors came over HTTPS, and nothing else is. `X-Forwarded-Port`, `X-Forwarded-Server`, `X-Forwarded-Ssl`, `X-Forwarded-Scheme`, `X-Url-Scheme`, `Front-End-Https`, `X-Original-Host`, `X-Host`, `X-Forwarded-Protocol`, `X-Forwarded-Prefix`, `X-Forwarded-Uri`, `X-Original-Url`, `X-Rewrite-Url` and `X-Scheme` don't reach a service at all, nor do `Client-Ip` and `X-Proxyuser-Ip`, beside the claims of an address ADR 0024 strips. Symfony and IIS take `X-Original-Url` and `X-Rewrite-Url` for the path, past what a proxy in front allows. Tornado reads `X-Scheme` ahead of `X-Forwarded-Proto`, so with `xheaders` on, a visitor could say they came over HTTPS. A route from before those last eight reads as Dockwell's own, and the next change on its server writes them in.

**The `cloudflare` preset reads Cloudflare's header, not X-Forwarded-For.** Its ranges are shared by every Cloudflare customer. [Cloudflare's docs](https://developers.cloudflare.com/fundamentals/reference/http-headers/) (last updated 28 October 2026) say a cross-zone Worker's request carries `CF-Connecting-IP: 2a06:98c0:3600::103`, that an `X-Forwarded-For` already on a request is appended to, and that an origin should read `CF-Connecting-IP` instead. That address is in the preset, so reading `X-Forwarded-For` from the right would skip it, and land on what the Worker's author wrote. That the Worker's value survives is inferred, not tested, as nothing here sends anything to Cloudflare, and it's the same route either way: reading the documented header is right whether or not it does.

- **Only from a peer in those ranges is the header read,** and those ranges aren't among what the server trusts, so nothing else walks past them, whatever else is named. No other peer can forge it through this route, or through another.
- **A named range inside Cloudflare's is refused,** with the preset named, as it would walk past the same address.
- **What isn't an address goes no further.** The step looks at what the placeholder gives, as `header_regexp` passes if any one line of a header matches, and the placeholder joins them, so a header sent twice would reach a service as a list. Otherwise the client is the peer. It checks the shape of an address, as Caddy compiles this for each route: an exact pattern was over three times the memory in Go's heap, and this took Caddy from 191 to 217 MB at 1,000 routes naming the preset. So `999.1.1.1` gets through, and a list, an empty value or text doesn't. A config from before this reads as drift on a route that names the preset, and reverting applies it.
- **What this address is good for:** it's what Cloudflare says connected to it, which is good for logs, and for limits on visitors who aren't trying to get past them. It isn't proof of who a request is from, and a limit by it doesn't hold against someone evading it. All of this is inferred from Cloudflare's docs, and hasn't been run on Cloudflare:
  - [The header page](https://developers.cloudflare.com/fundamentals/reference/http-headers/) (last updated 28 October 2026) says a cross-zone Worker reads as `2a06:98c0:3600::103`, which all of them share, a same-zone Worker's `x-real-ip` sets the header, and one sending to a domain that isn't on Cloudflare, as a DNS-only record is, writes what it likes. So the guide says to name the preset only for a domain Cloudflare proxies.
  - That doesn't close it. [Origin Rules](https://developers.cloudflare.com/rules/origin-rules/) (last updated 14 August 2026) can rewrite `Host`, so a customer who knows the origin could point a proxied record at it, rewrite `Host` to the route's domain, and have a same-zone Worker set the address. That page lists the override for Enterprise alone, and [its settings page](https://developers.cloudflare.com/rules/origin-rules/features/) (last updated 5 May 2026) names no plan. On a lesser plan their request carries their own `Host`, which no route answers.
  - Other ways of rewriting `Host` don't reach a domain Cloudflare proxies, as read: [Load Balancing's override](https://developers.cloudflare.com/load-balancing/additional-options/override-http-host-headers/) (last updated 16 April 2026) takes a name in the account, the endpoint's address, or one that publicly resolves to it, and a Worker's `resolveOverride` leaves `Host` matching the URL. That's what turned up, not proof there's no other.
  - A firewall can't tell any of this apart, as it's all from Cloudflare's ranges, and [Authenticated Origin Pulls](https://developers.cloudflare.com/ssl/origin-configuration/authenticated-origin-pull/) (last updated 14 August 2026) doesn't apply in Flexible mode, and its global certificate only says it came through Cloudflare. Don't allowlist on it. The header page names a tunnel, or that with your own certificate, as how to know traffic comes from your own zone, so an address that has to hold wants a tunnel.
- **The list is in the agent's source,** as Caddy's module for it is a third-party build. A range Cloudflare adds later isn't believed or answered until a release has it, which fails safe. `.github/workflows/cloudflare-ranges.yml` compares it nightly with what Cloudflare publishes, and opens an issue when they differ. Caddy's config holds the ranges, so a new list reads as drift on a route that names the preset, and reverting applies it. Only the preset's ranges exactly read as its name: one missing, as when Cloudflare adds one, one too many, as when it gives one up, and one added by hand each read as the list itself, which differs from the spec.

A route naming `127.0.0.1` behind a tunnel reads `X-Forwarded-For`. Cloudflare appends what connected to it, so that's the rightmost, and it isn't walked past, as Cloudflare's ranges aren't named. So a Worker through a tunnel reads as its own address, and the same goes for what it's good for.

**What a token can do.** Believing a proxy makes what it says of a client reach services as true, and apps use that for allowlists and limits. On a plain route it also says who gets an answer. So naming one is host access, of the new kind `trusted_proxy`, and a token gets `needs_session`, for a preset too, and for each one a change adds. Removing one needs no person, and undoing that does. A range that can't be one proxy is refused for everyone, in Validate: wider than `/8` on a private network, `/12` on the internet, or `/28` in IPv6, or any spelling of everything such as `::ffff:0:0/96`. Cloudflare's own widest are `/13` and `/29`.

### A route can reach a port on the server itself

For something that isn't a container, such as a process on the host, or a container someone else runs on a loopback port. `host_port` stands in for `port`, and for `service` when what's there is no service's.

**A route can name both.** `service` says whose it is, and `host_port` says where it dials, rather than the service's container. That's how a service on the server's own network has a domain like any other's: `ListDomains` finds it by its service, a service with such a route can't go while it serves, in the same words as any route, and a plan can take the two together. The form sends both for a service on the server's own network. The host-access gate is unchanged, as it's the port a person approves. The proxy can't tell whose a route is, so the agent keeps that beside its addresses, and a route reads back as what was asked for, not as drift. A rollout moves what dials a container, so it never touches such a route: `routesOf` and the address table go by what a route dials, and a route that dials `127.0.0.1` is nothing of theirs.

- **Host access, of the new kind `server_port`,** under ADR 0021's gate, with the port in the plan. It reaches past containers, to whatever listens there. What a person approves is the port at a domain and path, over HTTPS or plain HTTP: a person who approved it behind Access on one name hasn't approved it on another, so any of those changing is another approval, and so is another port or another server.
- **The plan says what that lets happen,** in `PlanStep.answers` and `PlanStep.server_port`: who can now reach the port, that what's there sees every caller as `127.0.0.1`, which an app that trusts localhost will believe, and what listens there, from what the agent can read, or that nothing does yet. What listens is what Caddy reaches as it dials `127.0.0.1`, on every address or on that one: a process on another of the server's addresses alone, or on IPv6 alone, isn't reached, so it isn't named, and the plan says nothing listens.
- **Docker's API is refused for everyone,** as it's root with no legitimate route: on 2375 and 2376 by number, in Validate, and on any other port by what listens there, `dockerd`, as the plan is made and as the step applies. Anything else is a person's to read and decide: a database on 5432 is a bad idea the plan names, and may be meant.
- **Dockwell's own ports are refused,** on plan and on apply: the panel's, the agent listener's, and those it holds its public site on, which the control plane tells its agent. So are the proxy's own, which would dial itself. Reaching the panel's loopback listener from the network is exactly what ADR 0011 refuses. A server that joined has no such listeners. A listener moved onto a routed port later is logged, once, as nothing's exposed: the panel's listener refuses a public `Host`.
- **Caddy dials `127.0.0.1:<port>`,** from the server's own network. Nothing a token sends becomes config: the port is a number, and the address comes from the agent.
- **It reads back as itself, so it isn't drift.** A dial the table doesn't know still reads as drift, and so does anything beside it.
- **It needs no network.** Routes to the server's own ports alone don't make the ingress network or its keeper.
- **A process listening only on IPv6 isn't reached,** as Caddy dials IPv4.
- **It's how a route reaches a service on the server's own network** ([ADR 0021](0021-the-docker-driver.md)). Docker can't put such a container on another network, so the ingress has no address for it, and a route that names it by `service` would fail as its step ran, in Docker's words. So it's refused as it plans, and says to use `host_port`, whether the service is there or the plan takes it there. A service with a route left on it can't go there either, and goes on serving. A plan can move both: the service's step then asks the ingress for nothing, so they can run in either order.

### A real certificate with no open port

DNS-01 through Cloudflare gives one, and it is [#94](https://github.com/Dockwell/dockwell/issues/94), not this. It's not small:

- **Caddy's official image has no DNS providers.** It needs Caddy built with `caddy-dns/cloudflare`, as an image of Dockwell's own, published and pinned by digest in place of the official one, so a mirror still can't change what runs.
- **A route would name a secret,** with `tls: dns` and the name of its API token, through the value path ADR 0022 gives services. Caddy's config would hold the token, so what's said of the panel's key applies: it goes in the last config and the agent's copy, and goes when the last route does.
- **That token can edit a zone's DNS,** so naming one is what a person approves.
- **`dns` in `issuers[].challenges`** on one automation policy per domain, so others keep HTTP-01. Failures get their own problem type, with the fix. Wildcards, refused today for want of this, are decided then.

## Consequences

- **What's between whatever ends TLS and this server is unencrypted.** The plan says so. On a tunnel that's the loopback or a bridge; on a proxy over the internet, as with Cloudflare's Flexible mode, it's anyone on the way.
- **A rollout's own routes must keep to this.** [#93](https://github.com/Dockwell/dockwell/pull/93)'s route that says `Connection: close` of an old config has no host, so it doesn't make HTTPS listen. It belongs on both servers. Whichever lands second handles that, and tests a route with no certificate through a rolling deploy.
- **Caddy's config grows with each route that names a proxy,** by a handler with up to four steps. `make budgets` doesn't measure Caddy, so this was measured at the limit of 1,000 routes, on a busy machine, where runs differ by up to a third: 86 to 105 MB with a reload of 0.6 s naming none, 114 to 142 MB and 1.7 s naming one proxy, and 142 to 192 MB and 3.9 s each naming the preset, or 217 MB with the check on `CF-Connecting-IP`. That is well inside its 1 GiB, and the 15 s an admin call gets.
- **`Proxy.Start` takes whether to start on the plain port alone.** `ingress.Upstreams` doesn't change, and a route's upstreams move through a rollout the same way whichever server it's on.
- **A panel on a domain, on a server with no open ports, still can't get a certificate,** and is reached through the SSH tunnel.
- **Names no public CA issues for are still refused,** though a route with `tls: off` doesn't ask one. It can be lifted without changing what a route means.
- **Port 80 is still needed.** A server whose other proxy holds it can't serve a route this way until that proxy stops. A tunnel straight to a published port needs no route at all, and the install guide says so.
- **Who a route answers is decided path by path.** A stranger to a path only a proxy reaches gets what else covers it, so a domain's root that answers anyone answers for that path too, and not a 404. If the root sends to the same service, that reaches the service at that path, past the proxy. The threat model says why that stays, and what to do instead.
- **Some of this hasn't been run.** A test can't reach Cloudflare's edge, and no real tunnel has run, so what's said of them is read from their docs, with the dates beside it in the threat model. `cloudflared` doesn't run in the Docker job: a container of nginx's image stands in for it, each way the guide runs it. A route moved between servers is planned and gated in tests, and no test runs two proxies to watch it go.
- **The threat model** is in [docs/security/ingress.md](../security/ingress.md), and [docs/install.md](../install.md#behind-cloudflare-tunnel-or-another-proxy) says how to set this up.

## Tests

- **Rules:** each refusal on its own (`route_test.go`, `tunnel_test.go`), and a domain's two ways and going off HTTPS on both stores (`claims_test.go`).
- **The gate,** through the API with a real token, on both stores (`TestARoutesReachNeedsAPerson`): a port, where it answers, a proxy and anyone each need a person, the same again doesn't, less doesn't and its undo does, deleting and undoing or going through a service gets nowhere, and everything is refused to everyone.
- **`DOCKWELL_TEST_CADDY`:** plain HTTP with no redirect and no certificate, nothing bound on 443, both kinds beside each other, a route moved between them in one reload, a WebSocket through those reloads, closing them, removing one with Caddy down, a hand edit of each thing that decides who it answers and what it believes, the panel's rules beside them, who a route answers from a loopback peer on IPv4 and IPv6 and from an address of the machine's own, and what a service reads from a proxy it named, one another route named, and the preset, with the peer standing in for Cloudflare's edge, as no test can come from its addresses.
- **`DOCKWELL_TEST_PEBBLE`:** HTTP-01 for one route past another's reach and the 404.
- **`make docker-e2e`:** a route on real Docker with no ACME order made and its state `none`, a stranger at the server's own address getting a 404, a container on the host network and one on a network of its own, with that range named or not, and one on Docker's default `bridge` not answered, a port on the server with a token refused and a person applying it, plain HTTP for anyone, a live domain's refusal to go off HTTPS for a token, 443 taken and freed, and Dockwell's and Docker's own ports refused.
