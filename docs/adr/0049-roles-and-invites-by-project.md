# 0049. Roles and invites, by project

- Status: Accepted
- Date: 2026-11-07

## Context

The PRD makes roles a launch requirement: "owner, admin, developer, viewer roles scoped by project" (Access, P0), with "four built-in roles" in the free edition. [ADR 0048](0048-members-as-far-as-one-owner-goes.md) set the frame for #263:

- roles are grants of a role in a place, the install or a project;
- a project's scope is held by the interceptor, by `internal/revision` and by one resolver;
- install-wide power is an admin's;
- it settles invites, removal, the owner and refusals.

What ADR 0048 left to this record:

- what each role may do, method by method;
- how a role meets a token's scope (ADR 0016) and a step-up (ADR 0032);
- what a developer may do in production, as `Members.dc.html` says "Plans for production wait for an owner or admin", and the PRD's Connections say "Bind is not read";
- how the panel shows all this to an install of one person, which the PRD's scaling rules say has no roles UI.

## Decision

### What each role may do

A role holds in the places its grants cover (ADR 0048): a grant on a project, or on the install, which covers every project.

| | Viewer | Developer | Admin | Owner |
| --- | --- | --- | --- | --- |
| Read apps, plans, activity, logs and backups | Their projects | Their projects | Everything | Everything |
| See the servers, destinations and domains their projects name, by name and state | Yes | Yes | Everything | Everything |
| Plan a change | No | Their projects | Everything | Everything |
| Apply or undo a change, restart, make a backup | No | Their projects, in open environments | Everything | Everything |
| Apply in a protected environment | No | No: it waits for an admin or the owner | Their own, or someone else's with a passkey | Same as admin |
| Remove data: delete a backup, a service with its volumes | No | Their projects, in open environments | Yes | Yes |
| Reveal a secret's value | No | Their projects, in open environments | Yes | Yes |
| Bind a variable, secret or connection to a service | No | Their projects; in a protected environment a plan that waits | Yes | Yes |
| Make a project | No | No | Yes | Yes |
| What ADR 0048 calls install-wide: root-equivalent plans, `host_port` routes, websites from a server folder, files, terminals, servers' inventory, firewall, destinations, install-wide values, settings, the audit log | No | No | Yes | Yes |
| Invite, change roles, remove people | No | No | Up to admin, never the owner | Everyone but themselves |
| Transfer ownership | No | No | No | Yes |

**Everyone reads logs in their projects,** as the mock says, production's included. A token still needs the logs grant (ADR 0025), as logs hold what apps print.

### Three classes of object

The resolver (ADR 0048) says where each object a call names belongs: **(project, environment, protected)**. An object falls in one of three classes:

1. **A project's own:** a service and its revisions, a change, a backup and its volume, a variable or a secret, a route, a Compose project, a deployment's logs, an event. Its project's grants decide.
2. **Install-level, but named by projects:** servers, as placement, a backup's `server`, `CheckDomain` and log filters name them; backup destinations, as a policy names them; and domains held by a project. A project role sees these **by name and state only**: a server's name, whether it's up, and its public address; a destination's name. Inventory, containers, processes, files and terminals stay an admin's.
3. **Install-level only:** everything else in no project, as ADR 0048 lists it. An admin's or the owner's.

**The resolver works in the interceptor,** for every call that isn't a change, from the request's own fields. An ID field, or a `project`, `environment` or `server` holding an ID, is placed by its ID. A `project` or `environment` named by name is found by name. A service named by name in `id`, or in `service` where a call reads one, as the CLI names it, is in the project and environment beside it. A website's ID, project name or domain resolves to its project and environment, and its request then names that project's ID. A server or a destination is named, not placed (class 2).

**Below an admin, a grant on the install covers every project's places, never the install's own.** Something in no project, such as a container Dockwell didn't make or an access event, needs an admin, whatever the call needs where projects are; out of all of a caller's grants it's not found, as anything out of reach is. Admins and the owner hold every place.

**A name never reaches past the caller's places.** Below an admin:

- something named by name without its project is found among the caller's places, as the API finds it, and the request is made to name where it is; found in several, they're asked which of theirs;
- a plan that names no project goes in their one project, for someone who holds projects alone, or they're asked which of theirs, not to the default project or a new one;
- a handler's own question of which project is answered with their one project, or asked again listing only theirs.

So no answer names a project out of reach.

**Lists and streams are narrowed for everyone below an admin.** Each element is placed by its change, its own ID, its environment, its resource's scope or what it's about, and left out when it isn't in their places, the install's included. A domain keeps the routes in their places. A change shows only when all its items are. A stream doesn't send a message the narrowing emptied, nor say a change out of reach was removed. A narrowed list's page token is sealed for its caller, as it says where the next page starts, which may be out of their reach; pages may come back short.

**What's about the one thing a request named is left whole,** as the request was checked: a change's steps, a service's log sources and chunks and its servers, a service's revisions, what a Compose file makes and the variables it exposes. A log call given a server reads a container Dockwell didn't make, which is the install's. An open stream reads the caller's grants again on each recheck, checks what its request named against them, and narrows what it sends next by them; it ends, not found, once what it named leaves their places. A recheck that can't read the grants keeps the stream, narrowed by what was read before, as the session's own check rides out a failed read; three in a row end it. A test fails on a list whose elements can't be placed, and another reads every project's list as a viewer of the install and as an admin, and compares them.

**Out of reach answers as missing does,** with the same code, reason and words. It isn't the same time: placing what was found takes a fraction of a millisecond more, measured at about 0.1 ms on loopback. IDs can't be guessed, and a name found by timing says only that the name is used somewhere, so this is accepted, and checked again if names come to matter more.

**Placement is any server,** for a developer as for an admin, until an admin can choose each project's servers or pool. That's the PRD Team tier's "client and project isolation".

### Every method says who may call it, in open and protected places

Every method declares three options beside `effect`, with no defaults:

- **`min_role`** (option 50104), the lowest role it needs where its objects are in an open environment, or in no environment;
- **`min_role_protected`** (option 50105), the lowest it needs where any of its objects is in a protected environment;
- **`reach`** (option 50106), what its objects are:
  - **anyone's:** calls before signing in, such as signing in, claiming and the version, exactly the interceptor's list of public calls. They need no role.
  - **self:** the caller's own sign-in, sessions and tokens, in `AuthService` and `TokenService` and nowhere else. These need any grant, and act only on the caller's own records. Signing out needs none, so someone with no grant left can still sign out, and only that.
  - **shared:** the same for everyone who holds a grant, such as the catalog's apps, read only.
  - **project:** a project's own objects, class 1 above.
  - **named:** class 2, read or checked, never written. Below an admin, a response holds names and states only: a server's ID, name, whether it's up, when it was last seen, whether it's the control server and its public addresses; a destination's name and when its last backup got there or didn't. Inventory, stats, history, builds, ingress, endpoints, buckets and prefixes stay an admin's.
  - **install:** class 3, an admin's.

Plan methods whose writes go through `internal/revision`, such as `DeployService`, `RestartService`, `UndoChange` and `RestoreBackup`, declare developer in both places, so a developer's protected change gets as far as check 2, which makes it wait. Writes that don't go through it, such as making or deleting a backup, declare admin where protected. A read that needs a session, such as revealing a secret, declares admin where protected.

A viewer may make an API token. It acts as a viewer does, so it only reads, whatever its scope.

`CheckDomain`, which resolves a name through the control server, is a developer's: only adding a domain asks it.

The interceptor checks `min_role` against the caller's highest grant anywhere, as a first gate (ADR 0048, check 1). A call outside `dockwell.v1` with no rule is the owner's, except gRPC's health and reflection. A REST path is checked the same way before Vanguard reads its query or body, so someone signed out, or whose role doesn't cover the call, learns that and not what's wrong with the request ([ADR 0015](0015-rest-and-openapi-from-the-schema.md)). The resolver then checks both roles against the grants on each object's place (check 3). `internal/revision` checks each item of a change (check 2). Each check uses the stricter of an item's place before and after the change.

**The descriptor test fails the build on:**

- a method without `min_role`, `min_role_protected` or `reach`, reads included;
- `min_role_protected` below `min_role`;
- reach anyone's on any call but the interceptor's public ones, or a public call without it;
- a role that needs nothing, outside the public calls and signing out;
- reach self outside `AuthService` and `TokenService`;
- `min_role` viewer with any effect but read, outside the caller's own;
- any write below developer, outside the caller's own;
- install's own objects below admin;
- named objects that write, or shared objects with any effect but read;
- a read that needs a session below admin where protected;
- a method that names an object by an ID field, or in a list of them, without the resolver (ADR 0048). `BeginStepUpRequest.target` counts as an ID field, and so does `server`, which, on the log calls, reads a container Dockwell didn't make: the install's.

**Every method's options are in a table checked in beside the test,** `internal/control/testdata/roles.txt`, a line each, so a change to who may call a method shows in review as a changed line.

### Protected environments

- **An environment is protected or open.** `Environment` gains `protection`: protected, open, or unset.
- **Unset reads as protected for an environment named `production`,** and open for any other. That rule is only for environments that exist today, so no revision is rewritten.
- **From now on, every environment is created with its protection written,** protected for `production`, open otherwise. So an import's `prod`, `live` or `main` doesn't fall to a rule by name. Creating one with other than that default is an admin's, as is changing it later. A rename, if one comes, writes the old protection explicitly.
- **A name isn't protection.** `display_name` can make `staging` read "Production", so the panel marks protection on its own, beside the name.
- **The first invite lists each environment's protection,** for the owner to confirm before anyone else signs in.

**Enforcement is where every other check is:**

- `internal/revision` checks each item's protection, at propose, apply, discard, undo and rollback, by the stricter of before and after. A change with any item in a protected environment waits as a whole.
  - It checks every item of every change, wherever the change comes from: ProposeChange's edits, an undo, a restore, a catalog install, a Compose import. Out of reach is not found, as the interceptor says it. A project, and anything in no project, is an admin's to make, change or delete.
  - The API carries the caller, with the grants the gate read, in each call's context, and the engine reads it there. The control plane has the engine refuse a person's change that comes without one. Dockwell's own changes aren't checked.
  - **Reach comes first.** Below an admin, the engine checks where each edit goes before anything the edit asks of it, so out of reach answers as missing does, a scope that isn't there answers the same, and no check after it describes another project: what an environment still holds, or which service a route can't find there. A project or an environment an edit names by name is found among the caller's projects alone.
- The resolver gives every other call its object's protection, for `min_role_protected`. That covers revealing a secret, and making, fetching or deleting a backup.
- What a plan reaches on its server that ADR 0048 lists is an admin's to apply, whatever a project's grants say, as it reaches the server every project on it shares: any server path, read-only or not, Docker's socket, the server's network, privileged mode, a capability or a group, a volume from a driver, a network Dockwell didn't make or one past the bridge, a proxy a route trusts, the panel's domain, a port on the server itself (`host_port`), a container Dockwell didn't make, and anything else as good as root. `internal/revision` checks it per item as a change applies. Other host access (ADR 0021), such as claiming another environment's domain, serving plain HTTP to anyone or turning HTTPS off, stays a person's to approve, with the fingerprint and a session, in a project they may apply in. `ApplyChange` asks the engine's check before any passkey, so nobody confirms what their role refuses.
- **Access kept while it runs something else is an admin's too.** Whoever changes what runs under access an admin gave gets what it reaches, so the plan records, beside what an item gains, what it keeps (`Keeps`), and only an admin or the owner applies it:
  - under what's as good as root (ADR 0032's list), any change to what runs: its image, another tag of the same repository included, as whoever can push to that repository would otherwise get root; pulling the same mutable tag again, which can run a newer image without changing the spec; its command, entrypoint and environment; and the access itself;
  - under the rest of what ADR 0048 lists, a server path, a capability, a device, the server's network and the others, another repository, command (including an exec health check) or entrypoint. Another tag of the same repository stays a developer's: that's the ordinary deploy, and the admin trusted that repository with that access when they gave it.

  It asks no passkey and shows no root warning; it decides only who may apply. The plan says why it waits.
- **A volume Dockwell didn't make waits for an admin,** below an admin, unless its environment mounts it already. It may be another project's data, as a taken-on stack keeps its volumes' names as external ones, and the stack keeps deploying. Placing taken-on volumes in their project is planned (#368).
- **Names.** A domain another project holds, or any name under it, is refused below an admin, as ADR 0048 says, in words that name neither the project nor what serves it: "login.shop.example.com is under shop.example.com, in use by a project you can't see". So is a port another project's service publishes on that server, before the server's own check, which names what holds it. A port a `host_port` route dials holding it, as ADR 0048 has it, is planned (#367).
- Installing from the catalog makes a project of its own, so it's an admin's (`InstallCatalogEntry` is ADMIN/ADMIN/INSTALL), and the refusal says why.

### Waiting for an admin in production

- **A developer proposes in a protected environment, and the change waits** as planned, on Activity and in the plan sheet, as "waiting for an admin".
- **An admin or the owner applies it** in a session, with the fingerprint of the plan they reviewed, which is required there, and a passkey step-up bound to the change and that fingerprint: `STEP_UP_ACTION_APPROVE_CHANGE` (114). That's the PRD's "approve with a passkey". The method that does it needs a session (`needs_session`), so no token approves anyone else's change.
- **An admin's or the owner's own change** in a protected environment applies as any change does, in a session or with a read-write token they made, so CI can deploy production with an admin's token.
- **How it shows.** A planned change with an item in a protected environment reads `protected`. For whoever reads it, it also says what applying asks of them, as `ApplyChange` would answer: `approves`, when applying it approves someone else's change, whether they planned it in the panel or with an API token, as the CLI does; and `waits`, why their role doesn't cover applying it, in a sentence that says why, who applies it, and to leave it planned for them. Someone below an admin who plans one leaves it for an admin from the plan sheet, rather than discarding it, and Activity says it waits for one. A step-up refusal says which step-up it takes, in a `StepUpNeeded` detail: for approving, `STEP_UP_ACTION_APPROVE_CHANGE` with the target `chg_…:fingerprint`. One passkey approves the plan, root's included. "Someone else's" means another person's: a plan Dockwell made needs no approval.
- **Discarding someone else's plan** is the proposer's, a developer's of the project in an open environment, or an admin's in a protected one. What the plan would reach on a server doesn't count, as discarding runs nothing there.

This is the mock's "Plans for production wait for an owner or admin", built on what Dockwell has: one person proposes, another applies, as the CLI's plans already do. It isn't the Organization edition's approval counts, approver rules and deploy windows.

### Bind is not read

- **A developer binds without reading.** A service names the variables and secrets it uses by reference (`specs.proto`), so binding a production secret is a plan that names it, and never shows its value. Setting a secret's value is the same: write only.
- **Revealing a value needs the role and a passkey,** and lands in the audit log. In a protected environment that is an admin's or the owner's (`min_role_protected`).

### Roles and tokens

**A token may make a call when all four hold,** checked on every call, and for each project the call touches:

1. its maker's grants allow the call there, as they stand now;
2. its scope allows the call's effect (ADR 0016);
3. the call isn't `needs_session`;
4. the token has any grant the call needs, such as reading logs.

So a token never does more than its maker could at that moment, and loses what its maker loses. Nobody makes a token for someone else.

### Roles and step-ups

A step-up proves who is there; a role says what they may do. Both are checked, and neither stands in for the other:

- an admin opening a file on a server still steps up (ADR 0032), and a developer who steps up still can't open one;
- inviting, changing a role, removing someone, transferring ownership and approving someone else's protected change each take a passkey step-up of their own: `STEP_UP_ACTION_INVITE` (110), `_CHANGE_ROLE` (111), `_REMOVE_MEMBER` (112), `_TRANSFER_OWNERSHIP` (113) and `_APPROVE_CHANGE` (114);
- **each is bound to what it's for.** An invite's proof names the email, role and place, a role change's names the member, role and place, and an approval's names the change and its fingerprint, so a proof can't be spent on anything else.

### The owner, and people with no grant

- **The owner can't remove or demote themselves:** they transfer ownership first, and then become an admin (ADR 0048).
- **The owner's grant changes only in a transfer's transaction,** held by a check in the store as well as the unique index: granting the owner's role, changing what the owner holds on the install and taking it back are refused, and `TransferOwnership` is the one way. Admins can't touch the owner's passkeys, codes, sessions or tokens.
- **`dockwell recover --owner EMAIL`** makes someone who signs in already the owner, from the control server, where whoever runs it controls the install already. An install with people and no owner, which only an older Dockwell could leave, says to run it.
- **A transfer revokes the local token** the CLI made for the old owner ([ADR 0019](0019-a-cli-generated-from-the-schema.md)), as it acted for them.
- **Removing someone** turns off their sign-in, and deletes their passkeys, codes and sessions, besides revoking their tokens. Their pending changes are listed in the removal plan, and discarded.
- **A project's grants go with it** when it's deleted ([ADR 0036](0036-deleting-a-project-takes-its-environments.md)). Its plan names who loses their last grant.
- **Someone with no grant left can only sign out.**

### Invites

As ADR 0048 says, with these specifics:

- **The link is `/invite#<secret>`,** with the secret after `#`, so it never reaches a log, a proxy or a referrer. The page clears it from the address bar with `history.replaceState` as soon as it reads it. It works once.
- **It lasts 7 days for a developer or a viewer, and 24 hours for an admin,** whose link is a bearer credential to the install.
- **Opening the page never spends it.** Only finishing the passkey does.
- **Making one shows the link once,** to copy and send. Mail comes once alerts bring SMTP (#242).
- **Invites need the panel's domain,** and the link always uses it. The invite sheet says so on an install without one.
- **The page says who invited them, with what role, where,** and asks for their name and a passkey. That's the claim flow (ADR 0011) for someone else.
- **Accepting only ever makes a new person.** The page refuses while someone is signed in: "You're signed in as Mira. Sign out, or open the link in a private window". An invite to an existing member's email is refused, pointing to Members.
- **The email is kept as unverified,** and Dockwell never links an OIDC identity to an account by an unverified email.
- **Members lists pending invites,** with who made them and when they expire, and each can be revoked. A `member.join` shows the address it came from, with Remove beside it.
- **Nobody is invited as owner,** though the mock's invite form offers Owner: ownership moves by transfer (ADR 0048).

### Who did what

Members' events are access events, on Activity and in the audit log, with a typed field of their own, `Event.member` (120). Its sentences name who, what role and where:

- `member.invite`;
- `member.invite_revoke`;
- `member.join`;
- `member.role`;
- `member.remove`;
- `owner.transfer`.

**Role refusals go in the audit log** as `role.refuse`, at most once an hour for each person or token and method, as ADR 0016 does for refused tokens, with a count of the rest. Its detail names the method, the role it needs, the address and what the caller held. A stream that ends because a grant changed isn't audited again: the change to the grant was.

**A change reads as not found** unless every one of its items is in the reader's scope.

### One person

- **With one person and no pending invite, Members shows them as today,** with an "Invite someone" button. The role, the place and the permissions table appear in the invite sheet. Members shows them once there is a second person or an invite, as the PRD's scaling rules say.
- **Settings' Protection column hides until there's a second person** too.
- **A place to choose from shows only once there's more than one project,** and otherwise reads "this install".

### What roles don't fence

Roles fence what Dockwell does for a person. They don't change what servers share:

- **Viewers read production's logs,** which hold whatever its apps print.
- **Projects on one server share its kernel,** and can reach each other's published ports through the server's address. Projects that mustn't touch belong on servers of their own.
- **Once developers build images,** the builder that runs privileged on a server is a trust boundary of its own (#292).

## Consequences

- A teammate gets an account of their own, so Activity says who did what, and a developer can ship to staging alone and to production with an admin.
- Every method gains `min_role` and `min_role_protected`, and the descriptor test checks both, with the class an object is in and the resolver (ADR 0048).
- `Environment` gains `protection`, written on every new environment and read with a default by name only for the ones that exist.
- A developer's production change waits for someone else, who approves it with a passkey. An install that wants developers to apply in production marks it open, as an admin's change.
- Custom roles, policy rules, approval counts, approver rules, deploy windows, SAML and SCIM stay in the Organization edition (PRD, P2). OIDC sign-in maps onto these roles when it comes (P1).
