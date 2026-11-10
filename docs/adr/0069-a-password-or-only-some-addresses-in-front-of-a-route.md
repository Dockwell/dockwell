# 0069. A password, or only some addresses, in front of a route

- Status: Proposed
- Date: 2026-11-10

## Context

The PRD's Network section lists "Private services: IP allowlists, basic auth, OIDC proxy in front of any service" as P1 (issue #266 tracks it as missing). Today a route answers anyone who reaches its domain: an admin tool, a staging copy or a dashboard on a domain is public, or isn't on a domain at all. 1Panel offers a password on a website and on paths under it, and allow and block lists of addresses, through its OpenResty.

What there is to build on:
- A route is already one domain, or one path under it, sent to a service's port ([ADR 0024](0024-the-caddy-ingress-driver.md)); Caddy matches a domain's longest paths first.
- A route already knows which client address to believe, the peer or what its own `trusted_proxies` say, in `{http.vars.dockwell_client}`, with the limits [ADR 0030](0030-routes-behind-a-tunnel-or-a-proxy.md) and [the ingress threat model](../security/ingress.md) set out.
- Every change to a route is a revision with a plan ([ADR 0009](0009-resources-revisions-and-plans.md)), and a field marked `(sensitive)` is sealed at rest and masked in plans ([ADR 0022](0022-secrets-encrypted-at-rest.md)), which also sets the rule that proposing a value must never tell whether it is the one there.
- Taking a domain off HTTPS needs a person, as a token can't weaken how a domain is served (ADR 0030, `WeakensClaims`).
- The panel's own domain isn't a route: the route kind reserves it, and the proxy sends it to the panel's socket on a route of its own ([ADR 0063](0063-the-panels-domain-set-from-the-panel.md)).

## Decision

### It is the route's: a field, so every change is a plan

`Route` gains `access` (field 9), with three fields: `users`, who a password lets in (HTTP basic auth); `allow`, the addresses and ranges that may reach it; and `deny`, those that may not. Proposing, planning, applying, undo and rollback are a route's, and the agent changes that one route in Caddy, which restarts nothing.

**An edit that leaves `access` out keeps what the route has,** as `keep_env` keeps a service's own values (#284). Opening a route up takes an explicit clear: `access` set and empty. Proto3 gives a message field presence, in Go and in protobuf-es, so the two can be told apart. Reads give `access` back without a password or a hash: `ListDomains` has each route's user names, `allow`, `deny` and `trusted_proxies`, so a client can see what a route has and send it back whole.

**Every whole-route writer keeps it:**
- The panel's "Move behind what's in front" rebuilds every route of a domain from its path, service, port and the front, and leaves `access` out, so each route keeps its own.
- A website's route is rebuilt from the website and its front (`website.Resources`), and leaves `access` out, so a site that changes what it serves keeps its protection. Protect on a website edits its route.
- An older CLI, and a token's `--edits @route.json` written before this release, send specs with no `access`, so they keep it.
- Adopting drift reads `access` back from Caddy, so it keeps what Caddy has.
- An undo or a rollback puts back the revision's `access` as it was, which the plan says by name (below).
- The 1Panel importer makes new routes. A site of its with a password gets a new one, as its hashes don't come across (below).

Slice 2 has a test that a protected route survives each.

**A path is protected by its own route.** Protecting `/admin` of an app served at the root is a second route, for `/admin`, to the same service and port, with its `access`, made in the same plan; the longer path answers first. Nothing is inherited. A plan that leaves a route of a protected domain open says so ("app.example.com/api answers anyone, though app.example.com asks for a password"). It also says what else reaches the service, as a state with an action: "web is also published on port 8080 · Remove", "web also answers on www.example.com, which anyone can reach". The sheet defaults to the whole domain. Caddy cleans and lowercases a path before matching, but `/admin;jsessionid=x` matches neither `/admin` nor `/admin/*`, and a Java server strips `;…` and serves `/admin`, so a plan protecting a path on such a service says to protect the whole domain.

### A password: hashed by the control plane alone, at one cost

- **A user** is a name of 1 to 64 letters, digits, `.`, `_`, `@` or `-`, compared as it is written, and a password. Anything else, such as `{`, which Caddy reads as a placeholder, a colon or a space, is refused, as is a name twice. A route has at most 20.
- **A request gives the password itself, never a hash.** `RouteUser.password` is write-only: 12 to 72 bytes, as bcrypt reads no more. `password_hash` is Dockwell's alone, and a request that sets it is refused; only an undo, a rollback or adopting drift puts one back. The control plane hashes in one hook it registers with the revision engine, which runs for a caller's edits only, before the route kind's checks and its keeper. The route kind's checks, which the agent runs too, refuse a spec with a `password` in it, and take a `password_hash` only as `$2a$14$` and 53 characters. So no revision, change, event, log or server ever holds a password, a hash of anyone else's making never reaches Caddy, and bcrypt stays out of the agent build.
- **bcrypt at cost 14, measured.** Caddy 2.11.7 checks a request that names a user it hasn't got against a stand-in hash, bcrypt at cost 14 (`caddyauth/bcrypt.go`, `FakeHash`), so a real hash at any other cost would tell by its timing whether a name exists. At 14, a known name and an unknown one cost the same, and every request costs one check. On one core of this machine's Xeon, a check costs 60 ms at cost 10, 233 ms at 12 and 0.93 s at 14. Argon2id at Caddy's defaults, whose stand-in matches them too, costs 29 ms, but 46 MiB a check, and nothing bounds how many run at once, so a burst of guesses could take a small server's memory. bcrypt's cost is CPU alone: a burst slows the server rather than stopping it.
- **What that leaves.** Timing tells nothing of which names exist, and no request costs more than one check. A fresh guess still costs about a second of a core, so a few a second keep a core busy. What stands in front of that: an allow list, as addresses are checked first and cost nothing; Caddy's hash cache, which keeps what it checked (both outcomes, up to 1000), so a person who signed in costs nothing again; and the ingress jail of [ADR 0052](0052-a-servers-firewall-ssh-and-fail2ban.md), which lacked a failure to count and gains 401s on protected routes, in a slice of its own.
- **Hashing costs the control plane too:** about a second for each new password, so at most 20 seconds for a proposal that sets 20 at once. A user kept by name isn't hashed again, and proposing takes write access.
- **No guess is confirmed.** A password is hashed with a new salt each time, so setting one always plans as a change, whatever it is, and nothing compares a request with the stored hash. The route kind doesn't rotate: a hash compares as any field, so changing a route's port plans no password change. A plan's diff keys users by name, so removing one of three reads as that one.
- **A user named with no password keeps its hash** (`resource.Keeper`), so changing a route's port from a client that never reads hashes keeps every password. A user the edit leaves out of a set `access` is gone; a new user with no password is refused.
- **Where a hash goes.** A hash follows its route, to every server it runs on, including one a move adds: a password made in the browser and shown once can't be given again, and root on any server a route runs on can read its hashes anyway. On a server, it's in Caddy's config, its autosave and the agent's last-good copy (all 0600), and in the agent's cache of desired state once ADR 0054 (#312) lands.
- **In Caddy:** an `authentication` handler with `http_basic`, bcrypt, the domain as the realm, and the hash cache on. `Authorization` is removed before the request goes on, so the app never sees the password, and an app that reads its own `Authorization` header can't sit behind one, as a browser sends one header: it takes an address list instead. Who signed in isn't passed to the app, in slice 2 or by a header a client could write; sign-in through a provider (below) decides that.

### Sealed only while it holds a password

- **A route is sealed while it has a user,** and stored in the clear without one, as before. So a release before this one still reads every route without a password, and only protected routes need care before a downgrade.
- **The rule for going back:** remove protection from every route before running a release before this one. That release can't open a sealed route where it reads them raw: `dockwell run` with a panel domain would stop, and the Domains list would fail, and its agents would serve those routes open. The release notes say so.
- **A route in the clear that holds a password,** as a downgrade round trip could leave, is read, logged and sealed on its next change, not refused.
- **One helper reads routes from the store,** opening each through the keeper in one batch: one read of their data keys, and one unseal for each key-encryption key. Its guard test has two rules: a route without users isn't sealed, and nothing outside the helper lists, fetches or decodes routes raw.

### Only some addresses: by the address the route believes

- **`allow` and `deny` each take up to 100 addresses or ranges,** IPv4 or IPv6, kept masked, as `normalizeProxy` keeps a proxy, and shown that way in the plan (`203.0.113.7` is `203.0.113.7/32`). A zone and an IPv4 address written in IPv6 are refused. Each IPv4 range goes into Caddy with its IPv6-mapped twin too, as a dual-stack proxy may write a client that way. An `allow` entry that a `deny` entry covers is refused, as it could never match; a narrower `deny` inside an `allow` is what `deny` is for.
- **`deny` is checked first; with an `allow`, everything not in it is refused,** with a 403 that says, as a state, what the server saw: "203.0.113.9 can't reach app.example.com." Addresses are checked before the password, so an address that may not reach a route never sees the prompt. A believed address that isn't one, as a garbled header gives, is refused whenever a list is set, so `deny` fails closed as `allow` does.
- **Whose address that is depends on the route.** The route's step first sets the variable Caddy's `client_ip` matcher reads to the route's own belief (`dockwell_client`), as Caddy works its own out from what any route on the server trusts.
  - A route that names no proxy believes the peer, and nothing another route on the server trusts reaches it. A test says so.
  - A route that names a range believes what its `X-Forwarded-For` says, and Caddy walks that past every range any route on the server names. So anyone whose address is in any of those ranges can claim any address through it, as the threat model's "Forge it through a range any route on the server names" says. Behind `cloudflare`, the claim is as sure as Cloudflare makes it ("Forge it through `cloudflare`", same document).
  - The plan says so whenever such a route gets an `allow`, as fact, consequence and fix: "Only from 198.51.100.7 is as sure as hz-1's proxies: anyone in 192.0.2.0/24 can claim to be it. Name your proxy's own addresses, not a provider's range." Or, for the preset: "… as Cloudflare says: a Cloudflare customer with a zone of their own may claim it. Add a password too." An allow list there stays allowed.
  - A route with HTTPS off and no proxy sees every request come from its tunnel on the server, so a list there lets all in or none: the plan says so, and how to name the tunnel.
- **"Your address" is one click.** `GetAuthState` gains `address` (field 11): where the request came from, as the control plane sees it, when `resource.IsPublicAddress` says it's public, which leaves out loopback, private ranges and `100.64.0.0/10`, and when it isn't one of Cloudflare's, as it is when the panel's own domain is proxied. The panel offers an IPv6 address as its `/64`, as privacy addresses change daily. The app's server may see the person differently, so the panel says "as the panel sees you", and the 403 shows the address it saw.
- **Calls from the server to itself.** An app or a task on a server that calls its own protected domain arrives from that server's public address, so the plan says when an `allow` leaves it out, with "Add hz-1's address".

### Never in clear text

- **A route with HTTPS off can't take a password.** The route kind refuses it: "app.example.com is served without HTTPS here, so a password would cross the network in clear. Turn HTTPS on, or protect it by address." The panel offers Turn HTTPS on in place of the password choice.
- **Cloudflare's proxy with plain HTTP to the server,** HTTPS off and `cloudflare` trusted, as Cloudflare's Flexible mode is, gets a plan notice on an address list: "Cloudflare reaches hz-1 over plain HTTP, so what visitors send crosses the internet in clear between them. Turn HTTPS on here and use Cloudflare's Full mode."

### Opening a route up takes a person

Removing a route's password (its last user), emptying its `allow` or widening it to every address, and deleting a protected route while an open route of its domain would answer its path, are an access a person approves (`AccessOpensRoute`), as taking a domain off HTTPS is, within the undo window as ADR 0030 has it. A token can propose it, and can't apply it alone. Adding protection, narrowing it, removing one user of several, and changing `deny` stay a token's: a `deny` keeps a few out, and doesn't keep a route private.

### Older agents

`Hello` gains `protects_routes` (field 211, in the apps area's block), as `writes_references` was added. The control plane refuses to plan a route with `access` on a server whose agent doesn't announce it: "hz-2's agent is older than this release, so it would serve app.example.com to anyone. Update its agent, then plan again", with that server's Update action. A route is never shown as protected while an agent serves it open: if a server's agent stops announcing it, as after a downgrade, the route's row says "Open on hz-2 · Update its agent", and plans there refuse as above.

### Certificates are never locked out

Caddy answers ACME's HTTP-01 challenges before any route runs ("handle them separately from any user-defined handlers", in `caddyhttp.Server.ServeHTTP` as of 2.11.7, the version `scripts/ingress-tools.sh` pins), and TLS-ALPN-01 ends in the TLS handshake. So neither a password nor an address list stands between a CA and a certificate, its first or a renewal, though the CA checks from addresses nobody lists. Caddy answers only the challenges it started, so nothing is left out of a route's own matchers, and a path the app serves under `/.well-known/acme-challenge/` stays protected. A real-Caddy test pins it: Pebble issues a certificate for a domain that asks for a password and allows only an address Pebble isn't, and the route still refuses a request without one.

### The panel's own domain isn't covered

It isn't a route, so Protect isn't offered and nothing here reaches it. A password in front of the panel would break API tokens, which use `Authorization`, and an agent joining over its domain, and the panel already has passkeys. Narrowing who reaches the panel by address is the firewall's ([ADR 0052](0052-a-servers-firewall-ssh-and-fail2ban.md)).

### Who may, and what it says

- **Whoever may change a route may protect it,** with no new permission; opening it up takes a person, as above. Each change is on the timeline and in the audit log, with every password masked.
- **A plan says what changes in words:** "Asks for a password: 1 user, mira (password set)", "Only from 203.0.113.7/32 and 198.51.100.0/24", "Refuses 192.0.2.0/24", "Anyone can reach app.example.com again".
- **An undo past a new password brings the old one back,** as revisions don't change, so the plan says it by name: "mira's password goes back to the one from 12 Nov".
- **The CLI refuses a sealed value written inline,** for any kind, a route's password among them, rather than one kind at a time, so the next sealed field needs no rule of its own and a password comes from a file or standard input, out of a shell's history. A container's own environment still goes inline, as `--env` does ([ADR 0022](0022-secrets-encrypted-at-rest.md)).
- **What the proxy has reads back.** The Caddy driver reads a route's `access` back from Caddy, users and hashes included, so drift compares as for any field: a user added by hand reads as that change. Anything else in those handlers reads as a hand edit, a range added by hand included, as Dockwell writes each range with its IPv6-mapped twin.

### In the panel

The app's domain rows and the websites' domain rows get **Protect**, a sheet with the two choices, defaults first:
- A password, for one user named after the signed-in person (their first name, lowercased, with what a name can't hold left out, or "user" when nothing is left, and editable in place), with a password made in the browser, 24 characters from a 32-letter alphabet, about 120 bits, shown once with Copy. With HTTPS off, Turn HTTPS on stands in its place.
- Only some addresses, starting at your address.
- Typing a password, more users and addresses to refuse are one step away.

The row says "Protected · password", "Only from 2 addresses", both, or "Open on hz-2 · Update its agent".

### In slices

1. **Reading routes through one helper, and sealing a route that holds a password,** with its guard, and no new field: inert until slice 2, and tried on the ARM trial.
2. **The spec and the driver:** `access`, the hashing hook, the keeper, `protects_routes`, opening up as an access, plan words, the CLI, and the real Caddy and Pebble tests.
3. **The panel.**

### Sign-in through a provider: a later slice, sketched

The PRD's "OIDC proxy" is a fourth member of `access`, not built here: `sign_in`, the members of the route's project, through Caddy's `forward_auth` to the control plane, which signs a person in with a passkey, or with an OIDC provider once the PRD's OIDC sign-in (Access, P1) is there, and sets a cookie on the app's domain. Its record says how that cookie is scoped, what an agent checks when the control plane is down, and whether who signed in reaches the app.

### Answers to the review's questions

1. **Calls from a server to itself:** the plan says when an `allow` leaves out the server's own address, with an action to add it (above).
2. **The user's name to the app:** not in slice 2. Sign-in through a provider decides it, with the header's name and that a client's own is stripped.
3. **Sealing only the hash:** weighed, under Considered.
4. **Numbers:** `Route.access` is 9, `GetAuthStateResponse.address` is 11, and `Hello.protects_routes` is 211, in the apps area's block (211 to 219), free on every branch as of this writing.
5. **1Panel's password on a website's path,** such as `/wp-admin`: a website takes its whole domain, so it has nothing to map to in slice 2, and the importer lists it as one it can't bring yet. 1Panel's own hashes don't come across either: a hash isn't taken from a request, so the import makes each user a new password, shown once.
6. **CORS:** a browser's preflight carries no credentials, so a cross-origin API behind a password fails its `OPTIONS`. That's under Consequences.

## Considered

- **Sealing every route:** decided first, and revised. It made every route unreadable to a release before this one, and stopped `dockwell run` from starting after a rollback, for routes that hold nothing secret.
- **Sealing the hash alone:** the keeper seals whole specs, and a field of its own would be a second sealing format with its own readers. Sealing a route only while it holds a password gets most of what this was for: every route without one reads as before.
- **A kind of its own for a route's passwords,** or secrets of the environment named by the route: either puts a second record beside every protected route, kept in step on every change, move and delete, and a secret can be revealed, where a route's password is never shown again.
- **bcrypt at a lower cost:** cheaper per guess, and it tells by timing which names exist, as Caddy's stand-in hash is cost 14.
- **Argon2id at Caddy's defaults:** 32 times cheaper a check, and 46 MiB a check with no bound on how many run at once.
- **A hash given in a request,** as htpasswd and 1Panel have them: its cost and shape would be a caller's to choose, which undoes both the timing and the cost bounds above.

## Consequences

- A route with a password is sealed, so reading it costs opening it, as reading a service does. The Domains list at 500 routes took 18.1 ms and 2.8 MB a call with none sealed, and 19.8 ms and 4.2 MB with every one sealed, on SQLite.
- Restoring a database without its key file now loses protected routes too, not only secrets.
- Someone who can write the database without the key can replace a protected route with an open one in the clear, as they could any route before this.
- A route's revisions hold bcrypt hashes, sealed. Anyone with the database and the install's key can guess at them offline: a made password resists that, and a typed one is as strong as its words, which is why one is made by default and a typed one takes 12 characters.
- Each guess at a password costs the server about a second of a core, and nothing in Caddy limits how often: an allow list, or the jail's slice, is what does.
- Basic auth and an app's own `Authorization` header don't mix, and a browser's CORS preflight to an API behind a password fails; an address list has neither problem.
- Address lists are only as good as the address a route believes: a route naming a provider's range believes anyone in it, and the plan says so.
- Going back to a release before this one takes removing protection first.
- The panel's own domain is out of scope by design.
- The agent build gains no dependency: hashing is the control plane's, and the agent writes the hash a route holds into Caddy's config.
