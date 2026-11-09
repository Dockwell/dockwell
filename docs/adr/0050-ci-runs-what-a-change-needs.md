# 0050. CI runs what a change needs

- Status: Accepted
- Date: 2026-11-07

## Context

Every push to a pull request started the same 25 jobs: 11 in CI, 13 in Install and the footprint comment, about 103 runner-minutes. The repository runs on GitHub's free hosted runners, 20 jobs at a time, and some ten tasks push pull requests at once, 17 to 33 pushes an hour. On 2026-11-07 at 21:19 UTC, 40 runs were queued with 3 in progress, so a push waited hours for its result.

Most of those minutes test what the push didn't change. A pull request that changes only the panel ran the race-detector Go tests, the drivers on real Docker and Caddy, and the service end to end on Docker, though none of them reads `web/`. A docs change ran all 25. The install script ran in eight containers and under systemd for every change, a docs change too.

CI is the gate: nothing merges until it's green. So picking fewer jobs mustn't let a change through that the full set would stop.

## Decision

### Each job asks whether it's needed

Every gated CI job starts, as before, when the pull request is pushed. Its first step after the checkout runs `scripts/ci-jobs.sh needs <job>`, and its other steps run only when the answer is yes. A job that isn't needed passes in about 20 seconds. Licenses and links has no question to ask, and always runs.

The question is about where the pull request's merge commit differs from main, which the step fetches, at depth 1 when the checkout is shallow. That's main, not the pull request's base: a pull request stacked on another one, or retargeted to main later, which `pull_request` doesn't run again for, is compared with what it would change on main. When main has moved since the merge commit was made, the difference lists more files than the pull request changed, never fewer. When main can't be fetched, or the run isn't for a pull request, every job is needed.

In Install, Release snapshot asks for itself and for the jobs after it, which already wait for it, and says which install legs they run. Memory budgets and the install legs run only when it says they're needed, and Install, every job holds them to what it picked. Build of main runs on every pull request, as it checks out main, not the change, and it takes about a minute.

A job that waits for another doesn't start when that one ends: it goes to the back of the queue. On 2026-11-07, Install's legs started 4 to 23 minutes after the Release snapshot they wait for had finished. So a job that picks the jobs, with every other job waiting for it, would make each push wait for a runner twice, and a check after them all, three times. With 17 to 33 pushes an hour against the 12 to 15 the runners can take, the queue stays long, and that would cost more than the minutes it saves. A job that asks for itself waits once, as before, and costs a runner for about 20 seconds when it's not needed.

The picker maps each path to the jobs that read it, and its rules are written from what each job reads, not from what usually changes together:

| A change to | Runs |
| --- | --- |
| Docs, `assets/`, Markdown at the top but the README | Licenses and links only |
| `LICENSE`, `README.md`, which the release archives carry | Release snapshot |
| `web/`, `packages/` but the mock viewer, the panel's package files, the design system's fonts | API schema, Panel, Panel end to end, Panel end to end on Docker |
| The rest of `design/`, `packages/mockview/` | API schema, Panel |
| A catalog entry | Go, Service end to end on Docker, Catalog on Docker, Panel end to end |
| A Go test, `*_test.go` | Go, Go on Docker, Go on Caddy |
| Other Go code, anywhere, `proto/`, `gen/`, most of `scripts/` | Every CI job, the Memory budgets, and three install legs |
| `go.mod`, `go.sum`, `tools/`, which build the release | The same, with every install leg |
| `scripts/install.sh` | Go, Hundreds of containers, Service end to end on Docker, the budgets, and every install leg |
| `scripts/connected.awk`, which runs inside the legs | As Go code, with every install leg |
| `scripts/install-test/`, the trust root | Every install leg |
| `.goreleaser.yaml` | The budgets and every install leg |
| A workflow, the Makefile, the picker, or a path no rule knows | Every job |

These choices are judgements:

- **The budgets don't run for the panel.** Only the full build embeds the panel, which reads each file once at start to hash it, so a panel change moves the control plane's idle RSS by about its size in file-backed pages. The Panel job holds that size instead: `scripts/check-panel-size.sh` fails above 3 MB. Its initial 2.5 MB limit allowed about half as much again as main's 1.67 MB on 2026-11-07. On 2026-11-08, the terminal's 392 KB xterm, fitting and search chunk receives a further 0.5 MB allowance; the complete build is 2,548,822 bytes. The control plane's separate 250 MB idle RSS budget still applies.
- **Go code runs three install legs, not eight and two.** The install script runs the binary's `run`, `join`, `token`, `claim` and `role`, which reach some 40 packages: the control plane, enrolment, the store and its migrations, the keys, and what reads `/etc/os-release` and the architecture. The legs are the only place a pull request runs an arm64 build, Debian, AlmaLinux and Alpine, OpenRC and systemd, and an upgrade, and a hand-kept list of the packages they reach would go stale. So any Go change runs Alpine on arm64, for busybox, musl and OpenRC; AlmaLinux on amd64, for an RPM distribution; and systemd on arm64. That's 3 jobs and about 3.3 runner-minutes on each Go push, against 10 and about 10.6 for every leg. Every leg runs for the install script, its tests, the release, and on main.
- **A Go test runs no install legs and no budgets.** A test isn't in either build.
- **A catalog entry runs no budgets.** The full build embeds every entry, and parses them all on the first call to the catalog's API, not at start, so an idle control plane holds none of them, and one that has served the catalog holds a few kilobytes an entry, against a 250 MB budget. The agent build doesn't link the catalog.

A push to main, a nightly run and a manual run pick every job, so main is tested in full after each merge, and a release is only ever tagged on a commit that was.

`scripts/ci-jobs-test.sh` holds the picker to examples of each kind of change, and Licenses and links runs it on every change. A rule that changes without its example fails the pull request that changed it.

### What a branch rule requires

A CI job that isn't needed still runs and passes, so it's never skipped, and each one can be required by name: a failed question fails its job. Install's legs are a matrix, named after their images, which legs run depends on the change, and they're skipped as a whole when they're not needed. A branch rule counts a skipped job as passing, and can't name a matrix leg that may not run. So Install ends with Install, every job, which runs whatever happened before it. It fails unless every job Release snapshot picked passed and every other one passed or was skipped, and it runs, and fails, when the rest were cancelled, so a cancelled run never reads as green. Build of main may fail, as it only adds a comparison.

As it runs whatever happened, Install, every job takes a runner for a few seconds even on a run that a newer push cancelled. That's one job per superseded Install run, the price of a cancelled run never reading as green.

The checks a branch rule on main requires are every CI job by name and Install, every job.

## Consequences

- Over the last 40 pull requests merged before this, a push uses about 81 runner-minutes instead of 103. Most of them changed Go code, which still runs nearly everything. A panel change uses about 30 minutes, a Go test about 35, a catalog entry about 57, and a docs change about 6.
- A push still starts the 11 CI jobs, as each one asks for itself. A CI job that isn't needed reads as passed, and its log and summary say it wasn't needed and why.
- A new kind of file runs every job until it has a rule. A rule that's too narrow would let a change through untested, so a new rule needs an example in `scripts/ci-jobs-test.sh`, and main still runs everything after it merges.
- Each CI job and Release snapshot fetches main to compare with, which takes a few seconds: at depth 1 from a shallow checkout, and in full in API schema, whose checkout is full, as the breaking-change check reads main's history.
- A new CI job needs a rule in the picker, the question as its first step after the checkout, `if: steps.pick.outputs.run == 'true'` on each step after that, and a place in the branch rule. A step without the `if:` runs anyway, which is slow but never lets anything through.
- The footprint comment stays as it was for the last push that measured memory, which it names, when a later push doesn't change Go code.
- Go changes still cost about 96 runner-minutes, the three install legs included. The Go test cache, restored from main, would let the Go job skip packages a change can't reach, and Go on Caddy could skip changes outside what `internal/ingress` imports. The repository's Actions cache is at its 10 GB limit, so the first needs room made for it.
