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
- **Making a variable a secret, or a secret a variable,** is one change that deletes the one and makes the other under the same name. It takes in every service that uses the value, with its use moved to the new kind under the same names (`resource.Rebinder`), so the switch is one plan, with a restart for each. A service the change edits itself keeps what the edit says, and is refused if it still uses what the change deletes. The panel proposes the two edits; so can REST, and `dockwell changes propose`, with the secret's edit read from a file with `--edits @file`.
- **What each revision uses is kept in the clear beside it,** in `revision_uses`: the name it sees each value as, the value's kind and name, and the revision it is pinned to, never a value. Finding the services that use a value, as every proposal and commit does, and listing an environment's values with their services, read it and open no spec. A change opens only the specs of the services it pins again.
- The plan of a rotation reads like any other: the secret's `value` is changed and sensitive, and each service's step shows `revisions[DATABASE_PASSWORD]: 3 → 4`.
- **Setting a value is a rotation, whatever it is.** An edit that sets a sensitive field, a secret's `value` or an entry of a container's `env`, always plans as a change: a new revision, and a restart of what uses the value. Its values count as changed, and are never compared with the ones there (`resource.DiffRotating`). Otherwise proposing a value would say whether it is the one there: a read-write token, or a stolen cookie with no passkey, would get no change for the right guess and one for a wrong one. Dockwell keeps the value, so deploys refer to secrets by name and never send one again; setting one is a deliberate rotation.
  - The agent is told so (`DesiredResource.rotate`), and plans such a resource as changed though it has the same, with every value the edit sets listed as changed. A map's keys still show, as added, kept or removed, as names aren't secret.
  - The change item records it (`change_items.rotates`), so applying plans it again the same way.
  - What Dockwell puts back rotates too: an undo, and a rollback. What a change takes in, such as the services that use a value, keeps its own values and doesn't, nor does keeping or reverting drift, whose values come from the revision or the server.
  - A container with values in its own `env` therefore restarts at every edit of it. Put them in secrets, and have it use them by name.
  - Nothing else in a plan depends on whether a value is the one there: its counts, actions, steps, downtime and wording are the same, and so is the work proposing does, as no value is compared. What is left is applying it: a wrong value set on a service shows when the service fails. That is a write, on the timeline, with a restart, and deploying an image can read a value anyway (see the threat model's gaps).

### Keys at rest

```
the install's key           <data>/control/key, 0600, never in the database
 └ key-encryption key       a row of key_encryption_keys, sealed with the install's key
    └ data key              a row of data_keys, wrapped under the key-encryption key
       └ spec               a revision's or change item's spec, sealed under its data key
```

- **What is sealed.** Every stored spec of a kind whose message has a `(sensitive)` field, at any depth: secrets, and also services and containers, whose environment may hold one. The gap ADR 0017 noted is closed for every such kind, not only for secrets. Variables, projects, environments and firewall rules stay in the clear, as they hold nothing secret.
- **A data key for each stored spec, bound to where it is stored.** Proposing a change seals each such item under a new data key, and applying it seals the revision under another. Each is AES-256-GCM with a random nonce. Its associated data names the resource's ID and kind, and the revision's number, or the change's ID and the item's position (`store.SpecAt`). So a sealed spec copied anywhere else doesn't open: into another resource's row, into another revision of the same resource, such as a leaked value passed off as its rotation, or into another change's item. A revision is sealed for the number it is about to get, the next after its base, which the commit has just checked is the latest; the store refuses to write it under any other.
- **The key-encryption key** is random, 256 bits, and kept sealed with the install's key, under a purpose that names it (`internal/seal`). Data keys are AES-256-GCM under it, bound to their own ID and the key-encryption key's.
- A stored spec says whether it is sealed: it starts with a zero byte, which no protobuf encoding does. A spec of a kind that may hold secrets must be sealed: one stored in the clear is refused when it is read, so someone who can write the database, without the key file, can't put a spec of their own where a sealed one belongs. A spec of any other kind is stored as it is.
- **Fingerprints don't give a secret away.** A plan's fingerprint hashed each item's spec, and a backup holds the fingerprint, so a guess at a short password could be checked against it. A sealed kind's spec is digested with HMAC-SHA256 under a key derived from the install's key (`seal.Key.Derive`), over the change's ID and the spec. So a fingerprint is useless without the key file, and it is the change's own: applying reproduces it, and no two changes' fingerprints match because they set one value, such as a guess beside an undo that puts an old value back. The same edits against the same state still give the same plan, and the same fingerprint when they set nothing sealed. Which kinds are sealed is said once, by `resource.Sealed`.
- **A change records how its fingerprint was made** (`changes.fingerprint_scheme`): 1 for every spec hashed as it is, 2 for the keyed digests bound to the change, with whether each item rotates. A change planned before the upgrade has 1, and is checked the way it was fingerprinted, so it applies while its plan is the one reviewed. When it isn't, it says an earlier version planned it, and to propose it again, rather than that the servers changed. An item it holds for a kind that may hold secrets, such as a container planned for a server that was offline, was stored in the clear, so it is refused: the change says an earlier version planned it, and to propose it again.
- **Plans mask by the schema.** An agent's steps say which field changes are sensitive; the control plane masks every change whose path the schema marks sensitive, whatever the agent says, before a plan is kept. It reads a path as `resource.Diff` writes it: field names, a list's index, and a map's key in brackets, which may hold dots, as a label such as `traefik.enable` does. A path that names a whole message, or a whole map or list of them, is sensitive when anything within it is: a change at a service's `app` would carry its container's environment.
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
- **A call sent again is sealed again.** A step the agent refuses as it moves to a renewed certificate goes to its next stream (ADR 0017). The values are read once, and sealed to the delivery key of each stream the call goes out on.
- **An agent that sends no delivery key** can't run a service that uses a variable or secret, and the step says to update it.

### On the agent

- The agent opens the values for one step in memory and hands them to the driver through the call's context (`resource.WithValues`). It clears them when the step ends. They go into no log, error, plan, file, Compose file or image. `agent.Desired` hears of the step with the call's own context, which holds none.
- **The seam for the Docker driver (#26).** The service driver builds the container's environment with `binding.Environment(ctx, svc)`: the app's own variables, then each value under the name the service sees. It records which variables and secrets it used, with their revisions and never their values, in the container label `dockwell.uses`. `binding.Restore` reads the service back from that label, so observing a service gives its spec, pins and all, and drift compares like with like. #26 needs only a Runtime that keeps a container's environment and labels as given. If it builds containers another way, it calls `binding.Environment` where it sets the environment, and writes `binding.Label` beside it.
- **`agent.Desired` gets references, never values.** It hears of each step with the pinned spec, which names what the service uses and at which revision. Values sealed to a stream are useless after it ends, so a cache can hold nothing that opens them. A server that restarts a service on its own while the control plane is away (#32) needs the values from the control plane again. Docker keeps running containers running meanwhile.
- **Docker itself** keeps a container's environment in its own configuration, on the server's disk, readable by root. That is Docker's, not Dockwell's, and anyone who is root there can read a running process's environment anyway. Files under `/run/secrets`, on tmpfs, avoid it for images that read `*_FILE` variables; they come later, through the same seam.

### Revealing a value, and changing how you sign in: a step-up

- **`RevealSecret` needs three things:**
  - a signed-in session;
  - a proof from `FinishStepUp` for revealing that secret;
  - nothing but a person: no API token can call it, whatever its scope.
- **A step-up is a fresh passkey assertion** that confirms one action on one target in one session (`AuthService.BeginStepUp`, `FinishStepUp`):
  - The options allow only the person's own passkeys for this address that aren't flagged as copied, and require user verification. Finishing checks the assertion against every passkey the options allowed, and it must come from one of them.
  - Finishing checks the assertion, its counter and copies as sign-in does, in one transaction, and audits it as `auth.step_up`, or `auth.step_up_failed` when refused.
  - It returns a proof of 256 random bits. Only the proof's hash is kept, in memory. It works once, within a minute, in that session, for that action and target. A proof given for anything else is spent all the same.
- **Changing how you sign in takes one too.** Otherwise a stolen session could add a passkey of its own and pass every step-up with it. Each of these needs a proof for itself, as its request's `proof`:

  | Action | Method | Target |
  | --- | --- | --- |
  | Adding a passkey | `BeginAddPasskey` | you |
  | Removing a passkey | `RemovePasskey` | the passkey |
  | Setting up codes, which ends with recovery codes | `BeginTotpSetup` | you |
  | Turning codes off | `DisableTotp` | you |
  | Making new recovery codes | `RegenerateRecoveryCodes` | you |
  | Creating an API token | `TokenService.CreateToken` | you |

  - A proof is spent once the action's own checks pass, so a refusal for another reason, such as the last way in or a token's missing name, doesn't cost another prompt.
  - The ceremony that adds a passkey works only in the session that began it.
  - `RemovePasskey` is `POST /v1/passkeys/{id}:remove`, so its proof is in the body, never in a URL.
  - Revoking sessions and tokens, signing out and unlocking codes need none: they take access away, and someone who lost their passkey must still be able to.
- **Someone with no passkey here adds one by signing in again.** A sign-in with an authenticator code, a recovery code or a link from `dockwell recover` confirms adding a passkey, as a step-up would, for 10 minutes. That is how someone adds one after adding a domain, or after losing every passkey they had here. It confirms nothing else, and nothing after the 10 minutes; a passkey sign-in confirms nothing, as a step-up with the passkey is at hand. Asked to confirm with none here, `BeginStepUp` says to sign in again (`needs_fresh_sign_in`). The audit log says which confirmed each passkey added. Claiming makes the first passkey in its own transaction, as before.
- **Revealing reads the value and records `secret.reveal` in one transaction:** who, which secret and revision, from where. If the record can't be written, nothing is revealed.
- **Reveals are on the timeline** among access events ([ADR 0020](0020-the-event-timeline.md)), as "Revealed the secret DB_PASSWORD", and so are rotations of the key-encryption key. Step-ups, and refused ones, stay in the audit log with the other attempts.
- **No token, three times over.** A method option, `(dockwell.v1.needs_session)`, puts `RevealSecret` beyond every token in the interceptor. The handler and the auth service refuse a token's principal as well. AuthService, where step-ups begin, was already beyond tokens. The PRD's later `connections:read` scope would be a new scope, decided on its own.
- **No authenticator-code fallback.** A step-up's worth is that a stolen session can't make one: it needs the person's authenticator, bound to this origin, with their fingerprint, face or PIN. A six-digit code is guessable within code sign-in's limits, and phishable in real time by whoever holds the session. Nobody needs to reveal a secret to use it, since binding isn't reading, and someone without a passkey here can sign in again, add one and then reveal.
- The ceremony and proofs live in memory, like sign-in's, on one control plane.

### The API, the CLI and the panel

- **Changing variables and secrets is ChangeService's,** as edits of kind `variable` and `secret`, so every change is a plan, with undo and the timeline.
- **`VariableService`** reads an environment's variables and secrets (`ListVariables`, `GET /v1/variables`), by name, a page at a time: each variable's value, never a secret's, and the services that use each, under each name they use it as. It opens no sealed spec. It also reveals (`RevealSecret`, `POST /v1/secrets/{id}:reveal`).
- **The CLI is generated, as ADR 0019 has it.**
  - `dockwell variables` and `dockwell secrets` hold the kind's `create`, `update` and `delete` beside the service's `list` and `reveal`: a kind's verbs join a noun a service's methods already have, and the noun's help names them all.
  - A sensitive scalar field takes no value on the command line. Its flag names a file, `--value-file path`, or `-` for standard input, which asks without echo on a terminal. So a secret stays out of shell history and the process list, with no option of its own. It reads at most what a secret holds, 64 KiB.
  - Kinds that run on no server have no `--on`, and fields Dockwell sets have no flag.
  - `dockwell secrets reveal` needs a person, and says to use the panel.
- **The panel's screen is an environment's variables and secrets,** at `/variables`, following `App-Environment`. Its environment switch shows once there are two environments. You reach it from the command bar, which has an entry for each environment once there are several, from the More tab on phones, which it sits under, and from Activity, where a change to a variable or secret, and a reveal, link to its environment: a reveal's event names the secret's environment as its subject's `scope`. The app screens (#31) will show an app's slice of the same list. Adding, editing and removing each open the plan sheet straight away, rather than the mock's tray of unapplied edits: one review step, the plan sheet, serves one edit. A secret shows masked, with Reveal, which asks for the passkey, then the value with Copy and Hide. It hides again after a minute.

## Consequences

- Every spec that may hold a secret is encrypted at rest, and a plan never carries one, so the PRD's security row holds for containers and services too, not only secrets.
- A backup of the database is safe to store apart from the key file, and useless without it. Losing the key file loses every secret, the CA and authenticator secrets with it, as before: the install guide says to back it up apart.
- Each sealed spec costs a row in `data_keys` and two AES-GCM operations to read. Reading a spec now takes a transaction, which every reader already had.
- Every service that uses a value restarts when it changes, in the change that changes it, one server at a time, as its driver plans. Rolling restarts with two replicas, and keeping the old value valid until every replica is healthy, as the PRD's rotation describes, come with replicas.
- A change can go out of date because of a value it uses, not only its own resources, and says which.
- The agent can't restart a service that uses secrets on its own while the control plane is away. #32 decides whether its cache keeps values sealed to a key of its own, a trade of principle 5 against what a stolen disk holds.
- Change items planned before this, for a server that was offline, may still hold a container's environment unsealed. They can't apply: no agent had drivers then, and now they are refused as unsealed. Discarding them doesn't erase them. No install is known to have any.
- Revealing needs a passkey for the panel's current address. After adding a domain, add a passkey there first, as for sign-in: sign in there with a code or a link from `dockwell recover`, then add it within 10 minutes.
- A stolen session can't add a passkey, codes or a token of its own, nor remove the person's: each takes the person's passkey, just now. People without a passkey at an address make one in the minutes after signing in some other way, and every change to how they sign in prompts for the passkey, as revealing does.
- Each change records how its fingerprint was made (`changes.fingerprint_scheme`), each of its items whether it rotates (`change_items.rotates`), and each revision of a service what it uses (`revision_uses`). None holds a value, and each is history like the rows beside it: triggers on both databases refuse to change or delete it, as for revisions and changes (ADR 0009).
- The threat model is in [docs/security/secrets.md](../security/secrets.md).
