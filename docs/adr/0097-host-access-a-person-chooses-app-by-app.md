# 0097. Host access a person chooses, app by app

- Status: Accepted
- Date: 2026-11-14

## Context

Taking on a container refuses what a spec can't hold ([ADR 0031](0031-containers-dockwell-didnt-make.md)), and among the shapes the Coolify and Dokploy imports ([ADR 0089](0089-importing-from-coolify-and-dokploy.md)) still leave behind are those that reach the server past what Docker gives a container. Counting each panel's templates: `devices` in 3 of Coolify's and 6 of Dokploy's, `security_opt` past `no-new-privileges` in 3 and 4, `sysctls` in 2 and 2, `pid: host` in 1 and 2, and `gpus` in one of Dokploy's. They're media servers with a GPU or a video device, VPNs, home automation with a USB stick, and monitoring that reads every process.

What was settled already:
- [ADR 0021](0021-the-docker-driver.md): anything a spec reaches on its server is host access, which the plan lists, a signed-in person approves and no token may apply; what's as good as root (`same_as_root`) leads in red, with the fingerprint typed.
- [ADR 0049](0049-roles-and-invites-by-project.md): applying a plan with host access is an admin's or the owner's, whatever a project's grants say. A change that keeps an app's host access as it is needs nothing more.

## Decision

### The spec holds them

- **Five fields of `Container`:** `devices` (101: each server path, the path in the container, and `r`, `w` and `m`), `gpus` (102: a count, `-1` for all, or device IDs, with capabilities and a driver, as `--gpus` asks), `security_opt` (103: `seccomp=unconfined`, `apparmor=unconfined`, `label=disable`, `label=type:…` and the other label fields, and `systempaths=unconfined`), `host_pid` (104), and `sysctls` (105).
- **What still refuses, as now:** a device cgroup rule, a seccomp profile, a GPU request with a driver's options or more than one, `pid` of another container, `ipc: host`, and a security option of any other form. A spec can't say those, so nothing holds them.
- **They read back as asked.** The agent writes each to Docker and reads it back in one form: devices in order, with `rwm` as none; a request with `gpu` as no capability; security options in order, `key=value`; `PidMode` as `host_pid`; sysctls as they are. A container taken on, and one Dockwell makes, compare without drift. An export writes them as docker compose does, `gpus` among them, rather than drop them.

### Sysctls are a field like any other

- **Only Docker's namespaced set:** `net.*`, `kernel.shm*`, `kernel.msg*`, `kernel.sem` and `fs.mqueue.*`, which are the container's own namespaces' and reach nothing of the server. So they're no host access, and need no choice.
- **What Docker would fail on refuses at plan, with why:** any other sysctl, which would change the server's, and a `net.*` one on the server's own network, where it would. `ipc: host`, which an IPC sysctl would change the server's for, isn't a spec's to say yet, so it refuses before that matters.

### Host access, chosen app by app

- **The default is no.** An app that needs any of the other four is left behind, as now, until a person chooses it, app by app: `host_access` on `ImportCoolify` and `ImportDokploy` (the apps' IDs, `ImportStep.app_id`), a boolean on `AdoptCompose` and `AdoptContainer`, and the CLI's flags from those.
- **What the person sees, on Move in:** the state "Needs host access", and a checkbox, not checked, that says what exactly in one line, as "Take it on with /dev/dri/renderD128, all GPUs" (`ImportStep.needs_host_access`). No prose under it. Choosing reads again, and the app comes with the rest.
- **What the plan shows:** each as host access on its step and its resource, in the plan sheet's words: "uses the device /dev/dri/renderD128", "reads the device …" for one it only reads, "uses all the GPUs", "runs with seccomp=unconfined", "sees and signals every process".

### How loud each is

- **A device by what it is, not by its name.** The agent stats the server path as a plan is made, through links: a block device; a character device whose kernel subsystem reaches a disk or raw memory (`scsi_generic`, `bsg`, `nvme-generic`, `mtd`, `raw`, and `mem`'s `/dev/mem`, `/dev/kmem` and `/dev/port`); or anything under `/dev/mapper`, `/dev/bsg/`, `/dev/mtd` or `/dev/raw/`, or `/dev/sg` and `/dev/ng` with a digit next (so Intel SGX's `/dev/sgx_*` isn't), is the same as root. Only a device `/sys/dev/char` has no entry for reads as plain: a `/sys` the agent can't read fails the plan, naming the device and how to let it read, as a quiet amber would be the wrong way to be wrong. Like where a path leads, that's the agent's to tell, and it can only make a plan louder (`Decision.DeviceRoot`, carried as a `DEVICE` entry that's the same as root). A device that isn't on the server fails the plan, by its path. Those names are the same as root from the control plane's reading too, whatever the agent says. The plan keeps which devices it found the same as root (`ResourcePlan.root_devices`), and the next plan compares with what was approved: one that's the same as root now and wasn't then is gained, and a plan's roles reach and admin's gate read the same verdict.
- **SELinux:** a type is amber only where it confines, `container_t`, `container_init_t`, `container_kvm_t` and `svirt_lxc_net_t`, and any other, with `label=user:` and `label=role:`, is the same as root. So is `label=disable`, which on a server with SELinux runs the container as `spc_t`, unconfined, as `label=type:spc_t` does; `apparmor=unconfined` and `seccomp=unconfined` are amber. `systempaths=unconfined` is the same as root. So is `host_pid`.
- **The daemon's own `label=disable` is none of the spec's.** Docker adds it to a privileged container, and to one with the server's processes or IPC, that asks no label, so it isn't read back, and a spec can't hold it beside those. A plan that turns those off says, in one line, that the app runs under SELinux's labels again, and how to keep them off (`confined_again`).
- **A GPU request says its driver and capabilities,** and a driver is Docker's default, `nvidia` or `cdi`; CDI device IDs are shown as devices.
- **Together they're more.** Host access with a `cap_add` of `SYS_ADMIN`, `SYS_MODULE`, `SYS_PTRACE`, `SYS_RAWIO` or `ALL`, or with `privileged`, is the same as root for the whole app, said once, at the app's first line (`ROOT_TOGETHER`), and the rest says what it is.

### Who may apply it

- **As for any host access:** a signed-in person, never a token, and an admin or the owner, whatever the project's grants. A developer can plan it, and can't apply it.
- **The choice stays the app's.** Once applied, it's in the app's spec, so a later change that keeps it, as a new image, needs nothing more, and one that widens it, as another device, goes through the same gate. A developer can't widen it with `services update`.
- **An agent that can't run them gets none.** One from before would make the container without them, so an agent says it runs them (`Hello.runs_host_access`, 337), and the control plane plans, applies and remembers none on one that doesn't, with how to update it. The fields that came beside them, `Healthcheck.start_interval` and `Container.network_aliases`, go through the same gate, and one function names them all for the agent and the control plane.
- **Making a Swarm app again asks the same choice,** by the app's ID. 1Panel's import has none to make, so an app of it that needs some is left, saying the take-on that chooses it: `dockwell compose adopt --name <project> --host-access`, or `AdoptCompose` with `host_access`.

### Server paths a person chooses

*Added 2026-11-14, from the gaps survey of #604.*

About 10 of the panels' templates mount a server path no container could mount at all: monitoring and log tools read the server's processes, the kernel's settings or every container's log, a file manager the whole of the server, and Metabase, in both panels, the kernel's random numbers.

- **Five become host access a person chooses, app by app, each the same as root:** `/proc` and what's in it, read-only or not, as it shows every process and its environment; `/sys` and what's in it, read-only alone, as it shows the kernel's devices and settings, and refused read-write, as a container only needs to read them; Docker's folder of every container's log, `<Docker's data directory>/containers`, which holds each one's output, secrets included; `/dev/disk` and what's in it; and the whole of `/`, which also holds Docker's socket. A container taken on asks for the choice as for a device: Move in's checkbox, `host_access` on the take-on, and a plan that shows it in red. Every other system path still refuses (`/boot`, the rest of `/dev`, and Docker's and Dockwell's data directories), and none of the five may be a volume's bind.
- **Docker's folder of every container's log is taken as it is, never followed.** The agent follows any other server path's symlinks to check where it leads, and refuses one it can't follow. This folder is root's alone on every server, so an agent that doesn't run as root could never follow it, and the choice would be no use. It's Docker's own, at the data directory Docker names, which nothing a container writes can lead elsewhere, so there's nothing to follow.
- **`/dev/urandom` and `/dev/random`, mounted read-only, are no host access at all:** every container reads them already. They're taken on with no choice and show nothing in a plan. Mounted read-write, they're refused, with how to fix it.
- **A Compose file a person imports, and a catalog app, ask for the five in their plan,** as for any host access: the same as root, in red, with the typed fingerprint, which only a signed-in admin applies. That's the same choice a person makes for each install, so they convert as before. A catalog app waits, as any that reaches its server does, until a person has looked at what it reaches. Before this, the stores converted five apps that mount them (a backup tool, two file managers, a monitor and a game server's daemon), whose plans every server then refused. Only a take-on, whose plan asks nothing of the kind, needs the choice made first. A spec a person writes through the API can hold them too.

### Numbers

`Container` 101 to 105, `Device` and `Gpus`; `HostAccessKind` 170 to 174 (`DEVICE`, `GPUS`, `SECURITY_OPTION`, `HOST_PROCESSES`, `ROOT_TOGETHER`); `Hello.runs_host_access` 337; `ImportCoolifyRequest.host_access` and `ImportDokployRequest.host_access` 3, `AdoptComposeRequest.host_access` 5, `AdoptContainerRequest.host_access` 6, `ImportStep.app_id` 8 and `needs_host_access` 9; the plan's `root_devices` is in its stored JSON, not the API.

## Consequences

- An app that needs a GPU, a USB stick or a VPN's tun device comes over once a person says so, and its plan says plainly what it reaches.
- Sysctls are taken on with no choice, which brings WireGuard's apps over as they are.
- A device that goes missing from the server, as a USB stick unplugged, fails the next plan for the app until it's back or taken out.
- A Compose file a person imports, and a catalog app, still can't ask for these: their converters refuse them as before, and choosing them there is a later decision.
