# 0104. Adding a domain publishes it: a name from a zone, a way in by default, and Cloudflare done for you

- Status: Accepted (2026-11-17)
- Date: 2026-11-17
- Amends [ADR 0030](0030-routes-behind-a-tunnel-or-a-proxy.md) (routes behind a tunnel or a proxy), [ADR 0088](0088-certificates-by-dns-01-and-wildcard-domains.md) (DNS providers) and [ADR 0099](0099-every-kind-of-thing-an-app-needs-met-the-same-three-ways.md) (provider accounts).
- Works with ADR 0103 (an app's address follows its domain), ADR 0070 (a domain's companion), ADR 0063 (the panel's domain) and ADR 0044 (alerts).

## Context

The owner's feedback from dogfooding (#785, 2026-11-16) is about adding a domain to an app or a website. Cloudflare is connected under Integrations with a zone, which today proves certificates by DNS-01 (ADR 0088). The server runs a Cloudflare tunnel, and the panel's own domain is served through it (ADR 0063's amendment, #734). Adding `wiki.example.com` to an app then still means:

- **Typing a bare name** in a text field, with the hint "Point its DNS at your tunnel or proxy", though Dockwell knows the zone.
- **A quiz behind a fold.** A collapsed HTTPS section holds "A tunnel or a proxy in front handles HTTPS". Inside it, four cards ask how traffic arrives, which Dockwell can mostly tell.
- **Work by hand in Cloudflare's dashboard:** the tunnel's public hostname, pointing at this server, and its DNS record. The catalog's cloudflared entry says so too ("In Cloudflare, publish your domain on this tunnel…"). A sentence telling a person to do something by hand is a missing feature (design/system/README.md, interaction principle 1).

What this rests on:

- **Routes with no certificate,** answering only what's named (ADR 0030): loopback by default, a container network's range, or Cloudflare's preset.
- **Caddy answers only names it has routes for,** and gives every other name a 404 (ADR 0030). So a tunnel can send a whole zone to Caddy, and only the names Dockwell serves answer.
- **A Cloudflare API token** as a `dns.provider`, with Zone Read and DNS Edit, held on the control plane alone and limited to the `projects` it names (ADR 0088).
- **Provider accounts** in ADR 0099, which aren't built yet. Until they are, `dns.provider` is Cloudflare's credential (ADR 0099, "Moving what holds credentials today").

### What Cloudflare's API gives, read on 2026-11-17

From developers.cloudflare.com, the API reference for `cfd_tunnel` and DNS records:

- **One document per tunnel.** A remotely managed tunnel's configuration is one document: `GET` and `PUT /accounts/{account}/cfd_tunnel/{tunnel}/configurations`. Its `config` holds an `ingress` list of `{hostname, path, service, originRequest}`, a top-level `originRequest`, and `warp-routing`. The last rule catches all, such as `http_status:404`.
- **A version, but no conditional write.** The answer carries a `version`, and `source`: `cloudflare` for a tunnel managed in the dashboard, `local` for one whose routes are a file on the server. The `PUT` takes no version, and the reference names no `If-Match` or ETag.
- **Rights.** Reading takes Cloudflare Tunnel Read; writing takes Cloudflare Tunnel Edit. A tunnel's connections, with each connector's origin address, read with Tunnel Read.
- **A public hostname needs a DNS record:** a proxied CNAME from the name to `<tunnel-id>.cfargotunnel.com`. DNS records change one at a time, by ID, so writing one never overwrites another.

What the docs didn't settle was checked against a scratch tunnel and a test zone on 2026-11-17, in "Verified" below. Four points need the owner's dashboard and are listed there.

## Decision

### 1. The name: picked from a zone, checked live

**Domain** becomes a subdomain field and a zone picker: `wiki` and `.example.com`.

- **The zones** are those a connected DNS provider reaches for the app's project, and only those (ADR 0088's `projects`, section 5). "Another domain" stays in the picker for a name in no connected zone, typed whole, and behaves as today.
- **The zone itself.** A subdomain left empty is the zone's apex, `example.com`. The check line then names its companion: "example.com, with www.example.com beside it" (ADR 0070, section 7).
- **A live check as the person types,** debounced, before any plan. It extends `DomainService.CheckDomain`, which already checks a name before any route has it, with its rate limit of 60 a minute per caller, its reach and its private-address rule. It answers with a state and a word beside the field:
  - **In Dockwell:** "Free", or "Used by blog in Websites · Open it", when a route serves the name.
  - **At Cloudflare,** every record type at the name, read with the DNS credential. An admin sees "A record exists: A 203.0.113.9". Below an admin it says only "Taken at Cloudflare", with no type or content, as a proxied record's content is the origin address public DNS hides. The action is a free name next to it: "Use wiki-2".
  - **Past Cloudflare's free certificate:** "a.docs.example.com is two levels down, past Cloudflare's free certificate · Use a-docs". Cloudflare's edge certificate covers one label below the zone, so the tunnel and the proxy ways are off for a deeper name; own HTTPS stays (Verified, 2).
  - **Other records at the name stay.** Cloudflare lets a CNAME sit beside TXT and MX records, and both keep working, so they don't stop a domain. Only an A, AAAA or CNAME already there does, which is a record that exists, above (Verified, 4).
  - **In the tunnel,** the rules evaluated in cloudflared's order, first match wins: "\*.example.com in web-1 catches it first, to http://127.0.0.1:8080". The plan is then refused by name. Below an admin it says "web-1 sends it elsewhere".
- **It costs nothing at the provider:** reads only, as ADR 0099 asks of every check, inside the provider's own limits.

### 2. How it's reached: a default in one line, power one step away

The sheet states the way in as one line, decided for the person, with **Change** beside it:

> Through the tunnel web-1 on hz-1 · Change

The default, in order:

1. **Through the server's tunnel,** where the agent finds one (section 3) that a connected account manages remotely and that Cloudflare shows connected from this server.
2. **Something else in front,** where the server runs any cloudflared Dockwell can't use: managed in a file, in another account, or not connected from here. A tunnel means the owner wants the server's address kept off DNS, so the default never publishes or opens it. The reason is the line: "hz-1's tunnel is managed in a file, so Dockwell writes nothing at Cloudflare". Once slice 3 lands, the default here is Dockwell's own tunnel instead.
3. **Through Cloudflare's proxy,** where the server has a public address and runs no cloudflared. Dockwell writes a proxied A or AAAA record to the server.
4. **This server's own HTTPS,** where the server has a public address and the zone isn't proxied. Dockwell writes a DNS-only A or AAAA record, and the certificate comes as today.
5. **No way in,** where nothing above holds, as a server with private addresses alone and no tunnel: "hz-1 has no public address and no tunnel · Add a tunnel" (section 9).

**Change** opens the ways as cards: the server's tunnel, Dockwell's own tunnel (section 9), Cloudflare's proxy, own HTTPS, and **Something else in front**, which is today's `tls: off` with Dockwell writing nothing at Cloudflare. A way that can't be taken says why, by name, with its action: "Port 443 on hz-1 is held by nginx · See it" (ADR 0030 names the program). Plain HTTP for anyone stays under the route's own Advanced. Nothing about HTTPS is behind a collapsed section: the one line says what happens.

**A zone set to Flexible.** Cloudflare then reaches the origin over HTTP on port 80, and a route with a certificate would redirect it to HTTPS, a loop. The check reads the zone's SSL mode (Zone Settings Read). On Flexible, the proxy way takes `tls: off` behind the `cloudflare` preset, and the plan shows ADR 0030's amber line. **Use Full (strict)** sits beside the line, for an admin: it changes how Cloudflare reaches every proxied name in the zone, so it's its own plan, on the DNS provider (`strict_zones`), naming those names. Dockwell never flips a zone's mode as a side effect of one domain.

Each way sets the route as ADR 0030 and 0088 already define it. Nothing new on the route but what it publishes (`publish`):

| Way | DNS record Dockwell writes | The tunnel | The route |
| --- | --- | --- | --- |
| The server's tunnel | CNAME to `<tunnel>.cfargotunnel.com`, proxied | Nothing, or the zone's rule once (section 4) | `tls: off`, believing what the tunnel comes from |
| Dockwell's own tunnel | The same, to its tunnel | Its own document (section 9) | The same |
| Cloudflare's proxy | A/AAAA to the server, proxied | None | A certificate by DNS, believing `cloudflare`; on Flexible, `tls: off` behind `cloudflare` |
| Own HTTPS | A/AAAA to the server, DNS only | None | A certificate, as today |
| Something else in front | None | None | `tls: off`, as today |

### 3. Which tunnel, and proof that it's this server's

**The agent finds what runs cloudflared on its server,** read as it reads other host state:

- containers whose command is `cloudflared tunnel … run`, as the catalog's entry is;
- host processes named `cloudflared`, read from `/proc`.

**How it reads the tunnel's ID:**

- **From the connector token:** `TUNNEL_TOKEN`, `TUNNEL_TOKEN_FILE`, `--token` or `--token-file`. The connector token is base64 JSON holding the account tag `a`, the tunnel's ID `t` and a secret `s`. The agent decodes it in memory, keeps `a` and `t`, and drops `s` at once. A test runs detection on a known token and searches the agent's log and the inventory message for `s`.
- **A token file is opened inside the process's own root.** The agent opens a descriptor on `/proc/<pid>/root` and opens the file from it with `openat2` and `RESOLVE_IN_ROOT | RESOLVE_NO_MAGICLINKS` (Linux 5.6 and later), so an absolute symlink resolves inside the container, never to a host file. Where `openat2` is missing, any symlink in the path is refused (`RESOLVE_NO_SYMLINKS`'s rule, checked component by component). The opened descriptor is checked with `fstat`, never the path with `stat`: only a regular file under 4 KB is read (a connector token is about 250 bytes). After reading, the process's start time is read again, and a PID that changed hands is dropped. No error quotes what a file holds.
- **Only the token is kept from an environment.** Of `/proc/<pid>/environ` or Docker's `Config.Env`, which hold a container's other secrets too, the agent reads the `TUNNEL_TOKEN` and `TUNNEL_TOKEN_FILE` entries and nothing else.
- **A cloudflared Dockwell installed** from the catalog has `TUNNEL_TOKEN` as a sealed input on the control plane, so the control plane takes `a` and `t` from it, and the agent reads nothing.
- **From a config file** (`--config`, or cloudflared's default places: `~/.cloudflared/`, `/etc/cloudflared/` and `/usr/local/etc/cloudflared/`, as `config.yml` or `config.yaml`), where the tunnel is locally managed, opened the same safe way. The agent reads its `tunnel:` key alone and never opens its `credentials-file`.

**What it reports for each tunnel** in its inventory: the tunnel's ID, the account tag, whether it came from a token or a file, the running connector's ID (below), and how the tunnel reaches Caddy, as ADR 0030's table has it:

- `http://127.0.0.1:80` from the host's network;
- `http://host.docker.internal:80` from a network of its own, with that network's range.

**A claim isn't proof.** Anyone who can run a container can run one that claims the panel's tunnel ID, which needs no secret. So the tunnel is proved by cloudflared's own connector:

- **The connector's ID.** cloudflared's `/ready` answers `{"readyConnections": 4, "connectorId": "…"}` on its metrics address, and Cloudflare's connections list for the tunnel, read with Tunnel Read, carries the same ID for each connector (Verified, 9). The agent reads `/ready` from inside the process's network namespace, at the address `--metrics` or `TUNNEL_METRICS` names, or else cloudflared's defaults, ports 20241 to 20245 on localhost (on all interfaces in a container). A connector's ID changes each time cloudflared starts, so the agent reads it again with each inventory.
- **Addresses, where `/ready` can't be read.** The control plane matches each connection's `origin_ip` against the server's ways out as ADR 0034 knows them (`Server.network`, STUN's answer included, proven or not), in the same family, as well as `outbound_address`. The interface address alone would never match on a cloud with 1:1 NAT, where it's private.
- **Every connector, not one.** A tunnel counts as this server's way in only when every active connector is this server's. A tunnel run as replicas on two servers would send a zone's names to whichever answers, half of them to a Caddy with no route, so the sheet says "web-1 runs on hz-1 and hz-2, so Dockwell can't send a name to one of them", and the default moves on as for a tunnel connected elsewhere. The drift timer (section 4) reads the connections list too, and raises the same alert when a second server joins later.
- **Servers behind one address,** where only addresses are left to go by, can't be told apart. Then the tunnel counts as this server's only if Dockwell installed its cloudflared here; otherwise the sheet says "hz-1 and hz-2 share an address, so Dockwell can't tell which runs web-1".

Reading the tunnel gives its name and status; its configuration gives `source`.

| What's found | What the sheet says |
| --- | --- |
| One tunnel, remotely managed, in a connected account, connected from here | The default way, by its name |
| Several tunnels | Each one, those connected from here first |
| One tunnel with connectors on several servers | "web-1 runs on hz-1 and hz-2, so Dockwell can't send a name to one of them" |
| Connected from somewhere else | "web-1 isn't connected from hz-2", and the default moves on as section 2 says |
| In an account no connected token reads | "hz-1 runs a tunnel in another Cloudflare account · Connect it" |
| Locally managed (`source: local`) | "hz-1's tunnel is managed in a file, so Dockwell writes nothing at Cloudflare". Dockwell never edits that file. |
| None | The next way, as section 2 orders them |

**What the tunnel's peer needs.** A tunnel on a network of its own reaches Caddy from that network's range. The route then believes that range, which ADR 0030 makes host access, of kind `trusted_proxy`, an admin's to apply (`installWide`). So a developer's domain on such a server waits for an admin, and the plan says so. A tunnel on the host's network needs nothing, as loopback is the default.

### 4. The tunnel's document: written at most twice per zone

The tunnel's document holds the panel's hostname and the owner's other routes, and Cloudflare's `PUT` replaces all of it with no version check. So Dockwell writes it as rarely as it can: never for each domain.

**What Dockwell writes in a tunnel:**

- **Nothing,** when the routing check (below) finds the name already reaches this server's Caddy: through the catch-all, a wildcard, or a rule for the name. Adding the domain is then its DNS record alone.
- **One zone rule,** otherwise: `*.example.com → http://127.0.0.1:80` (or the tunnel's way to Caddy), inserted just before the catch-all, in the plan that adds the first such domain. It's host access of kind `tunnel_rule`, so an admin confirms it once, with the routing check's result in the plan: "Every other name in web-1, the panel's among them, goes where it went". After that, every domain in the zone writes only its DNS record.
- **One rule for the zone's names, one for its apex, never per domain.** `*.example.com` in a tunnel matches every name under the zone, at any depth (Verified, 2). It doesn't match the apex (Verified, 10), so the apex takes one rule of its own, `example.com`, the first time it's added, by the same rules as the zone rule. So a zone's tunnel is written at most twice.
- **Where the zone rule can't go in,** because the routing check finds it would move a name that isn't Dockwell's, the plan is refused by that name: "\*.example.com would move ftp.example.com, which goes to http://127.0.0.1:2121 now". The default moves to Dockwell's own tunnel (section 9), and until slice 3 lands, to Something else in front. Dockwell doesn't fall back to a rule per domain.
- **The zone rule goes** in the plan that removes the last Dockwell route published through it in that zone, by the same rules as any write.

**How every write is made.** Whatever is written, a write keeps the document exactly as it was read:

- The `PUT` body is the `config` object as raw JSON, as read, with one element spliced into `ingress`, or taken out of it. Nothing else is decoded into types and encoded again. A `PUT` replaces the whole document, and one that leaves out the top-level `originRequest` drops it; Cloudflare keeps keys it doesn't know at every level, so a typed round trip would drop them too (Verified, 1 and 3).
- **Value for value, not byte for byte.** Cloudflare returns keys in its own order, so the written document is compared with the read one as decoded JSON.
- **A check before each write,** positional: both documents are decoded again, with numbers kept exact (`UseNumber`), and the new `ingress` must equal the read one with one element inserted at the index before the catch-all, or removed at its own index, every other element equal and in the same order. Everything outside `ingress` must be equal. If anything else differs, the step refuses to write and says so. A comparison as a set would pass a reordered list, which changes routing.
- **A tunnel with no routes yet** reads `config: null` (Verified, 1), so there's no catch-all to splice before. Dockwell then writes `{"ingress": [the rule, {"service": "http_status:404"}]}`, its only write that isn't a splice, as there's nothing to keep.
- A test fixture holds every documented key, at the top and in each rule, plus keys Cloudflare doesn't have yet at each level. The test asserts the written document equals the read one, decoded, with exactly one rule added or removed. `warp-routing` can't be set through this API (it reads `{"enabled": false}` whatever is written), so it's kept as read like everything else.

**The panel's names are never touched.** A guard in the tunnel writer, apart from route validation, knows every name the panel answers to: the domain in use, one saved and waiting for its restart, the old one during a move (ADR 0063), and `DOCKWELL_DOMAIN`. It splits rules by what they do to those names:

- **A rule that serves the panel** is never added, changed or removed: one whose hostname equals or falls under a panel name, or whose service is the panel's own listener address.
- **A wildcard that only covers a panel name,** as `*.example.com` covers `panel.example.com`, may be added, but only after the rule that serves the panel today, and only when the routing check shows every panel name still matching that same rule. The owner's panel shares its zone with the apps, so this is the usual case, and the zone rule goes in after the panel's.
- **A panel served by the catch-all alone** would be caught by the wildcard, so the routing check refuses it, by the panel's name. The stand-in tests hold all three: the panel by its own rule (the zone rule goes in after it), by the catch-all (refused), and by an earlier wildcard (nothing written).

**The routing check,** before each `PUT` and in each plan:

- For every name the panel answers to, every name in the tunnel's rules, and every name in the zone whose DNS points at this tunnel (one list call with the DNS credential), it works out which rule matches before the change and after it, first match winning, with paths.
- The write is refused if any name other than the ones being added or removed changes where it goes. A name whose answer stays a 404, as a name falling from a 404 catch-all to Caddy with no route for it, counts as unchanged.
- The same check says, in section 1's live check, when an earlier rule catches a name first. Cloudflare doesn't: it accepts a rule that an earlier wildcard shadows, and traffic then goes to the earlier one (Verified, 2). Cloudflare does refuse a catch-all that isn't last, a document with no catch-all, and a `*` anywhere but the leftmost label.

**What Dockwell keeps** for each write, in the store: the tunnel, the rule as written, the document it read and the one it wrote, the version Cloudflare's `PUT` returned, and the routes it serves. Tunnel rules have no comment field, so the store is their only record.

**One writer per tunnel inside Dockwell.** Two plans can apply at once on one tunnel, as two zones' rules, or an add and an undo. Each write holds a lock keyed by the tunnel's ID, from the step's read to its read-back. The lock is a row in the store, with a lease that runs out, so two control-plane processes during an upgrade can't both hold it and a crashed one doesn't hold it for ever. A step that finds it held waits for it. So the race below is only with writers outside Dockwell.

**A write that races.** No client can make Cloudflare's `PUT` fully safe, and with at most two writes per zone it happens rarely. Dockwell narrows it and says what happened:

1. Read the document as the plan is made. The plan names the rule it adds or removes, and nothing else.
2. As the step applies, read it again. Where its content isn't what the plan read, plan the change again from what's there, at most 3 times, then fail the step, saying the tunnel's routes keep changing.
3. Write at once, then read back. Each `PUT` that changes the document adds exactly 1 to its version, and the reply carries the new one; a `PUT` that changes nothing, a refused one, and a connector connecting don't move it (Verified, 1). So a reply above the version read plus 1 means another write landed in between.
4. **The read-back holds Dockwell's rule:** the step is done, so the plan goes on, or undoes, from a known state. Where the reply's version shows another write landed between Dockwell's read and its own, the step says so as a state, with the tunnel's page linked: "An edit made in Cloudflare between 10:42:01 and 10:42:02 was replaced. Dockwell kept what it read, and that's what's there now". Cloudflare gives no time for a version, so the window is Dockwell's read and its write. Only the document read and the one written stay on the change, as the replaced one can't be read. It also goes out as an alert through ADR 0044's rules, as whoever made that edit isn't watching this plan.
5. **The read-back lacks Dockwell's rule:** another write came after Dockwell's, so nothing of the other writer's was lost. That's drift, below.

**Drift, on a timer.** The likeliest way a rule disappears is a dashboard tab opened before Dockwell wrote, saving its older document. So every 5 minutes the control plane reads:

- each tunnel holding a Dockwell rule, acting only when the version has moved, and its connections list (section 3);
- each zone's records once, filtered by the comment prefix `dockwell <install-id>`, not each record by ID.

When something is gone or changed:

- the domain's row in Domains, and the app's page, say what's gone, with **Put it back** as a plan: "Not in the tunnel any more" for a rule, "Its DNS record is gone at Cloudflare" for a record;
- one alert goes out through ADR 0044's rules, with its own condition, per tunnel and zone, not per domain, as a zone rule gone takes every domain in the zone with it;
- nothing is fixed silently.

**Cloudflare's limit.** Cloudflare allows about 1,200 calls per 5 minutes per token, shared with everything else on it. The timer above costs a few calls per tunnel and zone. The live check (section 1) caches the tunnel's document, its connections and the zone's SSL mode per tunnel and zone for about 30 seconds, so only the name's records are read per check.

### 5. Who may write at Cloudflare

**Writes follow the provider's reach.** A route's `publish` is refused for a project the DNS provider doesn't name (ADR 0088's `projects`, ADR 0099's `reach`), whichever client sends it, a project-narrowed token included. The picker lists only those zones.

**What each write needs:**

| Write | Who applies it |
| --- | --- |
| A DNS record at a free name | Whoever may add the route, as adding a domain is a developer's (ADR 0103) |
| The zone rule, or the apex's rule, in a tunnel | An admin, once each per zone: host access of kind `tunnel_rule`, behind the routing check |
| Replacing a record, taking on a record or rule Dockwell didn't make, or Move it to Dockwell | An admin with a fresh passkey: host access of kind `cloudflare_takeover`, as good as root to apply, so the session's cover doesn't stand in and a token never can |
| Use Full (strict) | An admin, on the DNS provider |

Both new kinds join `installWide` in `internal/revision/roles.go`, which makes them an admin's, and `cloudflare_takeover` joins `Access.AsGoodAsRoot` in `internal/resource/resource.go`, which is what makes applying it ask for `APPLY_ROOT_CHANGE`.

**Replace it,** for an admin, when a record isn't Dockwell's:

- The plan names the record's type and content: "Replaces wiki.example.com's A 203.0.113.9, made in Cloudflare".
- The step stores the replaced record whole (type, content, proxied, TTL and comment) before it deletes it, so undo writes it back. Undo refuses by name if the name has changed at Cloudflare since.
- It's never offered for a name the panel answers to. It replaces only the A, AAAA or CNAME at the name; TXT, MX and other records there stay, as Cloudflare keeps them beside a CNAME (Verified, 4).

**Dockwell's records say they're Dockwell's.** Each record's `comment` is `dockwell <install-id> <route-id>`. A step run again after a crash finds its record by name, type and comment; a control plane restored from backup tells its records from those made by hand; and taking on a record can tell one made by another Dockwell.

### 6. Permissions: one token, each right checked on its own

**One Cloudflare token may carry every right this needs,** and each capability is checked and shown as its own state:

| Capability | Cloudflare's permission | Shows as |
| --- | --- | --- |
| DNS, per zone (ADR 0088) | Zone Read, DNS Edit | "Can write DNS in example.com" |
| Tunnels | Cloudflare Tunnel Edit (reads with it too) | "Can publish through tunnels" |
| The zone's SSL mode | Zone Settings Read; Edit for Use Full (strict) | "Reads example.com's settings" |
| Its own rights | Account API Tokens Read (User API Tokens Read for a user's token) | "Checks its own rights" |

- **Reading its own rights is what makes the rest checkable.** Without it, the check can't tell DNS Edit from DNS Read, or Tunnel Edit from Tunnel Read, without writing, which it never does. So without it the card says "Can't check this token's rights · Add it to this token", shows what Dockwell could read (the zones, the tunnels) as checked, and shows the edit rights as unknown, never as present. API Tokens Read makes no credential, so it may share the token under the amended ADR 0099.
- **The check matches rights by their IDs.** A token's own record lists each policy's permission groups by ID and name, and its scope, the account or the account's zones (Verified, 11). The API names them "Write" where the dashboard says "Edit" (DNS Write, Cloudflare Tunnel Write, Zone Settings Write), so names are only the fallback, and the words shown are the dashboard's.
- **A token that can make credentials is said to be one.** Where its record holds API Tokens Write, or another right that makes credentials, the card says "This token can make other tokens · Make a narrower one", as the amended ADR 0099 keeps such a right off a shared token. The token used for these checks held it, and Billing Write, among about 160 rights.
- **The account** comes from each zone's own account, read with Zone Read, so nothing is typed. One token can reach zones in several accounts, so it's kept per zone (`DnsZoneCheck.account_id`), not on the provider.
- **Where the token lacks a right,** the state names it in Cloudflare's words, with **Add it to this token**: "Can't publish through tunnels: the token lacks Cloudflare Tunnel edit · Add it to this token". It opens Cloudflare's page for editing that token; its value doesn't change, so nothing is pasted again. **Check again** picks it up.
- **A second token** stays possible under the card's Advanced, for anyone who wants tunnels on a token of their own (`tunnel_api_token`).
- **An account-owned token is recommended,** as it outlives a person's membership, but not required. A user-owned DNS token that gains the tunnel right works.
- **Whether Tunnel Edit can be scoped to one tunnel** is still to check with the owner. Until it's known, the card says what the token's record shows: "Can edit the account's 3 tunnels", for a right scoped to the account.
- **The live check** reads the token's own policies by its ID, as ADR 0099's table says for Cloudflare, lists the account's tunnels with the tunnel right, and reads each zone's SSL mode (Verified, 9). It verifies an account-owned token at `/accounts/{account}/tokens/verify`, which answers its ID, as `/user/tokens/verify` refuses one, and a user's token at the latter; then it reads the token's own record by that ID (Verified, 11). It never writes a test rule into a tunnel. The control plane never fetches a tunnel's connector token (`GET …/cfd_tunnel/{id}/token`), not even in slice 3 (section 9).

**Where it shows:** on Settings → Integrations, on Cloudflare's card, and in the add-domain sheet wherever the way the default would take lacks a right: "Through the tunnel web-1 · needs Cloudflare Tunnel edit · Add it to this token".

**Amending ADR 0099.** Its rule of one credential for each capability gains this: credentials for rights that are equals over the same names may share one token. DNS Edit and Tunnel Edit are such equals, as each can already send any of the zone's names anywhere. A right that can make credentials, as IAM users or API tokens, never shares a token with anything.

### 7. The order of a change's steps, and the app's address

Publishing is part of the change that moves the app's address, so ADR 0103 applies unchanged: the same change writes the address variables. The steps run in an order that never leaves an app pointing at a name that doesn't resolve:

1. **At Cloudflare first,** each a step run by the control plane: the DNS record, and the zone rule where one is needed. Both are harmless while Caddy has no route for the name.
2. **Then the route,** on the server.
3. **Then the address variables** (ADR 0103), and the restarts they bring.

A failed Cloudflare step, from a missing right, a refused routing check or a race that can't settle, stops the change before the route and the address, and says why. **Removal runs in reverse:** the address variables go back, then the route goes, then the DNS record, and the zone rule if it was the last.

**The address's scheme** is `https` for every way here, the tunnel, the proxy and Something else in front included, though the route says `tls: off`. It's `http` only for `plain_for_anyone`.

**A domain's companion** (ADR 0070) is published in the same plan when its box is checked, as ADR 0070 leaves it to the person, with its own DNS record. `www.example.com` falls under the zone rule; the apex takes its own rule, once.

### 8. Undo, removal and what was there before

**Every write at Cloudflare is a step of the change's plan,** run by the control plane, never the agent, as ADR 0099's provider steps are:

- "Writes a CNAME for wiki.example.com to web-1's tunnel, proxied"
- "Adds \*.example.com to web-1, to http://127.0.0.1:80, once for example.com"

**Each step is idempotent:** it looks before it writes, finds a record by name, type and comment, and records a record's ID in the step's transaction. A control plane stopped midway finds what it wrote on the next try.

**Removal and undo:**

- **Removing a domain** removes its DNS record, in the same plan as the route, only while the record reads as Dockwell wrote it. One changed in Cloudflare since stays, and the plan says so: "wiki.example.com's record was changed in Cloudflare, so it stays".
- **Undo puts back** what the change took away, a replaced record included, as steps of the undo's plan.
- **Undoing a removal** where the name has been taken at Cloudflare since refuses, by name, as a route's undo does when its domain is taken.

**A domain added before this, by hand,** is taken on, not duplicated, but only on the plan that adds the domain, or with **Manage in Dockwell** on its row in Domains. Never as a side effect of another plan. Taking it on is `cloudflare_takeover`, as section 5 says, because it lets Dockwell delete it later.

- **What's taken on:** a DNS record that points at this tunnel or this server, and the tunnel's rule for the name where that rule already reaches Caddy. The plan says "Takes on wiki.example.com's DNS record and tunnel route, made in Cloudflare".
- **A rule that points at the app's own published port,** as the old catalog text had people make, gets **Move it to Dockwell**, which is `cloudflare_takeover`, as it changes a rule Dockwell didn't make. One plan adds the route and, where the zone rule already covers the name, removes the old rule, which leaves one rule fewer; otherwise it points the old rule at Caddy. Either way the old rule is kept so undo restores it.
- **A rule that points anywhere else** isn't taken on, and the domain's row says where it goes.
- **A removal plan names what was taken on:** "Removes wiki.example.com's DNS record and tunnel route, made in Cloudflare and taken on 3 November".

The panel's names are never taken on or touched, whatever points where (section 4's guard).

### 9. Dockwell's own tunnel, and adding one

When a server has no tunnel Dockwell can use, or the zone rule can't go into the one it has, Dockwell can make a tunnel of its own. This is slice 3, and it's a normal catalog install through a plan:

- **The plan** makes a tunnel at Cloudflare named for the server (`dw-<install>-<server>`) with a secret the control plane generates, builds the connector token from it, and installs the catalog's cloudflared on the server's host network with that token as a sealed input, read from a file (`--token-file`). It has undo like any app.
- **The secret is never read back.** Cloudflare's answer to making a tunnel carries `credentials_file`, which holds the secret, so that answer is never logged or kept, and nothing fetches the token (Verified, 5).
- **The secret is sealed first.** The step seals the generated secret in its transaction before the create call, as ADR 0099's provider steps seal a secret, so a retry finds the tunnel by its name and uses the sealed secret instead of making a second tunnel.
- **Rotating a tunnel's secret** through the API is unchecked, and is slice 3's first check.
- **Its document is Dockwell's alone,** so its zone rules carry no race and no other route.
- **It costs** a second cloudflared where the server runs one already, about 20 MB.
- **Removing the server** (ADR 0090) deletes its tunnel and the records that point at it. **Marking it compromised** rotates the tunnel's secret, as its connector token can serve every name routed to it.

Under Change it's a way of its own: "Dockwell's own tunnel on hz-1 · Add it". Until slice 3 lands, the no-way-in state's **Add a tunnel** opens the catalog's cloudflared.

### 10. The add-domain sheet

Decided against design/system/README.md's interaction principles, and mocked first:

- **The name:** subdomain and zone, with the live check from section 1 beside the field. On a phone, the two stack under 420 px.
- **The way in:** one line under the name, with **Change**. The reason shows only when the way isn't the first: "Through Cloudflare's proxy, as hz-2 runs no tunnel".
- **What it reaches:** the app and its port, picked, as today. From an app's page both are set: "Reaches wordpress on port 80 · Change".
- **Nothing hidden:** no collapsed HTTPS section. Advanced keeps what a power user needs: the path, `trusted_proxies`, a redirect's kind, plain HTTP for anyone, and the server for a route a domain's DNS doesn't decide. Its summary says it in the person's words: "Path / · Answers only hz-1's tunnel".
- **No instructions:** every "point its DNS at…" line goes. What a person couldn't do from the sheet is a state with an action.
- **The plan** lists the steps in the order they run (section 7): Cloudflare's, the route, then "URL follows the app's address to https://wiki.example.com, which restarts wordpress" for an entry that has one. Each Cloudflare step says where it runs, with a `PlanStep` field for a step at a provider.

**Websites** take the same domain step: the new-website sheet's domain field, and a site's Add domain, use the same name-and-way component.

**Catalog installs** take it too, as built for #792: the install sheet leads with the app's domain, a name under a connected zone suggested from the project, checked live, with the way in one line and Change; another domain typed whole, with the record it needs; or "Only on the server", the default where no zone is connected. The sheet sends the route whole in `InstallCatalogEntryRequest.route` (11). The install binds its service, port and name to the app's web page, refuses a redirect or a port on the server, and plans it in the install's change, where the hold (ADR 0105), the publish rules and a take-over's passkey apply as to any route edit. The provider that publishes it serves the new project in the same change, as a website's does, with its tokens left out so it keeps them, and the app's address follows its domain (ADR 0103). A route with a path is refused, as the app's address is its whole domain. Where the chosen server has no way in, the domain stays "Only on the server" and says why, with Add a domain a step away.

Undoing one install or website, after another has added its project to the same provider, takes out only the project its change added, as a set, and keeps every other (`resource.PartUndoer`). A change to the provider beyond its projects is still undone whole or not at all.

**The catalog's cloudflared entry** drops its "In Cloudflare, publish your domain on this tunnel…" step. The install guide says to give cloudflared its token in a file (`--token-file`, or `TUNNEL_TOKEN_FILE`), or else through `TUNNEL_TOKEN` as the entry does, never `--token`, which shows in the server's process list.

**Mocks,** in design/mocks, beside Domain-Add:

- `Domain-Add-Zone.dc.html`: an app's add-domain sheet, with the tunnel found, the name free, and the first domain in the zone, so the plan shows the zone rule.
- `Domain-Add-Ways.dc.html`: Change opened, with Dockwell's own tunnel among the ways and one way off, by name.
- `Domain-Add-States.dc.html`: the states off the happy path, each a state and one action, and the Domains row with a domain gone from its tunnel.
- `Website-New-Domain.dc.html`: the new-website sheet's domain step.
- `M-Domain-Add.dc.html`: a phone version of the first.

### 11. Port in the app

Added for #791, after the sheet above landed. Add a domain and Add a port pick the port in the app from what's known of it, and say whether it answers, instead of a bare number field.

- **What's listed:**
  - The port that serves the app's web page comes first (`ServiceInfo.web_port`).
  - Then what the spec publishes and exposes.
  - Then what the server's agent finds: what the image exposes, and what listens in the container beyond its loopback, read from the container's own `/proc/<pid>/net/tcp`.
  - Each is labelled: "3000 · web page · answers 200 OK", "22 · SSH", "9000 · nothing listens".
  - A port known for something else, such as SSH's 22 or Redis's 6379, says so under the picker, with the web page's port one click away.
- **Where the check dials from:** `ServiceService.CheckServicePorts` asks the agent with `AppPortsCall`. The agent asks from inside the container's own network namespace, never from the server's.
  - It holds the namespace open by its file from `/proc/<pid>/ns/net`, and refuses one that is the agent's own.
  - Inside the namespace, on a thread of its own, the container's address must be on one of the namespace's own interfaces. The socket is bound to the namespace's `lo` (`SO_BINDTODEVICE`), so the kernel delivers it locally or not at all.
  - So no subnet a network is given, no policy rule (Tailscale's, a VPN's) and no default route can take a dial past the container: a container whose endpoint says a LAN or internet address that isn't its own has its ports listed and none asked.
  - It never dials a loopback, link-local, unspecified or multicast address.
  - This replaced dialing the endpoint's address from the server with a deny list, which the review of PR #872 showed could leave the server (H1). It was chosen over an allow-list of the server's bridges, as it needs no reading of networks and covers address reuse too.
- **What it reads:** a banner the app sends first, such as SSH's, or else the status line of a `GET /`. It follows no redirect, reads 256 bytes at most, and keeps 40 characters of them. Each port gets 2 seconds, four at once.
- **Bounds:**
  - A call reads 4 containers, lists up to 128 ports, and asks the first 24.
  - A port past those has no answer, and `cut` says when the list isn't whole: more ports, or more containers, than were read. The panel then never says nothing listens on a port it doesn't list.
- **The same container throughout:**
  - What listens counts only if the container is the same process (PID and start time) after `/proc` was read.
  - Before each port is asked, the container must still be the same process at the same address. Once it isn't, nothing asked counts, and `unread` says it restarted.
- **Not asked:** an app on the server's own network, as its ports are the server's. On rootless Docker, its ports are listed and none asked, as `checkReach` refuses there too.
- **Who may ask:** a developer of the service's project, checked by the gate and again by the handler. The server must be one the service runs on.
- **Limits:** 60 checks a minute per person, where a token counts as its owner, and two checks in flight per server.
- **In the panel:**
  - Answers read before the app last changed (a restart, another revision) are dropped and asked again.
  - Where the server didn't check, a state says why, with what to do: update its agent, or Check again.
  - An agent from before this, a stopped app, or a caller who may not ask leaves the list the spec gives.
- **Numbers, granted on 2026-11-19:**
  - agent.proto: `Call.app_ports` 390, `CallResult.app_ports` 391, `Hello.reads_app_ports` 392, in a block of 390 to 399 for a service's ports.
  - service.proto: `CheckServicePortsRequest`, `CheckServicePortsResponse`, `ServicePortCheck` and `PortAnswer`, all new.
  - `cut`, field 3 of `AppPortsResult` and of `CheckServicePortsResponse`, both messages this change adds, for the review's M1.
  - No migration.

## Verified

Checked on 2026-11-17 against Cloudflare's API, with a scratch tunnel made for it, remotely managed as the dashboard makes them, in a test zone of the owner's account (written here as example.com). Every call went through a guard that reached only that zone and tunnels named for the test. cloudflared was 2026.5.2. Everything made was removed after: the records, then the tunnel. No token is recorded here.

1. **A `PUT` replaces the whole document,** and the version moves by exactly one for each `PUT` that changes it, with the new version in the reply. A new tunnel reads version 0 with no `config`. A `PUT` that changes nothing, a refused `PUT`, and a connector connecting leave the version alone. A `PUT` that left out the top-level `originRequest` dropped it.
2. **A wildcard rule works in a remotely managed tunnel.** `*.example.com → http://127.0.0.1:<port>`, before the catch-all, saved and carried HTTPS traffic for a name with no rule of its own to a local server through cloudflared. Cloudflare validates as cloudflared does: it refuses a catch-all that isn't last, a document with no catch-all, and a `*` anywhere but the leftmost label, and it accepts a rule an earlier wildcard shadows, which then gets no traffic. `*.example.com` also routes a name two labels down, but HTTPS fails there at Cloudflare's edge, as its free certificate covers one level. The apex wasn't tested, as the test zone's apex is in use.
3. **What's read survives a write-back.** The top-level `originRequest` (`connectTimeout`, `noTLSVerify`, `httpHostHeader`, `keepAliveTimeout`), each rule's `originRequest` and `path`, and unknown keys at the top level, on a rule and inside both `originRequest` blocks, all came back equal. Key order didn't: Cloudflare returns its own. `warp-routing` can't be set through this API: a write of `enabled: true` changed nothing, and every read shows `enabled: false`.
4. **A proxied CNAME to `<tunnel-id>.cfargotunnel.com`,** with a comment, was made, read back with its comment and TTL, and deleted; a read after reads "Record does not exist" (81044). A CNAME was accepted beside a TXT, proxied or not, and beside an MX, and both served traffic. Beside an A it was refused: "An A, AAAA, or CNAME record with that host already exists" (81053).
5. **cloudflared ran from a token file** (`tunnel run --token-file`), connected four times, and its log held neither the secret nor the token. Cloudflare's answer to making a tunnel carries `credentials_file`, which holds the secret.
6. **The connections list** gives each connection's `origin_ip`, colo, start time and client version, and `origin_ip` matched the machine's outbound address.
7. **The zone's SSL mode** reads at `/zones/{zone}/settings/ssl` with its value and whether it's editable.
8. **The account's token is account-owned:** `/user/tokens/verify` refuses it, as it does every account token. It did everything above.

Checked again on 2026-11-17, after the re-check, with a second scratch tunnel, and no DNS record made:

9. **A connector proves itself by its ID.** cloudflared, run from a token file with `--metrics` on a known port, answered `/ready` with `{"status": 200, "readyConnections": 4, "connectorId": "…"}`, and the tunnel's connections list carried that same ID for its connector, with its 4 connections. A second run of cloudflared got a new ID. With no `--metrics`, cloudflared listens on localhost ports 20241 to 20245, or on all interfaces in a container. The list can trail a fresh connector by a few seconds.
10. **The apex takes its own rule.** A rule for `example.com` saved before the wildcard and after it. cloudflared's own matcher (`tunnel ingress rule`, offline) sends the apex past `*.example.com` to its own rule, and a name two labels down to the wildcard. No apex DNS record was touched, as the test zone's apex is the owner's; a CNAME at an apex that holds an A or AAAA follows from 81053 (4) and needs Replace it.
11. **A token reads its own rights.** `/accounts/{account}/tokens/verify` answered the token's ID and status, and `/accounts/{account}/tokens/<that ID>` answered its policies: each with its effect, its permission groups by ID and name, and its scope (the account, or all the account's zones). The rights are named "Write" in the API. The token held Account API Tokens Write, which includes reading, so whether API Tokens Read alone is enough is still to check.

Slice 1's first commit made these checks a test, `TestCloudflareLive` in `internal/control/dnsproviders`, which runs when `DOCKWELL_TEST_CLOUDFLARE` names a guard to a scratch zone (and `DOCKWELL_TEST_CLOUDFLARED` names cloudflared). It passed on 2026-11-17, reading the connector's ID with the agent's own `/ready` reader, and found two more:

12. **Records filter by their comment's start** (`comment.startswith`), so the drift timer lists Dockwell's records in a zone in one call.
13. **A tunnel whose connector just stopped can't be deleted** for a minute or so, while its connections close: "This tunnel has active connections" (1022). Deleting one, as removing a server does in slice 3, tries again until they've closed.

**What changed in this ADR because of it:**

- One `*.zone` rule serves every depth, so the rule per level went (section 4). A name past the first level is a state in the check, as Cloudflare's edge certificate doesn't cover it (section 1).
- A CNAME doesn't conflict with TXT or MX at Cloudflare, so that state went, and Replace it touches only the A, AAAA or CNAME (sections 1 and 5).
- Comparing versions by arithmetic is sound, so the race is detected by the `PUT`'s reply (section 4). Writes compare documents value for value, as key order isn't kept.
- Dockwell's own tunnel generates its secret and never reads one back (section 9), and the install guide prefers a token file (section 10).
- Servers sharing one outbound address can't be told apart by the connections list, which section 3 now handles.
- Decision 1 holds as decided: wildcards work, so the owner's tunnel with one rule per zone stays the default, and slice 3 doesn't have to land first.
- A tunnel is proved by its connector's ID, with addresses only as the fallback, and only when every connector is this server's (section 3).
- The apex is a second rule, so a zone's tunnel is written at most twice (section 4).
- Checking each right needs API Tokens Read, which joined the table; rights are matched by ID, and a token that can make tokens is said to be one (section 6).

**Still to check, with the owner,** at the start of slice 2, as none of them blocks slice 1 and there's no dashboard session here:

1. Whether the dashboard saves the whole document, so a stale tab removes a rule written since. The API's `PUT` does, so section 4's drift timer stays either way.
2. Whether editing a token keeps its value, so **Add it to this token** needs nothing pasted again, and the link to a token's edit page. If it doesn't, the action becomes make the token and paste it.
3. The link that makes a token with permission groups and the account filled in.
4. Whether Cloudflare Tunnel Edit can be scoped to one tunnel (section 6's card).
5. Whether a token with API Tokens Read, and not Write, reads its own record (11).

## Numbers to ask for, none taken

Read from origin/main at ea9b7b17a and every pushed and local branch on 2026-11-17. Messages with spread-out numbers take 340, which is free in each of them on main and on every branch; small messages take their next number.

| Message (file) | On main | Proposed |
| --- | --- | --- |
| `DnsProvider` (specs.proto) | 1 to 4 | `tunnel_api_token` 5 (sensitive, Advanced's second token); `strict_zones` 6. No `account_id`: it's per zone |
| `Route` (specs.proto) | 1 to 8, 10, 80, 110, 120 (9 is unused on main but held by the stale `ux/protect-ingress`) | `publish` 340, a new `RoutePublish`: `way` 1, `tunnel` 2, `provider` 3, `takes_over` 4, a repeated new `TakeOver`: `record_id` 1, or `tunnel` 2, `hostname` 3 and `path` 4 for a tunnel rule, which has no ID |
| new enum `RouteWay` (specs.proto) | none | 0 unspecified, 1 server's tunnel, 2 own tunnel, 3 proxy, 4 own HTTPS, 5 something else in front |
| `AlertRule.Condition` (specs.proto) | 1 to 10, 20, 21, 30, 40 | `CONDITION_DOMAIN_UNPUBLISHED` 50 |
| `HostAccessKind` (change.proto) | 0 to 19, 110, 120, 150, 170 to 174 | `TUNNEL_RULE` 340, `CLOUDFLARE_TAKEOVER` 341 |
| `PlanStep` (change.proto) | 1 to 18, 20 to 24, 30 to 36, 40 to 46, 70, 80 to 82, 100 to 102, 110, 130, 131, 150, 240, 260 (89 held by `t3/pg-own-user`) | `provider` 340, a new `ProviderStep`: provider, what (record, zone rule, takeover, restore), target (zone or tunnel), name, was, to, and the routing check's result |
| `CheckDomainRequest` (domain.proto) | 1 to 3 | `publish` 4, the way the person picked, to check that way |
| `CheckDomainResponse` (domain.proto) | 1 to 3 | `cloudflare` 4, a new `CloudflareCheck`: zone, record state, record words (an admin's only), types at the name, tunnel, the rule that catches it first, the default way, and each way with why it's off and its action |
| `CheckDnsProviderResponse` (domain.proto) | 1 to 4 | `capabilities` 5 (a new `ProviderCapability`: capability, state, detail, fix link); `tunnels` 6; `makes_tokens` 7 |
| `DnsZoneCheck` (domain.proto) | 1 to 3 | `ssl_mode` 4; `account_id` 5 |
| `DnsProviderInfo` (domain.proto) | 1 to 4 | `tunnel_api_token_set` 5 |
| `Domain` (domain.proto) | 1 to 5 | `published` 6, a new `DomainPublished`: name, way, tunnel, record, state (published, gone from the tunnel, changed at Cloudflare, can be taken on, can be moved), taken on at |
| `ServerInventory` (server.proto, sent by the agent) | 1 to 7, 10 to 15 (16 and 17 held by `t3/688-ephemeral-ports`) | `tunnels` 18, a new `CloudflaredTunnel`: tunnel ID, account tag, `connector_id`, from a token or a file, how it reaches Caddy, network range, container or process |
| `Event` (event.proto) | 1 to 11, 20, 30, 31, 90, 110, 120, 130, 140, 141 | `cloudflare` 340, a new `EventCloudflare`: what was written, removed, restored or found gone, where, and the version |
| `EventKind` (event.proto) | 0 to 5, 10, 90, 110, 130 | none: writes come under `CHANGE`, drift under `HEALTH` |
| `agent.proto` | | none: the inventory travels as `dockwell.v1.ServerInventory` |
| `StepUpAction` (auth.proto) | | none: takeover applies with `APPLY_ROOT_CHANGE` (8) |
| RPCs | | none new: `CheckDomain` and `CheckDnsProvider` grow, so `methods.ts` keeps its lines. Manage in Dockwell, Put it back and Move it to Dockwell are `ProposeChange` plans |
| Store migration | 0045 on main, none past it on any branch | 0046: what Dockwell wrote at Cloudflare, and the tunnel writer's lock with its lease, taking main's next number when it lands |

## Slices

1. **The server side, behind no panel change:**
   - the agent finds tunnels, opening files safely and keeping only the token, and reads each connector's ID from `/ready`;
   - the control plane proves them by connector ID, or by ways out, and only when every connector is the server's;
   - one token's capabilities, each checked by ID from its own record, and the account read per zone;
   - `CheckDomain`'s Cloudflare states;
   - the tunnel writer: the raw splice and its positional check, the panel guard's three cases, the routing check, the zone and apex rules, one writer per tunnel, and the race's states;
   - the DNS steps, with comments, the replaced record kept, and `cloudflare_takeover`;
   - the step order against ADR 0103's address, the companion, taking on, Move it to Dockwell, removal, undo, the drift timer and its alert.
   - Tested against a Cloudflare stand-in that keeps a versioned document, with the full fixture, a write that races, two Dockwell writers at once, a stale whole-document save, the panel served by its own rule, by the catch-all and by a wildcard, a symlink out of a container, and the token test that searches for `s`. cloudflared's offline matcher (`tunnel ingress rule`) checks the routing check's answers.
2. **The panel,** starting with the five checks still open with the owner (Verified): the sheet and its states as mocked, Websites' domain step, the Integrations card's capabilities and **Add it to this token**, the plan's provider steps, the Domains row's drift state, and the catalog entry's words.
3. **Dockwell's own tunnel** (section 9), after slice 2, as wildcards work (Verified, 2).

## Decided with the lead

1. **The default for the owner's tunnel:** leave it alone where its catch-all or a rule already reaches Caddy; otherwise one confirmed `*.zone → Caddy` rule per zone, behind a routing check that proves no other name moves. Dockwell's own tunnel is under Change. Wildcards were checked to work (Verified, 2), so this stands.
2. **One token carrying both rights,** each capability checked and shown on its own; **Add it to this token**, a second token under Advanced, and ADR 0099 amended.
3. **Add a tunnel** is slice 3: a catalog install through a plan, with server removal and compromise cleaning up or rotating its tunnel.

## What we turned down

- **A rule per domain in the owner's tunnel.** Dozens of whole-document writes over months, each with the race and the panel at stake, where one per zone does the same.
- **Two tokens required.** DNS Edit and Tunnel Edit already reach the same names, so a second token buys no safety and costs a trip to Cloudflare.
- **Flipping a zone to Full (strict)** to make the proxy way work. It changes every proxied name in the zone; it stays an admin's own plan.
- **Believing any container's tunnel ID.** It needs no secret, so only the connector's ID in Cloudflare's connections list proves it.
- **Matching the interface's address.** On a cloud with 1:1 NAT it's private, and no tunnel would ever be proved.

## Consequences

- **Adding a domain is one sheet and one plan,** with nothing done in Cloudflare's dashboard, and removing it cleans up after itself.
- **The owner's tunnel is written at most twice per zone,** for its names and its apex, never re-encoded, never for the panel's names, and only when the routing check proves no other name moves. Every domain after that is one DNS record, which can't overwrite another.
- **What Cloudflare's API allows is said, not hidden:** a race, or a stale dashboard save, shows as a state with its action, and an alert.
- **Nobody below an admin can change or read what someone else made at Cloudflare.** Replacing or taking on needs an admin's passkey, and undo restores what was replaced.
- **A token with Tunnel Edit can edit every tunnel in the account,** and the card says so. It lives on the control plane alone, as ADR 0088's does.
- **The agent reads a connector token's tunnel ID on its own server,** and drops its secret at once. Nothing new leaves the server but an ID and an account tag.
- **Routes behind a tunnel on its own network need a person once,** to approve believing that range (ADR 0030).
- **Nothing here names the owner's domain, server or tunnel.** The ADR, mocks and tests use example.com, hz-1 and web-1.
