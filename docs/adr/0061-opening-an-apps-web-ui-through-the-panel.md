# 0061. Opening an app's web UI through the panel

- Status: Proposed
- Date: 2026-11-10

## Context

Nineteen of the catalog's 24 entries publish their web UI on `127.0.0.1` alone, for one of two reasons: it signs in over plain HTTP (phpMyAdmin with a database's root password, Adminer, pgAdmin, Grafana, the RustFS console), or the first person to open it makes its owner account (Coder, Ghost, Gitea, Home Assistant, n8n, Plausible, Vaultwarden, WordPress and others). Eighteen of their "What to do first" notes then tell the person to run `ssh -N -L 8083:127.0.0.1:8083 you@your-server` by hand and open `http://localhost:8083`. The owner's verdict on #426 was that this is a missing feature, not a note: opening an app is a button.

1Panel opens such an app at the server's address and the app's port, with the firewall opened for it. That sends a root password over plain HTTP, reaches the app from anywhere the port does, and still fails behind a provider's firewall.

The options weighed:

- **Open the port for the person's address alone, for a while.** It's still plain HTTP across the internet, a provider's firewall in front still blocks it, and an address behind NAT is shared with strangers.
- **Make adding a domain the next step, with a subdomain filled in.** That makes the app public, which is right for sharing it and wrong for claiming its first account or for an admin tool. It stays the way to share an app, not the way in.
- **Proxy the app under a path of the panel's own origin**, such as `/open/<app>/`. Many apps break under a path they weren't built for, and the app's scripts would run on the panel's origin, where they could call the API as the person who opened it.
- **Proxy the app on an origin of its own, behind the panel's sign-in.** Chosen.

## Decision

### An origin of its own for each app

Every service someone opens gets a **label**: ten characters of base32 from an HMAC of the service's ID under a key the control plane keeps. It stays the same for that service, can't be guessed, and says nothing of the app, which matters because certificate logs are public. The app answers on a subdomain of the host the panel is on:

- **The panel on localhost**, directly or through one SSH tunnel to the panel: `http://<label>.localhost:7600`. Chrome, Edge and Firefox resolve any name under `localhost` to loopback and treat it as a secure context, so this takes no DNS and no certificate. Whether Safari does is checked before the first slice ships; where a browser doesn't, the tab says it can't find the name, and the panel's domain is the way. One tunnel to the panel then reaches every app, instead of one tunnel each.
- **The panel on its domain**: `https://<label>.panel.example.com`. This needs one DNS record, `*.panel.example.com`, pointing where `panel.example.com` does. The panel checks it as the person adds it (the DNS record check from #426) and, until it resolves, the Open button says what's missing: "Needs `*.panel.example.com` in DNS · Check again". Each label's certificate comes on its first open, from the ACME directory that serves the panel's own, through whatever serves the panel's domain: Dockwell's own HTTPS listener lets its ACME manager ask for a label it gave out, and the server's Caddy, when it holds the ports (ADR 0024), asks the control plane on loopback before it gets one (on-demand TLS). A panel started with a certificate of its own needs one that covers `*.panel.example.com`, or Open says it can't.

The panel answers on nothing else (`auth.Sites`), so there is no third case.

### Signed in as the panel is

- **Open** calls a new method that names the service and the port it publishes. Who may call it is who may reveal a secret: a developer of the project, an admin in a protected environment, in a session, never with an API token (ADR 0048, 0049). An admin tool or an unclaimed first account is worth as much as a secret.
- It answers with a one-time address on the app's origin: a ticket that works once, within a minute, for that service, tied to the session that asked. The browser opens it in a new tab, and the app's origin trades the ticket for a cookie of its own: HttpOnly, SameSite=Lax, on that label alone, `__Host-` over HTTPS. That cookie lasts as long as the panel session that made it: signing out, revoking the session, or the person losing the role ends it, checked every 30 s as streams are (ADR 0037).
- `service.open` goes on the audit log once per Open, with who, which service and from where. Requests through it aren't logged one by one.

### What it reaches, and how

- Only the port the service publishes, on a server where it runs. The control plane names the service and the container port; the agent finds the service's container, reads where Docker publishes that port (`127.0.0.1:8083`, or the address it's bound to), and dials that. It dials nothing a call names by address.
- The bytes go as a terminal's do (ADR 0037): opening is a call on the agent's main stream, and each connection the browser makes to the app is a stream the agent opens on a connection of its own, so an app streaming a large page shares nothing with plans and applies. The control plane runs Go's reverse proxy over it, so WebSockets and streamed responses work.
- The app sees the request as a reverse proxy in front of it would send it: `Host` is the label's, with `X-Forwarded-Proto`, `X-Forwarded-Host` and `X-Forwarded-For` set as Dockwell's ingress sets them.

### Kept apart from the panel

- The panel's API already refuses any other origin, by `Origin` and `Sec-Fetch-Site` (`authapi.Guard`), and an app's origin is another one. Its session cookie is HttpOnly, and over HTTPS it's `__Host-`, which no subdomain can set or shadow.
- The proxy takes the panel's cookies off every request it passes to the app, drops any cookie the app sets with a `Domain` attribute, so each app's cookies stay on its label and none reaches another app or the panel, and drops any cookie the app names like the panel's.
- On localhost, where a cookie can't take the `__Host-` prefix and a script on a label could set one for `localhost`, the panel refuses a request that carries its session cookie twice.
- Each app has its own label, so one app's scripts can't read another's pages or cookies.

## What it can't do

- **Share an app.** Only people signed in to the panel, with the role, open it. To give an app to others, add a domain, which stays the next step on its page.
- **Fix an app that keeps the address it was first opened at.** WordPress's site address, Ghost's `url` and Nextcloud's trusted domains keep the label's until a domain sets them; the entry fields that let the domain step set them are separate work under #426.
- **Carry anything but HTTP and WebSockets.** Gitea's SSH, an S3 client, or a database's own port need their port published, or a domain where the protocol allows it.
- **Serve a site.** Each request crosses the agent's link to the control plane, which is fine for an admin UI, not for traffic.
- **Reach a service that publishes no port.** Dialing a container's address on its network is possible later; every catalog entry publishes its UI, so this starts there.
- **Work without the wildcard record** when the panel is on its domain, or with a proxy in front that holds no certificate for `*.panel.example.com`. Cloudflare's free certificate covers one level of subdomain, so `*.panel.example.com` behind its proxy needs the record set to DNS only.

## Consequences

- The catalog's notes lose the SSH tunnel: an entry's web service gets an **Open** button on its page, on the catalog install's page and, for a database's admin tool, on the database's page.
- The PostgreSQL and MySQL entries publish Adminer and phpMyAdmin on every address today, over plain HTTP. Once Open works, a release of each moves them to `127.0.0.1`, as MariaDB's is.
- A new agent stream and a new public method, with their roles and an audit action. The agent build gains no new dependency: the agent dials and copies bytes, and the proxy is in the control plane.
- Each app opened on a panel with a domain costs one certificate from its ACME directory, once, then renewals. Let's Encrypt allows 50 a week for a registered domain.
- It lands in two slices: localhost origins with the stream, the method and the button first; the panel's domain, with its wildcard record check and certificates, second.
