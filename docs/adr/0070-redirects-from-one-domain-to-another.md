# 0070. Redirects from one domain to another

- Status: Proposed
- Date: 2026-11-10

## Context

A site at `example.com` is also expected to answer on `www.example.com`, or the other way round, and a site that moved sends its old domain, or an old section, to its new one. Today Dockwell can't do either. A route sends a domain to a service's port or to a port on the server, and nothing else ([ADR 0024](0024-the-caddy-ingress-driver.md)). So a person runs a second container to answer with a redirect, or does without. 1Panel offers both: a website has a primary domain and others, and a redirect from a domain or a path, 301 or 302, keeping the path or not. Issue #426's audit of Websites lists it as missing.

What there is to build on:
- A route is one domain, or one path under it ([ADR 0024](0024-the-caddy-ingress-driver.md)). It gets its certificate from the ingress, or none behind a tunnel or a proxy that ends TLS, with who it answers and believes ([ADR 0030](0030-routes-behind-a-tunnel-or-a-proxy.md)). The Domains list, DNS checks, certificate states and alerts all read routes.
- Every change to a route is a revision with a plan ([ADR 0009](0009-resources-revisions-and-plans.md)). A website's route is written from its settings, so the website is the source ([ADR 0039](0039-websites-as-projects-of-their-own.md)).
- ADR 0069, proposed in #487, puts protection on a route as a field, `access` (9). Every whole-route writer keeps a field it doesn't set.
- An agent announces in `Hello` what it can do, and the control plane refuses to plan what an older agent would get wrong, as `writes_references` does.

## Decision

### A redirect is a route that answers with one

`Route` gains `redirect` (field 10), a `RouteRedirect`:

- `to` (1): where it sends, an absolute `https://` or `http://` URL, such as `https://example.com` or `https://example.com/blog`.
- `temporary` (2): false by default, which answers 301 Moved Permanently. True answers 302 Found.
- `drop_path` (3): false by default, which keeps what follows the route's own path, and the query. True sends every request to `to` as it is.
- `keep_prefix` (4): for a route with a path, false by default, which strips the route's own path before what's kept goes after `to`. True keeps the whole path as it came.

What "keeping the path" sends, by example:

| Route | `to` | Request | Goes to |
| --- | --- | --- | --- |
| `www.example.com` | `https://example.com` | `/a?b=1` | `https://example.com/a?b=1` |
| `example.com/old-blog` | `https://blog.example.com` | `/old-blog/post?b=1` | `https://blog.example.com/post?b=1` |
| `example.com/old-blog`, `keep_prefix` | `https://blog.example.com` | `/old-blog/post` | `https://blog.example.com/old-blog/post` |
| `example.com/blog` | `https://example.com/articles` | `/blog/x` | `https://example.com/articles/x` |
| `promo.example.com`, `drop_path` | `https://example.com/?from=promo` | `/anything` | `https://example.com/?from=promo` |

Stripping is what a section that moved wants, and what 1Panel's path redirect does (`rewrite ^/old-blog(.*)$ target$1`), so its importer maps one to one.

A route with `redirect` sends to nothing. It names no `service`, `port` or `host_port`. Everything else a route has applies as it is:
- Its domain and path are claimed as any route's are, so a redirect and a service can't both answer the same address. A redirect can cover a path, such as `/old-blog`, beside a service that answers the rest of the domain, as the longest path answers first.
- `tls`, `trusted_proxies` and `plain_for_anyone` mean what they mean for any route. With `trusted_proxies`, the route's belief step is written as for a route that proxies, so it reads back without drift, and a site behind `cloudflare` gets its www as its own front.

It is a field rather than a kind of its own because a redirect needs everything a route already has: its domain claimed, a certificate or none, who it answers, and its place in the Domains list, DNS checks, certificate alerts and the 1Panel importer. A kind of its own would repeat each of those, or leave them out.

**Why 301 and 302, and not 307 and 308.** A browser keeps a 301 and asks again on its own; a crawler moves a page's standing to the new address. That is what www and a moved domain want, and what 1Panel and Caddy's own `redir permanent` answer. 307 and 308 also keep a POST's method and body. A form posted to the old domain is rare, and Caddy's own redirect from plain HTTP to HTTPS, the first hop below, is already 308. A later `code` field can offer 307 and 308 without changing what these two mean.

### Whole-route writers keep it

An edit that leaves `redirect` unset and names no `service` or `host_port` keeps the redirect the route has. This is how ADR 0069 keeps `access`, and it's the route kind's `resource.Keeper`, so it holds for every caller that writes a whole route without knowing the field:
- the panel's "Move behind what's in front", which rebuilds every route of a domain from its path, service, port and front;
- an older CLI, and a token's `--edits @route.json` written before this release;
- `website.Resources`, for the routes a website writes.

An edit that names a `service` or a `host_port` turns a redirect back into a route that sends there, and the plan says "www.example.com stops redirecting, and sends to web". `redirect` set with no `to` is refused. Adopting drift reads `redirect` back from Caddy, and undo and rollback put back the revision's spec as it was, so none of them asks the keeper. A test in slice 1 shows a redirect surviving each.

### `to` is normalized, then checked

The route kind's `Validate`, which the agent runs too, first normalizes `to`, and stores what it made:
- It lowercases the scheme and the host, and drops a port that is the scheme's own, `:443` or `:80`.
- When what follows is kept, it trims a `/` at the end, as a request's own path starts with one. `https://example.com/` and `https://example.com` send `/about` to the same place.
- It refuses a host that isn't ASCII, with its fix: "Write the domain in its xn-- form, such as xn--bcher-kva.example". So the agent links nothing new.

Then it checks:
- An absolute URL with `https` or `http` and a host, at most 2048 bytes once normalized.
- No user or password in it, no fragment, no space or control character, and no `{` or `}`, which Caddy reads as a placeholder. Caddy escapes a request's own path, so `{http.request.uri}` can't carry one in.
- A query only with `drop_path`, as appending a request's path after a query has no sensible answer.
- Any domain, Dockwell's or not. A route can already send its domain to any container, which could answer with a redirect to anywhere. So pointing a domain elsewhere needs no new permission, and a token that may change a route may make one a redirect.

### Loops are refused by which route would answer

A redirect loops when the address it sends to is answered by itself, or by redirects that lead back to it. Whether a route answers an address depends on the other routes of its domain, as the longest path answers first. So the check is the control plane's, at plan time, across the install's routes as the change leaves them:
1. It takes where a request lands: `to`'s host and path. For a kept path, that's `to`'s path with what follows the route's own path after it, which is answered by the same route as `to`'s path itself unless a longer route covers it.
2. It finds the route of that host that answers that path, by the longest path.
3. It refuses when that route is this one, or a redirect that leads back here within four hops. The scheme counts for nothing, as Caddy sends plain HTTP to HTTPS on a domain with a certificate.

"www.example.com would send visitors round in a circle: example.com redirects back to it. Send it to the site, or change example.com first." Or for one route: "example.com would send visitors round in a circle, back to itself at /blog. Send it to a path another route answers, or to another domain."

This takes `example.com/blog` to `https://example.com/articles`, the most common path redirect. It refuses `example.com` to `https://example.com/blog` with `drop_path` when nothing else answers `/blog`, and takes it when another route of the domain does.

The check is a new kind hook, `resource.AcrossChecker`, which the revision engine calls for each route a change makes or edits, with every route of the install as the change leaves them. It reads routes through the store's one route reader once ADR 0069's lands, and whichever of the two lands second adapts. The agent keeps only the check it can make alone, which can't be wrong: a redirect whose landing is exactly its own path, as `example.com/x` to `https://example.com/x`, loops whatever else there is.

### How Caddy serves it

The agent writes a redirect route as it writes any route, with the same matchers and `@id`, and with a `static_response` last in place of `reverse_proxy`:
- It answers the status, with a `Location` of `to` followed by `{http.request.uri}`, which Caddy fills with the request's escaped path and query, or `to` alone with `drop_path`.
- For a path route that strips its prefix, a `rewrite` with `strip_path_prefix` set to the route's path comes before it, so `{http.request.uri}` reads what follows it.
- A 301 says `Cache-Control: max-age=86400` (below). A 302 says nothing of caching, so browsers ask again each time.
- It sends no `Strict-Transport-Security`, as no route of Dockwell's does.

**The passes that rewrite a proxy's handler leave a redirect alone.** The driver's own passes over routes it wrote are for routes that proxy: `withTrust`, which sets the request headers a proxy sends on, and `alreadySo`, `headersAreOurs`, `transportIsOurs`, `throughFile` and `dialed`. A redirect's handlers are written whole by `routeJSON` and compared whole on read-back, so `withTrust` skips them, and the typed handler carries `status_code` and the response headers. A real-Caddy test pins it: a server where another route trusts a proxy, so `withTrust` runs on every change, changes that other route, and the redirect still answers its status and `Location`, path and query kept, with no drift. Slice 1 found the failure this guards against: `withTrust` had written request headers over a `Location`.

The Caddy driver reads a redirect back as it reads every route. Anything in its handlers but Dockwell's shape reads as a hand edit, as drift: another status, another header, a body, a second location, a placeholder or an upstream.

**Two hops from plain HTTP.** `http://www.example.com/a` takes a 308 to `https://www.example.com/a`, which is Caddy's own redirect, and then the 301 to `https://example.com/a`. hstspreload.org requires `http://` to go to `https://` on the same name first, so one hop straight to another domain would rule preload out. The cost is one request, once per visitor. A site whose main domain is `www.example.com` can't be preloaded either way, as preload needs the bare domain's HTTPS answer to carry HSTS, and that answer is the redirect. A later field can add HSTS to a redirect.

### Undo and a browser's cache

A browser keeps a 301 with no cache headers for as long as it likes, so an undo would never reach someone who followed one. A permanent redirect says `Cache-Control: max-age=86400`, so browsers ask again within a day. Crawlers still treat it as permanent. The plan for a permanent redirect says so: "Browsers keep it for up to a day, through an undo too." A redirect to try something out is Temporary, one step away.

### How it gets a certificate

A redirect route is a route, so its domain is in a host matcher, and Caddy's automatic HTTPS gets it a certificate the same way: HTTP-01 or TLS-ALPN-01, once its DNS points at the server. Caddy answers ACME's challenges before any route runs, so the redirect never stands in their way. Behind a tunnel or a proxy that ends TLS, it gets none, as any such route. Until DNS points here, Caddy retries with its own back-off, and the Domains list says the state it says for any route: "Not set yet" for DNS, and "Issuing" for the certificate.

### How it plans

A redirect is a route's spec, so proposing, planning, applying, undo, rollback and drift are a route's, and the agent changes that one route in Caddy, which restarts nothing. The plan says it in words, as `PlanStep.redirects` (240): "www.example.com redirects to https://example.com, permanently, keeping the path. Browsers keep it for up to a day, through an undo too." A route that was serving a service and becomes a redirect says both: "app.example.com stops sending to web, and redirects to …". Where `to` lands is checked on the control plane too, across the install: "served by hz-1", "not served by this install" or "without HTTPS", as `Judge` on an agent sees only its own server.

**A redirect left with nothing to land on is named.** A plan that deletes or moves the route a redirect lands on says so, as a plan names what visitors find when a route goes: "www.example.com still redirects here, so its visitors land on nothing · Delete it too".

### Protection and a redirect (with ADR 0069)

The same rule, word for word in both records:

> A redirect route carries no `access`. Setting `redirect` on a protected route takes clearing its `access` in the same edit, and the refusal says so: "admin.example.com asks for a password, and a redirect has nothing behind it to protect. Clear its access in the same change." That change makes the route's paths less protected, so it needs a person, as `AccessOpensRoute` does, and the plan names the paths: "admin.example.com no longer asks for a password, and redirects to https://example.com." A redirect isn't "an open route" for ADR 0069's delete rule, as it serves nothing behind it. A path redirect under a protected domain is worded as what it does: "app.example.com/old redirects anyone to https://example.com/new".

A redirect holds no users, so under ADR 0069 it's never sealed.

### Older agents, and going back

`Hello` gains `serves_redirects` (field 240, in the websites area's block, 240 to 249). The control plane refuses a route with `redirect` on a server whose agent doesn't announce it, at planning, at applying and at reading back, as `writes_references` is enforced per call. So a plan made while a server was offline is refused when it's applied there: "hz-2's agent is older than this release, so it can't serve www.example.com's redirect. Update its agent, then plan again", with that server's Update action. If a server's agent stops announcing it, as after a downgrade, the route's row says "Not served on hz-2 · Update its agent".

Going back to a release before this one takes removing redirects first. An older control plane keeps field 10 as an unknown field, refuses to plan that route again ("needs a service"), and lists it with no service. An older agent reads the redirect's handlers as a hand edit. The release notes say so.

### A website's other domains

A website says which domains redirect to it: `Website.redirect_from` (field 4), a list of domains. Each is a route in the website's environment, written by `website.Resources` as the site's own route is:
- It redirects permanently to the site's domain, keeping the path, with the same `tls`, `trusted_proxies` and `plain_for_anyone` as the site's route. Its `to` is `https://` and the domain, or `http://` for a site served as `plain_for_anyone`. Behind a tunnel, visitors reach the tunnel over HTTPS, so `https://` is right there too.
- Its name comes from its domain, as a project's does (`website.ProjectName`'s trimming to a valid name), with a number on a clash.
- The website owns only the routes its list writes. Removing a domain from `redirect_from` deletes the route the earlier list wrote for it, by name, and never a route a person added by hand, such as a path redirect.

So adding or removing one is an `UpdateWebsite`, one change, with undo. Moving the site behind a tunnel moves its redirects with it, and deleting the site deletes them. The site's domain can't be in its own list, and each domain is claimed as any route's is. Copying the front can need a person, as a route's front does: copying `plain_for_anyone` is host access for www too, and moving a www off a certificate it had is `https_off`. The plan says so. Behind a tunnel, the plan says "Add www.example.com to your tunnel too" as a state, and the Domains row shows it when its check fails.

An app's domain gets a redirect as a route, through `ProposeChange`, as any route does. It needs no new method.

### How the panel offers it

The panel offers the redirect most sites want, already decided, and the rest one step away:
- **New website, a website's Domain panel, and an app's Add a domain** offer the other half of a bare domain as one checkbox: "Also answer on www.example.com" for a site at `example.com`, and "Also answer on example.com" for a site at `www.example.com`. Checked, it adds the domain to `redirect_from` for a website, or a redirect route to an app's environment, so one plan makes the redirect and its certificate comes with it.
- **Where the checkbox starts.** `CheckDomain`'s answer gains `companion` (field 2): that other name, worked out by the control plane from the public suffix list (`golang.org/x/net/publicsuffix`), so `example.co.uk` gets `www.example.co.uk` and `blog.example.com` gets none. It also gains `companion_check` (field 3): what DNS says of that name, and whether another environment holds it already.
  - The box starts checked when the companion answers the way the site's own domain does: pointing at the server, or behind a tunnel or Cloudflare's proxy, pointing where the site's domain points. It starts unchecked otherwise, with the DNS record to make beside it and its live state.
  - When another environment holds the companion, the box is off, with a state and an action: "www.example.com is served by shop · Open".
  - The companion's lookup is part of the same `CheckDomain` call, so it counts once against the call's budget of 60 a minute, and is skipped when there's no companion.
- **Changing a website's domain** offers to keep the old one, checked: changing `example.com` to `example.org` offers "Send example.com here", which adds it to `redirect_from` in the same plan.
- **"Make www.example.com the main domain"** swaps a site's domain and its www in one `UpdateWebsite`, which slice 2 tests against real Caddy, as for a moment two routes may match one name.
- **"Send another domain here"** on a website's Domain panel takes a domain, such as an old one, shows its DNS record, and adds it to `redirect_from`.
- **The Domains list and a domain's page follow the mocks.** `Domains-List` folds `www.example.com` into `example.com`'s row as "Redirects: www → example.com", and shows a failing www certificate or DNS on that row, as a folded row would otherwise hide it. `Domain-Traffic` lists the redirect as a route row: "Redirect to example.com · 301 · keeps the path and query". A redirect that lands on no domain of the install has a row of its own: "Redirects to example.org".
- **Under Advanced,** the route's own fields: temporary, dropping the path, and keeping a path route's prefix. Add a domain from the Domains list gains "Redirect to a URL" beside "Send to an app", for a domain that belongs to no site, such as a vanity domain.

### In slices

1. **The route:**
   - `Route.redirect` with its normalizing and checks, and the loop check across the install.
   - The keeper, so whole-route writers keep it.
   - The Caddy driver writing and reading it, with the prefix stripped, the cache header, and the passes that leave it alone.
   - `serves_redirects` and its refusal for older agents.
   - The plan's words, `DomainRoute.redirect` (field 10) in `ListDomains`, and the Domains row.
   - Tested against real Caddy and Pebble: a redirect route gets its certificate, keeps the path and the query, survives `withTrust` on a server that trusts a proxy, and reads back without drift.
2. **A website's other domains, straight after:** `Website.redirect_from`, `website.Resources`, `CheckDomain`'s `companion`, the checkbox in New website and on the site's Domain panel, keeping the old domain on a change, "Make www the main domain", and "Send another domain here".
3. **Apps and the rest:**
   - The checkbox and "Redirect to a URL" in Add a domain.
   - The Domains fold and the domain page as the mocks have them.
   - Temporary, dropping the path and keeping the prefix under Advanced.
   - The delete plan's "still redirects here".
   - The 1Panel importer bringing a site's redirects across.

### Answers to the review's questions

1. **Slice order:** slice 2, the checkbox, follows straight after slice 1, so the trial servers get something to click soon. Slice 1 alone is for the CLI, the API and the Domains row.
2. **ADR 0069's notice on a protected domain:** a path redirect is worded as what it does, "redirects anyone to …", in place of "answers anyone".
3. **ADR 0069's delete rule:** a redirect isn't an open route there, as it serves nothing behind it.
4. **Pending certificates:** a box ticked before DNS is there holds one of the server's pending domains until DNS arrives. That's fine: the box starts unchecked when DNS isn't there, and the Domains row says the state.
5. **An app's domain list:** yes. Slice 3 shows "www.example.com redirects here" under the domain a redirect lands on, on an app's page as on a website's.
6. **The importer:** 1Panel strips a path redirect's matched prefix when it keeps the path (`rewrite ^/path(.*)$ target$1`), which is this record's default, so it maps one to one.

`DomainRoute.redirect` takes 10, and ADR 0069's `DomainRoute` fields for users, allow and deny take 11 onwards.

## Considered

- **A kind of its own, `redirect`:** it would need its own domain claims, certificate handling, Domains rows, DNS checks and alerts, each kept in step with routes. Or it would have none, and a redirect's certificate would fail with nothing to say so.
- **A list of extra domains on a route,** as Caddy can match several hosts: one route answering both domains with the same content, with no redirect. It splits a site's standing with search engines between two addresses, and every place that reads a route's one domain would need to read several.
- **A redirect done by the website's own Caddy** (`web`, ADR 0039): only a website that has a `web` service could have one, not a site sending to a port on the server, nor an app. And the redirect's domain would still need a route, so a certificate.
- **Keeping a path route's prefix by default:** `{http.request.uri}` alone does that, with one handler. It sends `/old-blog/post` to `blog.example.com/old-blog/post`, which a section that moved almost never wants, and 1Panel strips.
- **A loop check on the agent alone:** it sees one server's routes, and whether a route answers an address depends on the others of its domain. It keeps the one check that can't be wrong.
- **A 301 with no cache header,** with every plan that removes one saying browsers keep it until their cache is cleared: honest, but it leaves undo reaching nobody who followed it.
- **307 and 308:** above. A later `code` field can add them.
- **Offering www for any domain,** without the public suffix list: `www.blog.example.com` is never what anyone wants, and `example.co.uk` would be taken for a subdomain.
- **Checking the box always:** a companion that doesn't point here yet would sit as "Issuing" in the Domains list until DNS changes, which reads as something wrong. Starting it from what DNS says does what most people want, and says why when it doesn't.

## Consequences

- A domain, or a path under one, can now answer with a redirect and nothing else, with its own certificate, plan and undo, from the CLI, the API and the panel.
- `http://` to a www name takes two hops before it lands, a 308 then a 301, which leaves HSTS preload possible for the site's own domain.
- A redirect to a domain that isn't Dockwell's is a plan like any other. The plan names where visitors go, and says so when that is plain HTTP or not served by this install.
- A permanent redirect is kept by browsers for up to a day, so an undo takes up to a day to reach someone who followed it.
- Each redirect's domain needs its DNS pointed at the server and a certificate, as any route's does. A box ticked before DNS is there shows as waiting until it is.
- `golang.org/x/net` becomes a direct dependency of the control plane, for its public suffix list. The agent build doesn't link it.
- An agent older than this release can't serve a redirect, and the control plane says so before it plans one there, and again before it applies one.
- Going back to a release before this one takes removing redirects first.
