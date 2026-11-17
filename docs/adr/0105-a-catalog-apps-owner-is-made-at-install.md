# 0105. A catalog app's owner is made at install

- Status: Proposed. Amends [ADR 0029](0029-a-catalog-format-fed-from-other-catalogs.md), whose `first_visitor_owns` and `signups` the panel only said and offered; [ADR 0084](0084-what-an-app-needs-met-by-a-new-service-one-you-run-or-a-connection.md), whose hold covered an app that starts on an empty database alone; and [ADR 0101](0101-a-port-a-catalog-app-publishes-to-everyone-on-purpose.md), whose loopback ports this hold keeps.
- Date: 2026-11-17
- Issue: #790

## Context

A new catalog app belongs to whoever opens it first. In Dockwell's own catalog, 13 of 33 entries say so with `first_visitor_owns`: Coder, Ghost, Gitea, Home Assistant, Jellyfin, Memos, n8n, new-api, OpenList, Plausible, Uptime Kuma, Vaultwarden and WordPress.

- **The panel only warns.** The install sheet says "Its first account is its owner". The app's page says "Owner: Its first account", and before a domain, "make yours before it has a domain".
- **Sign-ups stay open.** Gitea and Vaultwarden say which variable closes sign-ups (`signups`), but the install leaves them open. The page says "Open to anyone who reaches it", with Close sign-ups as an action.
- **A domain opens the race.** Once a route sends a domain to the app, anyone who reaches it first takes it. WordPress's install wizard waits at its domain until someone finishes it.
- **Converted apps say nothing.** A store-converted app (Runtipi, CasaOS, Umbrel, Coolify, Dokploy, 1Panel) has no `sign_in` and no `first_visitor_owns` unless an overlay says so. So Dockwell can't tell whether a stranger owns it first.

What exists already:
- An entry says how a person signs in (`sign_in`), with a password Dockwell generates and seals (`Input.generate`). An app that reads it on first start alone is marked `set_once`.
- ADR 0084's hold: `PortMapping.held` keeps a port on loopback, and `Route.held` answers that the app isn't open. `OpenCatalogInstall` lifts both. Only SetCatalogNeed's start-empty sets it, and a route added later to a held app isn't held.
- An account has an email, so the install can start from the installer's.

## Decision

### An entry says how its owner is made

`Entry.owner` names how Dockwell makes the owner's account as the app installs:
- **`service`:** the service, by its Compose name.
- **`email_input`:** an email input, which the install starts from the installer's email.
- **`user_input`:** a user input, for an app that asks for a name as well. Left out, the user is the email.
- **`password_input`:** a secret input with `generate`. It's `set_once` where the app keeps the password itself, with `sign_in.changed_in` saying where to change it.
- **`steps`:** what makes the account, in order. Each is one of:
  - **a command** in the app's image, as a one-off container with the service's environment, mounts and networks, as a task's `TaskApp` runs one. Its words name inputs as `${NAME}`, each a whole word, and it may name a user to run as.
  - **a request** to the service's container port, from the agent: a method, a path, a content type, a body that names inputs as `${NAME}`, headers, and the status it expects. Its `Host` is the app's address (ADR 0103), as an app that writes its own address from the first request needs.
- **`done`:** optional. A request whose answer says the app is set up, such as WordPress's install page saying it's installed. It lets Dockwell tell an install's state without asking anyone.

An entry with `owner` has its `sign_in` worked out from it: the user, or the email, and the password input.

Each entry's steps are proven on its real image before it ships, in the catalog's checks. These are what we know of each app; a slice that finds otherwise moves the app to the hold.

| How | Apps |
|---|---|
| A command | Gitea: `gitea admin user create --admin` as `git`, its entry already sets `INSTALL_LOCK`. OpenList: `openlist admin set`. |
| A request | Ghost: `/ghost/api/admin/authentication/setup/`. n8n: `/rest/owner/setup`. Jellyfin: `/Startup/User`, then `/Startup/Complete`. Home Assistant: `/api/onboarding/users`. Coder: `/api/v2/users/first`. new-api: `/api/setup`. WordPress: `/wp-admin/install.php?step=2`, as a form. Memos: its sign-up, whose first account is its host. |
| An invite | Vaultwarden. Its account key comes from the person's master password, in their browser, so Dockwell never makes the account. It keeps sign-ups closed, generates `ADMIN_TOKEN`, and invites the owner's email through the admin API. The person sets their own master password. |
| None | Uptime Kuma, whose setup is a socket.io event, and Plausible, whose form carries a CSRF token. These install held. |

### The owner is a resource

- **A new resource kind, `AppOwner`.** It holds the service, the email, the user, the password's secret, and the steps, as the install wrote them. `DatabaseUser` is the model: something made once, kept as a resource, with a plan.
- **The plan says it:** "Make the owner account in Gitea as you@example.com, once gitea is healthy". It depends on its service, so its step runs after the service is healthy.
- **The agent makes it.** It runs the steps with the inputs filled in, from the sealed secret, and records that it's made. A step that fails fails the change, saying the app's own answer, and the change's rollback leaves the app held (below), never open.
- **Once made, it's never made again.** A later change to `AppOwner` makes nothing in the app. Setting the secret anew changes the app's password only where the app reads it again, which `set_once` says. Deleting `AppOwner` leaves the account in the app, and the plan says so.

### Sign-ups close at install

- An entry with `owner` and `signups` writes `signups.variable` as `signups.closed` at install: Gitea's `GITEA__service__DISABLE_REGISTRATION=true`, Vaultwarden's `SIGNUPS_ALLOWED=false`.
- The install sheet shows "Sign-ups: Closed", with a switch to open them, which writes the other value. The app's page opens or closes them as a plan, as it closes them today.
- An entry without `owner` leaves sign-ups as they are until its owner exists, as the first account needs them. The hold covers that time.

### An app no one owns yet is held

An install is held:
- when its entry has `first_visitor_owns` and no `owner`;
- when its entry has `owner`, until `AppOwner` is made;
- when it's converted from another store, has web, and has no `sign_in`, no `owner` and no overlay: Dockwell can't tell who owns it first, so it holds it.

While an install is held:
- **Its ports** stay on loopback (`PortMapping.held`), as ADR 0101 has them already, unless a person opens them.
- **Its routes** are held (`Route.held`): they answer that the app isn't open yet. That holds a route added after the install too: the changes handler's hook marks a route edit held when the service it sends to is a held install's, as ADR 0103's address hook works.
- **The pin records it:** `CatalogPin.held`. It's set at install, and cleared by `OpenCatalogInstall`, or by the change that makes `AppOwner`.
- **`OpenCatalogInstall`** lifts it, as a plan that says what opens: "Open WordPress to everyone: blog.example.com answers it".

### What a person sees

- **The install sheet:**
  - "Owner: you@example.com · Change", prefilled and required where the entry has `owner`.
  - "Sign-ups: Closed", with a switch.
  - For a held entry, one line: "Held until you set it up. You open it to everyone after."
- **The plan:**
  - "Make the owner account in Gitea as you@example.com";
  - "Sign-ups closed";
  - or "Held: its domains answer that it isn't open yet".
- **Once applied:** "Gitea's owner is you@example.com", with Copy password and Open Gitea. The password shows here once. After that, the Sign in card reveals it behind a passkey, as it does today.
- **The app's page:**
  - "Owner: you@example.com" in place of "Its first account", and the Sign in card.
  - A held app: "Held: only you reach it until its owner account exists", with "Open <app>" (the panel's Open on localhost, or the SSH tunnel command with Copy, ADR 0061) and "Open it to everyone".
  - Where `done` reads that it's set up: "Set up · Open it to everyone", with nothing to ask.
- **An install from before this:** it has no `AppOwner` and no hold, so nothing changes by itself. Where `done` reads that the app isn't set up and a route sends to it, the page says "Anyone who reaches git.example.com can take it", with "Hold it", a plan that holds its routes, and "Make my account", a plan that adds `AppOwner`.

### Store-converted apps

- A converter can't know an app's owner model. An overlay, which already carries `sign_in` and `first_visitor_owns`, carries `owner` too, for the store apps people use most.
- A store app whose form asks for its admin password keeps that as an input, as today.
- Every other converted app with web and no way to sign in installs held, as above. It costs one click to open, and no stranger takes it first.

### Numbers

- `Entry.owner` = 33, with the new messages `Owner`, `OwnerStep`, `OwnerCommand`, `OwnerRequest` and `OwnerProbe`.
- `Spec.app_owner` = 160, with the new message `AppOwner`.
- `CatalogPin.held` = 86.
- Agent protocol 380 to 389: reading an install's `done`, and what else the agent needs to make an owner.

## What we turned down

- **Asking the person to make the account first, as today.** It leaves every app open to the first visitor once it has a domain, and it's a sentence telling a person to do something by hand.
- **Making every account through the app's web UI, as a browser would.** CSRF tokens, sockets and wizards differ by app and version. A command or a request each entry proves is smaller, and an app with neither is held instead.
- **Holding every catalog app until opened.** An app whose owner Dockwell makes, or whose entry signs in with a generated password, has no first visitor to fear, and a hold would cost a click for nothing.
- **Closing sign-ups after the owner signs up.** It leaves the window open, and it needs a person to come back.

## Consequences

- No catalog app is open to a stranger once it has a domain: its owner is made at install, or it's held until its owner sets it up.
- Gitea and Vaultwarden install with sign-ups closed.
- A domain added to a held app answers that it isn't open, until a person opens it.
- A converted store app with no known sign-in takes one more click to open to everyone.
- An entry's owner steps are a contract with the app's version: an upgrade's release runs nothing again, as the account exists, but a release that changes how its app makes an owner needs its steps proven again in the catalog's checks.

## Order of work

1. This record, the proto, `AppOwner` and its agent driver, and the steps for Gitea, Ghost and n8n, proven on real images in CI's Docker job. Sign-ups close at install.
2. The hold: `CatalogPin.held`, routes added later held, `done`, and the page's states, for WordPress, Uptime Kuma, Plausible and converted apps.
3. The rest of the 13, Vaultwarden's invite, and installs from before.
4. Overlays with `owner` for the store apps used most.
