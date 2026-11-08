# 0058. Runtimes: an app from a folder

- Status: Accepted
- Date: 2026-11-08

## Context

The PRD makes one-click runtimes a launch requirement: "One-click runtimes: PHP, Node.js, Python, Go, Java" (Server, P0). PHP runs for websites since [ADR 0039](0039-websites-as-projects-of-their-own.md), as a PHP-FPM container per website with its version chosen. Nothing runs Node.js, Python, Go or Java from what's on the server: today someone writes a Dockerfile, builds an image elsewhere and deploys that. 1Panel's Runtimes page runs an app from a folder on the server with the version, the start command and the port someone picks, and no Dockerfile. Issue #261 asks for that, with each version change a plan, and set against what builds from Git in #253 (ADR 0046, in review).

## Decision

### A runtime is how an app runs, beside its container

An app from a folder is a service like any, whose spec says how it was made: `Service.runtime` names the language, the version, the folder on the server, the commands that install and start it, the port it listens on, and who it runs as. The parts of its container, `Service.app`, that the runtime decides are written from it each time the spec is checked, by the control plane, as a website's services are written from its settings (ADR 0039).

- **The runtime decides part of the container, and the app the rest.** The runtime writes the image, the command and entrypoint, the working folder, the user, the mount at `/app`, `PORT`, what's exposed, a TCP check on the port (unless the app has a check of another kind) and the capabilities dropped, whatever an edit sends there. Everything else is the app's, as an image app's is: its environment, ports, networks, volumes, limits and restart policy, so the Environment tab, Add port and the domain flow work unchanged. The language's caches and `HOME` go in its environment where it sets nothing, and give way to a variable or secret of the same name; what the runtime wrote is kept with it (`Runtime.wrote`), so the panel says which values are the runtime's and shows them, as none is secret, and a value of the runtime's that comes back empty, as a hidden value does in a whole spec sent back, is written again.
- **The runtime is the source of what it decides.** The plan shows what changes in it, "Node.js 22 to 24", and what that does to the container. To change what it decides, change the runtime, or drop it and keep the container as an image app. Deploying an image to an app from a folder is refused, saying so.
- **The agent runs a container, as ever.** The runtime is kept by the control plane alone: no plan or step sends it to an agent (`resource.ControlOnly`), and drift is what differs without it. So the agent runs it as any container, and the agent build's budget doesn't move. It says in its hello that it makes a mount's missing folder as its owner (`makes_folders`), which #241's PHP websites need too: an agent from before that is refused a mount that asks for it, as it would make the folder root's. Keeping drift puts the runtime back when it still writes what the server has; drift in what it decides, such as an image changed by hand, leaves an app that runs an image.
- **Everything an app has, it has:** a domain through the domain flow, health, logs, rollbacks, undo, variables and secrets, the Compose export, and the alerts a service has.

### What its container is

| Part | Is |
| --- | --- |
| Image | The language's official image of that version, pinned by tag and by the digest of its multi-platform index: `node`, `python`, `golang` and `eclipse-temurin` (the JDK, so it can build as well as run) |
| The folder | Mounted at `/app`, read-write, as an app writes what it installs there: `node_modules`, a Python virtual environment. A full path with no `.` or `..` in it, in the characters a website's folder takes (ADR 0039). A folder that isn't there is made as its user's, as a PHP site's is; an agent from before that refuses the plan, saying to update it, as it would make the folder root's |
| Command | `sh -c` with the install command, then `exec` the start command, from `/app`. Each start installs again, so a file edited in Files takes effect at the next restart, which the app's page says |
| User | `uid:gid`, the folder owner's unless it says, never root's, as PHP's (ADR 0039) |
| Port | Its own, with a TCP check on it, which the domain flow sends to |
| Hardened | Every capability dropped; `HOME` and the language's caches in `/tmp` |

Each language has what it installs and starts with, unless the runtime says:

| Language | Versions | Installs with | Starts with | Port |
| --- | --- | --- | --- | --- |
| Node.js | 26, 24, 22 | `npm ci` with a lockfile, `npm install` without | `npm start` | 3000 |
| Python | 3.15, 3.14, 3.13 | a virtual environment in `.venv`, and `pip install -r requirements.txt` when there's one | `python app.py`, with `.venv` first on the path | 8000 |
| Go | 1.27, 1.26 | `go build -o /tmp/app .` | `/tmp/app` | 8080 |
| Java | 25, 21, 17 | `./mvnw -q -DskipTests package` when there's `mvnw`, and the jar it makes in `target`, but Spring Boot's `-plain` one, copied to `/tmp/app.jar`; otherwise `app.jar` copied there | `java -jar /tmp/app.jar` | 8080 |

The app is given `PORT`, so one that reads it listens where Dockwell checks. A variable or secret can't be used as `PORT`, nor can the app set its own: set the runtime's port instead.

### Versions are pinned, and changed by plan

- **The versions on offer are a table in Dockwell,** each a release at least 14 days old, pinned by digest, as PHP's are. A release of Dockwell moves the pins, as it moves Caddy's.
- **Changing the version is a plan,** "Run shop on Node.js 24, from 22", which replaces the container. `ChangeServiceRuntime` makes it from the service as it's kept, so no one reads its environment back, as `DeployService` does for an image. Another tag of the same repository reaches nothing new ([ADR 0032](0032-files-on-a-server.md), as #252 counts it), so a token may apply it.
- **An app keeps its build.** An app on a build of its version Dockwell knows, the newest or one before it, keeps it through any other change, and through keeping drift, so a release that moves a pin restarts nothing. A newer build shows on the runtimes panel and the app's page, as PHP's does (ADR 0039): "Update to 24.22.0", the same call with the version it has, which writes the newest pin. A release that moves a pin keeps the one it moved from as a past build.
- **A retired version takes no new app.** A version Dockwell no longer offers stays in its table, retired: no new app runs on it and no app moves to it, and an app already on it keeps running and changing, edits, undo and rollbacks included. Its page says so, with Change version.
- **An empty version is the newest,** written as the version when it's checked, so an app stays on what it started with until someone changes it.

### What it isn't: a build

A runtime runs what's in a folder as it is, installing what it lists at each start, in a stock image. Builds from Git (#253, ADR 0046) make an image once, from a commit, which then runs wherever it's placed. The two are for different things: a runtime for code that lives on the server and changes there, as 1Panel's does; a build for code in Git, reproducible and moved between servers. A runtime app placed on two servers runs two copies of two folders, which the plan says. When #253's second slice detects languages with Railpack, an app can move from a runtime to a build by changing its service from one to the other, in a plan.

### Host access

The folder is read-write from the server, so a person approves a runtime app, as any read-write folder (ADR 0032, #252): no token applies it. For a runtime the image is stock and its commands are the code, as the image is for an image app: what writes the folder is the language's image running its install and start commands as its user (`WriterOf`), so a change to the commands or the user is new reach a person approves, as a new folder or language is. The plan names the folder and who writes it. Another version of the same language changes none of them, so a token may apply it.

### The runtimes panel

The runtimes panel that #241 built for PHP lists each version of PHP as before, then a card for each language an app from a folder runs on, with its versions, the apps on it and an older build to update. "New app on Node.js 26" opens New app on that runtime. New app gets a source of its own, "Folder", beside an image and a Compose file, which reads the folder on the server as PHP's form does: who owns it, which the app then runs as, and whether it holds what the language's commands run. An app's page says what it runs on and its folder, and changes its version where another deploys an image. `ListWebsiteRuntimes` lists every language, each with an `id` as a spec names it, and the commands and port it has unless the app says.

## Consequences

- The agent says one thing more in its hello, and a container from a runtime is a container like any.
- Installing at each start makes a start slower than an image's, and needs the network for what it installs. A runtime app that can't reach a registry doesn't start, and its logs say why.
- What a runtime installs lives in the folder, so a folder copied to another server runs there too, once its owner is right.
- Not yet: extensions and package tools on the panel's cards, which the mock draws; a PHP app outside a website; Bun, Deno, Ruby and .NET.
