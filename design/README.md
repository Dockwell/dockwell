# Design

Dockwell's design system and screen mocks, imported from Claude Design on 3 Nov 2026. These copies are what we build from. Go back to Claude Design only for a large redesign, and re-import afterwards (see [Re-importing](#re-importing-from-claude-design)).

The product requirements live next door in [`docs/product/prd.md`](../docs/product/prd.md).

## What's here

| Path | What it is |
| --- | --- |
| [`system/README.md`](system/README.md) | The brand book. Start here: principles, how the product writes, colour, type, spacing, shape, motion, focus, layout, iconography and the logo. |
| [`system/tokens.json`](system/tokens.json) | Every token, in light and dark, each with a usage note. This is the source; everything else is generated from it. |
| [`system/tokens.css`](system/tokens.css) | CSS variables generated from `tokens.json`, plus the `@font-face` rules. Themes switch with `data-theme="light"` or `"dark"` on the root. |
| `system/components/<Name>/README.md` | Usage guidelines for each of the 23 components. `FleetOverview` is a whole example page built only from the system; the `Cover` folder holds only the system's cover. |
| [`system/components/index.d.ts`](system/components/index.d.ts) | Every component's props, as TypeScript types with doc comments. |
| `system/components/bundle.js`, `bundle.css` | The reference implementation: one classic script that assigns `window.Dockwell` and expects React 18 on `window`. Port from it; don't ship it. |
| `system/components/<Name>/preview.html` | Live examples of each component. They expect a host page that injects the tokens, the bundle and React, so they don't open on their own. |
| [`system/brand/`](system/brand/) | Logotype, mark and app icon as SVG, in light, dark and one-colour versions. |
| [`system/fonts/`](system/fonts/) | Host Grotesk and JetBrains Mono variable fonts, both under the SIL Open Font License (licence files alongside). |
| `system/design-system.json` | Claude Design's index for the system. Its paths are relative to the original artifact, so `project/README.md` means `system/README.md`, and the `components/lib/` React files were not copied (React comes from npm). |
| [`mocks/`](mocks/) | 84 artboards: 63 desktop screens, 15 phone screens, 4 shared shell pieces, a screen map and the logo sheet. |
| [`mocks/canvas.json`](mocks/canvas.json) | Claude Design's canvas index: each artboard's title, page (area) and size, and the page order. |

## Reading the mocks

Each `.dc.html` file is one screen, written in Claude Design's component format:

- Layout is plain HTML with inline styles that use the design-system variables (`var(--surface)`, `var(--space-4)` and so on).
- `<x-import component-from-global-scope="Dockwell.Button" …>` places a design-system component with its props. Across the mocks, the most used are `Icon` (73 screens), `Button` (54), `StatusBadge` (52), `Table` (34), `TextField` (28), `SegmentedControl` (19), `Metric` (17), `Tag` (16), `Timeline` (16), `Switch` (15) and `Glyph` (14). `Glyph` is the status shape used inside `StatusBadge`; it has no README of its own, and its props are in `index.d.ts`.
- `<dc-import>` pulls in the shared shell: `Rail` and `Topbar` on every desktop screen, and `MobileHeader` and `MobileTabBar` on phone screens.
- `{{…}}` holes are filled from `renderVals()` in the `<script type="text/x-dc">` block at the bottom of each file, which also holds the screen's sample data. `<sc-for>` and `<sc-if>` repeat and branch.
- Desktop screens are fluid pages drawn at 1440 px wide; phone screens are 390 × 844.
- Eight artboards default to the dark theme: `App-Logs`, `Incident`, `Server-Terminal`, `Sign-In`, `M-Incident`, `M-Logs`, `M-Overview-Fleet` and `M-Terminal`. The rest default to light.

The mocks don't open in a browser as they are. They load `./support.js`, which is Claude Design's runtime and not ours to redistribute, and `ds/dockwell/…`, which is this folder's `system/` (it was byte-identical at import, so it isn't duplicated). Use the local viewer in [`packages/mockview`](../packages/mockview/README.md) instead:

- `make mocks` serves it on http://127.0.0.1:5174. The index lists every artboard by canvas page, with its size. Open one to see it at its canvas size, shrunk to the window's width unless you pick "Actual size". Links inside a screen open the screen they point to, the arrow keys step through the canvas order, and Escape goes back to the list.
- The theme switch shows every artboard light, dark, or as drawn (the theme it defaults to). The shell pieces have no theme of their own and follow the switch.
- `make mock-snapshots` writes a PNG of every artboard, light and dark, to `packages/mockview/snapshots/`, which git ignores. `make mock-snapshots ONLY="Overview-Single M-Logs"` takes just those. It drives the system Chromium at `/usr/bin/chromium`; set `CHROMIUM_PATH` to use another Chrome or Chromium, as nothing is downloaded. It fails when an artboard logs an error or a warning, or doesn't render as written.
- The viewer reads the files here as they are and draws them with `system/components/bundle.js`, so a re-import shows up without a build step. `make check` renders every artboard in both themes and fails on any error.

The viewer is our own reading of the format, not Claude Design's runtime. Where it can't follow a file it says so in the artboard, in place of the part it couldn't draw.

Sample data in the mocks (acme.dev, hz-1, the Supabase project and so on) is illustrative. Use it for fixtures and copy, never as real configuration.

## Screens

Every screen, with the roadmap phase that builds it. Phase 1 is split into the milestones on GitHub (M1 to M4). A phone screen ships with its desktop counterpart, because mobile parity is a launch gate.

| Area | Screen | File | Phase | Note |
| --- | --- | --- | --- | --- |
| Map | Screen map | [`Main.dc.html`](mocks/Main.dc.html) | — | Index of every screen |
| Shell | Navigation rail | [`Rail.dc.html`](mocks/Rail.dc.html) | 1 · M1 |  |
| Shell | Top bar | [`Topbar.dc.html`](mocks/Topbar.dc.html) | 1 · M1 |  |
| Shell | Phone header | [`MobileHeader.dc.html`](mocks/MobileHeader.dc.html) | 1 · M1 |  |
| Shell | Phone tab bar | [`MobileTabBar.dc.html`](mocks/MobileTabBar.dc.html) | 1 · M1 |  |
| Brand | Logo | [`Logo.dc.html`](mocks/Logo.dc.html) | — | Brand reference |
| Get started | Claim this server | [`Onboarding-Claim.dc.html`](mocks/Onboarding-Claim.dc.html) | 1 · M1 |  |
| Get started | First deploy | [`Onboarding-FirstDeploy.dc.html`](mocks/Onboarding-FirstDeploy.dc.html) | 2 |  |
| Get started | It’s live | [`Onboarding-Live.dc.html`](mocks/Onboarding-Live.dc.html) | 2 | The 90-second gate |
| Get started | Sign in | [`Sign-In.dc.html`](mocks/Sign-In.dc.html) | 1 · M1 |  |
| Overview | Overview · one server | [`Overview-Single.dc.html`](mocks/Overview-Single.dc.html) | 1 · M1 |  |
| Overview | Overview · fleet | [`Overview-Fleet.dc.html`](mocks/Overview-Fleet.dc.html) | 2 |  |
| Apps | Apps | [`Apps-List.dc.html`](mocks/Apps-List.dc.html) | 1 · M3 | All seven of how an app can run |
| Apps | Apps, control plane unreachable | [`Apps-List-Unreachable.dc.html`](mocks/Apps-List-Unreachable.dc.html) | 1 · M3 | `Apps-List` with `reads="unknown"` |
| Apps | Apps, offline | [`Apps-List-Offline.dc.html`](mocks/Apps-List-Offline.dc.html) | 1 · M3 | `Apps-List` with `reads="offline"` |
| Apps | New app | [`Apps-New.dc.html`](mocks/Apps-New.dc.html) | 1 · M3 |  |
| Apps | App · overview | [`App-Overview.dc.html`](mocks/App-Overview.dc.html) | 1 · M3 |  |
| Apps | App · overview, can't read Docker | [`App-Overview-Docker.dc.html`](mocks/App-Overview-Docker.dc.html) | 1 · M3 | `App-Overview` with `reads="docker"` |
| Apps | App · deployments | [`App-Deployments.dc.html`](mocks/App-Deployments.dc.html) | 1 · M3 |  |
| Apps | App · logs | [`App-Logs.dc.html`](mocks/App-Logs.dc.html) | 1 · M3 |  |
| Apps | App · environment | [`App-Environment.dc.html`](mocks/App-Environment.dc.html) | 1 · M3 |  |
| Apps | App · settings | [`App-Settings.dc.html`](mocks/App-Settings.dc.html) | 2 | Build settings need Git deploys |
| Apps | App · previews | [`App-Previews.dc.html`](mocks/App-Previews.dc.html) | 3 |  |
| Websites and catalog | Websites | [`Websites-List.dc.html`](mocks/Websites-List.dc.html) | 2 |  |
| Websites and catalog | Website · configuration | [`Website-Detail.dc.html`](mocks/Website-Detail.dc.html) | 2 |  |
| Websites and catalog | Catalog | [`Catalog.dc.html`](mocks/Catalog.dc.html) | 2 |  |
| Websites and catalog | Install from catalog | [`Catalog-Install.dc.html`](mocks/Catalog-Install.dc.html) | 2 |  |
| Data | Databases | [`Databases-List.dc.html`](mocks/Databases-List.dc.html) | 2 |  |
| Data | Database · postgres | [`Database-Detail.dc.html`](mocks/Database-Detail.dc.html) | 2 | Point-in-time recovery is phase 4 |
| Data | Domains | [`Domains-List.dc.html`](mocks/Domains-List.dc.html) | 2 |  |
| Data | Add a domain | [`Domain-Add.dc.html`](mocks/Domain-Add.dc.html) | 2 |  |
| Data | Backups | [`Backups.dc.html`](mocks/Backups.dc.html) | 2 | Restore drills are phase 3 |
| Data | Restore | [`Backup-Restore.dc.html`](mocks/Backup-Restore.dc.html) | 2 |  |
| Connections | Connections | [`Connections-List.dc.html`](mocks/Connections-List.dc.html) | 2 |  |
| Connections | Add a connection | [`Connection-New.dc.html`](mocks/Connection-New.dc.html) | 2 | Credentials Dockwell creates are phase 3 |
| Connections | Connection · supabase | [`Connection-Detail.dc.html`](mocks/Connection-Detail.dc.html) | 2 |  |
| Connections | Use in an app | [`Connection-Bind.dc.html`](mocks/Connection-Bind.dc.html) | 2 |  |
| Server | Server · overview | [`Server-Overview.dc.html`](mocks/Server-Overview.dc.html) | 1 · M1 |  |
| Server | Server · files | [`Server-Files.dc.html`](mocks/Server-Files.dc.html) | 2 |  |
| Server | Server · terminal | [`Server-Terminal.dc.html`](mocks/Server-Terminal.dc.html) | 2 |  |
| Server | Server · firewall | [`Server-Firewall.dc.html`](mocks/Server-Firewall.dc.html) | 2 |  |
| Server | Server · processes | [`Server-Processes.dc.html`](mocks/Server-Processes.dc.html) | 3 |  |
| Server | Server · updates | [`Server-Updates.dc.html`](mocks/Server-Updates.dc.html) | 3 |  |
| Fleet | Add a server | [`Add-Server.dc.html`](mocks/Add-Server.dc.html) | 2 |  |
| Fleet | Second server joined | [`Second-Server.dc.html`](mocks/Second-Server.dc.html) | 2 |  |
| Fleet | Servers | [`Servers-List.dc.html`](mocks/Servers-List.dc.html) | 2 |  |
| Fleet | New fleet action | [`Fleet-Action.dc.html`](mocks/Fleet-Action.dc.html) | 3 |  |
| Fleet | Fleet action running | [`Fleet-Action-Running.dc.html`](mocks/Fleet-Action-Running.dc.html) | 3 |  |
| Fleet | Connect another Dockwell | [`Merge-Install.dc.html`](mocks/Merge-Install.dc.html) | 3 |  |
| Fleet routing | Domains · fleet | [`Domains-Fleet.dc.html`](mocks/Domains-Fleet.dc.html) | 3 |  |
| Fleet routing | Traffic path · acme.dev | [`Domain-Traffic.dc.html`](mocks/Domain-Traffic.dc.html) | 3 |  |
| Fleet routing | Ingress | [`Ingress.dc.html`](mocks/Ingress.dc.html) | 3 |  |
| Operate | Activity | [`Activity.dc.html`](mocks/Activity.dc.html) | 1 · M2 |  |
| Operate | Review plan | [`Plan-Review.dc.html`](mocks/Plan-Review.dc.html) | 1 · M2 |  |
| Operate | Incident | [`Incident.dc.html`](mocks/Incident.dc.html) | 2 | The assistant summary is phase 3 |
| Operate | Alerts | [`Alerts.dc.html`](mocks/Alerts.dc.html) | 2 |  |
| Operate | Notification channels | [`Alerts-Channels.dc.html`](mocks/Alerts-Channels.dc.html) | 2 |  |
| Operate | Scheduled tasks | [`Scheduled-Tasks.dc.html`](mocks/Scheduled-Tasks.dc.html) | 2 |  |
| Operate | Command bar | [`Global-CommandBar.dc.html`](mocks/Global-CommandBar.dc.html) | 1 · M2 | Navigation first; verbs later |
| Operate | Ops assistant | [`Global-Assistant.dc.html`](mocks/Global-Assistant.dc.html) | 3 |  |
| Phone | Phone · overview, one server | [`M-Overview-Single.dc.html`](mocks/M-Overview-Single.dc.html) | 1 · M1 |  |
| Phone | Phone · overview, fleet | [`M-Overview-Fleet.dc.html`](mocks/M-Overview-Fleet.dc.html) | 2 |  |
| Phone | Phone · apps | [`M-Apps.dc.html`](mocks/M-Apps.dc.html) | 1 · M3 |  |
| Phone | Phone · app | [`M-App.dc.html`](mocks/M-App.dc.html) | 1 · M3 |  |
| Phone | Phone · logs | [`M-Logs.dc.html`](mocks/M-Logs.dc.html) | 1 · M3 |  |
| Phone | Phone · servers | [`M-Servers.dc.html`](mocks/M-Servers.dc.html) | 2 |  |
| Phone | Phone · server | [`M-Server.dc.html`](mocks/M-Server.dc.html) | 2 |  |
| Phone | Phone · terminal | [`M-Terminal.dc.html`](mocks/M-Terminal.dc.html) | 2 |  |
| Phone | Phone · approve plan | [`M-Plan-Approve.dc.html`](mocks/M-Plan-Approve.dc.html) | 3 | Phone approvals; the plan sheet itself ships in phase 1 |
| Phone | Phone · incident | [`M-Incident.dc.html`](mocks/M-Incident.dc.html) | 2 |  |
| Phone | Phone · search | [`M-CommandBar.dc.html`](mocks/M-CommandBar.dc.html) | 1 · M2 |  |
| Phone | Phone · more | [`M-More.dc.html`](mocks/M-More.dc.html) | 1 · M1 |  |
| Phone | Phone · edit a file | [`M-Files.dc.html`](mocks/M-Files.dc.html) | 2 |  |
| Phone | Phone · connection | [`M-Connection.dc.html`](mocks/M-Connection.dc.html) | 2 |  |
| Phone | Phone · domain | [`M-Domain.dc.html`](mocks/M-Domain.dc.html) | 3 |  |
| Access and settings | Organization | [`Settings-General.dc.html`](mocks/Settings-General.dc.html) | 2 | Approvals are phase 4 |
| Access and settings | Members | [`Members.dc.html`](mocks/Members.dc.html) | 2 | SSO is phase 3 (OIDC) and 4 (SAML, SCIM) |
| Access and settings | API tokens | [`API-Tokens.dc.html`](mocks/API-Tokens.dc.html) | 2 |  |
| Access and settings | Audit log | [`Audit-Log.dc.html`](mocks/Audit-Log.dc.html) | 2 |  |
| Access and settings | Control plane | [`Settings-ControlPlane.dc.html`](mocks/Settings-ControlPlane.dc.html) | 2 | Replicas are phase 4 |
| Access and settings | Move control plane | [`Move-ControlPlane.dc.html`](mocks/Move-ControlPlane.dc.html) | 3 |  |
| Access and settings | Integrations | [`Settings-Integrations.dc.html`](mocks/Settings-Integrations.dc.html) | 2 |  |
| Access and settings | Billing | [`Billing.dc.html`](mocks/Billing.dc.html) | 4 |  |
| Access and settings | Profile and security | [`Profile.dc.html`](mocks/Profile.dc.html) | 2 |  |


## Re-importing from Claude Design

The sources, at the versions imported:

| Source | Artifact | Version imported |
| --- | --- | --- |
| Design system | https://claude.ai/artifact/Fo1eeuf9wFrKyKYzii4pTS | `1793740038-e31b` |
| Screen mocks ("Dockwell Screens") | https://claude.ai/artifact/FFbFfM9vyJLTn1M76HszxK | `1793740379-27f1` |
| PRD (Claude Doc) | https://claude.ai/artifact/T1zZptA6tzpk8ZZjJWdwWE | revision 62 |

After a redesign there, an agent with the Artifact tool re-imports like this:

1. List each artifact's files. Take only paths under `project/`; everything else belongs to Claude Design.
2. Design system: copy `project/**` into `design/system/`, except `project/components/lib/`.
3. Mocks: copy `project/*.dc.html` and `project/canvas.json` into `design/mocks/`. Check that `project/ds/dockwell/` still matches `design/system/` byte for byte, and say so if it doesn't.
4. Update the versions above, regenerate the screen table if screens were added or removed, and review the diff before committing. Anything edited in this folder since the last import has to be merged by hand.

The PRD no longer syncs: [`docs/product/prd.md`](../docs/product/prd.md) is the source of truth.
