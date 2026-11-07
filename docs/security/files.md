# Files: threat model

Files hands a browser a server's disk, to read and to write, through an agent that runs as root. This says what that protects, from whom, what's refused, and what it doesn't protect. The design is in [ADR 0032](../adr/0032-files-on-a-server.md), beside the threat models for [secrets](secrets.md) and [sign-in](sign-in.md). The code is in `internal/agent/files` and `internal/control/files`.

## What there is to protect

| Asset | Where it lives | Why it matters |
| --- | --- | --- |
| The install's key and the database | Dockwell's data directory, on the control server | Together they open every secret |
| The agent's key, the proxy's certificates and ACME keys | Dockwell's data directory, on every server | They stand in for the server, and for its sites |
| Secrets' values on a server | Docker's and containerd's state, and each process's environment | ADR 0022 keeps them from every API response without a step-up |
| A CIFS volume's password | `opts.json`, beside the volume in Docker's data root | The same |
| The server's own credentials | `/etc/shadow`, SSH keys, TLS keys | They outlive anything Dockwell can revoke |
| Everything else on the disk | Everywhere | Apps' config, data and users' files |

## What's refused

The agent refuses to read or save these whoever asks, over every surface, and no step-up opens them. Each refusal says why, and points at SSH. Every refused or failed read or save, and every refused listing, is on Activity, whatever the reason, so a miss shows as a hit does. A listing that works isn't, as it returns names and runs on every click, and neither is one that can't reach its server, as the panel asks again by itself.

| Refused | Why |
| --- | --- |
| Dockwell's data directory, as configured and at `/var/lib/dockwell` | It holds the assets above |
| Docker's data root, other than the list of volumes and each volume's `_data`: at `/var/lib/docker`, where the engine says it is now, wherever it has said before, which the agent keeps on disk, and any directory that holds `containers` and at least two more of Docker's own directories, with links followed. A project with its own `containers` and `volumes` isn't one | Each container's `config.v2.json` holds its environment. A volume's `opts.json` holds its options. Not knowing where the data is doesn't open it |
| `config.v2.json`, `hostconfig.json`, an `opts.json` under a `volumes` directory, and any text with Docker's own keys in it, such as `MountOpts`, or `ResolvConfPath` with `Env`, which `docker inspect` prints | The same files, wherever a copy or a moved root leaves them |
| `/var/lib/containerd` | It holds what Docker's does, on an engine that keeps images there |
| `/proc`, `/sys`, `/dev` and `/run` | They aren't files: a process's environment, a container's disk through `/proc/<pid>/root`, the server's disks, and each container's runtime spec, which holds its environment. `/var/run` leads to `/run` |
| `/etc/shadow`, `/etc/shadow-`, `/etc/gshadow`, `/etc/gshadow-` | The server's password hashes |
| Any file named `.pgpass` | It can hold the control plane's database password |
| Any file whose text has lines of password hashes, `name:$id$…`, locked or not | A copy of a shadow file, such as `/var/backups/shadow.bak`, or a `.htpasswd`, whatever it is called |
| Any file whose text has a PEM private key on a line of its own, indented or not, or after a byte order mark | SSH host keys, users' SSH keys and TLS keys, wherever they are and whatever they are called, in a YAML file too |
| Anything that isn't a regular file or a directory | Sockets, FIFOs and devices have nothing to show |

## Who we consider

| Who | What they have |
| --- | --- |
| An API token, read-only or read-write | A bearer secret from a CI log or a dotfile ([ADR 0016](../adr/0016-scoped-api-tokens.md)) |
| A stolen session | A person's cookie, with no passkey. It acts as the person until it is revoked ([sign-in.md](sign-in.md#known-gaps)) |
| A compromised app | Code in a container, and whatever it can write to a volume: symlinks, FIFOs, odd names, huge directories |
| Something unprivileged on the server | Files others can read, and paths it can create |
| A signed-in person | Root on the server, in effect. Nothing here defends against them |

## What each can and can't do

### An API token

| It tries to | What happens |
| --- | --- |
| List a directory, read a file or save one, with any scope | Refused with `needs_session`, by the interceptor and again by the handler. Tested over Connect and REST, read-only and read-write |
| Use a person's session beside it | The token alone counts, and is refused ([ADR 0016](../adr/0016-scoped-api-tokens.md)) |

### A stolen session

| It tries to | What happens |
| --- | --- |
| Read a secret's value out of Docker's or containerd's state, or a process's environment | Refused: those paths are shut, for it and for the person it stole from |
| Read the install's key or the database, to open every secret offline | Refused the same way |
| Read `.env` files and app config | It can. Each read is on Activity, by person, server and path. It reaches them with no passkey, as it reaches the server through a plan: see [what this doesn't do](#what-this-doesnt-do) |
| Read a file without leaving a record | It can't. The audit event is written before the contents leave the control plane, and a failed write returns nothing. Tested |
| Look for credentials and leave no trace of what it missed | It can't. A refused or failed read is on Activity however it failed: nothing there, not a file, denied, refused, or not answering. So is a refused listing. Tested |
| Hide in the record with a crafted path | A name can hold a newline or a terminal's escape codes, and a link can lead through one. A path is cleaned before it's stored or shown. Tested |
| Save a file, such as a cron job or `authorized_keys`, to get root or keep a way in | It can't without the person's passkey, just now, for that path on that server, however it got there: a save to one of these takes a step-up, decided by the agent on where the walk ended. With no passkey at that address, it can't at all. Each one is on the timeline in a warning tone, and in the audit log. Tested |
| Save a file without leaving a record | It can't. `file.saving` is written before the agent is asked, and if it can't be written nothing is. Tested |
| Write over the install's key, or a secret's value in Docker's state | Refused: a save takes the same walk as a read |
| Force a cross-site request | Reading a file is a POST, so the guard's origin checks apply. Listing is a GET: a page on another site can't read the answer |

### A compromised app

It can't reach past its container on its own. What it can do is leave things for a person to open.

| It tries to | What happens |
| --- | --- |
| Plant a symlink in its volume, to `/var/lib/dockwell/control/key` or `/etc/shadow` | The agent follows links itself, and refuses where this one ends. Tested |
| Swap a directory for a symlink between a check and a read | There's no gap to swap in: the agent opens each part with `openat` and `O_NOFOLLOW` from the descriptor it checked, and reads what it opened |
| Plant a FIFO, a socket or a device | Refused by its type, before it is opened for reading, and opened without blocking if it changes in between |
| Fill a directory with entries, or with long link targets, to take the agent's stream down | A listing stops at 5,000 entries and 4 MiB, and says so |
| Run many large listings at once, on a stream it has slowed, to fill the agent's memory | A call keeps one of 4 places until its result has gone, so 4 listings are all that wait to send. Tested |
| Name a file so it can't be sent, with bytes that aren't UTF-8 | The listing carries on, and shows that one name as well as it can, marked. It can't be opened from the panel |
| Write markup in a file, to run in the panel | Contents are text in a text field, with nothing rendered, whatever the file's name says |
| Hide a private key from the check | It can. The check finds PEM on a line of its own, indented or not. See [known gaps](#known-gaps) |
| Plant a symlink where a person will open a file, to have them save through it | The save follows the link and writes where it leads, as a read does, unless that's refused. The panel shows a link's target in the folder and over the file, and the audit log and the timeline name where the save landed. Tested |
| Swap a file a person has open for a link to `authorized_keys`, even with the same text, for their save to land there | The version covers where the walk ended and which file that was, so the save reads as changed, and writes nothing. "Save over it" reads the path again, sees it leads elsewhere, stops, and names where, so a person only saves there with it in front of them. Tested |
| Swap a file for a link or another file as a save goes on | The save writes only to the file it read: the agent opens it from the directory it checked, with `O_NOFOLLOW`, and compares its identity. Anything else reads as changed, and nothing is written. Tested |
| Change a file a person is editing, to have their save undo it, or to slip something in | A save names the version it was edited from, and a file that differs isn't written over. The person sees that it changed, and chooses. Tested |
| Have a person's session overwrite a binary it knows the digest of, such as `/usr/bin/sudo` | A save goes only over what reads as text, and the version counts for nothing on anything else. Tested |
| Fill the disk, so a save leaves a file cut short | A copy of the old text goes beside the file first. A disk that can't hold that stops the save with the file untouched. One that holds the copy can still fail the write, as the file grows: the old text goes back, and failing that the copy stays, and the error names it, with the file cut short. Tested, with a real write that fails partway |

### A mount that stops answering

A network mount or FUSE file system whose far end is down holds a system call for as long as it stays down, and nothing can stop one. Anyone who can reach Files can walk into one, by opening it, or a folder with a link into it.

| What it could do | What happens |
| --- | --- |
| Land a save after the person has gone on | A save reads the file and checks its caller after flushing its copy, which is where a stalled disk holds it, so one left behind there writes nothing, and the person hears that. Tested |
| Leave a save half done | A save left behind as it writes can't be stopped. The person hears it may still land, Activity says so, and when it comes back Activity says how it ended, as whoever saved it. Tested |
| Take the server offline, as the agent waits on the call when its stream ends | The agent never waits on a file call. Stopping leaves it behind, and the next stream opens. Tested with a call that never returns, end to end |
| Take every place a call runs in, so Files only says it's busy | A call left behind gives its place back within 15 s, and healthy paths carry on. Tested |
| Leave a person waiting | They hear that the path isn't answering within 15 s, or at once if it's already known. A folder around it lists by name alone |
| Pile up calls that can't be stopped | A place known not to answer takes no more calls: the whole mount, for a network file system or FUSE. At most 8 are left behind: no call starts once 5 are, as the 3 more that could be under way can be left behind too. Tested |

### Something unprivileged on the server

It gets nothing new. The agent reads for the person who asked, and hands nothing back on the server.

## What this doesn't do

- **Keep a stolen session out of the disk.** A session controls every server: it can apply a plan that mounts `/etc`, with no passkey ([ADR 0021](../adr/0021-the-docker-driver.md#host-access)). Reading a file takes no step-up, as that would add a prompt without closing that door. Saving to a way back in does, and so does a plan that's as good as root: see [ADR 0032](../adr/0032-files-on-a-server.md#a-step-up-for-a-way-back-in-and-for-nothing-else).
- **Stop a stolen session reaching what's refused.** The refused list stops Files. ADR 0021 lets a session mount Docker's socket with no passkey, and through it reach everything here.
- **Keep a stolen session out of a server with no Docker by any other way.** With no plan to run, Files is new reach there, and Activity is what shows it.
- **Undo a save.** A file isn't managed state, so there's no revision to go back to. The panel says so beside the Save button.
- **Know what a file does.** A save to a cron job, a unit file or `sudoers` is root, and takes a passkey, but Dockwell doesn't read what's in it or check its syntax. What runs that isn't on the list, such as a script a cron job calls, takes none.
- **Keep what's typed in a tab that closes.** What's typed stays in memory through a session that ends, and leaving the page asks first, but it never goes in the browser's storage, so a tab the browser discards loses it.
- **Tell a config with a password in it from one without.** `.env`, `wp-config.php` and the like are readable with a session.
- **Protect one person on a server from another.** Every signed-in person can read every server's files until roles come.

## Known gaps

- **A file that holds a secret's value because an app wrote it there.** Dockwell delivers a value to a container's environment, and an app may copy it to a volume, a log or a config file. Files serves that as it serves any file. It's the same boundary as "deploying is reading" in [secrets.md](secrets.md#known-gaps).
- **A copy of something refused.** A backup of the data directory made by hand, under another path, is served like any file. The database is SQLite, which reads as binary, and the key is raw bytes, which do too, so neither's contents go out. A JSON or text copy of a token would.
- **An `EnvironmentFile` with `DOCKWELL_DATABASE` in it** is named by the operator, so Dockwell can't refuse it by its path. Keep the password in `/root/.pgpass`, which is refused. The database holds sealed specs, and reaching it isn't reaching the key.
- **Credentials in other forms.** A key that isn't PEM, such as a PuTTY key or one in the body of a config file, isn't refused, and neither is a token in a dotfile, such as the CLI's own.
- **A file over 1 MiB** is checked for a key only in what is read of it, and only that part goes out.
- **Five stuck calls stop Files on that server** until one comes back, for paths and mounts alike. The agent, its stream and everything else on the server carry on. A call stuck in the kernel can also outlast the agent's own stop, as nothing can end it until the mount answers.
- **A hard link to a refused file,** made by root, reads as what it is: a file somewhere else.
- **Activity shows paths.** Anyone who can read events, a read-only token included, sees which files were read, saved and refused, not what's in them.
- **Part of a Docker data root, apart from the rest.** A root no one has named is known by holding `containers` and at least two more of Docker's own directories. A backup of `overlay2` alone, or of `containers` alone, has nothing to tell it by, so what's in it is served, other than the files refused by what they are.
- **A save has a moment in the open.** The file is read and written moments apart, so a writer that lands between them is lost. A reader can catch a file half written, as a save writes in place to keep what a container mounts. A power cut in that moment can leave it half written, and the copy of what was saved beside it, as `.dockwell-save-…`.
