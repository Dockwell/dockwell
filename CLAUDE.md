# Dockwell

An open-source control plane for servers, from one VPS to a fleet. One Go codebase builds `dockwell` twice (ADR 0023): the full build, with the `control` and `agent` roles and a React and TypeScript panel embedded in it, for the control server; and the agent build, `-tags agent`, with only the agent, for every server that joins.

## Context lives in this repo

- Product requirements: `docs/product/prd.md`. It is the source of truth; edit it here.
- Decisions: `docs/adr/`. Read the relevant record before proposing an architecture change, and add a record for a new decision.
- Design: start at `design/README.md`, which says what is where and which phase builds each screen. Then `design/system/README.md` (brand book and content rules), `design/system/tokens.json`, `design/system/components/index.d.ts`, and the mocks in `design/mocks/*.dc.html`.
- Don't go back to the original Claude artifacts unless a redesign happened there; then re-import as `design/README.md` describes.

## Commands

- On a pull request, each CI job's first step asks `scripts/ci-jobs.sh` whether the files it changes need the job, and skips the rest when they don't; Install's Release snapshot asks for the jobs after it. Main runs everything (ADR 0037). A new kind of file, or a new job, needs a rule there and an example in `scripts/ci-jobs-test.sh`, and a new CI job needs `if: steps.pick.outputs.run == 'true'` on each step after the question.
- `make check` runs what CI runs: lint for Go, the schema, TypeScript, CSS and shell scripts (with shellcheck, when it is installed), translations, license headers and doc links; Go and TypeScript tests; the generated-code and breaking-change checks; and a smoke test of the built binary. CI is the gate: before a push, run `make lint` and the tests for what you changed, such as `go test -race` on the packages you touched, the panel's tests if you touched the panel, and `make e2e` if you changed what a person sees or does. Nothing merges until CI is fully green, the Docker jobs included, and its review is done. Several agents share one machine, so don't queue a full `make check` before every push.
- The Install workflow also runs `make budgets` (idle memory: agent under 40 MB, with a warning from 30 MB, control plane plus agent under 250 MB; on pull requests, beside main's build) and `make install-test` (the install script in containers; needs Docker and cosign v3). Run them when you change `scripts/install.sh` or anything that could move memory.
- `make logs-load` measures an agent while a container floods its log, on a fake Docker. Run it when you change how logs are read or sent.
- The store's tests run on SQLite, and on Postgres too when `DOCKWELL_TEST_POSTGRES` names a server, as CI sets it. Set it when you change `internal/store` or any query.
- The ingress's tests run against real Caddy and Pebble when `DOCKWELL_TEST_CADDY` and `DOCKWELL_TEST_PEBBLE` name them, as CI sets them. `scripts/ingress-tools.sh <dir>` fetches both. Set them when you change `internal/ingress`.
- `make generate` after any change in `proto/` or `design/system/tokens.json`. Commit what it writes.
- `make run` builds everything and serves on 127.0.0.1:7600; `make dev` adds the panel with hot reload on :5173. `make build-agent` builds the agent build as `bin/dockwell-agent`.
- Panel text: English in `<Trans>`/`` t`…` ``, then `make i18n` and translate `web/src/locales/zh-CN/messages.po`. Components come from `@dockwell/ui`; port a design-system component there (from `design/system/components/`) before using it in a screen.
- Tools: `go tool -modfile=tools/go.mod <buf|golangci-lint|…>`. Generate with `make generate`, not `buf generate` on its own, which empties `gen/` and deletes `gen/LICENSE`. pnpm comes from corepack (`packageManager` in package.json). New npm versions are held back 14 days (`minimumReleaseAge`); pin Go modules and actions to releases at least that old too, with actions pinned by commit hash.

## Rules

- UI text, CLI output, errors and docs follow the content fundamentals in `design/system/README.md`: sentence case, address the reader as "you", no emoji or exclamation marks, fact then consequence then fix.
- Terminology: *server* and *control server* in the product; *agent* for the binary in docs and the CLI; never *master* or *slave*.
- Every change to managed state is a revision with a plan (PRD principle 2). Don't add a mutating path that bypasses the revision store: only `internal/revision` calls `store.DB.Revise`, and a test fails on anything else that does.
- The panel uses only the public API through the generated client. Nothing is UI-only.
- Memory budgets: the agent stays under 40 MB RSS at idle, and the control plane plus agent under 250 MB.
- A test asks nothing beyond the machine it runs on. A test binary refuses to reach a real STUN server or metadata service, and `DOCKWELL_TEST=1` has a real binary ask neither. The Makefile and CI set it, so a new script run through them has it, but a script or e2e that starts `dockwell run` itself sets it too, and a service under systemd or OpenRC needs it in a drop-in or `/etc/conf.d/dockwell`.
- The agent build links only what an agent runs (ADR 0023). Control-plane code lives in packages the agent doesn't import; in `internal/cli`, files that reach it carry `//go:build !agent`, and `edition_agent.go` stands in for them. `cmd/dockwell/build_test.go` lists what the agent build may link: add a package or module there only for the agent, with what `make budgets` says it costs.
- Work on a branch and open a pull request to `main` that references its issue and milestone.
- Every source file starts with `SPDX-License-Identifier: AGPL-3.0-only`, except in `proto/`, `gen/` and `packages/api/`, which are `Apache-2.0` (ADR 0012). `scripts/check-spdx.sh` checks both.
- Go: errors and log messages follow the content rules too. Wrap errors with context, and when the operator can fix something, end the message with how.
