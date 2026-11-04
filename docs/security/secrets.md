# Secrets: threat model

How Dockwell keeps an environment's secrets, and every spec that may hold one, encrypted at rest. It also covers how it gets them to the servers that run what uses them, and how a person reveals one. It says what an attacker could try, and what stops them. The design is in [ADR 0022](../adr/0022-secrets-encrypted-at-rest.md), beside sign-in's threat model in [sign-in.md](sign-in.md). The code is in `internal/secrets`, `internal/resource/binding`, `internal/resource/variable`, the revision store in `internal/revision`, `internal/auth/stepup.go` and `internal/control/variables`.

## What there is to protect

| Asset | Where it lives | Why it matters |
| --- | --- | --- |
| Secrets' values | Sealed in `revisions` and `change_items`, in `<data>/control/state.db` or Postgres; in memory on the control plane while it reads one; in memory on an agent for one step; in the running container's environment | They are the credentials the apps use: databases, payment providers, storage |
| Containers' and services' environments | Sealed the same way, as their specs may hold a secret | People put credentials there too |
| The install's key | `<data>/control/key`, 0600 | It opens every key-encryption key, so every secret, the CA's key and the authenticator secrets |
| Key-encryption keys | `key_encryption_keys`, sealed with the install's key | Each opens every data key wrapped under it |
| Data keys | `data_keys`, wrapped under a key-encryption key | Each opens one stored spec |
| An agent stream's delivery key | The agent's memory, for one stream | It opens the values sent on that stream |
| Step-up proofs | Hashes in the control plane's memory | Each reveals one secret, once, within a minute |

## Trust boundaries

- **The control server** is trusted with everything above, as for sign-in. Whoever is root there, or the user that runs dockwell, has the key file and the database, so every secret.
- **A database backup, or the database server alone,** is untrusted. Postgres may run elsewhere, and backups travel.
- **Each server's agent** is trusted with the secrets of what runs on it, and with nothing else. Root on a server can read what runs there, through Docker or `/proc`, so nothing hides a server's own secrets from its root.
- **The network between agents and the control plane** is untrusted, as for every other message, beneath mTLS.
- **People with a session** may use secrets: bind them to services, rotate them, delete them. Revealing a value takes them, at the keyboard, with their authenticator, each time.
- **API tokens, and wherever they are kept,** are untrusted with values. No token can reveal one.
- **Logs, plans, the timeline and the audit log** are read by more people, and kept longer, than secrets should be: they never hold a value.

## Threats and mitigations

| Threat | Mitigation |
| --- | --- |
| A database backup, or a copy of the Postgres database, leaks | Every spec of a kind that may hold a secret is sealed under a data key of its own. Data keys are wrapped under a key-encryption key, which is sealed with the install's key, a file never in the database. Plans mask what changed, and fingerprints digest such specs with a key derived from the install's key, so a guess at a password can't be checked against them. Audit events name a reveal, never the value. A test reads every row of every table, on SQLite and Postgres, after secrets are made, used, rotated, undone, discarded, left planned and revealed, and finds no value in any encoding. |
| The install's key leaks with the backup | Then everything opens, as it was meant to: the key is what makes the backup the install's. Back the key file up apart from the database, and keep it 0600, which `dockwell run` checks. |
| A sealed spec is copied to another resource's row, to read it as that one, such as a production secret into a staging one someone may reveal | A spec is sealed with its resource's ID and kind as associated data, so it opens only as itself. A test moves one and fails to open it. |
| A key-encryption key leaks, or might have | `dockwell rotate-key` makes a new one, wraps every data key under it and retires the old, forgetting it, in one transaction, and the audit log records it. A copy of the old key opens only data keys from a backup taken before. |
| Stored specs are changed, such as a value swapped | Every seal is AES-256-GCM: a changed byte fails to open, and the change fails rather than run something else. |
| A plan, the timeline or an error shows a value | Fields that may hold one are `(sensitive)`. Diffs say they changed and never show them, and the API blanks them again on the way out. What an agent reports is masked by the schema's own field paths, not by what the agent says, so an agent that gets it wrong can't put a value in a plan; tested. Drivers' errors, which reach plans and progress, name what is missing, never a value; tests check theirs. A rotation's plan shows each service's revision of the secret moving, as `revisions[DATABASE_PASSWORD]: 3 → 4`. |
| A server receives secrets it doesn't run | The agent stream asks for the values of a step's spec only when it applies it. The keeper refuses unless the service's latest revision is placed on that server and is that spec, and then reads only what the spec uses, at the revisions it pins. Tested by asking for another server and for another spec, and across two real agent streams. |
| A message on the agent stream is read or replayed | Values travel sealed with HPKE to a key the agent made for that stream and keeps in memory, bound to the server and the resource, beneath mTLS. They can't be opened for another resource or server, or once the stream ends. |
| An agent's disk, or a snapshot of it, holds values | The agent writes none: it opens them in memory for one step and clears them after. `agent.Desired` hears of specs that name what they use and at which revision. Docker keeps a running container's environment in its own configuration, on the server's disk, as for any container. That is the server's root's to read anyway, and files on tmpfs for `*_FILE` variables come later to avoid it. |
| A container's label gives a value away | The `dockwell.uses` label names what a service uses, its kind and its revision, never a value. |
| A developer binds a production secret to read it | Binding is an edit of the service: the plan shows the binding, and the change is on the timeline. Its value goes to that service's container, so someone who can deploy any image can read what that image is given; that is deploying, not reading, and both are on record. Revealing through Dockwell takes a passkey and is audited. Roles that may deploy but not bind production secrets come with roles. |
| An API token reveals a value, or begins a step-up | `RevealSecret` is marked `(dockwell.v1.needs_session)`, so the interceptor refuses every token for it. Its handler and the auth service refuse a token's principal too, and step-ups begin in AuthService, which no token reaches. Tested over every protocol with read-only and read-write tokens, and with a token holding a person's fresh proof. |
| A script with someone's session cookie reveals values | It needs a fresh passkey assertion for each secret, which needs the person's authenticator and its user verification. A proof works once, within a minute, in its session, for one secret. |
| A step-up proof is replayed, or used for another secret, session or action | Proofs are 256 random bits, kept as hashes and taken out when used, whatever they are used for. Each is bound to the session, the action and the target, and expires after a minute. Tested for each. |
| A passkey assertion is replayed into a step-up | Each ceremony's challenge is random, works once, expires after 5 minutes, and is bound to the session and site that began it. An answer to another ceremony is refused and audited. |
| A copied passkey steps up | Its counter is checked against the stored one in the same write transaction as at sign-in. A count that goes back flags the passkey, refuses it and audits it. |
| A reveal goes unrecorded | The value is read and `secret.reveal` recorded in one transaction; if the record fails, nothing is revealed. Each step-up is recorded too, refusals included. |
| A secret lands in shell history or the process list | The CLI reads a secret's value from a file or standard input, `--value-file`, never from a flag's text, and asks without echo on a terminal. |
| A rotation leaves some service on the old value | A change to a variable or secret takes in every service that uses it, and its plan lists them. Applying checks that the pins still hold and that no service started using the value since; otherwise the change is out of date. Tested both ways. |

## Known gaps

These are tracked and not yet covered:

- **Adding a passkey needs only a session.** Someone who steals a session can register their own passkey, then step up with it. The audit log shows the new passkey and every reveal, and revoking the session ends it, but a step-up doesn't hold them off. Requiring a step-up to add or remove a passkey, or turn codes off, is the next use of the same mechanism. It is the re-authentication gap in [sign-in.md](sign-in.md#known-gaps).
- **Deploying is reading.** Whoever can deploy an image with a secret bound can make it print the value. Roles that separate binding production secrets from deploying come with roles.
- **The old value stays valid until every replica is healthy** isn't there yet: a service restarts with the new value at once, one server at a time. It comes with replicas.
- **Docker keeps a container's environment on disk.** Values as files on tmpfs, for images that read `*_FILE`, come later, through the same seam.
- **The agent can't restart a service that uses secrets while the control plane is away.** Docker keeps running ones running. #32 decides whether to keep values sealed to a key of the agent's own.
- **Rotating the install's key itself** has no command yet. It would seal again the key-encryption keys, the CA's key and the authenticator secrets.
- **Proofs and ceremonies live in one control plane's memory,** as sign-in's do. A highly available control plane needs shared state for them.
