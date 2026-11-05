# 0027. Health checks, rolling replacement and automatic rollback

- Status: Accepted. It changes what [0021](0021-the-docker-driver.md) says of a service's one container, corrects what [0024](0024-the-caddy-ingress-driver.md) says of reloads, and changes how a route dials its upstream.
- Date: 2026-11-05

## Context

Issue #28 decides whether a deploy can be trusted. A service has to go from one version to the next with no request dropped, survive a crash at any step, and come back on its own when the new version doesn't work, saying why.

Earlier records left it these seams:
- [ADR 0009](0009-resources-revisions-and-plans.md): every change is a revision with a plan, and the plan states its downtime and what it destroys.
- [ADR 0017](0017-the-change-api-and-the-agent-stream.md): a change left applying resumes when the control plane starts, and a dropped stream fails the change and commits its rollback, which waits for the server.
- [ADR 0021](0021-the-docker-driver.md): a service runs under Compose's names and labels, and a stop-first replace swaps a new container in under the name. It left rolling deploys and replicas to this record.
- [ADR 0024](0024-the-caddy-ingress-driver.md): `ingress.Upstreams`, and three things to settle: an instance's number, how long a drain waits, and what counts as healthy.

## Decision

### Three kinds of check, and what each means

- **A command** is Docker's, as Compose writes it, and stays as it was: the container's own check, or the image's. Docker runs it inside the container.
- **HTTP and TCP** are new, and the agent makes them, because a `FROM scratch` image has no `curl` for Docker to run. In the spec they're `healthcheck.http` and `healthcheck.tcp`, and in a Compose file `x-dockwell.healthcheck`, which `docker compose` reads past, so a file means the same to it with or without.

The agent keeps its check on the container's label `dev.dockwell.check`, and turns Docker's off with `Test: ["NONE"]`, so an image's own check doesn't run too and disagree.

An HTTP or TCP check goes only to the container, at the address Docker reports for it, on the ingress network if it has one. **The spec can't name another host,** as the `host` field is only the `Host` header. Redirects aren't followed, proxy settings in the environment are ignored, the body isn't read, and no connection is kept. So a write token can't use a check to reach something else from where the agent sits, or read an answer from it ([threat model](../security/health-checks.md)).

**A check the agent can't make is refused at plan time.** On rootless Docker, or on a macvlan, ipvlan or overlay network, the server can't reach a container's address, whether the network is the environment's or one the server already has. The refusal says to use a command check. A network the same change is still to make can't be asked about, so there the check's own failure says it. A service on the server's own network has no address of its own, so the agent couldn't tell it from whatever else listens there, and its check is refused as the spec is read.

**A passing check isn't a port that answers.** A command check runs inside the container, where localhost works, and an agent check can name a port of its own, so neither says anything of the port a route sends to. So a rollout also asks each route's port to take a connection, after any check that passes, before it moves anything, and fails if it doesn't, saying so. With no check at all, that and staying up are all there is, and the error says that a slow starter needs a check with a `start_period`, as the wait for a port is only 2 min.

### What a state means

`GET /v1/health` (`HealthService.ListHealth`) says how each service is, from what each server's agent last said.

| State | Means |
| --- | --- |
| `HEALTHY` | A check ran and passed. Nothing else reads as this |
| `NO_CHECK` | It runs, and nothing checks it. The panel says "Running", and never "Healthy" |
| `STARTING` | A check is set, and none has finished, or it is within its start period |
| `UNHEALTHY` | The check failed as often as it may |
| `NOT_RUNNING` | The server said, and the service has no running container there |
| `UNKNOWN` | Nobody can say: the agent hasn't read Docker yet, isn't connected, can't read Docker now, or what it said is too old to stand. What it last said comes with `stale` |

A service on a server is as well as its best container, as two exist while one takes over, and what serves is the one that passed. A service across servers is as well as its worst server.

The result carries what the check said, cut to one line of 240 bytes with control and bidirectional characters gone, each value of 6 characters or more that the container's spec set masked, and passwords in URLs masked. What the image's own `ENV` sets isn't masked, as anyone with the image has it, and masking it takes words such as `production` out of what a check says; a container that doesn't say what its spec set has all of it masked. That reaches anyone with the read scope, where a log needs a grant of its own, so it says so and it isn't a promise it holds no secret. The control plane cleans it again and holds it to 4,096 containers, as an agent is trusted with its own server and nothing else. For the same reason a server's report only reaches the timeline for a service placed on it.

Checks go on after a deploy, and a claim only stands for as long as someone is looking:
- **Scanning and checking are apart.** A Docker that hangs, as a stuck storage driver makes it, doesn't stop the checks the agent makes, which need nothing of it. Each call to Docker has 10 s. A check that ends wakes the loop, so an interval shorter than the scan is kept.
- **A list says where each container is,** so a check goes where the container is now, and never to an address another container has since taken. Each scan also reads 8 containers in full, those read longest ago, so it gets round them all.
- **Hung checks can't hold every worker.** At most 16 checks run at once, and what took a second or more last time gets at most half of them, so a check that would say a service has died gets its turn. Otherwise 160 checks hanging for 60 s would hold one up for about 10 min.
- **Each state says when it was last known, and how long that may stand** (`confirmed_at`, `good_for`). That is the last check the agent made, or the last list it read, and at least 90 s. Past that, or when a list fails, what only Docker can say reads `UNKNOWN`. The control plane works out a state's age from the server's own clock and its own, so they needn't agree, and a report older than 90 s reads `UNKNOWN` whatever it says, as stats keep a stream open whatever the monitor is doing. A report comes at least every 30 s, whether or not anything changed.
- **Having read nothing isn't that nothing runs.** An agent sends no report until it has read Docker, so the API says `UNKNOWN`, not `NOT_RUNNING`. Reading and finding nothing does say that.

It watches at most 4,096 containers, and a scan takes at least 10 ms for each. With Docker down it looks every 5 s to 30 s. 3,000 services whose checks all hang stay within the 16 workers and a bounded heap (`internal/health`).

**The timeline hears a change of state, not each check.** A state must last 30 s (`Dwell`) to be told, as a stop-first deploy's seconds are the change's to tell, and two events of a service on a server are at least 5 min apart (`HoldOff`). A service that flaps is told at that pace, with how many times it changed in between, and one that flaps faster than 30 s, which no state would last long enough to be told, is told as what it keeps falling to once it has gone back and forth 3 times. Not knowing isn't a flip. A service that has always been fine has no event. After a restart the recorder reads the last event, so it neither repeats a state nor misses one that ended while it was away. It needs no new table: an event is an audit row, action `service.health`.

### Numbers and names

A service's containers are found by Compose's labels, and numbered as Compose numbers them.

- **A rolling replace takes the next number, and it only goes up,** as `nextContainerNumber` does. A number whose name someone else holds is skipped. `<project>-web-1` becomes `<project>-web-2`.
- **A stop-first replace keeps its number,** as Compose does when it recreates, through a temporary name, `<id12>_<name>`.
- **One-offs aren't the service's.** `oneoff=True` is left alone, and takes no number.
- **What reads the service reads its newest.** So `docker compose ps`, a plain `docker compose up`, which takes what's there and keeps its number if it recreates it, and the next rollout all agree. `docker compose up --scale` is still not drift, and the next step that changes anything removes the extra containers, and its plan names them.

### One way forward, whatever it finds

`Run` moves a service from the containers it has to one that matches its spec. A deploy, a rollout that a crash stopped at any step, and a rollback are the same code with less to do. `keep` is the newest instance that matches, and everything else goes.

The steps of a rolling replace, each safe to find done:

| Point | What is true |
| --- | --- |
| `reserved` | The new instance has an address |
| `created` | It exists, and hasn't started |
| `started` | It runs, and isn't known to be healthy |
| `healthy` | It passed its check, and has no traffic |
| `upstream` | It has the traffic, and the old one has what is in flight |
| `drained` | What the old one had is done, or its time ran out |
| `marked` | The old one reads as a rollout's to stop, and may not have been told |
| `stopped` | The old one has stopped |
| `removed` | The old one is gone, and still has an address |
| `forgotten` | Its address is free, and nothing is left to do |

A stop-first replace goes through `swapready`, `swapmarked`, `swapstopped`, `swapgone`, `swapnamed` and `started`.

**A stop a rollout made is marked as one.** Docker goes on stopping a container whoever asked has gone ("Cancelling the request should not cancel the stop", moby's `daemon/stop.go`), so an agent whose stream drops mid-stop can't know how that ends, and a stopped container that matches its spec otherwise reads as one a person stopped, which isn't drift (ADR 0021). So before it asks, a rollout renames the container `stopping_<id>_<name>`. Docker keeps a name through anything, a `SIGKILL` of the agent included, and a label can't change. What finds that name knows two things: it isn't a person's doing, and Docker may yet stop it. To bring one back, a rollout asks Docker to stop it again, which returns once it has, starts it, and only then gives it back its name, so what dies on the way reads the same. A stop-first step that can't go on after it began to stop what ran puts that back itself, on its own time, as nothing may follow for a while. It says if it can't, and what to do, and the next change finishes it. A container someone stopped by hand has no such name, and stays stopped.

**What a plan finds left.** `resource.Settler` asks what is left when the specs are equal: a leftover instance, a temporary name, a container that never started, or a restart not yet carried. Without it, an agent killed between a rename and a start would leave a stopped container while the plan said there was nothing to do, which the kill test below found.

**What a service uses.** A plan has none of a service's variables and secrets ([ADR 0022](0022-secrets-encrypted-at-rest.md)), as only a step's call brings their values. So a plan reads a container by its `dockwell.uses` label, which names each and its revision, and a step, which has them, compares the values too. A new revision of a secret rolls like any other change.

**Where a crash ends.** Two different things can be cut short, and they end differently. These come from ADR 0017, and this record holds them for rollouts:
- **The control plane stops,** or the whole server does: the change stays applying, and the next start finishes it, forward.
- **The agent's stream drops, or the agent stops,** with the control plane still up: the step may or may not have run, so the change fails as disconnected, and its rollback commits at once and waits for the server. An agent that stops sends nothing for a step that ends in error as its stream ends, so a stop reads as this, rather than as the step having failed for a reason of its own. A restart has no rollback to wait for it, so it waits for the server itself, and finishes, once.

Either way it ends in one healthy instance and no orphan. `internal/fault` had no injection, so this adds `fault.Points`, and the kill test re-executes the test binary as an agent that kills itself with `SIGKILL` at each point, forward, rolling back and after a reboot.

### What can't roll says so

`decide` looks at what the two containers would share. Any of these stop it rolling, and the plan names them, with the downtime:
- a server port both would publish;
- a volume both would mount, and either write to;
- a server path both would mount, and either write to;
- what the image keeps in a `VOLUME`, which moves to the new container;
- anything the agent can't read the real place of;
- a route edited by hand that sends to the service;
- the server's own network, for the old container or the new one.

**It compares where each mount really lands,** so one directory under two names is one directory: a volume that binds `/srv/db` and the path `/srv/db`, two volumes binding it, or a link to it. A volume Docker keeps itself reaches nothing else. A volume on another driver or over a network, and a path that can't be followed, could be anything: the plan says it can't tell, and stops first.

**A route edited by hand keeps what it dials.** Dockwell doesn't move it, so a rolling replace would leave it dialing what it then removes. A stop-first replace keeps the instance's number, and so its address, so it goes on reaching the service.

**The server's own network has room for one.** A container on it listens on the server's ports itself, so a second can't bind them, or one that shares them lets a check read the old container and call the new one healthy. That holds moving off it too, as the new container may publish what the old one still holds. So it stops first, either way, and `start-first` on it is refused as the spec is read. A route can't send to a service there, as it joins no other network, so that is refused at plan time too.

**A tag that moved is a new image.** A deploy of the tag a service already runs asks the server for it again ([ADR 0021](0021-the-docker-driver.md)). A container of the old image still reads as the spec, which only says the tag, so the step compares what the tag names now with what each container runs, and takes the next number, or stops first, as any new image does. The plan is for that, as it can't know yet. With nothing newer, nothing happens. A run stopped partway, with a container of the new image and the old one still there, isn't done, however it reads, and goes on from there.

**A volume is exclusive by default.** Two writers on a database's files, even for seconds, can corrupt it, and Dockwell can't tell a database from a directory of uploads. So the safe default has a few seconds of downtime, stated in the plan, rather than a risk nothing states.

**`rollout.order` chooses otherwise,** with `start-first` or `stop-first`, or `deploy.update_config.order` in a Compose file. Choosing `start-first` over shared data rolls, the plan says that two containers may write to what it names, and **a person has to approve it,** as a plan that destroys data does: only they can say whether this data can take it, so a token can't choose it. `start-first` with a published port is refused, as Docker would refuse it later, when the old one is still serving.

**A mount that goes says so.** What the old container mounts and the new one doesn't stays on the server, and the service no longer sees it, which a change to its mounts doesn't say on its own. Where the new image declares a `VOLUME` there, an empty one takes its place, and only the plan can say that, so it does, plainly. It doesn't need a person: nothing is destroyed, and undoing it brings it back.

**Other services can reach a new container early, and this can't be fixed.** A service's names go on a container as it joins a network: its service's name, and any others its spec gives it, such as a Compose file's `container_name`. Docker's resolver answers them for every running container that has them, whatever its health. So while a new one starts, including one that goes on to fail, other services on that network can reach it by name beside the old one. On a network the server already has, that includes other projects' containers. Docker has no way to add a name to a container that has joined, and joining again resets its interface, which cuts what the new one has open on that network: its database, for one. So the plan says so, naming the networks and the names. Traffic through the ingress isn't affected.

A plan's fingerprint covers this, so a plan reviewed as having no downtime can't apply as one that has it. If what's on the server has changed since, so that a rolling step would now share what it mustn't, it stops rather than share, and says to plan the change again.

From Compose it reads `stop_grace_period` and `deploy.update_config` (`order`, `parallelism` of 1, `failure_action: rollback`), as each means something Dockwell does. `x-dockwell.drain` is the drain, 30 s by default. Anything else in `deploy` is refused by name.

### Traffic moves with no reload

A reload costs every site on a server. Go's HTTP server closes an idle HTTP/1.1 connection as it shuts down, with no way to tell the client first, so a POST sent on it at that moment is lost, and no client sends a POST again on its own. Measured on Caddy v2.11.7 across 10 rollouts that each reloaded once, under load: 9 of 49,917 POSTs on kept HTTP/1.1 connections failed, about 1 a reload, with `EOF` or "server closed idle connection". HTTP/2 and HTTP/3 lost none, as they can say a connection is going away. A site that isn't being deployed shouldn't pay for one that is.

**So a rollout doesn't reload.** A route that sends to one upstream dials `{file./run/dockwell/upstreams/<route>}`, which Caddy reads for each request (`hosts.go`, `fillDialInfo`). Moving traffic is writing that file, whole or not at all, in the same way as anything the agent writes in Caddy's directories. Measured the same way, with that change alone: 0 of 86,592 POSTs on kept HTTP/1.1 connections failed, and no reload, or new connection cut on its way in. What else was looked at:

| Way | Why not |
| --- | --- |
| `shutdown_delay`, so an old config goes on answering for a while | Caddy honours it only when a listener is really closing (`app.go`, `stop`). A reload keeps them open |
| `dynamic_upstreams`, with a small DNS server in the agent | A failed lookup fails the request (`upstreams.go`, `AUpstreams.GetUpstreams`), so an agent that stops would take every site with it, and the admin API doesn't count them |
| Upstreams with health checks, each with a slot of its own | A new container would take traffic as soon as it answered, not when it has held for its soak, and a slot with nothing there costs a timeout for each request |
| Swapping an address, by network or by the kernel's NAT rules | Taking an address off the old container cuts what it has in flight, and NAT rules are a lot to keep for this, and a rootless agent can't |
| Telling connections to close, then reloading | Busy clients come back on new connections just as the reload resets those on their way in, so it moves the loss |

What it changes:
- **A drain counts connections.** Caddy counts requests by what a config says to dial, and that is now every container a route has ever sent to. So a drain reads `/proc/net/tcp`, on the host's network, which is Caddy's and the agent's, for connections open to the old address. Caddy closes a connection to an upstream with nothing on it within 2 s (`keep_alive.idle_timeout`), so what's left is what's on its way, and a drain takes at least that long when anything had been sent lately. It needs two clear reads a poll apart, as a request's connection may open a moment after the file changes. If it can't read that, it gets all its time rather than call it clear. It costs an app a new connection after 2 s with no requests, over the server's own bridge.
- **A rollout needs nothing of Caddy.** With Caddy down, or another proxy on its ports, the file is written, the deploy goes on, and Caddy dials the new container when it is back. `IngressStatus` says that it is down, and a rollout doesn't assume otherwise.
- **What a file says is read with suspicion.** It is in Caddy's directory. What isn't an address and a port, or isn't a regular file, reads as a hand edit, as drift, and a revert writes it again. Caddy could dial anything anyway.
- **A route with several upstreams dials them as they are,** and changing one reloads. Nothing makes one yet: that is the seam for replicas.
- **A move changes what a route dials, and nothing else of it.** A route with no certificate stays on plain HTTP, and who it answers doesn't widen, through the move, what holds a service while it rolls, and what settles it after ([ADR 0030](0030-routes-behind-a-tunnel-or-a-proxy.md)).
- **A route to a port on the server belongs to no service,** so nothing moves it, drains it or holds it. If that port is one a Dockwell service publishes, the service stops first, as anything publishing a port does, and the route answers 502 for as long as the plan's downtime says.

What remains of the reloads, for what still changes a config, such as adding a route, is as it was:
- **A config knows it is old.** Go's HTTP/2 server sends a GOAWAY only to connections it has registered when it shuts down, and it registers one after its TLS handshake, so a connection mid-handshake at a reload stays on the old config. So each config has a first route on each of its servers, `dockwell-generation` and on the plain HTTP server `dockwell-generation-http`, as Caddy refuses two of one ID, that looks for a file of its own in Caddy's run directory, and once that's gone, answers `Connection: close`, which Go's HTTP/2 turns into a GOAWAY. It names no host, so it doesn't make HTTPS listen, and a server with plain routes alone holds nothing on its HTTPS port.
- **One change is one reload,** however many routes it changes (`PutRoutes`), as two about 5 ms apart gave 13,714 failed requests in the first run: a client a reload sent away came back in time to meet the next one.

**What this corrects in ADR 0024.** "Reloads drop nothing" holds for requests in flight, and for kept HTTP/2 and HTTP/3 connections. It doesn't hold for a kept HTTP/1.1 connection with a POST on its way, as above. It doesn't hold for a new connection on its way in either: Caddy binds a new `SO_REUSEPORT` socket for each reload, and closing the old one resets what is in its accept queue, and nothing can configure that away. Measured with no rollout at all, over 193 reloads at about 4,700 new connections a second: 39 of about 83,859 over HTTP/2, and 735 of about 104,161 over HTTP/1.1 (689 EOF, 46 reset), about 4 a reload. **So changing a route still costs every site on the server that much, once.** Changing a route is rare, and it is a change someone made, not a deploy, and each plan that changes one says so. A browser sends such a request again on its own, as none got an answer. An app's own client may not.

Beyond moving traffic:
- **An address isn't reused until the subnet goes round.** A rollout's new instance has a new address, the next free one, skipping what Caddy still dials and the keeper's. So a stale connection finds nothing, rather than another app.
- **Each step keeps to its budget.** An agent's call for a step gets `Decision.Budget()` more than `ApplyTimeout`: the health wait, 5 min by default and at most 30, the soak, the drain, and the stop grace.

### A moment of health isn't enough

One pass says little of an app that comes up and falls over on its first real work. So a new container has to stay healthy for 5 s (`DefaultSoak`) before anything moves to it: at least 3 of the agent's checks in a row, or Docker's own check not turning, or staying up with its routes' ports answering. A failure starts it again, and enough of them fail the deploy, with what ran still serving in a rolling step. A container a step didn't start, which has been up for a minute or more, needs none.

5 s is a choice. It catches what crashes on startup work, a restart loop's first turn, and a check that passes before the app is ready. It doesn't catch a slow leak, and nothing short of watching for as long as that takes could. It costs every deploy 5 s, and costs nobody a request: the old version goes on serving through a rolling step's, and a stop-first step's new version is already answering through its own. So the cost is only time. The plan says so, and `start_period` is still the way to wait longer for an app to come up.

### What starts a container stopped by hand

A stop by hand isn't drift (ADR 0021), so nothing Dockwell does on its own starts that container. A person's own request to run it does. The signal is `Run`, per service, on an edit, a change's item and what an agent is asked to plan.

| Leaves it stopped | Runs it, and verifies it |
| --- | --- |
| A reconcile, a resume, its agent reconnecting | A deploy, the same tag again included |
| A change to another service | A restart |
| A plan with nothing to do | A rollback of a change that was replacing it, whether or not that got as far as making anything |
| Importing a file that's unchanged, as importing is declarative | A rollback someone asked for, by its revision |
| A change in place, which skips verifying, as nothing runs to verify | Any change that replaces it |
| A rollback of another service's change | |

The plan says so, in the form "web is stopped, so this starts it", with no downtime, as nothing is up to take down. Such a step can't read as already so while the container isn't running, and it's verified before it reads as live. A rollback that follows a failure runs before anyone reads its plan, and where what failed is still there, its plan says only what replaces it.

**A stop a rollout made isn't a stop by hand,** as its mark says (above), so a resume finishes what it began.

### Rollback

A check that fails, or anything else that fails a step, fails the change and commits a rollback to the last revision that was deployed, on its own:

- **History stays honest.** The deploy stays failed, on record with its error. The rollback is a change of its own, with a revision that puts the old spec back, and says why in `Change.because`, with the check's own words: "web-3 runs, but fails its health check: GET /healthz on port 8080 answered 503 Service Unavailable". Both are on the timeline.
- **What ran never stopped,** in a rolling step: the rollback's `Run` finds it matches and removes only what failed. A stop-first deploy's rollback starts the old version again, with the data its image keeps where it was. There are two exceptions, where what ran can't be what the spec now asks for, so a rollback makes a new container, rolling where it can. A rollback pinned to a digest, as a moving tag's is, names an image what ran doesn't read as. And rolling back a rotation gives the secret a new revision with the old value (ADR 0022), so what ran doesn't read as that either, though it holds the same value.
- **It never touches data.** A rollback changes no volume and no server path, and what the new version wrote there stays. Each plan that replaces a service says so, in one note, `volumes_stay`, as code that has been rolled back can meet data migrated forward, which is for a person to know. The control plane reads it from the spec, what the image keeps comes from the server, and a restart, which has nothing to roll back, says nothing of it.
- **No loop.** A rollback isn't rolled back in turn. Nothing proposes one on its own again, and `Resume` only finishes what's applying.
- **A rollback that fails too is a person's to act on.** It stays failed, its deployment reads failed, and the deploy's error says "Rolling back failed too", what each said, and to check the servers. It's logged as an error, as nothing tries again. What runs stays as it is, and the next change takes it from whatever is there.

### Restart

`POST /v1/services/{id}:restart`, `dockwell services restart`: a rolling restart, with the same safety, under the revision store.

- **It's a change with a plan and no revision.** A restart changes no spec, so a new revision would say nothing. History has a change with `restart` set, and undoing it is refused, as there is nothing to undo.
- **Its token is its change's ID,** carried in the plan's step and on the new container's label `dev.dockwell.restart`, so planning again finds it done, and a crash can't make it happen twice. A counter in the spec was the other way, and any edit of the whole spec, or a Compose import, would have reset it.
- **One that rolls and fails removes what it made.** What ran goes on, there is no rollback, and the deployment's state is left alone.
- **One that stops first has nothing to go back to once the new one fails.** It stops first where a deploy would. If it can't go on before the new one has started, it puts what ran back. But once the old one has gone and the new one fails, nothing of the service runs, and the spec is the same, so there's nothing to roll back to. The plan says so before, in those words, and the failure after, with what to do.
- **One whose agent is lost waits for it,** and finishes, as there's no rollback to come.

### What a person reads

The plan sheet shows no steps for one server (ADR 0028), so how a step rolls is said where a person reads the plan, from `PlanStep.rollout`:
- **The first line says what it costs:** "rolling, with nothing stopped", or "about 12 s down". A container that is stopped has nothing to take down, so it says neither.
- **Beside the diff, quietly:** what takes over and when, what a drain ends, why it stops first, and data that stays.
- **What is worth weighing leads, in amber:** a mount that goes, and a restart that stops first and has nothing to go back to.
- **Two writers lead in red, with the fingerprint to type,** as a plan that destroys data does.
- A plan about a stopped container says only what is true of one: nothing goes on serving, and nothing is in flight.
- A restart never says it rolls back, here or in the row that says what a plan rolls back to, as it has no revision to go back to.

The CLI says the same from the same fields.

### Replicas are left out

They don't fall out cleanly, so they wait:
- ADR 0021 says a service scaled by hand isn't drift. A desired count makes it drift, which needs a decision of its own.
- `N` instances multiply the crash states above, and each needs the same proof.
- A read-write mount can't have two instances at all, so a count would need refusing for most of what people first run.
- The PRD puts replicas with phase 3's multi-server routing.

`deploy.replicas` other than 1 is refused by name. **The seam is there:** numbers go up, the table holds any number of instances per service, `Move` takes a list to move from, and `pick` chooses what stays.

## Consequences

- A deploy under load drops nothing, in the tests below, and reloads nothing.
- A deploy takes at least 5 s longer, for the soak, and a rolling one's old container takes at least 2 s longer to drain if it had recent traffic.
- A container's name changes with each rolling deploy, so `docker exec web-1` stops working. Anything that names a container should ask the API or use `docker compose exec`.
- A failed check is usually a few seconds of work thrown away, as a start period is the only wait that matters. An app that takes a long time to come up needs a `start_period`, as it would under Compose.
- A rollout's new address means the ingress table keeps the last it gave, and that a subnet goes round, after thousands of deploys on Docker's default pools. `give` skips what Caddy still dials, so going round isn't a risk.
- Changing a route still reloads, and costs every site on the server what's above, once.
- A failed deploy's new container goes with its rollback's step, and a failed restart removes its own, so what logs want to keep from them (#30) has to take it before that: `retire`, `stopFirst` and `discard` are where containers go.

## Memory

Measured with `make budgets`, 5 runs of each beside a build of the commit this one sits on, on one machine, idle for 180 s: an agent on its own went from 21.0 MB to 21.2 MB (budget 40 MB), and a control plane with its own agent from 41.1 MB to 41.8 MB (budget 250 MB). That machine has no Docker, so the monitor had nothing to watch. A test holds what it costs with something to watch: 3,000 services, each with a check that hangs, make 16 checks at a time and hold 2,680 bytes of heap each, 7.7 MiB in all (`TestManyServicesStayWithinWhatAMonitorHolds`).

## How it's tested

- **Real Caddy under load** (`TestARolloutDropsNoRequestOnRealCaddy`, `TestNoProtocolLosesARequestToARolloutOnRealCaddy` and their neighbours in `internal/ingress/caddy`): the real ingress, the route and service kinds, on high ports, with real HTTP servers standing where containers would, as a fake Docker Engine starts and stops them. Short requests over kept connections, each on its own connection, browser-like clients, POSTs over kept HTTP/1.1 and HTTP/2 connections, slow requests, server-sent streams and WebSockets, through 10 rollouts, then a deploy that fails and its rollback, and POSTs and GETs over HTTP/3 through as many.
- **Real Docker** (`make docker-e2e`, and tests named `…OnRealDocker`): the same counting, from `internal/loadtest`, through real Caddy in its container with a real certificate, on an image slow to turn healthy, one that never does, Docker's own check, a restart, a `SIGKILL` of all of Dockwell mid-rollout, and `docker compose` on what's left. An image that isn't ready answers every path with 503, so traffic that reaches it early counts as a failure.
- **Kills at every step** (`internal/resource/service/crash_test.go`), and **each step stopped from the control plane or lost from the agent** (`internal/revision/rollout_test.go`, on SQLite and Postgres).
- **Rollback** (`internal/revision/rollout_test.go`, `internal/control/rollout_test.go`): a real failing check through the real drivers on a fake engine, a rollback that fails too, and no loop.
- **Each safety check, taken out in turn, fails a test.** The list is in the pull request that added this.
