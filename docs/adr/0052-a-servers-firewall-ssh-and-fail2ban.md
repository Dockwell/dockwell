# 0052. A server's firewall, SSH and fail2ban, each through a plan

- Status: Proposed
- Date: 2026-11-07

## Context

The PRD makes a server's firewall, fail2ban and SSH hardening a launch requirement (Server, P0), and principle 2 names firewall rules among what every change previews, applies and undoes. 1Panel has all three in its free edition, and an owner moving off it loses them otherwise (#260).

What exists: the `firewall.rule` kind, its driver in `internal/resource/firewall`, and an in-memory backend. No agent registers the driver, so a rule can be planned and runs nowhere.

Each of the three decides who reaches the server. A wrong rule can lock its owner out: of SSH, of the panel, or, on the control server, every agent out of the control plane. Server-Firewall.dc.html answers that the way routers and cloud consoles do: a change applies, and rolls back on its own unless someone confirms within 60 s that they still have access.

Servers arrive with a firewall already: ufw on Ubuntu, firewalld on AlmaLinux, Docker's own rules everywhere, sometimes plain nftables a person wrote. Dockwell is a guest on them, as it is with containers it didn't make ([ADR 0031](0031-containers-dockwell-didnt-make.md)).

## Decision

### nftables, in a table of Dockwell's own

The agent writes one nftables table, `inet dockwell`, and nothing else. `inet` holds IPv4 and IPv6 alike, so a rule covers both unless its sources are of one family. Each change replaces the whole table in one `nft -f` transaction, so the server is never between two rulesets, and a failed load changes nothing.

nftables runs every base chain on a hook, in every table, and a packet passes only if none of them drops it. So:

- **Dockwell's table can close what another opens, and can't open what another closes.** Beside ufw or firewalld, Dockwell's rules are drops on top of theirs. The Firewall tab says which tool also filters, and that a port it closes stays closed whatever Dockwell allows.
- **Rules Dockwell didn't make are the server's own.** The agent reads the other tables, and ufw's and firewalld's status when they're there, and the tab lists what they allow and drop, read-only, as containers Dockwell didn't make are. Nothing Dockwell does changes them.

### What Dockwell's table does

- **Inbound is allowed by default, until someone chooses otherwise.** A server Dockwell meets keeps working as it did. Deny everything else, the mock's fixed last rule, is a policy a person turns on, `firewall.policy`, which is a resource like a rule and so a plan too. Turning it on lists, in the plan, every port something on the server listens on that no rule allows. The owner decided this on 2026-11-07: a default of deny would need every server's ports listed before it joined to be safe, where this leaves each as it was until someone closes it on purpose.
- **What keeps the server reachable is always allowed, and pinned above every rule:** traffic of connections already open, loopback, the ICMP and ICMPv6 that path MTU discovery and IPv6 neighbor discovery need, and on the control server the agent listener and the panel's own ports. A rule that would deny those is refused when it's planned, saying which it would cut.
- **Docker's published ports are filtered where they arrive.** Docker forwards a published port to its container before the input hook, so an input rule never sees it. The table's `forward` chain matches traffic Docker translated (`ct status dnat`) by the port it arrived on (`ct original proto-dst`), from interfaces that aren't Docker's own, so `8080/tcp` means what a person means by it: the port on the server. Traffic between containers, and from them out, is never filtered.
- **Outbound is never filtered.** The agent's stream to the control plane goes out, and so do the packages, images and certificates a server fetches.

### The confirmation timer

A change to the firewall, SSH or fail2ban applies as any change does, and then waits:

1. The agent applies the new rules, keeps the ones they replaced, and starts a timer of 60 s.
2. The step's Verify waits for a person, in the panel or the CLI, to confirm they still have access. Their confirmation reaching the control plane, and the control plane reaching the agent, is itself the check.
3. If it comes, the agent drops the old rules and the step passes.
4. If it doesn't, the agent puts the old rules back on its own, without the control plane, which it may no longer reach, and the step fails, and the change rolls back as any failed change does.

The agent keeps the timer and the old rules on disk too, so one that restarts in the 60 s still rolls back.

### SSH and fail2ban

- **SSH hardening** is a resource, `ssh.config`: whether passwords and root may sign in, the port, and the sources that may reach it. The agent writes it to a drop-in of Dockwell's own under `/etc/ssh/sshd_config.d/`, runs `sshd -t` before it reloads, and refuses a change that fails it. A change takes a person and a passkey, as anything that decides who gets onto the server does ([ADR 0032](0032-files-on-a-server.md)), and the confirmation timer. The tab says the terminal reaches the server through the agent, so closing port 22 leaves a way in.
- **fail2ban** is read where it's installed: its jails and recent bans, through `fail2ban-client`. Unbanning an address is a change with a plan and no revision, as a restart is. A jail for SSH and one for the ingress are resources, `fail2ban.jail`, written to a drop-in of Dockwell's own under `/etc/fail2ban/jail.d/`. Installing fail2ban is the package manager's, which #276 brings.

### What the agent build carries

The agent drives `nft`, `sshd` and `fail2ban-client` as commands, so it links no netlink or nftables library, and its idle memory doesn't move. A server without `nft` says so, and its Firewall tab says Dockwell can't read or change its firewall there, and why.

## Consequences

- A server keeps working as it did when Dockwell arrives; closing everything else is a choice, made with a plan that lists what would close, as the owner decided.
- Dockwell can't open what ufw or firewalld closes, and says so. Someone who wants Dockwell to own the firewall turns those off, which the tab says how to do.
- Every firewall, SSH and fail2ban change costs up to 60 s of waiting for a confirmation. That's the price of not locking anyone out.
- Tests run the table against real nftables in CI, in a privileged container, and against a fake elsewhere. The install-test images (Debian, Ubuntu, AlmaLinux, Alpine) check that `nft` is there or say it isn't.
