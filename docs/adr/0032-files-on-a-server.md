# 0032. Files on a server, for a person, outside revisions

- Status: Accepted
- Date: 2026-11-06

## Context

Issue #87 asks to browse, read, edit and save any file on any server from the panel, through the agent and the public API. It was about half of what was done on the panels of the two real servers looked at. Three earlier decisions bear on it:

- PRD principle 2 makes every change to managed state a revision with a plan.
- [ADR 0016](0016-scoped-api-tokens.md) gives tokens two scopes by a method's effect, and neither was made with a root agent's whole disk in mind.
- [ADR 0022](0022-secrets-encrypted-at-rest.md) draws its boundary at the API: it never returns a secret's value without a step-up. The agent runs as root, and the install's key, the database, and Docker's copy of every container's environment are all files.

## Decision

### A save isn't a revision

Managed state is what Dockwell holds the desired form of: it can plan a change to it, see it drift, and undo it. A file on a server is none of that. Anything on the server may write to it, Dockwell keeps no copy, and there is nothing to plan against.

So a save has no plan, no revision and no undo, and `FileService` doesn't reach `internal/revision`. What a save leaves on the control plane is one audit event, written with `store.DB.Update`, as a reveal's is. The rule that only `internal/revision` calls `store.DB.Revise` holds unchanged. The mock's "You can undo the save from Activity" comes out of the screen.

A save can't go around a plan, as what Dockwell manages stays out of reach: its own directories are refused below. A spec that mounts a file names its path, not what's in it, so editing such a file changes nothing a plan knows of.

### A person, never a token

Every method of `FileService` is `(dockwell.v1.needs_session)`, so no token reaches it, read-only or read-write. The handlers refuse a token's principal too, as `RevealSecret`'s does. `ListFiles` and `ReadFile` are `EFFECT_READ`. `SaveFile` is `EFFECT_DESTRUCTIVE`, as it can't be undone.

Read-only tokens are refused as well: reading the disk is worth more than the server list, as a read-only token's owner meant it. A grant of its own, such as the logs grant of [ADR 0025](0025-logs-per-deploy.md), is how a script would get this. Nothing asks for it yet, and it takes its own record.

### What's refused, for everyone

The agent refuses these whoever asks. No step-up opens them.

| Refused | Why |
| --- | --- |
| Dockwell's data directory | The install's key and the database, which together open every secret; the agent's key; the proxy's certificates |
| Docker's data root, other than each volume's `_data`, and containerd's | `containers/<id>/config.v2.json` holds a container's environment, secrets' values included, and a volume's `opts.json` a CIFS password |
| `/proc`, `/sys`, `/dev`, `/run` | These aren't files. `/proc/<pid>/environ` is a process's environment, `/proc/<pid>/root` a container's disk, `/dev/sda` the server's, and `/run/containerd` holds each container's runtime spec with its environment |
| `/etc/shadow`, `/etc/gshadow`, `.pgpass`, and any file whose text holds password hashes or a PEM private key | They can be told for certain, and have no reason to be in a browser. A backup of a shadow file goes by what it holds, as its name proves nothing. SSH host keys, users' SSH keys and TLS keys are all PEM |
| Anything that isn't a regular file or a directory | Sockets, FIFOs and devices have nothing to show, and reading a FIFO would hang |

A step-up doesn't open these because one tap would open every secret at once, where ADR 0022 asks one tap for each.

Not knowing where Docker's data is must never mean serving it. The engine says where it is whenever it answers, and the agent keeps each place it has said on disk, so a restart with Docker away, or a root that moved, still knows. A directory that holds `containers` and at least two more of Docker's own directories, such as `image` and `network`, counts as a data root whoever says so, links followed, so a project with its own `containers` and `volumes` isn't one. `config.v2.json`, `hostconfig.json`, a volume's `opts.json`, and text with Docker's own keys in it are refused wherever they turn up.

`.env`, `wp-config.php` and the like stay readable. Dockwell can't tell a config with a password in it from one without, and reading them beside a running app is what the screen is for. [The threat model](../security/files.md) says so.

### The agent decides, on what it opens

The control plane can't see the server, and a path's text proves nothing: an app can plant a symlink in its volume, pointing at the install's key, and swap it after a check. So the agent walks a path from `/` one part at a time, with `openat` and `O_NOFOLLOW`, follows links itself, and checks each directory's device and inode against the refused ones, which it reads again for each call. What it checked is the descriptor it reads. A symlink, a `..` in a link's target, or a bind mount of a refused directory leads nowhere new. A request's own path has no `.` or `..`, as in [ADR 0021](0021-the-docker-driver.md#host-access).

### A step-up for a way back in, and for nothing else

This record first said Files takes no step-up at all. That holds for reading, and for saving most files:

- This is the most used screen. A passkey prompt on it, even one that lasts some minutes, teaches people to tap through the prompt that does guard something, `RevealSecret`. It would also shut Files to anyone on a tunnel or with codes and no passkey for that address.
- A session already reaches what `FileService` reads, by a plan that mounts `/etc` into a container, which takes a person's session and no passkey (ADR 0021). The sign-in threat model says the same of stolen sessions.
- What a step-up guards today, a secret's value, is refused here outright.
- Each read and refusal is on Activity, with the path and who it was, as `secret.reveal` is, so a person can find out without reaching for the database.

Those reasons are wrong for a write to a place that lets someone back in, as they weigh seeing it against stopping it, and a plan against a save:

- A stolen session ends when it's revoked. A key in `authorized_keys`, a cron job or a unit file doesn't, so seeing it on Activity afterwards doesn't undo it, and a save has no revision to go back to.
- That the same door is open by a plan is a reason to close that one too, not to leave this one open. A plan has a sheet, a typed fingerprint when it's as good as root, and an undo. A save is one request.
- A server without Docker can't run that plan, so there a save is new reach for a stolen session.

So a save asks for the existing step-up where what it writes lets someone back in, and a plan asks where what it runs has root. These are the rules, and why each is what it is.

**What a save asks for**

- **The rule is a plan's.** A save asks where it ends under `resource.SystemPath`, which takes in a person's logins, or in Dockwell's data directory. That's what a plan's mount goes by, so mounting `/etc` and saving in `/etc` get the same answer, and closing a gap closes it for both. A list of files was the wrong shape: it let through whatever it didn't name.
- **Names that rule can't see stay as extras:** `authorized_keys`, `sudoers*`, `.rhosts`, `.shosts`, `.k5login`, `.k5users`, `~/.config/systemd/user`, `~/.local/share/systemd/user`, and the shell start files of root and of members of `sudo`, `wheel`, `admin` and `docker`, wherever their home is.
- **It goes by what a place is.** The agent stats each listed place once a call, and matches the walk's directories and the final file by device and inode, as the refused list does, so a bind mount of a listed place, a volume's `_data` bound to `/etc`, or a hard link counts. A bind mount of a folder inside a listed place isn't found this way, as nothing on it says where it came from. A home on a network mount isn't stat'ed, as that would hold every call while the mount is down, and its names still count by path.
- **What people edit most asks nothing:** site files, app configs and `.env` under `/srv`, `/var/www`, `/opt/<app>` and volumes. Docker's data is on the rule's list and holds apps' files, so past a volume only what a place is counts.
- **The agent decides,** on where its walk ended, as only it can tell, so a link to one of these is one too. It asks last, so a passkey isn't asked for over what would be refused anyway, and `file_changed` comes first.
- **It says so in its hello** (`guards_ways_in`), and the control plane saves through no agent that doesn't, with words that say to update it. The gate lives in the agent, so one built before it would save `authorized_keys` with nothing asked, and nothing else could tell.
- **With no passkey at that address,** the panel points the person to SSH.

**How a proof works**

- **It's for that path on that server.** The control plane hands the path on with the save, and the agent writes only if its own walk ended there, so a link swapped in after the passkey doesn't pass.
- **One stands for ten minutes,** for that path on that server, in that session, as editing a file means saving it again. It's the only step-up that lasts past its action. It starts with the first save, and saving again doesn't extend it. For someone who chooses to be asked less often, the session stands in for the passkey, and the control plane asks the agent again with the path confirmed ([ADR 0075](0075-how-often-a-passkey-is-asked-for-again.md)).
- **It goes where the action does.** ADR 0022 spends a proof in the transaction that acts, and neither of these has one that reaches: a save happens on an agent, and a change starts in the engine. So the proof is held, which stops a second call using it, and given back if the action is refused: busy, offline, out of date, or `file_changed`. A wrong proof is still spent, and so is one for a save that may have landed.

**What's on Activity**

- A save to one of these is `file.save_way_back_in`, in a warning tone, in place of `file.save`.
- Being asked isn't a failed save. But a save or an apply that asked for a passkey, with none following within a proof's minute, is `file.save_unconfirmed` or `change.apply_unconfirmed`, in a warning tone, as that's what a stolen session looks like. It's kept in memory until then, so a restart in that minute loses it.

**What a plan asks for**

- **A plan that's as good as root takes the same step-up, for that change** ([ADR 0021](0021-the-docker-driver.md#host-access)), as this section always said it would.
- **Keeping root doesn't excuse what runs with it.** Where a resource keeps access that's as good as root, any change to what runs in it counts as gaining that access again: an image, command, entrypoint, user, health check or environment. So it needs a person, the typed fingerprint and a passkey, or a stolen session or a token could put any code at all in a service with Docker's socket.
- **The one exception is how CI deploys:** another tag or digest of the same image repository, with nothing else changed, which takes in what a tag points at now. A token may apply that, and it reads as it did. It's the same code by the same hand, and a repository the plan named is what was approved.
- **A limit isn't what runs.** It runs nothing, so a change to one reads as it did, alone or beside another tag. All the rest of a spec counts, so what's added to one later asks until someone says it doesn't.
- **Who writes a folder counts, short of root too.** A person approves a read-write server path for what runs there. Root writes what anyone owns in it, files other services and the server's users trust among them, and leaves files that run as root, so a path written by root reaches more than the same path written by another user (#247). The same goes for a read-write external volume and a mount, such as an NFS share or a plugin's folder, but not tmpfs, which holds nothing past the container. A plan says whose code writes each of these, and as whom where that's root or may be, and a change to either needs a person, as any new reach does, with no passkey for what isn't the system's:
  - **Whose code, by its repository, whatever the user.** Docker leaves set-uid programs working in an app's container, so a program in any image can run as root whatever its user is, and an image is all a token has to choose. So another repository's code over the same folder is new. Another tag or digest of the same repository is how CI deploys, and isn't. `no-new-privileges` isn't set by default, as it breaks images that use `sudo`. An image named by its ID is that image alone.
  - **Root, however it's written.** Docker's runtime reads a user as `strconv.Atoi` does, so `0`, `00`, `+0`, `0:0` and `root` are all root, and so is a user left out before a colon.
  - **A user by name,** other than root, which only the image's `/etc/passwd` says the number of, and it can say 0. A plan doesn't read the image, so a name counts as may be root, and another name is new. So does a number past what a user can be, below 0 or over 2,147,483,647, as an image can list it, and Docker reads what's listed as 32 bits: `4294967296` is 0 there.
  - **The image's own user,** where none is set, which is root unless the image says otherwise. So moving to it from a number is new.
  - **What stays as it was:** within one repository, a number from 1 to 2,147,483,647 isn't root as a user, so moving between them, or to one, asks nothing. Another tag of the same repository keeps what writes, as CI deploys it. Root that was approved stays approved for that repository's code, and so does anything less. A path read-only, or what's as good as root already, has nothing to add.
  - **Root's group isn't counted.** Docker gives it to any user number the image doesn't list, so it would ask about nearly every user, and what it can write that the user can't is little.
  - **A capability already counts,** as every one a container adds is new access, `DAC_OVERRIDE`, `CHOWN`, `FOWNER` and `SETUID` among them. Dropping fewer of Docker's own isn't counted, as what was approved already said as much: a plan that runs as root over a folder says it can change any file there, whoever owns it, so a drop never narrowed what a person saw.
- **A path is what it leads to now.** A container keeps where its paths led when a person approved it (`dev.dockwell.leads`), as `{}` where none leads elsewhere, so having no record says it's older than records and nothing else. Docker can't label what's running, so one taken on keeps it on the agent's record of what it took on, and reads as having the label. Each plan that changes a container resolves every host path again, and counts one that's moved as gained, so a folder approved in amber and turned into a link to `/etc` asks as if for the first time. A step with nothing to do resolves nothing, as nothing is mounted, and a dead path on what's left alone shouldn't refuse a plan for something else. A plan to take a container on resolves them too, so a link to `/etc` reads as what it is.
- **Every start the agent asks for checks first.** Docker follows what it finds. A start step goes by its plan, before anything stops and again right before it starts, and what makes a container does too. A rollback's `bringBack`, and a backup's or a restore's start of what it stopped, go by the container's record and by what the step at hand approved, as they have no plan of their own for it. A token can have a backup stop and start a container at will, so that one matters as much as the rest. A path that moved keeps the container down, with its restart policy set to `no`, so a daemon restart or a reboot can't start it either, and says to put the path back and plan a start, or plan a restart: not to run `docker start`, which would follow it. With no record, a path stops a start, or reads as moved to a plan, only where it gives more now than it reads as, as refusing every link would keep `/var/run/docker.sock` down wherever `/var/run` is one, and ask on every plan of what was taken on with it. A stopped container whose path moved isn't the one to start: what a person approves makes it anew, with a record of where the path leads now, so the next plan doesn't ask again. A restore checks a volume's path against its plan before anything stops, and again right before it writes. Recover takes each run it finds left on its own, so what a moved path keeps down is named, and holds up nothing else.
- **Docker's own restarts ask nobody,** after a crash or a reboot, so no check before a start reaches them, and neither does a link made in the moment between a check and its start. So at plan time, a read-write mount that holds another mount's source on the same server counts as reaching whatever that source could be made to lead to, which is as good as root. This is decided, and kept, as what stands behind both. For every plan that runs something on a server, the agent says where every server path there leads, not only the plan's: every container's binds and the volumes they mount, as one folder can be written two ways, and what runs may mount a path only through a volume. A source counts by the folder it's in as well, so one that's a link itself is still a file the holder can replace, and by every link its walk passes through on the way, each in its resolved folder: rewriting any of them moves the source, even where it ends up outside the holder's folder. The agent names what containers Dockwell didn't make mount too, and those count as held, by their names in Docker, but never as what holds, as their code isn't what a plan here chooses. A managed resource with no container on the server has nothing Docker could restart, and the plan that makes one says where its own paths lead. It goes by the specs, as what a plan gains does, so an older agent doesn't turn it off: it only says less of where paths lead. It counts one service holding its own source, a folder and a file inside it, as one service can do this to itself, and a read-only source, as a socket mounted read-only still takes connections. It leaves out a source in what's as good as root already, which gains nothing by being moved. Whichever side a plan adds asks when the pair is new, and each side's plan names the other, and says that any change to the holder but another tag asks from now on. New code in the holder does ask from then on, which is what stops a token. New code in what's held doesn't, as moving it takes the other.
- **What that costs.** Compose layouts often have this. On a home server, the *arr stack has one app writing the folder another's library is in, and 1Panel's OpenResty mounts a folder and its configuration inside it. Each reads red once, when it's made or taken on. From then on another tag of the same image deploys as it did, and other changes to the holder ask. Pinned binds, which mount what a path led to when a person approved it, as Kubernetes did for CVE-2017-1002101, are the way to retire this rule, in #202.
- **A plan is approved whole.** Each step goes by where every path in the plan leads, as a service and the bind volume it mounts are two steps, and what a person approved is all of it. A plan made while its server is offline knows nothing of where paths lead, so it can't apply: not while the server is away, and not once it's back, as the plan made then isn't the one that was reviewed.
- **What a root service uses counts as what runs in it.** Changing a variable or a secret takes in every service that uses it, and pins that service to the new value, so the service's spec changes and the rule above applies: a token gets `needs_session`, and a person needs the fingerprint and a passkey. That has a cost: rotating such a secret now takes a person and a passkey, so a script can't do it.
- **A restore counts too.** It writes over all that's in a volume, and its spec stays as it is, so it would gain nothing and ask nothing. Where the volume reaches what's as good as root, such as a bind of `/etc` or a link to it, a restore and its undo take a person, the fingerprint and a passkey, as a save there does: a backup of `/etc` brings back whoever could log in when it was taken. A volume that reaches no further than its own data asks what [ADR 0033](0033-backups-of-volumes-and-databases.md) says, and no passkey, even where a root service mounts it: what comes back was in that volume, and any service that mounts it can write there anyway. Taking a backup isn't a plan and asks nothing, as it only reads, into Dockwell's data directory, which Files refuses.
- **This is the one rule for new code on what's `same_as_root`** ([ADR 0021](0021-the-docker-driver.md#host-access)), for what Dockwell made and what it took on, and `new_code_as_root` is its note on a plan. Taking on a container ([ADR 0031](0031-containers-dockwell-didnt-make.md)) brought a narrower rule first, which counted any new tag and left the environment open. Here a new tag of the same repository has the exception above, and the environment counts, as a variable such as `LD_PRELOAD` runs what it names.

The list goes by what's on it, so it has to grow with what runs on a server. What runs that isn't on it, such as a script a cron job calls, asks nothing.

The refused list stops Files, not a stolen session, which can still reach what's refused with a plan that mounts Docker's socket, now only with a passkey.

A backup is tied to its volume by name alone, and can come back into another of that name once the first has gone, 7 days after it's deleted. So a backup says what its volume reached that's as good as root, worked out before its bytes are read and again after, and a restore asks what that volume would have, with a plan that says why. That's on the server beside the backup, and an agent saying so can only make a plan louder. A backup taken before this says nothing, so the control plane reads its volume's spec as it was when the backup was taken, with paths as written, as where they led then is gone.

A check before a start and the start itself are still two steps, a moment apart, so a link made in that moment is followed. What holds another's source asks at plan time, which is what stands behind that, and behind Docker's own restarts.

### How a save lands

- **It says what it read.** `ReadFile` gives a version, a SHA-256 of where the walk ended, which file that was by device and inode, and its text, and `SaveFile` takes it back. The agent reads the file and compares, and one that differs is left as it is, with the reason `file_changed`. A time and a size can't tell: two saves can share both. Text alone can't either: a link swapped in to a file with the same text would read as unchanged, and the save would follow it to a file nobody opened. "Save over it" reads the path again, and if it leads somewhere else now it stops and names where, with the server's version to load.
- **It writes in place.** A container that mounts one file holds its inode, so a rename would leave it reading the old file, which is how a save to a file mounted beside a running app would seem to do nothing. Hard links would break too. A rename is atomic and this isn't, so a reader can see the file half written for a moment, as with any editor that writes in place.
- **A copy of the old text goes beside it first,** mode 0600, named `.dockwell-save-…`, as that's what can't be had again if a write fails partway. The browser has what was typed. If the write fails, the agent puts the old text back and removes the copy. If that fails too, the copy stays, and the error names it. It doesn't prove the disk has room: a disk can hold the copy, and not what the file grows by.
- **The check comes after the flush.** Flushing that copy is where a struggling disk stalls, so the agent reads and compares the file again after it, then checks that its caller is still there, and only then writes. A save that outlived its caller writes nothing, and what wrote the file meanwhile isn't undone.
- **Past that, it may land.** A save left behind as it writes can't be stopped, so the person hears it may still land, and when it comes back the agent says how it came out, on whichever stream is up (`SaveEnded`).
- **It changes no byte nobody touched.** A browser's field turns every line ending into LF, so the panel puts them back, which works for a file that uses one kind. One with a mix, or a lone CR, opens read-only, and says why. A byte order mark is lost to protobuf-es's decoder on the way in, in production's binary format and not in development's JSON, so the agent keeps the mark the old text had, and returns the version of what it wrote.
- **What's typed outlasts a session.** A session that ends takes the page with it, with nothing to ask first, so the panel holds what's typed in memory, by server and path with the version it was typed over, and offers it back to the same person after they sign in. It never goes in the browser's storage, so a tab that's closed takes it along, and leaving the page asks first.
- **It makes no file, directory or change of mode.** Making, moving and deleting came with the rest of the mock's toolbar, with the same care, in [ADR 0045](0045-a-file-manager-on-a-server.md), and a change of mode comes there too.

### On record

- `file.read` for each `ReadFile`, naming the person, the server and the path, written before the contents go out. If it can't be written, nothing goes out, as with `secret.reveal`.
- `file.refuse` for each read or listing that came to nothing, however it failed. Someone looking for credentials mostly misses, so a miss leaves a mark as a hit does.
- `file.saving` before each save, and `file.save` after one lands, or `file.save_failed`, however it failed, under where the walk ended. If the first can't be written, nothing is saved. A save that may have landed, as its stream dropped or it was left behind as it wrote, is `file.save_unknown`, and when the agent can say how it ended, that's `file.save` or `file.save_failed`, as whoever saved it. The first stays in the audit log, as a save may yet fail, so a save always leaves a line, and Activity says only what happened.
- `file.save_way_back_in` in place of `file.save`, for a file that lets someone back in, and `file.save_unconfirmed` for one that asked for a passkey and none came.
- All but `file.saving` are on Activity, by person, server and path, where ADR 0020 leaves other refusals in the audit log, as the case for no step-up on reads rests on them being seen.
- A listing that works isn't recorded: it returns names, and it runs on every click. Nor is a listing that can't reach its server, as the panel asks again by itself.
- A file's contents are never recorded or logged. A path is cleaned before it's stored or shown, as a name can hold a newline or a terminal's escape codes, and a link can lead through one.

### Bounds

- A file reads as text up to 1 MiB. A larger one reads as its first 1 MiB, and can't be saved. One that isn't UTF-8 reads as binary, and its contents aren't sent.
- A directory lists at most 5,000 entries and 4 MiB.
- These calls go on the agent's main stream ([ADR 0017](0017-the-change-api-and-the-agent-stream.md)), at most 4 at once, as they are this small. A call keeps its place until its result has gone, so a stream slower than the agent can't pile results up. Uploads and downloads, which aren't this small, would go on a connection of their own, as logs do.

### A path that stops answering

A network mount whose far end is down can hold a system call for as long as it stays down, and nothing can stop one. So the agent's connection never depends on a file call returning:

- Each call does its disk work on a goroutine of its own, and what waits for it stops after 15 s, with the reason `path_not_answering`. The stream can end without it.
- What's left behind gives back its place and stays until the mount answers, however long that is.
- The next call to the same place is told at once: the whole mount, for a network file system or FUSE, or that path alone on anything else. So a dead mount costs one wait, and a folder with a link into one lists around it.
- At most 8 calls can be left behind, which bounds what a stuck call holds. Calls stop starting once 5 are, as the 3 more that could be under way can be left behind too.

A network mount isn't refused before it's walked into, as a share is a fair thing to browse, and most of the time it answers.

## Consequences

- The file manager works on every server, as it goes through the agent, with nothing on the agent's disk and no new listener. It adds one package to the agent build, and no module.
- `.env` files and the like are readable with a session and no passkey, and a stolen session can read them until it is revoked. The audit log shows which.
- Saving has no undo, so the panel has to make a save safe another way: it notices a file that changed since it was opened, and keeps what you typed.
- When roles come, Files belongs to whoever may be root on that server.
- The refused list has to grow with what Dockwell and Docker write. A new place that holds a value belongs on it, in the same change.
