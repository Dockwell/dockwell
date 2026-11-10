# 0063. The panel's domain, set from the panel

- Status: Proposed. Amends [ADR 0042](0042-the-installs-settings.md), where it says how the panel is served is only `dockwell run`'s.
- Date: 2026-11-10

## Context

A new install answers on loopback alone, through an SSH tunnel ([ADR 0011](0011-claim-and-sign-in.md)). To reach it over HTTPS on a domain, Settings → General says, in a list of steps: point the domain's DNS at the server, then `sudo systemctl edit dockwell` to add `Environment=DOCKWELL_DOMAIN=…` (or `/etc/conf.d/dockwell` with OpenRC, or `--domain` by hand), restart, check that ports 80 and 443 reach the server, then run `dockwell recover` over SSH and open its link on the new domain to make a passkey there. The claim page says much of the same under the panel's address.

ADR 0042 chose words over a field because the domain is read as `dockwell run` starts, so a field "would only take effect after a restart nobody asked for". Issue #426 holds every screen to "the interface does the work": a sentence that tells a person to do something by hand is a missing feature. 1Panel sets its panel's domain, port and HTTPS from its own settings.

What the domain shapes is all read at start: the relying party ID passkeys are made for, the HTTPS and HTTP listeners or the hand-over to the server's Caddy when it has routes ([ADR 0024](0024-the-caddy-ingress-driver.md)), the certificate, the hosts every request is checked against, the claim link, and the name no route may take. Making each of those follow a change while the process runs would touch most of the control plane's start-up for a change made once or twice in an install's life.

## Decision

**The panel's domain is a setting a person sets from Settings → General,** kept in the store as the install's name and time zone are, with an activity event that says what it replaced. It isn't managed state: it changes how the panel is reached, not what runs on a server (ADR 0042's exception, widened).

**Where `dockwell run` was given one, that one wins.** `--domain` or `DOCKWELL_DOMAIN` is read first, as today; the panel then shows the domain read-only, with where it's set. Without either, `dockwell run` reads the setting as it starts. Nothing an operator set on the command line or in the service's environment is overridden from the panel.

**Setting it is a restart the person asks for.** Saving says what will happen ("Dockwell restarts to serve panel.example.com. Apps and websites keep running.") and, once confirmed, the control plane shuts down cleanly and starts again from the same binary with the same arguments and environment: it re-executes itself, so the same happens under systemd, OpenRC and a `dockwell run` started by hand. The agent in the same process reconnects as it does after an upgrade. The objection in ADR 0042 was to a restart nobody asked for; this one is asked for with a button.

**It's checked before it's saved, as the person types.** `SettingsService.CheckPanelDomain` (a read) answers:
- whether it's a name the panel can take: the rules `--domain` has, and no route serves it or a name under it (as `refuseClaimedDomain` refuses at start);
- what its A and AAAA records are, and whether they lead to this server's public addresses ([ADR 0034](0034-a-servers-public-address.md)), shown with the DNS record the websites track builds for #426 ("DNS doesn't point here yet · Check again");
- whether something other than Dockwell or its Caddy holds ports 80 or 443 on the control server, which would stop a certificate.

The save is refused while the name is invalid or taken by a route. DNS that doesn't lead here yet is a warning with the record to make, not a refusal: a person may set DNS and the domain in either order, and a certificate is tried again as it is today.

**`SettingsService.SetPanelDomain` sets or clears it.** It needs the owner, a signed-in session and a fresh passkey (a step-up, `STEP_UP_ACTION_SET_PANEL_DOMAIN`): the domain decides where everyone signs in. It's refused when `dockwell run` was given a domain. Clearing it goes back to loopback alone.

**The person who set it is handed the way in.** A passkey belongs to the address it was made on, so after the restart the person's passkeys for `localhost` don't sign in on the domain. The response carries a one-time sign-in link to the new address, for that person alone, as `dockwell recover` prints one (15 minutes, once, audited as `sign_in_link.create`). The panel waits for the domain to answer over HTTPS ("Getting a certificate for panel.example.com…"), then offers **Open panel.example.com**, which signs in with the link and lands on Profile and security with **Add a passkey here** (#445). The tunnel keeps working throughout, with the passkeys made there.

**When it doesn't come up,** the control plane still answers on loopback, as it does today with a domain whose certificate fails; the panel, reached through the tunnel, shows the domain's state (no certificate yet, and why) with **Check again** and **Stop using it**. A command run on the control server clears it, with local access to the data directory as `dockwell claim` and `dockwell recover` have, since a token can't call `SetPanelDomain`.

## Consequences

- Settings → General's Panel address becomes a field with the checks beside it, Save and the restart it says, in place of the list of steps; the claim page says in a line that a domain can come later, under Settings (#440).
- `dockwell run` reads one more setting at start, after its flags and environment. `docs/install.md` adds the panel as the first way to set the domain.
- Two methods (`CheckPanelDomain`, `SetPanelDomain`) and a step-up action; `methods.ts` lists both, and the panel asks `may` for the second (ADR 0059).
- A restart the panel asks for exists for the first time. Later settings that are read at start, such as the ACME directory, can use it the same way; until one does, they stay `dockwell run`'s.
- Without the domain set from the panel, nothing changes: an install that sets `DOCKWELL_DOMAIN` keeps working as it does.
