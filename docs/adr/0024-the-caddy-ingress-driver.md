# 0024. The Caddy ingress driver, on the server's own network

- Status: Accepted. It changes who holds ports 80 and 443 on a control server with a domain, from [0011](0011-claim-and-sign-in.md), once that server has a route. [0030](0030-routes-behind-a-tunnel-or-a-proxy.md) adds routes behind a tunnel or a proxy. [0027](0027-health-checks-rollouts-and-rollback.md) settles what it left to #28, adds `Move`, `Reachable`, `Hold` and `Routing` to `ingress.Upstreams`, has a route with one upstream dial through a file, so a rollout reloads nothing, and corrects "Reloads drop nothing": that holds for requests in flight and for kept HTTP/2 and HTTP/3 connections. A POST on a kept HTTP/1.1 connection can be lost, about 1 a reload under load, and a connection on its way in can be reset, about 4 a reload at 4,700 new connections a second.
- Date: 2026-11-04

## Context

Issue #27 asks for domain and path routes to services, with automatic HTTPS, as revisions. This is the path every user's public traffic takes. "Time to first live URL" is the product's headline metric (PRD, signature experience 1), and principle 5 says traffic keeps flowing with the agent or the control plane down.

[ADR 0005](0005-caddy-ingress.md) chose Caddy, as its own process managed through its admin API with JSON config, as a systemd unit or a container, with the config as a resource kind and OpenResty later behind the same interface. [ADR 0021](0021-the-docker-driver.md) gave services, ownership, host access and honest downtime. [ADR 0011](0011-claim-and-sign-in.md) has the panel hold 80 and 443 itself when it has a domain. [ADR 0013](0013-mirrors-for-releases-and-images.md) requires whatever a server downloads to work through a mirror and be verified.

Five things were decided before this record:

1. **No route, no Caddy.** Nothing binds 80 or 443 until a server gets its first route, so Dockwell runs beside another proxy, such as 1Panel's OpenResty.
2. **Taken ports are refused at plan time, by name,** and checked again on apply.
3. **Routes are resources with plans,** at environment level, referencing a service and its port. Certificates come from Caddy's own ACME on a single server; the fleet path, where the control plane issues them, is phase 3.
4. **Caddy's admin API is never on TCP,** and its data survives upgrades and reinstalls.
5. **Traffic keeps flowing when the agent or control plane is down.** Caddy keeps its last config and starts with it after a reboot; the agent reconciles when it comes back; hand edits read as drift.

## Decision

### Caddy runs as a container on the server's own network

| | Container, `--network host` | systemd unit or OpenRC service | Container, published ports |
| --- | --- | --- | --- |
| Client address, IPv4 | Real | Real | Real, through DNAT |
| Client address, IPv6 | Real | Real | The bridge's gateway, unless the daemon has IPv6 and `ip6tables`: Docker's `docker-proxy` forwards `[::]:443` to the container's IPv4 address, so every IPv6 visitor looks like one client |
| HTTP/3 on UDP 443 | Direct | Direct | Through conntrack or `docker-proxy` |
| Alpine with OpenRC | Same as everywhere | A second service definition, and a binary of a libc that fits | Same as everywhere |
| Delivery and checks | Docker pulls by digest and checks it, through any registry mirror | Dockwell's own download, checksum and mirror path | As host |
| Survives a reboot | Docker's restart policy | The init system | Docker's restart policy |
| Survives Docker restarting | Stops with Docker, unless `live-restore` is on, and comes back with the apps it serves | Keeps running, answering 502 with nothing behind it | Stops with Docker |
| Firewall | The server's: ufw and firewalld apply | The server's | Docker's rules bypass ufw |

**A container on the host network.** Apps already need Docker, so a route has no use without it. Host networking gives the real client address on both families and direct HTTP/3 with nothing in between, which a bridged container gets only on some daemon setups. A digest pull is verified by Docker and works through a mirror with no code of ours. A unit's only gain, surviving a restart of Docker, is worth little: the apps behind it stop too.

- **Image:** `caddy@sha256:e70b164da81ef698b3ad19af799ea6086ce6d9e48ed4079eb9fe26f6fbf14ee5`, the digest of the multi-platform index of `caddy:2.11.7-alpine`, in the agent's source (`caddy.Image`). 2.11.7 is the newest release older than the 14 days `CLAUDE.md` asks for. A new pin ships with the agent.
- **A newer pin is a plan.** An agent with a newer pin keeps the Caddy it has until a route step applies on that server. The plan says Caddy restarts, with its downtime, then replaces the container. An upgrade of Dockwell never restarts public traffic on its own, and it moves the keeper (below) no sooner: that sleeps on what it has until a route step applies, and nothing pulls an image but such a step. So a server knows when it's behind: the agent logs it once, and `Server.ingress` and `DomainServer.ingress` say `outdated`, with the version it runs and the one it pins. Moving a server without a route change is [#78](https://github.com/Dockwell/dockwell/issues/78), which starts from that, and from the image Caddy's status reports.
- **It runs with as little as it can.** All capabilities dropped but `NET_BIND_SERVICE`, a read-only root filesystem with a tmpfs at `/tmp`, a memory limit of 1 GiB with no swap, and uid 65532 when the agent is root. It doesn't run as root because a process with uid 0 on the host's network namespace can reach abstract unix sockets on the host, which CVE-2020-15257 made a way out through containerd's shim. The official image gives its binary the capability as a file capability, which `no-new-privileges` would ignore, so its container says `no-new-privileges=false`. An agent that isn't root, as on a developer's machine, runs Caddy as its own uid; CI runs the agent as root.
- **Rootless Docker is refused by name.** Its host network isn't the server's, so Caddy couldn't bind the server's ports, and the agent's `route` kind is unavailable with that reason.
- **A daemon with `no-new-privileges` runs it.** Moby sets a container's option from the daemon's, then from the container's own `SecurityOpt`, which may say `no-new-privileges=false` (`parseSecurityOpt`, in `daemon_unix.go`). So Caddy's container has that, the keeper's still says `no-new-privileges` as it needs nothing it takes away, and everything else on the server keeps the daemon's setting. CI's Service end to end on Docker job runs such a daemon. [The threat model](../security/ingress.md#if-caddy-itself-is-compromised) says what it means.
- **A daemon with `userns-remap` works.** It refuses the host network to a container in a user namespace, so Caddy's has `UsernsMode: host`, which changes nothing on any other daemon.
- **Its logs are bounded**, at 3 files of 10 MB, as Docker's default keeps them forever.

### Where Caddy keeps its data

Under `/var/lib/dockwell/ingress`, owner-only, beside `agent` and `control`, not in a Docker volume and not under `agent`:

| Host path | In the container | Holds |
| --- | --- | --- |
| `ingress/data` | `/data` | Certificates and ACME account keys |
| `ingress/config` | `/config` | Caddy's last config, `autosave.json`, which goes with the last route |
| `ingress/run` | `/run/dockwell` | The admin socket, and the panel's socket |
| `ingress/boot` | `/etc/dockwell`, read-only | The bootstrap config, and the ACME CA's roots |
| `ingress` itself | Not mounted | The agent's own: `active`, `addresses.json`, `domains.json` and `last-good.json` |

- `docker volume prune -a` removes any volume no container uses, and Caddy's container is gone while a server has no routes. A bind can't be pruned.
- `--uninstall`, upgrades and reinstalls leave `/var/lib/dockwell` alone. The install guide tells someone moving a server to another control plane to remove `agent`, so what lives there would be lost with it.
- **Uninstalling leaves the ingress running, and says so.** Traffic keeps flowing with Dockwell gone, as below, so `--uninstall` takes nothing away from it. `--uninstall --remove-ingress` removes Caddy, the keeper and the network, by their label, taking what else is on the network off it first. That frees the ports, for a proxy someone wants back. It says to do this before deleting `/var/lib/dockwell`: Caddy's container mounts directories under it, which Docker would make again as root at its next restart, and Caddy couldn't read them, so it would restart for ever.
- No container may mount Dockwell's data directory or anything under it ([ADR 0021](0021-the-docker-driver.md#host-access)), so no workload reaches any of this.
- Each bind carries `:Z`, so it works where SELinux is enforced, as on AlmaLinux. Elsewhere Docker ignores it.
- **Caddy owns `data`, `config` and `run`, and the agent is root, so what's in them isn't to be trusted.** A compromised Caddy could leave a link to `/etc/shadow` where root writes, or to `/dev/zero` where it reads, in the control plane's process. So what Caddy reads as it starts is in `boot`, which is root's and mounted read-only. What the agent must read or write in Caddy's directories goes through `os.Root`, takes regular files alone, never through a link or a FIFO, opens a directory there without waiting on one, and bounds each read: 64 MB for a config, 1 MB for a certificate, and 4 MB for each read of the TLS log, whose names it remembers up to 4,096 of, of 253 bytes at most, as DNS allows. `connect` follows a link at the admin socket, so the agent takes an answer there only from Caddy's user, as `SO_PEERCRED` says of the connection, and it logs who answered instead. Whatever stands where the panel's socket goes is removed inside the directory before the panel listens, as a directory there would otherwise stop the control plane starting, and the SSH tunnel with it. A Caddy that keeps putting something back can still win the moment before the panel listens, so a failed listen there warns, and goes on without the panel's domain, and is tried again every 5 s.

### The admin API is on a unix socket, and Caddy resumes its last config

- The socket is `unix//run/dockwell/admin.sock|0600` in a directory of mode 0700. Only root and Caddy's own uid reach it. The container is given the socket's directory, and the bootstrap config names the socket. Without that Caddy's default admin address is `localhost:2019` on the host's network, where any process on the server could load a config that serves `/data`.
- **`CADDY_ADMIN` names the socket too, in the container's environment.** A config says where the admin API goes, and a config that says nothing leaves it on Caddy's default. An empty last config is one of those: `--resume` takes it over the bootstrap, as it should take any last config. A test holds this on real Caddy.
- Caddy runs `caddy run --config /etc/dockwell/bootstrap.json --resume`. A last config wins over the bootstrap. Caddy writes one on every change, so a reboot brings back the last config with no agent and no control plane.
- The restart policy is `unless-stopped`. The agent starts a Caddy someone stopped by hand once it next reconciles, unless something else now holds its ports.

### A damaged last config

Caddy saves its config with a truncating write, so a full disk or a power cut can leave it empty, cut short, or as a block of zeros. An empty one starts Caddy with no routes. One cut short makes it exit, and Docker start it again, for good. Nothing else on the server knows what it served, so the agent keeps a copy of its own, `ingress/last-good.json`, root's alone and written so it survives a power cut, after each change and each minute. It won't keep a config whose admin API isn't on the socket, so a copy can't be what puts it back on TCP.

Each reconcile puts things right before it reads anything, as a Caddy that started with nothing would otherwise read as a server with no routes, and be stopped:

| | What the agent does |
| --- | --- |
| Caddy runs a config, and its file is damaged | What it runs is the truth, and it goes back in the file, as only Caddy's next start is at risk. A file that looks damaged is read again a second later, as Caddy truncates before it writes |
| Caddy runs nothing, and came up from an empty file | The copy is loaded through the admin API |
| Caddy isn't answering | The copy goes in the file, for its next start, which Docker or the agent makes |
| No copy either | The file is set aside as `autosave.json.damaged`, so Caddy starts from its bootstrap rather than exit for ever. The routes read as drift, for a person to revert |

The copy can be a moment behind, so a change made just before the damage may be missing. It reads as drift.

### Routes

A route is a resource of kind `route`, in an environment, with IDs `rte_…`:

```proto
message Route {
  string domain = 1;  // app.example.com
  string path = 2;    // /api; empty for the whole domain
  string service = 3; // a service in the route's environment
  uint32 port = 4;    // the port it listens on, in its container
  string tls = 5;     // "off" when TLS ends before this server (ADR 0030)
  repeated string trusted_proxies = 6; // whose word to take on a client (ADR 0030)
  uint32 host_port = 7; // a port on the server, in place of service and port (ADR 0030)
  bool plain_for_anyone = 8; // tls off, and answers anyone who reaches port 80 (ADR 0030)
}
```

- **There's nothing else a route can name.** No upstream address, no header, no raw config: what Caddy dials comes from the agent's own table (below), or for a port on the server, `127.0.0.1` and a number, which a person approves. Caddy's JSON is built with `encoding/json` from typed values, so nothing a token sends can become config.
- **A domain** is 2 or more lowercase DNS labels, up to 253 characters, with no trailing dot. A wildcard is refused, as it needs DNS-01, which this issue doesn't have. An IP address is refused, and so are names no public CA issues for: `localhost`, `.local`, `.internal`, `.test`, `.example`, `.invalid`, `.onion`, `.alt`, `.arpa`. An international name is written in punycode. Each refusal says what to write.
- **A path** starts with `/`, and is made of segments of lowercase letters, digits, `-`, `_`, `.` and `~`, with no `.` or `..` segment and no trailing slash. It matches itself and what is under it: `/api` serves `/api` and `/api/users`, not `/apis`. Caddy's path matcher ignores case, so capitals are refused rather than left to compare differently here and there. `{`, `}`, `*` and `%` can't appear, so no Caddy placeholder or wildcard can.
- **`/` is the same as empty,** so both compare equal.
- **A route's path reaches the service as it is.** Nothing is stripped. A field to strip it can come later without changing what a route means.
- **A route names its service** (`resource.Referrer`), so the engine's checks of [ADR 0021](0021-the-docker-driver.md) apply: the service must be in the route's environment and on the route's server, it comes first in a change, and it can't be deleted while a route serves it. What a service reaches doesn't count as the route's: serving traffic to a service gives the route nothing of its server.
- **A route runs on one server,** as HTTP-01 and TLS-ALPN from two servers behind one name would each fail some of the time. A move plans the new server's step first, then the old one's. Phase 3's fleet path lifts this (below).

### Who may claim a domain

The engine, not the agent, decides, as only it sees every environment. `resource.Claimer` says what a spec claims, and `checkClaims` runs when a change is proposed and again in the transaction that applies it:

- **One domain and path, one route,** across the install, and within one change. The check counts what the change makes and deletes, so moving a route between environments in one change works.
- **One domain, one environment.** Another environment can't add a path under a domain in use. Without this rule, a token that may only touch staging could claim `/login` on production's domain, which is more of a takeover than claiming the whole name.
- **The panel's domain, and anything under it, can't be claimed.** A passkey works on its RP ID and that name's subdomains, so a browser would offer the panel's passkeys on any of them. The control plane's `route` type is given the domain and refuses it in Validate, so this holds for undo and rollback too. `dockwell run --domain` refuses a domain that a live route claims, and names it.
- **A domain another environment held in the last 7 days needs a person.** Tokens may add routes and remove them, as preview environments do. But a token could otherwise delete production's route and claim its domain from elsewhere, in two changes. So the engine reads each route's history over the undo window, and a claim on a name another environment held then is `host_access`, of the new kind `domain`. The plan says which environment held it, and a token gets `needs_session`, as for any plan that reaches more ([ADR 0021](0021-the-docker-driver.md#host-access)). Editing a route in place, or pointing it at another service in its environment, needs no person: a token can already deploy any image to that service.

### How Caddy reaches containers, and how upstreams are named

This is the seam rollouts (#28) build on.

A host-networked Caddy can't use Docker's DNS, which answers only inside containers, and a container's ordinary IP address isn't stable: after a reboot, containers start in any order and can swap addresses. With the agent down, Caddy would send one app's traffic and cookies to another. Decision 5 rules that out.

**The ingress network.** The first route on a server makes one Docker network, `dockwell_ingress`:

- **It has a subnet of its own.** Docker assigns a static address only on a network with a configured subnet, and doesn't check a configured subnet against the server's routes. So the agent makes a throwaway network, reads the free subnet Docker picked, removes it, and makes `dockwell_ingress` with that subnet.
- **`internal: true`** gives it no gateway and no outbound path. A service on an `internal` environment network stays cut off, and no container's default route moves.
- **`com.docker.network.bridge.enable_icc: false`** stops containers on it reaching each other. Only the server, which is where Caddy is, reaches them. A service on it reaches no other environment through it.
- **A container keeps it from being pruned.** `docker network prune`, which `docker system prune` runs, removes a network with no running container on it, and every routed service can be stopped at once. So `dockwell-ingress-keep` sleeps on it: Caddy's image, which the server has already, as uid 65532, with no capabilities, no mounts, a read-only root, 16 MB and no way out of a network that has none. It costs the server that sleeping process and its containerd shim, which is outside the agent's budget, as Caddy is. The agent starts it again if it stops, on the image it has, and the last route removes it.
- **If the network goes all the same, it comes back on its recorded subnet, with the table kept.** Someone can remove the keeper. Caddy's routes still dial those addresses, and a stopped container still asks for its own, so the agent makes the network again, puts each instance back, as a stopped container names the old network's ID and can't start until it comes off it, and gives out nothing new. Without this a new table would give one service an address another's route still dials.
- **If another network has that subnet now, the routes close.** Docker gives out the first free subnet, so a network made next can take it, and Caddy dials those addresses whichever network has them. It isn't made anywhere else, and no address is reused. Every route answers 503, from a route with no matcher ahead of them, so what they dial is never dialled. The panel's site stays up, as it isn't a route, and certificates go on renewing. The error and `ingress.problem` say `SUBNET_TAKEN`, name the network, and give the fix: remove it, or delete this server's routes and add them again, which starts on another subnet. Until a reconcile sees this, within a minute, the routes dial what they did, which the keeper exists to make rare.
- **No address Caddy dials is given out,** whatever the table says, and none at all if what it dials can't be read.

**An instance is one container of a service,** named `<project>/<service>/<number>`, with Compose's container number: today `env_01kat…/web/1`. The agent gives each instance a static address on `dockwell_ingress`, and keeps the table in `ingress/addresses.json`, written before it counts, as deleted volumes are.

- **Upstreams are the addresses of a service's instances.** For a route on port 8080, Caddy dials `172.30.0.2:8080`. Docker gives a container the same static address after a reboot or fails to start it, so Caddy reaches that container or nothing, with no agent there to correct it.
- **Adding a route has no downtime.** `docker network connect` works on a running container, so the service isn't recreated.
- **A replaced container keeps its instance's address.** The container driver asks `container.Endpoints`, and connects the new container before it starts.
- **The agent's reconcile puts it back** if something else recreated the container, such as `docker compose up` from an export.
- **Reading back:** a dial in Caddy's config maps through the table to a service and a port. So a swap of containers never reads as drift: the spec has the service and the port, not the addresses. A dial the table doesn't know reads as drift, and so does any other change to a route's shape.
- **A service with no routes isn't on the network,** and the last route on a server removes the network too.
- **A container on its own can't name `dockwell_ingress`,** by its name or by its ID. It could take the address of a stopped service's instance, and receive its traffic. Docker takes a network by the start of its ID as readily as by its name, so the agent looks each name up exactly before it makes a container, and refuses one that answers to another name, as it does for an environment's networks ([ADR 0021](0021-the-docker-driver.md)).

**What #28 calls** (`ingress.Upstreams`, on the agent). None of these make a plan, a revision or drift, as none changes a spec:

```go
// Endpoint reserves the address of an instance and says how a container
// joins: pass both to docker.Client.ConnectEndpoint before the container
// starts. ok is false for a service with no routes, which needs no address.
Endpoint(ctx, instance) (network string, ep docker.EndpointSettings, ok bool, err error)
// AddUpstream sends new requests to the instance too, on every route that
// serves its service.
AddUpstream(ctx, instance) error
// Drain stops sending new requests to the instance, and waits until those
// in flight finish, or ctx is done, which isn't an error: what remains ends
// with the container. A route keeps its last upstream, so drain the old one
// only once the new one is added.
Drain(ctx, instance) error
// RemoveUpstream forgets the instance and frees its address.
RemoveUpstream(ctx, instance) error
```

A rollout makes the new container as instance 2 with `Endpoint`, starts it, waits for it to be healthy, calls `AddUpstream`, then `Drain` on instance 1, stops and removes that container, and calls `RemoveUpstream`. A service without routes gets `ok == false` from `Endpoint`, and the other three do nothing, so a rollout needn't ask.

Three things are #28's to settle, and each has a place to do it:
- **Which number a service's instance has after a rollout.** `addresses.json` keys on the instance, and a route change goes on using the instances the table has. Only a service with none, on its first route, is taken to be instance 1, and the container driver asks for instance 1 when it replaces a container (`joinIngress`), which #28 replaces with the instance it is making.
- **How long `Drain` waits.** It takes a context. Long-lived streams would hold it for as long as they last, so #28 gives it a deadline, and what is left ends with the container.
- **Health.** Caddy's own health checks aren't configured. `reverse_proxy` holds a request for 3 s (`try_duration`) while its upstream refuses connections, so a brief gap doesn't show as an error, and #28 adds an upstream only once Docker calls it healthy.

### Reloads drop nothing

Every change to Caddy's config goes through its admin API, a single `PATCH` of the routes under an `If-Match` ETag, so hand-added routes aren't lost. Caddy starts the new config before it stops the old one, sharing listeners. Measured on real Caddy, against 12 reloads under load, each taking about 7 ms:

| | With Caddy's defaults | With Dockwell's config |
| --- | --- | --- |
| About 7,300 short requests | 0 failed | 0 failed |
| A slow request across reloads | Finishes | Finishes |
| A server-sent stream across reloads | Finishes | Finishes |
| A WebSocket across reloads | **Closed at the first reload** | Stays open |

So each route's `reverse_proxy` has `stream_close_delay: 24h`, as Caddy otherwise closes every upgraded connection when its config reloads, and `flush_interval: -1`, so streams flow as they're written. A WebSocket that stays open past the 24 h is closed then, and the old config it held goes with it, which bounds what reloads leave behind. `grace_period` doesn't cut a request on a reload, as it goes on in the old config until it ends (the test passes with a grace of 1 ms). It matters as Caddy stops, so it is 25 s, under the 30 s Docker waits before it kills it. A test (`TestReloadsDropNothingOnRealCaddy`) holds this on real Caddy in CI: 8 clients of short requests, a WebSocket that echoes, a stream and a slow request, through 32 config changes, of the route they use and of others, and no failure of any of them.

Caddy keeps `X-Forwarded-For` for itself: with no `trusted_proxies`, it overwrites what a client sends, so a service reads the real client. It also sets `X-Real-IP` to that address, so an app moved from OpenResty keeps working, and removes what else a client might claim, such as `Forwarded`, `True-Client-IP` and `CF-Connecting-IP` (`forgeableHeaders`). `strict_sni_host` answers 421 to a request whose `Host` isn't its TLS name. `allow_0rtt` is false, as a request in HTTP/3's early data can be replayed by anyone who captured it, and its source address isn't proven; a client waits one round trip more.

### Plans

- **What changes, and where:** a route's step has its field changes, and its server, as every step does.
- **What the step does to the ingress,** `PlanStep.ingress`, which the fingerprint covers:
  - `starts`, on a server with no routes yet, with the ports it takes;
  - `stops`, on the last route's removal, with the ports it frees;
  - `restarts`, on a newer pin;
  - `takes_ports` and `frees_ports`, as the first route that needs a certificate comes to a server whose routes needed none, or the last goes ([ADR 0030](0030-routes-behind-a-tunnel-or-a-proxy.md));
  - `panel_domain`, when the panel's domain moves behind Caddy or back. Moving behind Caddy is `host_access`, of the new kind `panel_domain`, so only a person can apply it (below). A plan to start a Caddy the panel is behind already, as during a brief outage, moves nothing, and doesn't say so, so a token's route plan stays a token's.
- **What DNS says,** `PlanStep.domain_check`: whether the domain points here, has no record, or points elsewhere, with the addresses. It never refuses, as people often set DNS afterwards, and the fingerprint leaves it out, so a record that changes between planning and applying doesn't stale a plan. A server with private addresses alone can't tell whether a public record is its own, and says so.
- **What may come back is said by name.** `PlanStep.returning_containers` lists stopped containers that Docker starts again when it next restarts, and that publish 80 or 443 or are on the server's own network, which can't be asked what they bind while stopped. Moby starts `always`, and `on-failure` with a failing exit, which `docker stop` is, whatever stopped them; it leaves `unless-stopped` stopped by hand. It never refuses, and the fingerprint leaves it out.
- **Taken ports are refused at plan time, by name,** with the consequence and the fix, and checked again on apply. The agent reads who holds 80 and 443 over TCP, and 443 over UDP, from `/proc`, and asks Docker what publishes them, since a published port with the userland proxy off has no listening socket. It names the process and, through its cgroup, its container:

  > Port 80 is held by nginx, in the container 1Panel-openresty-abc12, and port 443 by the same, so Caddy can't start on this server and this route can't serve traffic. What runs there keeps running. Stop that proxy, or publish the service's port and point that proxy at it.

  When it can't tell, as an agent that isn't root can't read other users' processes, it says which port, and how to find out: `ss -ltnp 'sport = :80'`.
- **A service can't publish 80 or 443 while the ingress holds them.** The container driver refuses it at plan time: otherwise the new container would fail to start only once the old one was gone.
- **Downtime is honest.** A new route has none. A route's update has none. A step that restarts Caddy says so, with 35 s at most: the 30 s it gets to stop before Docker kills it, for what is in flight, and 5 s to start again. Removing a route ends what it serves on purpose, which the plan says by being a removal; nothing else stops.

### A proxy back on the ports

A manual stop of a container with `restart: always` lasts until Docker next restarts, so a reboot can bring back the proxy Caddy replaced, and if it binds first, Caddy can't. 1Panel's OpenResty is that: `restart: always` and `network_mode: host`.

- **Reconcile says who.** It names what holds each port, its container, and the fix, with `docker update --restart=no`. The log says it when it changes, and again each hour.
- **It doesn't fight.** With the ports taken it leaves Caddy's container as it is, rather than make it again each minute. Docker's restart policy has Caddy take them if they come free first, and reconcile starts it within a minute otherwise.
- **The API says it.** `Server.ingress` and `DomainServer.ingress` have `problem: PORTS_TAKEN`, the message, and `holders`, so the panel can say why a domain whose certificate reads as issued doesn't answer.
- **A route can always go.** Removing one never starts Caddy: with it down, the route comes out of its last config and the agent's copy, and Caddy serves neither when it next starts.

### What a server's routes can ask for

A write token can add any route, so what routes cost is limited per server, past what any server needs.

- **1,000 routes.** Caddy's memory grows with them, 153 MB at 1,000 and 486 MB at 5,000, as measured, and each change rewrites them all. A plan that takes a server past that is refused, with what to do. It counts what it removes, so swapping a route at the limit passes. Editing or removing is never refused.
- **50 domains waiting for a certificate, as they come.** Let's Encrypt allows 300 new orders per account per 3 hours, and that's the one account the panel's domain needs too. A route for a domain the server hasn't got, which would take it past 50, is refused, naming some of them, with what to do: point their DNS here, or delete those you don't need. What the plan takes off the server doesn't count, so fixing a typo in a domain at the limit passes. A plan that is past it alone says to split the change.
- **What a step allows.** A plan's makes run before its deletes, and a step knows only itself, so one that swaps at a limit goes past it for a moment. So a step checks at twice these, which is what plans made a while ago, which saw fewer than there are now, can reach and no further: 2,000 routes is well inside the 1 GiB below, and 100 pending well inside Let's Encrypt's 300. Planning is what holds at 1,000 and 50.
- **What pending counts:** a domain with no certificate, for 3 hours from when it was added, whether it's on its way or has failed. A domain that stays failed must not block others for ever, and 3 hours is that limit's own window, by which its first order has dropped out of the count, and Caddy retries on the staging directory, backing off. `ingress/domains.json` keeps when each was added, across restarts.
- **150 domains asked for, in 3 hours.** Removing a route doesn't take back what Caddy asked the CA, so without this, adding 50 names and removing them in turns got past both the limit above and Let's Encrypt's, 400 in the check of #76. A domain counts here from when it was added, for 3 hours, whether its route stays, goes or takes `tls: off`, until its certificate comes. It counts names with no certificate yet, not orders, and that's its stated limit: a name counted already isn't counted twice, though Caddy places another order for it, 5 for 5 adds of one name on Pebble, and a name stops counting as its certificate comes, so 500 new names that got theirs passed. A name with its certificate on the server asks for nothing, so an undo can put a route back. A removal never lowers this, so a plan and a step check it alike. It's half of Let's Encrypt's 300, which leaves the rest for the panel's domain and renewals. The refusal says when the first stops counting.
- **What that doesn't stop.** Caddy's reload starts a pending name's attempt again on the production directory, so many route changes with many names pending can still use up the limit. As measured against Pebble, 5 reloads in 5 s with 30 names pending made 1 new order, and a minute with no changes made 29.
- **1 GiB of memory.** Caddy runs at 63 MB with no routes. If it goes past this Docker starts it again, and it resumes.

### The last route

**Caddy stops and frees the ports.** The plan says so: "Caddy stops, and ports 80 and 443 are free". The container and the `dockwell_ingress` network go. So does its last config, with what a crash or an older build left of it in that directory, and the agent's copy: while the panel's certificate serves, those hold its key, and no later Caddy should read it. `ingress/data` stays, with its certificates and ACME account key, so undoing, or a new route for the same domain, reuses the certificate rather than asking the CA again. It leaves the server as it was before the first route, and a proxy that was stopped for Dockwell can start again.

### Certificate state

The agent reports each domain's state, and the API serves it (`DomainService.ListDomains`, `GET /v1/domains`). It isn't managed state, so it isn't a revision.

- **Pending, issued, failed, or none.** None is a domain whose routes take no certificate ([ADR 0030](0030-routes-behind-a-tunnel-or-a-proxy.md)). Issued means a certificate is in `ingress/data` and hasn't expired. That is read from disk, so it survives every restart. Pending and failed come from what Caddy logs as it works. Caddy has no API for them, so its `tls.obtain` and `tls.renew` loggers also write to `ingress/data/dockwell-tls.log`, which it rolls at 1 MB, and the agent reads what's new: every 5 s while a domain is pending or failed, every minute otherwise. It also says what DNS says, read every 30 s while a domain has no certificate.
- **A failure says why, in plain language, with the fix.** The agent classifies by the ACME problem type, RFC 8555's URN, as the wording varies by CA and the type doesn't, and adds what it read from DNS itself:

  | Problem | What it means | The fix it gives |
  | --- | --- | --- |
  | `dns` | No such record | Point an A or AAAA record at this server |
  | `dns`, with the lookup failing | The CA's lookup broke, such as SERVFAIL on a CAA record, with the CA's words | Check that the domain's nameservers answer; a new record won't help until they do |
  | `cant_reach_ca` | This server's own request to the CA failed, with no ACME problem type, so nothing is known of the domain | Check this server's DNS resolver and outbound network |
  | `connection` | The CA couldn't reach 80 or 443 | Open both in the firewall and at your provider; behind a tunnel or a proxy that ends TLS, set `tls` to `off` |
  | `wrong_server` | Something else answered | Point DNS here; turn off any CDN or proxy in front for now; behind a tunnel or a proxy that ends TLS, set `tls` to `off` |
  | `rate_limited` | The CA's limit, with when it lifts | Wait; Dockwell tries again by itself |
  | `caa` | A CAA record forbids this CA | Add one for Let's Encrypt, or remove it |
  | `refused` | The CA won't issue for the name | Choose another name |
  | `other` | Anything else, with the CA's words | Dockwell keeps trying |

  A cancelled attempt, as each reload cancels one and starts again, isn't a failure.
- **Fixing DNS works at once.** Caddy backs off, up to 6 hours between tries, so someone who fixes DNS could wait a long time. A reload starts a pending name's attempt again within about a second, as measured. So while a domain is pending or failed, the agent checks its DNS, and reloads once when the answer changes to this server. A reload made for any other reason does the same, which is one more attempt on the CA's production directory; Caddy's own retries go to the staging directory first, so failures don't use up the production limit.
- **The panel can poll.** `ListDomains` takes an environment or a service, and returns each domain with its routes and each server's certificate and DNS state. A server that's offline shows what it last reported, marked `stale`.
- **The panel's own domain** isn't a route, so it isn't listed. The agent logs where its certificate stands whenever that changes, as someone who can't reach the panel by its domain reads the server's log, through the tunnel's SSH.

Issuance is Caddy's own, HTTP-01 and TLS-ALPN-01, from Let's Encrypt unless `DOCKWELL_ACME_DIRECTORY_URL` names another CA, as for Pebble, the same setting as the panel's. `DOCKWELL_ACME_ROOTS` names a PEM file of roots to trust for it. No email is sent to the CA.

**The seam for phase 3.** `ingress.Proxy.PutRoute` takes a route, not a certificate. In a fleet, the control plane will issue, and the agent will load what it's given into Caddy's `tls.certificates`, with a policy that stops Caddy issuing for those names. `Certificates` keeps reporting their state. A route's one-server rule lifts then. Nothing else moves.

### Coexisting with the panel

With `DOCKWELL_DOMAIN`, the panel holds 80 and 443 through autocert. Only one process can. When such a server gets its first route, Caddy takes them and serves the panel's domain too.

**The handover** is part of the first route's step, and its plan says so (`panel_domain`):

1. Caddy's image is pulled, with nothing stopped yet, as that can take minutes.
2. `ingress/active` is written, which tells `dockwell run` not to take the ports on its next start.
3. The panel's HTTPS and HTTP listeners stop, gracefully.
4. Caddy starts, with the panel's site and the route.

If Caddy can't start, the marker goes, the panel takes the ports back, and the step fails. Removing the last route does the reverse. These run in one process, as the control server's agent is in-process with its control plane. A server that joined has no panel, so none of this applies there. A panel with `--https-listen` and `--http-listen` on other ports, as in CI, holds neither 80 nor 443, and stays as it is.

**Nothing about signing in changes.**
- **The RP ID stays the domain,** so passkeys keep working. A passkey is bound to the name, never to a certificate.
- **Sessions stay.** The cookie stays `__Host-dockwell_session`.
- **The certificate goes with it.** The existing one stays in `control/acme`, and is used again if the panel takes the ports back. Caddy serves it too, through `tls.certificates.load_pem`, as soon as it starts, and asks for its own, as `automatic_https.ignore_loaded_certificates` lets it. Within a minute of its own coming, the agent drops the panel's, key and all, from Caddy's config, its last config and the agent's copy. So moving behind Caddy can't take a working panel dark, even if Caddy can't get a certificate, until the panel's expires: HTTPS on it is down only for the moment the ports change hands. It goes in the config rather than a file, as a file gone missing would stop Caddy starting. Tying together autocert's and Caddy's storage formats isn't needed. A real test holds this against Pebble. A handshake on the panel's domain can fail with `internal error` as the loaded certificate goes and Caddy's own takes over, for about a millisecond: 2 of 3,825 in the one run of 12 that caught it, hammering it. It happens once for a domain, and a browser's next attempt works, so it stays.
- **Only a person can do this.** [ADR 0016](0016-scoped-api-tokens.md) says a token can't cut off access, and this moves what the panel's domain depends on, so a token gets `needs_session`. Taking the ports back isn't: a token can remove a route, and the panel still has its certificate. A step applied on a plan that didn't say so, as one made while its server wasn't reachable, refuses rather than move the panel.

**The panel's proxied listener is a unix socket, and that is the whole of why it is trusted.**

The panel decides that a request is HTTPS by `r.TLS`, and refuses a public `Host` from loopback with 421, so a loopback proxy that sends `X-Forwarded-Proto` is exactly what [ADR 0011](0011-claim-and-sign-in.md) refuses. That rule stays. Anything on the server can reach loopback, and so can a container on the host network. If a header could say "this was HTTPS", any of them could say it, get a `Secure` session cookie's treatment, and pass the `Origin` checks as the panel's own site.

So no header decides anything:

- **`ingress/run/panel.sock` is a listener of its own,** mode 0600, owned by Caddy's uid, in a directory of mode 0700. Only root and Caddy reach it. No container can: Dockwell's data directory can't be mounted.
- **What arrives on that socket is HTTPS traffic for the panel's domain, because of which listener took it.** A wrapper on that listener alone (`authapi.ViaIngress`) marks the request as TLS, and sets `RemoteAddr` from the `X-Forwarded-For` Caddy wrote, which it overwrites whatever a client sends. `Host`, `Origin` and `Sec-Fetch-Site` pass through as the browser sent them, and the checks of ADR 0011 run on them unchanged.
- **The loopback listener never reads a forwarded header.** A request there with `Host: panel.example.com`, `X-Forwarded-Proto: https` and `X-Forwarded-For` still gets 421, and on `localhost` gets no `Secure` cookie. A test proves each.
- **A route can't reach the socket.** Its upstreams are addresses from the agent's table, on the ingress network's subnet.
- **Plain port 80 keeps the panel's rules.** Caddy refuses a request for the panel's domain with an `Authorization` header, or any method but GET and HEAD, as `https.Redirect` does ([ADR 0016](0016-scoped-api-tokens.md)), and redirects the rest.

**With Caddy down, the tunnel still works.** The loopback listener doesn't depend on Caddy, Docker or the ports. `ssh -L 7600:127.0.0.1:7600 you@server` reaches `http://localhost:7600` as it always did. A passkey for the domain doesn't work on `localhost`, so you sign in with a code, a recovery code, or `dockwell recover`, which now prints the link for the tunnel beside the one for the domain. `dockwell run` logs the tunnel command when it starts with the ports handed over. To give the panel its ports back with Docker gone for good, remove `ingress/active` and restart Dockwell.

**Changes to [ADR 0011](0011-claim-and-sign-in.md).** Its "the same handler also serves HTTPS on `:443`" and "plain `:80` redirects" hold until the server's first route, and again after its last. Its consequence that a loopback proxy is refused stands, and "behind your own reverse proxy isn't supported" stands. This is Dockwell's own proxy, on a listener of its own.

### Traffic with the agent or the control plane down

- **Control plane down:** nothing changes. Certificates renew: Caddy does that itself.
- **Agent down:** nothing changes, as above. Reloads stop until it's back.
- **Reboot, with or without the agent:** Docker starts Caddy, Caddy resumes its last config, and instances come back on their static addresses.
- **The agent comes back:** it puts right a damaged last config, starts Caddy if it was stopped and its ports are free, makes the network again if it has gone, connects any instance not on the network, and takes the ports back for the panel if there are no routes. It doesn't touch a route's config: a hand edit reads as drift, for a person to revert or adopt (ADR 0009). When Caddy is down, `Observe` reads its last config from `autosave.json`.
- **A Caddy that hangs,** as under `docker pause`, accepts a connection and never answers. Each admin call has a bound of 15 s, after which the agent reads its last config, as for a Caddy that's down. A change refuses, with what to do, as it would write its config back when it wakes. What waits for the manager's lock gives up at its own deadline, so a plan isn't held up.

### The interface

`internal/ingress` is what stays when OpenResty arrives: the routes and instances table, port checks, DNS checks, limits, the network and its keeper, the handover, and `Upstreams`. `ingress.Proxy` is all it knows of a proxy: get ready, start and stop, read routes, put and delete a route, close the routes, recover a damaged config, set the panel's site, how many requests an upstream has in flight, certificates, and retrying them. `internal/ingress/caddy` implements it, with its JSON and its Docker container. `internal/resource/route` is the kind, and names no proxy. OpenResty is a second `Proxy`, and a choice on the server.

### Mirrors

Docker verifies a digest pull whatever serves it, so a mirror can't change what runs. Docker's `registry-mirrors` covers Docker Hub, where this image lives, so that setting is enough for most. For any other mirror, `DOCKWELL_INGRESS_IMAGE` names the repository, such as `registry.example.com/library/caddy`; the agent pulls `<repository>@<pinned digest>`, and only the repository changes. The revisioned mirror map of ADR 0013 replaces that when it comes.

## Consequences

- **Memory.** Caddy is outside the agent's budget, as ADR 0005 says: 63 MB in the run above. `internal/ingress`, `internal/ingress/caddy` and `internal/resource/route` are in the agent build, and bring no module it didn't link. An idle agent of the agent build reads 20.4 MB, 0.6 MB more than `main` at 2177586, and a control plane with its own agent, of the full build, 39.9 MB, 1.1 MB more: medians of 5 runs after 180 s, each beside the other on one machine (ADR 0018). `dockwell-ingress-keep` is outside the budget too, and nothing here measures it, as no machine this was written on has Docker. The agent build's binary is 0.5 MB larger.
- **Docker is required for ingress.** A server without it can't have a route, which is no loss: it has no service to route to either.
- **An app must listen on every address in its container,** as it must for Compose's ports. One bound to its first interface alone isn't reachable from the ingress network.
- **The server's firewall applies to 80 and 443.** A published Docker port goes around ufw; Caddy doesn't. A blocked port shows up as `connection`, with the fix.
- **A route's first request can't beat its certificate.** Until it's issued the domain doesn't answer over HTTPS, and `http://` redirects to it. That is why the state is on the API.
- **What's not here:** wildcards and DNS-01 ([#94](https://github.com/Dockwell/dockwell/issues/94)), path stripping, per-route settings such as headers or auth, TCP and UDP routes, several servers behind one name, an email for the CA, and hand-editing a site. Each can be a new field or a new kind.
- **Replacing a service without #28 still has the downtime ADR 0021 states,** though Caddy now holds requests for 3 s while the upstream refuses connections, and answers 502 after.
- **Docker's `iptables: false`** leaves the ingress network's rules out, so `internal` and `enable_icc` don't hold, and containers on it can reach each other. Someone who turned that off has taken on the server's rules themselves.
- **An upgrade of Dockwell doesn't move a server to a newer Caddy on its own,** so a security release of Caddy reaches a server on its next route change. It shows in the log and on the API until then. Applying it without one needs a way to ask, which is [#78](https://github.com/Dockwell/dockwell/issues/78).
- **A stale route from another control plane goes when a route takes its domain.** The engine lets one route hold a domain and path, and one environment a domain, across an install, so what can't stand beside the new route is what was left. `PutRoute` takes it out in the same reload, and logs it, and `Verify` fails if another route still comes first. Stale routes for other domains go on serving until someone deletes them, as nothing here knows of them.
- **The threat model** is in [docs/security/ingress.md](../security/ingress.md).

## Tests

- **Rules:** every refusal, on its own (`route_test.go`), and the engine's claims on both stores, SQLite and Postgres (`internal/revision/claims_test.go`).
- **The manager,** on a fake Docker Engine and a fake proxy (`internal/ingress`): ports, the network, the handover and its failures, drift, reconciling, the rollout seam and certificate state.
- **`DOCKWELL_TEST_CADDY`** names a Caddy binary, and tests whose names end `OnRealCaddy` run it on high ports with its admin API on a unix socket, and the same bootstrap config, to prove the JSON and the reloads are real. `TestARouteFromPlanToRequestAndBackOnRealCaddy` runs the route kind, the manager and real Caddy with a fake Docker Engine alone left out, whose subnet is on loopback, so a test server stands where a container would.
- **`DOCKWELL_TEST_PEBBLE`** names Pebble's binaries, and those tests get real certificates over HTTP-01 and TLS-ALPN-01, and real failures, on high ports.
- **`make docker-e2e`** runs it all on real Docker, with Caddy in its container on 80 and 443, and with `E2E_ROOT` set, Dockwell as root, as on a server, so Caddy runs as uid 65532 and its directories are given to it: taken ports refused by name, a plan naming a stopped container that comes back, the hardening of the container, the network kept to itself, a real certificate from Pebble, a request over HTTPS to a real container, the client's address on both families, a replace, traffic with Dockwell down and through a restart of Caddy and of Docker itself, a last config left empty and cut short, the keeper holding off `docker network prune`, the network lost and made again, another network on its subnet, a proxy back on the ports and a route removed while it's there, a hand edit reading as drift, the last route freeing the ports, the certificate reused, the server refusing a token a domain another environment held, and with `E2E_PANEL` set, the panel's own domain moving behind Caddy and back, with a person applying it and the server refusing a token, its RP ID, the refusals of plain HTTP and forwarded headers on loopback, and the tunnel with Caddy down. With `E2E_ROOT` it also uninstalls, and frees the ports when asked, and runs a Docker with `no-new-privileges`. What [ADR 0030](0030-routes-behind-a-tunnel-or-a-proxy.md) adds is there too. `scripts/ingress-tools.sh` fetches the binaries, each checked by SHA-256. CI runs all of these, the tests on real Caddy in its Go on Caddy job and the rest in Service end to end on Docker.
- **What only CI can show:** the container's capabilities and user on real Docker, `internal` and `enable_icc` as Docker enforces them, host networking, the read-only mount, `docker network prune` and a stopped container on a network made again, and Docker's restart policies. No machine this was written on has Docker or root.
- **What nothing here shows:** a daemon with `userns-remap`, which `UsernsMode: host` is for. The fake engine refuses as Moby's source says it does (`daemon_unix.go`), and CI's Service end to end on Docker job checks the setting reaches the container, but CI doesn't run such a daemon.
