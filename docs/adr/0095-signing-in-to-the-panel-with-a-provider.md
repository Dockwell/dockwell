# 0095. Signing in to the panel with a provider

- Status: Accepted. Builds on [ADR 0087](0087-private-routes.md), whose sign-in providers it uses for the panel too.
- Date: 2026-11-14

## Context

The PRD's Access section asks for "OIDC sign-in (GitHub, Google, any OIDC provider)" (P1), part of what Community includes, and #266 lists it as missing. #646 builds it. People sign in to Dockwell in these ways today:

- passkeys ([ADR 0011](0011-claim-and-sign-in.md));
- a code from an authenticator app, or a recovery code;
- a one-time link from `dockwell recover`;
- accepting an invite, which makes a passkey ([ADR 0049](0049-roles-and-invites-by-project.md)).

A team with an identity provider wants to sign in with it.

[ADR 0087](0087-private-routes.md) added sign-in providers, a `signin.provider` an admin saves in Settings, Integrations. Each is checked live against its discovery document, with its client secret sealed, so that a route can send a stranger to sign in. The agent runs the authorization code flow and checks the ID token itself.

What has to hold:

- **Nobody joins by signing in,** and no account is taken over by what a provider says of an email address. A provider can vouch for an address it doesn't own: a Google account can carry an address at a domain Google doesn't manage.
- **A provider that's down, deleted or moved locks nobody out.**
- **A risky action still needs a passkey,** as a provider anyone can have an account at is only as strong as that account ([ADR 0075](0075-how-often-a-passkey-is-asked-for-again.md)).
- **No login CSRF:** a callback signs in only the browser that began it.
- **Starting sign-in is bounded** for each address, as the passkey page is.

## Decision

### A provider can sign people in to Dockwell too

`SigninProvider` gains `panel` (9), "People sign in to Dockwell with it", off by default.

- Turning it on is an admin's edit, a change with a plan like the provider's other fields. It lets nobody in by itself, as only a linked member can sign in with it. So it isn't an opening (`ACCESS_OPENED`) on its own.
- Once it's on, the provider's sheet shows the address to register at the provider, with Copy: `https://<panel domain>/auth/provider/callback`. This is beside the routes' redirect addresses, which a provider takes as several.

### Only on the panel's own HTTPS domain

The provider brings someone back to the panel's domain ([ADR 0063](0063-the-panels-domain-set-from-the-panel.md)). On localhost, through an SSH tunnel, there's no address a provider could send a browser back to. So the sign-in page there offers passkeys and codes alone, and Profile and security offers no Link button. With no panel domain, the provider's switch says it needs one, with a link to Settings, General.

### Only members, linked once while signed in

- **A member links an account at a provider from Profile and security.** "Link <provider>" asks for their passkey, as adding a passkey does (`STEP_UP_ACTION_LINK_PROVIDER`, 250), so a stolen session can't make itself a way back in. It then goes through the provider's sign-in and comes back linked.
- **A link is a pair: the provider's issuer and the account's `sub`.** Beside them the link keeps the provider's ID, the member, and when it was made and last used.
- **An email address never picks an account.** What a provider says of an address is shown, as the account's name at the provider, and nothing else reads it.
- **One account at an issuer links to one member.** A member links at most one account per provider. Linking an account that's linked to someone else is refused, naming neither.
- **The link's state is checked as a sign-in's is**, with the flow on the server tied to the member who began it. Their session cookie isn't sent on the provider's way back, which comes from another site.

### Signing in

The sign-in page shows "Sign in with <display name>" for each provider with `panel` on, on the panel's domain.

- **Starting:** `BeginProviderSignIn` spends a token from the same per-address bucket as a passkey sign-in: a burst of 30, then one every 2 seconds, an IPv6 /64 counting as one address. With none left it's refused like a passkey sign-in, "too many requests from your address".
  - The flow is kept on the server for 10 minutes: its state, nonce and PKCE verifier, the provider and the site.
  - A flow cookie, `__Host-dockwell_provider_flow`, holds the flow's ID. It's HttpOnly, Secure and SameSite=Lax, so it comes back on the provider's top-level redirect.
  - The panel then goes to the provider.
- **The callback,** a GET at `/auth/provider/callback`:
  1. The state must match the flow cookie and a flow on the server, which is then used up.
  2. The code is exchanged for tokens, at most 4 exchanges at once and 30 a minute across the install, as [ADR 0087](0087-private-routes.md) bounds a route.
  3. The ID token is checked as the agent checks one: the same code, moved to `internal/oidc` for both.
  4. The (issuer, `sub`) pair must be linked to a member who still has a role.
- **Signed in:** a session starts with method `provider` (`SIGN_IN_METHOD_PROVIDER`, 7), audited as `auth.sign_in` with the provider's name.
- **Refused:** the refusal is audited as `auth.sign_in_failed`, and the page says "This account isn't linked to anyone here. Sign in with your passkey, then link it in Profile and security." It doesn't say whether the account is linked to someone who has lost their role.
- **The way back:** the callback answers with a small page that goes on to the panel. The session cookie is SameSite=Strict, and a browser doesn't send one set at the end of a redirect chain that began at another site on that chain's next request.

### Passkeys stay for risky actions

- A session begun with a provider has no passkey hour ([ADR 0075](0075-how-often-a-passkey-is-asked-for-again.md)): what asks for a passkey asks for it, at every level.
- Every member keeps a passkey, as claiming and accepting an invite make one. Unlinking never takes one away, and removing a member's last passkey is refused as before.
- So signing in with a provider is a way in for everyday work, and a provider that's down or gone leaves each member their passkey.

### Unlinking

- **A member unlinks from Profile and security.** It needs a signed-in session and no passkey, as it only takes a way in away. It ends every session that link began, so a member whose account at the provider was taken over ends what it opened in one step.
- **An admin or the owner unlinks a member's link from Members,** for a member who keeps their role but loses that account. It's audited, and it ends the link's sessions too.
- **Removing a member removes their links and ends their sessions,** as it does now.
- Each is audited as `provider_link.delete`. Linking is audited as `provider_link.create`.

### A provider deleted, turned off for the panel, or moved to another issuer

A link signs someone in only while its provider is there, has `panel` on, and has the link's issuer.

- **The plan says so** on the provider's row, as a note that doesn't stop it: "3 members sign in with it. They'd sign in with their passkeys instead."
- **Once it applies, the sessions those links began end,** as an admin who deletes a provider may be doing it because the provider can't be trusted.
  - Sign-in checks the providers itself, read again at most every 5 seconds, wherever a session begun with a link is used: a session whose link no longer counts is refused and ended there, audited with why.
  - So it holds however the change applied: from the plan, an approval, an undo or a rollback. The revision engine writes no sessions.
- **Deleting the provider deletes its links,** at that same check, as a provider's ID is never used again and there's nothing for them to come back to.
- **Turning `panel` off, or changing the issuer, keeps the links but they stop counting.**
  - Profile and security marks each one "Stopped working", with why: "<provider> no longer signs people in to Dockwell", or "<provider> moved to another issuer".
  - The member can unlink it and link again.
  - Turning `panel` back on, or the issuer back, makes them work again, as nothing about the account at the provider changed.
- **Another client keeps the links.** At most providers an account's `sub` is the same for every client. Where it isn't, as with Entra's pairwise subjects, the links stop matching, and members link again. Nothing ever matches the wrong account, as a `sub` belongs to its issuer.

### A public issuer

Only a linked member gets in, so a route's `any_account` and who-may lists don't apply to the panel. A link to a Google account is that Google account, whatever address it carries. A provider anyone can have an account at, such as Google, signs in to the panel as safely as any other.

### The API

Everything goes through the public API, and nothing is the panel's alone.

- `GetAuthState` gains `providers`: the ID and display name of each provider that signs in at this address.
- `BeginProviderSignIn(provider)` returns the address to go to, and sets the flow cookie.
- `ListProviderLinks` lists the member's own links, with their state.
- `BeginLinkProvider(provider, proof)` returns the address to go to.
- `DeleteProviderLink(id)` unlinks.
- An admin's `ListMemberProviderLinks` and `DeleteMemberProviderLink` come with Members, in slice 2.

The callback is a plain GET, a browser's way back, as file transfers have REST paths of their own. The CLI signs in with tokens and doesn't change.

### Numbers

- `SigninProvider.panel` is 9.
- `SIGN_IN_METHOD_PROVIDER` is 7.
- `STEP_UP_ACTION_LINK_PROVIDER` is 250, in a block of 250 to 259 for this.
- The links are a table of their own in a new migration, numbered after main's at the time.

### Slices

1. **The core:**
   - the switch, the redirect address, and `internal/oidc`;
   - linking, listing and unlinking in Profile and security;
   - the sign-in page's buttons, the callback and its limits;
   - what a deleted, turned-off or moved provider does to links and sessions, with the plan's note.
2. **Members and invites:** Members shows and removes a member's links, and an invite can be accepted with a provider, which links the account and still makes a passkey.
3. **GitHub:** GitHub's OAuth, which isn't OpenID Connect. Its user API names the account, keyed by GitHub's numeric user ID, never its login or email.

## Consequences

- There's one more way in, as strong as the account at the provider for everyday work, and no stronger: what's risky still asks for a passkey.
- The callback is a new path anyone can reach. It does nothing without a flow cookie and the flow on the server that a bounded start made, and each flow is used once.
- An admin registers one more redirect address at the provider. The sheet shows it with Copy.
- A provider's edit can end sessions. The plan says how many members it touches before anyone applies it.

## Tests

- **Rules:**
  - the plan's note counts the members a provider's deletion, turning off or move touches;
  - deleting a provider deletes its links.
- **The flow,** against a fake provider in Go:
  - a callback with no flow cookie, another browser's cookie, a used flow or a forged state is refused;
  - an account linked to nobody is refused, and so is a member with no role left, in the same words;
  - a start past the bucket is refused;
  - a linked member signs in, with method `provider`, and is asked for a passkey for a step-up.
- **Linking:**
  - it needs a passkey;
  - it's refused for an account linked to someone else;
  - unlinking ends the link's sessions;
  - removing a member removes their links.
- **On both stores:** the links, and what a provider's delete, turning off or move does to them and to sessions.
- **The panel:**
  - the sign-in page's buttons only on the domain;
  - Link and Unlink in Profile and security;
  - "Stopped working";
  - an e2e flow against a fake provider, to the panel signed in.
