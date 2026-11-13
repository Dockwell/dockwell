# 0081. An app's own values shown, its secrets kept

- Status: Accepted. It changes what [0022](0022-secrets-encrypted-at-rest.md) says a read, a plan and an export do with a container's own environment: a value its service says holds no secret shows. Everything else there stands.
- Date: 2026-11-12

## Context

On 2026-11-12 the owner installed WordPress from the catalog and opened its Environment tab: the database's host, name and user showed as dots, though none is a secret, and none of them was on the project's Variables and secrets page (#565).

What's there:

- **A catalog install keeps secrets as secrets.** `catalog.Fill` gives every secret input, and every password Dockwell generates, a stand-in; `composeapi.useSecrets` swaps each for a reference to a `secret` of the environment, sealed, which a passkey reveals on Variables ([ADR 0022](0022-secrets-encrypted-at-rest.md)). `standsAlone` refuses a secret inside a longer value, so none is left in a container's own environment.
- **Everything else lands in `Container.env`:** the entry's own text (WordPress's `WORDPRESS_DB_HOST: db`) and the values of its inputs that aren't secrets (a port, a site's address).
- **`Container.env` is `(sensitive)`.** A read blanks every value (`services.Blanked`), a plan masks every change, an export leaves every value out. ADR 0022 chose this: a Compose file a person wrote can put a password there, and telling a port from a password by its name or shape is a guess that shows a password when it's wrong.
- **Variables and secrets lists only the environment's `variable` and `secret` resources.** A service's own environment isn't one.

So the dots aren't a bug in the panel: no response carries the values. And for a catalog install, nobody had to guess: the entry's file and its inputs say exactly which values came from where.

## Decision

### A service says which of its own values show

- **`Service.shown_env`** (180) names the values of its app's own environment that hold no secret. A read (`GetService`, `ListServices`), a plan and an export show those as they are. Every other value stays as ADR 0022 has it: blanked, masked, left out.
- **Only the control plane keeps it** (`resource.ControlOnly`). A server never sees it, so changing it alone restarts nothing, drift never reads it as missing, and no container label or hash carries it. Drift kept as the latest revision keeps a name shown only while the server has the value it showed: one changed there by hand could be anything.
- **It names only what the environment has,** sorted and once each, as `Validate` leaves it.

### Who decides, and how a value comes to show

- **A catalog install shows what can't be a secret.** After its secrets are references, every value left in a service's own environment came from the entry's public text or an input that isn't a secret. Each shows, unless its name or its shape says it could be a secret all the same (`catalog.MaySecret`): a word such as PASSWORD, PASS, PWD, SECRET, TOKEN, KEY, AUTH, SALT, DSN or WEBHOOK in its name, or a URL with a user or password in it, a PEM block, a key prefix a service issues (`sk-`, `ghp_`, `AKIA` and kin), a JWT, or a long run of mixed letters and digits. It errs towards hidden: a value it hides wrongly costs a look, and one it shows wrongly is a password for everyone who can read the app.
- **An upgrade keeps that.** A merged value shows where it's what the side it came from showed: what runs, or the new release's file, read the same way.
- **A person decides for what they type.** On an app's Environment tab, a new value is the app's own and shows, unless they switch Secret on, which makes it a secret of the environment the app uses. Changing one of the app's own values asks the same: a hidden one starts as a secret, as it may be one.
- **A person's own Compose file shows nothing.** What they wrote in it could be anything, so an import keeps ADR 0022's rule. A value there shows once someone gives it again as not secret.
- **Showing a value takes giving it.** An edit that names a value in `shown_env` that wasn't there must set it: one it keeps without reading it (`keep_env`) is refused. So nobody shows a value they never saw by flipping a flag, and a plan where a value goes from hidden to shown, or back, masks it on both sides.

### Installs from before, once

- **Dockwell shows what's provably plain, once, as its own change.** For each catalog install whose pin keeps its file, it reads the file again with the values the pin kept, which hold no secret, and a value of no meaning for each secret. Each of a service's own values that is still exactly what that gives, and that `MaySecret` doesn't flag, shows. A value a person changed since stays hidden, as does every app not installed from a catalog, and an install from before Dockwell kept its file.
- **It waits for the servers.** It runs a minute after the control plane starts, and again every 10 minutes while a server it needs isn't connected, as a change can't apply on one that isn't. A setting records that it ran, so it never runs again.
- **It's a change like any other:** proposed and applied by Dockwell, on Activity, with a note naming each app and value it shows, and undoable. Should its plan do anything on a server, as one that drifted would, it's discarded and each app goes on its own: those that plan nothing apply, and the rest stay hidden.

### One place for what an app runs with

- **Variables and secrets lists each app's own values beside the shared ones,** from the same reads as the app's Environment tab, so the two agree on every row: Source says shared or set on the app, Used by names each app as a link, and a filter shows what one app runs with. An app's tab links there, filtered to it.
- **Make shared** turns one of an app's own values that shows into a variable of its environment the app uses, in one plan; one already there with the same value is used as it is. One there with another value, or a secret of that name, is said, and nothing is planned.
- **A secret is always a secret of the environment,** sealed, revealed with a passkey and audited as ADR 0022 has it, whichever page made it.

## What we turned down

- **Unsealing `Container.env`.** Every Compose file a person imports would put its passwords in plans, history and exports.
- **Moving plain values into the environment's variables.** Two apps that set `DB_HOST` differently would collide, the environment would fill with one app's settings, and moving existing installs would change every container's `dockwell.uses` label, so each would restart.
- **Deciding at each read from the pin.** It would read the entry's file again on every list, and keep the answer in two places: the pin, and whatever an edit later said.
- **Guessing for a person's own file.** By name and shape alone, a password called `FOO` shows. Only a source that says it holds no secret, a catalog entry, gets the guess, and even then only to hide more.
- **A reveal for a hidden own value.** A value that may be a secret becomes one in a step, by giving it again with Secret on; there it's sealed, revealed with a passkey and rotated like any other. A second way to reveal would be a second secret store.

## Consequences

- A catalog install's database host, name and user read as themselves on the app's tab, on Variables and secrets, in its plans and in an export.
- What a token that can read an app sees grows by those values. They were in the entry's public file, or typed into an input the entry says isn't secret.
- A plan names the values it changes where both sides show them, as it does for a variable.
- `dockwell services update` sets the whole spec, as before: a value that reads back hidden reaches it empty, and one that shows round-trips.
