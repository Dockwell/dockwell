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

#### Enforcing it: two checks and one resolver

One interceptor can't enforce a project's scope: it sees which method is called, not what it touches. A call can name an object by its ID; a change can touch several projects; applying, discarding and undoing act on a change someone else proposed; and lists and streams return everything. So there are two checks, and one resolver every other call goes through:

1. **The interceptor checks the lowest role a method needs anywhere.** Reading needs a viewer's grant somewhere, proposing a developer's somewhere, and what's install-wide, below, an admin's. Each method's rule sits beside its effect and its token scope (ADR 0016). A method with no rule is the owner's, so a new service is closed until it says.
2. **`internal/revision` checks every item, for everything done to a change,** against the person doing it: its project, whether its environment is protected ([ADR 0049](0049-roles-and-invites-by-project.md)), what it reaches beyond its project (`host_access`, below), and the names it takes, domains and server ports, which another environment may hold (below):
   - proposing it, against the proposer;
   - applying, discarding or undoing it, and rolling a service back, against whoever does that, who may be someone else, or may have lost the role since;
   - an undo or a rollback item by item, as the change it plans, as a new change is.

   For an item that moves an environment's protection, or moves something between environments, the stricter of before and after holds. A change with one item outside the person's grants is refused whole, and a change with one item in a protected environment waits whole. Only `internal/revision` calls `Revise` already, so every change passes here.
3. **One resolver says where any object belongs,** from its ID: its project, its environment, and whether that environment is protected. It covers a service and its revisions, a change, a backup and its volume, a variable or a secret, a route, a domain, a Compose project, a deployment's logs, an event. Every call that isn't a change goes through it:
   - every read by ID;
   - every list and stream, which keep only rows in the person's projects;
   - every write that isn't a revision, such as revealing a secret, or making, previewing, fetching or deleting a backup.

   An object in no project resolves to the install, which only an admin or the owner holds, with one exception: what a project needs to name, such as the servers it places on, is seen by name and state (ADR 0049).

   **Out of scope reads as not found.** A call that names an object outside the person's grants, by ID or in a list it passes, gets the same code and words as an ID that never existed, from the resolver, so neither the object nor its project's name leaks. A refusal from check 2 says the same of the item.

   Today that covers, among the rest, the calls by ID that a developer in `shop` holding one of `blog`'s IDs would otherwise reach: `VariableService.RevealSecret`; `BackupService.GetBackup`, `PreviewBackup`, `CreateBackup`, `FetchBackup` and `DeleteBackup`; `ChangeService.GetChange`, `DiscardChange` and `UndoChange` (check 2 as well); `ServiceService.ListServiceRevisions` and `RollBackService`; and `ComposeService.ExportCompose`. An ID being hard to guess is never what keeps it out.

**A test holds the line,** as the `Revise` test does for writes. It walks every method in the generated service descriptors, by its effect and the ID fields of its request, and fails on:

- one that names an object, by an ID field or in a list of them, without the resolver;
- one that returns rows without it;
- a write that changes managed state anywhere but `internal/revision`.

A new method with an ID field is closed until it resolves that ID.

#### Install-wide power is an admin's

A developer who can apply in one project could apply something as good as root on a server, such as a host path or Docker's socket (ADR 0021), and with it take the server and every project on it. A passkey step-up proves who is there, not that they may. So these are an admin's or the owner's, whatever someone's project grants say.

What a plan reaches beyond its project, as `host_access` names it:

- any server path, mounted into a container or bound by a volume, read-only or not;
- Docker's or containerd's socket;
- the server's own network;
- privileged mode, a capability beyond Docker's defaults, and a group a command joins;
- a volume mounted from a driver, such as an NFS or CIFS share, or a volume plugin;
- a network Dockwell didn't make, or one that reaches past the server's bridge, such as macvlan;
- a proxy a route trusts for where a request came from;
- the panel's domain, and any name under it;
- a route to a port on the server itself (`host_port`): it dials the server's own loopback, where anything listening, such as a database without a password or another panel, would go on the internet as if it were local, and it reaches other environments' published ports.

Anything else that isn't one project's:

- any item in no project, such as a container on its own;
- files on a server ([ADR 0032](0032-files-on-a-server.md));
- terminals, on a server or in a container;
- containers Dockwell didn't make (ADR 0031);
- firewall rules;
- servers and join tickets;
- backup destinations;
- variables and secrets set for the whole install;
- settings, members and invites, the audit log, and anyone else's API tokens.

**What a developer may use,** of what `host_access` also marks, are their own project's ordinary needs: a domain for a route, and a route over plain HTTP. A developer's route names a service in its own environment and a port of its container. Each still needs a person to approve it, as host access always has.

**A domain another project holds is refused when the change is planned,** as taken, with words that don't name the project that holds it: "shop.example.com is in use by a project you can't see. Ask an admin". So is any name under it, as with wildcard DNS a name such as `login.blog.example.com` would receive cookies `blog` sets for its whole domain. A domain another environment of the same project held within the undo window stays theirs to take back, as today.

**A server's ports are names too, held as domains are.** Placement is any server, so without this a developer in `blog`, or in `shop/staging`, could publish a port production publishes, or the one an admin's `host_port` route dials, and whichever container binds first after a deploy, a crash or a reboot would take production's traffic, and leave production's container unable to start.

- **A port on a server is held by an environment** when one of its services publishes it in its latest revision, on any address, `127.0.0.1` included, or when one of its `host_port` routes dials it.
- **An admin's `host_port` route to a port no service publishes** holds it for the route's environment.
- **Check 2 refuses, when the change is planned, a port another environment holds on that server,** as taken: "Port 5432 on web-1 is in use by another environment. Choose another port, or ask an admin". For another project, the words don't name it, as for domains.

A developer can read their project's secrets through a container they deploy, as the container is given them (ADR 0022). A role that deploys can read what it deploys. A viewer can't, as a viewer runs nothing.

#### Images on a shared server

A container starts from an image already on its server, whoever pulled it (`ensureImage` in `internal/resource/container`). On a server several projects share, that would let one project run a private image another pulled with its credentials. So once a project pulls with credentials of its own, a private image is pulled with that project's credentials every time a container is made from it, so the registry decides each time, and one project's credentials never serve another's pull. Until then a server pulls public images only, as it does today. The registries work, under Integrations' "Coming later", inherits this.

#### A change of role takes effect at once

- **The role is read on each request,** from the store, not from the session.
- **Any change to a person's grants closes all their streams and terminals at once,** through the registry that closes a revoked session's streams, and their tokens' streams too. The panel reconnects, and is checked again.
- **Every 30 seconds, an open stream or terminal is checked again,** as a token's is: the method's role, the resolver for what it names, and for a token, its maker's grants.
- **Removing someone, or lowering their role, also ends** their step-up proofs, the 10-minute window a file step-up opens, a half-finished authenticator setup, their terminals, and the invites they made that are still open.
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

- **A refusal for a role has a reason of its own,** `needs_role`, and says which role the call needs, and where, for a place the person can see, such as a viewer of `shop` deploying there. Anything out of their scope is not found instead, as above.
- **The audit log records** role refusals, invites made, revoked, expired and accepted, role changes, removals and ownership transfers: `member.invite`, `member.invite_revoke`, `member.join`, `member.role`, `member.remove` and `owner.transfer`, with who, from where, what was and what is. Those but refusals are on Activity's access events too.
- **The audit log and the members list are the owner's and the admins'.** The timeline narrows to what each person may see, through the shared filter.

#### Single sign-on comes after

OIDC in phase 3, SAML and SCIM in phase 4. Each maps the provider's people onto the same grants, and owners and admins keep a passkey to fall back on.

## Consequences

- Settings has a Members page that tells the truth: one person signs in, how they do, and that inviting comes later. There's no form that looks like it works.
- Teams still share an install the way they must today: one owner, with scripts on API tokens.
- The next step, #263, is a record of its own built on the part above, ADR 0049 if it needs one. It touches every service's authorization, and it's the largest change to access since ADR 0011, so it gets its own review.
