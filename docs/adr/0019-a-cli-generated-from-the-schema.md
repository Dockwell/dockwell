# 0019. A CLI generated from the schema

- Status: Accepted
- Date: 2026-11-04

## Context

PRD principle 1 says the UI, API, CLI, Git config and MCP server are views of one model, and anything doable in one is doable in all. [ADR 0003](0003-one-schema-protobuf-connect.md) decided that the CLI's commands are generated from the one schema, with custom method options marking what generators need. Issue #22 asks for `dockwell <noun> <verb>` for every API method, so the CLI and the API never drift, and for three things on top:
- mutating commands show the plan and ask before applying, with `--yes` to skip;
- output is for people by default, and `--json` for scripts;
- no `--on` flag is needed until there is more than one server (PRD principle 4, "never show a choice with one option").

What was already settled:
- Every method declares its effect, `(dockwell.v1.effect)`: read, plan, apply or destructive. Every unary method has one REST path under `/v1/` that follows Google's API design guide ([ADR 0015](0015-rest-and-openapi-from-the-schema.md)).
- Scripts and the CLI authenticate with API tokens. No token may call a destructive method, AuthService or TokenService ([ADR 0016](0016-scoped-api-tokens.md)), and none may apply a destructive plan ([ADR 0017](0017-the-change-api-and-the-agent-stream.md)), or one that gives a container more of its server ([ADR 0021](0021-the-docker-driver.md#host-access)).
- `dockwell token create` already makes join tickets for servers, so API token commands need another name.
- ChangeService proposes, applies, watches, undoes and discards changes, and lists drift. A request leaves out a scope while there is one to choose from, and gets the reason `choice` once there are several, with the options in its message.
- ADR 0017 left two things to this record: what a spec-reading method would look like, and how a client says it is the CLI, for the surface its changes record.

The binary is one (ADR 0007), so the CLI ships inside `dockwell` beside `run`, `join`, `token create`, `claim`, `recover`, `role` and `version`. Those stay as they are.

## Decision

### A protoc plugin writes the commands

- `cmd/protoc-gen-dockwell-cli` is a protoc plugin, in `buf.gen.yaml` beside the others, so `make generate` runs it. Its logic is in `internal/cli/cligen`.
- It writes `internal/cli/apicmd/apicmd.gen.go`, a description of each command:
  - its noun and verb;
  - its help, from the method's comment;
  - its effect, its REST rule, and its request's fields, each with its comment;
  - a typed call through connect-go.

  The directory holds only what the plugin writes, as buf empties it first. The file is AGPL-3.0, like the rest of the CLI: it describes commands, not a client anyone embeds ([ADR 0012](0012-apache-for-api-schema-and-clients.md)).
- A protoc plugin, not a program that reads the compiled-in descriptors, because only the plugin sees the schema's comments, which are the help.
- buf empties `gen/` before it runs plugins, so the plugin can't import the generated Go types. It reads `(dockwell.v1.effect)`, `(dockwell.v1.command)` and `(dockwell.resource.v1alpha1.sensitive)` from the descriptors in its request instead.
- Nothing about a method is written by hand. The shared machinery is in `internal/cli/apicli`, which turns descriptions into cobra commands and runs them:
  - flags from fields;
  - calls, with the plan flow and choices;
  - rendering, and errors.
- `make check-generated` covers `internal/cli/apicmd`, so CI fails when the commands are stale. `TestEveryMethodHasACommand` fails when a method of `dockwell.v1` has no command, which is what a schema changed without `make generate` gives. The plugin fails when two methods would get one command, when a method has no effect, or when a noun has no help of its own where it needs one.

### Names a person would type

- **The noun** is the collection in the method's REST path: `servers` for `/v1/servers/{id}`, `changes` for `/v1/changes/{id}:apply`, `drift` for `/v1/drift:revert`, `auth-state` for `/v1/authState`. ADR 0015's rules and its test already keep those paths resource-shaped, so the CLI inherits a reviewed naming.
- **A stream** has no REST path, so its noun is the rest of its name after the verb, plural: `WatchServers` is `servers watch` and `WatchChange` is `changes watch`.
- **The verb** is the path's custom verb, in kebab-case, such as `apply` for `:apply` or `revoke-others` for `:revokeOthers`. Without one, it is the first word of the method's name: `list`, `get`, `propose`, `remove`. So a person types `dockwell servers list`, `dockwell changes apply chg_…`, `dockwell drift list` and `dockwell drift revert`.
- **Where the rules give a name that is taken or misleading**, the method sets a new option, `(dockwell.v1.command) = {noun, verb}`. Three cases use it today:

  | Method | By the rules | Instead | Why |
  | --- | --- | --- | --- |
  | `GetVersion` | `version get` | `system version` | `dockwell version` is the CLI's own build |
  | `CheckClaim`, `BeginClaim`, `FinishClaim` | `claim …` | `claim-link …` | `dockwell claim` prints a claim link |
  | `CreateToken`, `ListTokens`, `RevokeToken` | `tokens …` | `api-tokens …` | `dockwell token create` makes join tickets |

  `TestCommandNamesDontCollide` fails when an API command takes the name of one of the server's own commands, or that name with an s, as `tokens` beside `token`.
- **Help** comes from the comments. The comment's first verb becomes a command: "ListServers returns every server" is "Return every server", and the short help stops at the first clause. Method names in comments, in a noun's help and in the API's errors become commands: "Follow them with WatchChange" reads "Follow them with dockwell changes watch". The long help ends with what the command does about plans, and its REST rule.
- **A noun's help** is its service's comment when the service's commands have that one noun, as ServerService's `servers` does. A service whose commands have several, as ChangeService's `changes` and `drift`, or AuthService's five, describes each with a service option, `(dockwell.v1.nouns) = {name, help}`, as the service's comment fits none of them alone. The plugin fails when one of those nouns has no help.
- `dockwell --help` lists the server's own commands, then the API commands, in two groups.
- The API commands are built only for a command line that isn't one of the server's own commands. `dockwell run`, which the control plane and the agent run as, never builds them, so they take none of a server's memory, and nothing in them can stop a server from starting.

### Flags from fields

- **A field named `id`** is the command's argument, as REST paths bind it: `dockwell changes get chg_…`. Every other field is a flag named after it in kebab-case, such as `--page-size`, with its comment as help.
- **A field's flag never takes one of the CLI's own**: `--url`, `--token` and `--json`, which every API command has; `--yes` and `--plan-only`, which mean the same wherever they are; `--help`; the global flags; and any flag its command has already, such as a kind's `--summary`, `--project`, `--environment` and `--on`. A field whose name would take one sets a field option, `(dockwell.v1.flag)`, to name its flag instead: a claim link's token is `--claim-token`, and a sign-in link's `--link-token`. Building the commands fails while a field's flag would take one, and `TestFieldFlagsAvoidTheCLIsOwn` walks every command to check. `dockwell changes propose --summary` is no exception: it sets the change's summary, which is what a kind's `--summary` sets too.
- **Values** follow each field's type:
  - numbers, strings and bools as typed;
  - an enum by its value without the prefix, such as `--scope read-write`, or by its full name;
  - a timestamp in RFC 3339, and a duration as Go writes one or in days, such as `90d`;
  - a repeated field by repeating the flag, and a map as `KEY=VALUE`;
  - any other message as JSON in the API's shape, `@file` or `-` for standard input;
  - a text field marked `(dockwell.v1.from_file)` as typed, `@file` or `-` too, as a Compose file is `--compose @compose.yaml`.
- Two fields of one oneof can't go together, and the error names both flags.
- Values are checked as they are parsed, so a mistake names its flag and is a usage error, with exit status 2.

### Commands for each kind of resource

- ChangeService takes edits of any kind, so `dockwell changes propose --edits '<json>'` can do anything, but no person types that. The plugin also writes commands for each kind in the spec an edit carries, the oneof of `dockwell.resource.v1alpha1.Spec`: `projects`, `environments`, `services`, `containers` and `firewall-rules`. Each has `create`, `update` and `delete`, with the resource's name as the argument.
- Their flags are the spec's fields, with fields of a message flattened, as `--app-image` for `Service.app.image`. A sensitive map, such as a container's environment, also takes `KEY` alone and reads the value from the CLI's environment, so a secret stays out of shell history and the process list.
- Each proposes one edit and goes on as any plan does. `update` sets the whole spec, as an edit does: a field left out is cleared, and the plan shows it before anything applies. The CLI checks the plan against the verb, so `create` of something that exists, or `update` of something that doesn't, discards its plan and says which verb to use.
- There is still no method that reads a spec, so there is no `get`, and no update of one field. Masking secrets while editing around them, which ADR 0017 left to #22 and #24, waits for a spec-reading method of its own.

### No `--on` until there is a choice

- The flags that say where a resource lives follow its kind's level:
  - none for a project;
  - `--project` for an environment;
  - `--project`, `--environment` and `--on` for a service, which runs on any number of servers, so `--on` repeats for each;
  - `--on` for a container or firewall rule, which is on one server, so `--on` takes one, and given twice is a usage error.

  All are optional, and the help says to leave them out while there is only one.
- When the API answers `choice`, the error now carries a detail, `dockwell.v1.Choice`, with the level, the options and the request's field that takes the answer, such as `edits[0].project`. It is a `google.rpc.Status` detail over every protocol, REST included, so the panel and the MCP server can ask the same way.
- On a terminal the CLI asks which, by number or name, sets the field, and sends the request again. Off a terminal it fails with the options and the flag to pass, such as "this organization has 2 projects, default and shop, so say which one you mean. Pass --project with one of them", with exit status 2.

### Plans before changes

- **A command whose method proposes a change** prints the plan, then asks "Apply this plan? (y/N)" on standard error. On yes, it applies the plan by its fingerprint, so what applies is what was shown, and follows it with WatchChange, printing each step as it runs and ends. On anything else, standard input ending included, it discards the change and says nothing changed.
  - The methods that propose one are ProposeChange, UndoChange, RevertDrift and AdoptDrift. The effect alone doesn't say so: DiscardChange is `EFFECT_PLAN` and returns a change too, the one it ended, so its command only prints it. `internal/cli/apicli` lists which plan methods that return a change propose one, and building the commands fails for one it doesn't list.
  - `--yes` applies without asking.
  - `--plan-only` leaves the change planned, to apply later or in the panel.
  - Off a terminal without either, it leaves the change planned and fails, saying to pass `--yes`.
  - A plan that changes nothing is discarded, and the CLI says so.
- **`dockwell changes apply`** shows the change's plan the same way before applying it.
- **Following a change**, after applying it or with `dockwell changes watch`, goes on until it is applied, failed or discarded. A watch that falls behind the change is ended by the control plane with the reason `watch_behind`, which asks for another: the CLI watches again, up to 10 times, and as each watch starts with the change as it is, nothing is missed. How long the change took is by its own times, from when it started applying to when its last step finished, so watching one that applied long ago says how long it took, not how long the watch did. A change with no steps that finished says how long it took only when this CLI applied it.
- **Other apply methods** ask before calling.
- **The plan** reads like the panel's plan sheet:
  - each resource that runs on no server, with its action and field changes;
  - then each server, offline or removed if it is, with its downtime, and each of its steps with action, effect, downtime, data loss and warnings, and its field changes;
  - then the counts, "1 to create · 0 to change · 0 to destroy", with replacements apart only when there are some, and the undo window.

  A sensitive field shows that it changed, never a value, which the API never sends anyway.
- **A plan no token may apply**, because it destroys data or may cut off access, isn't offered. The CLI says it needs a person, leaves it planned, and prints where the panel shows it, `<panel>/changes/<id>`. It exits with status 4, so scripts can tell "waits for a person" from a failure. The panel's plan screen (#24) serves that path.
- **A destructive method** (`EFFECT_DESTRUCTIVE`) isn't called: no token may, so the CLI says so and points to the panel, also with status 4. It says so before it has a token, so local access makes none just to refuse. When the CLI gains a credential a person confirms, such as a device flow approved with a passkey, these get a typed confirmation. That is its own decision.

### Output

- **For people:**
  - tables for lists, with sentence-case headers;
  - a server, a change and its plan, and drift each in their own layout;
  - anything else from its fields, labelled in words.
- **Times and sizes as the panel words them:** "3 min ago", then dates, "12 Oct", "in 5 hours"; 1024-based sizes with one decimal under 10, "1.2 of 4 GB"; enums as words, "Connected".
- **Questions and prompts go to standard error**, so standard output stays what was asked for.
- **`--json`** prints what the API returns as protojson, the shape REST returns: lowerCamelCase names, enums by name, unset fields left out. One response is indented. A command that receives several, such as a stream, or a plan that is applied and followed, prints each on its own line, as JSON Lines.

### Errors

- The API's messages are whole sentences with their fix (ADR 0017), and pass through as they are, with method names turned into commands.
- Reasons the CLI can fix better get its own words:
  - `token_rejected` says to make a new token and run `dockwell login`;
  - `token_scope` says the token is read-only, and how to get a read-write one;
  - `needs_session` adds the panel's address;
  - `misdirected` says which address to use.
- A control plane it can't reach is named, with what to check. A method the control plane lacks says it may be older than the CLI.

### Which control plane, with which token

The control plane and the token come from, in order:
1. `--url` and `--token`. The help says to prefer the environment or the config file, as a flag shows in the process list.
2. `DOCKWELL_URL` and `DOCKWELL_TOKEN`.
3. The config file `dockwell login` writes, `dockwell/config.json` in the user's config directory, mode 0600 in a 0700 directory. It holds a token for each control plane, by URL, and the one signed in to last, which commands use by default. A token is sent only to the URL it was stored for. The CLI refuses a config file others can read, and says to `chmod 600` it and revoke its tokens. A config file it can't use, because others can read it or it doesn't parse, is an error with its fix, never passed over for local access.
4. Local access, below.

More about how it calls:
- **The protocol** is Connect, through the same client code the API generates. Every call sends `Authorization: Bearer dwt_…` and the User-Agent `dockwell-cli/<version>`.
- **Plain HTTP** is refused anywhere but loopback, so a token never crosses the network in the clear. HTTP redirects aren't followed, so a token never goes to another host.
- **`dockwell login [url]`** checks the token's shape and checksum, checks that it works with a read, then stores it. The token comes from `--token`, `DOCKWELL_TOKEN`, a prompt that doesn't echo on a terminal, or the first line of standard input. The help says tokens are made in the panel under Profile and security, and that `dockwell token create` makes join tickets, not API tokens.
- **`dockwell logout [url]`** forgets the token, and says it works until it is revoked in the panel: a token can't revoke itself, as TokenService needs a session.

### Local access on the control server

On the control server, the CLI needs no setup.
- With no URL and no token from flags, the environment or a login, it looks in the data directory, the default or `--data-dir`, as `dockwell recover` does.
- When a control plane is there, and the user can read it, the CLI makes its own API token for the install's owner, and calls the control plane at the loopback address `dockwell run` recorded.

This is safe because reading that directory already gives full control of the install: its sealing key and its database. ADR 0011 relies on the same for `dockwell claim` and `dockwell recover`. Nothing new is reachable, and what local access does is limited and on record:
- **Read-only unless asked.** The token it makes is read-only, so a script running as root on the server can't change anything by accident. `dockwell login --local` makes a read-write one, which the CLI then uses. `dockwell logout --local` revokes the token and forgets it.
- **Short-lived.** A local token lasts 12 hours (`auth.LocalTokenTTL`). The CLI makes a new read-only one when it expires or the control plane rejects it, so revoking the read-write token in the panel takes write access away until someone asks for it again. When the control plane can't be reached, the CLI reads the address it records again, and tries once more only if it moved, as after a restart on another port, with the same token.
- **Kept where only its makers can read it.** The token is kept in `<data>/control/local-token.json`, mode 0600, so each command doesn't make one. Only those who could make it can read it.
- **On record.**
  - The token is named "Local access on hz-1, read-only" or "read-write", and listed on Profile and security like any other, where it can be revoked.
  - Its creation and revocation are `token.create` and `token.revoke` events, by `cli:<user>@<host>`, the actor `dockwell token create` and `dockwell claim` already record: the shell account on the control server that ran the command. The timeline shows it as a shell account, `mira@hz-1` ([ADR 0020](0020-the-event-timeline.md)).
  - Its calls are the token's, `token:tok_…`, and its changes, and every audit event its calls cause, come from the CLI.
- **Within ADR 0016's limits.** A local token is an ordinary token, so it can't apply a destructive plan, or one that gives host access, or manage sign-in, either.
- **Only for the owner.** An install with no owner refuses it, and says to claim it first.
- **Not for anyone else.** A user who can't read the data directory gets a message that says to use `sudo` or `dockwell login`. A user other than root looks in a data directory of their own by default, so when the system's, `/var/lib/dockwell`, has a control plane, or only root can look in it, the message says the same. A control plane on Postgres needs `DOCKWELL_DATABASE`, as `dockwell recover` does.
- **Never an upgrade.** Local access opens the control plane's state as it is, through `store.OpenExisting`: it never creates or migrates it, since a migration under a running control plane of another version would change its schema beneath it, as between an upgrade and the service's restart. A state at another schema version than the CLI's is refused, with what to do: restart the control plane so it runs this `dockwell`, or run the `dockwell` it runs. It reads what it needs and no more: the panel's address alone to find the control plane, and the sealing key only to make a token.

The other way considered was a Unix socket with the caller's credentials checked by the kernel (`SO_PEERCRED`). It needs no token in the database, but it needs a new listener, a new kind of principal in the guard and the interceptor, and its own scope and audit rules. Making an ordinary token reuses all of that unchanged. A socket can come later if minting tokens proves too heavy.

### The CLI as a surface

- `Surface` gains `SURFACE_CLI`. A call with an API token whose User-Agent starts with `dockwell-cli/` records its changes, and their audit events, as from the CLI. Any other call with a token is the API, and a session is the panel.
- One place decides, `auth.Client.Surface`, for a change and for every audit event a call causes, such as a token's first use from an address.
- What someone in a shell on the control server does with `dockwell`, such as making local access's token or `dockwell recover`, is from the CLI too, by the shell account that ran it. [ADR 0020](0020-the-event-timeline.md) puts both on the timeline.
- The surface is what a client says it is, for the timeline. Who is held to account is the actor, the token, which the client can't choose.

## Consequences

- Every method of the API is a command as soon as `make generate` runs, with no code to write. A new method needs a REST rule and an effect, which the schema's tests already ask for. It needs `(dockwell.v1.command)` only when its name would collide, `(dockwell.v1.flag)` on a field only when the field's flag would take one of the CLI's own, and a new service needs `(dockwell.v1.nouns)` only when its commands have several nouns. A new plan method that returns a change needs a line in `internal/cli/apicli` that says whether it proposes one.
- A new kind of resource gets `create`, `update` and `delete` from its spec. A new field gets a flag, and a field marked sensitive takes its value from the environment.
- The help is as good as the schema's comments, which now reach people on the command line. Comments should keep starting with the method's name and a verb. The short help is that verb's first clause, cut by a rule that knows grammar only roughly, so a comment whose first clause joins two nouns with "and" reads cut short.
- Commands that need a session, such as AuthService's and TokenService's, are there, as principle 1 asks, and answer with where to do it in the panel. They work once the CLI has a credential a person confirms.
- Scripts can rely on `--json` matching REST's shapes, on the exit statuses (0, 1, 2 for usage and choices, and 4 for what needs a person), and on `--yes` and `--plan-only`.
- Local access needs the binary the control plane runs, which a server that installed one `dockwell` has once the service restarts after an upgrade. Until then it refuses, and says to restart the service.
- Local tokens appear in the owner's token list, at most two working at a time, and count toward the 100 a person may have.
- The panel's plan screen (#24) is expected at `/changes/<id>`, the address the CLI prints.
- The MCP server can reuse the plugin's descriptions, `dockwell.v1.Choice` and the plan rendering.
