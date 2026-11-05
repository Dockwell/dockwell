# Health checks: threat model

A check is the one thing a spec can make the agent do on the network, and what it says reaches a timeline that anyone with the read scope sees. [ADR 0027](../adr/0027-health-checks-rollouts-and-rollback.md) is the design.

## What's worth protecting

- **What the agent can reach and containers can't:** the server's loopback, its cloud metadata address, and other private networks.
- **Secrets in a container's environment,** which a check's output could repeat.
- **What a person reads:** a timeline and a terminal, which a check's output, or an agent's report, could try to fool.
- **Whether "healthy" means anything.** A deploy, and a person, act on it.

## Who we consider

| Who | What they have |
| --- | --- |
| A write token, leaked or careless | Any spec that isn't destructive and gives no host access ([ADR 0016](../adr/0016-scoped-api-tokens.md)), a check among them |
| A compromised app | Code in a container: it chooses what its address answers, and what a command check prints |
| A compromised agent | Its own server, and its stream to the control plane. It can say anything |
| Someone with the read scope | The API's reads, the timeline among them |

## What each can and can't do

### A write token

- **Make the agent call another host.** It can't. An HTTP or TCP check goes to the address Docker reports for the container, and the spec has no host to set. `host` is only the `Host` header. The agent ignores the proxy settings in its environment.
- **Reach loopback through a redirect.** It can't. A check doesn't follow redirects, so a 3xx passes it and goes nowhere.
- **Read what a port on the container answers.** It can't, beyond the status. The body isn't read, and a TCP check only connects.
- **Tell a port is open on a container it made.** It can, as that is what a check does. It could run a container that does the same.
- **Run a command on the server.** It can't. A command check runs inside the container, as Docker's own does, and has what the container has.
- **Keep the agent busy.** A check has a timeout of at most 1 min, an interval of at least 1 s, and the agent runs at most 16 at a time, whatever a spec asks. Checks that all hang leave the rest of the agent as it was: 3,000 stayed within those 16 workers and a bounded heap. What took a second or more last time gets at most half of the workers, so a check that would say a service has died isn't held up behind them.

### A compromised app

- **Choose what its check says.** It can, for its own service. That's what a check is: an app that lies about its health gets what it says. It can't make another service's check pass.
- **Get a secret onto the timeline by printing it.** It's masked if it's the value of a variable the container's spec set, of 6 characters or more, or a password in a URL. What the image's own `ENV` sets isn't, as anyone with the image has it. A container that doesn't say what its spec set has all of its environment masked. A secret that's elsewhere, or transformed, isn't. So output is cut to 240 bytes, and reaches only the read scope. It isn't a promise. The app could send its secrets anywhere it can reach in any case.
- **Fool a terminal or a timeline with its output.** It can't. Output comes to one line of printable characters, with control and bidirectional characters gone, on the agent and again on the control plane.
- **Get traffic before it passes.** It can't. Caddy takes the new upstream only after the check passes. A later failure doesn't take traffic away, as a service has one container at rest, and the timeline says so.

### A compromised agent

- **Say what it likes of its own server.** It can. An agent is trusted with its server, as it runs it.
- **Say something of a service on another server.** It can't. The API reads a report by a service's placement, and the timeline only records a service placed on the server that speaks.
- **Flood the timeline.** It can't. A state has to last 30 s, and two events of a service on a server are at least 5 min apart. A service not in the store is forgotten at the next look.
- **Send more than its monitor would.** It can't. What it reports is cut to 4,096 containers, each cleaned as above, and a state with no name reads as unknown.

### Someone with the read scope

- **Read a check's last output.** They can, on the app's health, and on a deploy that failed its check and on the rollback that followed, for as long as those changes are on record. It's why that output is short and masked, where a log needs a grant of its own.

## Where a claim of health can be wrong

- **A service with no check isn't called healthy.** It reads `NO_CHECK`, and the panel says "Running".
- **An agent that's gone isn't called anything.** It reads `UNKNOWN`, and what it last said comes marked stale.
- **A claim only stands for as long as someone is looking.** Each state says when it was last known and how long that may stand, at least 90 s. Past that, or when Docker stops answering, what only Docker can say reads `UNKNOWN`, and so does a report older than 90 s, whatever it says. The age comes from each clock's own counting, so a server's wrong clock can't make an old state look new. An agent is trusted with its own server, so it could still claim anything of it.
- **An agent that hasn't read Docker yet says nothing,** so that doesn't read as nothing running.
- **A paused container reads as paused, never as healthy or running,** as it serves nothing and Docker stops its checks, so nothing it last said stands for it.
- **A step cut short has its check all the same.** The control plane stopping, or an agent going away, leaves a deploy or a restart to finish, and it isn't applied until what runs passes.
- **A check passing means what it checks.** A TCP check says a port takes a connection, and an HTTP check that the path answered below 400. A command check says nothing of the port a route sends to, so a rollout also asks each route's port to take a connection before it moves anything. Once it has moved traffic, only checks go on.

## What this leaves open

- **A rollback can meet data the new version migrated.** Rollback never touches a volume, so old code can read what new code wrote. Each plan that replaces a service says so (`volumes_stay`).
- **`rollout.order: start-first` over a shared volume** means two writers for as long as the step takes. Only a spec that asks gets it, a person has to approve it, so a token can't choose it, and the plan names what's shared. A rollback, which nobody approves, stops first there whatever order it goes back to says, and a step stops at two writers its plan couldn't read, as on a volume its own change makes.
- **Other services can reach a new container before it is healthy.** Docker answers a service's names for every running container that has them, and can't add a name to one that has joined. On a network the server already has, that includes other projects' containers. The plan names the networks and the names where this happens, and the record says why it can't be fixed ([ADR 0027](../adr/0027-health-checks-rollouts-and-rollback.md)). The ingress isn't affected.
