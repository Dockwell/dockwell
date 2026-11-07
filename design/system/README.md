Dockwell is a control plane for servers: one VPS or a fleet of thousands, websites, apps and deploys, in one panel. Its interface should feel like a precise instrument: calm when everything is fine, unmistakable when something is not, and fast enough that nobody waits on it.

## Principles

1. **Ink for action, colour for state.** The primary action is `action` (ink). The four hues (`healthy`, `warning`, `critical`, `cobalt`) mean state and nothing else. Never use them to decorate, to brand a section, or to make something look important.
2. **Every state is a shape, a word and a colour.** Healthy is a filled circle, degraded a triangle, down a square, deploying a spinning arc, stopped a hollow ring, running a ring with a dot in it, unknown a dashed ring with a dash across it. Healthy means a check passed: what runs with nothing checking it reads as running, and what nobody can say anything of as unknown, and neither is ever green. Use `StatusBadge`; never show a coloured dot alone.
3. **Every change shows its plan.** Deploys, config edits, firewall rules and fleet actions open a `Sheet` with a `PlanDiff` before they run, and leave an undoable entry in the `Timeline`.
4. **Calm by default, deep on demand.** The first screen answers "is everything OK?" Healthy things stay quiet; raw config, terminals and logs are one keystroke away (`CommandBar`), never in the way.
5. **Machine values in mono, human words in sans.** IPs, hashes, image tags, paths, commands and selectors are `code` or `code-sm`; everything a person reads as language is Host Grotesk.
6. **Live, not polled.** Numbers update in place with tabular figures, streams follow until the reader scrolls, and nothing reloads the page.

## Content fundamentals

Write like a senior operator explaining something to a colleague: plain, specific, unhurried.

- **Sentence case everywhere**: buttons, headings, tabs, table headers. No all-caps labels, no title case.
- **Address the reader as "you"**; the product never says "I" or "we". The ops assistant speaks in plain sentences, never as a persona.
- **Verbs name the result**: "Deploy", "Apply plan", "Roll back", "Replace server". The same action keeps its name through the flow: the button "Roll back" produces the toast "Rolled back to r127" and the timeline entry "Rolled back web".
- **Lead with the fact, then the consequence, then the fix**: "ams-1-03 is down. Its 2 replicas moved to ams-1-04 and ams-1-05. Check the provider console, then reconnect or replace the server."
- **Errors say how to fix the problem** and never apologize: "api.acme.dev already routes to another project. Remove it there, or use a different subdomain."
- **Numbers carry units with a space**: "1.8 GB", "41 s", "11.2k /min", "3 min ago". Round to what a person can act on. Use relative time under a day, then dates ("12 Oct").
- **No emoji, no exclamation marks, no filler** ("Oops", "Just", "Simply", "Successfully").
- **Empty states invite action**: "No servers yet. Paste this command on any Linux server to add one." with the command in `code`.

## Visual foundations

### Colour

- Ground the app in `canvas`; put panels, tables and inputs on `surface`; inset wells (logs, code, segmented tracks) in `surface-sunken`; float menus, the command bar, sheets and toasts on `surface-raised` with a shadow token.
- Set text in `ink`; secondary text, labels and placeholders in `ink-muted` (both hold 4.5:1 on every surface token in both themes). Use `ink-faint` only for disabled text.
- Divide with `line` hairlines. Give inputs and other controls whose edge matters a `line-strong` border (3:1 on every surface).
- Fill the one primary button with `action` and its label with `on-action`. In dark mode `action` turns light and `on-action` dark; never hard-code white text on a fill.
- Use `cobalt` for work in flight (deploying, building, applying, live streams), links, focus and selection. `cobalt-soft` is its ground.
- Use `healthy`, `warning` and `critical` only for state, as text on any surface or on their `-soft` grounds. `critical` also fills the danger button, with `on-critical` text.
- Use `running` for what runs with nothing checking it, `unknown` for what nobody can say anything of, and `idle` for what is stopped or paused. All three are `ink-muted`, never green and never ink, so they tell apart by their glyphs: a ring with a dot in it, a dashed ring with a dash across it, and a hollow ring, each unlike the others at 10 px.
- Show plans and diffs with `diff-add`, `diff-change` and `diff-remove` (aliases of the status hues) on their `-soft` grounds, and strike through removed values so the diff reads in grayscale.
- Healthy tiles in `FleetGrid` stay a light tint; only degraded, down and deploying get full colour. A calm grid means a calm fleet.

### Type

- Set every human word in Host Grotesk (`--font-sans`): `title` once per page, `heading` for panels, `body` (14/20) by default, `label` for controls, `caption` for metadata and table headers, `metric` for live numbers.
- Host Grotesk's default figures are tabular, so updating numbers never shift. Do not switch on proportional figures.
- Set machine values in JetBrains Mono (`--font-mono`): `code` (13/20) inline and in inputs, `code-sm` (12/18) in tags, table cells and logs.
- Chinese text falls back to PingFang SC, Microsoft YaHei or Noto Sans SC through the same stack; keep sizes and weights unchanged.
- Use weight for hierarchy (400, 500, 550, 600). Never use italics in the interface, and never letter-space body text.

### Spacing and density

- Build on the 4px grid: `space-2` inside controls, `space-3` for cell and control padding, `space-4` inside panels, `space-6` between panels, `space-8` between page sections.
- Controls are `control-md` (32px) by default, `control-sm` (28px) in toolbars and table rows, `control-lg` (40px) only in onboarding and empty states.
- Table rows are 48px comfortable or 36px compact. The rail is `rail-width` (232px), the top bar `scopebar-height` (52px), a sheet `sheet-width` (560px).

### Shape and elevation

- Radius grows with the size of the thing: `radius-xs` tiles, tags and key caps; `radius-sm` controls; `radius-md` panels, tables, callouts and toasts; `radius-lg` sheets and the command bar; `radius-full` pills and switches.
- Keep nested corners concentric: outer radius equals inner radius plus padding (the segmented control is `radius-sm` + 3px around `radius-sm` items).
- Resting panels have a `line` border and no shadow. Only floating layers get `shadow-float` (menus, toasts, the selected segment) or `shadow-sheet` (sheets, command bar). Dark-mode shadows include a faint light edge so layers stay separable.
- Sheets float 8px from the viewport edges rather than docking flush.

### Motion

- Motion answers an action or shows work in progress; nothing moves on its own for decoration.
- Use 120 ms for hover and press, 180 ms for toggles and the command bar, 260 ms for sheets and toasts, all on `cubic-bezier(.2, .8, .2, 1)`.
- Only three things loop: the deploying arc, the live-stream dot and the toast timer. All three stop under `prefers-reduced-motion`, and every other transition becomes instant.

### Focus and states

- Every interactive element shows `focus-ring` on keyboard focus: a 2px surface gap, then 2px of solid `cobalt`, at least 3:1 on every surface.
- Text inputs focus with a `cobalt` border and a soft cobalt halo; invalid inputs switch both to `critical`.
- Hover lightens toward `surface-sunken` (ghost controls, menu rows) or `canvas` (table rows). Selected rows use `selection`.
- Disabled controls drop to 45% opacity and say why nearby.

### Layout

- Desktop: the rail on the left, the top bar holding `ScopeBar`, the command entry and the primary action, then the page on `canvas` with a `space-6` gutter.
- A page leads with its `title` and one line of context in `ink-muted`, then at most one `Callout`, then up to four `Metric` panels, then the working panels.
- Below 960px the rail collapses to icons; below 640px it becomes a bottom sheet and tables turn into stacked rows. Every view works from 360px up.

#### Page widths

The mocks are drawn at 1440px, where every page fills the room right of the rail (the `max-width: 1360px` they give every page hardly shows there). Past 1440px a page grows only to the width its kind reads well at, as this table sets, and sits centred in that room, so it is balanced on any screen. Pick the kind by what the page is for, never by how much it holds today:

| Kind | Token | For | Past 1440px |
| --- | --- | --- | --- |
| Wide | `page-wide` (1680px) | Lists, tables and dashboards: Overview, Apps, Servers, Websites, Databases, Domains, Backups, Activity, the catalog, a server's pages | Grids gain columns rather than stretch what's there |
| Standard | `page-standard` (1360px) | Detail pages: an app's, a website's, a database's, a change's. The default | Columns share the width by weight, so a main column and its aside grow together |
| Narrow | `page-narrow` (1040px) | Forms and settings: Profile, Settings, API tokens, a form that is a page | Stops at its width, so fields and their notes stay a comfortable line long |
| Full | none | A log view and the terminal, where every column of a line helps | Edge to edge, with the usual gutter |

- At 1440px and below each kind fills the room it has, exactly as the mocks draw it, and phones don't change.
- A grid in a page that grows, such as the catalog's cards, uses `repeat(auto-fill, minmax(<floor>, 1fr))`, so it gains a column each time there's room for one, and a row of panels uses `auto-fit` with a floor, so a dashboard takes a third or fourth column where it has the width.
- The top bar spans the column, and its contents line up with the page's edges below: the scope above the title, the last control above the page's right edge.
- A sheet keeps `sheet-width` whatever the page's kind.
- In the panel, `Page` takes the kind as `width` (`"wide"`, `"standard"`, `"narrow"` or `"full"`). A screen never sets a max-width of its own.

## Iconography

- Use the `Icon` component: line icons on a 24-unit grid, 1.5 stroke, round caps and joins, drawn in `currentColor`. 16px in controls and lists, 14px in small buttons and tags, 20px in empty states.
- The bundled set covers the product's core nouns and verbs (server, service, globe, database, terminal, deploy, undo, lock and more). For anything missing, draw on the same grid and stroke; Lucide matches this construction closely and is the suggested source. The bundled icons are new drawings, not copies of any library.
- Status is never an icon. The status glyphs belong to `StatusBadge`, `FleetGrid`, `LogStream`, `Callout` and `Toast` and keep their meaning everywhere.
- No emoji in the interface.

## Identity

- The logo is the name in Host Grotesk 650 with a geometric D whose counter holds still water: the D is the dock, the cobalt below its waterline is the well. Use the `Logo` component, never typed text: `logotype` wherever the product names itself (14 px in the rail and top bars), `mark` for tight square spaces, `icon` for favicons and app stores.
- The water is always `cobalt` and always level; it is never a status colour. One-colour versions are for print and coloured fills. SVG files live in `brand/`.
- The cover's tile pattern comes from `FleetGrid`: the fleet at a glance is the product's signature image.

## Using this system

- Load `tokens.css`, then `components/bundle.css`, then React 18 and `components/bundle.js`; components are on `window.Dockwell`.
- Every token switches with `data-theme="light"` or `data-theme="dark"` on the root. Light is the first theme and the fallback.
- Compose screens from the components first; the Fleet overview page shows a full screen built only from this system.
