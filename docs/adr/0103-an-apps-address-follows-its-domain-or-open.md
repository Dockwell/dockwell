# 0103. An app's address follows its domain or Open

- Status: Accepted. Amends [ADR 0029](0029-a-catalog-format-fed-from-other-catalogs.md), whose `address_variables` followed a domain alone and only through the panel, and [ADR 0061](0061-opening-an-apps-web-ui-through-the-panel.md), whose Open address an app's own links never named.
- Date: 2026-11-17
- Issue: #779

## Context

An app tells a browser its own address in settings such as a base URL, the origins and hosts it accepts, a redirect, or the links it shares. Dockwell's converters wrote each store's address, such as Runtipi's `${APP_DOMAIN}` or Coolify's `SERVICE_URL_`, as `http://localhost` and the app's port, which nothing changed later.

So paperless-ngx, TubeArchivist, Joplin, Send, nextcloud and owncloud worked only through an SSH tunnel to their port:
- behind a domain or the panel's Open, an app that checks its host refused the request;
- one that builds its links sent the browser to the person's own machine;
- one that shares links handed out an address no one else could open.

Dockwell's own entries had `address_variables`, which the panel set on the first domain alone, as part of the plan that routed it. A converted entry had none.

## Decision

### An entry says which variables follow the address

- `Entry.address_variables` names each one, as before. Its value says the address with:
  - **`{origin}`**, as a browser opens it, such as `https://docs.example.com`;
  - **`{host}`**, the same without its scheme;
  - **`{scheme}`**, `https` at a domain and `http` otherwise.
- A value with these is written at install, and again whenever the address moves.
- **`{domain}` keeps its meaning.** A value with it, such as Ghost's `https://{domain}`, or a value that goes with a domain alone, such as Nextcloud's `OVERWRITEPROTOCOL: https`, is written once the app has a domain, and stays when it loses one, as before.
- **A variable the service doesn't have yet is added,** as the panel added Gitea's `GITEA__server__ROOT_URL`, n8n's `WEBHOOK_URL`, Vaultwarden's `DOMAIN` and Nextcloud's `OVERWRITEPROTOCOL` on the first domain before, and as an upgrade's merge adds a key new to both.
- **The converters mark what they write on loopback.** In each store's converted file, a value that names the app at web's port, `http://localhost:${PORT}` or `localhost:${PORT}`, becomes `{origin}` or `{host}`. That covers Runtipi's `${APP_DOMAIN}` and the server's address at the app's port, Coolify's `SERVICE_URL_` and `SERVICE_FQDN_` of the web route, and Dokploy's domain helpers. A value that also names another variable stays as the file writes it. The file keeps its loopback value, and no count moves (`make store-rates`).
- **An identity an app takes once is never marked:** a Matrix server's name (Conduit's, conduwuit's, tuwunel's and Synapse's `*_SERVER_NAME`), and a federated server's own domain (Mastodon's `LOCAL_DOMAIN`, GoToSocial's `GTS_HOST`, Bonfire's `HOSTNAME` by its image), and an identity provider's (Zitadel's `ZITADEL_EXTERNALDOMAIN`). Each is in every account made under it, so a domain that moved it would leave the app refusing its own database. The list is reviewed by hand: a variable joins it when its app's docs say it's set once.
- **A service on the server's own network, or another container's, is never marked:** what it names at the app's port is that service reaching the app from the server's side, as Beszel's agent's `HUB_URL` does, so it stays loopback.
- **A value that puts its own scheme or port around the address is never marked,** such as `wss://localhost:${PORT}/cable` or `ssh://localhost:${PORT}:23231`: no address of the app's renders into one that works there.
- **A store's converter adds what it knows its app needs.** Runtipi's value that is its protocol alone, `${APP_PROTOCOL}`, takes `{scheme}`, and Nextcloud gets `OVERWRITEHOST: {host}` beside it, from a reviewed list by app.
- **A host its app reads with its scheme takes the origin,** as TubeArchivist's `TA_HOST` does, which puts `http://` before a host alone and checks a sign-in's origin against it. A reviewed list too.

### The app's address

In order, the address is:
1. **Its domain:** the oldest domain routed to web's whole path. A redirect, a path, or a route to another service isn't the app's address. A second domain moves nothing until the first goes.
2. **Otherwise, its Open address:** `http://<label>.localhost:<panel port>` (ADR 0061), which one SSH tunnel to the panel reaches for every app. That's only where the panel opens apps: served on localhost alone, with no domain of its own and no proxy or tunnel in front. Served at a domain, the panel offers a tunnel to the app's own port instead, so the next address is the one used. The port is the one the panel listens on, so a tunnel that maps the panel to another local port opens the app on that port, while its links name the panel's own.
3. **Otherwise, web's port on loopback:** `http://localhost:<port>`, which a tunnel to that port reaches.

The control plane works it out, never the panel, so nothing is UI-only.

### When it's written

- **At install.** The install picks web's ID up front, as an edit may name the ID a new resource takes, since Open's label comes from that ID. It writes each variable from the address in the install's own plan. The install's pin records the address written (`CatalogPin.address`).
- **When a route moves the address.** A change that routes web's first domain, takes its last one away, or otherwise changes which domain is the app's address writes each variable at the new address, in the same change. That includes the panel's Add a domain, the CLI, and any client of `ProposeChange`. The plan says so: "URL follows the app's address to https://docs.example.com, which restarts web". Undoing the change takes both back. The change leaves the project alone, as changing one is an admin's while a domain is a developer's to add, so the pin keeps the address it recorded until the next upgrade.
- **A value asked for as the entry writes it,** `{origin}` and all, as the app's Set to sends, or an older panel did, is rendered at the address the change leaves, on any change, with or without a route; one that address can't give, such as `https://{domain}/` with no domain, is refused, saying so. It's never written as it stands.
- **A move beside a restart or a restore of a service:** a restart becomes the edit of its spec, which restarts it anyway; a restore beside a move of the address is refused, saying to plan them apart.
- **On upgrade.** The release's variables are written at the address the app has now. A variable the release no longer names goes back to what its file writes.
- **On a change of companion or need,** the pin keeps its address and the variables keep their values.

### A value a person set stays

- **What counts as Dockwell's value:** what the address before the change gives the variable, which Dockwell wrote as that address came, what the recorded address gives it, and what any domain a route in the environment ever sent to web's whole path gives it, from every revision of its routes, deleted ones' too. So a domain that comes after one that went moves Gitea's `ROOT_URL` on, though no address in between gave it a value. Only an install with no recorded address counts what its file gives the variable too, rendered with the install's own values, so a person who sets the loopback value by hand keeps it. A value that still names `{origin}`, `{host}` or `{scheme}`, as an older panel's Add a domain sent, is Dockwell's to render. A variable that holds anything else holds a value a person set.
- **What can't be told apart:** a person who sets the variable to exactly what Dockwell would write, such as the Open address after a domain, holds Dockwell's value as far as anyone can tell, and the next move rewrites it. It stays through a route change and an upgrade, and each plan says so: "URL holds a value you set, so it stays rather than follow the app's address".
- **An install from before this:** it has no recorded address, so only the file's value is Dockwell's: the converter's loopback default, or the value Dockwell's own entry writes. Where the current reader refuses its recorded file, a change that moves the address is refused only for a variable whose owner the file alone would tell, saying so and to upgrade the app first, which records its address. Any other route change plans as ever.

### What isn't covered yet

- **The gap:** only a change proposed through the control plane's changes handler moves the address. Every API and panel path does: `ProposeChange`, Compose, the catalog. A change made by the engine alone doesn't:
  - an undo or rollback of a route change made before Dockwell kept the address (one made since takes its address edits back with it);
  - a rollback of a service to a revision from before its address moved (`RollBackService`), which puts back the old value while the domain stays, as the plan's diff shows;
  - a move of an app to another project;
  - a restore of the control plane from a backup;
  - a panel that gains a domain after an app was installed: the app keeps its Open address until a route change or an upgrade writes the loopback one.

  After one of these, the variables keep what they had until the next route change or upgrade writes them.
- **A release that changes a variable's template:** its pin keeps the address alone, and the variables come from the release in the catalog now. So a value Dockwell wrote from an older release's template reads as a person's, and stays.
- **The later path:** if the gap matters, the engine renders a service's variable from a template with an address part at every plan, from the service's routes, as ADR 0084 writes a need's URL. That covers every path, including apps that don't come from the catalog. It's a larger change to the engine, and it needs a part of `ValueTemplate` of its own.

### Numbers

`CatalogPin.address` = 84, `CatalogInstall.address` = 11 and `CatalogInstall.address_kept` = 12. `AddressVariable.value` takes `{origin}`, `{host}` and `{scheme}` beside `{domain}`, with no new field.

## What we turned down

- **The panel writing the variables, as it did:** a route from the CLI or the API left them behind, and the panel had to read an entry's variables to write a plan.
- **A domain RPC of its own:** a domain is a route, and every way to make one ends in `ProposeChange`, so the follow-up goes there.
- **The engine at every plan, now:** see the later path above.
- **Writing the Open address into the file:** the label comes from web's ID, which a file doesn't have, and the panel's port is the control plane's to know.

## Consequences

- Runtipi's paperless-ngx, Joplin and Send work behind their domain, and before one, through Open. TubeArchivist does too once its volume can take an owner (#778).
- Runtipi's Nextcloud follows its address through `OVERWRITEHOST` and `OVERWRITEPROTOCOL`, which its image reads at every start, and which Nextcloud trusts whatever its `trusted_domains` holds. `NEXTCLOUD_TRUSTED_DOMAINS`, which the image reads on the first install alone, isn't enough. That's from Nextcloud's own source; it hasn't been run behind a domain here.
- Runtipi's ownCloud is marked (`OWNCLOUD_DOMAIN` and `OWNCLOUD_TRUSTED_DOMAINS`), but whether its image reads them again at each start, and how it learns its scheme behind a proxy, isn't checked yet.
- A plan that routes a domain to a catalog app restarts the services whose variables follow it, and says so.
- The panel's own address edits are gone. The control plane says where each install is reached and which variables don't hold what that gives them (`CatalogInstall.address` and `address_kept`), so the app's page says "Follow docs.example.com" only where they do, and "Not at docs.example.com" otherwise, with Set to as its action, which asks for each value as the entry writes it.
