# 0110. Host toolbox plans

- Status: Proposed
- Date: 2026-11-25

## Context

Server administrators need to configure swap, identity, time and DNS, reclaim disk space, and run background processes. These operations touch files and services that the OS and other tools also own. Changes to hostname, DNS and hosts entries can prevent the agent from reconnecting to its control server.

## Decision

The server's Toolbox uses the existing `server.host` change path from [ADR 0064](0064-a-change-on-a-server-installing-updates-and-rebooting.md). The agent reads the relevant host state on demand and supplies the plan. Apply checks that state again. Applying requires an install administrator in a browser session and a passkey proof bound to the change ID and fingerprint.

The engine stores the reviewed operation in the existing plan record. It resumes with the same reviewed operation and file list. It does not reconcile these settings against other host managers. A new reviewed change can adjust a setting again. Cleanup is permanent.

Hostname, DNS and hosts entries use the same Keep flow and server access claim as [firewall changes](0052-a-servers-firewall-ssh-and-fail2ban.md). The agent snapshots the files and arms an independent rollback before making changes. Keep requires a fresh TLS handshake with the control server. On startup the agent reverts an interrupted host settings change before it reports toolbox support.

Swap uses an owned file on ext4 or XFS with a free-space reserve. It backs up `/etc/fstab`, verifies the proposed table, and writes only its own entry. The agent edits DNS through systemd-resolved, eligible Netplan configurations, or an unmanaged resolver file. It reports other owners when it cannot safely edit their configuration.

Cleanup lists the exact files and allocated sizes before it runs. It limits candidates to APT package archives, archived systemd journals, rotated logs, and private, old, root-owned files immediately under sticky `/tmp`. It skips linked files and mount crossings. An exclusive file lease must be available for every candidate at apply.

Background services are prefixed systemd units with explicit users and literal arguments. Their environment values are selected by reference and pinned to the reviewed revisions. The control server seals the values to the target agent, which writes a root-only environment file. The plan shows the unit and binding names. Existing System services actions manage the unit after creation, and Toolbox can remove it.
