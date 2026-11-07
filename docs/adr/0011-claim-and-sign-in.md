# 0011. Claim over a tunnel or a domain, then sign in with passkeys

- Status: Accepted. [0024](0024-the-caddy-ingress-driver.md) changes who holds ports 80 and 443 on a server with a domain once it has a route: Caddy takes them and serves the panel's domain too, over a unix socket only it can reach. Everything else here stands, the refusal of a loopback proxy included.
- Date: 2026-11-04

## Context

The PRD's first signature experience is "Ninety seconds to a live URL": one install command, a passkey to claim the instance, then the first deploy. Its Access rows ask for passkeys and 2FA at launch, and its security requirements ask for "passkeys and TOTP for people". The panel controls root on every server, so whoever claims it first owns them all.

Passkeys (WebAuthn) bring constraints that shape everything else:
- Browsers only run WebAuthn in a secure context: HTTPS, or `http://localhost`.
- A passkey belongs to a relying party ID (RP ID), which must be a hostname. IP addresses are refused.
- A passkey works only on its RP ID and that name's subdomains.

A fresh VPS has an IP address and nothing else. Until #14, the API listened on loopback with no sign-in at all (PR #47).

### Options considered

1. **Serve the panel over plain HTTP on the server's IP.** Passkeys can't work there, cookies would cross the network in the clear, and the first visitor would own the server. Rejected.
2. **A self-signed certificate on the IP.** Browsers show a warning that teaches people to click through it, and the RP ID still can't be an IP. Rejected.
3. **Require a domain before anything works.** This is safe, but it puts DNS in front of the first minute. Many people try a tool on a bare VPS before they point a name at it. It's kept as the path for people who already have a name.
4. **A Dockwell-run hostname such as `<id>.dockwell.host`, with its certificate.** This is the smoothest path, but the domains aren't registered yet and the service doesn't exist. It comes later and fits the same design (see below).
5. **Loopback plus an SSH tunnel, and a domain when you have one.** Everyone installing on a VPS already has SSH. `http://localhost` is a secure context, and `localhost` is a valid RP ID. Chosen.

## Decision

**Two ways to reach the panel.**
- **Without a domain:**
  - The panel and API stay on `127.0.0.1:7600`. `dockwell run` refuses a non-loopback `--listen`, so the panel is never served over plain HTTP to the network. A domain doesn't lift this: it adds an HTTPS listener beside the loopback one. `install.sh` refuses such a `DOCKWELL_LISTEN` before it installs anything.
  - You reach it through an SSH tunnel, `ssh -L 7600:127.0.0.1:7600 you@server`, at `http://localhost:7600`. The printed command forwards to the address the panel is bound to, such as `[::1]` for `--listen [::1]:7600`.
  - The RP ID is `localhost`.
  - Any local port works for the tunnel, because the origin is taken from the browser's `Host`. Page loads on `127.0.0.1` redirect to `localhost`, where passkeys work.
- **With `--domain` (or `DOCKWELL_DOMAIN`):**
  - The same handler also serves HTTPS on `:443`, with a certificate from Let's Encrypt over ACME. It uses `golang.org/x/crypto/acme/autocert`, which answers TLS-ALPN-01 on `:443` and HTTP-01 on `:80`.
  - Plain `:80` redirects to HTTPS. It refuses anything but GET and HEAD, and it never serves the panel or the API.
  - The ACME account key and certificates live in `<data>/control/acme`, owner-only.
  - The RP ID is the domain. The loopback listener keeps working, for the CLI and for tunnels.
  - `--acme-directory-url` points at another CA, such as Let's Encrypt's staging CA or Pebble.
- **Every request** is checked against these hosts. Any other `Host` gets 421, which also defeats DNS rebinding against the loopback listener.

**The first owner comes only from a claim link.**
- On start with no owner, `dockwell run` makes a claim link and prints the exact steps: the tunnel and `http://localhost:7600/claim#<token>`, or `https://<domain>/claim#<token>`. It does that only when its standard output is a terminal. A service's goes to a log, the system journal, which more people than root can read, so there it makes no link, and prints the command that does, such as `sudo dockwell claim`, which the claim page shows too; `install.sh` runs it as it finishes, when its own output is a terminal, and otherwise says to ([#110](https://github.com/Dockwell/dockwell/issues/110), and [the threat model](../security/sign-in.md)). When a link it printed before still works, as after a restart, it keeps that link and says so: the link `install.sh` showed must survive the service restarting. That holds only while the link works as printed, so the address each claim link is printed for is recorded, and a restart where the panel answers elsewhere, on another port or at a domain, prints a new link instead, on a terminal.
- The tunnel it prints names the user who ran `sudo`, or of the SSH session it runs in, and the server's address only when it's public: a cloud server behind 1:1 NAT has a private one on its interface, which no laptop reaches. What isn't known is written `<user>@<your server>`.
- The token is 256 random bits. Only its SHA-256 is stored. It works once, expires in 24 h, and sits in the URL fragment, so it never reaches a server log or a `Referer` header.
- A new link replaces any earlier unused one, so at most one works at a time. `dockwell claim` prints a fresh link, and is the only way to replace one that still works; like `dockwell token create`, it needs local access to the data directory. A user other than root looks in one of their own, so where that has none, it looks in `/var/lib/dockwell` too, and says to use `sudo` when only root can. Once the install has an owner, it exits with status 3, so `install.sh` can tell that from a failure, which it shows.
- Claiming takes two calls. `BeginClaim` checks the link without spending it and starts a passkey registration. `FinishClaim` verifies the passkey, then spends the link, creates the owner, stores the passkey and starts a session, all in one transaction.
- The spend is a conditional update, and a partial unique index allows one owner, so racing claims can't make two. No other code path creates a user.

**Passkeys.**
- They use `github.com/go-webauthn/webauthn` v0.18.2.
- Registration requires a discoverable credential (resident key) and user verification. Sign-in requires user verification and names no account: the browser offers the passkeys it holds for this address.
- Attestation is `none`. Dockwell doesn't restrict authenticator models.
- Each passkey is stored with its RP ID, credential record and sign count. A ceremony is bound to the origin it began on, and it's held in memory for at most 5 minutes, once.
- **Clone detection.** A passkey whose sign count goes backwards (both counts nonzero) is treated as copied. The sign-in is refused, the passkey is flagged and never signs in again, and the event is audited. Synced passkeys always report zero and never trip it.
  - Reading the stored count, checking the assertion against it and storing the new count are one write transaction. Writes are serialized, so of a copy and the original signing in at once with one count, the second is refused as a copy, and the stored count never goes backwards.
  - When the flag can't be stored, the sign-in is refused all the same, the failure is logged and returned, and the flag is stored on the passkey's next sign-in, whichever copy makes it.
- A signed-in user can add passkeys and remove them, but never the last way in. Since [ADR 0022](0022-secrets-encrypted-at-rest.md#revealing-a-value-and-changing-how-you-sign-in-a-step-up), each needs a fresh assertion from a passkey they have here (a step-up). Adding one can instead be confirmed, once, by a sign-in in the last 10 minutes that needed no passkey here: a link from `dockwell recover`, a recovery code, or an authenticator code when no passkey is registered for this address. Afterwards there must be another passkey that signs in at an address the panel answers on now, or code sign-in. Such a passkey is one for `localhost` or the current domain that isn't flagged as copied; one for a domain the panel no longer has can't sign in, so it doesn't count. Recovery codes come and go with code sign-in. `dockwell recover` doesn't count either: it is the last resort for whoever can reach the server, not a way in to keep.

**Codes from an authenticator app (TOTP), with recovery codes.**
- **Codes:**
  - RFC 6238 with HMAC-SHA1, 6 digits and 30 s steps, accepting one step either side of now.
  - The newest accepted step is stored, and only a later step is accepted, so a code works once even when requests race.
  - The secret is sealed at rest with `internal/seal`, bound to the user's ID.
- **Turning codes on** needs a code from the new secret, and since ADR 0022 a step-up to begin, which only that session can finish, within 10 minutes and 5 wrong codes. It then returns ten recovery codes, shown once. Each is 80 random bits, written as `XXXX-XXXX-XXXX-XXXX` in Crockford's base32, and stored only as a hash.
- **Code sign-in** takes an email and a code. It is the fallback, so its limits are strict:
  - Each source address (IPv6 by /64) gets 5 wrong codes before backoff. Each email address, counted in the database so a restart doesn't reset it, gets 3 wrong authenticator codes.
  - After that, each try waits twice as long as the last, up to 15 minutes.
  - After 10 wrong authenticator codes in a row, code sign-in locks for that email until the owner unlocks it from a session made some other way. This caps what a patient guesser can try: about 10 guesses per owner decision, not a few a minute forever.
  - Recovery codes are limited only by source address. A right one is checked before anything about the account, so it works while code sign-in is slowed down or locked, and a wrong one doesn't count towards the lock. A limit kept for the account would be one anyone with the owner's email could trip, and 80 random bits need none ([the reasoning](../security/sign-in.md#recovery-codes-and-the-lock)).
  - An email with no account gets the same answers, lock included. Wrong codes are counted in one table for every email tried, whether or not it has an account, by the same code. Emails share 65,536 buckets by a hash, which bounds the table.

**Sessions.**
- A session is an opaque 256-bit token in a cookie, stored as a hash.
- The cookie is HttpOnly and SameSite=Strict. On HTTPS it is also Secure, with the `__Host-` prefix (`__Host-dockwell_session`). On localhost it is `dockwell_session` without Secure. Plain HTTP anywhere else never gets a cookie.
- A session is bound to the RP ID it was made on. It ends after 12 hours idle, or 7 days after sign-in, whichever comes first. An open stream counts as use, so a panel page left open keeps its session until the 7 days are up.
- Revoking a session, or signing out, closes its open streams at once. A stream registers before it checks its session a last time, so one that opens as its session is revoked closes too. Every stream also re-checks its session every 30 s.
- You can list your sessions (browser, address, created, last seen, which one is this), revoke any of them, or revoke all the others.

**Protecting the API.**
- An interceptor on every service of the API listener requires a live session. The exceptions are:
  - `GetAuthState`;
  - the claim and sign-in calls;
  - `SystemService.GetVersion`, because the sign-in screen shows the version;
  - gRPC health, and `/healthz`.
- A test calls every registered procedure without a session and fails on any answer not in that list.
- When a session can't be checked, as when the database fails, the calls in that list go ahead as if signed out, so the sign-in screen still loads; `GetAuthState` then keeps the cookie, whose session may still work. Every other call answers `unavailable`, with the reason `unavailable`.
- **CSRF**, on top of SameSite=Strict and Connect's non-simple content types:
  - A request with an `Origin` other than the panel's own is refused, and so is one whose `Sec-Fetch-Site` says another site. That includes `same-site`, which is how another port on `localhost` shows up.
  - A write authenticated by cookie must carry a matching `Origin` or `Sec-Fetch-Site: same-origin`.
- The agent listener is separate and unchanged: mTLS (ADR 0007).
- **Refusals say why.** Every call refused for want of a session carries a reason under `Dockwell-Error`: `unauthenticated` with no session, `session_ended` for a cookie whose session no longer works or a stream whose session ended, `origin` for the CSRF checks, and `misdirected` for another host. AuthService's own errors carry theirs, such as `code_rejected`, so a client can tell a wrong code from a session that ended.
- **Audit.** The claim, every sign-in and failure, sign-out, session revocation, passkeys added, removed or flagged, codes on, off, locked and unlocked, and each recovery code used are audit events. A failure could come from anyone, so it names the account it tried as its subject and the address it came from as its actor, as `anonymous:203.0.113.9`; only a sign-in that worked names the person. Failures for emails or passkeys that match no account are logged but not audited, so strangers can't fill the audit log. Nor can they through a real account: codes sent while code sign-in is locked, and wrong recovery codes, which aren't bounded by the lock, get at most one event an hour per account, carrying the count since the last.

**Adding a domain later.**
- Passkeys made on `localhost` don't work on the new domain, and the browser won't offer them.
- When `dockwell run` starts with a domain it didn't have before, it says so and explains what to do. You sign in on the domain with an authenticator code or a recovery code, or with a one-time link from `dockwell recover`, then add a passkey for the new address.
- The panel's passkey list shows which address each passkey works on.
- `dockwell recover` prints a one-time sign-in link that expires in 15 minutes. It needs local access to the data directory, like `dockwell claim`. Anyone who has that already controls the install, and the sign-in screen points to it.

**Auth state is access, not managed state.** Users, passkeys, codes and sessions aren't resources with revisions and plans (ADR 0009). Undoing the removal of a passkey or a revocation would re-grant access, which is the opposite of what undo is for. These changes are audited instead.

**What the hostname service will add.**
- `internal/https.Source` is the seam. It gives the control plane a domain, a TLS configuration and a plain-HTTP handler, and ACME is one implementation.
- A future `dockwell.host` source will:
  - register `<install>.dockwell.host` for the install;
  - point it at the server;
  - get its certificate, through DNS-01 run by the service, or by delegating the name to the server for TLS-ALPN-01.
- Nothing else changes. The RP ID becomes that hostname, and the same domain-change flow covers moving from it to the owner's own domain.

## Consequences

- A fresh VPS is claimed in about a minute over SSH, with no DNS, and nobody without SSH access can reach the claim. The one extra step is the tunnel command, which `dockwell run` prints ready to paste.
- People with a domain get HTTPS and passkeys on it with one flag, but the server needs ports 80 and 443 (root or `CAP_NET_BIND_SERVICE`), and DNS must point at it first.
- Behind your own reverse proxy, the panel isn't supported yet. A loopback proxy that sends a public `Host` header is refused. That needs trusted-proxy settings later.
- Passkeys are per address, so changing the address means adding a passkey. The output and the panel explain this when it happens, and a code or `dockwell recover` bridges the gap.
- Ceremonies, rate-limit counters for addresses, and open-stream tracking live in memory. They would move to shared state for the highly available control plane.
- Code sign-in can be locked by anyone who knows the owner's email. Passkeys, recovery codes and `dockwell recover` still work, and the lock is audited, so this costs convenience, not access.
- Re-authentication before changing passkeys or codes was left out here, so a stolen session could add a passkey of its own. [ADR 0022](0022-secrets-encrypted-at-rest.md#revealing-a-value-and-changing-how-you-sign-in-a-step-up) closed it with step-ups: adding or removing a passkey, setting up or turning off codes, making recovery codes and creating an API token each take a fresh passkey assertion.
- The threat model is in [docs/security/sign-in.md](../security/sign-in.md).
