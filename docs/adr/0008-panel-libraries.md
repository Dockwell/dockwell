# 0008. Libraries and tooling for the panel

- Status: Accepted
- Date: 2026-11-03

## Context

[ADR 0006](0006-web-react-typescript.md) chose React and TypeScript and left the router, data fetching, i18n and tooling to the panel's first pull request. The constraints come from the PRD:

- interactions under 100 ms at p95, and a first load under 1.5 s on a cold cache;
- English and Simplified Chinese at launch;
- live data over the schema's server streams ([ADR 0003](0003-one-schema-protobuf-connect.md));
- every filter, tab and selection addressable by URL, so links can be shared during an incident.

## Decision

| Concern | Choice | Why |
| --- | --- | --- |
| Build | Vite 8, embedded into the binary from `internal/panel/dist/app` | Fast builds and hot reload; output is plain static files. A build step writes Brotli and gzip variants, and preloads the fonts. |
| Routing | TanStack Router, file-based routes in `web/src/routes` | Type-safe routes and search params, so URL state is checked by the compiler. Routes are code-split. |
| Server state | TanStack Query with Connect-Query | Caching, deduplication and retries for unary calls, keyed by the generated method descriptors. |
| i18n | Lingui 6 with PO catalogs | Messages are written in English in the code (`<Trans>`, `` t`…` ``) and extracted. PO is the format translators already use. Macros are transformed natively, with no Babel. `lingui check` fails CI on stale or missing translations. |
| Design system strings | `UIStringsProvider` in `@dockwell/ui` | The design system stays free of any i18n library, and the panel passes it translated words. |
| Lint and format | Biome | One fast tool for TypeScript, JSON and CSS formatting and linting, including accessibility rules. |
| Tests | Vitest, Testing Library, axe-core | Component tests run against the real providers, with an in-memory Connect transport. axe checks every component. |

The panel serves under a strict Content-Security-Policy with no inline scripts or styles. The script that applies a chosen theme before first paint is a file (`/theme.js`), not inline.

## Consequences

- `make build` builds the panel and the binary together. `go build` alone still works and serves a page explaining how to add the panel.
- Generated files are committed and checked in CI: `routeTree.gen.ts`, the tokens and the API client.
- Lingui 6 and Vite 8 are young majors. Their configuration is small and isolated in `web/vite.config.ts` and `web/lingui.config.ts` if either needs replacing.
