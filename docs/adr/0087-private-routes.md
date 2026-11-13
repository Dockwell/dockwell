# 0087. Private routes: an address list and passwords, with room for signing in

- Status: Accepted. It adds to what a route is, from [0024](0024-the-caddy-ingress-driver.md) and [0030](0030-routes-behind-a-tunnel-or-a-proxy.md).
- Date: 2026-11-13

## Context

Issue #598, and the PRD's networking row "Private services: IP allowlists, basic auth, OIDC proxy" (P1). 1Panel offers basic auth and IP rules on each website, and people put them in front of an admin panel, a staging app or a dashboard that has no sign-in of its own. A route answers whoever reaches it today: [ADR 0024](0024-the-caddy-ingress-driver.md) gives it nothing else to say, and listed "per-route settings such as headers or auth" as not there yet.

[ADR 0069](0069-a-password-or-only-some-addresses-in-front-of-a-route.md) proposed the same, and wasn't built. This record takes its place. Where it differs: Caddy checks a hash itself, at cost 10, with a check of names ahead of it and a list in front, where 0069 had the agent check at cost 14, with a cap and a wait for each address; a route is sealed as its kind is, with routes from before read as they are, where 0069 sealed a route only while it held a password; and there's no list of addresses to refuse.

Three things this has to get right, as getting them wrong lets in exactly who the setting is meant to keep out:

- **Who a client is.** [ADR 0030](0030-routes-behind-a-tunnel-or-a-proxy.md) decides it per route: the peer on the connection, or what a proxy the route names says. Behind a tunnel that names no proxy, every visitor reads as `127.0.0.1`. Behind Cloudflare's preset, the address is only as sure as Cloudflare says, which ADR 0030 says not to allowlist on.
- **Who may open a route again.** A token can edit routes. If it could take a password off, widen the list, or add a user of its own, it could let itself in.
- **What a password is kept as.** It must never be shown back, and Caddy checks a hash.

## Decision

### A route says who gets in

`Route.access` (110), a `RouteAccess`:

```proto
message RouteAccess {
  repeated string allow = 1;      // addresses and ranges, IPv4 and IPv6
  repeated RouteUser users = 2;   // who may sign in with a password
  // 3 is kept for signing in with a provider (OIDC), the next slice.
}
message RouteUser {
  string name = 1;
  string password = 2 [(sensitive) = true];       // given in an edit, never kept
  string password_hash = 3 [(sensitive) = true];  // bcrypt, what's kept
}
```

- **Neither is a route as it is today:** it answers whoever reaches it.
- **A list alone:** an address on it gets in, and anyone else gets **403 Forbidden**, with nothing else said.
- **Users alone:** everyone is asked for a name and password, **401** with `WWW-Authenticate: Basic realm="<domain>"`.
- **Both:** someone must be on the list *and* sign in. Off the list, they get 403 before a password is asked, so a stranger costs the server no password check. "On the list, or with a password" is a reasonable later setting; it isn't here, as the stricter one is the safe default.
- **It covers what the route covers,** its domain and path, and is decided route by route, as ADR 0030's reach is. A path under a private domain that has a route of its own answers as that route says.

### Who the client is, for the list

The list is checked against the client the route already believes, never against a header on its own:

| The route | Its client, for the list |
| --- | --- |
| Names no proxy, with a certificate | The peer on the connection. Caddy overwrites `X-Forwarded-For` (ADR 0024), so a client can't write it in. |
| Names addresses or ranges as its proxies | From one of them, the first address from the right of `X-Forwarded-For` that no route on the server names (ADR 0030). From anyone else, the peer. |
| Names Cloudflare's preset | **Refused**, with the reason: `CF-Connecting-IP` is what Cloudflare says, and a Worker can write it on a domain Cloudflare doesn't proxy, so ADR 0030 says not to allowlist on it. Passwords work, and so does a tunnel that names its own address. |
| Takes no certificate, names no proxy, and answers only this server | **Refused**, with the fix: every visitor reads as this server's loopback, the tunnel's address, so a list can't tell them apart. Name the tunnel's address in `trusted_proxies` first, and its visitors read as themselves. |
| Takes no certificate and answers anyone (`plain_for_anyone`) | The peer on the connection, as with a certificate: TLS ends somewhere that doesn't hide who connects, such as a private network. |

- **Caddy's own matchers do it.** With no proxy named, `remote_ip`. With proxies named, `client_ip` (which is `{http.vars.client_ip}`, ADR 0030's walk) where the peer is one of them, and `remote_ip` where it isn't. Nothing reads `X-Forwarded-For` on its own.
- **What ADR 0030 says of the walk holds here.** Anyone whose outgoing address is in a range another route on the server names can write what they like further left, and get past this list. And anything that can connect from a named proxy's address can claim any client: name `127.0.0.1` for a tunnel on the server's own network, and any process on the server, or any container on its host network, can. The plan says the second when the list depends on it, and the threat model says both.
- **IPv6 is a first-class list entry.** Ranges are written canonical, masked and sorted, as `trusted_proxies` are, so a list compares equal to what Caddy reads back. An IPv4 address written in IPv6 (`::ffff:203.0.113.7`) is refused, with how to write it, as Caddy compares IPv4 clients with IPv4 ranges. A browser's IPv6 address usually changes every day or so (privacy addresses), so the panel offers its `/64`, which stays. A list may hold up to 64 entries.
- **Everyone isn't a list.** A range wider than `/8` in IPv4 or `/16` in IPv6, `0.0.0.0/0` and `::/0` among them, is refused: leave the list out instead. So are the unspecified address, multicast and a network interface's zone.
- **The server's own addresses, and loopback, on a list** let in everything running on that server. The sheet says so when it offers one.

### Passwords, as hashes

- **A password given in an edit is hashed as the control plane checks the edit**, before anything compares or keeps it, with bcrypt at cost 10, and dropped. The hashing is the control plane's alone (`routehash`): the agent build links no bcrypt, and an agent refuses a user with no hash. Only `password_hash` is stored, never in a read, and only "changed" in a plan. A client may give a bcrypt hash itself instead, as from `htpasswd -B -C 10`, at a cost from 10 to 12.
- **A password needs HTTPS where the browser sends it,** as 0069 said and signing in does. A route with users must take a certificate here, or name the tunnel or proxy in front, whose edge serves HTTPS, Cloudflare's preset among them. One that answers anyone over plain HTTP, or names nothing in front, is refused: "Turn on HTTPS for this domain, or name the tunnel or proxy in front of it". The sheet says why in a line, with Turn on HTTPS for this domain, which plans every route of the domain with a certificate. A list still works there.
- **A password has 12 characters or more**, as nothing limits guesses at one, and at most 72 bytes, as bcrypt ignores the rest. The panel makes 24.
- **An edit that names a user with no password keeps that user's hash** (`resource.Keeper`). So the panel and the CLI, which can't read hashes back, send the whole route and lose nothing. A new user needs a password. Removing a user is leaving them out.
- **A kept hash stays on its server.** A plan sends its spec to each server it would run on as it's made, so a route that kept a hash can't move to another server, as a token could carry the hashes to one it controls (ADR 0022). The refusal says so: "its users' passwords stay on the server it runs on. Give ci a new password to move it".
- **Routes are sealed at rest now**, as any kind with a sensitive field is ([ADR 0022](0022-secrets-encrypted-at-rest.md)). Routes were kept in the clear before, and every kind sealed so far was sealed from the start, where the keeper refuses a spec in the clear. A route is `resource.SealedLater`: one stored in the clear that holds no secret, as every route stored before this does, reads as it is, and one in the clear that holds a hash is refused, as Dockwell never wrote one. What read routes straight from the store (the list of domains, a website's place in authorization, and the panel's domain check as `dockwell run` starts) opens them through the store's opener, which the keeper sets (`store.Tx.Open`); `dockwell run` makes a keeper before that check.
- **Why not a secret in the environment's store.** A secret is a value to deliver and reveal ([ADR 0022](0022-secrets-encrypted-at-rest.md)); a password the proxy checks is never delivered, and never needs revealing, only replacing. Keeping its hash alone means nothing on the control plane or the server can show it, and a leaked config gives an attacker a bcrypt to break, not a password.
- **Cost 10, and Caddy's `hash_cache`.** Caddy checks a hash on each request it hasn't cached, and has no limit on attempts. At cost 10 a check takes tens of milliseconds, so a flood of wrong passwords costs a core, where cost 14, Caddy's own default, would cost far more for each. The panel makes the password: 24 random characters, shown once, so the cost isn't what protects it. A list in front stops strangers before any check.
- **Only a user's name costs a check.** Caddy checks a name it doesn't know against a fake hash of its own, at cost 14: about 0.9 s on one core, sixteen times a wrong password here. So a step before Caddy's answers 401, with the same `WWW-Authenticate`, to credentials whose name isn't one of the route's users, by a pattern on the start of their base64 that pins every bit of `name:`. A user's name with a wrong password costs one check at cost 10. Which names exist isn't a secret, as viewers see them on the route anyway; the time a check takes says the same.
- **The app never sees the password.** A route with users takes `Authorization` out of what it sends on, so an app that logs its headers can't log it. An app that needs its own `Authorization` can't sit behind a password anyway.
- **Users:** up to 20, a name of letters, digits, `.`, `_` and `-`, up to 64 characters, with no `:`, which Basic auth splits on.

### Opening a route again needs a person

A change that lets in anyone the route kept out within the undo window is host access, of the new kind `ACCESS_OPENED` (110), so a token gets `needs_session`:

- the list taken off, or an entry added that the old list didn't cover;
- users taken off, or a user added, or a password changed;
- the whole of `access` taken off.

Closing it further is a token's: a narrower list, a user removed, a list or users added to a route that had none. `HostAccess` couldn't say this, as it describes what one spec reaches, and a new public route reaches nothing it shouldn't; so a route says it with `resource.Opener`, which compares two specs, and the engine asks it beside `WeakensClaims`.

What the engine compares is **what serves each domain and path** before the change and after it: the route whose domain and path cover it, the longest. So a change opens what it serves itself, and what it leaves to a route above it:

- **Editing a private route**, in place.
- **Adding a public route to a path under a private domain**, which then serves that path.
- **Deleting a private route to a path under a public domain**, which leaves the path to the domain. Deleting a private route with nothing above it keeps everyone out, and is a token's.
- **With nothing serving a path now**, what served it within the undo window counts, from any environment, as going off HTTPS does (ADR 0030): deleting a private route and making a public one in its place, in one change or two, is the same as editing it. With something serving it, only that counts, so what a person opened stays a token's to edit.

**A domain that keeps anyone out runs from one server.** What serves a path is worked out from specs, wherever they run, and on a server without a path's own route, the route above it serves it there. So a private path moved alone to another server would leave it to a public root on the first, and a private path placed apart from its root from the start leaves it open there already. Once any of a domain's routes keeps anyone out, the engine refuses a change to that domain that would run its routes from more than one place, naming each route and where it runs: "This would run admin on hz-2 and shop on hz-1. Move the whole domain to one server". The sheet offers that move, a button for each server, and holds Review plan until it's done. Moving all of them together opens nothing, and is a token's. A change to another domain in the environment answers only for its own. A domain that keeps no one out runs where it likes, as before.

**A moved domain is never open between steps.** A kind's resources that keep anyone out go ahead of the rest of it in a change's order, so a private path is made before the public route above it, and, as deletes run last first, taken down after it: on the server the domain moves to and on the one it leaves.

The plan says each way in as a fact (`list-off`, `allow:<range>`, `users-off`, `user:<name>`, `password:<name>`), which the plan's words, the CLI's and the panel's put in their own language.

### On the server

- **Caddy applies it in the same reload as any route change** (ADR 0024): no app restarts, and Caddy doesn't either.
- **A route's handlers, in order:** what it believes (ADR 0030), a subroute that answers 403 where the client isn't on the list, a subroute that answers 401 where the credentials name no user, Caddy's `authentication` handler (`http_basic`, bcrypt, `hash_cache`, the domain as realm), then `reverse_proxy`. Each is written from typed values; a name, a range and a hash are all that come from a spec, after checks.
- **A domain with a private path answers 400 to what no browser writes**, ahead of all its routes, on the decoded path:
  - `;` or `\` anywhere;
  - a control character;
  - a `.` or `..` segment, or one with something odd after it (`.well-known` is fine);
  - a byte encoded twice;
  - `::$`;
  - Unicode white space at the end.

  Tomcat, Jetty and Undertow take `;x` off every segment and then resolve `..`, and IIS reads `\` as `/`, so `/x/..;/admin`, `/;x/admin`, `/manager;x/html` or `/%5cadmin` would reach a private path through the public route above it. A browser resolves dot segments and encodes once, so these come from no one a site serves. The cost is that a `;jsessionid` in a path gets 400 on such a domain, which a Java app falls back from to its cookie. On a domain behind a tunnel, it answers only who the domain's routes answer, so a stranger still can't tell they're there. It's one route Dockwell derives on each of Caddy's servers on every change, as it does the 404 for a path no route covers.
- **A private path's other spellings reach it.** Caddy's path matcher sees `/admin;x=1`, `/admin%00`, `/admin%20`, `/admin%5cx` and `/admin.` as other paths, so they'd fall to a public route above it, and Tomcat, Jetty and Undertow take `;x` off, as Windows servers take off trailing dots and spaces. A private path's route matches a second way too: a `path_regexp`, in any case, on the decoded path, of the path followed by nothing, `/`, `;` or `\`, or by nothing but spaces, control characters and dots. What only starts the same, such as `/admin2` or `/admin.php`, is another path. Checked on Caddy 2.11.7 with each spelling.
- **A private route's answers aren't a shared cache's to keep.** It adds `Cache-Control: private` to what it sends back.
- **It reads back as itself.** The list, the users and their hashes are read from Caddy's config, so a hand edit of them reads as what it now holds, and anything else in those handlers reads as edited by hand. Either is drift from the spec.
- **Certificates keep coming.** Caddy answers HTTP-01 before any route (`Server.ServeHTTP`), and TLS-ALPN-01 in the handshake, so neither meets the list or the password. A test holds this against Pebble.
- **Tunnels keep working**, with the client read as above.
- **A website's route keeps its access** when `WebsiteService` writes it again from the website's settings.
- **An agent from before this says nothing of it**, and would serve a private route to anyone, as if its access weren't there. So an agent says `keeps_routes_private` (110) in its hello, and the control plane refuses a route with access on one that doesn't, at plan, at apply and in its desired state, with "Update the agent on <server>".

### What a stranger gets, checked live

The route's server asks it as a stranger, on each pass of the agent's domain watch (about once a minute) and at once after a route changes, and reports what came back with the domain's status (`DomainStatus.strangers`, 110), so the panel reads it where it reads a certificate:

- **From the server itself, with no password**, to Caddy on loopback, over the port it serves the route on. It checks no certificate, as it reads only Caddy's own status line.
- **As someone no list names.** Through a tunnel on the server that the route names (`127.0.0.1`), it says it passes on `192.0.2.1`, from the range kept for documentation, as that's who a tunnel's visitor reads as. Otherwise it's the server's loopback.
- **A private path is asked its odd spellings too:** `P;x`, `/;x/P`, `/x/..;/P` and `/%5cP`, sent as written. Let in any way reads as let in; 400 is kept out.
- **What it says:** kept out (403), asked for a password (401), let in (below 400, which a private route shouldn't be), another answer, or that it **can't tell**: where who it asks as is on the list, or the route answers only proxies the server isn't.
- Once Caddy doesn't answer one, the rest of that pass aren't asked. A request with no `Authorization` costs Caddy no bcrypt.

### Making it private, in the panel

From a lock beside each route, on Domains and on an app's page, **Make it private** opens a sheet (and `?private=<route id>` on Domains links to it), defaults first:

- **What a stranger gets** leads it: the live reading for each of the route's servers, and what applying the sheet would make it.
- **Other ways in** follow it, as a route keeps out only who comes through it: each port the app publishes on every address of its server, with the way to bind it to the server alone, and each other route to the app that lets anyone in, with Make it private for that one. A row on a list says the first of them in place of what a stranger gets: "Private here · open on port 8080".
- **Only these addresses**, with your own address first, as the control plane saw your session, and its `/64` for IPv6, written as the server writes it back. A network is everyone at that office or on that line, so with one first, a password comes with it. Through the SSH tunnel that's loopback, which the sheet says can't be offered, and a new private route starts with a password instead. Behind Cloudflare's preset, or a tunnel the route doesn't name, the list is off, with why.
- **Ask for a password**, with a user name and a password Dockwell makes in the browser: 24 letters and digits no one mixes up, shown once with a copy button, and Review plan waits for "I've kept a copy". A user the route has keeps their password unless you ask for a new one.
- Applying it is a plan, as any change; the panel sends the whole route, as `DomainRoute` now says its list, its users' names and its trusted proxies.

### Signing in with a provider, next

`RouteAccess.sign_in` (3) is kept for an OIDC proxy: Caddy's `forward_auth` to a small proxy Dockwell runs, which sends a visitor to a provider and back, and passes on who they are. It goes beside a list and users, so a route can ask for both. It needs a client secret, which is a secret in the environment's store, delivered as ADR 0022 delivers one to a service, and it is its own issue.

## Consequences

- A request that isn't cached costs one bcrypt check at cost 10; a flood of wrong passwords can keep a core busy. Caddy has no limit on attempts; a list in front stops strangers before any check. The threat model says so.
- A route with access is a few handlers longer in Caddy's config.
- A route spec with users is sealed at rest, so what reads route specs reads them through the keeper.
- A password can't be shown again. A forgotten one is replaced.
- `X-Forwarded-User` or another way for an app to know who signed in isn't here; the provider slice brings identity.
- What isn't here: "on the list, or with a password", a list that names a country or a network by name, limits on failed attempts, and exceptions for a path under a private route other than a route of its own.

## Tests

- **Rules:** each refusal on its own, and that a password is hashed, kept, and never comes back in a read (`route_test.go`).
- **The gate,** on both stores: each way of opening a route needs a person, read across a delete and a new route, and each way of closing it doesn't.
- **`DOCKWELL_TEST_CADDY`:** 403 off the list, on IPv4 and IPv6; a forged `X-Forwarded-For` from a peer the route doesn't name ignored; a named proxy's word taken; 401 without a password, 200 with it, and `Authorization` not reaching the app; a hand edit of the list or a hash reading as drift; no reload dropping a request.
- **`DOCKWELL_TEST_PEBBLE`:** a certificate for a private route over HTTP-01 and TLS-ALPN-01.
- **The stranger check,** on real Caddy: 403 off a list over HTTPS, 401 for a password over plain HTTP, through a tunnel on the server as a documentation address, can't tell where the server is on the list, and a change read at once.
- **From #606's review:** a private path or its root moved alone to another server is refused, and moving them together isn't (both stores); a moved route with kept hashes says why it can't go; each of a private path's other spellings gets 403 on real Caddy, and a sibling that only starts the same doesn't; five names that aren't users cost no hash; a private route's answers say `Cache-Control: private`; the stranger check reads a public `;x` spelling as let in; the panel names a published port and another public route, and pairs a network with a password.
- **The panel:** your address first and its `/64`, a password shown once that gates the plan, kept users, the list off behind Cloudflare, the live reading on a row and in the sheet, and an e2e flow to the plan, which shows neither the password nor its hash.
