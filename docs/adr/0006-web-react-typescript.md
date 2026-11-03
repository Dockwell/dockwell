# 0006. React and TypeScript for the panel, on the Dockwell design system

- Status: Accepted
- Date: 2026-11-03

## Context

The PRD treats design quality as a feature. Its targets for the panel are interactions under 100 ms at p95, a first load under 1.5 s on a cold cache, every feature at 360 px, WCAG 2.2 AA with full keyboard operation, English and Simplified Chinese at launch, and an installable PWA with push. The design system is ready in [`design/system/`](../../design/system/README.md), with a React reference implementation and TypeScript prop types.

## Decision

- The panel is a React and TypeScript single-page app built with Vite. It lives in `web/` and is embedded in the `dockwell` binary, so one URL serves the whole fleet with nothing else to install.
- `packages/ui` is the design system as typed React components, ported one by one from the reference bundle and the component guidelines. Its CSS variables are generated from `design/system/tokens.json`. The mocks are built from these components only.
- It talks to the control plane only through the generated Connect client ([0003](0003-one-schema-protobuf-connect.md)). Live data arrives over server streams and updates in place.
- Internationalization, the PWA manifest and service worker, and accessibility checks are set up in the first web milestone, not added later.
- Router, data-fetching and i18n libraries are chosen in the web skeleton's pull request and recorded there.

## Consequences

- The design system becomes code that every screen is assembled from, which is the PRD's mitigation for design ambition slowing shipping.
- Bundle size and interaction latency are budgets checked in CI, like the agent's memory.
- Every string goes through the i18n layer from the first screen. That slows the first screens slightly and saves a retrofit.
