# Security policy

Dockwell runs an agent with root access on every server it manages, so we take every report seriously, including anything that could weaken that trust.

## Reporting a vulnerability

Don't open a public issue, pull request or discussion. Report it privately instead, either:

- on GitHub, from the repository's **Security** tab, with **Report a vulnerability**; or
- by email to **team@dockwell.io**.

Include what you found, the version or commit, steps to reproduce and the impact you expect. If you have a fix in mind, describe it in the report rather than opening a pull request.

We aim to acknowledge reports within three working days and to keep you updated until the problem is fixed. Unless you'd rather stay anonymous, the advisory credits you.

## Supported versions

Dockwell has not had a release yet. Once it has, security fixes go to the latest release.

## What we care about most

- The agent's connection to the control plane: enrollment, join tokens, mTLS and certificate rotation
- Authentication and authorization: passkeys, sessions, roles and API tokens
- Secrets: how they are stored, sent to servers and kept out of logs and plans
- Remote access through the agent: the terminal, files and commands
- The supply chain: release signing, agent self-updates and catalog images

An external security audit is planned before 1.0, followed by a public bug bounty.
