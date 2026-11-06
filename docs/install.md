# Installing Dockwell

One command installs Dockwell on a Linux server. Running it again upgrades in place, and `--uninstall` removes it without touching your workloads, and tells you what stays.

> `get.dockwell.dev` isn't set up yet. Until it is, use the script's address in this repository instead: `https://raw.githubusercontent.com/Dockwell/dockwell/main/scripts/install.sh`. The commands below work the same with either.

## Install the first server

Run this as root:

```sh
curl -fsSL https://get.dockwell.dev | sh
```

On Alpine, which comes without curl, use `wget -qO- https://get.dockwell.dev | sh`.

The first server becomes your control server. It runs the full build of `dockwell`: the control plane, which serves the panel and API, and an agent of its own. The panel listens on `127.0.0.1:7600`, so only the server itself can reach it. To open it from your computer, forward the port over SSH:

```sh
ssh -L 7600:127.0.0.1:7600 <user>@<your server>
```

The install ends with a one-time link, `http://localhost:7600/claim#…`. Open it through the tunnel to create the owner account with a passkey. It works once, for 24 h; `dockwell claim` on the server prints a new one. Open the panel at `localhost`, not `127.0.0.1`: passkeys need a hostname.

To serve the panel over HTTPS on your own domain instead, point the domain's DNS A record at the server, then set `DOCKWELL_DOMAIN` for the service, as in [Change settings](#change-settings). The panel then also answers on `https://<domain>`, with a certificate from Let's Encrypt, on ports 443 and 80. Passkeys belong to the address they were made on, so add one for the domain after you sign in there. [ADR 0011](adr/0011-claim-and-sign-in.md) explains both ways in. When that server gets its first route, Caddy takes those ports and serves the panel's domain too. Its address and passkeys stay as they are, and the SSH tunnel works whatever Caddy is doing ([ADR 0024](adr/0024-the-caddy-ingress-driver.md#coexisting-with-the-panel)).

## Add a server

On the control server, make a join ticket:

```sh
dockwell token create
```

Then run the install on the new server with that ticket. The new server joins and runs only the agent, with the agent build of `dockwell`, which has none of the control plane. It opens no ports, because it dials out to the control server on TCP port 7443, so make sure that port on the control server is open to it.

```sh
curl -fsSL https://get.dockwell.dev | sh -s -- --join <ticket>
```

The `sh -s --` passes the options to the script. You can set `DOCKWELL_JOIN` instead, as long as it is set for `sh` rather than for `curl`: `curl -fsSL https://get.dockwell.dev | DOCKWELL_JOIN=<ticket> sh`. A ticket works once and expires after an hour by default, so it does little harm in your shell history.

## Two builds

Each release has two builds of `dockwell`, from the same source ([ADR 0023](adr/0023-a-full-build-and-an-agent-build.md)). Both are installed as `/usr/local/bin/dockwell`:

- **The full build**, on the control server: the control plane, its own agent, the panel and every command.
- **The agent build**, on every server that joins: only the agent, with `dockwell run`, `join`, `role` and `version`. It links none of the control plane, so it idles in about a third less memory. The commands that manage the control plane, such as `dockwell token create`, `dockwell claim` and `dockwell servers list`, say they're on the control server.

The script picks the build from what the server is:

- Without `--join`, the server becomes a control server, and gets the full build.
- With `--join`, it joins, and gets the agent build. A release from before there were two builds, such as `--version 0.2.0`, has only the full build, which runs the agent too.
- A server that has joined gets the agent build when it upgrades. Servers that joined with v0.1.0 or v0.2.0 run the full build, which runs the agent too, so their next upgrade switches them to the agent build, and the script says so.
- A control server always gets the full build, and a server with the agent build that never joined, run again without `--join`, becomes a control server like any first install.

`dockwell version` says which build is installed.

## What the script does

1. It checks it runs as root on Linux, on amd64 or arm64, and works out the OS family and whether it uses systemd or OpenRC.
2. It finds the release: the latest, or the one you pass with `--version`. It picks the [build](#two-builds) for this server.
3. It verifies the release before it installs anything. It downloads cosign, at the version and SHA-256 pinned in the script, and checks that Dockwell's release workflow signed the release's checksums at that release's tag. Then it checks the archive against them. If any check fails, nothing is installed. [ADR 0010](adr/0010-install-script-verifies-with-pinned-cosign.md) explains why it works this way. Through a mirror, the checks are the same, against a copy of Sigstore's trust root pinned in the script ([below](#install-through-a-mirror)).
4. With `--join`, it runs `dockwell join` before the service ever starts, because a server that starts without an identity becomes a control server. A server that already joined keeps its control plane, and the script says which one; to move it to another, remove `/var/lib/dockwell/agent` and run the script again with a new ticket. `dockwell role` shows which role a server takes.
5. It installs the service and starts it, then waits until the panel answers, or on a server that joined, until the agent is up. If the panel doesn't answer within 30 s, it shows the service's last log lines, which say why.
6. On a control server, it prints a one-time claim link from `dockwell claim`. Once the install has an owner, `dockwell claim` exits with status 3, and the script says where the panel is instead. Any other failure of `dockwell claim` is shown, and the script fails.

It changes only these files:

| Path | What it is |
| --- | --- |
| `/usr/local/bin/dockwell` | The binary |
| `/etc/systemd/system/dockwell.service` | The service, on systemd |
| `/etc/init.d/dockwell` | The service, on Alpine's OpenRC, which logs to `/var/log/dockwell.log` |

Dockwell itself keeps its state in `/var/lib/dockwell`. A server's ingress keeps its certificates and ACME account keys in `/var/lib/dockwell/ingress`, which upgrades and reinstalls leave alone, and so does moving the server to another control plane. A route the new control plane makes for a domain replaces the one the old control plane left, and so does one for the same domain from another environment. Other routes it left go on serving until you delete them through Caddy's admin API, as nothing else knows of them.

On a control server, `/var/lib/dockwell/control/key` is the install's key. It opens every secret the database keeps, the control plane's certificate authority and the authenticator-app secrets, and the database holds them only encrypted ([ADR 0022](adr/0022-secrets-encrypted-at-rest.md)). Back it up, apart from the database: a backup of the database without the key opens none of them, and the key lost is all of them lost. `dockwell rotate-key` rotates the key that wraps every secret's own key, and the control plane can keep running while it does.

The service sets `GOMEMLIMIT=200MiB`, a soft limit on the memory the Go runtime manages. It is sized from the PRD's budget of 250 MB for the control plane and its agent, and leaves the rest for memory Go doesn't manage, such as the binary's own pages and SQLite's cache. A few minutes after starting, an idle agent on its own uses about 19 MB with the agent build, and a control server about 37 MB, most of it pages of the binary, so the limit only comes into play if memory grows far past that.

## Beside another proxy

Dockwell binds nothing on ports 80 and 443 until a server gets its first route, so it can run beside another proxy, such as 1Panel's OpenResty. To reach a service through that proxy, publish its port on loopback and point the proxy at it. A route on such a server is refused when it is planned, naming what holds the ports, and nothing changes. A server whose routes all [take no certificate](#behind-cloudflare-tunnel-or-another-proxy) holds port 80 alone, so it runs beside whatever holds 443.

If that proxy holds both 80 and 443, as 1Panel's OpenResty does, a route of any kind is refused on port 80 until it stops. If what you want is a tunnel beside it, you don't need a route: publish the service's port on loopback, and point the tunnel straight at that, such as `http://127.0.0.1:8081`. You get no route's plan or domain state for it. Move onto Dockwell's ingress, below, when you can stop the other proxy.

## Move a server onto Dockwell's ingress

When you are ready to stop the other proxy, what it serves stops answering until you add a route for each site, so do this when you can afford that, and start with one that matters least.

**1. Give each site something a route can send to.** A route sends to a Dockwell service, or a port on the server itself. A container someone else made, such as one of 1Panel's, isn't a service, so there are two ways, for each site:

- **Run it as a service.** Create it with `dockwell services create`, or from its Compose file with `dockwell compose import`, which is what you'll want in the end, with plans, undo and rollback. `dockwell compose import` takes no external network, `container_name` or `deploy.resources` yet, so a file from 1Panel's app store may need those taken out first ([#84](https://github.com/Dockwell/dockwell/issues/84)). Dockwell keeps its own names and its own volumes, so stop the old container before you start one on the same data, as two databases on one directory corrupt it, and copy what's in its volumes across if they're named ones.

  ```sh
  docker stop 1Panel-myapp-abc12
  dockwell services create myapp --app-image ghcr.io/acme/myapp:1.4 --yes
  ```

- **Leave it as it is, for now, and send to the port it publishes.** Publish its port on loopback if it doesn't already, then name that port on its route in step 4 with `--host-port`. It reaches past containers, so a person approves it, in the panel. Dockwell doesn't see or restart such a container ([#86](https://github.com/Dockwell/dockwell/issues/86)). The same goes for a process on the server that isn't a container.

**2. Find what holds the ports.** Dockwell names it when you plan a route. You can also ask:

```sh
ss -ltnp 'sport = :80 or sport = :443'
docker ps --format '{{.Names}}  {{.Ports}}'
docker ps --filter network=host --format '{{.Names}}'
```

1Panel's OpenResty is a container named like `1Panel-openresty-abc12`, on the server's own network, so it shows in the first and the last of these.

**3. Stop it, and keep it stopped.** `docker stop` isn't enough. A container with `restart: always`, as 1Panel's OpenResty has, lasts until Docker next restarts, which a reboot does, and then it can take the ports back. So change its restart policy first:

```sh
docker update --restart=no 1Panel-openresty-abc12
docker stop 1Panel-openresty-abc12
```

If something manages that container, stop it there too, as it can start it again or make it anew: for 1Panel, stop OpenResty under its installed apps, or a later upgrade there may bring it back. A proxy that isn't in a container is your init system's to stop, such as `systemctl disable --now nginx`.

**4. Add a route for each site.** Point the domain's DNS record at this server first, or just after, as the certificate comes once it does. A server the internet can't reach on 80 or 443 needs [the section below](#behind-cloudflare-tunnel-or-another-proxy) instead.

```sh
dockwell routes create www --domain app.example.com --service myapp --port 8080 --yes
# Or to what you left as it is, which stops at the plan for a person to apply in the panel:
dockwell routes create shop --domain shop.example.com --host-port 8081 --yes
```

A service on the server's own network, as `network_mode: host` runs one, takes `--host-port` too, with the port it listens on, beside `--service` to say whose route it is. Docker can't put it on the ingress's network, so a route that dials it by name is refused, and says so.

Caddy starts as a container, on the server's own network, and takes the ports. The plan names any stopped container that Docker would start again and that may take them, so you can run the same two commands on it. On a server with its panel on a domain, the first route moves that domain behind Caddy, which only a person can approve, in the panel. Its address, passkeys and sessions stay, and it keeps its certificate until Caddy has its own.

**5. If the other proxy comes back,** Caddy can't start, and your routes, and the panel's domain, stop answering. The panel still answers through an SSH tunnel. Dockwell's log names what holds the ports, with the commands above, and `dockwell servers list --json` shows it under `ingress`. Dockwell doesn't fight it for them: run those commands, and Caddy takes the ports within a minute. You can still remove a route in the meantime.

Removing the last route stops Caddy and frees the ports, and its certificates stay on the server for when a route comes back, so you can go back the way you came: set the other proxy's restart policy back, with `docker update --restart=always`, and start it. [ADR 0024](adr/0024-the-caddy-ingress-driver.md) has the details.

### What you'll see, and what to leave alone

- **`dockwell-ingress`** is Caddy. **`dockwell-ingress-keep`** only sleeps on the ingress network, so `docker network prune` and `docker system prune` can't remove it while your services are stopped. If you remove both it and the network, the agent makes them again within a minute, with each service's address as it was. If another network has taken that subnet by then, every route answers 503 until you remove that network, as the alternative is sending your users to whatever is on it. The log and `dockwell servers list --json` name it.
- **Limits:** a server takes 1,000 routes, and no new domains while 50 of its domains wait for a certificate, or once 150 have been added in 3 hours with no certificate yet, whether their routes stayed or went. Both keep a burst of names that don't resolve from using up Let's Encrypt's limit for your whole server. A domain stops counting 3 hours after you add it, or as its certificate comes. Point its DNS here to make room sooner. Deleting its route makes room among the 50 and not among the 150, as Caddy has already asked. A domain with its certificate on the server doesn't count, so you can always put a route back. To add more than 50 at once, split them into changes of up to 50. A domain that takes no certificate doesn't count.
- **Hardened Docker:** a daemon with `no-new-privileges` keeps it for everything but Caddy's container, which needs it off for the file capability it binds ports with, and says so for itself. Rootless Docker can't run Caddy, as its host network isn't the server's.
- **An older Caddy:** a new release of Dockwell can pin a newer Caddy, which a server moves to on its next route change. Until then the log says so once, and `dockwell servers list --json` shows `outdated` under `ingress`.

## Behind Cloudflare Tunnel or another proxy

A route's certificate needs the internet to reach the server on 80 or 443. A server at home or in an office, on a private address with no ports open, can't be reached that way, so its certificate stays failed. If something in front of it ends TLS, as Cloudflare Tunnel and Cloudflare's proxy do, it needs no certificate. Set `tls` to `off` on its routes:

```sh
dockwell routes create www --domain app.example.com --service myapp --port 8080 --tls off --yes
```

Caddy then serves that domain over plain HTTP on port 80, asks for no certificate, and doesn't redirect it to HTTPS, which would loop behind a tunnel. The plan says its traffic reaches this server unencrypted, which is fine on a tunnel or a private network, and not on the open internet. Its certificate reads "none". While no route on the server takes a certificate, Caddy holds port 80 and nothing else. All of a domain's routes take the same setting, so to change one, change them together in one change. A domain that has had a certificate needs a person to take it off, in the panel, as `https://` stops answering, and browsers that saw HSTS refuse plain HTTP until that runs out. The panel's own domain can't be served this way, as its passkeys and cookies need HTTPS: reach it through the SSH tunnel.

**It doesn't answer the whole internet.** Port 80 is open on every address, so someone who finds the server's address could otherwise send `Host: app.example.com` straight to it, around whatever you have in the tunnel or the proxy. So such a route answers only the proxies it names in `--trusted-proxies`, by the address on the connection, or with none, this server's own loopback. Anyone else gets what a name with no route gets, a 404, so they can't tell it's there. For a site on a private network, or a load balancer whose addresses you don't know, `--plain-for-anyone` answers anyone who can reach the port, and a person approves it, in the panel.

A real certificate with no open port, over DNS-01, is [#94](https://github.com/Dockwell/dockwell/issues/94).

### Run Cloudflare Tunnel

This follows [Cloudflare's guide](https://developers.cloudflare.com/tunnel/get-started/), which covers making the tunnel and its token. `cloudflared` connects out to Cloudflare on port 7844, so nothing on the server needs opening for it, and with what's above, a route behind it doesn't answer anyone who comes straight to the server.

**1. Run `cloudflared` on the server's own network,** where it reaches Caddy on loopback. Through Dockwell, that's a container a person approves in the panel, as it isn't kept to a bridge. `--env TUNNEL_TOKEN` alone takes the value from your shell, which keeps it out of your history:

```sh
read -rs TUNNEL_TOKEN; export TUNNEL_TOKEN
dockwell containers create cloudflared --image cloudflare/cloudflared:latest --networks host \
  --restart unless-stopped --env TUNNEL_TOKEN \
  --command tunnel --command=--no-autoupdate --command run --yes
```

**2. In Cloudflare, add a published application to the tunnel,** with your domain as its hostname and `http://127.0.0.1:80` as its service. Dockwell's routes go by the hostname, which Cloudflare passes on. Write `http://`: with `https://`, nothing here serves TLS for that name, so the tunnel fails with `tls: internal error`, or a refused connection on a server with no route on a certificate. Write `127.0.0.1`, as `localhost` may reach `::1`, which a route naming `127.0.0.1` doesn't answer. Cloudflare gives up on a response after [125 s](https://developers.cloudflare.com/fundamentals/reference/connection-limits/) with a 524, whatever this server does, so a slow request needs to be something else.

**3. Say which proxy to believe.** A route that names none works already, and answers `cloudflared` alone, as it's on this server. But every visitor reads as `127.0.0.1`, and your app reads `X-Forwarded-Proto: http`, which can have an app that insists on HTTPS redirect for ever. Naming the tunnel fixes both, and a person approves it, as whoever it names can say any visitor is anyone:

```sh
dockwell routes update www --domain app.example.com --service myapp --port 8080 --tls off --trusted-proxies 127.0.0.1 --yes
```

Your app then reads the visitor's address in `X-Real-IP` and `X-Forwarded-For`, and `https` in `X-Forwarded-Proto`. It's read from the right, so what a visitor sends in that header themselves doesn't count, unless their own address is in a range a route on this server names, as [the threat model](security/ingress.md) says. `CF-Connecting-IP` is stripped, whoever sends it: read `X-Real-IP` instead. With `127.0.0.1` named, anything else on the server can say the same over loopback. Treat this as what Cloudflare says connected to it, which is good for logs and for limits on visitors who aren't trying to get past them, and not for deciding who someone is: a Worker on someone else's Cloudflare zone reads as `2a06:98c0:3600::103`, [as Cloudflare's docs say](https://developers.cloudflare.com/fundamentals/reference/http-headers/), which all of them share.

**If it runs on a bridge instead,** give it a network of its own, not Docker's default `bridge`. A service with no networks runs on `bridge`, and a token can deploy one with no person approving. If a route names that range, that service reaches the route, even one to a port on the server, and says who its client is.

```sh
docker network create -o com.docker.network.bridge.name=tunnel0 tunnel
docker network inspect tunnel --format '{{(index .IPAM.Config 0).Subnet}}'
```

Start it with `docker run --network tunnel --add-host host.docker.internal:host-gateway`. Its service is `http://host.docker.internal:80`, and the route names the range that printed, such as `--trusted-proxies 10.77.0.0/24`. Without that, it's someone else, and gets a 404. A firewall such as ufw treats it as incoming, so allow it: `ufw allow in on tunnel0 to any port 80`. Keep everything else off that network, as anything on it could say the same. A service on a network of its environment can do this too, with that network's range and the address of its gateway, if the network isn't `internal`.

**On a network other projects share,** such as one a service joins with `external_networks`, it's the same with that network's range, from `docker network inspect NAME --format '{{(index .IPAM.Config 0).Subnet}}'`. Naming it names everything on it: each could reach the route, and say who its client is. So give the tunnel a network of its own, or run it on the server's own.

### Cloudflare's proxy, or another in front

Cloudflare's proxy needs its SSL/TLS encryption mode set to Flexible, as this server answers only over HTTP, and any other would fail. [Cloudflare says](https://developers.cloudflare.com/ssl/origin-configuration/ssl-modes/flexible/) not to use that mode for anything with logins or personal data, and the plan's warning is about this: between Cloudflare and your server is the open internet, unencrypted. Prefer a tunnel. Or, with port 80 or 443 reachable, use an ordinary route with a certificate and Full (strict).

Name its ranges with `--trusted-proxies cloudflare`, which is [the list Cloudflare publishes](https://www.cloudflare.com/ips/) as a release of Dockwell has it. The route then answers those ranges alone, and your app reads the address from Cloudflare's `CF-Connecting-IP`, as Cloudflare recommends for an origin.

Don't allowlist on that address, and don't count on a limit by it to stop someone evading it. It's good for logs, and for limits on visitors who aren't trying to get past them. Those ranges are shared by all of Cloudflare's customers, so anyone with an account can reach your server through them, and your firewall can't tell them apart. [Authenticated Origin Pulls](https://developers.cloudflare.com/ssl/origin-configuration/authenticated-origin-pull/) doesn't apply in Flexible mode.

This is inferred from [Cloudflare's header page](https://developers.cloudflare.com/fundamentals/reference/http-headers/) (last updated 28 October 2026) and [Origin Rules](https://developers.cloudflare.com/rules/origin-rules/) (last updated 14 August 2026), and hasn't been run on Cloudflare. A Worker on another zone reads as `2a06:98c0:3600::103`. One sending to a domain that isn't on Cloudflare can set the address to anything, so name the preset only for a domain Cloudflare proxies. That doesn't close it: a Worker in a customer's own zone can set the address too, and with Origin Rules' `Host` override, which that page lists for Enterprise alone, they could point a record at your server and reach your route with it.

If an address has to hold, use a tunnel, above. Cloudflare's header page names that to know traffic comes from your own zone, as Authenticated Origin Pulls can't here.

Any other proxy that ends TLS works the same way: `--tls off`, and its address in `--trusted-proxies`, and the route answers that alone. A public range wider than `/12`, or `/28` in IPv6, or a private one wider than `/8`, isn't taken, as it wouldn't be naming a proxy.

## Upgrade

Run the same command again. If a newer release is out, the script verifies and installs it, then restarts the service. A control server keeps the full [build](#two-builds), a server that has joined gets the agent build, switching to it if it joined with the full build, and your data stays where it is. If you already have the latest, in the build the server needs, it changes nothing and doesn't restart the service.

Pass `--version 0.2.0` to install a particular release instead of the latest.

## Uninstall

```sh
curl -fsSL https://get.dockwell.dev | sh -s -- --uninstall
```

This stops the service and removes the binary and the service file. Your workloads keep running: Dockwell runs apps as Docker containers and its ingress as a container of its own ([ADR 0024](adr/0024-the-caddy-ingress-driver.md)), so stopping Dockwell's service stops neither: public traffic keeps flowing, and Caddy keeps its last config across a reboot. Your data stays in `/var/lib/dockwell`, so installing again picks up where you left off.

**On a server with routes, that leaves the ingress.** Caddy goes on holding ports 80 and 443, beside the container `dockwell-ingress-keep` and the network `dockwell_ingress`, and the script says so. To get your ports back, as when you tried this beside another proxy, run it with `--remove-ingress` as well, now or later:

```sh
curl -fsSL https://get.dockwell.dev | sh -s -- --uninstall --remove-ingress
```

That removes those two containers and the network, by Dockwell's label, and takes what else is on the network off it, so your services keep running on their other networks. Your routes stop answering. Then start the other proxy again: set its restart policy back with `docker update --restart=always`, and start it. Removing your last route before you uninstall does the same, if Dockwell is still there.

To delete your data too, run `rm -rf /var/lib/dockwell`. Do it after removing the ingress: Caddy's container mounts directories under it, which Docker would make again as root the next time it restarts, and Caddy couldn't read them, so it would restart for ever. Deleting it removes the server's identity, and on a control server, the record of every server that joined it. Your services' containers and volumes stay until you remove them with `docker`.

**The logs Dockwell kept of removed containers stay too,** in `/var/lib/dockwell/agent/kept-logs`, and the script says so when there are any. They may hold secrets, and while Dockwell runs they leave after 30 days at most. With Dockwell gone nothing removes them, so delete them yourself if you won't install it again: `rm -rf /var/lib/dockwell/agent/kept-logs`.

## Options

| Option | Environment | What it does |
| --- | --- | --- |
| `--join <ticket>` | `DOCKWELL_JOIN` | Join a control plane with a ticket from `dockwell token create`, and run only the agent |
| `--version <version>` | `DOCKWELL_VERSION` | Install that release, such as `0.1.0`, instead of the latest |
| `--downgrade` | `DOCKWELL_DOWNGRADE=1` | Install a release older than the one installed, or over a build whose version it can't read, which it otherwise refuses. On a control server an older control plane doesn't start on a database a newer one has moved on, and on a server that joined, an older agent can lack what the control plane relies on |
| `--mirror <url>` | `DOCKWELL_MIRROR` | Download Dockwell from a [mirror](#install-through-a-mirror) of its GitHub releases, and check it without reaching Sigstore |
| `--cosign-mirror <url>` | `DOCKWELL_COSIGN_MIRROR` | Download cosign from a mirror of its GitHub releases |
| `--uninstall` | | Remove Dockwell, and keep your workloads and data |
| `--remove-ingress` | | With `--uninstall`, also remove Dockwell's ingress, which frees the ports it holds |
| `--help` | | Show the options |

These settings are for air-gapped servers, mirrors that re-sign releases, and tests:

| Environment | What it does |
| --- | --- |
| `DOCKWELL_RELEASES_URL` | Where releases come from, laid out as GitHub lays them out: `<url>/latest/download/` and `<url>/download/v<version>/`. It defaults to `https://github.com/Dockwell/dockwell/releases`. Unlike `--mirror`, it changes nothing else: cosign still comes from GitHub, and Sigstore's trust root from Sigstore |
| `DOCKWELL_COSIGN` | A cosign binary to use instead of downloading one. It must be the version the script pins, and the script checks its SHA-256 |
| `DOCKWELL_VERIFY_KEY` | A cosign public key to verify releases with, instead of Dockwell's release signature, for a mirror that re-signs them. It skips the transparency log, so the key's holder is all you trust. A key needs no Sigstore trust root, so such a mirror needn't serve `sigstore_trusted_root.json` |

## Change settings

The script replaces the service file on every upgrade, so put your changes somewhere it doesn't touch:

- On systemd, run `systemctl edit dockwell` to add a drop-in. To change the agent listener's address, or the URL other servers use to reach it, add `Environment=DOCKWELL_AGENT_LISTEN=…` or `Environment=DOCKWELL_ADVERTISE_URL=…`. To serve the panel on a domain, add `Environment=DOCKWELL_DOMAIN=panel.example.com`.
- On OpenRC, set them in `/etc/conf.d/dockwell`, with `export`, such as `export DOCKWELL_ADVERTISE_URL=https://203.0.113.7:7443`.

The panel itself always stays on loopback. `DOCKWELL_LISTEN` can move it to another loopback address or port, such as `127.0.0.1:7700` or `[::1]:7600`, and the script reads it from the service, so it waits for the panel there. An address beyond loopback, such as `0.0.0.0:7600`, would serve the panel over plain HTTP to the network, so `dockwell run` refuses to start with it, and the script refuses it before it installs anything. To reach the panel from elsewhere, set `DOCKWELL_DOMAIN` instead: the panel is then served over HTTPS on the domain, and its loopback address keeps working.

The control plane keeps its state in SQLite, in `/var/lib/dockwell/control/state.db`. To keep it in Postgres instead, ahead of running more than one control plane, set `DOCKWELL_DATABASE` to a `postgres://` URL before the first start, such as `postgres://dockwell@db.internal:5432/dockwell?sslmode=verify-full`. [ADR 0014](adr/0014-object-model-and-store.md) says how the two compare.

- Keep the password out of the service file, which anyone on the server can read: put it in `/root/.pgpass`, mode 0600, or set `DOCKWELL_DATABASE` in an `EnvironmentFile=` that only root can read. Dockwell never logs or prints the password.
- Commands you run beside the control plane, such as `dockwell claim`, `dockwell recover` and `dockwell token create`, need the same `DOCKWELL_DATABASE` in your shell.
- Moving an install that has started on SQLite to Postgres isn't supported yet. `dockwell run` refuses to start that way, rather than begin a second install that your servers wouldn't recognize, and it also refuses when `DOCKWELL_DATABASE` goes missing from an install on Postgres.

`dockwell run --help` lists every setting.

## What the script needs

- Debian, Ubuntu, a RHEL-family distribution or Alpine, on amd64 or arm64, with systemd, or OpenRC on Alpine. Other distributions with systemd may work, and the script says when it doesn't know yours.
- Root, and `curl` or `wget`, `tar`, `gzip` and `sha256sum`. When one is missing, the script says how to install it.
- Access to `github.com`, for the release and for cosign, and to `tuf-repo-cdn.sigstore.dev`, for Sigstore's trust root. Or a [mirror](#install-through-a-mirror) instead.
- About 200 MB free in `/var/tmp` while it runs, most of it for cosign, which must be able to run from there. If `/var/tmp` is mounted `noexec`, as hardened servers often have it, set `TMPDIR` to somewhere that allows programs, such as `/root`.

## Install through a mirror

From mainland China, and from other networks where GitHub or Sigstore is slow or out of reach, you can install from a mirror instead. Dockwell runs no mirror and no servers in China, and doesn't recommend any mirror service. You don't have to trust a mirror. The script checks everything it downloads from one, just as it checks a download from GitHub ([ADR 0013](adr/0013-mirrors-for-releases-and-images.md)):

- cosign must match the SHA-256 pinned in the script.
- Sigstore's trust root must match the SHA-256 pinned in the script. cosign checks the signature against it, so the install never contacts Sigstore.
- The checksums must carry the signature of Dockwell's release workflow at that release's tag, and the archive must match them.

If the mirror changed any of these, nothing is installed. What a mirror can still do is fail to serve you, serve an out-of-date release, or serve an older release as the latest. The script refuses to install a release older than the one installed, the latest included, unless you pass `--downgrade`, and says what it costs. To rule out an out-of-date release on a new server, pin one with `--version`.

### What a mirror serves

A mirror of Dockwell serves Dockwell's GitHub releases at its own URL, with the same paths after it:

| On the mirror | Copied from |
| --- | --- |
| `<mirror>/download/v<version>/<file>` | `https://github.com/Dockwell/dockwell/releases/download/v<version>/<file>` |
| `<mirror>/latest/download/<file>` | The same files for the newest release |

Each release has these files. The mirror needs all of them, except an archive for an architecture or a build none of your servers use:

- `dockwell_<version>_linux_amd64.tar.gz` and `dockwell_<version>_linux_arm64.tar.gz`, the full build, for control servers;
- `dockwell-agent_<version>_linux_amd64.tar.gz` and `dockwell-agent_<version>_linux_arm64.tar.gz`, the agent build, for servers that join. Releases before it, v0.1.0 and v0.2.0, have none;
- `checksums.txt` and its signature, `checksums.txt.sigstore.json`;
- `sigstore_trusted_root.json`, the copy of Sigstore's trust root that the release's script pins. When a release lacks it, or its copy doesn't match the pin, the script takes the latest release's copy instead, so the mirror needs it at least in `latest/download/`;
- `install.sh`, the install script.

cosign comes from its own GitHub releases, at `https://github.com/sigstore/cosign/releases/download/v3.1.3/cosign-linux-amd64` and `…/cosign-linux-arm64`. A mirror of those, with the same paths after its URL, goes in `--cosign-mirror`. You can instead copy cosign to the server and point `DOCKWELL_COSIGN` at it.

The mirror can be any web server or object storage bucket you copy the files to. A service that proxies GitHub downloads works too, as long as it keeps GitHub's paths after its own URL.

### Install from it

Run the install script from the mirror's latest release, and pass the mirror to it:

```sh
curl -fsSL https://<mirror>/latest/download/install.sh | sh -s -- \
  --mirror https://<mirror> --cosign-mirror https://<cosign mirror>
```

Add `--join <ticket>` to add a server, as above. To upgrade, run the same command again: the script doesn't remember the mirror.

Use the script from the same mirror. Each release's script pins the trust root that release carries, so a script from elsewhere may pin another one. When it does, the script says so and installs nothing.

If you can reach GitHub but not Sigstore, name GitHub itself as the mirror: `--mirror https://github.com/Dockwell/dockwell/releases`.

### Run your own mirror

Copy a release, and cosign, to a directory your web server or bucket serves. The checksums list every file of the release but their own signature, so this copies both builds:

```sh
v=0.2.0
release=https://github.com/Dockwell/dockwell/releases/download/v$v
mkdir -p mirror/download/v$v mirror/latest/download cosign/download/v3.1.3
(
  cd mirror/download/v$v
  curl -fsSLO "$release/checksums.txt"
  curl -fsSLO "$release/checksums.txt.sigstore.json"
  for f in $(awk '{ print $2 }' checksums.txt); do curl -fsSLO "$release/$f"; done
)
cp mirror/download/v$v/* mirror/latest/download/
for arch in amd64 arm64; do
  curl -fsSL -o cosign/download/v3.1.3/cosign-linux-$arch \
    https://github.com/sigstore/cosign/releases/download/v3.1.3/cosign-linux-$arch
done
```

When a new release comes out, copy it the same way, and replace what's in `latest/download/`. Keep older releases, so servers can still install them with `--version`.

### Container images

Dockwell runs apps as Docker containers, and Docker on each server pulls their images the way it is set up there. To pull Docker Hub images through a registry mirror, add it to `/etc/docker/daemon.json`:

```json
{
  "registry-mirrors": ["https://<registry mirror>"]
}
```

Then run `systemctl reload docker`. Docker reloads this setting without restarting, so running containers keep running. `docker info` lists it under "Registry Mirrors". The setting covers Docker Hub only: Docker has no setting to mirror other registries, such as `ghcr.io`. Dockwell's Docker driver will add one, and pull each image by digest, so a mirror can't change what runs ([ADR 0013](adr/0013-mirrors-for-releases-and-images.md#container-images-from-m3)). For now it pulls by tag ([ADR 0021](adr/0021-the-docker-driver.md#consequences)).

A server's first route starts Caddy, as a container, and it is pulled the same way. Its image is pinned by digest, so Docker checks it whatever serves it, and a mirror can't change what runs. `registry-mirrors` covers it, as it is on Docker Hub. To pull it from any other registry, set `DOCKWELL_INGRESS_IMAGE` to the repository, such as `registry.example.com/library/caddy`; the digest stays the same ([ADR 0024](adr/0024-the-caddy-ingress-driver.md#mirrors)).

## Read it first

You can read the script before you run it:

```sh
curl -fsSL https://get.dockwell.dev -o install.sh
less install.sh
sh install.sh
```

Everything runs from one function call on its last line, so a download that is cut short runs nothing.

## How it is tested

The Install workflow ([`.github/workflows/install.yml`](../.github/workflows/install.yml)) runs on every pull request against a release snapshot built from that commit. The snapshot is laid out as releases and signed with a throwaway key, so no published release is needed.

- **Containers**: stock Debian 13, Ubuntu 26.04, AlmaLinux 9 and Alpine 3.24 images, on amd64 and arm64. Each one installs an older build, refuses a tampered archive and a bad signature, upgrades in place to the full build, runs again with nothing to do, and uninstalls, saying what an ingress leaves and removing it when asked, on a stand-in for Docker. CI's Service end to end on Docker job does that on real Docker. Then it joins a control plane, beside the key a failed first start as a control server leaves, gets the agent build and runs only the agent. Last, it joins with the older build, which stands in for a release from before the agent build and has only the full build, and upgrades: the server switches to the agent build, its agent connects again, and running the script again keeps it. systemd isn't running in these containers, so the unit is checked with `systemd-analyze verify` and the binary started the way the unit says. Alpine runs the OpenRC service for real, and uses busybox `wget` instead of curl.
- **Mirror**: in the same containers, the install and upgrade run again through a mirror on a second URL, with GitHub and Sigstore cut off. A cosign, archive or signature changed on the mirror is refused, and so is a version the mirror lacks. The test releases are signed with a key, which needs no trust root, so the upgrade comes from a mirror that carries none. The script's own steps that find the trust root on a mirror are tested on their own, through a mirror that answers 404 for a missing file, one that answers 403 as storage buckets do, and a `file://` path: a release without its own copy takes the latest release's, a changed copy is refused, and the latest release's copy is fetched once. A keyless signature from Sigstore's public instance verifies offline against the pinned trust root.
- **systemd**: the same steps on the runner's own Ubuntu 24.04, where systemd is running, so the service really starts, restarts and stops.
- **Install time**: every night, an install to a running panel with the install and the service held to one CPU, which fails at 60 s. It runs once with main's snapshot, and once with the latest published release, checked as a server would check it.

The Go tests, which `make check` and CI run, test the script's own steps that pick the build ([`internal/cli/install_script_test.go`](../internal/cli/install_script_test.go)): against data directories of each kind of server, with a stand-in for the installed binary that is run once, and against `dockwell run`'s own test for a control plane.

Run the container tests locally with `make install-test`, which needs Docker and cosign v3. The scenario itself is [`scripts/install-test/run.sh`](../scripts/install-test/run.sh).

The Sigstore trust root workflow ([`.github/workflows/trust-root.yml`](../.github/workflows/trust-root.yml)) checks that the copy the script pins is the one Sigstore publishes through TUF now, and that it verifies keyless signatures with cosign cut off from the network. Once there is a published release, that includes Dockwell's latest release. Sigstore changes its trust root on its own schedule, so the check doesn't run on every pull request, where one change would fail them all. It runs nightly, on pull requests that change the install script, its tests, the pin, or the workflows that use them, and in the release workflow before a release is published. A failed nightly run opens an issue, or comments on the one already open.
