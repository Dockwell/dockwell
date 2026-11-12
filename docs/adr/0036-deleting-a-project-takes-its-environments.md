# 0036. Deleting a project takes its environments

- Status: Accepted. Amended by [ADR 0074](0074-projects-you-can-see-and-that-go-with-their-last-app.md): the panel's plan to delete a project takes everything left in it, and deleting the last app in a project offers the project too.
- Date: 2026-11-06

## Context

[ADR 0014](0014-object-model-and-store.md) says a scope goes only once it is empty, and a project keeps at least one environment. Each rule holds, but together they meant `dockwell projects delete` could never work. A project always has an environment, so the project was refused for holding it, and the environment was refused for being the last one. Only a change with two edits got through, which no command or screen makes.

This matters because a project can come to life without anyone asking for one. Importing a Compose file makes one ([ADR 0026](0026-the-app-screens.md)), and it stays once its services go. A second project also makes every command with no `--project` ask which one you mean, so one you can't remove stays in your way.

## Decision

A change that deletes a project takes every environment in it, in the same change.

- **The engine adds them, as it adds a new project's first environment.** So the API, the CLI and the panel all behave alike, the plan lists each environment that goes, and undoing the change brings them back with the project.
- **Every environment goes, not only the last.** A project with nothing in it has nothing to keep an environment for.
- **What lives in them still keeps the project.** The engine doesn't delete a service, a volume, a network, a variable or a secret on the way past: each has its own plan and consequences, and a volume holds data. The refusal names each one by kind and by environment, with the command that deletes it, so you can delete them first, or in the same change.
- **The panel offers what the panel can't otherwise delete.** "Delete this app" keeps its volumes, and the panel has no other way to delete a volume or a network, so its plan to delete a project asks for those too, naming each. A volume going is data going, so that plan needs a person and the typed confirmation, as any data loss does, and the volume is kept for 7 days, so undo brings it back with the project. Anything else left still refuses it.
- **A scope waits for what's applying in it.** A change's revisions are in before its steps run, so a deletion still running reads as nothing left. If that step then failed, its rollback would have nowhere to put things back, and what it was deleting would stay on the server, unmanaged. So deleting a project or an environment is refused, naming the change, until that ends. It doesn't wait for itself, or for the change it rolls back.
- **An undo takes what its change made and no more.** Undoing the change that made a project doesn't take an environment a later change added: that's refused by name, with what to do. Undoing a deletion inside a project that has since gone says which change to undo first, as undo is how what a deletion kept comes back.
- **The other rules stand.** An environment can't go alone while it is its project's last, and the organization keeps a project. A plan is checked again as it applies, so what arrived after planning refuses it.

The panel offers this in the command bar, once there are two projects, since the only one can't go.

## Consequences

- `dockwell projects delete <name>` works as it reads, and undo makes it safe to use.
- A project left with a network or a volume from a Compose file takes one more step: delete those first. The refusal says which.
- A plan to delete a project lists more than was asked for, which is what a plan is for.
