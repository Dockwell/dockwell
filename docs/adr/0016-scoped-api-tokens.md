# 0016. Scoped API tokens

- Status: Accepted
- Date: 2026-11-04

## Context

The PRD's Access rows ask for "scoped API tokens; audit log" at launch. Scripts, CI systems, the CLI (#22), the MCP server and other integrations need to call the API without a browser. [ADR 0011](0011-claim-and-sign-in.md) gave people sessions: a cookie that is bound to the address it was made on, that a passkey or a code starts, and that the guard protects from cross-site requests. None of that suits a script. It has no passkey, keeps no cookie jar, and often runs from another machine.

A token for them is a bearer secret that lives in files, environment variables and CI secrets, and those leak more often than a passkey. So the design has to answer:

- what a token looks like, so it can be found where it leaks;
- what a token may do, and what it must never do;
- how it fits the guard and the interceptor, over every protocol, including the CSRF checks;
- its lifecycle, and what the audit log records.

Every method already declares its effect in the schema (`(dockwell.v1.effect)`: read, plan, apply or destructive), for the CLI and MCP generators. That is the natural boundary for scopes.

## Decision

### The secret

- A secret is `dwt_`, then 43 base62 digits holding 256 random bits, then 6 base62 digits holding a CRC-32 of those 43. That is 53 characters, matching `dwt_[0-9A-Za-z]{49}`.
  - The prefix lets secret scanners, such as GitHub's, gitleaks and TruffleHog, find tokens in code and logs.
  - The checksum lets a scanner confirm a match, and the CLI reject a mistyped token, without asking the server.
  - Base62 survives double-click selection, URLs and shells without quoting.
- Only a SHA-256 of the whole secret is stored. With 256 random bits there is nothing to search, so a slow hash would add nothing, as for sessions.
- The secret is shown once, in the response to `CreateToken`. Afterwards a token shows only its hint, such as `dwt_4f9K…x9Ta`: the prefix, and the first and last 4 of the 43 random digits. It never shows the checksum, which is computed from the random digits and would give more of them away. That tells tokens apart and recognises one in a log, and leaves about 208 of the 256 random bits unknown.
- A token is sent as `Authorization: Bearer <secret>`, with REST, Connect, gRPC and gRPC-Web alike. It is never read from a URL or a cookie.

### What a token may do

- A token acts for the person who made it, and only within its scope. Scopes follow each method's `(dockwell.v1.effect)`:

  | Scope | `EFFECT_READ` | `EFFECT_PLAN`, `EFFECT_APPLY` | `EFFECT_DESTRUCTIVE` | `AuthService`, `TokenService` |
  | --- | --- | --- | --- | --- |
  | Read only | Yes | No (`token_scope`) | No (`needs_session`) | No (`needs_session`) |
  | Read and write | Yes | Yes | No (`needs_session`) | No (`needs_session`) |

- **No token may call a destructive method.** Apply methods can be undone through revisions (ADR 0009); destructive ones may not come back. The PRD makes destructive methods ask a person for a typed confirmation, which a script can't give honestly. A leaked read-write token can therefore change things, but the owner can undo every change it made.
- **No token may manage sign-in or tokens.** With `AuthService` and `TokenService` out of reach, a leaked token can't make itself a successor, outlive its revocation, add a passkey, or lock the owner out. The services check this in three places: the interceptor refuses the call, the handlers refuse a principal that came with a token, and the auth service refuses to create or revoke a token for one.
- A procedure the interceptor can't find an effect for needs a session, so a new service is closed to tokens until its methods say what they do.
- Reflection is open to any token, as it only shows the schema, which is public.
- Calls that need no session, such as `GetVersion` and the sign-in calls, go ahead as signed out whatever token comes with them.
- Finer scopes, such as per project or the PRD's `connections:read`, will come as new `TokenScope` values or a list of scopes, without changing these two.
- Until revisions land, no method outside the two session-only services plans or applies, so read-write allows nothing more than read-only yet. The scope exists now so that tokens made today keep their meaning.

### How a token fits the guard

- When a request has a Dockwell token, a bearer that starts with `dwt_`, the token alone authenticates it: the session cookie is ignored, and never used as a fallback. A wrong token is refused even with a working cookie beside it.
- Anything else in the `Authorization` header isn't Dockwell's and is ignored: other schemes, such as Basic credentials, and other bearers, such as those oauth2-proxy and Pomerium add in front of an app. The cookie then authenticates the request, origin checks and all, as if the header weren't there, so a proxy's bearer neither breaks the panel nor stands in for a token.
- "Bearer" with nothing after it counts as a token, which fails with `token_rejected`. A script with an unset variable sends that, and no proxy does.
- A request with a Dockwell token skips the guard's origin checks, and the rule that a write with the cookie must prove it came from the panel. Those checks exist because a browser attaches the cookie to any request a page makes, from any site. No browser attaches an `Authorization: Bearer` header on its own, and a page on another origin can't add one without a CORS preflight, which the API never answers. So a request with a token can't be forged by a page that lacks the token, and one that has it could send it from anywhere.
- The host check applies to every request, with a token or not, so DNS rebinding gets 421 either way.
- Tokens aren't bound to an address, unlike sessions. They work on `localhost` through a tunnel, on the loopback IP where the CLI runs on the server, and on the panel's domain. The panel serves plain HTTP only on loopback, so a token crosses the network only over TLS.
- With a domain, plain `:80` refuses a request with an `Authorization` header, with 400, instead of redirecting it to HTTPS. Many clients follow a redirect with the header still on, so a script set up with `http://` would otherwise work, sending its token in the clear on every call. The message says the token may have been seen, and to revoke it and use `https://`.
- One interceptor serves every protocol, REST included ([ADR 0015](0015-rest-and-openapi-from-the-schema.md)). It reads each procedure's effect from the registered descriptors once, at start.
- Refusals carry a reason under `Dockwell-Error`:
  - `token_rejected` (unauthenticated, 401) for a token that is malformed, unknown, revoked or expired. Whoever holds the token can tell which, and nobody else learns anything.
  - `token_scope` (permission denied, 403) for a read-only token making a change.
  - `needs_session` (permission denied, 403) for a call no token may make.
  - `unavailable` (503) when the token can't be checked, as when the database fails.

### Lifecycle

- A signed-in person creates, lists and revokes their tokens with `TokenService`, and on Profile and security in the panel.
- A token has a name of up to 100 characters, a scope, and an optional expiry: a time in the future, or a TTL, which the control plane adds to its own clock ([AIP-214](https://google.aip.dev/214)). The panel offers 30 days, 90 days (the default), a year, or never, and sends a TTL, so a browser whose clock is off doesn't move the expiry.
- Someone can have at most 100 tokens that still work.
- `ListTokens` returns the tokens that still work, newest first, with each one's hint, scope, creation time, expiry, and when and from which address it was last used. Like a session's last use, that is recorded at most once a minute per address, and best effort. An address here is what `token.use` counts as one, below, so a client whose IPv6 privacy address changes doesn't write on every call.
- Revoking a token stops it at once. Its open streams close at once too, through the same registry that closes a revoked session's streams, and every stream checks its token again every 30 s, which covers expiry.
- Revoked and expired tokens are kept for 30 days after they stop working, then deleted, with their addresses, the next time someone signs in or creates a token, as ended sessions are. The control plane has no periodic job to do it sooner.
- "Sign out everywhere else" ends sessions, not tokens. Tokens are revoked in their own list.
- Like sessions, tokens are access, not managed state, so they aren't resources with revisions (ADR 0011). Their changes are audited.

### Audit

- These are audit events:
  - `token.create`, with the name, scope and expiry, never the secret;
  - `token.revoke`;
  - `token.use`, the first time a token is used from an address it hasn't used before. An IPv6 /64 counts as one address, and an IPv4 address is one however it is written, such as `::ffff:203.0.113.9`. Each token remembers its 256 most recently seen addresses.
  - `token.refuse`, when a known token is refused because it was revoked, expired, or is outside its scope.
- A refused token's events are limited to one an hour for each token, carrying the count since the last. The counts are kept in memory, so a script that loops with a revoked token writes nothing to the database. A restart forgets them, which costs at most one more event. They are kept for at most 10,000 tokens, and making room forgets tokens with nothing left to report first, so only someone holding more tokens than that, all refused, could make a count be lost.
- Unknown and malformed tokens are logged, not audited, so strangers can't fill the audit log, as with unknown emails in ADR 0011.
- A token's own calls name it as the actor, as `token:tok_01JD…`. Refusals of a revoked or expired token name the address they came from, as `anonymous:203.0.113.9`, because anyone could hold it by then.

### No limit by address

- Sign-in keeps its limits by source address, and they apply to REST unchanged. Tokens get none.
- 256 random bits can't be guessed, so a limit protects no secret. Checking a token is one indexed read, no dearer than any other request. A limit by address would let anyone who shares one, behind a NAT or a CI provider's egress, keep working tokens out. ADR 0011 accepted that trade-off for 6-digit codes, which need it. Tokens don't.

## Consequences

- Scripts, CI, the CLI and the MCP server use the same API as the panel, over any protocol, from anywhere the API answers, with one header.
- A leaked token is limited by design:
  - A read-only one can read what its owner can, including server inventory.
  - A read-write one can also apply changes, each of which can be undone.
  - Neither can remove data, cut off access, or make or keep tokens alive. The owner revokes it in the panel, and the audit log shows each new address it was used from.
- The CLI can't run destructive commands, or manage sign-in and tokens, with a token. #22 needs a way to do those that a person confirms, such as a short-lived session from a device flow approved with a passkey. A token scope that allows destructive calls would need its own record.
- `dockwell token create` already makes join tickets for servers. The CLI must name its API token commands so the two can't be confused, or rename the join ticket command.
- Refusal counts, like ceremonies and stream tracking, live in memory, on one control plane. A highly available control plane needs shared state for them.
- The threat model in [docs/security/sign-in.md](../security/sign-in.md) covers tokens.
