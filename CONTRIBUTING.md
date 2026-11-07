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

Before we merge your first pull request, you sign the [Contributor License Agreement](CLA.md) once. Dockwell's core is licensed under the AGPL-3.0, and the paid Organization edition and Dockwell Cloud are built on that same core. The CLA lets the project ship your contribution there too. In return, it commits the project to keep your contribution to the open-source core available under the AGPL-3.0, or another licence approved by the Open Source Initiative, for as long as the project distributes it. You keep the copyright.

The API schema in `proto/` and the clients generated from it in `gen/` and `packages/api/` are licensed under the Apache-2.0 instead, so anyone can build the clients into their own tools. Contributions there are published under the Apache-2.0, under the same CLA. The CLA doesn't yet say that they stay under the Apache-2.0; that question is open for the CLA's review ([ADR 0012](docs/adr/0012-apache-for-api-schema-and-clients.md#the-cla)).

The CLA is still a draft, and the bot that collects signatures isn't set up yet. Until it is, we can review outside pull requests but can't merge them.

## Development setup

You need Go 1.27 or later, Node.js 24, and pnpm. Run `corepack enable` once to get the pnpm version this repository pins.

```sh
make deps    # install the JavaScript dependencies
make dev     # the API on 127.0.0.1:7600 and the panel with hot reload on localhost:5173
make run     # build bin/dockwell with the panel built in, and run it on 127.0.0.1:7600
make mocks   # browse the screen mocks in design/mocks on 127.0.0.1:5174
make check   # what CI runs: lint, tests, generated-code checks and a smoke test of the binary
make e2e     # claim, sign in and run the panel's flows in headless Chromium, with a virtual passkey and a fake Docker Engine
make budgets # idle memory of an agent, and of a control plane with its agent, against their budgets;
             # BASELINE=other/dockwell measures that build beside it
make install-test  # the install script in Debian, Ubuntu, AlmaLinux and Alpine containers (needs Docker and cosign v3)
```

The Install workflow runs the last two, as well as the install script under systemd on the runner and, nightly, the time from install to a running panel on one CPU. On a pull request, CI and Install run only the jobs its files need, as [`scripts/ci-jobs.sh`](scripts/ci-jobs.sh) picks them ([ADR 0050](docs/adr/0050-ci-runs-what-a-change-needs.md)): the budgets for a change to Go code, and the install tests for one to the install script, the release, or what the script runs in the binary. [docs/install.md](docs/install.md#how-it-is-tested) describes how.

The first start prints a one-time link that claims the install: open it to create your owner account with a passkey. With `make dev`, open it on port 5173 instead of 7600. Run `dockwell claim --data-dir <dir>` for a new link, and `dockwell recover` to sign in without your passkey.

The control plane's store runs on SQLite and on Postgres, and the tests that touch it run on each. Without a Postgres server they run on SQLite only, and the Postgres runs skip, saying so. To run them too, point `DOCKWELL_TEST_POSTGRES` at a server you can create schemas on, such as `DOCKWELL_TEST_POSTGRES=postgres://postgres@127.0.0.1:5432/postgres make test`; each test gets a schema of its own and drops it. CI does this for every pull request.

Run `make` on its own to list every target. Developer tools such as buf and golangci-lint are pinned in `tools/go.mod` and run with `go tool -modfile=tools/go.mod <tool>`, so there is nothing else to install.

The panel's text goes through Lingui: write it in English inside `<Trans>` or `` t`…` ``, run `make i18n` to extract it, and add the Simplified Chinese translation to `web/src/locales/zh-CN/messages.po`. CI fails on a missing translation.

The API schema lives in `proto/`. Code generated from it goes to `gen/` (Go) and `packages/api/src/gen/` (TypeScript). After changing the schema, run `make generate` and commit the result. Run it through make rather than `buf generate` on its own, which empties `gen/` and deletes `gen/LICENSE`. Files in `proto/`, `gen/` and all of `packages/api/` start with `SPDX-License-Identifier: Apache-2.0`, and generated files take that header from their `.proto` file. Every method must set `(dockwell.v1.effect)`, and a test fails the build when one doesn't. The CLI's API commands are generated from the schema too, with help from its comments, so write each method's comment as its name and a verb, such as "ListServers returns every server". A method whose command would take a name that is already used sets `(dockwell.v1.command)`, a request field whose flag would take one of the CLI's own, such as `--token`, sets `(dockwell.v1.flag)`, and a service whose commands have more than one noun describes each with `(dockwell.v1.nouns)` ([ADR 0019](docs/adr/0019-a-cli-generated-from-the-schema.md)).

## Repository layout

| Path | What it holds |
| --- | --- |
| `cmd/dockwell/` | The binary's entry point |
| `internal/cli/` | The `dockwell` commands |
| `internal/cli/apicli/`, `internal/cli/apicmd/`, `internal/cli/apinames/` | The API commands: the machinery that runs them, their descriptions, and their names alone, which the agent build links to say they're in the full build. The last two are generated from the schema by `cmd/protoc-gen-dockwell-cli` and `internal/cli/cligen` ([ADR 0019](docs/adr/0019-a-cli-generated-from-the-schema.md), [ADR 0023](docs/adr/0023-a-full-build-and-an-agent-build.md)) |
| `internal/control/` | The control plane: API and agent listeners, services, and the identity authority |
| `internal/auth/` | Who may use the panel and API: the claim, passkeys, authenticator codes and sessions ([ADR 0011](docs/adr/0011-claim-and-sign-in.md)) |
| `internal/https/` | The panel's domain and its certificates from Let's Encrypt |
| `internal/agent/` | The agent: its identity (`enroll`) and runtime |
| `internal/store/` | The control plane's state, in SQLite or Postgres, with embedded migrations ([ADR 0014](docs/adr/0014-object-model-and-store.md)) |
| `internal/resource/`, `internal/revision/`, `internal/scope/` | The resource model and its kinds, the revision store with plans, undo and drift, and the projects and environments things live in ([ADR 0009](docs/adr/0009-resources-revisions-and-plans.md), [ADR 0014](docs/adr/0014-object-model-and-store.md)) |
| `internal/pki/`, `internal/seal/`, `internal/ticket/` | The CA, secrets sealed at rest, and join tickets ([threat model](docs/security/server-identity.md)) |
| `internal/serve/` | What every HTTP listener shares: timeouts, TLS, shutdown, recovery, RPC logging |
| `internal/panel/` | Serves the panel embedded in the binary |
| `internal/buildinfo/`, `internal/logging/` | Build information and structured logging |
| `proto/` | The API schema ([ADR 0003](docs/adr/0003-one-schema-protobuf-connect.md)), under the Apache-2.0 |
| `gen/`, `packages/api/` | Code generated from the schema, for Go and TypeScript, under the Apache-2.0 |
| `packages/ui/` | The design system as typed React components, with tokens generated from `design/system/tokens.json` |
| `packages/mockview/` | A local viewer and snapshot tool for the screen mocks in `design/mocks/` (`make mocks`) |
| `web/` | The panel ([ADR 0008](docs/adr/0008-panel-libraries.md)) |
| `scripts/install.sh`, `scripts/install-test/` | The install script ([docs/install.md](docs/install.md)) and its tests |
| `scripts/sigstore_trusted_root.json`, `scripts/check-trust-root.sh` | The copy of Sigstore's trust root the install script pins, for installs through a mirror, and its check ([ADR 0013](docs/adr/0013-mirrors-for-releases-and-images.md)) |
| `scripts/budgets.sh` | The idle memory budgets |
| `tools/` | Pinned developer tools |
| `docs/`, `design/` | Requirements, decisions and the design system |

## Code of conduct and security

Everyone taking part follows the [code of conduct](CODE_OF_CONDUCT.md). Report security problems privately as described in [SECURITY.md](SECURITY.md), never in a public issue.
