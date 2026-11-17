# 0085. Most of other stores' apps, installed safely

- Status: Accepted
- Date: 2026-11-12

## Context

On 2026-11-12 the owner added 1Panel's app store as a source and found 179 of its 263 apps left out. They asked that most of 1Panel's, Dokploy's and Coolify's apps install and work (#570).

[ADR 0078](0078-other-stores-read-on-the-server.md) reads those stores on the server and converts what it can, running nothing a store holds. What it leaves out, it counts by kind. Nothing measured those counts over time, so nobody could say whether a change raised them or lowered them, and "converts" meant only that an entry passed the catalog's checks, not that the app ran.

We measured the three stores on 12 Nov 2026 through the control plane's own fetch and converter, at 1Panel's `55061b99`, Dokploy's `4f0ddfb5` and Coolify's `93c7e53e`:

| Store | Apps | Converted | Rate |
| --- | ---: | ---: | ---: |
| 1Panel's app store | 269 | 93 | 34% |
| Dokploy's templates | 612 | 169 | 27% |
| Coolify's templates | 384 | 78 | 20% |

The converter stops at an app's first reason, so the counts by kind hide what's behind each. We let each gate through in turn, in a scratch build, to see what each one holds back:

| Gate let through | 1Panel | Dokploy | Coolify |
| --- | ---: | ---: | ---: |
| None | 93 | 169 | 78 |
| A tag that moves, such as `latest` | 93 | 288 | 175 |
| and a secret inside a longer value, such as a connection URL | 97 | 387 | 233 |
| and files shipped beside the Compose file | 112 | 446 | 233 |
| and scripts on the server, taken as no-ops | 140 | 446 | 233 |
| and an app that runs on amd64 alone | 151 | 446 | 233 |

What's left after all five is mostly Compose keys Dockwell can't run yet (`hostname`, `ulimits`, `pull_policy`, `stdin_open`, `init` and a few more), Coolify's files written inline (`content:`), and values a store fills in from another app, such as 1Panel's `PANEL_DB_HOST`, which connectable apps (ADR 0084, #569) are to meet.

This record says what the converter now accepts and why each is safe. It grows a section for each later slice of #570.

## Decision

### What each store converts to is measured, in the repository

- **`dockwell-catalog rates` converts each store at a commit the repository pins, as a server does:** through `convert.Fetch`, the control plane's own two-round fetch, factored out of the catalog's API for both to use, and `convert.OnServer`. It prints, for each store, how many apps it holds, how many convert, the rate, and the rest counted by kind, the most first. `--list` names each app left out and why; `--show <format>/<id>` prints one app's converted entry, or its reason. All three stores convert in under ten seconds.
- **The record is `internal/catalog/convert/rates.json`:** each store's repository, its pinned commit, how many apps it holds, every app that converts by ID, and how many are left out of each kind.
- **CI fails when an app that converted no longer does,** naming each, and when the record doesn't say what converts now, so the record stays true. `make store-rates RECORD=1` writes it, and `PIN=1` moves each store to its default branch's commit first, a person's commit, so upstream changes never fail a pull request by themselves.
- **It runs in a CI job of its own, Other stores on Docker,** which reaches GitHub for the stores, as that job pulls images anyway. A fetch that fails is tried twice more, a few seconds apart. The job runs for any Go change, as Catalog on Docker does (ADR 0050).
- **What converts isn't quite what the Sources page shows.** The page leaves out an app whose ID Dockwell's own catalog has, as `taken`, and shows one that's plainly the same app as another choice on that app's card (ADR 0078). Those depend on Dockwell's own catalog, which moves on its own, so the record counts the converter alone. At the commit above, 1Panel's 99 apps that convert show, as its only source, as 90 cards of its own, 6 as another choice on a card of Dockwell's own, and 3 taken.

### A tag that moves is pinned when an install is planned

- **On the server, an image named by a tag that moves, such as `latest`, `stable` or `main`, converts,** as a version's tag does: the entry names it by its tag, and the plan pins the build it points at then, through the agent of the app's server, and shows the digest (ADR 0078). An image named with no tag is written out as `latest`, which is what Docker pulls for it, so the plan names the tag it pins. A 1Panel app whose only version folder is `latest` converts at version `latest`.
- **It's as safe as a version's tag, which can be pushed again too.** What runs is the digest a person saw in the plan and approved. No registry is asked at a refresh, and nothing about the image is trusted that a version's tag isn't.
- **The offline converter still leaves such an app out.** It pins each image as it converts, for a catalog that's published, where a moving tag would never move on.
- **The converter's version goes up with each slice that changes which apps convert** (`convert.Version`, 3 for moving tags and 4 for what follows, after the 2 that reads stores' logos and words), so a server converts a commit it kept again, and a store that's pinned shows the new apps once Dockwell is updated, not only once the store moves.

### A new build of a moving tag is an update, through the install's catalog

A store's file that names `latest` never changes when `latest` moves, so the catalog's releases can't say an app on it has moved on. Its registry can:

- **A catalog install that runs an image by a tag that moves is checked for a new build,** with the same HEAD of the tag ADR 0079 makes for any image, every 6 hours and by Check now. Its catalog's releases are read from its catalog, as before; nothing else about the image, such as a newer version tag, is an install's to take.
- **A new build is offered as an update of the install,** of the kind `digest`, at the release it runs, so the panel's Update, and automatic updates of that kind (ADR 0080), upgrade the install through its catalog and name no image.
- **Upgrading the install reads each moving tag again,** for the build it points at now: with no newer release, it's planned only when one of them has a new build, with the summary "Update moving to the newest build of nginx:latest"; with a newer release, each moving tag is pinned afresh too, as a person upgrading expects what's current. The plan shows each new digest, and backs up first, as any upgrade does. A companion added or taken away still keeps every digest.
- **Taking a new build takes that build alone,** never the store's newer release, which a person may have skipped: its update asks for a rebuild (`UpgradeServiceRequest.rebuild`, 90), and the panel's Update, Update several and automatic updates send it for an update of the kind `digest`. When the install's catalog has moved on past the release it runs, a rebuild alone is refused, and the release stays an update of its own, which brings the newest builds with it.
- **A release whose version stays, or names a tag that moves, is skipped by its number,** such as `release:7` (`AvailableUpdate.skip_as`, 90, which the panel sends as it is), so skipping one release of an app on `latest` never skips the next, nor a new build.
- **The agent keeps the digest its check read beside a plan's reads,** for the 10 minutes it keeps those, so an update planned right after a check pins the build the check saw.

### "Converts" means "works", for a sample

- **Other stores on Docker installs a sample of each store's apps on real Docker,** through the CLI, as a person would: `scripts/store-e2e.sh` adds each store at its pinned commit, and each app installs from its converted entry, its plan pins each image, every container runs, healthy where its entry has a check, on loopback alone, the app answers on the port a person opens, and it's removed through a plan. The sample is small apps, at least one from each store: one on a moving tag, one named with no tag, and one by a version.
- **Its first run found a store's own mistake:** Dokploy's CyberChef publishes port 80, where its image has listened on 8080 since it moved to an unprivileged web server, so it runs and answers nothing, in Dokploy as here. A plan could read the port an image declares, as a form already does (ADR 0062), and say when a store's differs; that's later work, and the sample takes another app.
- **The job's agent reads registries as a server does,** under `DOCKWELL_TEST_REGISTRY=public`, while `DOCKWELL_TEST` keeps everything else it would reach off (ADR 0034). Without it a test still reads no image from a registry.

### What a container asks of itself

*Added with the second slice of #570.*

After moving tags, the Compose keys Dockwell refused were the next gate: `hostname` in 23 apps, `ulimits` 17, `pull_policy` 13, `stdin_open` 9, `security_opt` 9 (mostly `no-new-privileges:true`), `read_only` 6, `init` 5, a health check's `start_interval` 5, `pids_limit` 4, `dns` 3, `tty` 3 and `stop_signal` 2. Each changes the container alone, and none reaches the server, so each is now a field of the container, as Compose names it:

- **A spec holds `hostname`, `init`, `tty`, `stdin_open`, `stop_signal`, `ulimits`, `read_only`, `pids_limit`, `dns` and `no_new_privileges`** (Container 90 to 99), checked as Docker would take them: a hostname as a DNS name, and none on the server's own network, which has the server's; a signal by its name or number, written as SIGQUIT; a limit Docker knows, its soft limit within its hard; a count of 0 or more; up to 8 DNS servers, by address. They read back from Docker as the spec has them, so none is drift: a hostname Docker gave for the ID, or a stop signal the image gives, isn't one the spec set. The Compose importer reads and exports each, and a container Dockwell takes on keeps them (ADR 0021).
- **A real-time priority is access a person approves.** A limit of `rtprio` above 0 lets the app's processes take the CPU ahead of everything on the server, its agent and control plane among them, whatever its cpus limit, on a kernel that doesn't group real-time tasks: it counts as `SYS_NICE`, as a plan shows it. A store's app that locks memory with no limit (`memlock: -1`) is left out, as locked memory can't be reclaimed from the server.
- **A DNS server is held to where a request a person names may go**, in netguard's words: loopback, link-local, unspecified, multicast and reserved addresses are refused.
- **Of `security_opt`, only `no-new-privileges` is taken.** It tightens what a container may do. `seccomp:unconfined`, `apparmor:unconfined` and `label:disable` loosen what confines it, and are refused by name, as before.
- **A pull policy and a health check's start interval are taken and dropped.** Dockwell pulls the digest a plan pins whenever a server hasn't it, so a policy says nothing it does; `build`, which builds, is refused as a build is. A start interval only says how often a check runs while the start period lasts, which Docker before 25 doesn't keep: Dockwell checks at the interval throughout, so a new container is seen healthy a little later at worst.
- **An agent says it sets them, in its `Hello` (`own_settings`, 300).** The control plane plans none of them on an agent that doesn't, which would leave them out: "the agent on hz-2 is from before Dockwell set a container's hostname, limits and the like, which web asks for, so it would leave them out. Update the agent on hz-2."
- **compose-go asserts what a value is in places,** and a limit written as text, as a variable gives one, made it panic. A file is a stranger's when a store refreshes with nobody watching, so the importer writes each limit as the number it is, refusing text that's none, and turns any panic in compose-go's decoding into a file it can't read.

### A secret inside a word of a command

*Added with the second slice of #570.*

The catalog refused a secret anywhere but a value of its own, as an upgrade stands something in for a secret it never opens. That holds for an environment's value, where `postgres://u:${PASSWORD}@db/x` would carry the stand-in. It doesn't for a command, an entrypoint or a health check's test, which keep a value by name, with the value written into each word as the container is made, and a rotation reaching it ([ADR 0022](0022-secrets-encrypted-at-rest.md)). So a secret may now stand anywhere in one of their words, as in `redis-server --requirepass ${PASSWORD}` or `--api-key=${KEY}`, which 19 of the stores' apps write. A test installs such an app from a converted store and upgrades it: the spec keeps the names, the secret comes by reference, the container gets the secret itself, and the upgrade keeps it.

In a shell's script, the word `sh -c` or a `CMD-SHELL` test reads, a name isn't written in: it stays for the shell to read from the container's environment, where the value is, quoted for where it stands (`"${PASSWORD}"` outside quotes, `'"${PASSWORD}"'` inside single ones). Written in, a value would show in the process's command line to every user of the server, and a typed one with a quote, a `$(` or a `;` would change what the script does. The references label says which word is a script, so the container reads back as its spec, and a container made before keeps the form it has. One docker compose makes from an export has the value written in, as Compose writes any value it's given, and reads back as the spec too. In any other word the value is the program's own argument, as it is for any program given a password on its command line.

A secret inside an environment's value, over 100 of the stores' apps and most of them a companion database's URL, is the next section's.

### A secret inside an environment's value

*Added with the next slice of #570, 16 Nov 2026.*

A value of a service's environment with a secret inside it, as `DATABASE_URL: postgres://app:${DB_PASSWORD}@db/app` is, becomes a secret of its own, which Dockwell writes from a template of the environment's values, as it writes a need's URL (ADR 0084's `ValueFrom`).

- **The install's file:** the value names the new secret whole, `${DATABASE_URL}`, which the service uses by reference. The secret is named as the variable is, or with its service's name before it where an input or another value has the name. Once installed, a value keeps its secret through every release: an upgrade names it as what runs does, so a release that adds or drops a service before it, whose value would take the name, never moves it, or a person's own value with it, to another service. A value a release adds takes a name nothing runs.
- **Which values:** a value that's one URL from its start, in one pair of quotes or none, or a line of text with nothing a format reads, such as a key=value DSN or a prefix. Each place it names a secret, as `${S}` or a bare `$S`, must be a placeholder of its own. Anything else stays refused, as it was: JSON, two URLs, a URL after other text, a config of several lines, and a secret inside another value's default, as in `${GIVEN_URL:-postgres://u:${PASSWORD}@db/x}`, which Compose reads and a template can't hold.
- **The template is the value's own text,** each secret named where it stands. In a URL it's percent-encoded as a URL's user and password before its host's `@`, in its path, or in its query, a space as `%20` there. A URL's query may hold what a format reads, such as JSON, as its password is never there. In a line of text it's as it is. A plain value it names, such as the database's user or `${DB:-app}`, is written into the template as the install has it, encoded the same way, as only secrets are values of the environment a template can name. So a plain value reaches the URL when an upgrade or a change of need writes the template again.
- **An upgrade stands in nothing,** as the app's file names the new secret whole and its template is made again from the release. Its merge reads the file the install ran with each such value named as what runs names it, so a URL a person set by hand on the service reads as theirs and stays. A secret that holds what a person gave it is kept too, and the plan says so ("URL holds a value you set"), for a need's URL as well: a value of its own, a reference, or a template other than the one Dockwell wrote it from, as Connect a database writes one. The install's pin keeps each template Dockwell wrote (`CatalogPin.written`), set at an install, an upgrade and a change of need, and that record is what a secret is judged by. So a release that changes a need's URL, as when it renames the need's password, writes the URL anew, and a change to how Dockwell makes a template reads no install's as a person's. A secret the pin has no template for, as one from an install made before pins kept them, is Dockwell's while it has a template, as it was. Where a release names a need's database password anew, the new secret takes the old one's value, or its reference to a shared server's password, rather than a new one: the database holds the password it has, so it stays as it is, and the plan says so. Dockwell's own URL follows the new name through what the release writes. A kept URL of the person's, a template or a value, stays exactly as they wrote it, and the old secret stays while anything reads it, so it still signs in, wherever it points. This also mends a shared server's rename, which before gave the app a new password the server never had. A change of need keeps such a value with a secret inside the same way, and writes its own URL anew. A secret the new release no longer writes goes, unless it holds what a person gave it, something else reads it, or the release has an input or a need's URL of that name, so the deletion never holds an upgrade up.
- **A release that takes a person's secret for another service is refused.** Where a value's secret holds what a person gave it, and the new release has an input or a need's URL of that name on another service, the upgrade stops and says so: the value would move to a new secret, and the other service would read the person's. The person points the value at a secret of another name and deletes the old one, then upgrades. A secret Dockwell wrote takes a new name, as it is.
- **A rotation writes the URL again, encoded.** It reaches the app's side alone: a companion database, such as the PostgreSQL beside most of these apps, reads its password only as it first makes its data, and keeps it until it's changed in the database. So a rotation of that password leaves the app unable to sign in until the database takes the new one, as with a password the app reads whole.
- **Tests prove it:**
  - An install of such an entry runs with the URL holding its password.
  - A rotation to `second/one@x` gives `second%2Fone%40x`, and an undo puts back both.
  - An upgrade that changes the URL keeps the password, and one that changes only the image keeps a person's own URL, on the service or in the secret.
  - A release that drops the URL removes its secret, and one that takes it as an input keeps it.
  - A release that adds a service before web, whose own URL would take web's name, leaves web's URL, and a person's own, on web, through an automatic update too. One that gives another service an input of that name is refused while the person's value is in it.
  - A release that renames a need's password keeps its value, on a new database and a shared one, and writes Dockwell's URL with the new name; one that changes the URL's template writes it, with no note. A person's own URL, a template to the need's database or to a server of theirs, or a value, stays as written through either, with the note, and still signs in.
  - Without the template, the app ran with the stand-in where its password belongs.
- **Not yet:** a need's own secret stays a whole value, as Dockwell writes it from the need's parts already, and a template of it would name a value that's written in turn. Neither does a value outside a service's environment, such as a label, a top-level YAML anchor a service's environment merges in, or a config's content.

The catalog's check lets such a value through:

| Store | Before | After |
|---|---|---|
| 1Panel | 155 of 269 | 158 |
| Dokploy | 369 of 612 | 432 |
| Coolify | 235 of 384 | 289 |
| Runtipi | 160 of 267 | 175 |

Dokploy's count takes in LibreChat too: its config holds `$${OPENROUTER_KEY}`, a `$` of its own the app reads from its environment, which the check now reads as Compose does, not as the secret written in.

#### A value Dokploy makes up inside a longer one

*Added with the next slice of #570, 16 Nov 2026.*

Dokploy's converter left out a template that makes up a value inside a longer one in the app's environment, before the catalog's check saw it. That's 54 of its apps, such as calcom's `database_url = "postgresql://calcom:${postgres_password}@postgres:5432/calendso"` and snipe-it's `APP_KEY = "base64:${app_key}"`.

- **The converter hands it on.** The value made up inside is an input of its own, which the longer value names, `${POSTGRES_PASSWORD}`. The catalog then writes the longer value from a template, as above.
- **Which input:** the one a value of its own already is, as `POSTGRES_PASSWORD=${postgres_password}` beside the URL. Otherwise a new one under the variable's name, with `_SECRET` after it where that's taken, so snipe-it's key is `APP_KEY_SECRET` and its `APP_KEY` is the secret the app reads. A helper written inline is named for the value or variable it's in and for what it makes, so `ADMIN=${email}:${password:16}` asks for `ADMIN_EMAIL` and generates `ADMIN_PASSWORD`, and silverbullet's `SB_USER=admin:${password}` reads `SB_USER_PASSWORD`. A variable is one value however many values use it, as Dokploy works it out once.
- **Generated as the template asks:** `${password:32}` 32 letters and digits, `${hash:32}` 32 hex digits, `${base64:32}` 32 bytes in base64.
- **A short value is an identifier, not a secret.** A value made up inside a longer one that asks for fewer than 16 characters, and a `${hash:N}` under 16 anywhere, leaves the app out. Dockwell generates a secret of at least 16, and inside a value its length is part of what the app reads: akaunting's `${hash:3}_` is a table prefix, which at 16 hex digits makes an index name longer than MySQL takes. Nor is a prefix something a person may rotate.
- **Encoded only in a URL,** by the template's rule above: `base64:${APP_KEY_SECRET}` is a line of text, so a key's `+`, `/` and `=` stay as they are.
- **A variable the Compose file names itself,** as `TZ=${timezone}` or `DATABASE_URL=${db_url}`, comes to what Dokploy writes for it, as a value of the template's environment does. Before, it was never worked out, and became an empty input of its own lowercase name, which the catalog's check refused (linkstack) or left a person to fill.
  - **A plain value is a person's to set,** such as syncthing's time zone, confluence's and geoserver's memory sizes, geoserver's database name, and mediafetch's public URL and output folder. It stays an input under the name an install had it by before, `UPPER(name)`, with the template's value as its default, and in the file where it's left empty: `TZ=${TIMEZONE:-America/Sao_Paulo}`. So an upgrade carries what a person set.
  - **A secret the template writes out,** such as geoserver's `geoserver_secure`, is generated, as any written-out secret is. A domain it routes is the app's address, as before.
- **A licence key is no value Dockwell makes.** A value made up for a key or variable named for a licence leaves the app out: fivem's template makes up `license_key`, where FiveM needs the key Cfx.re's keymaster issues, so a generated one would read as done while the server refuses it.
- **Still left out:** a value made up inside a file the template mounts, as a file isn't written from a template.
- **Tests:** apps shaped as calcom's, plane's and snipe-it's install from a Dokploy repository. A password of every character a URL treats apart parses back out of each URL, a generated one too, and a key of `+`, `/` and `=` reaches `APP_KEY` as it is.

Dokploy goes from 432 to 467 of 612: 34 of the 54, and linkstack, whose Compose file names the template's variables itself. The other 20 of the 54 stop at a second reason:
- **A YAML extension, 2:** the value is in a top-level extension, such as `x-chatwoot-base`, that a service's environment merges in, where Dockwell writes no value from a template (chatwoot, shopware). The reason names the extension and the template's own value.
- **The catalog's own check, 1:** JSON (dokploy-prom-monitoring-extension).
- **A value made up into a file too, 3:** livekit, pre0-22-5-supabase and wazuh.
- **Compose Dockwell can't run yet, 4:** a variable with no value (calcom, peerdb), `links` (plane), and three replicas (windmill).
- **What an entry can't say yet, 5:** an ID that starts with a digit (3dokploy), `build` (brightbean-studio), a bind outside the app's folder (ethora-core), a folder inside another (fonoster), and an image from a variable (triggerdotdev).
- **A value Dockwell doesn't make yet, 5:** a random port (teable), a UUID (trypost), a table prefix of 3 hex digits (akaunting), and an instance's ID of 8 (puppetflow, puppetflow-postgres).

The ones still out:
- **Not one URL or a line of text:** GitLab's two omnibus configs and HyperDX's JSON.
- **Outside a service's environment:** 1Panel's cloudreve, and Dokploy's dify, openpanel and reloop.

The store sample adds Coolify's Miniflux, whose `DATABASE_URL` holds its PostgreSQL password and its user, to install on real Docker.

### Files beside the Compose file, as data

*Added with the third slice of #570.*

Many stores' apps mount a config file their store ships: 1Panel's beside the Compose file in the version's folder (`./conf/my.cnf:/etc/mysql/my.cnf`), Dokploy's as a template's `[[config.mounts]]`, which its Compose file mounts from `../files`, and Coolify's written into the Compose file, as a volume's `content:`. A file is text a container reads, which Dockwell can carry as data, never running it:

- **A container mounts a file Dockwell writes, as a `Mount` of the kind `file`** (`MountFile`, 90 in `Mount`'s source): its content and its mode. It's 256 KiB at most, a container mounts 64 at most and 1 MiB of them together, its mode has read and execute bits alone, and it's always read-only, with no `external`, `make_owner` or `relabel`. It isn't a secret, and a plan shows what it holds.
- **The agent keeps each file in a folder of its own,** `/var/lib/dockwell/agent/files`, which only root writes, named by its mode and the SHA-256 of its mode and content, and binds it into the container read-only, as a mount of type bind, which fails where the file is missing rather than have Docker make an empty folder in its place. Two containers with one file share it, a new version is a new file that no running container sees change under it, and nothing the file says chooses where it's written, nor in `/proc`, `/sys` or `/dev`. A file is read back for a container only from that folder, by a name its bytes still match, so drift is a container that mounts something else. A file nothing has mounted for an hour is removed: writing it again for a container counts the hour from then, and a prune that can't read what every container mounts removes nothing. An empty folder on a file's name gives way to the file.
- **An agent says it writes them, in its `Hello` (`writes_files`, 301).** The control plane plans no file mount on an agent that doesn't, which would mount nothing: "the agent on hz-2 is from before Dockwell wrote the files a container mounts, which web does, so it would run without them. Update the agent on hz-2."
- **The Compose importer reads Compose's own `configs:` with `content:`,** each as a file of every service that names it, at its `target` (or `/<name>`), with its `mode`. A config from a server's `file:`, the importer's `environment:`, or Docker's `external:` reaches outside the file, and is refused, as is a `uid` or `gid` other than root's. A variable inside a config's content lands as text in a file a plan shows, so the exposure warnings name each service that mounts it. An export writes each file back as a config.
- **The converter writes each store's files as configs.** It reads the files an app's Compose file binds, in a third round of the fetch, 500 at most a commit, each within ADR 0078's bounds; each must be text (UTF-8, with no NUL), and an app's come to 128 KiB at most. A file whose first line is `#!` gets mode 0555, so a container can run it, as 1Panel's `run.sh` for its language runtimes is: it runs inside that container, as its image does, never on the server. A shipped folder bound read-only becomes a file for each file in it. A shipped folder the app writes to is left out, saying Dockwell would copy it into the app's volume as it installs, and doesn't yet. A Dokploy file is worked out as its template's values are, each variable within 4 KiB and the file within 128 KiB, and a file that holds a value the template makes up is left out, as an upgrade couldn't reach it.
- **The catalog refuses a secret anywhere in a config,** even as all it holds: a file is what a plan shows.
- **The panel shows a file in Volumes and mounts** as "A file Dockwell writes, 12 lines", opening to what it holds, and editing the volumes and mounts keeps each file as it is.
- **The sample on real Docker takes 1Panel's Glance,** which won't start without the config file its store ships.

### 1Panel's scripts, read as steps

*Added with the fourth slice of #570.*

68 of 1Panel's apps ship scripts: `init.sh` runs on the server as an app installs, `upgrade.sh` as it upgrades and `uninstall.sh` as it goes, as root. Dockwell runs none of them. Most do one of a few things, which Dockwell does itself, so the converter reads each script as text, line by line, and takes it only when every line is one of these:

- **Make a folder the app binds and give it to the user its image runs as** (`mkdir -p data`, `chown -R 1000:1000 data`, 40 apps, also through a variable the script sets to a path). Docker makes a new volume's folder root's, and an app that runs as another user can't write it. The folder becomes a volume, as any the app binds does, and the volume is made for that user: `Mount.make_owner`, which a server path took already, now takes a volume too, written in Compose as the service's `x-dockwell: {make_owner: {/var/lib/app: "1000:1000"}}`, by the mount's target, as Compose has no field for it.
- **Run `init.sh` again on an upgrade**, which is read as `init.sh` is.
- **Add a variable to `.env` with its value on an upgrade**, which an upgrade does for a new input with its default, and the Compose file's own default stands for.
- **Remove the app with its volumes as it goes** (`docker compose down --volumes`), which Dockwell's own removal of a project asks about.
- **Say something** with `echo`, or set the shell's options.

Anything else, such as `sed` on `.env`, `chmod`, a value made up into `.env`, or reading the database the app is given, leaves the app out, naming the script and the command. A folder the script gives away inside a volume, rather than the volume's own, leaves it out too.

How the agent gives a volume its owner:

- **Only a volume Dockwell made, kept by Docker's local driver,** with no options, is given one. A volume a driver keeps elsewhere, such as an NFS share, keeps what its server gives, and Docker's own volume (`external`) takes no `make_owner`.
- **Only while it holds nothing.** Before a container is made, the agent reads whether each such volume's folder is empty, opening the folder itself and refusing a link in its place. A volume with anything in it keeps its owner, so an app's data is never given away, and so does an empty one another container mounts, which is that container's to fill.
- **Never a server path, from a store.** An entry's `x-dockwell: make_owner` names a volume its install makes, and the catalog refuses one that names a server path. In a person's own Compose file it may name one, and making that folder for a user counts as writing to it in the plan, however the container reads it, so a read-only mount with an owner is approved as the write it is.
- **After Docker makes the container, before it starts.** Docker copies into a new volume what the image has at the mount's target, with that folder's owner, as it makes the container, which would undo an owner given before. So the agent gives the volume's folder, and nothing inside it, its owner then, through a handle on it, while nothing of the app has run.
- **An agent says it does, in its `Hello` (`owns_volumes`, 302).** The control plane plans no volume with an owner on an agent that doesn't, which would leave it root's: "the agent on hz-2 is from before Dockwell gave a new volume the owner its mount names, which web asks for, so it would be root's. Update the agent on hz-2."
- **The panel says it**, in Volumes and mounts, as "Owned by 1000:1000", and editing the mounts keeps it.
- **The sample on real Docker takes 1Panel's Navidrome,** which runs as user 1000 and writes its volume.

An upgrade's `chown -R` of data an app already wrote isn't taken: the data keeps the owner it has.

### Numbers

ADR 0085. `UpgradeServiceRequest.rebuild` (90) and `AvailableUpdate.skip_as` (90), `Container` 90 to 99, `Mount.file` (90) and its `MountFile`, and the agent's `Hello.own_settings` (300), `Hello.writes_files` (301) and `Hello.owns_volumes` (302); `CatalogInstall.release` in Go alone, and `ImageUpdatesCall` as it is.

### As built, 13 Nov 2026

| Store | Apps | Moving tags | With a container's own settings and secrets in commands | With files | With scripts as steps | Rate |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 1Panel's app store | 269 | 99 | 112 | 123 | 142 | 52% |
| Dokploy's templates | 612 | 299 | 331 | 374 | 374 | 61% |
| Coolify's templates | 384 | 199 | 224 | 240 | 240 | 62% |

## What we turned down

- **A record of rates alone, with a floor.** A count can stay the same while one app stops converting and another starts; the record lists each app that converts, so CI names the one that fell.
- **Measuring without the network,** from copies of the stores in the repository. The stores' files are their authors', under their licences, which waits for counsel (ADR 0029), and a copy would measure a converter that reads files, not the fetch a server makes.
- **Pinning a moving tag when a store is converted,** with a cache. It's ADR 0078's reason: a refresh would ask registries hundreds of times.
- **Taking a moving tag's new build at each refresh.** What runs never moves without a person's plan, or their automatic updates.
- **A moving tag's version read from the image,** such as a label. Few images say one, and the digest says exactly what runs.

## Consequences

- A change to the converter that loses an app fails its pull request, by name, and one that gains apps records them.
- Over 250 more apps from Dokploy and Coolify install, with each new build offered as an update.
- Each store's pinned commit is a person's to move. Until it moves, what upstream adds is measured only by a server reading the store.
- CI reaches GitHub, and the registries of the sample's images.
- Still to come in #570: a shipped folder the app writes to, copied into its volume; a volume's mode, as 1Panel's `chmod` gives one; a secret inside a value outside a service's environment, such as a YAML anchor; an app that runs on amd64 alone offered where such a server is; and another app's database, as a need (ADR 0084).
