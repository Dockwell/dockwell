# Threat model: public traffic through the ingress

Every request a server takes from the internet goes through its ingress, so this is the part of Dockwell the most people can reach. [ADR 0024](../adr/0024-the-caddy-ingress-driver.md) is the design. This says what it protects, from whom, and what it doesn't.

## What's worth protecting

- **The panel.** It controls root on every server. Its session cookie, its passkeys' RP ID, and the rule that plain HTTP never reaches it beyond loopback ([sign-in](sign-in.md)).
- **A domain's traffic and certificate.** Whoever a domain's route sends to reads that site's cookies, passwords and users' requests.
- **Caddy's admin API.** It can rewrite any route, and so everything above.
- **Certificates and ACME account keys** in `/var/lib/dockwell/ingress/data`.

## Who we consider

| Who | What they have |
| --- | --- |
| Anyone on the internet | Requests to ports 80 and 443, with any `Host`, SNI and headers |
| A compromised app | Code in a container on a bridge network, and on the ingress network if it has a route |
| A write token, leaked or careless | `ProposeChange` and `ApplyChange` for what isn't destructive and gives no host access ([ADR 0016](../adr/0016-scoped-api-tokens.md)) |
| Something else on the server, unprivileged | Loopback, any port it can bind, and files others can read |
| A signed-in person, root on the server | Everything. Nothing here defends against them |

## What each can and can't do

### Anyone on the internet

- **Pretend to be HTTPS to the panel.** They can't. The panel's proxied listener is a unix socket, and a request it takes is HTTPS because of that listener. No header decides it. `X-Forwarded-For` is what Caddy wrote: with no `trusted_proxies`, it overwrites what a client sends.
- **Reach a site by another name.** A `Host` that isn't the TLS name gets 421 (`strict_sni_host`), and a name with no route gets a TLS alert: nothing is served for it, and no certificate is asked for, as on-demand issuance is off.
- **Burn the CA's rate limit.** They can't trigger issuance: Caddy issues only for the names in routes.

### A compromised app

- **Reach another environment's service through the ingress network.** It can't: `enable_icc: false` stops containers on that network reaching each other. CI's Docker job checks this on a real engine. On a shared bridge network it could, as it always could: use an environment's own networks to keep services apart.
- **Get out through the ingress network.** It can't: the network is `internal`, with no gateway. A service on an `internal` environment network stays cut off, and no container's default route moves.
- **Reach Caddy's admin API, or the panel's socket.** Neither is on TCP. Both are in `ingress/run`, which no container may mount, as it is under Dockwell's data directory ([ADR 0021](../adr/0021-the-docker-driver.md#host-access)).
- **Reach the panel over loopback.** A bridged container has no route to the server's loopback.
- **Reach Caddy.** It can, on the bridge's gateway, ports 80 and 443, as anyone can. It gets what anyone gets.
- **Take another service's address.** A container on its own can't name `dockwell_ingress`, and a service's networks are its environment's. A person with root could; see below.
- **Keep serving after a reboot, as something else.** Addresses are static, so Docker gives a container its own address or fails to start it. Caddy never sends one service's traffic to another because they started in another order.

### A write token

A leaked read-write token can already deploy any image to any service, and so read that service's traffic. It can undo nothing a person did, and everything it does has a plan and shows in the timeline. Routes add this, and no more:

| It tries to | What happens |
| --- | --- |
| Claim the panel's domain, or a name under it | Refused in Validate, in the plan and in undo. A browser would offer the panel's passkeys on a name under it |
| Claim a domain and path another route has | Refused, in every environment, and within one change |
| Add a path under another environment's domain, such as `/login` | Refused: a domain lives in one environment |
| Delete another environment's route, then claim its domain | The delete applies, and shows in the timeline. The claim, for 7 days after, is `host_access` of kind `domain`, with the environment it was taken from: the token gets `needs_session`, and only a person can apply it |
| Point a route at another environment's service | Refused: a route's service is in its own environment, by name |
| Point a route at an address, or the panel's socket | There's no field for it. Upstreams come from the agent's table |
| Put a wildcard, a placeholder, a `..` or anything else into Caddy's config | Refused: domains and paths take a short list of characters, and JSON is built from typed values |
| Claim a name that points at this server but isn't Dockwell's yet | Allowed, and Caddy gets a certificate for it. Anyone who can point a route here can do this with HTTP-01; a name someone left pointing at this server is theirs to clean up. It shows in the plan and the timeline |
| Take ports 80 and 443 from another proxy | Refused at plan time, by name |
| Stop the proxy by removing the last route | Allowed, as removing any route is. Undo brings it back, with its certificate |
| Burn the CA's rate limit with many routes | Possible: 50 certificates per registered domain each week at Let's Encrypt, and each failure is one more attempt. Revoke the token and delete the routes; existing certificates keep serving |

The time limit on a takeover is the undo window: after 7 days without a route, a domain is anyone's to claim, as a site down for a week has been noticed. A finer scope for tokens, per project or environment, is what would close this further ([ADR 0016](../adr/0016-scoped-api-tokens.md)).

### Something else on the server, unprivileged

- **Reach the admin API or the panel's socket.** It can't. `ingress/run` is mode 0700, and each socket 0600, owned by Caddy's uid.
- **Read a certificate's key or the ACME account key.** It can't: `ingress` is 0700.
- **Pretend to be HTTPS to the panel over loopback.** It can't. A public `Host` from loopback gets 421, whatever `X-Forwarded-Proto` says, and on `localhost` the panel gives a plain cookie, which is what it always gave anyone on loopback. A process on the server that can reach loopback can try to sign in like anyone through a tunnel, and needs a passkey or a code to get anywhere.
- **Take a port as Caddy stops.** It can, in the moment a last route's removal frees them, if it can bind them, which takes root or `CAP_NET_BIND_SERVICE`. The next route's plan then refuses, by name.

### If Caddy itself is compromised

Its process runs as uid 65532 with `NET_BIND_SERVICE` alone, on a read-only root filesystem, and sees only `ingress/data`, `ingress/config` and `ingress/run`. It is on the server's network, as host networking needs, so it can reach loopback, and so the panel's API, where a person's session or a token is still needed. It holds every site's traffic and certificates, which no sandbox changes. It isn't uid 0, because uid 0 on the host's network namespace can reach abstract unix sockets, as CVE-2020-15257 did through containerd's shim.

## What comes down when

| Down | Public traffic | The panel on its domain | The panel through a tunnel |
| --- | --- | --- | --- |
| Control plane | Keeps flowing | Down | Down |
| Agent | Keeps flowing | Keeps flowing, on the control server only if its process is up | As the control plane |
| Caddy, or Docker | Down | Down | Works: it needs neither |
| Reboot, with the agent down | Back as Docker starts Caddy | As the control plane | As the control plane |

The tunnel, `ssh -L 7600:127.0.0.1:7600 you@server`, depends on nothing here. A passkey for the domain doesn't work on `localhost`, so you sign in with a code, a recovery code, or `dockwell recover`.

## What this doesn't do

- **Tell whether a domain is yours.** HTTP-01 proves only that a name points here.
- **Protect one environment's service from another on a bridge network they share.** That's what environment networks are for.
- **Hide a hand edit.** Root can edit Caddy's config through its socket. It reads as drift, unless Dockwell can't see it: a change to something a route doesn't model, such as a new TLS policy, isn't compared.
- **Rate-limit or filter requests.** Caddy passes on what it gets.
