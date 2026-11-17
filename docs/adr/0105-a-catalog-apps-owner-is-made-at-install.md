# 0105. A catalog app's owner is made at install

- Status: Accepted (2026-11-17, after a review and its re-check).
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
- **`done`:** a check that says it's made. Where the app has a sign-in Dockwell can try, `done` signs in as the owner with the sealed password.
- **`reset`:** optional steps that set a new password.
- **`made_when_done`:** the steps succeeding isn't the owner being made, as for an invite. The owner counts as made only once `done` reads it too.

**Starts with a known password.** `known_user` and `known_password` say what the app starts with.
- The `steps` sign in with them and change them to the owner's, the password first: Umami signs in as admin, changes its password, then changes its user name.
- Its `done` reads that the owner signs in with the sealed password, and that the known password is refused for the known user and for the owner's user, so a run that stopped after the user name changed doesn't read as set up.
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
- **Inputs never go in argv.** They reach a command through its environment, or its standard input. A command whose words name a secret, as `"$NAME"` in a `sh -c` line does, is refused, as the shell writes the value into the command's own arguments. Where an app takes a password only as an argument, the app makes one up, a step captures it from what the command prints (`capture`), and a request sets the owner's own through the app's API, signing in with the made-up one (`DOCKWELL_BASIC_<NAME>`). Where an app reads the password from its environment, the entry uses that in place of a step, as OpenList's `OPENLIST_ADMIN_PASSWORD` does.
- **A request never follows a redirect.** An answer of 3xx fails the step, so a body with a password never goes to another address.
- **A request's `Host` is the app's address** (ADR 0103), and `X-Forwarded-Proto` may be set, as an app behind a proxy expects it.
- **Nothing a step prints or answers is kept,** beyond what's below. A one-off container is removed when its step ends, and its output is never kept as a task run's.
- **Captured values live in the agent's memory, for the call alone.** They're never sent back or kept.
- **The app's answer is scrubbed, then cut.**
  - Where a step fails, the agent first replaces, in the whole answer or output, each filled input's value and each captured value with `[hidden]`. It replaces their plain, URL-encoded, JSON-escaped and HTML-escaped forms, and the base64 of `user:password`, as an echoed `Authorization` header carries.
  - Only then does it cut the answer to 512 bytes, so no part of a value survives a cut.
  - The cut answer is all that's stored, logged, sent as an event or put on a change.
  - A test searches the store, the events, the change and the agent's log for every value and form, with a value placed across byte 512.
- **Steps are safe to run again.** A run can stop part-way, as when the account is made and the answer is lost. So Try again first reads `done`, and records the owner made where it reads made. An entry orders its steps so that each one that ran leaves the next able to run, or makes a step safe to repeat. The catalog's proof runs each entry's steps twice, and stops a run after each step and then tries again.

### The owner's password

- **It's generated** with `generate_as: password`: letters and digits alone, so every text form of it is the plain one, with at least one upper-case letter, one lower-case letter and one digit in every value, within the input's `min_length` and `generate`. n8n, for one, refuses a password without them.
- **It's sealed** as the install's secret, as every generated input is.
- **A password owner steps use reaches the agent for the step alone.** The control plane sends it in the call that runs the step. It's never part of the agent's desired state, so it's never in the agent's cache or sent again in a fill (ADR 0054). An admin its environment makes, as Grafana's, is a value of the service's environment, sealed and cached as ADR 0054 allows.
- **It's revealed with a passkey, any time,** in the session that asks, from the app's Sign in card. It's never on a change, the install's answer or an event.
- **Rotate:**
  - Where the entry has `reset`, "Make a new password" generates one and seals it as pending before the call, beside the current one. It then runs `reset`, and keeps both until the outcome is known, by `reset`'s answer or, where that's lost, by `done` signing in with one of them. The one that signs in stays.
  - Where it has none, "Changed it in Gitea? · Forget it" is a plan that deletes the sealed copy, and the card says where the app changes it (`sign_in.changed_in`).
  - **As built in slice 3:**
    - `ResetCatalogOwnerPassword` takes a person signed in to the panel and a passkey (step-up 261, by the install's project ID). It refuses an owner that isn't made, one whose new password is being set (`rotating`), and an entry without `reset_steps` (`no_reset`).
    - It generates the password as the entry's password input does, and seals it beside the owner's row, for that rotation alone (`app_owners.pending`, sealed as `app.owner.pending` at the rotation's ID). The owners runner runs `reset_steps` with it in place of the current one.
    - Where the steps answer made, a change of Dockwell's puts it on the password's secret, holding no password, and the rotation ends. Where the answer is lost, it tries again, reading `done` with the new one first. Where a step says no, `done` reads which one signs in: the new one is kept, or the current one stays, with what the app said, scrubbed. Where neither does yet, both are kept.
    - After the slice 3 review (H1), only a password read signing in ends a rotation: the new one read yes is kept, the current one read yes stays and the new one goes. A no, no answer, an upgrade to a release without reset steps (done is then read from the run that makes the owner), an unreadable catalog or a store error keeps both sealed. The reset steps run three times at most, then done is read every five minutes, and after twelve reads hourly, with no end. Past those twelve, the card says why ("Neither password signs in now", or that Dockwell can't read which one it takes) and offers "Show the new one" and "Keep the current one", each with a passkey and the role that reveals the owner's password. What's shown goes to no log, event or cache. A refusal's words are scrubbed of both passwords (L2), and a rotation whose owner is gone or isn't made ends (L3).
    - The card's "Make a new password" says "Setting a new password in Gitea" while it's set. The current one works until then.
    - "Forget it" is a plan that deletes the password's secret, with no passkey, as any secret's deletion, which undo puts back (L1). An `app.owner` holds the secrets its steps read as references (`held_secrets`) only while its owner isn't made, so none can be deleted then (the review's H2), and Dockwell clears them once the owner is made or opened, so the plan isn't refused. An upgrade never generates a made owner's forgotten password again (L1). An invite's owner (Vaultwarden) has no password.
    - The owners runner goes on past one owner's error. A secret its steps read that's gone fails that owner alone: "its secret OWNER_PASSWORD, which the owner's steps read, was deleted. Undo that change on Activity, then try again".
    - Gitea's entry resets with the steps after making the user. Umami's has none: its reset would need the current password, which `reset_steps` don't send.

### The owner is a resource, and its being made is observed

- **`AppOwner` is a resource kind.** The install writes it, with the service, the email, the user, the kind, and the password's secret by reference, never its value. The plan says "Make the owner account in Gitea as you@example.com, once gitea is healthy". `DatabaseUser` is the model.
- **Whether it's made is a fact Dockwell observes, never a field a change writes.**
  - After the install's change applies and the service is healthy, the control plane asks the agent to run the steps (agent protocol 380).
  - The agent answers with the outcome (381).
  - The control plane records it in the store's `app_owners` table (migration 0048), with the state, the scrubbed error, the tries, and when.
- **The states:**
  - `waiting_email`: a token install with no owner email;
  - `waiting`: steps not yet run, or running;
  - `made`: the steps succeeded, sign-ups' steps too where they run, and `done` reads it where the entry says `made_when_done`;
  - `failed`;
  - `opened`: a person opened the install before the owner was made.
- **Made and opened are final.** Once a row reads made, closed or registered, no later reading takes it back into a hold. `done` and `signups.check` are read again only for the page.
  - A later reading that finds sign-ups open again, or the owner gone, raises an alert with its action, as "Memos' sign-ups are open again · Close them" or "Vaultwarden's owner is gone · Hold it".
  - It never re-holds a live app silently.
- **A failed step never rolls the install back.**
  - The change stays applied, with a note: "Dockwell couldn't make Gitea's owner".
  - The app stays installed and held.
  - Its page says "Couldn't make the owner: <the app's answer, scrubbed> · Try again". Try again calls `RetryCatalogOwner`, which reads `done` first and runs the steps only where it doesn't read made. The fallback is "Set it up yourself", which stays held until the person opens it.
- **A made owner is never made again.** A later change to `AppOwner` makes nothing in the app. Deleting it leaves the account in the app, and the plan says so.

### Sign-ups close

- **Closed by a variable:** an entry whose `signups` names a variable writes the closed value at install: Gitea's `GITEA__service__DISABLE_REGISTRATION=true`, Vaultwarden's `SIGNUPS_ALLOWED=false`.
- **Closed by the app's settings:** `signups.steps`, run right after the owner's steps and in the same call, with the owner's session captured. A check, `signups.check`, reads them closed.
  - Memos sets "disallow user registration" in its instance settings.
  - new-api turns password registration off through its options.
  - The owner isn't made until `signups.check` reads them closed, so the app stays held until then, and Memos' `first_steps` goes.
  - Where the pin says sign-ups open, `signups.steps` don't run and `signups.check` isn't needed for made.
- **The install sheet's switch:** "Sign-ups: Closed", with a switch to open them. The pin records the choice (`CatalogPin.signups_open`).
- **Opening closes them too.** `OpenCatalogInstall`'s plan closes sign-ups where the entry says how and the pin doesn't say open: "Open Memos to everyone, with sign-ups closed".

### Held is a fact the control plane works out, from the install's own state

**What holds an install.** An install is held while either:
- `CatalogPin.held` is set. An install sets it for an entry with `first_visitor_owns` and no owner steps, and for a converted app with no owner way (below).
- The install has an `AppOwner` whose `app_owners` row is neither made nor opened.

**The current entry never counts.** A hold is never worked out from the entry an install's pin names now, only from the install's own pin and `AppOwner`.
- **An install from before this record** has no `AppOwner` and no `held`, so it's never held. Its entry gaining owner steps in a release changes nothing for it; only "Make my account" adds an `AppOwner`.
- **An upgrade to a release whose entry lost its owner steps** keeps the install's `AppOwner`. Where its row isn't made, the upgrade sets `CatalogPin.held`, so the install stays held after the steps are gone.
- **An upgrade to a release whose entry gained owner steps** adds nothing to an install from before. It runs the steps for no install whose `AppOwner` is already made.
- **Deleting an `AppOwner` that isn't made or opened** sets `CatalogPin.held` in the same change, whatever path deletes it, a whole-spec write included, so removing the owner never lifts the hold. The changes handler enforces it, as it keeps `held`.

**Every pin write keeps the hold.** Every write of a pin keeps `held` and `signups_open` as they were. That covers an install, an upgrade (one by a person, and one ADR 0100 applies by itself), `SetCatalogNeed`, `SetCatalogStorage`, a companion, and a change that writes the project's spec directly. The one exception is an applied `OpenCatalogInstall`. The changes handler enforces it for every write, as it keeps `PortMapping.held` (below), so a path that rebuilds the pin, as `pinTo` does on main, can't drop them.

**How each hold lifts:**
- **First visitor, converted app, or set up by hand.** An applied `OpenCatalogInstall`, by a person with a passkey, clears `CatalogPin.held` and records the `AppOwner`, where there is one, as `opened`.
- **Owner steps, a known password, sign-ups by steps.** The row reads made, then the release (below).
- **Vaultwarden's invite** (`made_when_done`). The steps succeed and `done` reads the owner registered, then made, then the release.
- **A token install with no email.** Its `AppOwner` waits in `waiting_email`, not on the pin. When a person gives the email (`SetCatalogOwnerEmail`, a plan), the steps run, and made lifts it.
- **A failed owner that a person sets up by hand.** They open it, which records `opened`.

No change clears a hold in the same write that asks for the owner. Only an observed fact or an applied open lifts it.

**While an install is held:**
- **Routes.** A route that sends to one of its services is held (`Route.held`): it answers that the app isn't open yet. So is a route whose `host_port` is one of its published host ports, as that dials the same port. The changes handler's hook holds them at plan time, for routes added later too, as ADR 0103's address hook works.
- **Ports.** An install that starts held writes `held` on every port it publishes beyond loopback, ADR 0101's public ones included. A `PortMapping` of its services keeps `held` whatever an edit sends, including a whole-spec edit that leaves the field out, and a mapping an edit adds is written held.
- **Tunnels.** ADR 0104's tunnel rules point at Caddy, so a held route covers them. A taken-over tunnel rule that points at a held install's port directly is refused, saying so.

**Tests:**
- a route added after the install, a host-port route, a ports edit, a new mapping, a whole-spec edit and a tunnel rule;
- an upgrade, an automatic upgrade, `SetCatalogNeed`, `SetCatalogStorage` and a direct write of the project's spec, each keeping `held` and `signups_open`;
- an entry that gains owner steps, which holds no install from before;
- an entry that loses them, which leaves a failed install held;
- a whole-spec write that drops an `AppOwner` not yet made, which leaves the install held.

**The release.** When a hold lifts by an observed fact, the control plane releases what it held as a change of its own, made by Dockwell and applied at once, as automatic updates are: "Gitea's owner exists: blog.example.com answers it". The release also clears `Route.setup`. There's nothing to release where nothing was routed or published, as on most fresh installs.

**Late plans.** A route edit planned while the install was held, and applied after the release, lands held. So the control plane releases any held route, or held port, of an install that isn't held whenever it sees one, not only in the one release change.

### Setting up a held app: a setup link on its own domain

A held app needs a way in for the person setting it up, and for no one else. Of the two candidates, the reviewer's (answer only the asker on the real domain for an hour) is taken, with one change: the asker is told apart by a setup link, not by their address.

**Why not by address:**
- An address list can't tell visitors apart behind a tunnel or a proxy that's named as Cloudflare's (ADR 0069), and the owner's own servers sit behind one.
- An address is shared by everyone behind the same NAT.

**Why not Open through the panel:**
- A panel reached at its domain doesn't open apps yet (ADR 0061's second slice), so a person would type an SSH tunnel.
- A local address breaks apps that keep the address they're set up at: WordPress keeps it, and Vaultwarden's web vault needs HTTPS.

**How the setup link works:**
- **The key.** "Set it up at blog.example.com" calls `SetUpCatalogInstall`, as a person signed in to the panel, after a passkey step-up. It makes a key of 32 random bytes and writes its SHA-256 hash, with an expiry at most an hour away, on the install's held routes of that domain (`Route.setup`), as a plan. It then opens `https://blog.example.com/.well-known/dockwell-setup?key=...` in a new tab. The key is in the person's browser and on no change.
- **The check, as ADR 0087's sign-in is.** The held route's setup path, and its hold, go through `forward_auth` to the route's agent (`internal/agent/signin`), never a comparison in Caddy's config, which would put a value there and in its saved copy on the server.
  - The agent compares the key's SHA-256 with `key_hash`, and remembers a used key until `until`, so a key works once.
  - It then sets a cookie of its own, never the key: `__Host-dw_setup_<route>`, which is `Secure`, `HttpOnly`, `SameSite=Strict`, `Path=/` and has no `Domain`. Its value is one the agent signs with the route's key, bound to the route's ID, its domain and `until`.
  - As a `__Host-dw_` cookie, the sign-in's rules already strip it from what goes to the app, and refuse it from the app.
- **The going page.** The setup address answers with a page that goes on to `/` by itself, as a page of the site's own, as `internal/agent/signin/pages.go` ends a sign-in. So the Strict cookie it set is sent on the next request. It's never a 3xx. It sends `Referrer-Policy: no-referrer`, so the key never reaches the app as a referrer.
- **What the cookie lifts.** It lifts the hold alone, never a route's own access (ADR 0069's addresses and passwords, ADR 0087's sign-in), which still applies. One key covers the install's held routes of its domain, path routes included. Every other request gets "isn't open yet".
- **Over plain HTTP.** A domain the browser doesn't reach over HTTPS (`tls: off` with `plain_for_anyone`, and no proxy in front that serves HTTPS) gets no link, as a `Secure` cookie isn't kept over HTTP. It falls back to Open (ADR 0061).
- **Who writes `Route.setup`.**
  - Only `SetUpCatalogInstall` writes it.
  - The install, `RetryCatalogOwner` and the release clear it.
  - The changes handler keeps the field as it was on every other write: a general route edit, a token's, the CLI's or a whole-spec edit neither adds, changes nor drops it.
  - A test proves each.
- **On the page.** The page counts the hour down, and says "Set up · Open it to everyone" when `done` reads it set up.
- **Fallbacks.** Open on localhost and the SSH tunnel command stay as fallbacks.
- **As built in slice 2:**
  - The proxy asks the route's agent at `/setup/<route>` through `forward_auth`, over the sign-in's socket, in place of the held answer. The agent keeps each route's key hash and end (`signin.Keep.PutSetup`) and its cookie key, made on the server.
  - A key works once across a restart: the agent keeps each used key's hash in its data folder, beside the routes' cookie keys, until its link ends.
  - A link's end is the control plane's, held once, when the agent is given it, to an hour from then, and read with a minute's leeway, so a clock a little apart neither locks a browser out nor keeps a link longer.
  - One use sets a cookie for every held route of the domain with the key's hash, path routes included.
  - Caddy's default log deletes `key`, `code` and `state` from every request address it writes, so a link whose agent didn't answer leaves no working key in the ingress's log.
  - The cookie is named `__Host-dw_<12 hex>`, from a hash of the route apart from its sign-in's, the shape every route's proxy already strips on the way to the app and from what the app sets. A name of its own would have needed a new rule on every route, and a route written before it would have read as edited.
  - The link's page is a 403 that goes on to the route's path by itself, never a 2xx, which would send the key on to the app.
  - `Edit.SetsUp` marks the two writes of `Route.setup`: `SetUpCatalogInstall`'s and `RetryCatalogOwner`'s, which clears it. Every other write keeps it while the route is held, and drops it with the hold, which is how the release and Open clear it.
  - `SetUpCatalogInstall` proposes and applies its change as the person asking, so the change is theirs, and gives the link back once the change has applied on its server, a minute at most, so the first click works. It needs a person (`needs_session`), a developer's in an open environment and an admin's in a protected one, as every method a token can't call is.
  - Whether a domain is reached over HTTPS is `route.PlainAtTheBrowser`, which the panel mirrors: every held route of the domain must pass it.
  - `RetryCatalogOwner` takes the link away first, as a change it applies, so in a protected environment it's an admin's. An undo or a rollback keeps only the link stored now, never an older one.
  - The page counts the hour down from `DomainRoute.setup_until`, the link's end alone, so any page shows it.

### Vaultwarden: held until its owner has registered

- **Why Dockwell doesn't make the account:** its key comes from the person's master password, in their browser, so Dockwell never makes the account.
- **At install:**
  - Sign-ups are closed.
  - The install generates an admin token. The service's `ADMIN_TOKEN` is its Argon2 PHC hash. The plain token stays sealed and reaches the agent only for the call that sends the invite.
  - Its steps invite the owner's email through the admin API.
- **Held until the owner has registered.** Its entry says `made_when_done`. So it stays held until `done` reads, through the admin API's user list, that the invited user has registered with a master password. That's observed, never assumed from the invite. While it's held, `done` is read (382) when its page asks, and every five minutes.
- **The only way in:** the owner registers through the setup link, on HTTPS. With no mail set up, an invited address registers without a token, so the hold and the link are the only way in. Sign-ups stay closed throughout.
- **Once the owner has registered:** the release also takes `ADMIN_TOKEN` out, which turns `/admin` off. A later need for it sets it again, as a plan.

- **As built in slice 3:**
  - The steps sign in to `/admin` with the sealed token, capturing the `VW_ADMIN` cookie, and invite the owner's email, skipped where `/admin/users` has them. `done` signs in the same way first (`before`) and reads `[email=${OWNER_EMAIL}]._status=0`, where 1 is invited.
  - Its entry raises the admin page's rate limit to 30 sign-ins a minute, as Dockwell signs in for each read, until the page goes off.
  - ADMIN_TOKEN goes once the owner is made or opened, as observed state the runner works towards at each look (the review's M1): a change of its own, which also lets go of the secrets the owner held, and an upgrade's three-way merge keeps. Where it fails, it's tried again a minute later, then five, then fifteen, then hourly. After an hour, Activity says so once (`app_owner.unset_failed`), and the app's page says "Vaultwarden still takes ADMIN_TOKEN · Turn it off", which plans the same change (`SettleCatalogOwner`). Once done, it's recorded settled, so a later write that sets it again on purpose stays.
  - The invite's skip reads `[email=${OWNER_EMAIL}].object=user`, and a pick compares in any case, so an owner's email in mixed case, which Vaultwarden keeps in lower case, skips a second invite (M3).
  - The plain token and its hash stay as secrets once the page is off (L5), unused and revealable only with a passkey: deleting them would have an upgrade generate them again. Each read of done while the owner hasn't registered sends the plain token to the agent for the sign-in, in a request body and never in argv, scrubbed from what's said (L6).
  - The real-Docker test signs in with Vaultwarden's own check of the PHC Dockwell writes, invites, reads the owner invited, registers as an invited address does with no mail, and reads them registered.
  - The plain token is a sealed secret of the environment, as every generated input is. Nothing Dockwell writes, logs or says holds it; a person with a passkey can reveal it, as any secret, while the admin page is on.

### Installs by token

- `InstallCatalogEntryRequest.owner_email` gives the owner's email, and `signups_open` the switch. A person in the panel starts from their own email.
- An install with an owner kind that needs an email, made with a token and no `owner_email`, installs held: its `AppOwner` waits in `waiting_email`. Its page asks for the email, and giving it (`SetCatalogOwnerEmail`) runs the steps, after which made lifts the hold as for any install.
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
- **As built in slice 3:**
  - An install's source says whether it's converted. A converted entry with web installs held unless it has owner steps, `none`, or `sign_in` with a password input. An overlay's `owner` carries over.
  - Umbrel's `deterministicPassword` becomes `sign_in`, with `defaultUsername` and its own APP_PASSWORD, which its environment makes. A fixed `defaultPassword` becomes `known_user` and `known_password` with no steps, which the check takes for a converted entry: it's held. Its held page shows the user and password to copy, and its Open plan notes `known_password`, "It signs in as admin with a password everyone knows. Change it in Kiosk first: its held page shows it", for which the panel asks a passkey up front and for "I changed its password" before Apply (the review's M2). A password with no user says so alone, never "admin" (L4).
  - Not built: refusing that Open until the known sign-in reads refused, as a converted entry has no `done` to read it; and CasaOS's tips, which are prose.

### Per app

Each entry's owner way is proven on its real image in the catalog's checks before it ships, signing in as the owner from a fresh client, not only checking a status. A slice that finds an app otherwise moves it to the hold.

| Kind | App and how |
|---|---|
| Steps, a command and a request | Gitea: `gitea admin user create --admin --random-password`, run as `git` in the running container, as it refuses root; the step captures the password Gitea made up, and a request then sets the owner's own over `PATCH /api/v1/admin/users/<user>`, so no process's arguments hold it. |
| Environment | OpenList: `OPENLIST_ADMIN_PASSWORD`, set once. Its user is the fixed `admin`, which the page shows in place of the email. |
| Steps, requests | Ghost: its setup and its `done` as commands in its container, `node` asking its own loopback, the only address whose `X-Forwarded-Proto` Ghost believes, with the app's Host and scheme, so a Ghost reached over HTTPS answers rather than redirects. n8n: `/rest/owner/setup`. Jellyfin: read `/Startup/User`, post it, set `/Startup/Configuration`, then `/Startup/Complete`. Home Assistant: `/api/onboarding/users`, with a `client_id` that's a URL of the instance; the person finishes onboarding after signing in. Coder: `/api/v2/users/first`. new-api: `/api/setup`, then registration off; the proof also tries the one-api line's old `root` and `123456` and fails the entry if it works. WordPress: `/wp-admin/install.php?step=2`, as a form; its `WP_HOME` and `WP_SITEURL` are address variables (ADR 0103), so its stored address follows its domain. Memos: its sign-up, whose first account is its host, then registration off. |
| Known password | Umami: sign in as `admin` with `umami`, capture the token and the user's ID, change the password, then the user name. As built in slice 3, one request changes both, `POST /api/users/<id>` as admin, so no try that stops part-way leaves the known password under the owner's name; its `done` signs in from inside its container as the owner, and as `admin` and the owner's name with `umami`, refused. |
| Invite | Vaultwarden, as above. |
| None | Adminer. |
| Held | Uptime Kuma (setup is a socket.io event) and Plausible (its form carries a CSRF token). |

**Ghost and mail.** Ghost emails a sign-in code on each new device, and with no mail its owner is locked out. Its entry turns staff device verification off (`security__staffDeviceVerification: false`) until its mail need (ADR 0099) is met, and the plan that meets the need turns it back on. As built in slice 1, the entry turns it off and nothing turns it back on: Ghost's entry has no mail need yet, so it stays off with mail too, until that need comes with the step that turns it on.

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
  - Where `done` reads that the app isn't set up and a route sends to it, the page says "Anyone who reaches git.example.com can take it", with "Hold it" (a plan that holds its routes) and "Make my account".
  - "Make my account" is a plan that adds `AppOwner` and holds the routes already there, as "Hold it" does. The owner's being made is then observed as above, and the hold lifts only then.
  - Where the entry has no `done`, it says "Dockwell can't tell whether its owner is set up · Hold it".

### Numbers

These were checked against main, every pushed branch and the local worktrees on 2026-11-17, and amended as built in slice 1.

- **Entry** (`dockwell.catalog.v1alpha1`):
  - `Entry.owner` = 33.
  - The new message `Owner`: `service` 1, `email_input` 2, `user_input` 3, `password_input` 4, `steps` 5, `done` 6, `user_pattern` 7, `reset_steps` 8 (named so, as `reset` clashes with Go's generated `Reset`), `none` 9, `known_user` 10, `known_password` 11 and `made_when_done` 12.
  - `Signups.steps` = 4 and `Signups.check` = 5.
  - `GenerateAs.GENERATE_AS_PASSWORD` = 3.
- **Resource** (`dockwell.resource.v1alpha1`), where the step messages live, as the agent build links this package and never the catalog's:
  - `OwnerStep`: a oneof of `command` 1 and `request` 2.
  - `OwnerCommand`: `words` 1, `user` 2, `one_off` 3 and `capture` 4, regular expressions over what it prints.
  - `OwnerRequest`: `method` 1, `path` 2, `content_type` 3, `body` 4, `headers` 5, `expect_status` 6, `capture` 7 and `port` 8, the container's port.
  - `OwnerCheck`: `request` 1, `status` 2, `contains` 3 and `command` 4, for an app that answers only its own loopback as it's reached.
  - `Spec.app_owner` = 160, with the new message `AppOwner`: `service` 1, `email` 2, `user` 3, `password` 4 and `app` 12, with 5 to 11 reserved. It holds no step: the steps are the entry's.
  - `OwnerRun`, never stored: what the agent call carries, the steps from the entry at the install's release with who the owner is and where the app is reached (`host` 10, `https` 11).
  - `CatalogPin.held` = 86, `CatalogPin.signups_open` = 87 and `CatalogPin.opened` = 88. An applied Open sets `opened` and every write after keeps it, so a route or port planned while the install was held, and applied after it opened, is released at the runner's next look, though the install has no `AppOwner`.
  - `Route.setup` = 81, with the new message `RouteSetup`: `key_hash` 1 and `until` 2. #785's work takes `Route.publish` = 340.
- **Values a step names beyond its inputs:** `DOCKWELL_OWNER_BASIC`, the owner's user, or email, and password as a Basic credential; `DOCKWELL_BASIC_<NAME>`, the same with a value a step captured, as Gitea's made-up password; and `DOCKWELL_HOST` and `DOCKWELL_SCHEME`, where the app is reached.
- **API:**
  - `InstallCatalogEntryRequest.owner_email` = 9 and `signups_open` = 10.
  - `CatalogInstall.owner` = 13, with the new message `CatalogOwner` (email, user, state, error, set up), and `CatalogInstall.held` = 14. #789 takes 11 and 12.
  - The new RPCs `RetryCatalogOwner`, `ResetCatalogOwnerPassword`, `SetUpCatalogInstall` and `SetCatalogOwnerEmail`, each with a decision in the panel's methods.
- **Events:** `Event.app_owner` = 150 and `EventKind.EVENT_KIND_APP_OWNER` = 150, with the new message `EventAppOwner`: `app` 1, `email` 2, `user` 3, `state` 4, `error` 5 and `tries` 6. The actions are `app_owner.made`, `app_owner.failed` and `app_owner.opened`, each once, as an owner turns to it.
- **Step-up:** `StepUpAction.STEP_UP_ACTION_SET_UP_CATALOG_INSTALL` = 260, by the install's project ID. 260 to 269 are this record's.
- **`CatalogOwner.set_up`** = 5: a failed owner's app reads set up, by its `done`, while a setup link to it works. What was read goes once the link ends, the owner leaves failed, or it's tried again.
- **`DomainRoute.setup_until`** = 114: when a held route's setup link ends, in Unix seconds, and never its key or hash.
- **Agent protocol:**
  - 380 `MakeAppOwnerCall` and 381 `MakeAppOwnerResult`;
  - 382 `ReadAppOwnerCall` and 383 `ReadAppOwnerResult`, for `done` and `signups.check`;
  - 384 `Hello.makes_app_owners`;
  - 385 to 389 kept for this record.
- **Slice 3:**
  - `Input.hash_of` = 10: a secret that's the Argon2id PHC of another secret of the entry's own, with OWASP's parameters (m=19456, t=2, p=1), written at install, kept on upgrade, and never asked of a person.
  - `Owner.unset_when_made` = 13: variables of the owner's service its entry takes out once the owner is made, as a change of Dockwell's. Vaultwarden's ADMIN_TOKEN, which turns its admin page off.
  - `OwnerCheck.before` = 5, a request sent first whose captures are the check's alone, and `OwnerCheck.where` = 6, what the JSON answer must say as `path=value`, where a part may pick a list's object by one of its fields, in any case. A small grammar, never an expression: a key is letters, digits, `_` and `-`, and no value holds `[`, `]` or `=`.
  - A request's capture takes `cookie:NAME` beside a JSON path: the cookie its answer sets, a secret scrubbed as every capture is and kept nowhere.
  - `CatalogOwner.rotating` = 6, and `CatalogOwner.error` says, for a made owner, what kept its last new password from being set, or, past twelve reads of a rotation, why neither password reads signing in.
  - After the review: `AppOwner.held_secrets` = 13 (5 to 11 and the name "secrets" stay reserved), `CatalogOwner.unset_left` = 7, `StepUpAction` 262 (show the new password) and 263 (keep the current one), and the RPCs `RevealCatalogOwnerPendingPassword`, `KeepCatalogOwnerPassword` and `SettleCatalogOwner`. Migration 0050 also holds the settled state: `settled`, `settle_tries`, `settle_next`, `settle_since`, `settle_error`, `unset_left` and `settle_alerted`.
  - `StepUpAction.STEP_UP_ACTION_RESET_CATALOG_OWNER_PASSWORD` = 261.
  - Migration `0050_app_owner_rotation.sql`: `app_owners.pending`, `rotation`, `rotate_error` and `rotate_tries`. ADR 0104's panel branch holds 0049 locally; whichever lands first takes 0049, and the other renumbers.
- **Store:** migration `0048_app_owners.sql` (`app_owners`, with the states `waiting_email`, `waiting`, `made`, `failed` and `opened`, and the changes that open it while they're planned). It follows ADR 0104's `0046_cloudflare.sql` and the fix to Cloudflare's pending-row index, which takes 0047.
- **Plans:** `Plan.unholds`, a plan that lifts a hold, which ApplyChange takes from a person with a passkey, never a token.
- **Note reasons** (words, no numbers): `owner_made`, `owner_failed`, `owner_held`, `signups_closed`, `known_password`, `setup_link`, and `opens_to_everyone`, an Open whose owner isn't made, which the panel asks a passkey for up front, as it does for `known_password`, an Open of a store's app that signs in with a password everyone knows. Slice 3 adds `owner_password`, a change that keeps an owner's new password, and `owner_unset`, one that takes out what only making the owner needed.

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
- A converted store app with no owner way takes one more click to open to everyone. One with a published password shows it on its held page to copy, and opening it takes a passkey and the person saying "I changed its password", which Dockwell can't read for itself, as a converted entry has no `done`.
- An entry's owner steps are a contract with the app's version. An upgrade runs nothing again, as the account exists, but a release that changes how its app makes an owner needs its steps proven again in the catalog's checks.
- The catalog's check refuses an entry with web that has neither an owner way nor `first_visitor_owns`, and one that speaks of a default password without `known_password`. As built in slice 3, the second is the catalog's own check of its folder (`dockwell-catalog check`), never a server's read, so a store's converted words don't drop its app; the first isn't built yet.

## Order of work

1. This record, the proto and migration 0048, `AppOwner`, the agent's call, the scrubbing and its test, and the steps for Gitea, Ghost and n8n, proven on real images in CI's Docker job. Sign-ups close at install. The hold as a fact worked out from the install's own state, kept by every pin write, with routes, host-port routes and ports.
2. The setup link, Umami's known password, Memos' and new-api's sign-ups, WordPress (which needs ADR 0103), Uptime Kuma and Plausible held, `done` and the page's states, and converted apps.
3. The rest of the 13, Vaultwarden's invite, Rotate, and installs from before. Rotate moved here from the page's states of slice 2: no entry has `reset_steps` yet, and a real rotate keeps a pending password beside the current one, which needs its own store.
4. Converters' default credentials and admin passwords, and overlays for the store apps used most.

## Review

The review of 2026-11-17 (accept after changes) maps to this revision as follows.

| Finding | Where it's addressed |
|---|---|
| H1, the hold cleared in desired state | "The owner is a resource, and its being made is observed" and "Held is a fact the control plane works out": `made` is observed (381, `app_owners`, migration 0048), a failed step leaves the app installed and held with Try again, and release is a change after the fact. |
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

The re-check of fdbaca6fd maps as follows.

| Finding | Where it's addressed |
|---|---|
| H1, an upgrade, a need or storage change, or a new release drops the hold | "Held is a fact the control plane works out, from the install's own state": held by the pin or an `AppOwner` not made or opened, never by the current entry; every pin write keeps `held` and `signups_open` except an applied Open, enforced in the changes handler; an entry losing steps sets `held` where the owner isn't made; one gaining steps holds nothing from before; the tests are listed. |
| M1, how each hold lifts | "How each hold lifts", the `app_owners` states with `opened` and `waiting_email`, sign-ups open skipping their steps, and "Made and opened are final", with an alert and its action in place of a silent re-hold. |
| M2, Strict cookie and a redirect | "The going page": a page that goes on by itself, as the sign-in's does, with `Referrer-Policy: no-referrer`. |
| M3, how the key is checked and kept to one use | "The check, as ADR 0087's sign-in is": `forward_auth` to the agent, SHA-256 against `key_hash`, a used key remembered until `until`, a signed `__Host-dw_setup_<route>` cookie, never the key. |
| M4, any route edit can write `Route.setup` | "Who writes `Route.setup`": `SetUpCatalogInstall` alone, after a step-up; cleared by the install, Retry and the release; kept as it was on every other write; a test. |
| M5, scrubbing order and forms | "The app's answer is scrubbed, then cut": scrub the whole answer first, then cut; captured values, HTML-escaped and base64 `user:password` forms; captured values only in the agent's memory; `generate_as: password` draws letters and digits alone. |
| M6, a retry after a partial run, and Umami's order | "Steps are safe to run again": Try again reads `done` first; steps ordered or repeatable, with the proof stopping after each step. Umami changes its password first, and its `done` checks the owner's sign-in and the known password for both users. |
| M7, the invite kind's field | `Owner.made_when_done` (12), read by the page and every five minutes while held. |
| L1 | "Ports": held at install beyond loopback, and new mappings written held. |
| L2 | "Late plans". |
| L3 | "Make my account" holds the routes already there. |
| L4 | "What the cookie lifts" and "Over plain HTTP". |
| L5 | "Rotate": the new value sealed as pending, both kept until the outcome is known. |
| L6 | "A password owner steps use", with the environment kind cached as ADR 0054 allows. |
