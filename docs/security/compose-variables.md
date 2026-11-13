# Compose variables: threat model

What happens to a value passed to a Compose file, who can read it afterwards, and what stops that being a surprise. The design is in [ADR 0021](../adr/0021-the-docker-driver.md) and [ADR 0026](../adr/0026-the-app-screens.md); the code is in `internal/compose` and `internal/control/composeapi`.

## What there is to protect

| Asset | Where it lives | Why it matters |
| --- | --- | --- |
| A variable's value, such as a database password | The request that imports the file, then wherever the file puts it in a spec | It may open something other than this install |
| A spec's revisions | The store, for good: a revision is immutable | A value written once stays readable in that revision, after the next deploy |

## Trust boundaries

- **Someone who can apply a plan** can run anything on the server, so nothing here keeps a value from them.
- **A read-only API token, or a member who can only read,** can read every service's spec with its sensitive fields blanked. It is the boundary this is about. Tokens are assumed to leak more often than passkeys ([the sign-in threat model](sign-in.md)).
- **An exported file** goes wherever its reader takes it, so it holds what a read-only token can read and no more.

## Threats and mitigations

| Threat | Mitigation |
| --- | --- |
| A read-only token reads a password from a command, an entrypoint or a health check | They keep what a variable gives them by name, and the value goes in the container's environment, which is blanked ([ADR 0022](../adr/0022-secrets-encrypted-at-rest.md#a-command-an-entrypoint-and-a-health-check-name-values-too)). The exception is a value of several words in a command written as one string, which a name can't stand for: that value alone is text, and warns as below, and the line's other values keep their names. |
| A read-only token reads a password from a label | Only a spec's `(sensitive)` fields are blanked, and `labels` isn't, on purpose. So this is possible, and the person importing the file is told first. A pass over the file's strings records each variable that can land in any field but those listed under "Not recorded", with a value or not, default or not. `CheckCompose` says so while the file is being typed, before the value is, and again once a value makes a command what a name can't stand for. `ImportCompose` and the plan say it again. |
| Someone doesn't notice the warning | It says the fact, what it means, and the fix, which depends on the field: write a command as a list, or pass a value in `environment`, which is blanked, for the app to read. A label, a user or a mount has nowhere else to put it, so it says not to pass a secret. The plan's note reaches the CLI too. |
| A plan or a timeline entry repeats a value | A plan shows a label as a field that changed, so a value written there shows in that plan, as the warning says. The warning itself names the variable and never holds its value. |
| A read-only token reads a password from a config's `content` | A config is a file Dockwell writes, which every plan and read shows, as it isn't sensitive. A variable inside its content warns on each service that mounts it, as a label's does, and the fix says to pass the value in `environment` instead ([ADR 0085](../adr/0085-most-of-other-stores-apps-installed-safely.md#files-beside-the-compose-file-as-data)). A config's `name`, `target` and `mode` aren't recorded: each is a name, a path or a number. |
| A value in `environment` is read back | `Container.env` is sensitive: a response carries its keys alone, a plan says it changed, an export writes `KEY: null`, and a config hash leaves it out. |
| A value is logged | The request's variables aren't logged, and what a container prints isn't read back into an error (ADR 0017). A step's error can quote a command only as the driver wrote it, which never holds an environment value, and what Docker says of one has the values it names masked. |

### Not recorded

These are what the warning leaves out, on purpose. `TestEveryFieldAVariableReaches` has a row for each field Dockwell takes, and fails on a field without one, so a field added later warns until someone decides it needn't. A plan goes by the values it was given and a form by what they could be, and both ask the same list.

| Where | Why |
| --- | --- |
| `name:`, and a service's `image`, `container_name`, `network_mode`, `networks`, `user`, `cap_add`, `cap_drop` and `group_add` | Each is a name, and shows as what it is. Nearly every real file writes these from a variable, so a warning there would be on nearly every plan, and the one about a password in a command would go unread. |
| `ports`, `restart`, `cpus`, `mem_limit`, `mem_reservation`, `shm_size`, `deploy`, `privileged`, and a health check's times, retries and `disable` | Each is a number or one of a few words. |
| `volumes`, `tmpfs` and `working_dir` | Each is a path, and shows as what it is. |
| A service's `hostname`, `init`, `tty`, `stdin_open`, `read_only`, `stop_signal`, `pids_limit`, `ulimits`, `dns` and `security_opt` | Each is a name, a switch, a signal, a count or an address, and shows as what it is ([ADR 0085](../adr/0085-most-of-other-stores-apps-installed-safely.md)). |
| A service's `pull_policy`, and a health check's `start_interval` | Neither goes in a spec: Dockwell pulls the digest a plan pins, and checks a container at its interval from the start. |
| A volume's or a network's `driver`, and a network's `internal`, `external` and `name` | Each is a name or a word. |
| `expose` and `depends_on` | They change nothing on one server, or order the change, so neither is kept. |
| A value in `environment`, and a volume's `driver_opts` | They're sensitive, so they are blanked. A list entry's name isn't, so a variable in one does warn. |
| A `command`, an `entrypoint` and a health check's `test` | They keep a value by name, so there's nothing to warn of, but for a value of several words in a command written as one string, which a plan warns of, as only the value can say. |
| A default's own words | The person wrote them in plain sight, in a file that is exported as it is. The variable beside them counts, as its value lands there once it has one. |

## What we decided not to do

**Make `command` sensitive.** A plan would say only "command changed", and the app's page couldn't show what it runs, which is what most people open it for, and an approver can't read what they approve. So it stays shown, with names in it. A value already written stays in its revision for good, so one that shouldn't have been there is rotated, not deleted.

**Refuse such a file.** Many files pass harmless values into a command, such as a log level, so refusing would break them to protect the few that don't, and it can't tell which is which.
