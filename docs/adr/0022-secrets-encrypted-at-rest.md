# 0022. Variables and secrets, encrypted at rest and referenced by services

- Status: Accepted
- Date: 2026-11-04

## Context

The PRD asks for "encrypted secrets per environment, referenced not copied" at launch, and its security rows say secrets are "envelope-encrypted at rest; never written to logs or plans". Its Connections section adds three rules that apply to every secret, not only a connection's:
- **Injection.** Values reach containers at start, never in Compose files, image layers or logs. A server receives only the secrets of apps placed on it, and the agent decrypts them in memory.
- **Rotation is a plan.** Editing a value opens a plan listing every app and environment that will restart.
- **Bind is not read.** Binding a secret doesn't let you see it. Revealing a value needs a passkey and lands in the audit log, and API tokens can't read values.

What was there:
- [ADR 0009](0009-resources-revisions-and-plans.md) marks fields that may hold secrets `(sensitive)`, and plans say such a field changed but never show it.
- [ADR 0017](0017-the-change-api-and-the-agent-stream.md) noted the gap this record closes: revision rows, and the change items that ask for them, held specs in the clear, a container's environment included.
- `internal/seal` seals the CA's key and authenticator secrets under the install's key, a file beside the database.
- [ADR 0011](0011-claim-and-sign-in.md) gave people passkeys and sessions, and [ADR 0016](0016-scoped-api-tokens.md) kept tokens away from destructive and sign-in methods.
- The agent's cache of desired state (#32) plugs in at `agent.Desired`. The Docker driver (#26) is being built beside this.

Issue #29 asks for all of it: the specs, encryption at rest, delivery, revealing, the panel's screen and the CLI.

## Decision

### Two kinds, in an environment

- **`variable`** holds a value that isn't secret, such as `LOG_LEVEL=info`. Plans, history and the panel show it.
- **`secret`** holds one that is, in a field marked `(sensitive)`.
- Both live in an environment and have revisions and plans like any kind. They run on no server themselves: each is a `resource.Valuer`, whose value goes to the servers of the services that use it.
- Their names are what a process's environment takes, such as `DATABASE_URL`. A variable and a secret can't share a name in one environment, as the panel shows them as one list.
- A value is at most 64 KiB and has no zero byte, which no environment can hold. A secret can't be empty, which is almost always an empty file read by mistake.

### Services use them by reference

- A service's spec names the variables and secrets it uses, each under the name the service sees: `secrets: {DATABASE_PASSWORD: DB_PASSWORD}` gives the service the secret `DB_PASSWORD` as `DATABASE_PASSWORD`. An empty name means the same name.
- **Each use is pinned to a revision.** When a change is planned, the revision store writes the revision each use resolves to into the service's spec, in `revisions`, a field Dockwell sets and an edit can't (`google.api.field_behavior = OUTPUT_ONLY`). So a service's spec says exactly which value it runs with, and a new value makes a new spec.
- **Rotation is a plan.** A change to a variable or secret takes in every service that uses it, with its uses pinned again. Each gets a revision of its own, and its agent plans the restart. The plan lists the value as changed, without it, and every service and server that restarts, with downtime. Undo and rollback put the old revision back, and the services with it.
- **A change applies only while its pins hold.** Applying checks again that each pin is still the latest revision, and that no service started using a changed value since the change was planned. Otherwise the plan is out of date, as for a base revision that moved.
- A service can use only what its environment has, and nothing it uses can be deleted while it does, unless the same change stops using it.
- The plan of a rotation reads like any other: the secret's `value` is changed and sensitive, and each service's step shows `revisions[DATABASE_PASSWORD]: 3 → 4`.

### Keys at rest

```
the install's key           <data>/control/key, 0600, never in the database
 └ key-encryption key       a row of key_encryption_keys, sealed with the install's key
    └ data key              a row of data_keys, wrapped under the key-encryption key
       └ spec               a revision's or change item's spec, sealed under its data key
```

- **What is sealed.** Every stored spec of a kind whose message has a `(sensitive)` field, at any depth: secrets, and also services and containers, whose environment may hold one. The gap ADR 0017 noted is closed for every such kind, not only for secrets. Variables, projects, environments and firewall rules stay in the clear, as they hold nothing secret.
- **A data key for each stored spec.** Proposing a change seals each such item under a new data key, and applying it seals the revision under another. Each is AES-256-GCM with a random nonce, and its associated data names the resource's ID and kind, so a sealed spec copied into another resource's row doesn't open.
- **The key-encryption key** is random, 256 bits, and kept sealed with the install's key, under a purpose that names it (`internal/seal`). Data keys are AES-256-GCM under it, bound to their own ID and the key-encryption key's.
- A stored spec says whether it is sealed: it starts with a zero byte, which no protobuf encoding does. A spec stored before this change opens as it is.
- **Fingerprints don't give a secret away.** A plan's fingerprint hashed each item's spec, and a backup holds the fingerprint, so a guess at a short password could be checked against it. A sealed kind's spec is digested with HMAC-SHA256 under a key derived from the install's key (`seal.Key.Derive`), which keeps fingerprints reproducible and useless without the key file.
- **Plans mask by the schema.** An agent's steps say which field changes are sensitive; the control plane masks every change whose path the schema marks sensitive, whatever the agent says, before a plan is kept.
- **Revisions stay immutable.** Rotating the key-encryption key wraps every data key again, in their own table, and changes no revision.

### Rotating the key-encryption key

`dockwell rotate-key`, on the control server, as the user that runs dockwell:
1. makes a new key-encryption key;
2. wraps every data key under it;
3. retires the old one, emptying its sealed copy;

all in one transaction, which the audit log records as `secrets.rotate_key`. The control plane can keep running: it reads the active key in each transaction that seals, and finds each data key's own key when it opens one. Rotating the install's key itself, the root, means sealing again the key-encryption keys, the CA's key and the authenticator secrets; that has no command yet.

### What a database backup alone reveals

Nothing of any secret. It holds:
- sealed specs, data keys wrapped under a sealed key-encryption key, and keyed fingerprints;
- plans that say a secret changed, and never what it is;
- audit events that name which secret was revealed, by whom and when, never the value.

The install's key is a separate file, and a backup of the database without it opens none of this. With the key file, it all opens: back them up apart. `TestADatabaseDumpHoldsNoSecret` makes secrets, a service that uses them and a container with a secret in its environment, rotates, undoes, discards, reveals and leaves a change planned. It then reads every row of every table, on SQLite and Postgres, and finds none of the values: as they are, in hex, or in base64.

### Delivery

- **Only to the servers a service runs on.** When the engine applies a step that runs a service, the agent stream asks the keeper for the values its spec uses (`resource.Values`). The keeper refuses unless the service's latest revision is placed on that server and is the spec being applied. It then reads each value at the revision the spec pins. A deletion carries none, and planning and observing carry none.
- **Sealed to one stream.** Each agent stream makes an X25519 key pair, keeps the private key in memory, and sends the public key in its hello (`delivery_key`). The values for a step travel in the `ApplyCall` sealed to it with HPKE (RFC 9180: DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, AES-256-GCM, Go's `crypto/hpke`). The info binds them to the server and the resource. So values never appear in the clear in a message, even within mTLS. They can't be opened for another resource or server, nor once the stream ends, as the key that opens them is gone.
- **An agent that sends no delivery key** can't run a service that uses a variable or secret, and the step says to update it.

### On the agent

- The agent opens the values for one step in memory and hands them to the driver through the call's context (`resource.WithValues`). It clears them when the step ends. They go into no log, error, plan, file, Compose file or image.
- **The seam for the Docker driver (#26).** The service driver builds the container's environment with `binding.Environment(ctx, svc)`: the app's own variables, then each value under the name the service sees. It records which variables and secrets it used, with their revisions and never their values, in the container label `dockwell.uses`. `binding.Restore` reads the service back from that label, so observing a service gives its spec, pins and all, and drift compares like with like. #26 needs only a Runtime that keeps a container's environment and labels as given. If it builds containers another way, it calls `binding.Environment` where it sets the environment, and writes `binding.Label` beside it.
- **`agent.Desired` gets references, never values.** It hears of each step with the pinned spec, which names what the service uses and at which revision. Values sealed to a stream are useless after it ends, so a cache can hold nothing that opens them. A server that restarts a service on its own while the control plane is away (#32) needs the values from the control plane again. Docker keeps running containers running meanwhile.
- **Docker itself** keeps a container's environment in its own configuration, on the server's disk, readable by root. That is Docker's, not Dockwell's, and anyone who is root there can read a running process's environment anyway. Files under `/run/secrets`, on tmpfs, avoid it for images that read `*_FILE` variables; they come later, through the same seam.

### Revealing a value: a step-up

- **`RevealSecret` needs three things:**
  - a signed-in session;
  - a proof from `FinishStepUp` for revealing that secret;
  - nothing but a person: no API token can call it, whatever its scope.
- **A step-up is a fresh passkey assertion** that confirms one action on one target in one session (`AuthService.BeginStepUp`, `FinishStepUp`):
  - The options allow only the person's own passkeys for this address that aren't flagged as copied, and require user verification.
  - Finishing checks the assertion, its counter and copies as sign-in does, in one transaction, and audits it as `auth.step_up`, or `auth.step_up_failed` when refused.
  - It returns a proof of 256 random bits. Only the proof's hash is kept, in memory. It works once, within a minute, in that session, for that action and target. A proof given for anything else is spent all the same.
- **Revealing reads the value and records `secret.reveal` in one transaction:** who, which secret and revision, from where. If the record can't be written, nothing is revealed.
- **Reveals are on the timeline** among access events ([ADR 0020](0020-the-event-timeline.md)), as "Revealed the secret DB_PASSWORD", and so are rotations of the key-encryption key. Step-ups, and refused ones, stay in the audit log with the other attempts.
- **No token, three times over.** A method option, `(dockwell.v1.needs_session)`, puts `RevealSecret` beyond every token in the interceptor. The handler and the auth service refuse a token's principal as well. AuthService, where step-ups begin, was already beyond tokens. The PRD's later `connections:read` scope would be a new scope, decided on its own.
- **No authenticator-code fallback.** A step-up's worth is that a stolen session can't make one: it needs the person's authenticator, bound to this origin, with their fingerprint, face or PIN. A six-digit code is guessable within code sign-in's limits, and phishable in real time by whoever holds the session. Nobody needs to reveal a secret to use it, since binding isn't reading, and someone without a passkey here can add one and then reveal.
- The ceremony and proofs live in memory, like sign-in's, on one control plane.

### The API, the CLI and the panel

- **Changing variables and secrets is ChangeService's,** as edits of kind `variable` and `secret`, so every change is a plan, with undo and the timeline.
- **`VariableService`** reads an environment's variables and secrets (`ListVariables`, `GET /v1/variables`): each variable's value, never a secret's, and the services that use each. It also reveals (`RevealSecret`, `POST /v1/secrets/{id}:reveal`).
- **The CLI is generated, as ADR 0019 has it.**
  - `dockwell variables` and `dockwell secrets` hold the kind's `create`, `update` and `delete` beside the service's `list` and `reveal`: a kind's verbs join a noun a service's methods already have.
  - A sensitive scalar field takes no value on the command line. Its flag names a file, `--value-file path`, or `-` for standard input, which asks without echo on a terminal. So a secret stays out of shell history and the process list, with no option of its own.
  - Kinds that run on no server have no `--on`, and fields Dockwell sets have no flag.
  - `dockwell secrets reveal` needs a person, and says to use the panel.
- **The panel's screen is an environment's variables and secrets,** at `/variables`, following `App-Environment`. Its environment switch shows once there are two environments. You reach it from the command bar, which has an entry for each environment once there are several, from the More tab on phones, which it sits under, and from Activity, where a change to a variable or secret, and a reveal, link to it. The app screens (#31) will show an app's slice of the same list. Adding, editing and removing each open the plan sheet straight away, rather than the mock's tray of unapplied edits: one review step, the plan sheet, serves one edit. A secret shows masked, with Reveal, which asks for the passkey, then the value with Copy and Hide. It hides again after a minute.

## Consequences

- Every spec that may hold a secret is encrypted at rest, and a plan never carries one, so the PRD's security row holds for containers and services too, not only secrets.
- A backup of the database is safe to store apart from the key file, and useless without it. Losing the key file loses every secret, the CA and authenticator secrets with it, as before: the install guide says to back it up apart.
- Each sealed spec costs a row in `data_keys` and two AES-GCM operations to read. Reading a spec now takes a transaction, which every reader already had.
- Every service that uses a value restarts when it changes, in the change that changes it, one server at a time, as its driver plans. Rolling restarts with two replicas, and keeping the old value valid until every replica is healthy, as the PRD's rotation describes, come with replicas.
- A change can go out of date because of a value it uses, not only its own resources, and says which.
- The agent can't restart a service that uses secrets on its own while the control plane is away. #32 decides whether its cache keeps values sealed to a key of its own, a trade of principle 5 against what a stolen disk holds.
- Change items planned before this, for a server that was offline, may still hold a container's environment unsealed. They can't apply (no agent had drivers), and discarding them doesn't erase them. No install is known to have any.
- Revealing needs a passkey for the panel's current address. After adding a domain, add a passkey there first, as for sign-in.
- Adding a passkey still needs only a session. So someone who steals a session can add their own passkey and step up with it. Making a step-up guard adding passkeys, removing them and turning codes off is the next use of the same mechanism. Until then, the audit log shows the new passkey and every reveal.
- The threat model is in [docs/security/secrets.md](../security/secrets.md).
