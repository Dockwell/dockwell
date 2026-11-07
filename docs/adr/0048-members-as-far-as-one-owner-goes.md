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
- **How someone signs in, in detail, is theirs.** Everyone who reads the list sees, for each member, the kinds of sign-in that work for them now (`sign_in`: a passkey, an authenticator app) and when they were last active, as the mock's column does. Only the person asking gets their own passkeys' count and addresses, their codes and how many are left, whether code sign-in is locked, and their browsers. Deciding this before there is a second person keeps it from being a breaking change later.
- **What counts is what signs in now,** as signing in decides it (`auth.Service.SignInOf`): a passkey flagged as copied, or for an address the panel no longer serves, doesn't count, and an authenticator app doesn't count while code sign-in is locked.
- **The page shows that, and says plainly that inviting comes later.** It has no invite form, disabled or otherwise. What the owner may do is listed as it is, everything, rather than as a matrix of roles that don't exist. How someone signs in leads to Profile and security, where each person changes their own.
- **Nothing here changes access.** There is nothing to step up for yet.

### What more people and roles need

Each of these is its own decision, and together they are the record that replaces this part, for #263.

#### Grants: a role in a place

- **A person holds grants, each a role in a place:** (person, role, the install or one project). The roles are owner, admin, developer and viewer, as the PRD names them.
- **Owner and admin are install-wide,** granted on the install only. **Developer and viewer are granted per project,** or on the install, which covers every project, those made later included.
- **One person can hold different roles in different projects,** such as developer in `shop` and viewer in `blog`. Their role in a project is the highest grant that covers it. Viewers are scoped too: a viewer of one project reads that project, and nothing of another.
- **The store keeps grants in a table of their own,** `(user_id, role, project_id)`, with no project for the install. `users.role` goes. A unique index keeps exactly one owner grant.

#### Enforcing it: two checks and one filter

One interceptor can't enforce a project's scope: it sees which method is called, not what it touches. Proposing a change can touch several projects, applying acts on a change someone else proposed, and lists and streams return everything. So there are two checks, and one filter:

1. **The interceptor checks the lowest role a method needs anywhere.** Reading needs a viewer's grant somewhere, proposing a developer's somewhere, and what's install-wide, below, an admin's. Each method's rule sits beside its effect and its token scope (ADR 0016). A method with no rule is the owner's, so a new service is closed until it says.
2. **`internal/revision` checks every item's project,** when a change is proposed, against the proposer's grants, and again when it's applied, against whoever applies it, who may be someone else or may have lost the role since. A change with one item outside the person's grants is refused whole. Only `internal/revision` calls `Revise` already, so this is the one place every change passes.
3. **One shared filter narrows every list and stream** to what the person may see: changes, events, services, variables, logs, files, servers and the timeline. Each reads through it. A test walks every list and stream method in the generated service descriptors and fails on one that returns rows without it, as the `Revise` test does for writes.

A project outside someone's grants reads as not found, in a refusal as in a list, so its name doesn't leak.

#### Install-wide power is an admin's

A developer who can apply in one project could apply something as good as root on a server, such as a host path or Docker's socket (ADR 0021), and with it take the server and every project on it. A passkey step-up proves who is there, not that they may. So these are an admin's or the owner's, whatever someone's project grants say:

- plans as good as root, as the plan's `host_access` marks them;
- files on a server ([ADR 0032](0032-files-on-a-server.md));
- terminals, on a server or in a container;
- containers Dockwell didn't make (ADR 0031);
- firewall rules;
- servers and join tickets;
- backup destinations;
- variables and secrets set for the whole install;
- settings, members and invites, the audit log, and anyone else's API tokens.

A developer can read their project's secrets through a container they deploy, as the container is given them (ADR 0022). A role that deploys can read what it deploys. A viewer can't, as a viewer runs nothing.

#### A change of role takes effect at once

- **The role is read on each request,** from the store, not from the session, and a stream checks again every 30 seconds, as a token's does, and ends once the person may no longer read it.
- **Removing someone, or lowering their role, also ends** their step-up proofs, the 10-minute window a file step-up opens, a half-finished authenticator setup, and the invites they made that are still open.
- **A change they proposed is checked again** against whoever applies it, as above.

#### Invites

- **An owner or an admin makes an invite** for an email, a role and a place. It's 256 random bits, kept as a hash, shown once, works once, can be revoked, and expires after 7 days, as the mock says.
- **Dockwell sends no mail,** so the email binds nothing on its own: the inviter passes the link on, and the email is who the invite names in the list and on the claim page.
- **The claim page says who invited them, with what role, where.** The person makes their passkey there, as claiming does. Accepting works only at the panel's domain, or at `localhost` for an install without one, so the passkey is made at an address the install keeps serving, not a tunnel that goes away.
- **An admin can't invite above admin.** Nobody invites an owner: ownership moves by transfer.
- **An invite is access, not managed state,** so it's audited, not a revision (ADR 0011).

#### The owner

- **There is exactly one owner.** Ownership moves only by transfer, to someone who already signs in, with a passkey step-up. The old owner becomes an admin.
- **An admin can't remove or demote the owner,** nor touch the owner's passkeys, codes or sessions.
- **When the owner is gone,** having lost every way in or left, `dockwell recover` on the control server makes them a sign-in link, as today. With members, it can also move ownership to an admin who signs in, from the control server, since whoever runs it there controls the install already.

#### Removing someone

- Their sessions and streams end at once, as revoking a session does today, and their tokens are revoked with them.
- **Removing someone is planned first:** the plan lists each token it revokes, with when each was last used, so a CI job that will break isn't a surprise.
- Tokens that belong to no person, service accounts, are a follow-up of their own (#281).

#### Tokens act within their maker's grants

A token's scope is narrowed by its maker's grants when it's used, not when it's made, so a token loses what its maker loses. A token never holds more than its maker.

#### Refusals and the record

- **A refusal for a role has a reason of its own,** `needs_role`, and says which role the call needs, and where.
- **The audit log records** role refusals, invites made, revoked, expired and accepted, role changes, removals and ownership transfers: `member.invite`, `member.invite_revoke`, `member.join`, `member.role`, `member.remove` and `owner.transfer`, with who, from where, what was and what is. Those but refusals are on Activity's access events too.
- **The audit log and the members list are the owner's and the admins'.** The timeline narrows to what each person may see, through the shared filter.

#### Single sign-on comes after

OIDC in phase 3, SAML and SCIM in phase 4. Each maps the provider's people onto the same grants, and owners and admins keep a passkey to fall back on.

## Consequences

- Settings has a Members page that tells the truth: one person signs in, how they do, and that inviting comes later. There's no form that looks like it works.
- Teams still share an install the way they must today: one owner, with scripts on API tokens.
- The next step, #263, is a record of its own built on the part above, ADR 0049 if it needs one. It touches every service's authorization, and it's the largest change to access since ADR 0011, so it gets its own review.
