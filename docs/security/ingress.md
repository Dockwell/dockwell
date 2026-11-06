# Threat model: public traffic through the ingress

Every request a server takes from the internet goes through its ingress, so this is the part of Dockwell the most people can reach. [ADR 0024](../adr/0024-the-caddy-ingress-driver.md) and [ADR 0030](../adr/0030-routes-behind-a-tunnel-or-a-proxy.md) are the design. This says what it protects, from whom, and what it doesn't.

## What's worth protecting

- **The panel.** It controls root on every server. Its session cookie, its passkeys' RP ID, and the rule that plain HTTP never reaches it beyond loopback ([sign-in](sign-in.md)).
- **A domain's traffic and certificate.** Whoever a domain's route sends to reads that site's cookies, passwords and users' requests.
- **Caddy's admin API.** It can rewrite any route, and so everything above.
- **Certificates and ACME account keys** in `/var/lib/dockwell/ingress/data`.

## Who we consider

| Who | What they have |
| --- | --- |
| Anyone on the internet | Requests to ports 80 and 443, with any `Host`, SNI and headers |
| Someone on the way to a route with `tls: off` | What passes between whatever ends TLS and this server, unencrypted |
| A proxy a route names | Its word on where a request came from, which that route's service believes |
| A compromised app | Code in a container on a bridge network, and on the ingress network if it has a route |
| A write token, leaked or careless | `ProposeChange` and `ApplyChange` for what isn't destructive and gives no host access ([ADR 0016](../adr/0016-scoped-api-tokens.md)) |
| Something else on the server, unprivileged | Loopback, any port it can bind, and files others can read |
| A signed-in person, root on the server | Everything. Nothing here defends against them |

## What each can and can't do

### Anyone on the internet

- **Pretend to be HTTPS to the panel.** They can't. The panel's proxied listener is a unix socket, and a request it takes is HTTPS because of that listener. No header decides it. `X-Forwarded-For` is what Caddy wrote: with no `trusted_proxies`, it overwrites what a client sends.
- **Reach a site by another name.** A `Host` that isn't the TLS name gets 421 (`strict_sni_host`), and a name with no route gets a TLS alert: nothing is served for it, and no certificate is asked for, as on-demand issuance is off.
- **Burn the CA's rate limit.** They can't trigger issuance: Caddy issues only for the names in routes.
- **Forge where a request came from.** They can't. Caddy overwrites `X-Forwarded-For`, sets `X-Real-IP` to the real client address, and removes what else a client might claim, such as `Forwarded`, `True-Client-IP` and `CF-Connecting-IP`, so an app that trusts one of these for per-address limits or allowlists reads the truth or nothing. With a proxy in front and none named, what a service reads is the proxy's address.
- **Forge it through a proxy a route names.** Only a request whose peer is in that route's own list has anything believed, whatever another route on the server names. From a range, `X-Forwarded-For` is read from the right, to the first address that isn't a proxy, so what a client writes itself lands further left, and a service gets the one address, not the list.
- **Forge it through a range any route on the server names.** Two groups can, which is a known limit, and a test pins it. Caddy has that walk per server, so it skips an address named on any route there. So anyone whose outgoing address is in a range named on any route on that server can write the address a service reads, through a route that names a proxy. So can a customer of a shared CDN or cloud range on a route that names it, whose compute comes from that range. What they get is a forged address and nothing else: no more reach, and no change to `X-Forwarded-Proto`. Routes naming no proxy, routes naming only `cloudflare`, and the panel aren't affected. A range too wide to be a proxy is refused, which limits who this can be and doesn't remove it. If a service acts on the address it reads, name your proxy's own addresses, not a provider's range.
- **Forge it through `cloudflare`.** Some can, going by Cloudflare's docs. This is inferred from reading them, and hasn't been run on Cloudflare. A route reads `CF-Connecting-IP` from a peer in Cloudflare's ranges alone, and only if it has the shape of an address, and never walks `X-Forwarded-For` past those ranges, so nobody outside Cloudflare can write it. Every Cloudflare customer comes from those ranges, though:
  - **With a zone of their own that can rewrite `Host`.** [Cloudflare's header page](https://developers.cloudflare.com/fundamentals/reference/http-headers/) (last updated 28 October 2026) says a same-zone Worker's `x-real-ip` sets `CF-Connecting-IP`. [Origin Rules](https://developers.cloudflare.com/rules/origin-rules/) (last updated 14 August 2026) can rewrite `Host` on a proxied record. So a customer who knows your server's address could point a record at it, rewrite `Host` to your domain, and write any address. That page lists the override for Enterprise alone. On a lesser plan their request carries their own `Host`, which no route answers. No other way turned up, which doesn't prove there's none.
  - **With a Worker sending to a domain that isn't on Cloudflare.** That Worker writes what it likes, so name the preset only for a domain Cloudflare proxies. That closes this way, not the one above.
  - **With a Worker on another zone.** It reads as `2a06:98c0:3600::103`, which all of them share, so it can't write an address, and a limit on that one falls on all of them.

  Those ranges are shared by every Cloudflare customer, so a firewall can't tell them apart, and Authenticated Origin Pulls doesn't apply in Flexible mode. **Don't allowlist on this address.** It's good for logs. A limit by it holds for visitors who aren't trying to get past it, and each of them reads as what connected to Cloudflare. It doesn't hold against someone evading it. If an address has to hold, use a tunnel, which is one of the two ways Cloudflare's page names to know traffic comes from your own zone. The other is Authenticated Origin Pulls with your own certificate, which Flexible mode can't use.
- **Say how a request came.** They can't, on any route. A service reads the route's own domain in `Host` and `X-Forwarded-Host`, whatever a visitor sends. Caddy matches a route on the name before the last colon of `Host`, so `Host: app.example.net:1,evil.example.org` reaches the route, and an app that takes the last of a list would build a reset link from it. `X-Forwarded-Proto` is what the connection came over, or `https` from a proxy that route named, and nothing else. Other claims of the kind, such as `X-Forwarded-Ssl` and `X-Original-Host`, don't reach a service. A proxy that lets a visitor's `X-Forwarded-Proto: https` through can have an app believe that, as naming a proxy says to believe it. Cloudflare overwrites it.
- **Go around a tunnel or a proxy.** They can't reach a route with `tls: off`. It answers the proxies it names by the peer on the connection, or with none, this server's own loopback, and to anyone else it's what a name with no route is, a 404, whatever `Host` or headers they send. So an Access policy or a WAF in a tunnel can't be walked around from the server's own address. With `plain_for_anyone`, a person has said otherwise, and they can. A proxy it names can still be reached by whoever else its range holds, as with Cloudflare's, so what's in front is only as narrow as what it names.
- **Replay a captured request.** They can't. HTTP/3's 0-RTT is off, so Caddy takes no request before the handshake finishes, and none in early data, whose source address isn't proven either. A test holds this on real Caddy.

### A compromised app

- **Reach another environment's service through the ingress network.** It can't: `enable_icc: false` stops containers on that network reaching each other. CI's Service end to end on Docker job checks this on a real engine. On a shared bridge network it could, as it always could: use an environment's own networks to keep services apart.
- **Get out through the ingress network.** It can't: the network is `internal`, with no gateway. A service on an `internal` environment network stays cut off, and no container's default route moves.
- **Reach Caddy's admin API, or the panel's socket.** Neither is on TCP. Both are in `ingress/run`, which no container may mount, as it is under Dockwell's data directory ([ADR 0021](../adr/0021-the-docker-driver.md#host-access)).
- **Reach the panel over loopback.** A bridged container has no route to the server's loopback.
- **Reach Caddy.** It can, on the bridge's gateway, ports 80 and 443, as anyone can. It gets what anyone gets, unless a route names the range of a network it's on. Then it's that route's proxy: it reaches the route, a route to a port on the server too, and says who its client is. A service with no networks runs on Docker's default `bridge`, and a token can deploy one with no person approving. So a route that names that range answers whatever a token deploys, which is why the guide gives a proxy a network of its own.
- **Take another service's address.** A container on its own can't name `dockwell_ingress`, by its name or by an ID: Docker takes either, so the agent looks each network up exactly, and refuses one that answers to another name. A service's networks are its environment's. A person with root could; see below.
- **Keep serving after a reboot, as something else.** Addresses are static, so Docker gives a container its own address or fails to start it. Caddy never sends one service's traffic to another because they started in another order.
- **Get another's traffic by the network going.** `docker network prune` can't take the network, as a container sleeps on it. If someone removes both, the network comes back on its own subnet with the same addresses. If another network has that subnet, every route answers 503, and no address is reused ([ADR 0024](../adr/0024-the-caddy-ingress-driver.md#how-caddy-reaches-containers-and-how-upstreams-are-named)). There's a window of up to a minute before the agent sees that, as there's no event for it, in which Caddy dials whatever now has those addresses. With the agent down it's as long as the agent is.

### A write token

A leaked read-write token can already deploy any image to any service, and so read that service's traffic. It can undo nothing a person did, and everything it does has a plan and shows in the timeline. Routes add this, and no more:

| It tries to | What happens |
| --- | --- |
| Claim the panel's domain, or a name under it | Refused in Validate, in the plan and in undo. A browser would offer the panel's passkeys on a name under it |
| Claim a domain and path another route has | Refused, in every environment, and within one change |
| Add a path under another environment's domain, such as `/login` | Refused: a domain lives in one environment |
| Delete another environment's route, then claim its domain | The delete applies, and shows in the timeline. The claim, for 7 days after, is `host_access` of kind `domain`, with the environment it was taken from: the token gets `needs_session`, and only a person can apply it |
| Point a route at another environment's service | Refused: a route's service is in its own environment, by name |
| Point a route at an address, or the panel's socket | There's no field for it. Upstreams come from the agent's table, or for a port on the server, `127.0.0.1` and a number |
| Point a route at a port on the server itself | `host_access` of kind `server_port`, naming the port and where it answers: the token gets `needs_session`, and only a person can apply it. Another port, domain, path, server, or `tls` is another approval, and so is going round through a delete and its undo, or a service. The plan says who can now reach it, that what's there sees them all as `127.0.0.1`, and what listens there |
| Point a route at the panel's port, the agent listener's, or the proxy's own | Refused on plan and on apply, for everyone |
| Point a route at Docker's API | Refused for everyone, on 2375 and 2376 by number, and on any other port `dockerd` listens on, by what the agent reads there, on plan and on apply: it's root, with no legitimate route. An agent that isn't root can't read another user's processes, and names what's there as a process it can't name, for a person to read |
| Put its own container on a port a person approved, while nothing's there | Allowed: publishing `127.0.0.1:8080` isn't host access, so a token can have an approved route's traffic reach its container, if the port is free. It could already deploy any image to a service. A person approves a port on the understanding that what's on it stays there |
| Believe a proxy on where requests came from | `host_access` of kind `trusted_proxy`, naming each, for a preset too: only a person can apply it. Believing less is a token's to do, and undoing that isn't |
| Trust everyone, so client addresses are forgeable | Refused for everyone in Validate: a range wider than `/8` on a private network, `/12` on the internet or `/28` in IPv6, or any spelling of everything, isn't naming a proxy |
| Serve a new domain over plain HTTP, with `tls: off` | Allowed, and the plan says traffic reaches this server unencrypted, and who it answers: this server alone, unless a proxy is named. It can already deploy any image to that service and read its traffic. The panel's domain can't be served this way, for anyone |
| Take a live domain off HTTPS | `host_access` of kind `https_off`: only a person can apply it, whether it's an edit, or a delete and a new route, in one change or two, within the undo window. The plan says HTTPS stops, what visitors see, and that browsers that saw HSTS refuse it |
| Have a plain route answer anyone | `host_access` of kind `plain_for_anyone`, for each domain: only a person can apply it |
| Put a wildcard, a placeholder, a `..` or anything else into Caddy's config | Refused: domains and paths take a short list of characters, and JSON is built from typed values |
| Claim a name that points at this server but isn't Dockwell's yet | Allowed, and Caddy gets a certificate for it. Anyone who can point a route here can do this with HTTP-01; a name someone left pointing at this server is theirs to clean up. It shows in the plan and the timeline |
| Take ports 80 and 443 from another proxy | Refused at plan time, by name |
| Move the panel's domain behind Caddy, off a certificate that works | `host_access` of kind `panel_domain`: the token gets `needs_session`, and only a person can apply it. The panel's certificate moves with it, so it can't go dark |
| Stop the proxy by removing the last route | Allowed, as removing any route is. Undo brings it back, with its certificate |
| Burn the CA's rate limit with many routes | Limited, not stopped: 1,000 routes on a server, no new domain while 50 wait for a certificate, or 100 if it plans them all before applying any, and none past 150 asked for in 3 hours, whether their routes stayed or not, as removing a route doesn't take back what Caddy asked. Each counts for 3 hours from when it was added, until its certificate comes. **That counts names with no certificate yet, not orders, so it stops a burst of names that don't resolve, and doesn't bound what a token can spend.** The same names added and removed again count once, though Caddy places an order for each add: 50 names, 10 times over, are 500 adds and none refused. A name stops counting as its certificate comes, so new names that do resolve pass without end. So a token that keeps at it can still use up Let's Encrypt's 300 new orders per account. It shows in the timeline; revoke the token and delete the routes. Existing certificates keep serving. Domains with `tls: off` from the start ask for nothing, and neither does a name with its certificate on the server, so an undo can put a route back at any of these limits |

The time limit on a takeover is the undo window: after 7 days without a route, a domain is anyone's to claim, as a site down for a week has been noticed. A finer scope for tokens, per project or environment, is what would close this further ([ADR 0016](../adr/0016-scoped-api-tokens.md)).

### Something else on the server, unprivileged

- **Reach the admin API or the panel's socket.** It can't. `ingress/run` is mode 0700, and each socket 0600, owned by Caddy's uid.
- **Read a certificate's key or the ACME account key.** It can't: `ingress` is 0700.
- **Pretend to be HTTPS to the panel over loopback.** It can't. A public `Host` from loopback gets 421, whatever `X-Forwarded-Proto` says, and on `localhost` the panel gives a plain cookie, which is what it always gave anyone on loopback. A process on the server that can reach loopback can try to sign in like anyone through a tunnel, and needs a passkey or a code to get anywhere.
- **Take a port as Caddy stops.** It can, in the moment a last route's removal frees them, if it can bind them, which takes root or `CAP_NET_BIND_SERVICE`. The next route's plan then refuses, by name. The same goes for 443 on a server whose routes take no certificate, which Caddy doesn't hold.
- **Say anything of a client, to a route that names loopback as its proxy.** It can, as anything on the server can reach loopback. A route behind a `cloudflared` on the host network names `127.0.0.1`, so that is what it believes. On a server where that matters, run `cloudflared` on a network of its own, and name that.
- **Say anything of a client, to a route that names a network's range.** It can, if it's on that network, and it can reach the route. On one a service joins with `external_networks`, that can be other projects' services. A person approves the range by name, and the plan says whatever else is in it can do the same, as it can't say what that is today or will be, so name a network the proxy has to itself.
- **Reach a route with `tls: off` that names no proxy.** It can, over loopback, as that's who it answers, and so can a service on the server's own network, which a person approved. A bridged container that isn't named can't. It could already reach what's behind it, if that's on loopback.
- **Reach what a route to a port on the server sends to.** It could already, on loopback. What the route adds is everyone else, which is why a person approves it, and the plan says so. Everyone it adds reads as `127.0.0.1`, which an app that trusts localhost will believe.
- **Reach the admin API if Caddy's last config is damaged.** It can't. `CADDY_ADMIN` keeps it on the socket with no config to read, so an empty file, which a full disk or a power cut can leave, doesn't open it on TCP.

### If Caddy itself is compromised

Its process runs as uid 65532 with `NET_BIND_SERVICE` alone, on a read-only root filesystem, with 1 GiB of memory, and sees only `ingress/data`, `ingress/config`, `ingress/run`, and `ingress/boot` read-only. It is on the server's network, as host networking needs, so it can reach loopback, and so the panel's API, where a person's session or a token is still needed. It holds every site's traffic and certificates, which no sandbox changes, and the panel's certificate while that serves, until its own comes. It isn't uid 0, because uid 0 on the host's network namespace can reach abstract unix sockets, as CVE-2020-15257 did through containerd's shim.

Its container says `no-new-privileges=false`, on a daemon that sets it for every container too, as the image's binary binds low ports with a file capability, which the setting ignores. So a process in it can gain what a file capability or a setuid binary in the image gives, as on any daemon without the setting. Its capabilities are bounded at `NET_BIND_SERVICE` whatever it runs, as all the rest are dropped, so the most it can reach for is a setuid binary, if the image has one that helps, for uid 0 with no capability. On a hardened daemon that is the one container this applies to: the keeper and every other container keep the setting.

- **Send a route's traffic elsewhere by writing what it dials.** It can: a route with one upstream dials through a file in Caddy's run directory, `upstreams/<route>`, and Caddy reads it for each request. A compromised Caddy could dial anything anyway, so this gives it nothing new. The agent reads what's there as it reads anything of Caddy's: only a regular file, of 1 KiB at most, and what isn't an address and a port reads as a hand edit, as drift, which a revert writes again.
- **Trip up the root agent with what it leaves in its directories.** It can't. Root reads and writes there only through `os.Root`, takes regular files alone, never through a link or a FIFO, and bounds each read, so a link to `/etc/shadow` isn't written through or given to Caddy's user, and a link to `/dev/zero` doesn't fill the control plane's memory. What Caddy reads as it starts is root's, in a read-only mount.
- **Give the agent a config that opens the admin API.** The agent's copy of what Caddy runs is what it puts back after damage, and it doesn't keep one whose admin API isn't on the socket.
- **Hold up the agent.** A Caddy that hangs holds up an admin call for 15 s at most, and a plan no longer than its own deadline.
- **Read the panel's own key from what's left behind.** It can't once the last route has gone. The panel's own key is in Caddy's config until Caddy has its own certificate. Its last config goes when it stops, with `autosave.json.last` from an older build, `autosave.json.damaged`, and any temp copy a crash left, and a write clears what an earlier crash left of that file. **Caddy's own key for the panel's domain stays,** under `ingress/data`, until it expires, as every certificate there does, so a later Caddy reads that one, and could serve that name with it. A test holds both halves of this.

## What comes down when

| Down | Public traffic | The panel on its domain | The panel through a tunnel |
| --- | --- | --- | --- |
| Control plane | Keeps flowing | Down | Down |
| Agent | Keeps flowing | Keeps flowing, on the control server only if its process is up | As the control plane |
| Caddy, or Docker | Down | Down | Works: it needs neither |
| Reboot, with the agent down | Back as Docker starts Caddy | As the control plane | As the control plane |
| Another proxy on the ports, as after a reboot | Down, and the API and the log name it | Down | Works |
| Caddy's last config damaged | Down until the agent puts it back, within a minute of its being up | As public traffic | Works |
| The ingress network gone, with its subnet taken | 503 for every route | Works | Works |

The tunnel, `ssh -L 7600:127.0.0.1:7600 you@server`, depends on nothing here. A passkey for the domain doesn't work on `localhost`, so you sign in with a code, a recovery code, or `dockwell recover`.

## What this doesn't do

- **Tell whether a domain is yours.** HTTP-01 proves only that a name points here.
- **Encrypt what reaches a route with `tls: off`.** That's fine on a tunnel or a private network, and not on the open internet. Who it answers is limited, by the peer on the connection, and what passes between that peer and this server isn't protected.
- **Say who a request is from, through a proxy.** A proxy's word is a claim. What it's good for depends on who else can send through it, which for Cloudflare's ranges is all its customers.
- **Protect one environment's service from another on a bridge network they share.** That's what environment networks are for.
- **Hide a hand edit.** Root can edit Caddy's config through its socket. It reads as drift, unless Dockwell can't see it: a change to something a route doesn't model, such as a new TLS policy, isn't compared.
- **Rate-limit or filter requests.** Caddy passes on what it gets.
