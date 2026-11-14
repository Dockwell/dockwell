# 0096. A taken-on container's values, kept by name

- Status: Accepted
- Date: 2026-11-14

## Context

Taking on a container refuses one whose spec would show a value of its environment ([ADR 0031](0031-containers-dockwell-didnt-make.md), "A value of the environment shows nowhere else"). A plan shows every field but the environment, and a Compose file that wrote `${PASSWORD}` into a health check left the value there on the container, where Docker keeps the words as they came to be. The refusal names the variable and the field, never the value.

That is now the most common reason the Coolify and Dokploy imports ([ADR 0089](0089-importing-from-coolify-and-dokploy.md)) leave an app behind, by far. Counting each panel's templates, with each app counted once for each kind of thing refused, 40 of Coolify's 382 (v4.7.5) and 64 of Dokploy's 612 hold a value of their environment in another field, about one in ten. The next most common refusal, `devices`, is in 3 and 6 of them. By field:

| Where the value is | Coolify | Dokploy |
|---|---|---|
| The health check's test | 27 | 47 |
| The command | 15 | 20 |
| The entrypoint | 5 | 1 |
| A label | 0 | 3 |

Most of them are a database's health check with its password in it, as `mysqladmin ping -p${SERVICE_PASSWORD_MYSQLROOT}` beside `MYSQL_ROOT_PASSWORD` with the same value, and the rest are names that only might be secrets, such as a database's name beside `POSTGRES_DB`.

What was settled already:
- [ADR 0022](0022-secrets-encrypted-at-rest.md), "A command, an entrypoint and a health check name values too": in a field `Container.references` lists, `${NAME}` is the value of `NAME` in the container's environment and `$$` is a `$`. A plan, drift and an export show the name, the value goes in only as the container is made, a container reads back by `dev.dockwell.references` and never by its text, Docker's errors are masked by those values, and an agent that can't write them in (`Hello.writes_references`) gets no such spec.
- [ADR 0085](0085-most-of-other-stores-apps-installed-safely.md), "A secret inside a word of a command": in a shell's script, a name stays for the shell to read from the environment, quoted for where it stands, and a container whose script has the value written in, as docker compose writes one, reads back as the spec too.
- ADR 0031: taking one on doesn't touch it. Docker can't label a running container, so the agent keeps a record of what it took on, which its client reads as labels (`docker.Taken`).

## Decision

### A value in a command, an entrypoint or a health check is taken on by name

- **Where a value of the environment is in a word of `command`, `entrypoint` or `healthcheck.test`, the take-on writes its name there,** as `${NAME}`, writes every other `$` in that field as `$$`, and lists the field in `references`. That is ADR 0022's form, unchanged: a plan shows the name, the value stays sealed in `env`, a rotation reaches it, and it's written in only as a container is made.
- **No new form of a spec.** `$$NAME` in the field itself would change what runs: in a list with no shell, `mysqladmin -p$$PW` runs with the words `-p$PW`. Expansion of the spec as a whole, with internal/compose's interpolator, would give every field a meaning for `$` that specs from before don't have. Neither is needed: the field already names values, and the agent already writes them in.
- **What it reads is the value, not a file's syntax.** The container has the words Compose made, with the values in them, so the take-on finds each value as text, as `valuesShown` does now. The longest value is matched first, and matches don't overlap.
- **A secret's name goes wherever its value stands; any other name, only where it stands for a word.** A value is named:
  - for a variable whose name says it's a secret (`PASS`, `SECRET`, `TOKEN`, `KEY` or `CREDENTIAL`, in any case), wherever it stands, at any length, as anything else would show it;
  - for any other variable, with 6 characters or more as now, only where the value is a whole word, or the whole of what follows `=` in a word, as in `--user=chatwoot`, and never in the word that names the program: the first word of the command or the entrypoint, the first after `CMD`, or a script's first word or one after `;`, `&`, `|`, `(` or a line break.
  - So `POSTGRES_USER=postgres` beside the command `postgres -c max_connections=200` names nothing, where renaming the user would otherwise change the program that runs. Where a value can't be named by these rules, the container is refused, as now, by name and field: the worst case is today's.
- **One name for a value that several hold.** Where two variables have the same value, a name that says it's a secret is taken first, then the first by name, of those the rules above let stand there. Any of them comes to the same words now. Which one stands there decides what a later change of it reaches, and the plan shows it, as `-U ${POSTGRES_PASSWORD}` where a password is the same as a user's name, for the person to see before they approve it.

### What a script and a plain list each get

- **In a list with no shell,** as `["CMD", "redis-cli", "-a", "${REDIS_PASSWORD}", "ping"]`, the value is written into the word as the container is made, as Compose did. It's in `docker inspect` and the process's command line, as it was, and the plan shows the name.
- **In a shell's script,** the word `sh -c` reads or a `CMD-SHELL` test, the name stays for the shell, as ADR 0085 has it, so a container Dockwell makes again later has no value in its command line at all. That reads the same only when the shell reads the value as the word it is. So in a script, a value with a character the shell reads as more than a word's, such as a space, a quote, `$`, `\`, `` ` ``, `;`, `&`, `|`, `<`, `>`, `(`, `)`, `*`, `?`, `[`, `#`, `~` or a line break, is not named, and the container is refused for it as it is now, by name and field.

### Reading a taken-on container back

- **The agent's record of a container it took on keeps its references,** as `dev.dockwell.references` would hold them, beside the paths it keeps already (`docker.Taken.references`). Its client reads them as that label, so `readWords` reads the container as its spec, with the value written into the script as a container docker compose made from an export has it. A field that doesn't come to what Docker has reads as `binding.Unread`, as for any container.
- **An agent that can't read that record gets no such take-on, and nothing taken on so.** One from before it would read the container's command with the value in, and its reports would carry the value into drift. So an agent says it can (`Hello.reads_taken_references`, 336). On one that doesn't, the control plane refuses a step that takes a container on with names in it, and reads, plans and applies nothing that a change took on (`change_items.adopts`) and that names values, now or in what the server was given, with how to update the agent (`Keeper.TakenOnNamingValues`, beside ADR 0022's `NamingValues`). With nothing to ask, it reads nothing there, as ADR 0022 has it. Its read of a container still offers the spec with the value in the field, which is refused as now.

### What still refuses, and what never shows

- **A label still refuses.** Docker can't write a label from the environment as a container is made, and what reads a label, a proxy and Dockwell's own lookups among them, reads it as it is. It's 3 of Dokploy's templates, and a credential in a label stays refused as ADR 0089 has it.
- **A value in any other field still refuses,** as a value in a script that the shell would read as more than a word does. In a field that names values, only its own text counts, as a short secret could match a `${NAME}` the take-on wrote, or the field names in `references`.
- **A value never reaches a plan, a log or the panel.** The spec that leaves the agent holds names in those fields, and the value only in the sealed `env`. A refusal names the variable and the field. Docker's answers are masked by the values a field names, which now takes in a taken-on container's. Drift reads names on both sides. A test takes on a container from each kind of word above, with a secret whose value shares text with an ordinary variable's, and checks that no value is in the spec's other fields, the plan, the record's references, the agent's log or an error.

### Where it applies

Every take-on reads a container through the same agent call (`ReadAdoption`, then `Adoption` as it applies), so each gets it: a container from the list, a Compose project, 1Panel's apps, and Coolify's and Dokploy's, Swarm-run ones made again included. Both calls write the names the same way, so what a person approved is what the agent checks again.

### Numbers

`Hello.reads_taken_references` is 336, in the 330 to 339 the import from Coolify and Dokploy holds in the agent protocol.

## Consequences

- **What it takes on, measured.** The first slice ran the take-on's own reading over each panel's templates, with a value made up for every variable, as each panel makes one: of Coolify's 382, 61 hold a value of their environment in another field, 39 of those are taken on now, and 22 are still refused; of Dokploy's 612, 123 do, 102 are taken on now, and 21 are still refused. That's more than the count by variable in the context above, which saw only a variable written in a field, where the take-on sees any value of 6 characters or more, such as a database's name, wherever it stands.
- **What still refuses** is almost all an ordinary value inside another word or where a program's name goes, such as `appwrite` in `/tmp/.appwrite-ready` or `elastic` in `elastic:${ELASTIC_PASSWORD}`: 12 of Coolify's in a health check and 12 in a command, and 11 and 9 of Dokploy's, with 3 of Dokploy's in a label. Those are refused as before, by name and field. Telling such a word from a secret by its shape is the guess ADR 0022 turned down, so that stays a refusal.
- The plan of a take-on shows names where it showed nothing before, as it refused. A person reads `${MYSQL_ROOT_PASSWORD}` in a health check, which is what the template's author wrote.
- When Dockwell first makes the container again, its script reads the value from the environment where it had the text. That's the same words for the shell, and the value leaves the process's command line.
- A name chosen for a value two variables share can be the wrong one. The plan shows it, and changing the other variable's value doesn't reach that word.
