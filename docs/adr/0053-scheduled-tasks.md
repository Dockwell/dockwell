# 0053. Scheduled tasks

- Status: Accepted
- Date: 2026-11-07

## Context

The rail's Scheduled tasks has opened "Not in this release yet" since M1, and on `main` nothing runs on a schedule but a backup policy. The PRD makes scheduled tasks a launch requirement ("shell, script, URL, backup", Server, P0), and principle 1 says a cron job is the same kind of object as anything else that runs. 1Panel has cron in its free edition, so without this you keep a crontab beside Dockwell, outside plans and Activity. #259 asks for it, to `Scheduled-Tasks.dc.html`: a shell command on a server, a command in an app's image, an HTTP request to a URL and a backup, each with its schedule in words and as cron, where it runs, its timeout and retries, and its last runs and their output.

What's decided already:

- [ADR 0033](0033-backups-of-volumes-and-databases.md): a backup schedule is a resource, `backup.policy`, and the control plane keeps its time. It says an agent keeping its own schedule can come later.
- [ADR 0042](0042-the-installs-settings.md): the install's time zone is what a new schedule starts in, and a schedule keeps its own zone in its spec.
- [ADR 0032](0032-files-on-a-server.md) and #179: a plan that's as good as root needs a person, the typed fingerprint and a passkey, and so does any change to what runs with root.
- PRD principle 5: if the control plane goes down, servers keep serving and keep reconciling.

## Decision

### A task is a resource

- **Kind `task`,** in an environment, with the ID prefix `tsk`. Making, changing, pausing and deleting one is a change with a plan, with revisions, history and an undo. Its spec says what it runs, its schedule, its time zone, its timeout and its retries, and whether it's paused. A task isn't a `service`: it has no container while it waits, nothing reaches it, and nothing restarts it. It's the same model as one, which is what principle 1 asks.
- **It's placed on servers, as a service is,** and runs on each one it's placed on. The panel places a new task on one server, so it runs once on each beat; a shell command meant for every server is placed on every server.
- **Three kinds run on a server:**
  - **A shell command on the server.** The agent runs `/bin/sh -c` with it, as root, in a new process group, with stdin closed, in `/` unless the task names a folder, with `PATH`, `HOME=/root` and `LANG=C.UTF-8` and nothing else. It's as good as root, so making one, and any change to what it runs, takes a person, the typed fingerprint and a passkey, as ADR 0032 decided for a plan. A new kind of host access says so in the plan: the server's shell, as root.
  - **A command in an app's image,** as `docker compose run` runs one: a one-off container from the image the app's container runs on that server at that moment, with the app's entrypoint, environment, secrets, mounts, networks, user and limits, and the task's command in place of the app's. The command is words, split as a shell splits them, and runs without a shell unless it names one, as an image may have none. It's on the servers the app runs on, and the app must be running there, or the run fails and says so. A deploy carries the task to the new image with no plan of its own. The container publishes nothing, restarts never, goes when the run ends, and is labelled as the task's, so it's never read as a replica or as drift. It reaches what the app reaches, so a task in an app that's as good as root is too, and making one asks for a passkey.
  - **An HTTP request to a URL,** made by the agent on that server, so it reaches what the server reaches. `GET`, `HEAD` or `POST` with no body, the status it expects (any 2xx unless it says), redirects followed up to 5, and TLS verified. The run keeps the status, the time it took and the size of what came back, and never the body, so a URL task can't read anything back to whoever can see its runs. It refuses link-local addresses, where clouds keep their metadata services.
- **A backup is a backup policy.** It stays `backup.policy` (ADR 0033), made and changed as one. `TaskService.ListTasks` lists each policy beside the tasks, as a task of kind backup that names its policy, so the screen, the CLI and the API show one list. Its runs are its volume's backups. The control plane keeps its time, as ADR 0033 decided.
- **SQL in a database comes later,** as a kind of its own run with the database's own client in its container, as its readings are (ADR 0038), once the Databases pages land.
- **A task holds no secrets of its own,** for now. An app's task has the app's, from its container. A shell command reads what it needs on the server, as a crontab's does. So no secret's value is kept on an agent's disk for a schedule's sake.

### The agent keeps the time

- **Each agent runs its own tasks on its own clock,** from the specs it was last given, which it keeps in its data directory. A control plane that's down, being upgraded or moved, or out of reach changes nothing about when a task runs, as principle 5 asks of what runs on a server. A server that's off runs nothing, and says so when it's back.
- **A schedule is cron:** five fields, minute, hour, day of the month, month and day of the week, with lists, ranges, steps and names (`JAN`, `MON`), and `@hourly`, `@daily`, `@weekly`, `@monthly` and `@yearly`. Where both day fields are restricted, either one matching is enough, as in Vixie cron. Nothing more often than once a minute, no seconds field, and no `@reboot`. The parser is Dockwell's own, small, in both builds, so the control plane and the agent agree on every next run. The panel and the CLI say it in words as well, such as "Every day at 04:00".
- **It's in a time zone,** an IANA name kept in the task's spec, which starts as the install's time zone (ADR 0042), and UTC where there's none. Not "server time": servers in a fleet can disagree. The agent build links Go's zone data, about 450 KB of binary, so a zone means the same on a server with no zone files.
- **Daylight saving:** a time a change skips runs once, at the first minute after it; a time that comes twice runs the first time only.
- **A run missed while the agent was stopped,** or the server was off, runs once when the agent starts, as a backup policy's does, however many were missed. A task can say to skip missed runs, for work that mustn't run outside its hour. Either way the run says it was missed.
- **Runs never overlap.** A run due while the one before is still going is skipped, and says so. The timeout bounds how long that lasts.
- **A timeout ends a run:** 10 minutes unless the task says, at most 24 hours. The agent sends `SIGTERM` to the process group, or stops the container, and `SIGKILL` 10 seconds later. A run that fails or times out tries again up to 3 times, a minute apart, each with the whole timeout, if the task asks for retries. A run is one entry with its attempts.
- **Run now** is `TaskService.RunTask`, `EFFECT_APPLY`, on the record. It runs what was approved, as the schedule would, so it takes no passkey, even for a shell command, and a token that can apply may ask for it. It needs the server connected. It doesn't wait for the run.

### What a run keeps

- **Each run keeps** when it started and ended, why it ran (the schedule, by hand, or missed), each attempt's exit status or HTTP status, and whether it succeeded, failed, timed out, was skipped or couldn't start.
- **Output is the last 64 KB,** stdout and stderr together, each line with the time it came. What's cut is from the start, and the run says how much. 64 KB is a screen or two of a script's log, and the most a run sends.
- **The agent keeps runs until the control plane has them,** up to 100 for each task, oldest going first and counted. The control plane reads them from where it last read, when a server connects and when the agent says a run ended, so a run the agent sends twice is kept once.
- **The control plane keeps each task's newest 100 runs on each server,** with their output, in the store. Reading output takes what reading logs takes: a session, or a token with the logs grant.
- **A run that fails or times out is on Activity,** as `task.failed`, under the task, with its server, what it ran and why it ended, and goes to alerts once #242 lands. A run that succeeds after one that failed is `task.recovered`. Runs that succeed aren't, as a task every 5 minutes would bury everything else.
- **A server that's offline** shows its tasks as Dockwell last knew them, with the server's state. Its runs go on, and show once it's back.

### Numbers

The agent protocol's task messages take 150 to 159, a step-up action would take 120 to 129, and an event field 130 to 139, as handed out across the open work. Run now needs no step-up of its own, as above, so none is taken.

## Consequences

- A crontab can move into Dockwell: each line is a task with a plan, an undo, a history, its runs and their output, and failures on Activity.
- A task runs while the control plane is down, which a backup policy doesn't. Two keepers of time is the cost, until backup policies move to the agent with no change to what a policy says.
- A shell task is root on its server, which is what a crontab line is. Making one asks for a passkey, so a token or a stolen session can't add one, and a token can run one that a person made.
- The agent build grows by the zone data and a small cron parser. `make budgets` measures it with tasks scheduled, under the 40 MB idle budget.
- A URL task keeps no body, so it can't tell you what a page said, only whether it answered as expected. That's what keeps it from reading metadata or internal pages back out.
- A task holds no secrets of its own. A shell command that needs one reads it from the server; giving tasks Dockwell's secrets, kept as Docker keeps a container's, is for a later record.
- The page says the schedule's zone where the mock says "server time".
