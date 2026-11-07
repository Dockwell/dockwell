# 0052. A server's firewall, SSH and fail2ban, each through a plan

- Status: Proposed
- Date: 2026-11-07

## Context

The PRD makes a server's firewall, fail2ban and SSH hardening a launch requirement (Server, P0), and principle 2 names firewall rules among what every change previews, applies and undoes. 1Panel has all three in its free edition, and an owner moving off it loses them otherwise (#260).

What exists: the `firewall.rule` kind, its driver in `internal/resource/firewall`, and an in-memory backend. No agent registers the driver, so a rule can be planned and runs nowhere.

Each of the three decides who reaches the server. A wrong rule can lock its owner out of SSH, of the panel, or, on the control server, every agent out of the control plane. Server-Firewall.dc.html answers that the way routers and cloud consoles do: a change applies, and rolls back on its own unless someone confirms within 60 s that they still have access. A design review of this record (#288) found that the timer alone protects much less than it seems, and this record is built around what it doesn't cover.

Servers arrive with a firewall already: ufw on Ubuntu, firewalld on AlmaLinux, Docker's own rules everywhere, sometimes plain nftables or iptables-legacy a person wrote. Dockwell is a guest on them, as it is with containers it didn't make ([ADR 0031](0031-containers-dockwell-didnt-make.md)).

## Decision

### nftables, in a table of Dockwell's own

The agent writes one nftables table, `inet dockwell`, and nothing else. `inet` holds IPv4 and IPv6 alike, so a rule covers both unless its sources are of one family. Each change replaces the whole table in one `nft -f -` transaction, `table inet dockwell {}; delete table inet dockwell; table inet dockwell { … }`, so the server is never between two rulesets, and a failed load changes nothing.

nftables runs every base chain on a hook, in every table, and a packet passes only if none of them drops it. iptables-legacy rules run beside nftables's on the same hooks, and a drop in either wins there too. So:

- **Dockwell's table can close what another opens, and can't open what another closes.** That's true both ways: a Dockwell drop overrides a person's own accepts and Docker's. Beside ufw or firewalld, the Firewall tab says which tool also filters, and that a port it closes stays closed whatever Dockwell allows.
- **Rules Dockwell didn't make are the server's own.** The agent reads ufw through `ufw status verbose`, firewalld through `firewall-cmd`, other nftables tables through `nft -j list ruleset`, and iptables-legacy through `iptables-legacy-save`, and the tab lists them, read-only, as containers Dockwell didn't make are. Nothing Dockwell does changes them.
- **The tab works out reach twice:** for what listens on the host, which input rules cover, and for ports Docker publishes, which reach their containers before input rules see them. ufw's and firewalld's rules don't close a published port, and the tab says so beside each.

### What Dockwell's table holds, and in what order

- **Inbound is allowed by default, until someone chooses otherwise.** A server Dockwell meets keeps working as it did. Deny everything else, the mock's fixed last rule, is a policy a person turns on, `firewall.policy`, which is a resource like a rule and so a plan too. Turning it on lists, in the plan, every port something on the server listens on that no rule allows. The owner decided this on 2026-11-07: a default of deny would need every server's ports listed before it joined to be safe, where this leaves each as it was until someone closes it on purpose.
- **Rules apply in a fixed order, whatever order they were made in:** what's always allowed, then every allow, then every deny, then the policy. `FirewallRule` has no order of its own, so that's what decides which wins. With inbound open by default, an allow matters only as an exception to a deny or to the policy. The plan names any rule another makes useless, such as an allow no deny or policy narrows.
- **What keeps the server reachable is always allowed, and comes first:** traffic of connections already open; loopback; ICMP's and ICMPv6's errors, types 1 to 4, and IPv6 neighbor and router discovery, types 133 to 137 with a hop limit of 255; DHCPv4 and DHCPv6 replies, so a lease renews; and invalid packets are dropped. On the control server, the agent listener and the panel's own ports are pinned too. A rule that would deny one of these is refused when it's planned, saying which it would cut.
- **On a control server with no domain, SSH is a panel port.** The panel is then only on `127.0.0.1:7600`, reached by `ssh -L` (ADR 0011), so the SSH port, read from `sshd -T` and not assumed to be 22, is pinned too, at least from the address in use now. With a domain, 80 and 443 may be narrowed under the timer, but a source list that leaves out the address the person is using now is refused.
- **Under deny everything else, containers still reach the host,** as an app reaches a database on the host's address; the plan says so.

### Where each rule goes

Each rule goes in both places a packet can arrive:

- **The input chain,** for what listens on the host, and for what Docker's userland proxy accepts, which arrives there: IPv6 to an IPv4-only published port, and hairpin traffic from the host to itself.
- **The forward chain, for ports Docker publishes.** It matches traffic Docker translated (`ct status dnat`) by the port it arrived on, `meta l4proto tcp ct original proto-dst 8080`, so `8080/tcp` means what a person means by it: the port on the server. It matches only traffic arriving on an interface that isn't one of Docker's, and the agent takes Docker's interface names from Docker's network list, not by matching `br-*`.

Both chains hook at `filter`, priority 0: a chain at -200 or lower runs before connection tracking, and couldn't see `ct original`. Traffic between containers, and from them out, is never filtered, and outbound is never filtered: the agent's stream to the control plane goes out, and so do the packages, images and certificates a server fetches.

**Some paths to a container can't be filtered here, and the plan says so for any rule they'd skip:** Docker 28's `routed` and `nat-unprotected` modes, and IPv6 published without NAT, have no translation to match; macvlan and ipvlan networks bypass the host's firewall entirely.

### The confirmation timer, and what it doesn't prove

A change to the firewall or to SSH applies, then waits for a confirmation:

1. Each agent the change reaches applies all of that change's steps for its server in one transaction, under one timer of 60 s, keeping what they replaced. One confirmation covers the change, not each step.
2. The step's Verify waits up to 90 s, the timer and room for its answer to come back, for the agent's verdict.
3. **The agent decides.** The step passes only on its "kept" answer, sent once a confirmation reached it before the timer ran out. A confirmation that arrives after gets "already rolled back", and the step fails. Confirming and running out can't both win: the agent takes whichever comes first.
4. If the timer runs out, the agent puts the old rules back on its own, without the control plane, which it may no longer reach, and the change rolls back as any failed change does.
5. **Automatic rollbacks, and steps that change nothing, apply with no timer.** A rollback restores what was confirmed before, so it needs no confirmation of its own, and has nothing to race.
6. The agent records a spec as what it should hold only once it's confirmed, so its cache of desired state (#32) never re-applies rules that were rolled back.

**The timer protects only what the person tests.** Open connections stay allowed, and reloading sshd keeps open sessions, so a click over the panel connection they already have succeeds after almost any change. So:

- **The confirm dialog names the test:** "Open a new SSH session to port 22 from where you are now, then confirm." For a change to the panel's ports, it's a new tab on the panel's address.
- **Where it can, the control plane tests on its own before it confirms:** a fresh TCP connection to the server's SSH port, and a fresh connection to its agent, not the stream already open, and the person sees what each found.
- **The rest is checked before anything applies,** at plan time, by the rules above and below: what's pinned, and that someone can still sign in.

**The table stays in place.** At start, before it connects anywhere, the agent loads the last confirmed table; a change that was unconfirmed when it stopped reverts at once, so a reboot inside the 60 s rolls back. On every reconcile, it compares the table with what it wrote, puts it back if something changed or removed it, such as `nft flush ruleset`, which Debian's default `nftables.service` runs as it starts, and puts that drift on Activity. A small unit ordered before the network and Docker loads the last confirmed table at boot, so published ports aren't open with no Dockwell rules until the agent starts; on OpenRC the same runs as an early service. Until that unit ships, the tab says so.

### SSH

SSH hardening is a resource, `ssh.config`: whether passwords and root may sign in, and the port. Who may reach the port isn't a second setting of its own: the SSH card reads and edits the firewall rules for sshd's port, so it's in one place.

- **It's written to `/etc/ssh/sshd_config.d/00-dockwell.conf`,** a drop-in of Dockwell's own. sshd keeps the first value it reads, and reads drop-ins in name order before the main file, so `00-` comes before Ubuntu's `50-cloud-init.conf` and RHEL 9's `50-redhat.conf`.
- **After writing it, the agent reads `sshd -T` and refuses a change whose values in effect differ from the spec,** naming the file that wins, as an open `Match` block in an earlier file can. `sshd -t` passes in all of those cases, so it isn't enough. The tab shows the values in effect, not what Dockwell wrote.
- **Where `sshd_config` doesn't include its drop-in folder,** as the RHEL 8 family's doesn't and Alpine's may not, Dockwell says so and changes nothing. It never edits `sshd_config`.
- **"Password login off" turns off `PasswordAuthentication` and `KbdInteractiveAuthentication` both,** as PAM still asks for a password through the second while it's on.
- **Turning off passwords or root login checks, at plan time, that someone can still sign in:** an account still allowed in has a key, root's if root may sign in with keys, or a member of `sudo` or `wheel`'s, following `AuthorizedKeysFile`. It refuses when none has one. Where `AuthorizedKeysCommand` or `TrustedUserCAKeys` is set, Dockwell can't tell, and the plan says so and asks the person to say they've checked.
- **On a control server with no domain, a change that could stop the person's next login is refused** (its port, passwords or root login), as SSH is the way into the panel there; the plan says to give the panel a domain first.
- **Changing the port lists every step it takes,** and refuses where another tool would close the new port: on Ubuntu 22.10 and later, where `ssh.socket` holds the port, `daemon-reload` and a restart of the socket; on the RHEL family with SELinux, `semanage port -a`; ufw or firewalld, which it can't open; the firewall rules and the fail2ban jail for the old port, which move with it. sshd reloads with `systemctl reload` under systemd and `rc-service sshd reload` under OpenRC.
- **The terminal reaches the server through the agent,** so closing port 22 leaves a way in, once that terminal is there (#208). Until it merges, `ssh.config` doesn't ship, and the way back is the provider's console.

### fail2ban

fail2ban is read where it's installed: its jails and recent bans, through `fail2ban-client`. A jail for SSH and one for the ingress are resources, `fail2ban.jail`, written to `/etc/fail2ban/jail.d/dockwell.conf`.

- **Its bans aren't in `inet dockwell`,** which is replaced whole: fail2ban keeps them in its own table, as it does.
- **fail2ban takes the last value it reads,** so a person's `jail.local` overrides Dockwell's file, and Debian already turns on `[sshd]` in `defaults-debian.conf`. The tab shows each jail as fail2ban says it runs, not what Dockwell wrote.
- **The SSH jail reads the journal** (`backend = systemd`), as Debian 12 has no `/var/log/auth.log`.
- **The ingress jail watches Caddy,** which signs nobody in itself, so it needs a log source and a filter of its own: what it counts as a failure is decided with that slice. 1Panel's `openresty-auth` is its own jail, and isn't carried over.
- **`fail2ban-client -t` runs before every reload,** and the control server's addresses are in `ignoreip`.
- **A jail change can't lock anyone out of a new session,** so it has no timer. Unbanning an address is a change with a plan and no revision, as a restart is.
- **Installing fail2ban** is the package manager's, which #276 brings.

### Who may change them

Every change to `firewall.rule`, `firewall.policy`, `ssh.config` and `fail2ban.jail` takes a signed-in person and a passkey just now, as anything that decides who gets onto the server does ([ADR 0032](0032-files-on-a-server.md)), and an admin or the owner (ADR 0048, in #255). No API token may make one. The person who applied it confirms it, in that session. Unbanning an address is an admin's, with no passkey, and is on Activity.

### How the agent runs its commands

The agent drives `nft`, `sshd`, `fail2ban-client`, `ufw`, `firewall-cmd`, `iptables-save`, `iptables-legacy-save`, `systemctl`, `rc-service` and `semanage` as commands, as ADR 0043 (#276) runs a package manager: by fixed paths, with no shell, a clean environment with `LC_ALL=C`, stdin from `/dev/null`, a deadline each, capped output, and one at a time. It writes with `nft -f -` and reads with `nft -j`. It links no netlink or nftables library, so its idle memory doesn't move, and it reads only while someone looks.

**A server without `nft`** says so, apart from one whose kernel won't load a table, as LXC and OpenVZ containers can refuse: each has its own words in the tab, and neither can be changed. Alpine and some minimal images ship without `nft`; installing `nftables` is a plan step through #276's install path, or `install.sh`'s.

### How this differs from the mock

- **The locked "Always last" row** becomes the policy, on or off, as the owner decided.
- **Rules other tools made** need a state of their own in the mock: read-only, under each tool.
- **"Used by"** needs what listens on the host and what Docker publishes, which the agent reads alongside.
- **The bans chart** needs a history `fail2ban-client` doesn't keep; Dockwell would keep it, which is decided with fail2ban's slice.
- **`openresty-auth`** is 1Panel's jail; Dockwell's would watch Caddy.

## Consequences

- A server keeps working as it did when Dockwell arrives; closing everything else is a choice, made with a plan that lists what would close, as the owner decided.
- Dockwell can't open what ufw, firewalld or iptables-legacy close, and says so. Someone who wants Dockwell to own the firewall turns those off, which the tab says how to do.
- Every firewall and SSH change costs up to 60 s of waiting for a confirmation, and the confirmation is only as good as the test the person runs, which the dialog names. What the timer can't see, a locked-out next login, is refused before anything applies.
- The table survives reboots and flushes, as the agent and a boot unit put it back, and drift shows on Activity.
- Tests run the table against real nftables in CI, in a privileged container, and against a fake elsewhere. The install-test images (Debian, Ubuntu, AlmaLinux, Alpine) check the distributions' defaults this record relies on: `nft`'s presence, the sshd drop-ins and their order, fail2ban's defaults, and `ssh.socket`. Those defaults come from the review's knowledge, not live images, until those tests run.
