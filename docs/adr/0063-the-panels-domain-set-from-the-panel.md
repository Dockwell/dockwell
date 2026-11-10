# 0063. The panel's domain, set from the panel

- Status: Proposed. Amends [ADR 0042](0042-the-installs-settings.md), where it says how the panel is served is only `dockwell run`'s.
- Date: 2026-11-10

## Context

A new install answers on loopback alone, through an SSH tunnel ([ADR 0011](0011-claim-and-sign-in.md)). To reach it over HTTPS on a domain, Settings → General says, in a list of steps: point the domain's DNS at the server, then `sudo systemctl edit dockwell` to add `Environment=DOCKWELL_DOMAIN=…` (or `/etc/conf.d/dockwell` with OpenRC, or `--domain` by hand), restart, check that ports 80 and 443 reach the server, then run `dockwell recover` over SSH and open its link on the new domain to make a passkey there. The claim page says much of the same under the panel's address.

ADR 0042 chose words over a field because the domain is read as `dockwell run` starts, so a field "would only take effect after a restart nobody asked for". Issue #426 holds every screen to "the interface does the work": a sentence that tells a person to do something by hand is a missing feature. 1Panel sets its panel's domain, port and HTTPS from its own settings.

What the domain shapes is all read at start: the relying party ID passkeys are made for, so a passkey belongs to the address it was made on ([ADR 0011](0011-claim-and-sign-in.md)); the HTTPS and HTTP listeners, or the hand-over to the server's Caddy when it has routes ([ADR 0024](0024-the-caddy-ingress-driver.md)); the certificate; the hosts every request is checked against; the claim link; and the name no route may take (`refuseClaimedDomain`, which stops `dockwell run` when a route already serves the name). Making each of those follow a change while the process runs would touch most of the control plane's start-up for a change made once or twice in an install's life.

Two moves need designing, not one: from loopback alone to a domain, where the person comes through the SSH tunnel and every passkey is the tunnel's; and from one domain to another, where people sign in on the first with its passkeys, most have no SSH, and the first stops answering when the second starts.

## Decision

**The panel's domain is a setting a person sets from Settings → General,** kept in the store as the install's name and time zone are, with an activity event that says what it replaced. It isn't managed state: it changes how the panel is reached, not what runs on a server (ADR 0042's exception, widened).

**Where `dockwell run` was given one, that one wins.** `--domain` or `DOCKWELL_DOMAIN` is read first, as today; the panel then shows the domain read-only, with where it's set. Without either, `dockwell run` reads the setting as it starts. Nothing an operator set on the command line or in the service's environment is overridden from the panel.

**Setting it is a restart the person asks for.** Saving says what will happen ("Dockwell restarts to serve panel.example.com. Apps and websites keep running.") and, once confirmed, the control plane shuts down cleanly and starts again from the same binary with the same arguments and environment: it re-executes itself, so the same happens under systemd, OpenRC and a `dockwell run` started by hand. The agent in the same process reconnects as it does after an upgrade. The objection in ADR 0042 was to a restart nobody asked for; this one is asked for with a button.

- **The answer goes out first.** The restart begins once the response to `SetPanelDomain` is written and flushed, so the panel knows the save took before its connection drops, and waits for the control plane to answer again.
- **It never cuts a step.** Saving is refused while a change is applying ("A change is applying. Save once it ends."), so the restart doesn't stop one partway.

**It's checked before it's saved, as the person types.** `SettingsService.CheckPanelDomain` answers, for an admin, as it has the control plane resolve the names typed:
- whether it's a name the panel can take: the rules `--domain` has, and no route serves it or a name under it (as `refuseClaimedDomain` refuses at start);
- what its A and AAAA records are, and whether they lead to this server's public addresses ([ADR 0034](0034-a-servers-public-address.md)), shown with the DNS record the websites track builds for #426 ("DNS doesn't point here yet · Check again");
- whether something other than Dockwell or its Caddy holds ports 80 or 443 on the control server, which would stop a certificate.

The save is refused while the name is invalid or taken by a route. DNS that doesn't lead here yet is a warning with the record to make, not a refusal: a person may set DNS and the domain in either order, and a certificate is tried again as it is today.

**`SettingsService.SetPanelDomain` sets or clears it.** It needs the owner, a signed-in session and a fresh passkey (a step-up, `STEP_UP_ACTION_SET_PANEL_DOMAIN`): the domain decides where everyone signs in. Owner is a check in the handler, as for the install's other owner-only acts, since the roles table's `min_role` stops at admin. It's refused when `dockwell run` was given a domain. Clearing it goes back to loopback alone.

**From the moment it's saved, no route can take it.** The domain waiting for the restart is kept beside the one in use, and a route's plan is refused for either, or a name under either, as for the panel's domain today. So nothing can claim it between the save and the restart.

**The person who set it is handed the way in.** After the restart, the person's passkeys for the old address don't sign in on the new one. The panel waits for the domain to answer over HTTPS ("Getting a certificate for panel.example.com…"), then offers **Open panel.example.com**, which mints a one-time sign-in link to the new address for that person alone, as `dockwell recover` prints one (15 minutes, once), with a fresh passkey step-up, and lands on Profile and security with **Add a passkey here** (#445). The link is minted then, not at the save, so a certificate that takes longer than 15 minutes, as DNS and ACME's back-off can, doesn't leave it expired. Like `dockwell recover`'s, it carries its token in the fragment (`/recover#…`), so no server log or Referer holds it, and the page clears it from the address bar once used; Activity's `sign_in_link.create` names the link, never the token.

**From loopback to a domain,** the tunnel keeps working throughout, with the passkeys made there, and the person who set the domain is on it: that is the whole move.

**From one domain to another, the old one keeps answering.** Moving from A to B restarts the control plane on both names: B is the panel's domain, and A answers beside it, with its certificate, for a grace period. So:
- the page the person saved on, on A, keeps polling "Getting a certificate for B…", and when B fails, **Check again** and **Stop using it** are there on A, not only through the tunnel;
- everyone else keeps signing in on A with A's passkeys, which are bound to A's relying party ID, and each finds **This panel is moving to B** with **Move my sign-in to B**: a one-time link to B minted for them, after a fresh step-up on A, which lands on B's **Add a passkey here**;
- the confirm before the save names how many people have passkeys only for A, and Settings → Members shows who hasn't moved yet, with **Send a sign-in link** for each, for the owner to pass on, as an invite;
- A also serves `/.well-known/webauthn` listing B (WebAuthn's related origin requests), so in a browser that supports them, A's passkeys sign in on B directly.

The grace period ends 7 days after B has answered over HTTPS and its owner has signed in there, or when the owner ends it early in Settings → General; then A answers 421 as any other name does. Loopback and the tunnel answer throughout. Moving back to a domain set before (A to B to A) brings A's passkeys back, as they're bound to A's ID.

**A domain from the setting never stops `dockwell run`.** Only a domain given on its command line or in its environment does, as today, since the operator who typed it is there to read why. When the setting is refused as `dockwell run` starts, as when a later release's rules refuse the name, `dockwell run` starts on loopback alone, with the tunnel, and the agent in the same process, logs why, and the panel shows the refusal on Settings → General with **Stop using it**. When the domain doesn't get a certificate, the control plane answers on loopback, and on the old domain during a move, as it does today with a domain whose certificate fails, and the panel shows the domain's state (no certificate yet, and why) with **Check again** and **Stop using it**.

**The way back is `dockwell panel-domain --clear`,** run on the control server with local access to the data directory, as `dockwell claim` and `dockwell recover` have, since a token can't call `SetPanelDomain`. It opens the store as they do, SQLite in the data directory or Postgres from the service's own environment (`dockwell.env`, `/etc/conf.d/dockwell`), so it works while `dockwell run` is stopped or failing to start. It clears the setting, goes on Activity, and restarts the service when it runs as root under systemd or OpenRC, or says the command that does. `dockwell recover` and the claim page name it beside themselves.

**Behind Caddy, the agent moves the panel's site as it starts.** On a control server with routes, Caddy serves the panel's domain (ADR 0024). As `dockwell run` starts with a new domain, its agent writes Caddy's panel site for it, and for the old one during a move, outside a plan, as it does today when `DOCKWELL_DOMAIN` changes between starts. That's no change to managed state: the routes are as they were, and the panel's site follows the setting, whose save the owner confirmed with a passkey and Activity holds.

## Consequences

- Settings → General's Panel address becomes a field with the checks beside it, Save and the restart it says, in place of the list of steps; the claim page says in a line that a domain can come later, under Settings (#440).
- `dockwell run` reads one more setting at start, after its flags and environment, and never stops for it. `docs/install.md` adds the panel as the first way to set the domain, and `dockwell panel-domain --clear` as the way back.
- Two methods (`CheckPanelDomain`, admin, and `SetPanelDomain`, owner) and a step-up action; `methods.ts` lists both, and the panel asks `may` for them (ADR 0059). One local command, `dockwell panel-domain`.
- For a move, the control plane serves two names for up to a week after the new one works, with a certificate for each, and passkey ceremonies take their relying party ID from the name the request came to. Every member gets a way to B without SSH.
- A restart the panel asks for exists for the first time. Later settings that are read at start, such as the ACME directory, can use it the same way; until one does, they stay `dockwell run`'s.
- Without the domain set from the panel, nothing changes: an install that sets `DOCKWELL_DOMAIN` keeps working as it does.
