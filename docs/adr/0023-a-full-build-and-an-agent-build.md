# 0023. A full build and an agent build, from one codebase

- Status: Accepted. It supersedes the "one binary" of [0007](0007-one-binary-two-roles.md).
- Date: 2026-11-04

## Context

[ADR 0007](0007-one-binary-two-roles.md) has every server run one binary, `dockwell`, with the control plane's role turned on only on the control server. It chose that for one artifact to build, sign and update. [ADR 0018](0018-keep-the-budgets-in-rss.md) then found what it costs: most of an idle agent's RSS is pages of the binary that package initialization maps, and Go initializes every package linked in, whether or not the agent calls it. Each control-plane feature costs every agent memory. An idle agent's RSS grew from 19.6 MB to 26.7 MB, and to about 30 MB with the work in review, against its 40 MB budget and a warning at 30 MB. ADR 0018 also measured the way out: the same code, built with only the agent linked in, idled at 12.8 MB.

The Docker driver (#71) and secrets (#70) are next, and each brings packages that only the control plane runs, such as compose-go and the encryption of secrets at rest.

The options:

1. **Trim each feature.** Defer what packages do at init, or keep heavy dependencies out. ADR 0018 tried the first: deferring the token table's heap moved neither figure, because the pages init maps are the cost, and Go gives no way to defer the initialization of a package that is linked in. The second is a review rule for every control-plane change, forever, and it can't keep out dependencies the control plane needs, such as SQLite, pgx and WebAuthn.
2. **Raise the agent's budget.** The PRD promises an agent under 40 MB, and the growth wouldn't stop.
3. **Two builds of the same code.** The control server runs the full build, and every server that joins runs an agent build that links only the agent. Chosen.

And for how to make two builds:

- **Build tags.** One main package, built with or without `-tags agent`. The split lives in `internal/cli`, where the commands are, and in `internal/buildinfo`, which says which build it is. Every other package is either in the agent build or not. `go build ./cmd/dockwell` keeps making today's binary.
- **Two main packages.** No tags, but `internal/cli` would have to become two packages, one for the commands both builds have and one for the rest, and its tests would move with them. Rejected as the larger change for the same result.

## Decision

### Two builds of `dockwell`

- **The full build**, `go build ./cmd/dockwell`: the control plane, its own agent, the panel, the CLI and the API commands. The control server runs it. It is today's binary.
- **The agent build**, `go build -tags agent ./cmd/dockwell`: only the agent. Every server that joins runs it.
- Both come from the same source, commit and release, and both are called `dockwell` on a server. They speak the same agent protocol ([ADR 0017](0017-the-change-api-and-the-agent-stream.md)), so a control server talks to agents of either build alike.

### What the agent build does

- `dockwell run` runs the agent with the identity `dockwell join` gave it. Without one, it never becomes a control plane: it says this is the agent build, and how to join a control plane, or to get the full build by running the install script without `--join`. On a data directory that holds a control plane, it refuses, and says to install the full build.
- `dockwell join`, `role` and `version` work as in the full build. Before the server joins, `dockwell role` prints `none`.
- The commands that run beside the control plane, `dockwell token create`, `claim` and `recover`, each have a stand-in that says they run on the control server. The API commands, such as `dockwell servers list`, and `dockwell login` and `logout`, say they are in the full build, on the control server. The agent build knows the API commands by name from `internal/cli/apinames`, which the generator writes from the schema beside them ([ADR 0019](0019-a-cli-generated-from-the-schema.md)), and imports nothing. Both exit with status 2, as a usage error does.
- Any other command it lacks is a mistake, as in the full build: `dockwell joni` says it isn't a command, with the nearest one there is, `dockwell join`. Shell completion works as in the full build.
- `--database`, and the control plane's flags of `dockwell run`, such as `--listen` and `--domain`, aren't there.
- `dockwell version` says which build it is, and `BuildInfo` in the API carries it as `edition`, `BUILD_EDITION_FULL` or `BUILD_EDITION_AGENT`. A build from before there were two, v0.1.0 or v0.2.0, names none, and is the full build. The agent's hello carries its `BuildInfo`, so the control plane knows each server's build: it logs it when the server connects, and serves it as the server's `agent_build`, which the panel can show on the server page.

### How the agent build stays apart

- In `internal/cli`, each file that reaches the control plane, its store or the API client carries `//go:build !agent`. `edition_full.go` holds the full build's side of the commands, and `edition_agent.go`, with `//go:build agent`, the agent build's stand-ins. Outside `internal/cli`, only `internal/buildinfo` has build tags, to say which build it is. Of the API commands, the agent build links only `internal/cli/apinames`, their names, and a test of the full build checks those are exactly its API commands.
- `cmd/dockwell/build_test.go` runs `go list -deps -tags agent` for linux amd64 and arm64, and fails on a package the agent build may not link: one of Dockwell's own that isn't on `agentPackages`, one of another module that isn't on `agentModules`, or one that only the control plane runs, such as `internal/control`, the store, SQLite, pgx, WebAuthn, Vanguard or compose-go. It names the package, says why, and shows the chain of imports that brought it in. A second test checks that the full build still links the control plane, so the build constraints can't have been turned around.
- `go vet`, golangci-lint and the tests run for both: once as the full build, and again with `-tags agent` for the packages the agent build links (`scripts/agent-packages.sh`), as `make lint`, `make test` and CI do. CI builds both for amd64 and arm64, and a smoke test joins the agent build to a control plane of the full build (`scripts/smoke-agent.sh`).

### Future features stay out of the agent build

- A control-plane feature lives in packages the agent build doesn't import: under `internal/control`, the store, auth, the revision engine, or a package of its own. Its commands go in a file of `internal/cli` with `//go:build !agent`.
- What the agent runs of a feature, such as a driver, goes in a package of its own, which imports nothing of the control plane's. The control plane may import it. For the Docker driver (#71), `internal/docker` and the container, volume and network drivers are the agent's, and Compose import and export (`internal/compose`) is the control plane's. For secrets (#70), decrypting a secret on the agent is the agent's, and encrypting and keeping it is the control plane's.
- A package on `agentPackages` or a module on `agentModules` is on every server that joins. A pull request that adds one says what `make budgets` measured it costing.

### Releases

- Each release has two archives for each of linux amd64 and arm64: `dockwell_<version>_linux_<arch>.tar.gz`, the full build, and `dockwell-agent_<version>_linux_<arch>.tar.gz`, the agent build. The binary in each is called `dockwell`.
- One `checksums.txt` lists the archives of both, with `install.sh` and Sigstore's trust root, and is signed once. The provenance attestation covers every archive and SBOM. Both builds are reproducible, as before ([releasing](../releasing.md)).
- A mirror ([ADR 0013](0013-mirrors-for-releases-and-images.md)) serves a release's files, so it carries both archives. One that only serves agents may leave out the full build's, and the other way round.

### The install script

- It picks the build from what the server is, as its data directory says:
  - A control plane in `/var/lib/dockwell/control`, its key or its SQLite state, as `dockwell run` decides it: the full build. A first start that failed can leave only `control/key.new`, which isn't a control plane yet.
  - A server that has joined, with an agent's identity and no control plane: the agent build, whichever build it has, since that runs everything such a server runs. Servers that joined with v0.1.0 or v0.2.0 run the full build, which runs the agent too, so their next upgrade switches them to the agent build, and says so.
  - Otherwise, with `--join`: the agent build. Without it, the server becomes a control server: the full build. So a server with the agent build that never joined, run again without `--join`, becomes a control server, as any first install does.
- It reads the installed version and build from one run of `dockwell version --json`, which every release has, and takes a binary that names no edition for the full build, which v0.1.0 and v0.2.0 are.
- A release with no agent build, such as `--version 0.2.0 --join <ticket>`, gets the full build, and the script says so.
- Verification doesn't change: the same signed checksums list both builds' archives, and the archive must match them.

### Memory budgets

- `scripts/budgets.sh` measures each budget with the build that runs it: an agent on its own with the agent build, joined to a control plane, and a control plane with its own agent, in one process, with the full build.
- On a pull request, it measures main's two builds beside the pull request's. A main from before there was an agent build has only the full build, so its agent figure is its full build running as an agent, which is what a server that joined ran then. `budgets.sh` says so in one sentence, in the job summary and in `budgets.json`, which the footprint comment posts as it is, once this is on main. That happens once: for the pull request that adds the agent build.

### Later work, recorded now

- **Self-updates.** When agents update themselves from signed releases (PRD), each picks the build as the install script does: a server that has joined, with no control plane, updates to the agent build, a `dockwell-agent_` archive, so one that joined with the full build switches then too. The control server's own agent runs in the full build's process, so it updates with the control plane, to a `dockwell_` archive.
- **Moving the control plane.** The plan that moves it (PRD: "Move control plane") installs the full build on the server it moves to, as one of its steps, before the control plane starts there. Promoting an agent with `dockwell control restore` does the same, since the agent build has no control plane to restore. A server that merges into another install (PRD: "Connect to another Dockwell") becomes an agent. It keeps the full build while its data directory still holds a control plane, and gets the agent build at the first upgrade after that, as any server that has joined does.

## Consequences

- **An idle agent reads 18.7 MB with the agent build, against 27.7 MB for main's full build running as an agent, 9.0 MB less.** A control plane with its own agent reads 37.0 MB, against 36.9 MB on main. Those are the medians of 5 runs side by side, each sampled after 180 s, as `scripts/budgets.sh` measures (ADR 0018), on a machine with 16 CPUs. The agent build's binary is 13.2 MB, against 33.2 MB, and its archive for amd64 5.0 MB, against 11.9 MB.
- The agent build isn't the floor. A scratch `main` that calls `agent.Run` and nothing else read 13.5 MB at 30 s and 16.3 MB at 180 s, against the agent build's 15.6 MB and 18.8 MB, in 3 runs of each side by side. The other 2.5 MB is what the scratch `main` leaves out: the command line, with cobra, `text/template` and `internal/cli`. A lighter command line for the agent build would take most of it back. ADR 0018's 12.8 MB for an agent-only build was sampled at 30 s, at 50acc68.
- A control-plane feature no longer costs agents memory, and what does cost them is named in a list that a pull request has to change.
- Agents don't have the API commands. Run them on the control server, or with the full build anywhere else.
- Every release builds, signs and attests two archives per platform.
- `go vet`, the linters and the tests run twice for the packages the agent build links. A test of either build alone carries its build constraint, as a test of the agent build's stand-ins does.
- [ADR 0007](0007-one-binary-two-roles.md)'s "one binary" is superseded: every machine runs one of two builds of `dockwell`. Its roles, agents that dial out, and everything else it decided stand. [ADR 0018](0018-keep-the-budgets-in-rss.md)'s note that ADR 0007 rules out an agent-only binary no longer holds.
