# Mock viewer

A local viewer for the screen mocks in [`design/mocks`](../../design/mocks/), and a script that writes a PNG of each one. It's a development tool; nothing here ships in the binary.

## Use it

```sh
make mocks                                   # the viewer on http://127.0.0.1:5174
make mock-snapshots                          # every artboard, light and dark, into packages/mockview/snapshots/
make mock-snapshots ONLY="Overview-Single"   # just the artboards named, or whose names contain the text
```

The snapshot script starts the viewer on a free port and drives the system Chromium at `/usr/bin/chromium` through `playwright-core`. Set `CHROMIUM_PATH` to use another Chrome or Chromium; no browser is downloaded. It also takes `--theme light|dark`, `--out <dir>` and `--jobs <n>` when you run it with `pnpm --filter @dockwell/mockview snapshots -- …`. It fails when an artboard logs an error or a warning, throws, or reports a problem. Git ignores the PNGs.

`make check` runs `src/mocks.test.tsx`, which renders all 84 artboards in both themes in jsdom and fails on any problem, error or warning.

## How it reads the format

The mocks are Claude Design artboards. This package is our own reading of the format, written from the files and Claude Design's format notes; it doesn't use or copy Claude Design's runtime (`support.js`).

| In the file | What the viewer does |
| --- | --- |
| `<x-dc>…</x-dc>` | The artboard's markup, parsed by `src/format/html.ts`, which keeps it exactly as written. The browser's own parser would move `<sc-for>` out of tables and drop it inside `<select>`. |
| `<helmet><style>` | Goes to the document head, once per artboard however often it's imported. |
| `{{path}}`, `{{16}}`, `{{true}}` | A dotted lookup into what `renderVals()` returns and the `<sc-for>` variables, including `$index`, or a literal. Anything else is reported. A whole-attribute hole passes the raw value; mixed text becomes a string. |
| `<sc-for list as>`, `<sc-if value>` | Repeat and branch. The `hint-*` attributes only matter while Claude Design streams a file, so they're ignored. |
| `<x-import component-from-global-scope="Dockwell.Button" …>` | The component from `window.Dockwell`, which [`design/system/components/bundle.js`](../../design/system/components/bundle.js) defines. Attribute names go from kebab-case to camelCase (`on-row-click` → `onRowClick`), `aria-*` and `data-*` stay as they are, and `dc-props="{{object}}"` spreads an object of props. Children follow JSX whitespace rules. |
| `<dc-import name="Rail" active="apps">` | The other artboard, rendered in place with no wrapper element, with its `data-props` defaults under the given attributes. An artboard that imports itself is stopped and reported. |
| `class Component extends DCLogic` | Evaluated as is. `DCLogic` is a React class component, so `this.props`, `state`, `setState` and lifecycle methods work. The `React.createElement` that scripts see adds index keys to static lists, which the mocks build without keys. |
| `data-props` | Defaults for the artboard's props. The theme switch overrides `theme`, and also sets `data-theme` on the page, so the shell pieces, which have no theme prop, follow it. |
| `<a href="Apps-List.dc.html">` | Opens that artboard in the viewer. Links to other sites are sample data and go nowhere. |

React 19 has no UMD build, so `src/runtime/design-system.ts` puts the npm `react` and `react-dom` on `window` before it runs the bundle.

Anything the viewer can't draw is reported to the console and shown in the artboard, in place of the part that failed.

## Layout

| Path | What it holds |
| --- | --- |
| `src/format/` | The parser and the renderer. Pure TypeScript and React, unit-tested. |
| `src/frame/`, `frame.html` | One artboard on its own page: `frame.html?board=Rail.dc.html&theme=dark`. It loads only the design system's reference files. |
| `src/viewer/`, `index.html` | The index and the artboard view, built with `@dockwell/ui`. Artboards show in an iframe of `frame.html` at their canvas size. |
| `scripts/snapshots.ts` | The snapshot script. |
