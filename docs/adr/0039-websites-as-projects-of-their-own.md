# 0039. Websites, as projects of their own

- Status: Accepted
- Date: 2026-11-07

## Context

Issue #206 asks for websites: a static folder, a PHP site, or a reverse proxy to a URL or a port, each a plan with undo, with its domain, certificate or tunnel from the domain flow that routes already have, and its files in the server's file browser. The owner serves a PHP 8 site behind 1Panel's OpenResty today, and has a few reverse proxies there.

The PRD calls a website a kind of service (principle 1, and the object model), and `Service.runs` was left open for it. [ADR 0024](0024-the-caddy-ingress-driver.md) keeps the ingress to what a route names, a service's port or a port on the server, and nothing else: no upstream address, no header, no raw config. [ADR 0029](0029-a-catalog-format-fed-from-other-catalogs.md) installs an app as a project of its own, pinned to what it came from, so one change makes it and its undo takes it away.

## Decision

### A website is a project, and what it serves is written from its settings

A website is a project of its own, as a catalog install is. `Project.website` says what it serves, in the words a person chose: a URL, a port on the server, a folder, a PHP version. Its one environment holds what serves it: a route for its domain and, where one is needed, a service. `WebsiteService` plans making a website, changing what it serves and deleting it, each as one change, so a plan shows all of it and an undo puts all of it back.

- **The settings are the source.** The control plane writes the route and the services from `Project.website` each time, so the panel never builds a container, and the CLI and API get the same result from the same few fields. The plan shows the settings that change on the project, and what that does to each service and route, as for any change.
- **Its domain is the route's.** A website's domain comes from the domain flow routes already have: a certificate from the ingress, or no certificate behind a tunnel or a proxy that ends TLS, with who the route answers and believes ([ADR 0030](0030-routes-behind-a-tunnel-or-a-proxy.md)). Changing what a website serves keeps its route's domain and what's in front as they are.
- **It isn't an app.** The Apps list leaves out what runs in a website's project, as the Websites list is where it's managed. Its services are still services, so health, deploys, logs, and the Compose export work on them unchanged.
- **The project is named after the domain,** such as `blog-example-com`, and shows as the domain.

### Each kind, as resources

| A website that | Is |
| --- | --- |
| Sends to a port on the server | A route with `host_port`, and no service. A person approves it, as ADR 0030's `server_port` host access says, with what listens there |
| Sends to a URL | A service, `web`, running Caddy as a small reverse proxy to that URL, and a route to it |
| Serves a folder | The same `web` service, serving the folder, which it mounts read-only, so a person approves the path |
| Runs PHP | `web`, serving the folder and passing PHP to a second service, `php`, which runs PHP-FPM of the version chosen. Both mount the folder at the same path, read-write for `php`, so a person approves it |

### A website's own web server, not the ingress

`web` runs the image the ingress pins (`caddy.Image`), which a server with a route has already, with a config written from the settings, on port 8080 in its container.

- **The ingress can't serve a folder without restarting every site on the server.** A container's mounts are fixed, so each new folder would recreate the ingress, with the downtime ADR 0024 says a restart has. A site's own container mounts its own folder, and nothing else is touched.
- **The ingress keeps naming nothing but a service or a port.** A URL in a route would let the proxy on the server's own network dial anything, its loopback and the cloud's metadata address included. `web` reaches what any container reaches, so sending to a URL needs no person, as a token can already deploy a container that does the same.
- **Some addresses no website sends to,** whoever plans it: a link-local one (169.254.0.0/16, fe80::/10), where a cloud keeps its metadata service, which hands the server's own credentials to whatever asks; the names and other addresses clouds give that service, such as `metadata.google.internal`; and ports 2375 and 2376 on any host, Docker's API, which runs anything as root on the server it answers for. Each is refused in words that say why, as ADR 0030 refuses host ports. A name that resolves to one of these isn't caught, as Dockwell sees only the name: keeping such names away from a server's containers is the operator's, as it is for any app. `docs/security/ingress.md` says so too.
- **A site gets what a service has, for free:** a health check, rolling replacement with no downtime, logs per deploy, rollback, and the export.
- **The config travels in the spec.** Caddy reads its config from a file, so the container's command writes it to a tmpfs and starts Caddy on it. A plan shows the lines that change, and drift reads it as it reads any command. It's built from typed settings, and a value that could break out of its place is refused, so nothing a request sends becomes a directive.
- **It runs with little.** Uid 65532, every capability Docker gives dropped but `NET_BIND_SERVICE`, tmpfs for what Caddy writes, no admin API and no automatic HTTPS, as TLS ends at the ingress. The image's Caddy carries that one as a file capability, and the kernel won't start a binary whose file capabilities it can't grant, so dropping it too stops Caddy starting, and adding it back would be host access. It believes the forwarded headers from private addresses alone, which is where the ingress reaches it from, so the visitor's address goes on to what it sends to.
- **It has a network of its own.** `web` joins the website environment's `site` network, and the ingress's, which its route adds, and never Docker's default bridge. The default bridge holds every service that names no network and every container run by hand, and `web` takes the ingress's word on who a visitor is from any private address, so one of those could say it was anyone. On its own network, what reaches it from a private address is the ingress. It may use 128 MB of memory, so one website can't starve another.
- **Its health check is a TCP connection.** A GET would ask the upstream, so an upstream that was down for a moment would fail the deploy and roll it back. So whether what it sends to answers is asked apart from deploys: the website's server asks it, as the ingress or `web` would, when the list is read, at most every 30 seconds, and the list says "Can't reach …" or "Nothing listens on port 3000" in place of a state. That reads the status line, and nothing of what answers.

### A folder, as it is

A folder's web server serves the files as they are, with its index for a folder, a 404 for what isn't there, and, for a single-page app, the top index in place of that 404. It mounts the folder read-only at `/srv`, so serving writes nothing and nothing served can change it, and an edit, in Files or anywhere, shows at the next request with nothing to deploy.

- **It reads the folder as uid 65532,** so the folder must be open to everyone, as a site's usually is. The forms read it through Files and say when it isn't, or has no index, with the `chmod` that fixes it. They read the folder above first, as a listing that comes to nothing goes on Activity as refused ([ADR 0032](0032-files-on-a-server.md)), and a folder that isn't there yet is the form's business, not Activity's.
- **A folder that isn't there yet is made by Docker,** empty and owned by root. Files edits what's there and makes no file, so a new site's first files arrive by `scp` or `rsync` for now, and the form says so.
- **Deleting the website leaves the folder as it is.**

### PHP runs beside the web server

`php` is PHP-FPM from a pinned image for each version on offer, listening on 9000 on the environment's own network, where `web` reaches it by name. Its workers run as the folder's owner, so files it writes keep the owner they had. Choosing another version is a change to `php`'s image, which rolls with no downtime. Each PHP site has its own `php`, so one site's code can't read another's folder, and one version's upgrade moves no other site.

## Consequences

- A site's web server costs about 10 MB of memory, and a PHP site's FPM more, by what its code uses. Both are outside the agent's budget, as the ingress is ([ADR 0005](0005-caddy-ingress.md)). Neither was measured here, as no machine this was written on has Docker.
- A request to a URL or a folder takes one more hop on the server, through loopback and a bridge.
- The control plane links no new module, and the agent build doesn't change: a website is services and a route, which agents run already.
- `Project.website` and what serves it stay in step. A change to what a project serves comes with the route `WebsiteService` writes for it, or the engine refuses it, so a plain edit of the project can't leave it saying one thing while its web server runs another. A setting someone changes on a website's service by hand, through the API, lasts until the website's settings next change. `ListWebsites` returns the config its web server really runs, and says when it differs from its settings, and the panel shows that config, marked.
- What isn't here yet: more than one domain for a website, redirects, and a config typed by hand. Each can be a setting later.
