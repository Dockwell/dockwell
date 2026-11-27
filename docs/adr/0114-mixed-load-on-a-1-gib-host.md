# 0114. Measure mixed load on a 1 GiB host

- Status: Accepted
- Date: 2026-11-27

## Decision

`make small-host` starts `dockwell run`, with its control plane and embedded
agent in one process, real Caddy and the test Docker Engine. Three HTTP
stand-ins hold 312 MiB. A separate 160 MiB hold represents the OS and Docker
daemon. The Engine checks out Git and publishes an image through Dockwell's
build path while a stand-in holds 128 MiB for the builder. Measurements
represent these fixtures and the Dockwell paths they exercise.

After a 60-second latency baseline, Caddy serves three minutes of traffic
while the panel makes Connect reads and follows and searches logs. The agent
builds an image, handles four file uploads, copies a volume and dumps a
PostgreSQL database. After the admitted build, a second build waits a minute
under extra pressure and must record a clean refusal before launch.
[ADR 0046](0046-builds-from-git.md) describes the memory admission policy.

Both modes gate sampled process-tree PSS at 1 GiB and Dockwell peak RSS at
250 MB. Any swap or app restart fails the run. Each app's p99 under load must
stay within its baseline plus 25 ms or three times its baseline, whichever
is higher. The allowance accounts for short scheduling delays on shared runners.

On a runner with a writable cgroup v2 scope, systemd also enforces
`MemoryMax=1G` and `MemorySwapMax=0` across the entire stand. The run records the
kernel's peak with reclaim and OOM counters. A memory charge reaching
the limit can reclaim cache and continue. The OOM gate allows one confirmed
kill of the builder stand-in, which runs with OOM score adjustment 1000.
Every other stand process must survive, and any further OOM fails the run.
Build admission reads the real scope shared by the fake Engine. The measuring-only mode feeds the
admission policy a synthetic meminfo derived from sampled anonymous and shmem
PSS. The sampled gate omits unmapped cache and kernel charges, and scope OOM
counts are unavailable. The job summary names the mode and CI keeps the JSON
as the `small-host` artifact.

## Measurements

The first local stand, before the 160 MiB host hold and the new admission policy,
peaked at 768.4, 789.7 and 791.8 MiB of sampled PSS across three runs.

With the host hold, two local measuring runs passed. Both completed all work
and reported zero swap and zero app restarts.

| Run | Process-tree peak (MiB) | Dockwell peak / settled RSS (MiB) | Build wait | Refusal wait and available memory |
| --- | ---: | ---: | ---: | ---: |
| 1 | 969.1 | 124.9 / 106.4 | 0.039 s | 60.04 s at 177 MiB |
| 2 | 968.8 | 127.0 / 107.2 | 0.103 s | 60.05 s at 176 MiB |

The admission check refused after 60 seconds in both pressure runs. Those
refusals used harness meminfo. A cgroup run would additionally test the real
scope's headroom, cache and kernel accounting. The local environment's cgroup
mount was read-only.
