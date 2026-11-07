# 0049. Roles and invites, by project

- Status: Accepted
- Date: 2026-11-07

## Context

The PRD makes roles a launch requirement: "owner, admin, developer, viewer roles scoped by project" (Access, P0), with "four built-in roles" in the free edition. [ADR 0048](0048-members-as-far-as-one-owner-goes.md) set the frame for #263: roles are grants of a role in a place, the install or a project. Project scope is held by the interceptor, by `internal/revision` and by one resolver. Install-wide power is an admin's, and the ADR settles invites, removal, the owner and refusals.

What ADR 0048 left to this record:

- what each role may do, method by method;
- how a role meets a token's scope (ADR 0016) and a step-up (ADR 0032);
- what a developer may do in production, as `Members.dc.html` says "Plans for production wait for an owner or admin", and the PRD's Connections say "Bind is not read";
- how the panel shows all this to an install of one person, which the PRD's scaling rules say has no roles UI.

## Decision

### What each role may do

A role holds in the places its grants cover (ADR 0048). Every method says the lowest role it needs, in a new method option, `min_role`, beside its `effect`. A test fails the build on a method without one, as it does for `effect`.

| | Viewer | Developer | Admin | Owner |
| --- | --- | --- | --- | --- |
| Read apps, plans, activity, logs and backups | Their projects | Their projects | Everything | Everything |
| Plan a change | No | Their projects | Everything | Everything |
| Apply or undo a change, restart, make a backup | No | Their projects, outside protected environments | Everything | Everything |
| Apply in a protected environment | No | No: it waits for an admin or the owner | Yes | Yes |
| Remove data: delete a backup, a service with its volumes, a project | No | Their projects, outside protected environments | Yes | Yes |
| Reveal a secret's value | No | Their projects, outside protected environments | Yes | Yes |
| Bind a variable, secret or connection to a service | No | Their projects; in a protected environment it's a plan that waits | Yes | Yes |
| What ADR 0048 calls install-wide: root-equivalent plans, files, terminals, servers, firewall, destinations, install-wide values, settings, the audit log | No | No | Yes | Yes |
| Invite, change roles, remove people | No | No | Up to admin, never the owner | Everyone |
| Transfer ownership | No | No | No | Yes |

- **Everyone reads logs in their projects,** as the mock says. A token still needs the logs grant (ADR 0025), as logs hold what apps print.
- **"Their projects" means a grant on the project, or on the install,** which covers every project.

### Protected environments

- **An environment is protected or open.** `Environment` gains `protection`: protected, open, or unset. Unset reads as protected for an environment named `production` and as open for any other, so every install's existing production is protected from the start, with no revision rewritten.
- **In a protected environment, a developer proposes and an admin or the owner applies.** The change waits as planned, on Activity and in the plan sheet, as "waiting for an admin". This is the mock's "Plans for production wait for an owner or admin", built on what Dockwell has: one person proposes, another applies, as the CLI's plans already do. It isn't the Organization edition's approvals and deploy windows, which add rules and policies on top.
- **Changing an environment's protection is an admin's,** checked by `internal/revision` as an item of its own.

### Bind is not read

- **A developer binds without reading.** A service names the variables and secrets it uses by reference (`specs.proto`), so binding a production secret is a plan that names it, and never shows its value. Setting a secret's value is the same: write only.
- **Revealing a value needs the role and a passkey,** and lands in the audit log. In a protected environment that is an admin's or the owner's.

### Roles and tokens

- **A token acts with the smaller of its scope and its maker's role,** read on each call, as ADR 0048 says. A read-only token of an admin reads; a read-write token of a developer plans and applies in that developer's projects, outside protected environments.
- **A protected environment's plan is applied by an admin or the owner,** in a session or with a read-write token they made. A developer's token proposes it, and it waits, as the developer's own would.
- **What needs a session still does** (`needs_session`), whatever the role.

### Roles and step-ups

A step-up proves who is there; a role says what they may do. Both are checked, and neither stands in for the other:

- an admin opening a file on a server still steps up (ADR 0032), and a developer who steps up still can't open one;
- inviting, changing a role, removing someone and transferring ownership each take a passkey step-up of their own: `STEP_UP_ACTION_INVITE` (110), `_CHANGE_ROLE` (111), `_REMOVE_MEMBER` (112) and `_TRANSFER_OWNERSHIP` (113).

### Invites

As ADR 0048 says, with these specifics:

- **The link is `/invite#<secret>`,** with the secret after `#`, so it never reaches a log, a proxy or a referrer. It works once and lasts 7 days.
- **Making one shows the link once,** to copy and send. Mail comes once alerts bring SMTP (#242).
- **The page it opens says who invited them, with what role, where,** and asks for their name and a passkey. That's the claim flow (ADR 0011) for someone else.
- **Pending invites are listed in Members,** with who made them and when they expire, and can be revoked.

### Who did what

Members' events are access events, on Activity and in the audit log, with a typed field of their own, `Event.member` (120). Its sentences name who, what role and where:

- `member.invite`;
- `member.invite_revoke`;
- `member.join`;
- `member.role`;
- `member.remove`;
- `owner.transfer`.

### One person

With one person and no pending invite, Members shows them as today, with an "Invite someone" button. The role, the place and the permissions table appear in the invite sheet. Members shows them once there is a second person or an invite, as the PRD's scaling rules say. A place to choose from shows only once there's more than one project, and otherwise reads "this install".

## Consequences

- A teammate gets an account of their own, so Activity says who did what, and a developer can ship to staging alone and to production with an admin.
- Every method gains `min_role`, and the descriptor test checks it with `effect` and the resolver (ADR 0048).
- `Environment` gains `protection`, read with a default by name, so nothing existing changes until someone changes it.
- A developer's production change waits for someone else. An install that wants developers to apply in production marks it open, as an admin's change.
- Custom roles, policy rules, deploy windows, SAML and SCIM stay in the Organization edition (PRD, P2). OIDC sign-in maps onto these roles when it comes (P1).
