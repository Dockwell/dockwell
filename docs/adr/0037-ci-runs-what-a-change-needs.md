# 0037. CI runs what a change needs

- Status: Accepted
- Date: 2026-11-07

## Context

Every push to a pull request started the same 25 jobs: 11 in CI, 13 in Install and the footprint comment, about 103 runner-minutes. The repository runs on GitHub's free hosted runners, 20 jobs at a time, and some ten tasks push pull requests at once, 17 to 33 pushes an hour. On 2026-11-07 at 21:19 UTC, 40 runs were queued with 3 in progress, so a push waited hours for its result.

Most of those minutes test what the push didn't change. A pull request that changes only the panel ran the race-detector Go tests, the drivers on real Docker and Caddy, and the service end to end on Docker, though none of them reads `web/`. A docs change ran all 25. The install script ran in eight containers and under systemd for every change, though it only runs `version`, `role`, `claim`, `join`, `token` and `run` in the binary.

CI is the gate: nothing merges until it's green. So picking fewer jobs mustn't let a change through that the full set would stop.

## Decision

### Each job asks whether it's needed

Every gated CI job starts, as before, when the pull request is pushed. Its first step after the checkout runs `scripts/ci-jobs.sh needs <job>` on the files the pull request changes, read from its merge commit against main, and its other steps run only when the answer is yes. A job that isn't needed passes in about 20 seconds. Licenses and links has no question to ask, and always runs.

In Install, Release snapshot asks for itself and for the jobs after it, which already wait for it. Memory budgets and the install legs run only when it says they're needed. Build of main runs on every pull request, as it checks out main, not the change, and it takes about a minute.

A job that waits for another doesn't start when that one ends: it goes to the back of the queue. On 2026-11-07, Install's legs started 4 to 23 minutes after the Release snapshot they wait for had finished. So a job that picks the jobs, with every other job waiting for it, would make each push wait for a runner twice, and a check after them all, three times. With 17 to 33 pushes an hour against the 12 to 15 the runners can take, the queue stays long, and that would cost more than the minutes it saves. A job that asks for itself waits once, as before, and costs a runner for about 20 seconds when it's not needed.

The picker maps each path to the jobs that read it, and its rules are written from what each job reads, not from what usually changes together:

| A change to | Runs |
| --- | --- |
| Docs, top-level Markdown, `assets/` | Licenses and links only |
| `web/`, `packages/` but the mock viewer, the panel's package files | API schema, Panel, Panel end to end, Panel end to end on Docker |
| `design/`, `packages/mockview/` | API schema, Panel |
| A catalog entry | Go, Service end to end on Docker, Catalog on Docker, Panel end to end |
| A Go test, `*_test.go` | Go, Go on Docker, Go on Caddy |
| Other Go code, `proto/`, `gen/`, most of `scripts/` | Every CI job, and the Memory budgets with its two builds |
| What the install script runs: most of `internal/cli`, `internal/buildinfo`, `internal/datadir`, `internal/ticket`, `cmd/dockwell`, `go.mod`, `tools/` | The same, and the install tests |
| `scripts/install.sh` | Go, Hundreds of containers, the budgets and the install tests |
| `scripts/install-test/`, the trust root | The install tests |
| `.goreleaser.yaml` | The budgets and the install tests |
| A workflow, the Makefile, the picker, or a path no rule knows | Every job |

Two choices there are judgements:

- **The budgets don't run for the panel.** Only the full build embeds the panel, which reads each file once at start to hash it, so a panel change moves the control plane's idle RSS by about its size in file-backed pages. That's far inside the 250 MB budget, and the Panel job reports the bundle's size on every panel change.
- **The install tests run for the commands the script runs.** The containers and systemd are the only place an arm64 build runs, and the only place `dockwell run` starts with its default paths under a service manager. A change to any other Go code is still built for arm64 by the Go job, and run on amd64 by every other job.

A push to main, a nightly run and a manual run pick every job, so main is tested in full after each merge, and a release is only ever tagged on a commit that was.

`scripts/ci-jobs-test.sh` holds the picker to examples of each kind of change, and Licenses and links runs it on every change. A rule that changes without its example fails the pull request that changed it.

### What a branch rule requires

A CI job that isn't needed still runs and passes, so it's never skipped, and each one can be required by name: a failed question fails its job. Install's legs are a matrix, named after their images, and are skipped as a whole when they're not needed. A branch rule counts a skipped job as passing, and can't name a matrix leg that may not run. So Install ends with Install, every job, which runs whatever happened before it. It fails unless every job the change needs passed and every other one passed or was skipped, and it runs, and fails, when the rest were cancelled, so a cancelled run never reads as green. Build of main may fail, as it only adds a comparison.

The checks a branch rule on main requires are every CI job by name and Install, every job.

## Consequences

- Over the last 40 pull requests merged before this, a push uses about 82 runner-minutes instead of 103. Most of them changed Go code, which still runs nearly everything. A panel change uses about 30 minutes, a Go test about 35, a catalog entry about 57, and a docs change about 6.
- A push still starts the 11 CI jobs, as each one asks for itself. A CI job that isn't needed reads as passed, and its log and summary say it wasn't needed and why.
- A new kind of file runs every job until it has a rule. A rule that's too narrow would let a change through untested, so a new rule needs an example in `scripts/ci-jobs-test.sh`, and main still runs everything after it merges.
- A new CI job needs a rule in the picker, the question as its first step after the checkout, `if: steps.pick.outputs.run == 'true'` on each step after that, and a place in the branch rule. A step without the `if:` runs anyway, which is slow but never lets anything through.
- The footprint comment stays as it was for the last push that measured memory, which it names, when a later push doesn't change Go code.
- Go changes still cost about 93 runner-minutes. The Go test cache, restored from main, would let the Go job skip packages a change can't reach, and Go on Caddy could skip changes outside what `internal/ingress` imports. The repository's Actions cache is at its 10 GB limit, so the first needs room made for it.
