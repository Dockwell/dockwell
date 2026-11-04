# Contributing to Dockwell

Thanks for your interest. Dockwell is at the very start of its first phase, so the shape of the project is still being set. This guide says how to help now, and it grows as the code does.

## Where help is most useful now

- **Feedback on the plan.** Read the [product requirements](docs/product/prd.md) and the [design](design/README.md), and open an issue where something is wrong, missing or unclear. This matters most if you run servers today with 1Panel, Coolify, Dokploy or by hand.
- **Your migration story.** What would you need to move your servers to Dockwell? Importers are planned for 1Panel, Coolify, Dokploy and plain Compose.
- **Issues labelled `good first issue`**, once the first code lands.

## Before you write code

- Pick an existing issue, or open one first for anything bigger than a small fix, so nobody's work is wasted.
- Changes to the architecture, the API schema, the data model, or a dependency with a wide reach need a decision record in [`docs/adr/`](docs/adr/). The template is in its README.
- Every pull request targets `main`, stays small enough to review in one sitting, and says what it changes and how you tested it.

## How Dockwell writes

The product has one voice, set out in the design system's [content fundamentals](design/system/README.md#content-fundamentals). The short version, for UI text, CLI output, errors and docs:

- Sentence case everywhere. No emoji, no exclamation marks, no filler ("simply", "just", "successfully").
- Address the reader as "you"; the product never says "I" or "we".
- Lead with the fact, then the consequence, then the fix. Errors say how to fix the problem and never apologize.
- Numbers carry units with a space: `1.8 GB`, `41 s`.
- Say *server* and *control server* in the product, and *agent* for the binary in docs and the CLI. Never *master* or *slave*.

## Contributor License Agreement

Before we merge your first pull request, you sign the [Contributor License Agreement](CLA.md) once. Dockwell's core is licensed under the AGPL-3.0, and the paid Organization edition and Dockwell Cloud are built on that same core. The CLA lets the project ship your contribution there too. Your contribution always stays available under the AGPL in the open-source core, and you keep the copyright.

The CLA is still a draft, and the bot that collects signatures isn't set up yet. Until it is, we can review outside pull requests but can't merge them.

## Development setup

You need Go 1.27 or later, Node.js 24, and pnpm. Run `corepack enable` once to get the pnpm version this repository pins.

```sh
make deps    # install the JavaScript dependencies
make dev     # the API on 127.0.0.1:7600 and the panel with hot reload on localhost:5173
make run     # build bin/dockwell with the panel built in, and run it on 127.0.0.1:7600
make mocks   # browse the screen mocks in design/mocks on 127.0.0.1:5174
make check   # what CI runs: lint, tests, generated-code checks and a smoke test of the binary
```

Run `make` on its own to list every target. Developer tools such as buf and golangci-lint are pinned in `tools/go.mod` and run with `go tool -modfile=tools/go.mod <tool>`, so there is nothing else to install.

The panel's text goes through Lingui: write it in English inside `<Trans>` or `` t`…` ``, run `make i18n` to extract it, and add the Simplified Chinese translation to `web/src/locales/zh-CN/messages.po`. CI fails on a missing translation.

The API schema lives in `proto/`. Code generated from it goes to `gen/` (Go) and `packages/api/src/gen/` (TypeScript). After changing the schema, run `make generate` and commit the result. Every method must set `(dockwell.v1.effect)`, and a test fails the build when one doesn't.

## Repository layout

| Path | What it holds |
| --- | --- |
| `cmd/dockwell/` | The binary's entry point |
| `internal/cli/` | The `dockwell` commands |
| `internal/control/` | The control plane: API and agent listeners, services, and the identity authority |
| `internal/agent/` | The agent: its identity (`enroll`) and runtime |
| `internal/store/` | The control plane's state, in SQLite, with embedded migrations |
| `internal/pki/`, `internal/seal/`, `internal/ticket/` | The CA, secrets sealed at rest, and join tickets ([threat model](docs/security/server-identity.md)) |
| `internal/serve/` | What every HTTP listener shares: timeouts, TLS, shutdown, recovery, RPC logging |
| `internal/panel/` | Serves the panel embedded in the binary |
| `internal/buildinfo/`, `internal/logging/` | Build information and structured logging |
| `proto/` | The API schema ([ADR 0003](docs/adr/0003-one-schema-protobuf-connect.md)) |
| `gen/`, `packages/api/` | Code generated from the schema, for Go and TypeScript |
| `packages/ui/` | The design system as typed React components, with tokens generated from `design/system/tokens.json` |
| `packages/mockview/` | A local viewer and snapshot tool for the screen mocks in `design/mocks/` (`make mocks`) |
| `web/` | The panel ([ADR 0008](docs/adr/0008-panel-libraries.md)) |
| `tools/` | Pinned developer tools |
| `docs/`, `design/` | Requirements, decisions and the design system |

## Code of conduct and security

Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md). Report security problems privately as described in [SECURITY.md](SECURITY.md), never in a public issue.
