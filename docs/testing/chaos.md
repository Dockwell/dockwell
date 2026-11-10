# Chaos tests

The chaos tests prove what [ADR 0054](../adr/0054-the-agent-keeps-its-desired-state.md) promises, and the PRD's phase 1 gate: when the control plane is lost, workloads keep running, and agents reconcile to the last desired state (#35). Each scenario breaks one thing at a bad moment, puts it right, and checks the same invariants. They run in CI's Chaos on Docker job, on a real Docker Engine, and `make chaos` runs them on any machine.

## The stand

One stand serves every scenario, so a scenario costs only its fault:

- **The control server:** the full build, `dockwell run`, with its data in a fresh folder. Its own agent runs in it and holds nothing of the scenarios'.
- **One server, `chaos-1`:** the agent build, joined to it, with its data on a small disk of its own (a tmpfs of 16 MiB in CI). It reaches the control plane through a TCP proxy the harness holds (`--advertise-url`), so the harness can cut that connection without touching either side.
- **What `chaos-1` runs,** in one project:
  - `web`, which serves HTTP on a published port, restarting unless stopped. The harness asks it for a page every 100 ms for the whole run.
  - `slow`, which takes about 8 s to turn healthy, so a deploy of it is a change in flight long enough to break.
  - `spare`, on a published port of its own, with restart `always`, for Docker to fail to start after a daemon restart.
  - `held`, which a person stopped with `docker stop` before the first scenario, and which has to stay stopped.
- **The agent looks every 2 s, not every 60 s.** `DOCKWELL_TEST_AWAY=2s` sets both how long it waits without a stream before it acts and how often it looks then, and only under `DOCKWELL_TEST`, as `DOCKWELL_TEST_PACKAGES` is. Nothing else about the agent or the control plane is changed for the tests.

## The faults

| Fault | How | Put right by |
|---|---|---|
| Kill the agent | `SIGKILL` | starting it again |
| Kill the control plane | `SIGKILL` the control server, which takes its own agent | starting it again |
| Kill Docker | `systemctl kill --signal=SIGKILL docker` | `systemctl start docker` |
| Cut the connection | the proxy drops every byte both ways and closes nothing, as a network that loses packets does | the proxy passing bytes again |
| Fail to keep an answer | a full disk prevents the agent from saving the hand-back acknowledgement | freeing the disk and restarting the agent, which sends the same receipts again |
| Fill the disk | a file that takes all of `chaos-1`'s data disk | removing the file |
| Jump the clock | `date` two hours ahead, with time sync off | `date` back to the real time, and the original time-sync setting restored |
| Corrupt the cache | with the agent stopped, one byte of a record changed, and another record cut short | nothing: the control plane fills them again |

Killing Docker, filling the disk and jumping the clock need `CHAOS_HOST_FAULTS=1` and root through `sudo -n`. Enable them on a disposable host, as CI does. Other runs skip those scenarios. `CHAOS_REQUIRE_ALL=1` makes any selected scenario skipped a failure, so CI cannot pass without exercising every fault.

## When it breaks

- **Mid-change:** a deploy of `slow` to a new image, broken once its new container has started and before it turns healthy, while the step is running on the agent. The harness knows that moment by the change still applying while its new container runs. Docker's own health state says nothing here: an HTTP check is the agent's, kept in a label.
- **Mid-reconcile:** with `chaos-1` away and `web`'s container removed, broken while the agent puts it back. The stand is away by a cut connection, so the control plane stays up and can be asked what it sees, except in the scenario that kills it.

## The invariants

Every scenario checks these once its fault is put right, with up to 60 s to settle and two consecutive checks a second apart. Automatic rollbacks must have ended too. It then reads the stopped agent's records, restarts it and checks the invariants again, all within the scenario's four-minute deadline.

1. **Nothing running is removed or replaced.** Each container that was running as the scenario began is running at its end with the same ID, unless the scenario removed it, when one container stands in its place, made from its record, or the scenario's change replaced it, when the change's container stands there. Docker starting a container again after it was killed isn't a replacement: the ID stays.
2. **No change ends applied with a server holding part of it.** Each change the scenario made ends applied or failed, never left applying. An applied change's resource on `chaos-1` runs the image the store calls live, labelled with the change and its revision. After a failed one, each resource runs the store's live image. A rollback to what already runs can have no step, leaving an older revision label, so the failed-change check compares images.
3. **Nothing a person stopped is started.** `held` is still exited, with the time it stopped unchanged.
4. **Every reconcile reaches Activity once.** Each entry the agent wrote in its journal, read from the journal before the hand-back, is one `server.reconciled_*` event, and no entry is two. A count of dropped entries is one `server.reconciled_dropped`, however often it's sent.
5. **The store and the agent agree once it's back.** `chaos-1` is connected, `drift list` says nothing of it, and each resource runs the store's live image. At every scenario's end, the harness stops the agent and reads its records with its key: each has that image and a revision no higher than the store's, and there are no extra records. A record can lag after a rollback with no step; the next fill brings it up. After the agent reconnects, the invariants are checked again, including whether it sent a receipt twice.

And as #35 asks, **traffic keeps flowing**: every request to `web` is answered, but while Docker is down and while `web`'s container is gone on purpose.

## The scenarios

| Scenario | When | Fault | What it shows beyond the invariants |
|---|---|---|---|
| `change/kill-agent` | mid-change | kill the agent, start it 3 s later | a step cut short is resumed or failed by the control plane, not half kept |
| `change/kill-control` | mid-change | kill the control plane, start it 3 s later | an applying change survives its control plane, and the agent finishes what it was doing |
| `change/kill-docker` | mid-change | kill Docker, start it 3 s later | a step whose engine goes away ends, applied or failed, and the store says which |
| `change/cut` | mid-change | cut for 40 s | longer than either side's 30 s of silence, so both see the stream gone |
| `change/full-disk` | mid-change | fill the disk, free it 10 s later | a step whose record can't be kept says so, and the control plane sends it again |
| `change/clock` | mid-change | the clock 2 h ahead, back 10 s later | timers run on the monotonic clock, and certificates still verify |
| `away/baseline` | mid-reconcile | none | slice 2 end to end: `web` made again, `held` left, the hand-back on Activity once |
| `away/kill-agent` | mid-reconcile | kill the agent as it makes `web` again, start it | no second container, and the journal's entry once |
| `away/kill-control` | mid-reconcile | the control plane killed rather than cut, and started | a control plane that restarted takes the hand-back and fills the cache |
| `away/silent-cut` | mid-reconcile | the proxy drops every byte and closes nothing | the HTTP/2 silence check detects the cut, and the agent acts after losing its stream |
| `away/kill-docker` | mid-reconcile | stop `spare`, hold its port and kill Docker; its `always` policy tries it when Docker returns, then the port is freed | Docker must leave `State.Error`, and the agent starts it; the scenario fails if that failed-start condition wasn't reached |
| `away/full-disk` | mid-reconcile | fill the disk while away, remove `web`, then free the disk | Docker is left alone until the agent can keep its pending action durably, then `web` is made again and its receipt reaches Activity |
| `away/answer-unkept` | mid-reconcile | the full disk prevents saving the hand-back acknowledgement, then the agent is killed | an entry sent twice is on Activity once |
| `away/clock` | mid-reconcile | the clock 2 h ahead while away, back before the hand-back, whose acknowledgement can't be kept | an entry from ahead, sent twice, is on Activity once |
| `cache/corrupt` | agent stopped | corrupt the cache | records that don't open are put aside and filled again; nothing on the server changes |

The slice 3 proof ADR 0054 describes is `away/kill-control` and `away/kill-docker` together: the control server stopped, a container removed and one that can't start, the agent restarted, everything back, and Activity saying so.

## Keeping it short

- A scenario has a four-minute deadline, including its fault, recovery and checks. Once the fault is put right, it has at most 60 s for the invariants to hold. The cuts take longest, as detecting silence takes about 30 s.
- The stand is made once, about a minute with its images, and each scenario starts from where the last one left it, with the invariants as its starting state.
- After a scenario fails, the harness starts whichever of `web`, `slow` and `spare` it left stopped, and says so, so one failure isn't counted again against each scenario after it. The failed scenario stays failed.
- The pull-request job has a 30-minute limit; the nightly job runs three rounds with a 60-minute limit.

## Where it runs

- **On a pull request,** in the Chaos on Docker job, when it changes the agent, the control plane, the store, the resource drivers, the harness or the job ([ADR 0050](../adr/0050-ci-runs-what-a-change-needs.md)).
- **On every push to main,** as every job does, and **nightly,** where each scenario runs three times so a rare race shows.
- **Before a release:** `scripts/require-green.sh` waits for it, so a tag on a commit where it failed isn't built.
- **On your machine:** `make chaos` builds both binaries and runs every scenario it can. With Docker, it uses Docker; `CHAOS_FAKE_DOCKER=1 make chaos` runs the eight scenarios that need neither Docker nor root on the fake Engine, without the traffic check. `CHAOS_ONLY=away/` selects scenario names by prefix, and `CHAOS_ROUNDS=3` repeats them three times.
- **After a failure:** the stand's folder is retained, with the process logs and a JSON description of containers, live revisions, Activity and drift. CI uploads only these files, leaving keys, cookies, passkeys and desired records out. `CHAOS_KEEP=1` retains successful stands too.

## What Docker leaves behind: the evidence

The job's first step, `scripts/docker-stops-evidence.sh`, judges nothing. It stops containers each way a person can, kills the engine, and prints each container's state, restart count and events, so what [ADR 0054](../adr/0054-the-agent-keeps-its-desired-state.md) rests on is in a log and not in someone's memory of Docker. On Docker 28.0.4, without `live-restore` (run 42762502373):

- **A person's stop.** `docker stop`, `docker kill` and `docker compose stop` each leave `kill` (and `stop`) then `die`, after the container's `start`. The state reads exited, exit 0 or 137, no error.
- **A killed engine.** `systemctl kill --signal=SIGKILL docker.service` leaves the containers running under containerd. The engine systemd starts again stops every one of them, saying `kill`, `stop` and `die` of each, and marks each stopped by hand on disk. `always` comes back, a stopped one too. `unless-stopped`, `on-failure` and `no` stay down, and read exited, exit 0, no error: the same as after a person's stop.
- **What tells them apart.** The returned engine holds no event from before its death, so the stops it made have no `start` before them. Its `docker info` is the same as before: nothing there tells one run of the engine from the next.
- **How far back events reach.** Of about 450 events made in 6 s, the engine replayed 258: it keeps its last 256.
- **A clean restart** brings `unless-stopped` back by itself.

Still to be printed by a run, as the script asks and no run has answered yet: whether a `docker stop` of a container Docker couldn't start leaves the error in its state; and what the list says of a container between `docker rm -f`'s kill and its going (`removing`, `exited` or nothing), which the agent's rule for a forced removal took from Docker's source.

## What it has found

- **A killed Docker engine left every app whose policy isn't `always` down, and the agent left them too.** It read "not running, no error" as stopped by someone. The agent now settles why a container is down from Docker's events, starts what Docker stopped as it came back, with the control plane there or not, and leaves and reports what it can't place (ADR 0054). `away/kill-docker` and `change/kill-docker` are its proof on a real engine.
- **A container the agent took for stopped that nobody had stopped.** In the second run `away/clock` failed: the agent said of `web`, removed with `docker rm -f`, "It was stopped, and is gone", and didn't make it again. The log can't say which look set that mark; the likeliest is one that landed between the kill and the removal. A stop is now settled only after two lists, and only from what Docker says happened. `cache/corrupt` failed after it only because `web` was still gone: the harness starts what a failed scenario left stopped, and doesn't make again what it left gone.
- **A write that failed on a full disk read "rename: no such file or directory".** A shadowed error in the cache's temporary-file writer; fixed with #357.
- **Harness faults, put right:** it waited for a Docker health state that never comes, as an HTTP check is the agent's own; it counted a failed scenario's stopped containers against the next ones; and it compared a reason letter for letter, where the control plane keeps it with single spaces (`away/full-disk`, second run).
