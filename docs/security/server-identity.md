# Server identity: threat model

How a server proves who it is to the control plane, what an attacker could try, and what stops them. The design is in [ADR 0007](../adr/0007-one-binary-two-roles.md); the code is in `internal/pki`, `internal/seal`, `internal/control/identity`, `internal/control/agentapi` and `internal/agent/enroll`.

## What there is to protect

| Asset | Where it lives | Why it matters |
| --- | --- | --- |
| The CA's private key | `<data>/control/state.db`, sealed | Whoever holds it can mint any server's identity |
| The seal key | `<data>/control/key`, 0600 | It opens the CA key and, later, every stored secret |
| Join ticket secrets | The operator's clipboard; only a SHA-256 hash on the control plane | A ticket admits one server |
| Each server's private key | `<data>/agent/server.key`, 0600, made on that server | It is that server's identity |
| The control plane's state | `<data>/control/state.db`, 0600 | It records which servers exist and which are revoked |

## Trust boundaries

- **The control server** is trusted with everything above. Root on it is root over the install.
- **Each server's agent** is trusted only as itself. It runs as root on its own machine.
- **The network** between them is untrusted, including the path a joining server takes.
- **The operator's terminal and chat**, where tickets are pasted, are assumed to leak eventually.

## Threats and mitigations

| Threat | Mitigation |
| --- | --- |
| Someone intercepts a join on the network, or redirects it to an impostor | The ticket pins the CA by the SHA-256 of its public key, and the agent refuses any control plane whose chain doesn't include that CA (`pki.VerifyPinned`). The secret only ever travels inside that verified TLS 1.3 connection. |
| A ticket leaks, through shell history or a chat | A ticket works once and expires after 1 h by default, 24 h at most. Creating and spending one is audited, with who and from where. Spending is atomic, so two servers racing with one ticket can't both join. |
| An attacker guesses ticket secrets | The secret is 256 random bits, compared in constant time. A wrong, spent or expired ticket all get the same error, so failures reveal nothing. |
| A joining server asks for someone else's identity | The CSR contributes only its public key; the control plane chooses the server ID and name. Only ECDSA keys are accepted, and the CSR's signature must verify. |
| An agent's key is stolen | Every renewal brings a new key, and only the server's current certificate may renew. Once the real server renews, a copied key and certificate are refused, and the attempt is audited as `server.renew_rejected`. If the thief renews first, the real server is refused instead, so the theft can't go unnoticed. Certificates last 30 days either way, and revoking the server stops both. |
| A copied old certificate is used to stream made-up stats | The agent stream accepts only the server's current certificate, or the one it just replaced, and checks again every 30 s. The agent moves its stream to each new certificate as soon as it renews, so a stream on anything older is closed. |
| A server is revoked while its stream is open | The stream checks every 30 s. A revoked server's stream is closed and the server leaves the live fleet. |
| An agent opens a stream and stays silent | A stream must send its hello, and then stats, within 30 s of the last message, or it is closed. |
| A renewal's answer is lost, or the agent crashes mid-renewal | The agent writes its next key to disk before asking. A retry with the previous certificate and that same key is accepted once; a retry with any other key is not. |
| A compromised server tries to act as another | Its certificate names only itself (a SPIFFE-style URI chosen by the control plane), and it can't sign certificates. |
| A database backup leaks | The CA key in it is sealed with AES-256-GCM, bound to its purpose, under a key kept in a separate file. Ticket secrets are stored only as hashes. The database files are owner-only. |
| Someone floods the agent listener | Join is the only call that works without a client certificate, and it does one indexed read before rejecting. |

## Known gaps

These are tracked and not yet covered:

- **CA rotation.** Changing the CA means rejoining every server until a rotation procedure exists.
- **Rate limits on Join.** They would slow floods further, but no ticket can be guessed without them.
- **The seal key on the same disk as the database.** Stealing both together exposes the CA. Keeping the seal key in an OS keyring, a TPM or a cloud KMS is phase 4 work (PRD: external secret stores).
