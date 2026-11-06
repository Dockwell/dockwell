# Files: threat model

Files hands a browser a server's disk, through an agent that runs as root. This says what that protects, from whom, what's refused, and what it doesn't protect. The design is in [ADR 0031](../adr/0031-files-on-a-server.md), beside the threat models for [secrets](secrets.md) and [sign-in](sign-in.md). The code is in `internal/agent/files` and `internal/control/files`.

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

The agent refuses these whoever asks, over every surface, and no step-up opens them. Each refusal says why and points at SSH, and is in the audit log.

| Refused | Why |
| --- | --- |
| Dockwell's data directory, as configured and at `/var/lib/dockwell` | It holds the assets above |
| Docker's data root, other than the list of volumes and each volume's `_data`: at `/var/lib/docker`, where the engine says it is now, wherever it has said before, which the agent keeps on disk, and any directory that holds both `containers` and `volumes` | Each container's `config.v2.json` holds its environment. A volume's `opts.json` holds its options. Not knowing where the data is doesn't open it |
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
| List a directory or read a file, with any scope | Refused with `needs_session`, by the interceptor and again by the handler. Tested over Connect and REST, read-only and read-write |
| Use a person's session beside it | The token alone counts, and is refused ([ADR 0016](../adr/0016-scoped-api-tokens.md)) |

### A stolen session

| It tries to | What happens |
| --- | --- |
| Read a secret's value out of Docker's or containerd's state, or a process's environment | Refused: those paths are shut, for it and for the person it stole from |
| Read the install's key or the database, to open every secret offline | Refused the same way |
| Read `.env` files and app config | It can. Each read is in the audit log, by person, server and path. It reaches them with no passkey, as it reaches the server through a plan: see [known gaps](#known-gaps) |
| Read a file without leaving a record | It can't. The audit event is written before the contents leave the control plane, and a failed write returns nothing. Tested |
| Force a cross-site request | Reading a file is a POST, so the guard's origin checks apply. Listing is a GET: a page on another site can't read the answer, and it records nothing |

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
| Write markup in a file, to run in the panel | Contents are text in a `<pre>`, with nothing rendered, whatever the file's name says |
| Hide a private key from the check | It can. The check finds PEM on a line of its own, indented or not. See [known gaps](#known-gaps) |

### A mount that stops answering

A network mount or FUSE file system whose far end is down holds a system call for as long as it stays down, and nothing can stop one. Anyone who can reach Files can walk into one, by opening it, or a folder with a link into it.

| What it could do | What happens |
| --- | --- |
| Take the server offline, as the agent waits on the call when its stream ends | The agent never waits on a file call. Stopping leaves it behind, and the next stream opens. Tested with a call that never returns, end to end |
| Take every place a call runs in, so Files only says it's busy | A call left behind gives its place back within 15 s, and healthy paths carry on. Tested |
| Leave a person waiting | They hear that the path isn't answering within 15 s, or at once if it's already known. A folder around it lists by name alone |
| Pile up calls that can't be stopped | A place known not to answer takes no more calls: the whole mount, for a network file system or FUSE. At most 8 are left behind, and past that no call starts until one comes back. Tested |

### Something unprivileged on the server

It gets nothing new. The agent reads for the person who asked, and hands nothing back on the server.

## What this doesn't do

- **Keep a stolen session out of the disk.** A session controls every server: it can apply a plan that mounts `/etc`, with no passkey ([ADR 0021](../adr/0021-the-docker-driver.md#host-access)). There's no step-up here for that reason: it would add a prompt without closing that door.
- **Tell a config with a password in it from one without.** `.env`, `wp-config.php` and the like are readable with a session.
- **Protect one person on a server from another.** Every signed-in person can read every server's files until roles come.

## Known gaps

- **A file that holds a secret's value because an app wrote it there.** Dockwell delivers a value to a container's environment, and an app may copy it to a volume, a log or a config file. Files serves that as it serves any file. It's the same boundary as "deploying is reading" in [secrets.md](secrets.md#known-gaps).
- **A copy of something refused.** A backup of the data directory made by hand, under another path, is served like any file. The database is SQLite, which reads as binary, and the key is raw bytes, which do too, so neither's contents go out. A JSON or text copy of a token would.
- **An `EnvironmentFile` with `DOCKWELL_DATABASE` in it** is named by the operator, so Dockwell can't refuse it by its path. Keep the password in `/root/.pgpass`, which is refused. The database holds sealed specs, and reaching it isn't reaching the key.
- **Credentials in other forms.** A key that isn't PEM, such as a PuTTY key or one in the body of a config file, isn't refused, and neither is a token in a dotfile, such as the CLI's own.
- **A file over 1 MiB** is checked for a key only in what is read of it, and only that part goes out.
- **Eight stuck calls stop Files on that server** until one comes back, for paths and mounts alike. The agent, its stream and everything else on the server carry on. A call stuck in the kernel can also outlast the agent's own stop, as nothing can end it until the mount answers.
- **A hard link to a refused file,** made by root, reads as what it is: a file somewhere else.
- **Part of a Docker data root, apart from the rest.** A root no one has named is known by holding both `containers` and `volumes`. A backup of `overlay2` alone, or a root that has never answered and holds only one of them, has nothing to tell it by, so what's in it is served, other than the files refused by what they are.
