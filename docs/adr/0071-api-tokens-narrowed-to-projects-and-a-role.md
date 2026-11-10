# 0071. API tokens narrowed to projects and to a role

- Status: Proposed
- Date: 2026-11-10

## Context

The PRD's P0 Access row asks for "scoped API tokens". [ADR 0016](0016-scoped-api-tokens.md) scoped them by effect: read only, or read and write, plus grants such as reading logs. [ADR 0049](0049-roles-and-invites-by-project.md) made a token act as its maker does at that moment, so it never does more than they could and loses what they lose. ADR 0016 also said finer scopes, "such as per project", would come.

Today a token reaches everything its maker does. That costs three things:

- **A CI token for one project reaches every project,** and for an admin, the install's own: its servers, settings and every project's apps. A token leaks more often than a passkey (ADR 0016), and when it does, all of that is exposed.
- **An admin's token applies in a protected environment** without anyone approving, which ADR 0049 allows on purpose, so CI can deploy production. A team that wants CI's production deploys to wait for a person's yes has no way to ask for it, short of making the token from a developer's account.
- **An agency or a team with clients** (the PRD's Team persona: "client and project isolation") can't give each client's pipeline a token that reaches that client's project and nothing else. Servers stay shared: placement is any server, for a developer as for an admin, until an admin can choose each project's servers (ADR 0049), so a client's token still places containers on servers other clients use. Per-project servers come later.

Tokens elsewhere are narrowed this way: a GitHub fine-grained token names its repositories and what it may do in them.

What bears on it:

- **Places are projects.** Grants are a role on the install or in one project, and an admin holds the install only (ADR 0049). Every method declares its reach: anyone's, self, shared, project, named or install.
- **Lists and streams are already narrowed** for anyone below an admin, element by element, and what's out of reach answers as missing does (ADR 0049).
- **Tokens can't manage sign-in or tokens** (ADR 0016), so a token can't widen itself or make another.
- **Some of a project's changes reach past it.** ADR 0048 makes an admin's what a plan reaches on its server beyond its project: a server path, a volume driver, a network past the bridge, a trusted proxy, a route to a port on the server, or a change that keeps such reach while it runs another repository, command or entrypoint. On main, only new host access needs a person; a change that keeps it needs only an admin, so an admin's token applies it.
- **Who owns a token is a separate question.** #281 asks for tokens that belong to no person, so a CI job doesn't break when its maker leaves. This record decides what a token reaches. A service account would take the same two narrowings, with grants of its own in place of a maker's.

## Decision

### Two narrowings, chosen when a token is made

- **Its projects:** every project its maker holds, now and later, which is the default and what every token does today; or a list of projects its maker holds something in when it's made.
- **Its role:** its maker's, which is the default; or developer or viewer, which caps what it does in every place at that role.

Both are fixed for the token's life, as its scope is: a token that should reach more, or less, is a new token. Neither widens anything. ADR 0049's four checks still hold on every call, and these add two more.

### What a token narrowed to projects reaches

By each method's reach:

- **Anyone's and self:** unchanged. A token can't call self methods anyway, as they are `AuthService` and `TokenService`.
- **Shared:** unchanged, as for anyone with a grant.
- **Project:** only objects placed in its projects.
  - A request that names no project is made to mean the token's one project, or refused with a choice listing only its projects. That is what someone who holds projects alone gets today.
  - Lists and streams are narrowed to its projects, as they are below an admin, whatever its maker's role. What a list holds on a server alone, such as a firewall rule, is the install's, so it's left out.
  - An object outside its projects answers as missing does, with the same code, reason and words (ADR 0049).
- **Named:** names and states only, as below an admin, whatever its maker's role.
- **Install:** refused with a new reason, `token_places` (permission denied, 403). Its message names the token's projects and says a token for the whole install can do this. The method's existence isn't a secret, so this doesn't answer as missing.

### What a narrowed token holds

**A narrowed token's grants are project grants.** Its maker's grant on the install counts, capped at its role, as that role in each of its projects, and never as a grant on the install. So:

- **What reaches past its project is an admin's on the install,** which a narrowed token never is. Where ADR 0048 and ADR 0049 make something an admin's because it reaches what every project on a server shares (what a plan reaches on its server, a change that keeps such reach while it runs something else, a volume driver, a network past the bridge, a trusted proxy, a route to a port on the server), a narrowed token's change waits there for a person, as a developer's does. So a narrowed token alone never changes a service that keeps server-wide access.
- **Its admin counts in one place:** applying in its own projects' protected environments, so CI can still deploy production with an admin's narrowed token (ADR 0049). Anything else that's an admin's, such as an environment's protection, waits for a person too.
- **Running a task is refused** with `token_places`. A shell task runs as root on its servers (ADR 0053), which reaches every project there.
- **Lists and named objects** are narrowed and trimmed as for anyone below an admin, as both key on grants on the install, and `token_places` covers the first gate, which checks the highest grant anywhere.

So an admin's token narrowed to `shop` deploys, plans and reads in `shop`, and applies in `shop`'s production. It sees the servers by name and state only, can't read or change anything of another project or of the install's own, and what it plans that reaches past `shop` waits for a person.

### What a token capped at a role does

- **Each of the maker's grants counts as the lower of it and the cap.** An admin's token capped at developer acts as a developer on the whole install, which is a developer in every project. Narrowed to `shop`, it is a developer in `shop` alone.
- **In a protected environment,** a token capped at developer plans, and its change waits for an admin or the owner to approve it, as a developer's does (ADR 0049). That's how a team has CI deploy production only with a person's yes. An uncapped token made by an admin or the owner applies there, as ADR 0049 lets it, within its projects.
- **A change a narrowed or capped token plans is someone else's to everyone, its maker included.** On main, a token's change counts as its maker's own, so the maker would apply it in the panel with no passkey and no fingerprint, and Activity would show no approval. Instead, a person who applies it approves it, with the plan's fingerprint and `STEP_UP_ACTION_APPROVE_CHANGE` (114), its maker as anyone else. The token itself still applies its own change where it may. A token that isn't narrowed keeps main's rule.
- **What it's refused says the cap is why:** "this API token acts as a developer at most", with a token without the cap as the fix, not "ask an admin to change its maker's role". The same goes for what a waiting change says.
- **Capped at viewer,** it only reads, whatever its scope, as a viewer's token does.
- **The caps offered are below its maker's highest role.** A cap at the maker's own role is no cap. Admin isn't offered: it would be no cap for an admin, and an owner's token does nothing an admin's can't, as everything only the owner does needs a session.

### Making one, seeing it, and the audit log

- **`CreateToken`** takes `projects`, each an ID or a name, kept as IDs, up to 100, and `role`: developer, viewer, or unset for the maker's.
  - A project the maker holds nothing in is refused in the same words as one that isn't there, as out of reach answers as missing (ADR 0049). A cap at or above their highest role is refused too, as it would be no cap.
  - The step-up for making a token keeps no target. With the same proof, a person could make a token that isn't narrowed at all, so a narrowed one asks no more.
- **`Token`** carries its projects, each with its ID and its name as it is now, and its role. `ListTokens` shows them. A project that has been deleted shows as gone, and one its maker no longer reaches shows as out of reach, not by its new name.
- **The panel's API tokens page** asks both when a token is made, with the defaults first, under the design system's interaction principles:
  - Projects: "Every project you hold", or projects picked from a list of the maker's.
  - Acts as: "Your role", Developer or Viewer. Developer shows one line on what happens in a protected environment.
  - Each token's row says where it acts and as what, such as "shop, blog · developer" or "Every project you hold".
- **The CLI** gets `--project`, which repeats, and `--role` from the schema, like every other field, and the MCP server will when it comes.
- **`token.create`'s detail** names the projects and the role. A call refused for a token's places is audited as `role.refuse` is, at most once an hour for each token and method with a count of the rest (ADR 0049), naming the token's projects.
- **The store** gives `api_tokens` two columns, read in the query that already checks a token: `projects`, NULL for every project its maker holds and otherwise the IDs it names, so a list that's emptied, by a cleanup or a hand edit, reaches no project rather than every one; and `role`, empty for the maker's and held to developer or viewer. Tokens made before this have NULL and empty, so they act as they do today. A narrowed token whose projects are all deleted reaches none, which a test checks.
- **An older control plane can't widen a narrowed token:** it refuses a store with a newer schema (`internal/store/migrate.go`), so it can't open it at all. ADR 0057's bundles carry the new columns.

### What changes a narrowed token's reach later

- **A project its maker loses** is lost to the token too, as it acts as its maker does now.
- **A project created later** is reached by a token that isn't narrowed, if its maker reaches it, and never by a narrowed one.
- **A project deleted** stays on the token by ID and reaches nothing. A token whose projects are all gone reaches only what anyone may call, as it holds no grant.
- **The maker's role dropping below the cap** lowers the token with it: the lower of the two always applies.
- **The maker's role rising** raises an uncapped token with it, within what a narrowed token holds above: a developer made an admin gives their narrowed token an admin's say in its projects' protected environments. A capped token stays at its cap. In the panel, "Your role" reads as the maker's role as it changes, not as it is now.

### Not decided here

- **Narrowing to an environment,** such as production alone. Grants are by project, so it would need a place below a project. It waits until someone asks for it.
- **Tokens that belong to nobody** are #281.
- **Grants that aren't places,** such as the PRD's `connections:read`, stay grants on top of a scope, as reading logs is (ADR 0016).

### Slices

1. The store, `CreateToken` and `Token`, the two checks in the interceptor and the narrowing of lists and streams, `token_places` (tasks included), the revision engine's rules for a narrowed token's changes (what reaches past its project waits, and applying one is an approval), the audit details, and the CLI's flags. Every check has a test that fails when the check is removed.
2. The panel's API tokens page: the two choices when making a token, and where each token acts and as what.

## Consequences

- A leaked CI token made for one project exposes that project, and if capped, only what a developer may do there. What its project's services reach on their servers stays what a person approved: the token can't change it or run a task there without a person.
- A token's change that waits is applied with a passkey and the plan's fingerprint by whoever applies it, its maker included, so a cookie stolen from the maker's browser can't apply it alone.
- A team can have CI deploy production only after an admin approves, without a developer's account to make the token from.
- A narrowed token's grants are read as project grants on every call, so every check that places a call works as it does for someone who holds projects alone, at the same cost. The interceptor adds `token_places`, and the revision engine the two rules for a narrowed token's changes.
- Tokens made before this, and every token made with the defaults, behave as they do today.
- A token's reach can't be edited. A person makes a narrower one and revokes the old, as they do to change a scope.
