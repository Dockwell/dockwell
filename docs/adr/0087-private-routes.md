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
- **A domain with a private route and another route answers 400 to a request target an app could read as another route's path**, ahead of all its routes, root included. Caddy's matchers see a path decoded and with its dot segments resolved, so `/x/;/../admin` is `/admin` to them and `/x/..;/admin` goes on as it is, while an app behind reads the target as the client wrote it: Tomcat, Jetty and Undertow take `;x` off each segment before they resolve `..`, IIS reads `\` as `/`, and Windows takes trailing dots and spaces off. Either way, a public route on the domain would carry what the app serves as the private path, or as a private root, as `/api/..;/secret` does below a public `/api`. So the domain checks the target as the client sent it (`{http.request.orig_uri}`, which Caddy 2.11.7 holds as it came, with nothing in it expanded), and the path decoded but not cleaned (`{http.request.orig_uri.path}`). It refuses:
  - `;`, `\` or `#` anywhere in the path, or a control byte;
  - an ASCII character written as `%XX`, such as `%2e`, `%2f`, `%3b`, `%5c`, `%20`, `%23`, `%3f` or `%25`, and a byte past ASCII written raw;
  - a `.` or `..` segment, or a segment that ends in a dot (`.well-known` is fine);
  - a path that starts with `//`, which Python's `urlparse` and Node's `URL` read as a host, so `//a/admin` is `/admin` to them;
  - bytes that aren't UTF-8 once decoded, overlong ones among them;
  - a character no one sees (a zero-width space, a byte order mark, a bidirectional control, a tag character, a variation selector other than an emoji's U+FE0E and U+FE0F, a Hangul filler), Unicode white space at the end of a segment, and look-alikes of `/`, `\`, `.` and `..` (full-width `／` `＼` `．`, `∕`, `∖`, `⁄`, `﹨`, `‥`, `︰`, `…`, `︙`, `¥`, `₩` and the like), which some servers fold to ASCII;
  - `:$`, as NTFS names a file's stream;
  - anything that isn't `*` or a path, as after a scheme and host.

  It takes letters, digits, `-._~!$&'()*+,=:@|^[]`, and UTF-8 percent-encoded, which is what a browser writes for a site's own links, a non-English path among them, and anything in the query. The cost is on such a domain alone: a `;jsessionid` in a path gets 400, which a Java app falls back from to its cookie, and so does a link with an encoded space or slash, such as `/files/a%20b.pdf`. A private part with links like those belongs on a domain of its own. A domain with one route has nothing for a request to fall to, and one with no private route is as it was. Not caught: letters written full-width (`ａdmin`), IIS's short names (`ADMINI~1`), and what an app does with a path after it reads it. On a domain behind a tunnel, it answers only who the domain's routes answer, so a stranger still can't tell they're there. It's one route Dockwell derives on each of Caddy's servers on every change, as it does the 404 for a path no route covers. Checked on Caddy 2.11.7 with each form #606's review and its final check sent.
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
- **A private route is asked its odd spellings too**, sent as written, of its path `P`, or `/` for a root: `P;x`, `/;x/P`, `/x/..;/P`, `/%5cP`, `/x/;/../P`, `/x/.;/../P`, `/x/%3b/../P` and an overlong `/`; and from below each other path `Q` on its domain, `Q/..;/P`, `Q/.;/../P` and `Q/x%5c..%5c..%5cP`. Let in any way reads as let in; 400 is kept out.
- **What it says:** kept out (403), asked for a password (401), let in (below 400, which a private route shouldn't be), another answer, or that it **can't tell**: where who it asks as is on the list, or the route answers only proxies the server isn't.
- Once Caddy doesn't answer one, the rest of that pass aren't asked. A request with no `Authorization` costs Caddy no bcrypt.

### Making it private, in the panel

From a lock beside each route, on Domains and on an app's page, **Make it private** opens a sheet (and `?private=<route id>` on Domains links to it), defaults first:

- **What a stranger gets** leads it: the live reading for each of the route's servers, and what applying the sheet would make it.
- **Other ways in** follow it, as a route keeps out only who comes through it: each port the app publishes on every address of its server, with the way to bind it to the server alone, and each other route to the app that lets anyone in, with Make it private for that one. A row on a list says the first of them in place of what a stranger gets: "Private here · open on port 8080".
- **Only these addresses**, with your own address first, as the control plane saw your session, and its `/64` for IPv6, written as the server writes it back. A network is everyone at that office or on that line, so with one first, a password comes with it. Through the SSH tunnel that's loopback, which the sheet says can't be offered, and a new private route starts with a password instead. Behind Cloudflare's preset, or a tunnel the route doesn't name, the list is off, with why.
- **Ask for a password**, with a user name and a password Dockwell makes in the browser: 24 letters and digits no one mixes up, shown once with a copy button, and Review plan waits for "I've kept a copy". A user the route has keeps their password unless you ask for a new one.
- Applying it is a plan, as any change; the panel sends the whole route, as `DomainRoute` now says its list, its users' names and its trusted proxies.

### Signing in with a provider

Proposed with #606's review under way; its code comes in a pull request of its own, after that review, and nothing below depends on what the review changes in the slice before it.

A list keeps out who isn't at the office, and a password is shared. What a team wants in front of an admin panel, a dashboard or a staging app is to sign in as themselves, once, with the provider they already have: a self-hosted Authentik, Authelia, Keycloak, Pocket ID or Zitadel, or Google or Microsoft Entra. All of them speak OpenID Connect. Caddy's stock image, which Dockwell pins by digest (ADR 0024), has no OIDC module.

#### Where it runs: the route's agent

Caddy asks the route's agent before each request, over a socket in Caddy's run directory, as it reaches the panel's (ADR 0024), with the `reverse_proxy` that Caddy's `forward_auth` is made of:

- **Signed in:** the agent answers 200 from a cookie it signed, with no call to anyone, and Caddy goes on to the app with who it is.
- **Not signed in:** the agent's answer goes back to the visitor as it is: a redirect to the provider, or 401.
- Checked on Caddy 2.11.7: a 302 and its `Set-Cookie` reach the visitor unchanged, and on a 200 the app gets the agent's header, over one a visitor wrote, with the visitor's own method and path.
- **Only signing in talks to the provider,** from the server. If the agent is down, Caddy gets no answer, and the visitor gets 502: closed, never open.

Not chosen:

- **A Caddy plugin,** such as caddy-security: Dockwell would build and pin its own Caddy, where it pins the stock image today.
- **oauth2-proxy beside Caddy on every server:** a third-party image with its own configuration and releases, and tens of megabytes on each server, where an agent runs on every server already.
- **The control plane in the path:** every sign-in, or every request, would hang on it being reachable from the visitor, where a panel is often reached only through an SSH tunnel.

#### A provider is an install-wide resource

`signin.provider`, saved by an admin and named by the projects that may use it, sealed at rest, as a registry login is ([ADR 0086](0086-registry-logins-for-private-images.md)):

| Field | What it is |
| --- | --- |
| `display_name` | What the panel calls it, such as "Our Authentik" |
| `issuer` | The provider's issuer, such as `https://auth.example.com/application/o/dockwell/` |
| `client_id` | The client Dockwell is at the provider |
| `client_secret` | Its secret (sensitive). Empty for a public client, which PKCE protects alone |
| `scopes` | What it asks for beyond `openid email profile`, such as `groups` |
| `groups_claim` | The claim that lists someone's groups. Empty takes `groups` |
| `any_account` | Every account at this provider is your team's, as at an Authentik you run. Refused for an issuer anyone can have an account at, such as Google's or Entra's common one |
| `projects` | The projects whose routes may use it, by ID |

- **Checked live as it's saved.** The control plane reads the issuer's discovery document through `netguard`, so never a link-local address or a metadata service, and refuses what it can't use, saying why:
  - an issuer that differs from what was asked for;
  - an endpoint that isn't HTTPS;
  - no authorization code flow, or no PKCE with S256;
  - no signing algorithm Dockwell takes.
  The form says what it found: "Found auth.example.com: it signs with ES256". A self-hosted provider on a private network is fine, as an admin names it.
- **A secret is checked at the first sign-in,** as nothing else proves it. After applying, the sheet offers to try signing in.

#### A route asks for it

`RouteAccess.sign_in` (3), a `RouteSignIn`:

| Field | What it is |
| --- | --- |
| `provider` | A `signin.provider` its project may use, by name |
| `emails` | Who may sign in, by email, as the provider says it, verified |
| `email_domains` | Or anyone at a domain, such as `example.com` |
| `groups` | Or anyone in a group, from the provider's groups claim |
| `session_hours` | How long a sign-in lasts. 0 takes 24, and it takes at most 720 |
| `generation` | Raising it signs everyone out, as Sign everyone out does |

- **Someone must be named,** by email, domain or group, unless the provider says every account is yours (`any_account`). An empty list at Google would let in anyone with a Google account.
- **A list goes in front,** as with a password: off the list is 403, before any redirect.
- **With users too,** a request that carries Basic credentials is checked against them, and any other goes to sign in. So a script or a webhook uses a password, and people sign in. Neither is asked for twice.
- **Sign-in needs HTTPS at the browser,** for its Secure cookie. It's refused on a route that answers anyone over plain HTTP (`plain_for_anyone`). It works with a certificate, and behind a tunnel or a proxy that ends TLS and that the route names, from whose word the route reads `https`.
- **Who may sign in is checked at sign-in.** Emails and domains are checked again on every request, from the cookie, so taking someone off works at once. A group's change at the provider counts at their next sign-in, which `session_hours` bounds.

#### The flow

OpenID Connect's authorization code flow, with PKCE (S256), `state` and `nonce`. Everything a visitor can send is checked on the server:

- **Not signed in, a navigation** (a GET or HEAD that takes HTML): a redirect to the provider's authorization endpoint. A flow cookie, `__Host-dw_flow_<route>`, holds `state`, `nonce`, the PKCE verifier and the path to come back to (a path on this origin, never a URL). It lasts 10 minutes and is signed.
- **Not signed in, anything else** (a POST, a fetch, a WebSocket): 401 and nothing else, so a script fails plainly, rather than following a redirect to a sign-in page.
- **The callback,** at `<route's path>/.dockwell/signin/callback`:
  1. `state` must match the flow cookie.
  2. The agent exchanges the code at the token endpoint, with the client secret as discovery says to send it.
  3. It checks the ID token itself:
     - the algorithm is RS256, PS256, ES256 or EdDSA, never `none` or an HMAC;
     - the key comes by `kid` from `jwks_uri`, cached, and fetched again once for a `kid` it hasn't seen, at most once a minute;
     - `iss` is exactly the issuer;
     - `aud` holds the client ID, and `azp` is the client ID where `aud` holds several;
     - `exp`, `nbf` and `iat` hold within two minutes;
     - `nonce` matches the flow cookie.
  4. It reads `sub`, `email`, `email_verified` and the groups claim from the ID token.
- **The session cookie,** `__Host-dw_<route>`, made fresh at each sign-in: HttpOnly, Secure, SameSite=Lax, Path=/, signed with HMAC-SHA256. It holds the route, its generation, `sub`, the email, the route's groups that matched, and when it was made and runs out. `__Host-` means no other subdomain can set or replace it.
- **Signing out** at `<route's path>/.dockwell/signin/out` clears it, here alone. A provider's own sign-out is a later step.
- **`/.dockwell/signin/` under a private route's path is Dockwell's.** An app's own path there is out of reach while sign-in is on.

#### What the app gets

- On a signed-in request, `X-Dockwell-User` (the email) and `X-Dockwell-Subject` (`sub`), after Caddy takes out any a visitor sent, as it does the forgeable headers (ADR 0024).
- Dockwell's cookies don't reach the app, so it can't log them. Caddy takes them out of `Cookie` with a regular expression and leaves the app's own; checked on Caddy 2.11.7.
- An app that signs people in by a header, such as Grafana's auth proxy, can use them. That's safe only where nothing reaches the app but Caddy, so the sheet says so when the app publishes a port of its own.

#### Keys and secrets on the server

- **The client secret** reaches only the route's agent, sealed to its stream, as a service's secret does (ADR 0022). The agent keeps it in its desired state, sealed (ADR 0054). Caddy's config never holds it, and nothing logs it.
- **The cookie key** is 32 random bytes the agent makes for each route and keeps in its data directory, readable by root alone. It never leaves the server.
  - `generation` derives the key from it with HKDF, so raising it ends every session and leaves the provider alone.
  - A route moved to another server signs everyone out, as its key stays behind.

#### Opening it again needs a person

`Opens` gains these facts:
- `sign-in-off` (taken off with nothing left in its place);
- `provider:<name>` (another provider);
- `email:<address>`, `email-domain:<domain>` and `group:<name>` (each one added);
- `any-account`.

An admin's edit of a provider that changes who can sign in to the routes that use it counts as `ACCESS_OPENED` on each of them: its issuer, its client, its groups claim, or `any_account` turned on.

#### Limits

- **Signing in reaches out boundedly.** A route exchanges at most 4 codes at once, and 30 a minute. A flood of made-up callbacks costs at most that many requests to the provider, and each first needs a flow cookie, which only a redirect makes.
- **Discovery and keys** are read through `netguard`, as the control plane reads them.
- **A cookie stays under 4 KB:** it keeps only the groups the route names.

#### The agent and its protocol

- An agent says `signs_in` (111) in its hello, and a route with `sign_in` is refused on one that doesn't, as `keeps_routes_private` is for the rest.
- The route's step carries the provider's client ID, scopes and groups claim, which the control plane fills into what the server gets, and the secret as a delivered value. A plan's fingerprint covers the provider's revision, as with a registry login.
- The agent checks tokens with Go's standard library alone (`crypto/rsa`, `crypto/ecdsa`, `crypto/ed25519`), so the agent build links no new module. `internal/agent/signin` joins its list of packages with what `make budgets` says it costs.

#### What a stranger gets

The stranger check asks as a navigation, and a redirect to the provider's own origin reads as the new outcome `SENT_TO_SIGN_IN` (6): "A stranger is sent to sign in at auth.example.com".

#### In the panel

- **Settings, Integrations, Sign-in providers,** beside registry logins. Adding one checks the issuer live and says the address to register at the provider, with Copy.
- **Make it private** gains **Sign in with**: a picker of the providers this project may use. With none, an admin is offered to add one, and anyone else sees who can.
- **Who may sign in** offers your own email first, and its domain, as the list offers your address.
- **The redirect address** to register at the provider is shown, with Copy.
- After applying, **Try it** opens the route in a new tab, and the live reading says what a stranger gets.

#### Tests, when it's built

- **Rules:** each refusal, on its own.
- **The flow,** against a fake provider in Go, refusing each bad token: the wrong `iss`, `aud` or `azp`; `alg: none`; HS256 signed with the public key; expired or not yet valid; the wrong `nonce` or `state`; an unknown `kid`, fetched once. A good one is taken, and its keys rotate.
- **Real Caddy:**
  - a redirect, and 401 for a POST;
  - a callback, then the cookie;
  - the user header reaching the app, a forged one gone, and Dockwell's cookie gone;
  - a list in front;
  - a password for a script beside sign-in;
  - an agent that's down, as 502.
- **The gate,** on both stores: each new fact, and a provider's edit, needs a person.
- **An older agent** is refused.
- **The panel** and an e2e flow to the plan.

#### What isn't in this slice

- A provider's own sign-out, and refresh tokens.
- The userinfo endpoint, for a provider that keeps groups only there.
- GitHub's OAuth, which isn't OpenID Connect.
- A page to choose among several providers.
- Rules by path.
- SAML.

## Consequences

- A request that isn't cached costs one bcrypt check at cost 10; a flood of wrong passwords can keep a core busy. Caddy has no limit on attempts; a list in front stops strangers before any check. The threat model says so.
- A route with access is a few handlers longer in Caddy's config.
- A route spec with users is sealed at rest, so what reads route specs reads them through the keeper.
- A password can't be shown again. A forgotten one is replaced.
- How an app knows who signed in comes with signing in with a provider, as `X-Dockwell-User`.
- What isn't here: "on the list, or with a password", a list that names a country or a network by name, limits on failed attempts, and exceptions for a path under a private route other than a route of its own.

## Tests

- **Rules:** each refusal on its own, and that a password is hashed, kept, and never comes back in a read (`route_test.go`).
- **The gate,** on both stores: each way of opening a route needs a person, read across a delete and a new route, and each way of closing it doesn't.
- **`DOCKWELL_TEST_CADDY`:** 403 off the list, on IPv4 and IPv6; a forged `X-Forwarded-For` from a peer the route doesn't name ignored; a named proxy's word taken; 401 without a password, 200 with it, and `Authorization` not reaching the app; a hand edit of the list or a hash reading as drift; no reload dropping a request.
- **`DOCKWELL_TEST_PEBBLE`:** a certificate for a private route over HTTP-01 and TLS-ALPN-01.
- **The stranger check,** on real Caddy: 403 off a list over HTTPS, 401 for a password over plain HTTP, through a tunnel on the server as a documentation address, can't tell where the server is on the list, and a change read at once.
- **From #606's review:** a private path or its root moved alone to another server is refused, and moving them together isn't (both stores); a moved route with kept hashes says why it can't go; each of a private path's other spellings gets 403 on real Caddy, and a sibling that only starts the same doesn't; five names that aren't users cost no hash; a private route's answers say `Cache-Control: private`; the stranger check reads a public `;x` spelling as let in; the panel names a published port and another public route, and pairs a network with a password.
- **From #606's final check:** on real Caddy, each target that Caddy cleans to a public path and Tomcat reads as a private one gets 400 or 403, as does each below a public path to a private root, and each low (a trailing dot or space in a segment, overlong UTF-8, full-width `／` `＼`, `%23`, `%3f`, a zero-width space); a domain with one route, or none private, takes them as before; the stranger check reads `/x/;/../admin` and `/api/.;/../` as let in on a build without the 400.
- **From #606's last check:** on real Caddy, a target that starts with `//` gets 400, as `//a/admin` and `//api/secret` below a public path to a private root would reach the private path in an app that reads them with `urlparse` or `URL`, and so does `︰`, `…` or `︙` between segments; the stranger check asks `//x/P` and `/x/︰/P`.
- **The panel:** your address first and its `/64`, a password shown once that gates the plan, kept users, the list off behind Cloudflare, the live reading on a row and in the sheet, and an e2e flow to the plan, which shows neither the password nor its hash.
