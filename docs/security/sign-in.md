# Sign-in: threat model

How a person claims an install and signs in to the panel, what an attacker could try, and what stops them. The design is in [ADR 0011](../adr/0011-claim-and-sign-in.md); the code is in `internal/auth`, `internal/control/authapi`, `internal/https` and `internal/cli/claim.go`.

## What there is to protect

| Asset | Where it lives | Why it matters |
| --- | --- | --- |
| The claim link | Printed by `dockwell run` and `dockwell claim`; only a SHA-256 hash in `<data>/control/state.db` | Whoever spends it first becomes the owner, with root on every server |
| Session tokens | The browser's cookie jar; only hashes on the control plane | A session can do anything its user can |
| Passkeys' private keys | The person's devices and password managers | They are the main way in |
| Authenticator secrets | `<data>/control/state.db`, sealed with the key in `<data>/control/key` | Whoever has one can make valid codes |
| Recovery codes | Wherever the person keeps them; only hashes on the control plane | Each signs in once, even while code sign-in is locked |
| Sign-in links | Printed by `dockwell recover`; only hashes on the control plane | Each signs in once |
| The ACME account key and the panel's certificate key | `<data>/control/acme`, 0700 | Whoever has them can impersonate the panel's domain |

## Trust boundaries

- **The control server** is trusted with everything above. Local access to its data directory, root or the dockwell user, is ownership of the install: such a user can run `dockwell claim` before anyone has claimed it, and `dockwell recover` after.
- **The person's browser and devices** are trusted for their own sessions and passkeys.
- **The network** is untrusted, including the path to the domain. Without a domain, the panel never leaves loopback; it is reached through the operator's SSH connection.
- **Other web pages in the same browser** are untrusted. So are other services on the operator's `localhost`, which is where the SSH tunnel ends.
- **The operator's terminal and logs** are assumed to leak eventually. The claim link appears in `dockwell run`'s output, so it reaches the system journal.

## Threats and mitigations

| Threat | Mitigation |
| --- | --- |
| Someone else claims a fresh install first | Only a claim link creates the owner, and only the server's own output has one. Without a domain, the panel answers only on loopback, so reaching it at all needs SSH access. The link's 256 random bits are compared as a hash; a wrong, used, replaced or expired link all get the same error. |
| A claim link leaks, through the journal, shell history or a screenshot | It works once and for 24 h, and a new one from `dockwell claim` replaces it. It sits in the URL fragment, which browsers never send to a server or in a `Referer`. Creating and spending it are audited. Anyone who can read the journal on the control server is usually root there already. |
| Two people race to claim | Spending the link is a conditional update, and the database allows one owner (a partial unique index), all in one transaction. Exactly one claim wins; tested with 12 racing claims. |
| The panel is exposed over plain HTTP | `dockwell run` refuses a non-loopback `--listen`. With a domain, the panel is served only over HTTPS, and `:80` only redirects (GET and HEAD) or answers ACME challenges. The redirect target is the configured domain, never the request's `Host`. |
| A DNS rebinding page talks to the loopback panel | Every request's `Host` must be localhost, a loopback address or the domain, and plain HTTP must come from a loopback address. Anything else gets 421. |
| Cross-site request forgery | Session cookies are SameSite=Strict, and Connect's JSON and binary content types can't be sent cross-origin without CORS, which the panel doesn't allow. A request whose `Origin` isn't the panel's, or whose `Sec-Fetch-Site` says another site, is refused. A write authenticated by cookie must also prove it came from the panel, by `Origin` or `Sec-Fetch-Site: same-origin`. |
| A page on another `localhost` port forges requests through the tunnel | Ports don't separate cookies, and `localhost:3000` is the same site as `localhost:7600`, so SameSite doesn't stop it. The `Origin` check does, and so does `Sec-Fetch-Site: same-site`. |
| Another service on the operator's `localhost` sees the session cookie | The cookie goes to every `localhost` port. A local service could replay it, but only through the tunnel while it is open, only on `localhost`, and only for 12 idle hours. Use a domain for daily work; the tunnel is for the claim and for occasional use. |
| A session cookie is stolen from an HTTPS page | It is HttpOnly, so scripts can't read it, and Secure with the `__Host-` prefix, so it never goes over HTTP and no subdomain can set or shadow it. HSTS keeps browsers on HTTPS. It is bound to the address it was made on, ends after 12 idle hours or 7 days, and shows in the session list, where you can revoke it. |
| A session token is guessed | 256 random bits, stored only as a hash. |
| A passkey made for one address is used at another | WebAuthn binds every assertion to its RP ID, and the server checks both the RP ID hash and the passkey's stored RP ID. A ceremony is bound to the origin it began on. |
| A passkey is used without the person present | User verification is required at registration and at every sign-in, so the authenticator checks a fingerprint, face or PIN. |
| A passkey is copied, for example off a security key | When the sign count goes backwards, the sign-in is refused, the passkey is flagged and stops working, and the event is audited. The person signs in another way and removes it. |
| A passkey response is replayed | Each ceremony's challenge is random, works once, and expires after 5 minutes. |
| Authenticator codes are guessed | Each source address gets 5 wrong codes and each account 3, then every try waits twice as long as the last, up to 15 minutes. The account's count is stored, so a restart doesn't reset it. After 10 wrong codes in a row, code sign-in locks until the owner unlocks it. A guesser therefore gets about 10 tries, each with a 3 in a million chance, per decision by the owner, and every try is audited. |
| A code is watched and replayed | The newest step accepted is stored, and only a later one is accepted, so each code works once. Racing requests can't both use it; this is tested. |
| An email is checked for an account | A code for an email with no account gets the same answers as a wrong code, including the lock after 10. Response time may still differ slightly, and the email isn't secret. |
| Someone locks the owner's code sign-in on purpose | It takes the owner's email and 10 wrong codes. Passkeys, recovery codes and `dockwell recover` still work, the lock is audited, and the owner unlocks it in the panel. |
| A database backup leaks | Session and link tokens, and recovery codes, are stored only as hashes of at least 80 random bits, which can't be reversed by search. Authenticator secrets are sealed with AES-256-GCM, bound to the user, under a key kept in a separate file. Passkeys store only public keys. |
| The person loses every passkey and their authenticator | Recovery codes work once each. Failing those, `dockwell recover` on the control server prints a one-time sign-in link that expires in 15 minutes, and it is audited. |
| A signed-in person removes their last way in | Removing a passkey, or turning off codes, is refused when nothing usable would be left. |
| A revoked session keeps a stream open | Revoking or signing out closes the session's streams at once, with `Unauthenticated`. Every stream also re-checks its session every 30 s, which covers expiry. |
| Someone floods the sign-in endpoints | Each source address may start 30 ceremonies or link checks at once, then one every 2 s. At most 1,024 ceremonies are kept, and the oldest go first. Failures for unknown emails or passkeys are logged, not audited, so they can't fill the audit log. |
| Someone gets a certificate issued for the panel's domain from this server | The ACME host policy allows only the configured domain, and the plain-HTTP listener answers challenges only for it. |

## Known gaps

These are tracked and not yet covered:

- **Re-authentication for sensitive actions.** A signed-in session can add or remove passkeys and turn codes off without proving itself again. A stolen session already controls every server, so this matters less than it would elsewhere, but asking for a passkey again is planned.
- **The panel behind your own reverse proxy.** Trusted-proxy settings, which would also give rate limits the client's real address, don't exist yet.
- **Shared state for a highly available control plane.** Ceremonies, the per-address counters and stream tracking are in memory, on one control plane.
- **Sessions through a tunnel share `localhost`.** Every local service on the operator's machine receives the `localhost` cookie, as described above. A domain avoids it.
- **API tokens.** The CLI and scripts can't authenticate yet, apart from the commands that use the data directory directly.
