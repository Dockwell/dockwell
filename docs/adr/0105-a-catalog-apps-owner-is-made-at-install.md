# 0105. A catalog app's owner is made at install

- Status: Proposed (revised after review).
- Amends:
  - [ADR 0029](0029-a-catalog-format-fed-from-other-catalogs.md), whose `first_visitor_owns` and `signups` the panel only said and offered;
  - [ADR 0054](0054-the-agent-keeps-its-desired-state.md), as an owner's password never enters the agent's desired state or its cache;
  - [ADR 0084](0084-what-an-app-needs-met-by-a-new-service-one-you-run-or-a-connection.md), whose hold covered an app that starts on an empty database alone;
  - [ADR 0101](0101-a-port-a-catalog-app-publishes-to-everyone-on-purpose.md), whose loopback ports this hold keeps, and whose approval model opening an unconfirmed app takes.
- Depends on:
  - [ADR 0061](0061-opening-an-apps-web-ui-through-the-panel.md), one of the ways into a held app;
  - [ADR 0069](0069-a-password-or-only-some-addresses-in-front-of-a-route.md), whose route rules the setup link sits beside;
  - ADR 0103 (PR #789), whose address hook this hook follows, and whose address variables WordPress's address takes.
- Date: 2026-11-17
- Issue: #790

## Context

A new catalog app can belong to whoever reaches it first. In Dockwell's own catalog (33 entries):
- **13 make their first visitor the owner** (`first_visitor_owns`): Coder, Ghost, Gitea, Home Assistant, Jellyfin, Memos, n8n, new-api, OpenList, Plausible, Uptime Kuma, Vaultwarden and WordPress.
- **One starts with a published password:** Umami, as admin with the password umami. Its `first_steps` tells the person to change it by hand.
- **Sign-ups stay open where a setting closes them:** Gitea and Vaultwarden by a variable. Memos and new-api by their own settings: Memos' `first_steps` tells the person to turn registration off by hand.
- **The rest sign in as an admin their environment sets at first start:** Gotify, Grafana, MariaDB, MySQL, Nextcloud, Nginx Proxy Manager, pgAdmin, PostgreSQL, SFTPGo and others, each with a password Dockwell generates (`sign_in`, `Input.generate`). These have no first visitor to fear.
- **Some have no accounts of their own:** Adminer signs in to a database. Cloudflared, MongoDB and Redis have no web page.

Where that leaves things today:
- **The panel only warns.** It shows "Its first account is its owner", and "Open to anyone who reaches it" with Close sign-ups as an action. Once a route sends a domain to the app, the first stranger to reach it takes it. WordPress's install wizard waits at its domain until someone finishes it.
- **Converted apps say nothing.** A store-converted app (Runtipi, CasaOS, Umbrel, Coolify, Dokploy, 1Panel) has no `sign_in` and no `first_visitor_owns` unless an overlay says so, and stores such as Umbrel publish default credentials the converters don't read.
- **ADR 0084's hold covers one case.** `PortMapping.held` keeps a port on loopback, `Route.held` answers that the app isn't open, and `OpenCatalogInstall` lifts both. Only SetCatalogNeed's start-empty sets it, and a route added later to a held app isn't held.

## Decision

### How an entry says its app is owned

`Entry.owner` says how the app's owner comes to exist, in one of four kinds, named by which fields are set. Every entry with web has one of them, or `first_visitor_owns`, and the catalog's check refuses one with neither.

**Made by its environment.** The app makes its admin from its environment at first start, as Grafana does.
- The entry's `sign_in` with a generated password input says so, and `owner` adds nothing.
- This is the default the catalog already has for most entries.

**Made by steps.** Dockwell makes the account once the app runs, and its fields say how:
- **`service`:** the service, by its Compose name.
- **`email_input`:** an email input.
- **`user_input`:** a user input, required where the app has user names. It starts from the email's local part, cut to `user_pattern`, which the entry gives.
- **`password_input`:** a secret input with `generate` and `generate_as: password`.
- **`steps`:** what makes the account, in order.
- **`done`:** a check that says it's made.
- **`reset`:** optional steps that set a new password.

**Starts with a known password.** `known_user` and `known_password` say what the app starts with.
- The `steps` sign in with them and change them to the owner's: Umami signs in as admin, then changes its user name and password.
- Its `done` reads "the known sign-in is refused".
- The catalog's check refuses an entry whose notes or `first_steps` speak of a default password and that has no `known_password`.

**No accounts.** `none` says the app has no accounts of its own, as Adminer and a tunnel have.

An entry whose app has none of these ways, as Uptime Kuma's socket.io setup and Plausible's CSRF-guarded form have, keeps `first_visitor_owns` and no `owner` steps. It installs held (below).

### Steps

A step is one of:
- **A command,** run in the service's running container as the `user` it names, as `docker exec` runs it. A step runs as a one-off container in the app's image, with the service's environment, mounts and networks, only where its entry says `one_off`.
- **A request** to the service's container port, from the agent, with:
  - a method, a path, a content type, a body, headers, and the status it expects;
  - `capture`: fields of its answer, by JSON path, that later steps read as inputs. That's how a session token or a user's ID passes from a sign-in to the change that follows it.

Rules for every step:
- **Inputs never go in argv.** They reach a command through its environment, or its standard input, and a command names them as `"$NAME"` in a `sh -c` where the app takes them only as arguments. Where an app reads the password from its environment, the entry uses that in place of a step, as OpenList's `OPENLIST_ADMIN_PASSWORD` does.
- **A request never follows a redirect.** An answer of 3xx fails the step, so a body with a password never goes to another address.
- **A request's `Host` is the app's address** (ADR 0103), and `X-Forwarded-Proto` may be set, as an app behind a proxy expects it.
- **Nothing a step prints or answers is kept,** beyond what's below. A one-off container is removed when its step ends, and its output is never kept as a task run's.
- **The app's answer is scrubbed.** Where a step fails, the agent keeps 512 bytes of the app's answer or output. Before it's stored, logged, sent as an event or put on a change, each filled input's value, in its plain, URL-encoded and JSON-escaped forms, is replaced with `[hidden]`. A test searches the store, the events, the change and the agent's log for the value.

### The owner's password

- **It's generated** with `generate_as: password`, which guarantees an upper-case letter, a lower-case letter and a digit in every value, within the input's `min_length` and `generate`. n8n, for one, refuses a password without them.
- **It's sealed** as the install's secret, as every generated input is.
- **It reaches the agent for the step alone.** The control plane sends it in the call that runs the step. It's never part of the agent's desired state, so it's never in the agent's cache or sent again in a fill (ADR 0054).
- **It's revealed with a passkey, any time,** in the session that asks, from the app's Sign in card. It's never on a change, the install's answer or an event.
- **Rotate:**
  - Where the entry has `reset`, "Make a new password" generates one, runs `reset`, and seals it once the step succeeds.
  - Where it has none, "Changed it in Gitea? · Forget it" is a plan that deletes the sealed copy, and the card says where the app changes it (`sign_in.changed_in`).

### The owner is a resource, and its being made is observed

- **`AppOwner` is a resource kind.** The install writes it, with the service, the email, the user, the kind, and the password's secret by reference, never its value. The plan says "Make the owner account in Gitea as you@example.com, once gitea is healthy". `DatabaseUser` is the model.
- **Whether it's made is a fact Dockwell observes, never a field a change writes.**
  - After the install's change applies and the service is healthy, the control plane asks the agent to run the steps (agent protocol 380).
  - The agent answers with the outcome (381).
  - The control plane records it in the store's `app_owners` table (migration 0047), with the state (waiting, made or failed), the scrubbed error, the tries, and when.
- **A failed step never rolls the install back.**
  - The change stays applied, with a note: "Dockwell couldn't make Gitea's owner".
  - The app stays installed and held.
  - Its page says "Couldn't make the owner: <the app's answer, scrubbed> · Try again". Try again calls `RetryCatalogOwner`, which runs the steps again. The fallback is "Set it up yourself", which stays held.
- **A made owner is never made again.** A later change to `AppOwner` makes nothing in the app. Deleting it leaves the account in the app, and the plan says so.

### Sign-ups close

- **Closed by a variable:** an entry whose `signups` names a variable writes the closed value at install: Gitea's `GITEA__service__DISABLE_REGISTRATION=true`, Vaultwarden's `SIGNUPS_ALLOWED=false`.
- **Closed by the app's settings:** `signups.steps`, run right after the owner's steps and in the same call, with the owner's session captured. A check, `signups.check`, reads them closed.
  - Memos sets "disallow user registration" in its instance settings.
  - new-api turns password registration off through its options.
  - The app stays held until `signups.check` reads them closed, and Memos' `first_steps` goes.
- **The install sheet's switch:** "Sign-ups: Closed", with a switch to open them. The pin records the choice (`CatalogPin.signups_open`).
- **Opening closes them too.** `OpenCatalogInstall`'s plan closes sign-ups where the entry says how and the pin doesn't say open: "Open Memos to everyone, with sign-ups closed".

### Held is a fact the control plane works out

An install is held while any of these hold:
- `CatalogPin.held` is set. An install sets it for an entry with `first_visitor_owns` and no owner steps, for a converted app (below), and for an install by token with no owner email (below). Only an applied `OpenCatalogInstall` clears it.
- Its entry has owner steps, or a known password, and `app_owners` doesn't read it made.
- Its entry closes sign-ups by steps, and `signups.check` doesn't read them closed.
- Its entry is Vaultwarden's kind, an invite (below), and `done` doesn't read the owner registered.

No change clears a hold in the same write that asks for the owner. Only an observed fact or an applied open lifts it.

While an install is held:
- **Routes.** A route that sends to one of its services is held (`Route.held`): it answers that the app isn't open yet. So is a route whose `host_port` is one of its published host ports, as that dials the same port. The changes handler's hook holds them at plan time, for routes added later too, as ADR 0103's address hook works.
- **Ports.** A `PortMapping` of its services keeps `held` whatever an edit sends, including a whole-spec edit that leaves the field out, so a public port stays on loopback.
- **Tunnels.** ADR 0104's tunnel rules point at Caddy, so a held route covers them. A taken-over tunnel rule that points at a held install's port directly is refused, saying so.
- **Tests** cover a route added after the install, a host-port route, a ports edit, a whole-spec edit and a tunnel rule.

When a hold lifts by an observed fact, the control plane releases what it held as a change of its own, made by Dockwell and applied at once, as automatic updates are: "Gitea's owner exists: blog.example.com answers it". There's nothing to release where nothing was routed or published, as on most fresh installs.

### Setting up a held app: a setup link on its own domain

A held app needs a way in for the person setting it up, and for no one else. Of the two candidates, the reviewer's (answer only the asker on the real domain for an hour) is taken, with one change: the asker is told apart by a setup link, not by their address.

**Why not by address:**
- An address list can't tell visitors apart behind a tunnel or a proxy that's named as Cloudflare's (ADR 0069), and the owner's own servers sit behind one.
- An address is shared by everyone behind the same NAT.

**Why not Open through the panel:**
- A panel reached at its domain doesn't open apps yet (ADR 0061's second slice), so a person would type an SSH tunnel.
- A local address breaks apps that keep the address they're set up at: WordPress keeps it, and Vaultwarden's web vault needs HTTPS.

**How the setup link works:**
- "Set it up at blog.example.com" makes a key, puts its hash and an expiry an hour away on the held route (`Route.setup`), and opens `https://blog.example.com/.well-known/dockwell-setup?key=...` in a new tab.
- The route answers that address once: it sets a cookie for the key, `Secure`, `HttpOnly` and `SameSite=Strict`, and redirects to `/` without the key.
- After that, a request with the cookie passes, and every other request gets "isn't open yet".
- The key is in the person's browser and on no change. It's made by a person signed in to the panel, with a passkey, and it's a plan, as every route edit is.
- The page counts the hour down, and says "Set up · Open it to everyone" when `done` reads it set up.
- Open on localhost and the SSH tunnel command stay as fallbacks.

### Vaultwarden: held until its owner has registered

- **Why Dockwell doesn't make the account:** its key comes from the person's master password, in their browser, so Dockwell never makes the account.
- **At install:**
  - Sign-ups are closed.
  - The install generates an admin token. The service's `ADMIN_TOKEN` is its Argon2 PHC hash. The plain token stays sealed and reaches the agent only for the call that sends the invite.
  - Its steps invite the owner's email through the admin API.
- **Held until the owner has registered.** It stays held until `done` reads, through the admin API's user list, that the invited user has registered with a master password. That's observed, never assumed from the invite.
- **The only way in:** the owner registers through the setup link, on HTTPS. With no mail set up, an invited address registers without a token, so the hold and the link are the only way in. Sign-ups stay closed throughout.
- **Once the owner has registered:** the release also takes `ADMIN_TOKEN` out, which turns `/admin` off. A later need for it sets it again, as a plan.

### Installs by token

- `InstallCatalogEntryRequest.owner_email` gives the owner's email, and `signups_open` the switch. A person in the panel starts from their own email.
- An install with an owner kind that needs an email, made with a token and no `owner_email`, installs held (`CatalogPin.held`). Its page asks for the email to make the owner.
- **Opening an install that `done` doesn't read as set up** is host access, as ADR 0101 makes a public port:
  - a signed-in person approves it with a passkey;
  - its plan says "Anyone who reaches blog.example.com first takes WordPress";
  - a token alone can't apply it.

### Store-converted apps

- **Held unless the entry says how the app is owned.** A converted app with web is held unless its entry, from the converter or an overlay, has an owner way: owner steps, a known password, an admin its environment makes (`sign_in` with a password input), or `none`. Whether it has an overlay counts for nothing, as most overlays only fix words.
- **A store form's admin password.** A converter that reads one sets `sign_in` from it, so that app is made by its environment and isn't held.
- **A store's published credentials** (Umbrel's `defaultUsername` and `defaultPassword`, CasaOS's tips) become the entry's `known_user` and `known_password`.
  - Where the store uses a variable for them, the converter generates it.
  - Where they're fixed and no steps change them, the app is held, and opening it can't be one click: the Open plan says "It signs in as admin with a password everyone knows · Change it first". The plan is refused until `done` reads the known sign-in refused.
- **Never held:** imports and takeovers of running apps (ADR 0056, 0089), and upgrades of installs from before.

### Per app

Each entry's owner way is proven on its real image in the catalog's checks before it ships, signing in as the owner from a fresh client, not only checking a status. A slice that finds an app otherwise moves it to the hold.

| Kind | App and how |
|---|---|
| Steps, a command | Gitea: `gitea admin user create --admin`, run as `git` in the running container, as it refuses root, with the password from the environment. |
| Environment | OpenList: `OPENLIST_ADMIN_PASSWORD`, set once. Its user is the fixed `admin`, which the page shows in place of the email. |
| Steps, requests | Ghost: `/ghost/api/admin/authentication/setup/`, with `X-Forwarded-Proto: https`. n8n: `/rest/owner/setup`. Jellyfin: read `/Startup/User`, post it, set `/Startup/Configuration`, then `/Startup/Complete`. Home Assistant: `/api/onboarding/users`, with a `client_id` that's a URL of the instance; the person finishes onboarding after signing in. Coder: `/api/v2/users/first`. new-api: `/api/setup`, then registration off; the proof also tries the one-api line's old `root` and `123456` and fails the entry if it works. WordPress: `/wp-admin/install.php?step=2`, as a form; its `WP_HOME` and `WP_SITEURL` are address variables (ADR 0103), so its stored address follows its domain. Memos: its sign-up, whose first account is its host, then registration off. |
| Known password | Umami: sign in as `admin` with `umami`, capture the token and the user's ID, then change the user name and password. |
| Invite | Vaultwarden, as above. |
| None | Adminer. |
| Held | Uptime Kuma (setup is a socket.io event) and Plausible (its form carries a CSRF token). |

**Ghost and mail.** Ghost emails a sign-in code on each new device, and with no mail its owner is locked out. Its entry turns staff device verification off (`security__staffDeviceVerification: false`) until its mail need (ADR 0099) is met, and the plan that meets the need turns it back on.

### What a person sees

- **The install sheet:**
  - "Owner: you@example.com · Change", prefilled from their account and required, where the kind needs an email.
  - "Sign-ups: Closed", with a switch.
  - A held entry: "Held until you set it up. You open it to everyone after."
- **The plan:**
  - "Make the owner account in Gitea as you@example.com";
  - "Sign-ups closed";
  - "Held: its domains answer that it isn't open yet".
- **Once made:** "Gitea's owner is you@example.com", with Copy password, which asks for a passkey, and Open Gitea.
- **The app's page:**
  - "Owner: you@example.com", or the fixed user, and the Sign in card with Reveal and Rotate.
  - **A held app:** "Held: only people in this project reach it until its owner account exists", with:
    - "Set it up at blog.example.com" (the setup link);
    - "Open <app>" (ADR 0061) or the SSH tunnel command;
    - "Open it to everyone".
  - **A failed owner step:** "Couldn't make the owner: ... · Try again".
- **An install from before this:** it has no `AppOwner` and no hold, so nothing changes by itself.
  - Where `done` reads that the app isn't set up and a route sends to it, the page says "Anyone who reaches git.example.com can take it", with "Hold it" (a plan that holds its routes) and "Make my account" (a plan that adds `AppOwner`, after which the owner's being made is observed as above, and the hold lifts only then).
  - Where the entry has no `done`, it says "Dockwell can't tell whether its owner is set up · Hold it".

### Numbers

These were checked against main, every pushed branch and the local worktrees on 2026-11-17.

- **Entry** (`dockwell.catalog.v1alpha1`):
  - `Entry.owner` = 33.
  - The new message `Owner`: `service` 1, `email_input` 2, `user_input` 3, `password_input` 4, `steps` 5, `done` 6, `user_pattern` 7, `reset` 8, `none` 9, `known_user` 10 and `known_password` 11.
  - `OwnerStep`: a oneof of `command` 1 and `request` 2.
  - `OwnerCommand`: `words` 1, `user` 2 and `one_off` 3.
  - `OwnerRequest`: `method` 1, `path` 2, `content_type` 3, `body` 4, `headers` 5, `expect_status` 6 and `capture` 7.
  - `OwnerCheck`: `request` 1, `status` 2 and `contains` 3.
  - `Signups.steps` = 4 and `Signups.check` = 5.
  - `GenerateAs.GENERATE_AS_PASSWORD` = 3.
- **Resource:**
  - `Spec.app_owner` = 160, with the new message `AppOwner`.
  - `CatalogPin.held` = 86 and `CatalogPin.signups_open` = 87.
  - `Route.setup` = 81, with the new message `RouteSetup`: `key_hash` 1 and `until` 2. #785's work takes `Route.publish` = 340.
- **API:**
  - `InstallCatalogEntryRequest.owner_email` = 9 and `signups_open` = 10.
  - `CatalogInstall.owner` = 13, with the new message `CatalogOwner` (email, user, state, error, set up), and `CatalogInstall.held` = 14. #789 takes 11 and 12.
  - The new RPCs `RetryCatalogOwner`, `ResetCatalogOwnerPassword` and `SetUpCatalogInstall`, each with a decision in the panel's methods.
- **Events:** `Event.app_owner` = 150 and `EventKind.EVENT_KIND_APP_OWNER` = 150.
- **Agent protocol:**
  - 380 `MakeAppOwnerCall` and 381 `MakeAppOwnerResult`;
  - 382 `ReadAppOwnerCall` and 383 `ReadAppOwnerResult`, for `done` and `signups.check`;
  - 384 to 389 kept for this record.
- **Store:** migration 0047 (`app_owners`). Main ends at 0045, and #785's work takes 0046.
- **Note reasons** (words, no numbers): `owner_made`, `owner_failed`, `owner_held`, `signups_closed`, `known_password` and `setup_link`.

## What we turned down

- **Asking the person to make the account first, as today.** It leaves every app open to the first visitor once it has a domain, and it's a sentence telling a person to do something by hand.
- **Making every account through the app's web UI, as a browser would.** CSRF tokens, sockets and wizards differ by app and version. A command or a request each entry proves is smaller, and an app with neither is held instead.
- **Holding every catalog app until opened.** An app whose owner is made, or that signs in with an admin its environment makes, has no first visitor to fear, and a hold would cost a click for nothing.
- **Clearing the hold in the change that makes the owner.** The change applies before the app runs, so a failed step would leave an app unheld with no owner.
- **Rolling an install back when its owner step fails.** It removes an app a person asked for, over something a retry or a hand setup fixes.
- **Showing the password once, outside the reveal.** Whatever carried it, a change or an answer, would be readable without a passkey.
- **Holding a domain for the asker's address.** See the setup link above.

## Consequences

- No catalog app is open to a stranger once it has a domain: its owner is made at install, it starts with a password no one else knows, or it's held until its owner sets it up.
- Gitea, Vaultwarden, Memos and new-api install with sign-ups closed, and opening a held app closes them.
- A domain added to a held app answers that it isn't open, except to the browser with the setup link.
- A converted store app with no owner way takes one more click to open to everyone, and one with a published password can't be opened until it's changed.
- An entry's owner steps are a contract with the app's version. An upgrade runs nothing again, as the account exists, but a release that changes how its app makes an owner needs its steps proven again in the catalog's checks.
- The catalog's check refuses an entry with web that has neither an owner way nor `first_visitor_owns`, and one that speaks of a default password without `known_password`.

## Order of work

1. This record, the proto and migration 0047, `AppOwner`, the agent's call, the scrubbing and its test, and the steps for Gitea, Ghost and n8n, proven on real images in CI's Docker job. Sign-ups close at install. The hold as a worked-out fact, with routes, host-port routes and ports.
2. The setup link, Umami's known password, Memos' and new-api's sign-ups, WordPress (which needs ADR 0103), Uptime Kuma and Plausible held, `done` and the page's states, and converted apps.
3. The rest of the 13, Vaultwarden's invite, Rotate, and installs from before.
4. Converters' default credentials and admin passwords, and overlays for the store apps used most.

## Review

The review of 2026-11-17 (accept after changes) maps to this revision as follows.

| Finding | Where it's addressed |
|---|---|
| H1, the hold cleared in desired state | "The owner is a resource, and its being made is observed" and "Held is a fact the control plane works out": `made` is observed (381, `app_owners`, migration 0047), a failed step leaves the app installed and held with Try again, and release is a change after the fact. |
| H2, Vaultwarden's invite | "Vaultwarden: held until its owner has registered": held until `done` reads the user registered, `ADMIN_TOKEN` as an Argon2 hash, sign-ups closed, the setup link as the only way in, `/admin` off after. |
| H3, the password in argv, logs and the change | "Steps" (inputs by environment or stdin, no redirects, nothing kept, 512 scrubbed bytes, a test) and "The owner's password" (for the step alone, never in desired state, revealed by passkey, Rotate). |
| H4, Umami's default password | "How an entry says its app is owned", the known-password kind with `capture`, Umami in "Per app", and the catalog's check. Umami is the only entry with a published default. |
| M1, sign-ups in the app's settings | "Sign-ups close": `signups.steps` and `signups.check`, Memos and new-api, held until closed. |
| M2, sign-ups after opening | "Sign-ups close": `OpenCatalogInstall` closes them unless the pin says open. |
| M3, host-port routes and port edits | "Held is a fact...": host-port routes and `PortMapping.held` kept through any edit, with tests. |
| M4, a terminal to set up | "Setting up a held app": the setup link, with the reasons for and against each option. |
| M5, password rules and user names | "The owner's password" (`generate_as: password`) and "How an entry says...": `user_input` required, with `user_pattern`. |
| M6, shown once against reveal | "The owner's password": revealed by passkey any time, Rotate by `reset` or Forget it, nothing on the change. |
| M7, installs and opening by token | "Installs by token": `owner_email` and `signups_open`, held without an email, opening an unconfirmed install needs a person and a passkey. |
| M8, converted apps | "Store-converted apps": the test is an owner way, not an overlay; defaults mapped; no one-click open with a known password; imports and upgrades never held. |
| M9, redirects, Host and WordPress's address | "Steps" (no redirects, Host, `X-Forwarded-Proto`) and WordPress's address variables in "Per app". ADR 0103 is listed as a dependency. |
| M10, Ghost without mail | "Ghost and mail" in "Per app". |
| L1 | "only people in this project reach it". |
| L2 | "Per app": Gitea by exec as `git`, OpenList by env and its fixed user, Jellyfin's order, Home Assistant's `client_id`, new-api's old default, WordPress placed. |
| L3 | "An install from before this": the "can't tell" state. |
| L4 | The status lines. |
| L5 | "Numbers", all reserved. |
| L6 | "Held is a fact...": tunnels. |
