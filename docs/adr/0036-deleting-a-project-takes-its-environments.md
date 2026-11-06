# 0036. Deleting a project takes its environments

- Status: Accepted
- Date: 2026-11-06

## Context

[ADR 0014](0014-object-model-and-store.md) says a scope goes only once it is empty, and a project keeps at least one environment. Each rule holds, but together they meant `dockwell projects delete` could never work. A project always has an environment, so the project was refused for holding it, and the environment was refused for being the last one. Only a change with two edits got through, which no command or screen makes.

This matters because a project can come to life without anyone asking for one. Importing a Compose file makes one ([ADR 0026](0026-the-app-screens.md)), and it stays once its services go. A second project also makes every command with no `--project` ask which one you mean, so one you can't remove stays in your way.

## Decision

A change that deletes a project takes every environment in it, in the same change.

- **The engine adds them, as it adds a new project's first environment.** So the API, the CLI and the panel all behave alike, the plan lists each environment that goes, and undoing the change brings them back with the project.
- **Every environment goes, not only the last.** A project with nothing in it has nothing to keep an environment for.
- **What lives in them still keeps the project.** Dockwell doesn't delete a service, a volume, a network, a variable or a secret on the way past: each has its own plan and consequences, and a volume holds data. The refusal names each one by kind and by environment, so you can delete them first, or in the same change.
- **The other rules stand.** An environment can't go alone while it is its project's last, and the organization keeps a project. A plan is checked again as it applies, so what arrived after planning refuses it.

The panel offers this in the command bar, once there are two projects, since the only one can't go.

## Consequences

- `dockwell projects delete <name>` works as it reads, and undo makes it safe to use.
- A project left with a network or a volume from a Compose file takes one more step: delete those first. The refusal says which.
- A plan to delete a project lists more than was asked for, which is what a plan is for.
