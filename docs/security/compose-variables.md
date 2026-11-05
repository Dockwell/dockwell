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
| A read-only token reads a password from a command, a label, a user or a mount | Only a spec's `(sensitive)` fields are blanked, and `command` isn't, on purpose: see the decision below. So this is possible, and the person importing the file is told first. A pass over the file's strings records each variable that can land in any field but those listed under "Not recorded", with a value or not, default or not. `CheckCompose` says so while the file is being typed, before the value is, and `ImportCompose` and the plan say it again. |
| Someone doesn't notice the warning | It says the fact, what it means, and the fix, which depends on the field: pass the value in `environment`, which is blanked, for the app to read, or read `$$NAME` in a shell, in a `CMD-SHELL` health check or a command run as `sh -c`. A label, a user or a mount has no shell, so it says not to pass a secret. The plan's note reaches the CLI too. |
| A plan or a timeline entry repeats a value | A plan shows a command as a field that changed, so a value written there shows in that plan, as the warning says. The warning itself names the variable and never holds its value. |
| A value in `environment` is read back | `Container.env` is sensitive: a response carries its keys alone, a plan says it changed, an export writes `KEY: null`, and a config hash leaves it out. |
| A value is logged | The request's variables aren't logged, and what a container prints isn't read back into an error (ADR 0017). A step's error can quote a command only as the driver wrote it, which never holds an environment value. |

### Not recorded

These are what the warning leaves out, on purpose. `TestAQuietFieldCantHoldASecret` holds the second row to its word.

| Where | Why |
| --- | --- |
| `name:`, `image` and `ports` | These say what runs and where it is reached, and no one keeps them secret. A value there is the file's own choice. |
| `restart`, `network_mode`, a service's `networks` and `depends_on`, a health check's times and retries, a network's `internal` | Each takes only a fixed word, a number or another name in the file. A file that puts anything else there is refused, so a secret can't land there. |
| `expose` | It changes nothing on one server, so it isn't kept. |
| A value in `environment`, and a volume's `driver_opts` | They're sensitive, so they are blanked. A list entry's name isn't, so a variable in one does warn. |
| A default's own words | The person wrote them in plain sight, in a file that is exported as it is. The variable beside them counts, as its value lands there once it has one. |

## What we decided not to do

**Make `command` sensitive.** A plan would say only "command changed", and the app's page couldn't show what it runs, which is what most people open it for. Few commands hold a secret, and those that do have a better place to put it. So the decision is to be honest rather than to blank it. A value already written stays in its revision for good, so one that shouldn't have been there is rotated, not deleted.

**Refuse such a file.** Many files pass harmless values into a command, such as a log level, so refusing would break them to protect the few that don't, and it can't tell which is which.
