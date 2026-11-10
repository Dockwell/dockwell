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

Every service someone opens gets a **label**: ten characters of base32 from an HMAC of the service's ID under a key the control plane keeps (`opener.key` in its data directory). It stays the same for that service, can't be guessed, and says nothing of the app, which matters wherever certificate logs would list it.

- **Slice 1, the panel on localhost**, directly or through one SSH tunnel to the panel: `http://<label>.localhost:7600`. Chromium resolves any name under `localhost` to loopback and treats it as a secure context, which was checked on the stand. Firefox and Safari aren't checked yet (WebKit couldn't run where this was built), and the button shows in every browser. Where a browser doesn't resolve the name, the tab says it can't find it. One tunnel to the panel then reaches every app, instead of one tunnel each.
- **Slice 2, the panel on its domain**, needs an origin on another site, as below. Until it lands, a panel reached at its domain doesn't open apps: `OpenService` refuses with `open_on_localhost`, and the app's page keeps the tunnel command one copy away.

The panel answers on nothing else (`auth.Sites`), so there is no third case.

### Signed in as the panel is

- **Open** calls `ServiceService.OpenService`, which names the service and the port it publishes. Who may call it is who may reveal a secret: a developer of the project, an admin in a protected environment, in a session, never with an API token (ADR 0048, 0049). A service named by name is found in the caller's projects alone, as every call that takes a name is (`idNames`, with a test that fails on a call that takes a project beside its id and isn't listed).
- **It takes a passkey, one touch, as revealing a secret does** (`STEP_UP_ACTION_OPEN_SERVICE`, its target the service's ID). Many admin tools sign in whoever reaches them by themselves, and an unclaimed app makes its first visitor the owner, so opening one hands over as much as a secret does. The call refuses for any other reason before it asks, so nobody confirms what can't happen, and a proof opens the app once.
- It answers with a one-time address on the app's origin: a ticket that works once, within a minute, for that service's label. **The ticket is a bearer address for that minute.** On localhost the browser that redeems it isn't checked, and can't be, as the panel's cookie there is host-only on `localhost` and the label's origin never sees it. What makes it safe is that it's single-use, short-lived, made only after a passkey, and handed only to the panel session that asked.
- The label's origin trades the ticket for a cookie of its own, `dockwell_open`: HttpOnly, SameSite=Lax, host-only on the label. **That cookie is bound to the panel session that asked**, not to a browser: the open session follows it as streams do (ADR 0037). Signing out or revoking the session ends it at once, and every connection it carries closes, WebSockets and other upgraded connections too. The person losing the role, or the environment becoming protected where they're a developer, ends it within a recheck, 30 s, with the same rule the interceptor applies. A read of the role that fails is ridden out three times in a row, then ends it. An open session lasts 12 hours at most.
- `service.open` goes on the audit log once per Open, with who, which service, the port, the label's origin and the address the panel was asked from. Requests through it aren't logged one by one.

### What it reaches, and how

- Only the port the service publishes, on a server where it runs. The control plane names the service and the container port; the agent finds the service's running container by its Compose labels, which a spec can't set, reads where Docker publishes that port over TCP (`127.0.0.1:8083`, loopback for one published on every address), and dials that. It dials nothing a call names by address.
- Opening is a call on the agent's main stream. Each connection the browser makes to the app is a `Forward` stream the agent opens to the control plane, four to a connection of their own, apart from plans and applies. Each takes in at most 64 KiB of what the browser sends before the agent writes it to the app, and its connection 256 KiB, so a forward to an app that reads slowly holds up no other (`enroll.ForwardClient`). An agent carries 16 forwards at once at most, and refuses the next with why: "this server carries 16 connections to apps opened through the panel already". An open session holds 6 connections to its app at most, a browser's own number for a host, and a request past them waits. `make forwards-load` holds an agent to its 40 MB budget with all 16 busy: 36.3 MB at most, from 26.7 MB settled with four apps open. At 20 it was 39.1 MB, too near the budget as the agent grows. The agent listener takes HTTP/2 frames of 16 KiB, as a Go client writes each stream's body through a buffer of the peer's frame size, which with Go's own 1 MiB was 512 KiB for each.
- The control plane runs Go's reverse proxy over each forward, so WebSockets and streamed responses work. The app sees the request as a reverse proxy in front of it would send it: `Host` is the label's, with `X-Forwarded-Proto`, `X-Forwarded-Host` and `X-Forwarded-For` replaced as Dockwell's ingress sets them.

### Kept apart from the panel

- The panel's API already refuses any other origin, by `Origin` and `Sec-Fetch-Site` (`authapi.Guard`), and a label's requests to the panel arrive as `cross-site` with no cookie, as the security review's probe showed. Its session cookie is HttpOnly, and over HTTPS it's `__Host-`.
- The proxy takes Dockwell's cookies (`dockwell_session`, `__Host-dockwell_session`, `dockwell_open`) off every request it passes to the app and leaves the rest as the browser sent them, byte for byte. Of what the app sets, it drops a `Set-Cookie` whose `Domain` isn't the label's own host, and any named as Dockwell's, and passes the rest byte for byte. That covers headers alone: a cookie an app's script sets with `document.cookie` doesn't pass the proxy. On localhost the browser's own rule keeps labels apart: Chromium refuses `Domain=localhost` and `Domain=.localhost` from a label, by header and by script, as the review's probe showed. On a domain, nothing would, which is why slice 2 needs another site.
- **What a panel on localhost can't be kept from.** Cookies ignore ports, so a page on another port of localhost, such as an app reached through a tunnel of its own or a development server, can set a `dockwell_session` cookie that reaches the panel. With a longer path, the panel gets two, and refuses the request (`two_sessions`) rather than choose one, which says to clear the site's cookies. At the panel's own path, `/`, it overwrites the panel's cookie, which signs the person out, or into a session of that page's choosing, and nothing on plain HTTP can stop it. That's a limit of every panel reached on localhost, with or without Open, and the duplicate refusal covers only the first case.
- Each app has its own label, so one app's scripts can't read another's pages or cookies.

### Slice 2: an origin on another site

`<label>.panel.example.com` was the first plan, and it doesn't hold:

- **It's the same site as the panel.** A site is a registrable domain, so `<label>.panel.example.com` is the same site as `panel.example.com` and as every host under `example.com`. SameSite separates nothing: an app's pages make requests to the panel, and to the owner's other sites under `example.com`, with their Lax and Strict cookies. The panel holds through `__Host-` and `checkOrigin`, which refuses `same-site`, but an app's script can set `Domain=panel.example.com` cookies in every other app's origin and `Domain=example.com` cookies on the owner's other sites, and many owners put the panel under their main site's domain.
- **ADR 0024 forbids anything under the panel's domain**, because a browser offers the panel's passkeys there. A page on a label may ask for an assertion for the RP ID `panel.example.com`. The assertion is useless against the panel, which pins its exact origin (`RPOrigins`), but a hostile app can still raise a passkey prompt that looks like the panel's.

So slice 2 serves apps on a site that is neither the panel's nor any of the owner's other sites.

#### Proposed: an apps domain the owner gives

Awaiting the owner's decision; nothing is built for it yet.

- **What the owner gives.** A registrable domain used for nothing else, such as `apps-example.net`, and one DNS record, `*.apps-example.net`, pointing where the panel's domain does. They set it once, in Settings, as the panel's apps domain, and the panel checks the record as they type, with the DNS record check of #429: "`*.apps-example.net` doesn't point here yet · Check again". Until it does, a panel at its domain keeps the tunnel command, as now.
- **The origin.** `https://<label>.apps-example.net`, with the label slice 1 gives each service. It's another site from the panel and from every site the owner has under other domains, so SameSite keeps the panel's cookies off an app's requests, and a browser never offers the panel's passkeys there, as its RP ID isn't a suffix of the app's host.
- **What serves it.** Whatever serves the panel's domain: Dockwell's own HTTPS listener, or the server's Caddy when it holds the ports (ADR 0024), which passes `*.apps-example.net` to the control plane on a socket of its own, as it does the panel's domain, marked HTTPS by which listener took it, never by a header. The opener answers there for labels it gave out, and for nothing else on that domain.
- **Certificates.** One for each label, on its first open, from the ACME directory that serves the panel's own: Dockwell's ACME manager takes a label it gave out; Caddy asks the control plane on its socket before it gets one (on-demand TLS). Let's Encrypt allows 50 a week for a registered domain. A wildcard certificate would need the owner's DNS provider's API, which this doesn't ask for.
- **Signing in** is slice 1's: a passkey, then a single-use ticket for a minute, then a cookie of the label's own, `__Host-dockwell_open`, Secure, HttpOnly and SameSite=Lax, bound to the panel session that asked, which ends it at once on sign-out. The ticket now crosses the internet in an address, over HTTPS, and the browser that redeems it still isn't checked, as on localhost.
- **Apps share a site with each other.** Every label is under `apps-example.net`, so one app's script can set a cookie for `apps-example.net` that another app's pages then send to it. The proxy keeps app headers to their own host, but not scripts. On localhost the browser keeps labels apart; here it doesn't, so this is a limit to state: an app opened through the panel can plant cookies in the others. A domain for each app would end it, at a cost no owner pays.
- **Behind Cloudflare's proxy**, its free certificate covers `*.apps-example.net`, one level, so the record can stay proxied with Cloudflare's certificate in front and Dockwell's behind it, or be DNS only.

#### Alternatives

- **The server's IP address, a port each**: `https://203.0.113.7:7701`, with a short-lived IP certificate and a listener for each app. It's another site from the panel, but cookies ignore ports, so every app on the address shares cookies with every other, and each needs a port open, which a provider's firewall often blocks.
- **A name under a shared suffix**: `https://<label>.<install>.dockwell-apps.example`, with the suffix on the Public Suffix List so each install is its own site. It asks nothing of the owner, but the project would run DNS and certificates for every install, a service outside the owner's servers that every install then depends on, and a listing takes months.

## What it can't do

- **Share an app.** Only people signed in to the panel, with the role and a passkey, open it. To give an app to others, add a domain, which stays the next step on its page.
- **Fix an app that keeps the address it was first opened at.** WordPress's site address, Ghost's `url` and Nextcloud's trusted domains keep the label's until a domain sets them; the entry fields that let the domain step set them are separate work under #426.
- **Carry anything but HTTP and WebSockets.** Gitea's SSH, an S3 client, or a database's own port need their port published, or a domain where the protocol allows it.
- **Serve a site.** Each request crosses the agent's link to the control plane, which is fine for an admin UI, not for traffic, and an agent carries 16 connections at once.
- **Reach a service that publishes no port.** Dialing a container's address on its network is possible later; every catalog entry publishes its UI, so this starts there.
- **Outlive a restart.** Open sessions are in the control plane's memory, so a restart ends every open app, which then says "This app's session has ended. Open the app from the panel again."
- **Help a panel reached at its domain**, until slice 2.

## Consequences

- The catalog's notes lose the SSH tunnel: an entry's web service gets an **Open** button on its page, on the catalog install's page and, for a database's admin tool, on the database's page. On localhost, any port a service publishes over TCP opens, with a domain or without.
- The PostgreSQL and MySQL entries publish Adminer and phpMyAdmin on every address today, over plain HTTP. Once Open works, a release of each moves them to `127.0.0.1`, as MariaDB's is.
- A new agent stream and a new public method, with their roles, a step-up action and an audit action. The agent build gains no new dependency: the agent dials and copies bytes, and the proxy is in the control plane.
- It lands in two slices: localhost origins with the stream, the method and the button first; an apps domain the owner gives, as proposed, once they've decided, second. Slice 1 serves a person who reaches the panel on localhost, at the control server or through a tunnel to it; a panel used at its domain day to day gets Open from slice 2.
