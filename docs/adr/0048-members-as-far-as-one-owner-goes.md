# 0048. Members, as far as one owner goes

- Status: Accepted
- Date: 2026-11-07

## Context

Issue #223 asks for Settings' Members: who can sign in and their roles, and inviting and removing people, "as far as the auth model allows today". `Members.dc.html` shows an invite form, five people with roles, sign-in by passkey or SAML, a matrix of what four roles may do, and single sign-on.

The PRD lists "owner, admin, developer, viewer roles scoped by project" at launch, OIDC in phase 3, and SAML and SCIM in phase 4.

The auth model has one person:

- [ADR 0011](0011-claim-and-sign-in.md): the first and only person comes from a claim link, and no other code path creates a user. `users.role` allows `owner` alone, and a partial unique index allows one owner.
- `Role` in the API has `ROLE_OWNER` alone.
- Every check in the control plane asks "is there a session or a token", never "may this person do this". A token acts for the person who made it (ADR 0016).

So inviting someone would give them everything the owner has, on every server. That isn't a member: it's a second owner, with nothing in the model to tell them apart.

## Decision

### What Members is today

- **`MemberService.ListMembers`** says who can sign in, oldest first: today, the owner. For each person it gives:
  - their name, email and role;
  - whether they're the one asking;
  - how many passkeys still sign in, and the addresses those work at;
  - whether they have an authenticator app, and how many recovery codes are left;
  - how many browsers they're signed in on, and when one was last used.
- It's a read, for a person signed in (`needs_session`). No API token reads it, as it names people and their emails.
- **The page shows that, and says plainly that inviting comes later.** It has no invite form, disabled or otherwise. What the owner may do is listed as it is, everything, rather than as a matrix of roles that don't exist. How someone signs in leads to Profile and security, where each person changes their own.
- **Nothing here changes access.** There is nothing to step up for yet.

### What more people and roles need

Each of these is its own decision, and together they are the members record that replaces this part:

1. **Roles in the store and the API.** `users.role` gains `admin`, `developer` and `viewer`, and `Role` gains their values. The unique index on the owner stays, so there is always exactly one owner.
2. **Invites, as claim links for someone else.** An owner or admin makes one for an email and a role. It is 256 random bits, stored as a hash, and works once, within days. It's bound to that email, and the person makes their passkey on accepting it, as claiming does today. An invite is access, not managed state: it's audited, not a revision (ADR 0011).
3. **Authorization in one place.** An interceptor maps each method's effect and service to the role it needs, as tokens' scopes do today (ADR 0016):
   - a viewer reads;
   - a developer plans and applies in the projects they're given, scoped as `scope` resolves them;
   - an admin does everything but ownership;
   - the owner adds transferring ownership and deleting the install.

   A method with no rule is the owner's, so a new service is closed until it says.
4. **Tokens act within their maker's role.** A token's scope is narrowed by its maker's role when it's used, not when it's made, so a token loses what its maker loses.
5. **Step-ups for access.** Inviting, changing a role, removing someone and transferring ownership each take a fresh passkey assertion, as new `StepUpAction`s.
6. **Removing someone ends their access at once.** Their sessions and streams close, as revoking a session does today, and their tokens are revoked with them. The last owner can't be removed. Ownership moves only by transfer, to a member who already signs in.
7. **The timeline and the audit log.** `member.invite`, `member.join`, `member.role` and `member.remove` are access events on Activity, and in the audit log with who, from where, what was and what is.
8. **Reading by role.** The audit log ([ADR 0047](0047-the-audit-log-for-a-person.md)) and the members list are the owner's and the admins'. The timeline narrows to what each person may see.
9. **Single sign-on comes after.** OIDC in phase 3, SAML and SCIM in phase 4. Each maps the provider's people onto the same roles, and owners and admins keep a passkey to fall back on.

## Consequences

- Settings has a Members page that tells the truth: one person signs in, how they do, and that inviting comes later. There's no form that looks like it works.
- Teams still share an install the way they must today: one owner, with scripts on API tokens.
- The next step is a record of its own covering points 1 to 7, which touches every service's authorization. It's the largest change to access since ADR 0011, and it gets its own review.
