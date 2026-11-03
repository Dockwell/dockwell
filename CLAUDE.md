# Dockwell

An open-source control plane for servers, from one VPS to a fleet. One Go binary, `dockwell`, with `control` and `agent` roles, and a React and TypeScript panel embedded in it.

## Context lives in this repo

- Product requirements: `docs/product/prd.md`. It is the source of truth; edit it here.
- Decisions: `docs/adr/`. Read the relevant record before proposing an architecture change, and add a record for a new decision.
- Design: start at `design/README.md`, which says what is where and which phase builds each screen. Then `design/system/README.md` (brand book and content rules), `design/system/tokens.json`, `design/system/components/index.d.ts`, and the mocks in `design/mocks/*.dc.html`.
- Don't go back to the original Claude artifacts unless a redesign happened there; then re-import as `design/README.md` describes.

## Rules

- UI text, CLI output, errors and docs follow the content fundamentals in `design/system/README.md`: sentence case, address the reader as "you", no emoji or exclamation marks, fact then consequence then fix.
- Terminology: *server* and *control server* in the product; *agent* for the binary in docs and the CLI; never *master* or *slave*.
- Every change to managed state is a revision with a plan (PRD principle 2). Don't add a mutating path that bypasses the revision store.
- The panel uses only the public API through the generated client. Nothing is UI-only.
- Memory budgets: the agent stays under 40 MB RSS at idle, and the control plane plus agent under 250 MB.
- Work on a branch and open a pull request to `main` that references its issue and milestone.
