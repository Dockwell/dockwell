# 0042. The install's settings

- Status: Accepted
- Date: 2026-11-07

## Context

The rail's Settings has opened "Not in this release yet" since M1. #223 asks for Settings as `Settings-General.dc.html` and five more mocks have it, starting with General: what the install is called, the panel's domain and how it's served, a time zone, and a language.

PRD principle 2 says every change is a revision with a plan, and names two exceptions: a restart, which changes no spec, and a server's public address, which changes nothing on a server, only what Dockwell says of it ([ADR 0034](0034-a-servers-public-address.md)). The mock's General also holds projects, environments and their protection, which are managed state already and stay revisions; #223 leaves them for later.

## Decision

### Settings that aren't managed state

An install's **name** and **time zone** are settings, kept in the store's `settings` table, like the public address:

- They change what Dockwell says and suggests, not what runs: the name is what the sign-in screen and the browser's tabs say, and the time zone is what a new schedule, such as a backup's, starts in. A schedule keeps the zone it was made with, in its own spec, so changing the setting moves none.
- So a change needs no plan, and has nothing to undo but setting it back. Each goes on the activity log as an event of a new kind, `EVENT_KIND_SETTINGS`, with the value and the one it replaced, once: setting what's already there records nothing.
- `SettingsService.SetInstallName` and `SetTimeZone` are `EFFECT_APPLY`, so a token that can make changes can set them, as for the public address. A name holds 64 characters at most, on one line. A time zone is one the IANA names, as the backup schedules take; `Local` and paths aren't.
- The organization's own row, named `default`, stays as it is: its name is a scope's, which nothing shows, and the mock's slug and logo wait for organizations to mean something.

### What comes from how Dockwell was started

How the panel is served is `dockwell run`'s, set by its flags or environment where it runs (`DOCKWELL_DOMAIN`), and changed there with a restart, as `docs/install.md` says. `GetSettings` reports it as the running control plane has it: the loopback address it always answers on, its domain if any, and what serves that domain over HTTPS: Dockwell itself, with a certificate from Let's Encrypt, or the server's Caddy once the server has a route ([ADR 0024](0024-the-caddy-ingress-driver.md)). The panel says how to change it, rather than offering a field that would only take effect after a restart nobody asked for.

### A language for this browser

The panel picks its language from the browser's. Settings can choose one for this browser, kept in its local storage: it's how this browser shows the panel, not something of the install's, and another browser, or the command line, keeps its own. Choosing the browser's again clears it.

## Consequences

- PRD principle 2 names the install's settings as a third exception.
- Settings has a layout of its own, with its sections beside the open one on wide screens and as a list on a phone. Profile and security moves into it, at the same `/profile`. Each later section of #223 adds itself there.
- Activity has settings events under All; its kind filter, which holds five kinds already, doesn't grow a sixth.
- Backup schedules made in the panel start in the install's time zone once someone has set one, and in the browser's until then, as before, and the schedule's form says which.
