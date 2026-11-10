# 0070. Redirects from one domain to another

- Status: Proposed
- Date: 2026-11-10

## Context

A site at `example.com` is also expected to answer on `www.example.com`, or the other way round, and a site that moved sends its old domain to its new one. Today Dockwell can't do either. A route sends a domain to a service's port or to a port on the server, and nothing else ([ADR 0024](0024-the-caddy-ingress-driver.md)). So a person runs a second container to answer with a redirect, or does without. 1Panel offers both: a website has a primary domain and others, and a redirect from a domain or a path, 301 or 302, keeping the path or not. Issue #426's audit of Websites lists it as missing.

What there is to build on:
- A route is one domain, or one path under it ([ADR 0024](0024-the-caddy-ingress-driver.md)). It gets its certificate from the ingress, or none behind a tunnel or a proxy that ends TLS, with who it answers and believes ([ADR 0030](0030-routes-behind-a-tunnel-or-a-proxy.md)). The Domains list, DNS checks, certificate states and alerts all read routes.
- Every change to a route is a revision with a plan ([ADR 0009](0009-resources-revisions-and-plans.md)). A website's route is written from its settings, so the website is the source ([ADR 0039](0039-websites-as-projects-of-their-own.md)).
- ADR 0069, proposed in #487, puts protection on a route as a field, `access` (9). Every whole-route writer keeps a field it doesn't set.
- An agent announces in `Hello` what it can do, and the control plane refuses to plan what an older agent would get wrong, as `writes_references` does.

## Decision

### A redirect is a route that answers with one

`Route` gains `redirect` (field 10), a `RouteRedirect`:

- `to`: where it sends, an absolute `https://` or `http://` URL, such as `https://example.com` or `https://example.com/blog`.
- `temporary`: false by default, which answers 301 Moved Permanently. True answers 302 Found.
- `drop_path`: false by default, which keeps the path and the query. `https://www.example.com/a?b=1` goes to `https://example.com/a?b=1`, and with `to` holding a path, to `https://example.com/blog/a?b=1`. True sends every request to `to` as it is.

A route with `redirect` sends to nothing. It names no `service`, `port` or `host_port`, and the route kind refuses one that does: "www.example.com redirects, so it sends to no service. Remove the service, or the redirect." Everything else a route has applies as it is:
- Its domain and path are claimed as any route's are, so a redirect and a service can't both answer the same address. A redirect can cover a path, such as `/old-blog`, beside a service that answers the rest of the domain, as the longest path answers first.
- `tls`, `trusted_proxies` and `plain_for_anyone` mean what they mean for any route.
- `access` (ADR 0069) is refused on a redirect, as nothing behind it needs protecting.

It is a field rather than a kind of its own because a redirect needs everything a route already has: its domain claimed, a certificate or none, who it answers, and its place in the Domains list, DNS checks, certificate alerts and the 1Panel importer. A kind of its own would repeat each of those, or leave them out.

**Why 301 and 302, and not 307 and 308.** A browser keeps a 301 and asks again on its own; a crawler moves a page's standing to the new address. That is what www and a moved domain want, and what 1Panel and Caddy's own `redir permanent` answer. 307 and 308 also keep a POST's method and body. A form posted to the old domain is rare, and Caddy's own redirect from plain HTTP to HTTPS, the first hop below, is already 308. A later `code` field can offer 307 and 308 without changing what these two mean.

### What `to` may be

The route kind checks `to`, and the agent checks it again, as it checks every route:
- An absolute URL with `https` or `http` and a host, at most 2048 bytes. No user or password in it, no fragment, and no `{` or `}`, which Caddy reads as a placeholder. A query only with `drop_path`, as appending a request's own path after a query has no sensible answer.
- Not itself. A redirect whose `to` is its own domain, and that keeps the path, sends a browser round in a circle, so it's refused: "www.example.com would redirect to itself. Send it to another domain." The same domain with `drop_path` and another path, such as `/` to `/blog`, is taken.
- Any domain, Dockwell's or not. A route can already send its domain to any container, which could answer with a redirect to anywhere. So pointing a domain elsewhere needs no new permission, and a token that may change a route may make one a redirect. The plan names where it goes, with a word for one that leaves for plain HTTP or for a domain no route here serves: "www.example.com sends visitors to http://example.net, without HTTPS."

### How Caddy serves it

The agent writes a redirect route as it writes any route, with the same matchers and the same `@id`, and one handler in place of `reverse_proxy`: `static_response`, with the status and a `Location` header. That header is `to` followed by `{http.request.uri}`, which Caddy fills with the request's path and query, or `to` alone with `drop_path`. On the plain-HTTP server, a route with TLS off gets the same handler behind the same reach as today. Caddy's own redirect from plain HTTP to HTTPS still runs first for a route with TLS on, so `http://www.example.com/a` takes two hops, a 308 to `https://www.example.com/a` and then a 301 to `https://example.com/a`. One hop would need a route on the plain server for every domain with TLS, which Dockwell doesn't write, and the extra hop costs a request once per visitor.

The Caddy driver reads a redirect back from Caddy, as it reads every route: a `static_response` with the status and a `Location` in Dockwell's shape is that redirect, and anything else in it reads as a hand edit, as drift.

### How it gets a certificate

A redirect route is a route, so its domain is in a host matcher, and Caddy's automatic HTTPS gets it a certificate the same way: HTTP-01 or TLS-ALPN-01, once its DNS points at the server. Caddy answers ACME's challenges before any route runs, so the redirect never stands in their way. Behind a tunnel or a proxy that ends TLS, it gets none, as any such route. Until DNS points here, Caddy retries with its own back-off, and the Domains list says the state it says for any route: "Not set yet" for DNS, and "Issuing" for the certificate.

### How it plans

A redirect is a route's spec, so proposing, planning, applying, undo, rollback and drift are a route's, and the agent changes that one route in Caddy, which restarts nothing. The plan says it in words: "www.example.com redirects to https://example.com, permanently, keeping the path", and for a change, "now temporarily", or "to https://example.org". A route that was serving a service and becomes a redirect says both: "app.example.com stops sending to web and redirects to https://example.com".

### Older agents

`Hello` gains `serves_redirects` (field 240, in the websites area's block, 240 to 249). The control plane refuses to plan a route with `redirect` on a server whose agent doesn't announce it, as that agent would write a route with nothing to send to: "hz-2's agent is older than this release, so it can't serve www.example.com's redirect. Update its agent, then plan again." The refusal comes with that server's Update action.

### A website's other domains

A website says which domains redirect to it: `Website.redirect_from` (field 4), a list of domains. Each is a route in the website's environment, named after its domain, such as `www-example-com`, written by `website.Resources` as the site's own route is. It redirects permanently to the site's domain, keeping the path, with the same `tls`, `trusted_proxies` and `plain_for_anyone` as the site's route. Its `to` is `https://` and the domain, or `http://` for a site served as `plain_for_anyone`. Behind a tunnel, visitors reach the tunnel over HTTPS, so `https://` is right there too. So:
- Adding or removing one is an `UpdateWebsite`, and one change, with undo.
- Moving the site behind a tunnel moves its redirects with it, and deleting the site deletes them.
- The site's domain can't be in its own list, and each domain is claimed as any route's is.

An app's domain gets a redirect as a route, through `ProposeChange`, as any route does. It needs no new method.

### How the panel offers it

The panel offers the redirect most sites want, already decided, and the rest one step away:

- **New website, and a website's Domain panel, offer the other half of a bare domain as one checkbox:** "Also answer on www.example.com" for a site at `example.com`, and "Also answer on example.com" for a site at `www.example.com`. Checked, it adds the domain to `redirect_from`, so one plan makes the redirect and its certificate comes with it. `CheckDomain`'s answer gains `companion` (field 2): that other name, worked out by the control plane from the public suffix list (`golang.org/x/net/publicsuffix`), so `example.co.uk` gets `www.example.co.uk` and `blog.example.com` gets none. It also gains `companion_check` (field 3): what DNS says of that name. The checkbox starts checked when the companion points at the server and unchecked otherwise, with the DNS record to make beside it and its live state, as the site's own domain has. Ticking it before DNS is there is fine: the certificate comes once it is.
- **"Send another domain here"** on a website's Domain panel takes a domain, such as an old one, shows its DNS record, and adds it to `redirect_from`.
- **The Domains list** says "Redirects to example.com" where an app or a service would be, with the state of the redirect's own domain and certificate.
- **Under Advanced,** the route's own fields: temporary, and dropping the path. Add a domain from the Domains list gains "Redirect to a URL" beside "Send to an app", for a domain that belongs to no site, such as a vanity domain.

### In slices

1. **The route:** `Route.redirect` and its checks, the Caddy driver writing and reading it, `serves_redirects` and the refusal for older agents, the plan's words, `DomainRoute.redirect` (field 10) in `ListDomains`, and the Domains row's "Redirects to". Tested against real Caddy and Pebble: a redirect route gets its certificate, keeps the path and the query, and reads back without drift.
2. **A website's other domains:** `Website.redirect_from`, `website.Resources`, `CheckDomain`'s `companion`, and the panel's checkbox in New website and on the site's Domain panel, and "Send another domain here".
3. **Apps and the rest:** "Redirect to a URL" in Add a domain, temporary and dropping the path under Advanced, and the 1Panel importer bringing a site's redirects across.

## Considered

- **A kind of its own, `redirect`:** it would need its own domain claims, certificate handling, Domains rows, DNS checks and alerts, each kept in step with routes. Or it would have none, and a redirect's certificate would fail with nothing to say so.
- **A list of extra domains on a route,** as Caddy can match several hosts: one route answering both domains with the same content, with no redirect. It splits a site's standing with search engines between two addresses, and every place that reads a route's one domain would need to read several.
- **A redirect done by the website's own Caddy** (`web`, ADR 0039): only a website that has a `web` service could have one, not a site sending to a port on the server, nor an app. And the redirect's domain would still need a route, so a certificate.
- **307 and 308:** above. A later `code` field can add them.
- **Offering www for any domain,** without the public suffix list: `www.blog.example.com` is never what anyone wants, and `example.co.uk` would be taken for a subdomain.
- **Checking the box always:** a companion that doesn't point here yet would sit as "Issuing" in the Domains list until DNS changes, which reads as something wrong. Starting it from what DNS says does what most people want, and says why when it doesn't.

## Consequences

- A domain can now answer with a redirect and nothing else, with its own certificate, plan and undo, from the CLI, the API and the panel.
- `http://` to a www name takes two hops before it lands, a 308 then a 301.
- A redirect to a domain that isn't Dockwell's is a plan like any other; the plan names where visitors go, and says so when that is plain HTTP.
- Each redirect's domain needs its DNS pointed at the server and a certificate, as any route's does. A box ticked before DNS is there shows as waiting until it is.
- `golang.org/x/net` becomes a direct dependency of the control plane, for its public suffix list. The agent build doesn't link it.
- An agent older than this release can't serve a redirect, and the control plane says so before it plans one there.
